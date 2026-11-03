package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

// LocalIngest writes into the lake and the local index and state of this
// process: what laker serve does for its own adapters and for its agents.
type LocalIngest struct {
	Lake   out.Lake
	Stores *Stores
	Now    func() time.Time
}

var _ out.Ingest = (*LocalIngest)(nil)

func (l *LocalIngest) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *LocalIngest) BlobExists(ctx context.Context, sha string) (bool, error) {
	key, err := domain.BlobKey(sha)
	if err != nil {
		return false, err
	}
	return l.Lake.Exists(ctx, key)
}

func (l *LocalIngest) PutBlob(ctx context.Context, sha string, size int64, r io.Reader) (bool, error) {
	key, err := domain.BlobKey(sha)
	if err != nil {
		return false, err
	}
	return l.Lake.PutIfAbsent(ctx, key, r, size)
}

// Commit writes the files, then the manifest, then the index. Every step is
// idempotent: a commit sent twice changes nothing the second time.
func (l *LocalIngest) Commit(ctx context.Context, c domain.Commit) error {
	if c.Empty() {
		return nil
	}
	man := domain.Manifest{
		Run: c.Run, Seq: c.Seq, Adapter: c.Adapter, Type: c.Type, At: l.now(),
		BlobsNew: c.BlobsNew, BlobsDedup: c.BlobsDedup, Bytes: c.Bytes,
	}
	for _, f := range c.Files {
		if _, err := l.Lake.PutIfAbsent(ctx, f.Key, bytes.NewReader(f.Data), int64(len(f.Data))); err != nil {
			return fmt.Errorf("ERR106: cannot write %s: %w", f.Key, err)
		}
		man.Files = append(man.Files, f.Key)
	}
	if len(man.Files) > 0 || man.BlobsNew > 0 {
		b, _ := json.MarshalIndent(man, "", "  ")
		name := fmt.Sprintf("_runs/%s/%s-%04d.json", c.Adapter, c.Run, c.Seq)
		if _, err := l.Lake.PutIfAbsent(ctx, name, bytes.NewReader(b), int64(len(b))); err != nil {
			return fmt.Errorf("ERR107: cannot write the manifest %s: %w", name, err)
		}
	}
	if len(c.Mail) == 0 && len(c.FileDocs) == 0 && len(c.ChatDocs) == 0 && len(c.Unindex) == 0 {
		return nil
	}

	state, index, release, err := l.Stores.Acquire()
	if err != nil {
		return fmt.Errorf("ERR10A: cannot open the local stores: %w", err)
	}
	defer release()

	docs := map[string]any{}
	var changes []out.StateChange
	merged := map[string]*domain.MailDoc{}
	for _, d := range c.Mail {
		seenIn := d.Folders // where this run saw it
		doc, ok := merged[d.SHA256]
		if !ok {
			var locs domain.MailLocations
			if b, _ := state.Get(domain.BucketMailLocations, d.SHA256); b != nil {
				_ = json.Unmarshal(b, &locs)
			}
			d.Accounts, d.Folders = locs.Accounts, locs.Folders
			doc = &d
			merged[d.SHA256] = doc
		}
		locs := domain.MailLocations{Accounts: doc.Accounts, Folders: doc.Folders}
		for _, f := range seenIn {
			locs.Add(c.Adapter, f)
		}
		doc.Kind, doc.Accounts, doc.Folders = "mail", locs.Accounts, locs.Folders
	}
	for sha, doc := range merged {
		docs[domain.MailDocID(sha)] = *doc
		changes = append(changes, out.StateChange{Bucket: domain.BucketMailLocations, Key: sha,
			Value: domain.MustJSON(domain.MailLocations{Accounts: doc.Accounts, Folders: doc.Folders})})
	}
	for _, d := range c.FileDocs {
		d.Kind = "file"
		docs[domain.FileDocID(d.Source, d.Path)] = d
	}
	for _, d := range c.ChatDocs {
		d.Kind = "chat"
		n, _ := strconv.ParseInt(d.ChatID, 10, 64)
		docs[domain.ChatDocID(d.Source, n, d.MsgID)] = d
	}
	for _, id := range c.Unindex {
		delete(docs, id)
	}
	if err := index.Apply(ctx, docs, c.Unindex); err != nil {
		return err
	}
	if err := state.Apply(changes); err != nil {
		return fmt.Errorf("ERR10B: cannot save the mail locations: %w", err)
	}
	return nil
}

// Finish records the run, for list and the status page.
func (l *LocalIngest) Finish(_ context.Context, run domain.Run) error {
	state, _, release, err := l.Stores.Acquire()
	if err != nil {
		return fmt.Errorf("ERR10C: cannot open the local stores: %w", err)
	}
	defer release()
	return state.Apply([]out.StateChange{{Bucket: domain.BucketRuns, Key: run.Adapter, Value: domain.MustJSON(run)}})
}

// dryIngest reads but writes nothing.
type dryIngest struct{ out.Ingest }

func (d dryIngest) PutBlob(ctx context.Context, sha string, _ int64, _ io.Reader) (bool, error) {
	exists, err := d.BlobExists(ctx, sha)
	return !exists, err
}
func (dryIngest) Commit(context.Context, domain.Commit) error { return nil }
func (dryIngest) Finish(context.Context, domain.Run) error    { return nil }
