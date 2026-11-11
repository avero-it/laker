package core

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

// The rebuilds read the whole lake, so they keep in memory only what must be
// compared across files (where each message is, the latest trees, the live
// rows of the databases) and write everything else as they read it.
const (
	rebuildBatch      = 500      // index documents in one batch
	rebuildBatchBytes = 32 << 20 // or this much text, whichever comes first
)

// stateBatch is how many state changes one transaction of a rebuild holds. A
// var for the tests.
var stateBatch = 10000

// indexBatch collects index documents and flushes them by count or by size.
type indexBatch struct {
	index out.Indexer
	docs  map[string]any
	bytes int
}

func (b *indexBatch) add(ctx context.Context, id string, doc any, size int) error {
	if b.docs == nil {
		b.docs = map[string]any{}
	}
	b.docs[id] = doc
	b.bytes += size
	if len(b.docs) >= rebuildBatch || b.bytes >= rebuildBatchBytes {
		return b.flush(ctx)
	}
	return nil
}

func (b *indexBatch) flush(ctx context.Context) error {
	if err := b.index.Apply(ctx, b.docs, nil); err != nil {
		return err
	}
	b.docs, b.bytes = nil, 0
	return nil
}

// stateWriter applies state changes in transactions of stateBatch.
type stateWriter struct {
	state   out.StateStore
	pending []out.StateChange
	n       int
}

func (w *stateWriter) put(bucket, key string, value []byte) error {
	w.pending = append(w.pending, out.StateChange{Bucket: bucket, Key: key, Value: value})
	if len(w.pending) >= stateBatch {
		return w.flush()
	}
	return nil
}

func (w *stateWriter) flush() error {
	if err := w.state.Apply(w.pending); err != nil {
		return fmt.Errorf("ERR112: cannot write the rebuilt state: %w", err)
	}
	w.n += len(w.pending)
	w.pending = w.pending[:0]
	return nil
}

