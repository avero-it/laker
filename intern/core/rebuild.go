package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

const rebuildBatch = 500

// Reindex fills an empty index from the lake: every message once, with all its
// places, and every file present in the latest tree of its source.
func Reindex(ctx context.Context, lake out.Lake, index out.Indexer, lg zerolog.Logger) (mails, files int, err error) {
	locs := map[string]*domain.MailLocations{}
	err = EachMail(ctx, lake, func(r domain.MailRow) error {
		if r.SHA256 == "" {
			return nil // not stored (too large), nothing to index
		}
		l, ok := locs[r.SHA256]
		if !ok {
			l = &domain.MailLocations{}
			locs[r.SHA256] = l
		}
		l.Add(r.Account, r.Folder)
		return nil
	})
	if err != nil {
		return 0, 0, err
	}

	batch := map[string]any{}
	flush := func() error {
		if err := index.Apply(ctx, batch, nil); err != nil {
			return err
		}
		batch = map[string]any{}
		return nil
	}

	done := map[string]bool{}
	err = EachMail(ctx, lake, func(r domain.MailRow) error {
		if r.SHA256 == "" || done[r.SHA256] {
			return nil
		}
		done[r.SHA256] = true
		batch[domain.MailDocID(r.SHA256)] = domain.NewMailDoc(r, *locs[r.SHA256])
		mails++
		if len(batch) >= rebuildBatch {
			lg.Debug().Int("mails", mails).Msg("DBG110: reindexing")
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return mails, 0, err
	}

	trees, err := TreeAt(ctx, lake, "", zeroTime)
	if err != nil {
		return mails, 0, err
	}
	// files first without text, then again for those whose text is in the
	// lake: the texts are streamed, never all held in memory
	type ref struct {
		source string
		entry  domain.FileEntry
	}
	bySHA := map[string][]ref{}
	fileDoc := func(r ref) domain.FileDoc {
		return domain.NewFileDoc(domain.FileRow{Source: r.source, Path: r.entry.Path, SHA256: r.entry.SHA256, Size: r.entry.Size, Mtime: r.entry.Mtime})
	}
	for source, tree := range trees {
		for _, e := range tree {
			r := ref{source, e}
			bySHA[e.SHA256] = append(bySHA[e.SHA256], r)
			batch[domain.FileDocID(source, e.Path)] = fileDoc(r)
			files++
			if len(batch) >= rebuildBatch {
				if err := flush(); err != nil {
					return mails, files, err
				}
			}
		}
	}
	err = EachText(ctx, lake, func(t domain.TextRow) error {
		if t.Text == "" {
			return nil
		}
		for _, r := range bySHA[t.SHA256] {
			if r.source != t.Source {
				continue
			}
			doc := fileDoc(r)
			doc.Content = t.Text
			batch[domain.FileDocID(r.source, r.entry.Path)] = doc
			if len(batch) >= rebuildBatch {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return mails, files, err
	}
	return mails, files, flush()
}

// RebuildState writes back into an empty state what the sources need to
// resume: IMAP checkpoints, mail locations and file inventories.
func RebuildState(ctx context.Context, lake out.Lake, state out.StateStore) (int, error) {
	// per folder and UIDVALIDITY: highest UID and last time seen; the
	// UIDVALIDITY seen last is the current one
	type folderKey struct{ account, folder string }
	type validity struct{ lastUID, lastSeen int64 }
	folders := map[folderKey]map[int64]*validity{}
	locs := map[string]*domain.MailLocations{}

	err := EachMail(ctx, lake, func(r domain.MailRow) error {
		if r.SHA256 != "" {
			l, ok := locs[r.SHA256]
			if !ok {
				l = &domain.MailLocations{}
				locs[r.SHA256] = l
			}
			l.Add(r.Account, r.Folder)
		}

		k := folderKey{r.Account, r.Folder}
		if folders[k] == nil {
			folders[k] = map[int64]*validity{}
		}
		v, ok := folders[k][r.UIDValidity]
		if !ok {
			v = &validity{}
			folders[k][r.UIDValidity] = v
		}
		v.lastUID = max(v.lastUID, r.UID)
		v.lastSeen = max(v.lastSeen, r.SeenAt.UnixNano())
		return nil
	})
	if err != nil {
		return 0, err
	}

	var changes []out.StateChange
	for k, vals := range folders {
		var cur domain.FolderState
		var curSeen int64 = -1
		for uv, v := range vals {
			if v.lastSeen > curSeen {
				cur, curSeen = domain.FolderState{UIDValidity: uv, LastUID: v.lastUID}, v.lastSeen
			}
		}
		changes = append(changes, out.StateChange{
			Bucket: domain.SourceBucket(k.account), Key: domain.FolderStateKey(k.folder), Value: domain.MustJSON(cur),
		})
	}
	for sha, l := range locs {
		changes = append(changes, out.StateChange{Bucket: domain.BucketMailLocations, Key: sha, Value: domain.MustJSON(l)})
	}

	trees, err := TreeAt(ctx, lake, "", zeroTime)
	if err != nil {
		return 0, err
	}
	for source, tree := range trees {
		for _, e := range tree {
			changes = append(changes, out.StateChange{
				Bucket: domain.SourceBucket(source), Key: domain.InventoryKey(e.Path),
				Value: domain.MustJSON(domain.InventoryEntry{Size: e.Size, MtimeNano: e.Mtime.UnixNano(), SHA256: e.SHA256}),
			})
		}
	}

	rows, err := lastDBRows(ctx, lake)
	if err != nil {
		return 0, err
	}
	for k, hash := range rows {
		changes = append(changes, out.StateChange{Bucket: domain.SourceBucket(k.source), Key: domain.RowStateKey(k.table, k.pk), Value: []byte(hash)})
	}

	err = EachText(ctx, lake, func(t domain.TextRow) error {
		changes = append(changes, out.StateChange{Bucket: domain.SourceBucket(t.Source), Key: domain.TextStateKey(t.SHA256), Value: []byte{1}})
		return nil
	})
	if err != nil {
		return 0, err
	}

	if err := state.Apply(changes); err != nil {
		return 0, fmt.Errorf("ERR112: cannot write the rebuilt state: %w", err)
	}
	return len(changes), nil
}

type dbRowKey struct{ source, table, pk string }

// lastDBRows replays the database changelogs: the hash of every row present
// after the last run.
func lastDBRows(ctx context.Context, lake out.Lake) (map[dbRowKey]string, error) {
	type change struct {
		domain.DBMeta
		source, table string
	}
	var all []change
	var keys []string
	err := lake.List(ctx, "db/", func(k string) error {
		if strings.HasSuffix(k, ".parquet") {
			keys = append(keys, k)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		source, table, ok := domain.DBTableOf(k)
		if !ok {
			continue
		}
		metas, err := readParquet[domain.DBMeta](ctx, lake, k)
		if err != nil {
			return nil, err
		}
		for _, m := range metas {
			all = append(all, change{m, source, table})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].SeenAt.Equal(all[j].SeenAt) {
			return all[i].SeenAt.Before(all[j].SeenAt)
		}
		return all[i].RunID < all[j].RunID
	})
	last := map[dbRowKey]string{}
	for _, c := range all {
		k := dbRowKey{c.source, c.table, c.PK}
		if c.Op == domain.DBDelete {
			delete(last, k)
		} else {
			last[k] = c.RowHash
		}
	}
	return last, nil
}