// Reindex fills an empty index from the lake: every message once, with all its
// places, and every file present in the latest tree of its source.
func Reindex(ctx context.Context, lake out.Lake, index out.Indexer, lg zerolog.Logger) (mails, files int, err error) {
	// first where every message is, without its text
	locs := map[string]*domain.MailLocations{}
	err = eachMailRef(ctx, lake, func(r mailRef) error {
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

	batch := &indexBatch{index: index}
	err = EachMail(ctx, lake, func(r domain.MailRow) error {
		l, ok := locs[r.SHA256]
		if !ok {
			return nil // not stored, or indexed already from another place
		}
		delete(locs, r.SHA256)
		mails++
		if mails%(10*rebuildBatch) == 0 {
			lg.Debug().Int("mails", mails).Msg("DBG110: reindexing")
		}
		return batch.add(ctx, domain.MailDocID(r.SHA256), domain.NewMailDoc(r, *l), len(r.BodyText))
	})
	if err == nil {
		err = batch.flush(ctx)
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
			files++
			if err := batch.add(ctx, domain.FileDocID(source, e.Path), fileDoc(r), 0); err != nil {
				return mails, files, err
			}
		}
	}
	if err := batch.flush(ctx); err != nil {
		return mails, files, err
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
			if err := batch.add(ctx, domain.FileDocID(r.source, r.entry.Path), doc, len(t.Text)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return mails, files, err
	}
	// chat messages: the last row of each message wins (an edit after the original)
	err = EachChat(ctx, lake, func(r domain.ChatRow) error {
		doc := domain.NewChatDoc(r)
		return batch.add(ctx, doc.ID(), doc, len(r.Text))
	})
	if err != nil {
		return mails, files, err
	}
	return mails, files, batch.flush(ctx)
}

// RebuildState writes back into an empty state what the sources need to
// resume: IMAP checkpoints, mail locations, file inventories, database row
// hashes and chat checkpoints. It returns how many changes it wrote, in
// several transactions: on an error the state is incomplete and must not be
// used.
func RebuildState(ctx context.Context, lake out.Lake, state out.StateStore) (int, error) {
	w := &stateWriter{state: state}

	// per folder and UIDVALIDITY: highest UID and last time seen; the
	// UIDVALIDITY seen last is the current one
	type folderKey struct{ account, folder string }
	type validity struct{ lastUID, lastSeen int64 }
	folders := map[folderKey]map[int64]*validity{}
	locs := map[string]*domain.MailLocations{}

	err := eachMailRef(ctx, lake, func(r mailRef) error {
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

	for k, vals := range folders {
		var cur domain.FolderState
		var curSeen int64 = -1
		for uv, v := range vals {
			if v.lastSeen > curSeen {
				cur, curSeen = domain.FolderState{UIDValidity: uv, LastUID: v.lastUID}, v.lastSeen
			}
		}
		if err := w.put(domain.SourceBucket(k.account), domain.FolderStateKey(k.folder), domain.MustJSON(cur)); err != nil {
			return w.n, err
		}
	}
	for sha, l := range locs {
		if err := w.put(domain.BucketMailLocations, sha, domain.MustJSON(l)); err != nil {
			return w.n, err
		}
	}
	folders, locs = nil, nil

	trees, err := TreeAt(ctx, lake, "", zeroTime)
	if err != nil {
		return w.n, err
	}
	for source, tree := range trees {
		for _, e := range tree {
			v := domain.MustJSON(domain.InventoryEntry{Size: e.Size, MtimeNano: e.Mtime.UnixNano(), SHA256: e.SHA256})
			if err := w.put(domain.SourceBucket(source), domain.InventoryKey(e.Path), v); err != nil {
				return w.n, err
			}
		}
	}
	trees = nil

	rows, err := lastDBRows(ctx, lake)
	if err != nil {
		return w.n, err
	}
	for k, hash := range rows {
		if err := w.put(domain.SourceBucket(k.source), domain.RowStateKey(k.table, k.pk), []byte(hash)); err != nil {
			return w.n, err
		}
	}
	rows = nil

	chats := map[[2]string]*domain.ChatState{}
	err = EachChat(ctx, lake, func(r domain.ChatRow) error {
		if r.Service == "whatsapp" {
			// the last version of each message, as the source keeps it: the
			// rows come in the order they were seen, so the last one written
			// to a key is the last version
			return w.put(domain.SourceBucket(r.Source), domain.ChatMsgStateKey(r.Chat(), r.Msg()), domain.MustJSON(r))
		}
		k := [2]string{r.Source, domain.ChatStateKey(r.ChatID)}
		st := chats[k]
		if st == nil {
			// the first import may have stopped half way: the next run
			// carries on from the last message, outside an export session
			st = &domain.ChatState{Imported: true}
			chats[k] = st
		}
		st.LastID = max(st.LastID, r.MsgID)
		if r.EditDate.After(st.EditSeen) {
			st.EditSeen = r.EditDate
		}
		return nil
	})
	if err != nil {
		return w.n, err
	}
	for k, st := range chats {
		if err := w.put(domain.SourceBucket(k[0]), k[1], domain.MustJSON(st)); err != nil {
			return w.n, err
		}
	}

	err = eachTextRef(ctx, lake, func(t textRef) error {
		return w.put(domain.SourceBucket(t.Source), domain.TextStateKey(t.SHA256), []byte{1})
	})
	if err != nil {
		return w.n, err
	}
	return w.n, w.flush()
}

type dbRowKey struct{ source, table, pk string }

// lastDBRows replays the database changelogs in the order they were written:
// the hash of every row present after the last run.
func lastDBRows(ctx context.Context, lake out.Lake) (map[dbRowKey]string, error) {
	keys, err := parquetKeys(ctx, lake, "db/", nil)
	if err != nil {
		return nil, err
	}
	last := map[dbRowKey]string{}
	seen := func(m domain.DBMeta) time.Time { return m.SeenAt }
	for _, key := range keys {
		source, table, ok := domain.DBTableOf(key)
		if !ok {
			continue
		}
		err := eachRow(ctx, lake, []string{key}, seen, func(m domain.DBMeta) error {
			k := dbRowKey{source, table, m.PK}
			if m.Op == domain.DBDelete {
				delete(last, k)
			} else {
				last[k] = m.RowHash
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return last, nil
}
