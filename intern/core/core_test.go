package core_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/adapters/indexer/bleve"
	"github.com/avero-it/laker/intern/adapters/state/bbolt"
	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/intern/coretest"
	"github.com/avero-it/laker/pkg/parquetx"
)

// fakeSource writes two messages (one twice) and one file, committing once.
type fakeSource struct{ fail bool }

func (f fakeSource) Run(ctx context.Context, rs out.RunSink) error {
	date := time.Date(2025, 10, 6, 8, 0, 0, 0, time.UTC)
	for i, m := range []struct{ sha, folder, subject, body string }{
		{strings.Repeat("a", 64), "INBOX", "Kitchen quote", "Here is the quote"},
		{strings.Repeat("a", 64), "Archive", "Kitchen quote", "Here is the quote"},
		{strings.Repeat("b", 64), "INBOX", "Electricity bill", "Amount to pay"},
	} {
		if _, err := rs.PutBlob(ctx, m.sha, 4, strings.NewReader("mail")); err != nil {
			return err
		}
		row := domain.MailRow{SHA256: m.sha, Account: "acc", Folder: m.folder, UIDValidity: 7, UID: int64(i + 1),
			Date: date, From: "Anna Smith <anna.smith@example.com>", Subject: m.subject, BodyText: m.body, SeenAt: date, RunID: rs.RunID()}
		parquetx.Append(rs, domain.MailPrefix("acc", date), row)
		rs.IndexMail(domain.NewMailDoc(row, domain.MailLocations{Accounts: []string{"acc"}, Folders: []string{m.folder}}))
	}
	file := domain.FileRow{Source: "files", Path: "/home/me/taxes/730_2025.pdf", Op: domain.FileAdd, SHA256: strings.Repeat("c", 64), Size: 3, Mtime: date, SeenAt: date, RunID: rs.RunID()}
	parquetx.Append(rs, domain.FilePrefix("files", date), file)
	rs.IndexFile(domain.NewFileDoc(file))
	rs.SetState(domain.FolderStateKey("INBOX"), domain.MustJSON(domain.FolderState{UIDValidity: 7, LastUID: 3}))
	if f.fail {
		return context.Canceled
	}
	return nil
}

func run(t *testing.T, env *coretest.Env, src out.Source) domain.Run {
	t.Helper()
	return env.Runner.Run(context.Background(), core.Adapter{Name: "acc", Type: "fake", Source: src}, false)
}

func TestFailedRunCommitsNothing(t *testing.T) {
	env := coretest.New(t)
	r := run(t, env, fakeSource{fail: true})
	if r.Status != domain.RunFailed {
		t.Fatalf("status = %s", r.Status)
	}
	var parquet int
	_ = env.Lake.List(context.Background(), "", func(k string) error {
		if strings.HasSuffix(k, ".parquet") {
			parquet++
		}
		return nil
	})
	if parquet != 0 {
		t.Fatalf("a failed run left %d parquet file(s)", parquet)
	}
	env.With(t, func(st out.StateStore, _ out.Indexer) {
		if v, _ := st.Get(domain.SourceBucket("acc"), domain.FolderStateKey("INBOX")); v != nil {
			t.Errorf("state saved for a failed run: %s", v)
		}
	})
}

func TestSearchFields(t *testing.T) {
	env := coretest.New(t)
	if r := run(t, env, fakeSource{}); r.Status != domain.RunOK {
		t.Fatal(r.Error)
	}
	ctx := context.Background()
	env.With(t, func(_ out.StateStore, idx out.Indexer) {
		for q, want := range map[string]uint64{
			"quote":                      1,
			"from:smith":                 2, // address split on dots
			"smith -quote":               1,
			"730":                        1, // file name split on _ and .
			"taxes":                      1, // directory in the path
			"kind:file":                  1,
			`subject:"electricity bill"`: 1,
		} {
			res, err := idx.Search(ctx, domain.Query{Text: q, Highlight: "ansi"})
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			if res.Total != want {
				t.Errorf("%s: %d hits, want %d", q, res.Total, want)
			}
		}
	})
}

func TestReindexSnapshotRestore(t *testing.T) {
	env := coretest.New(t)
	ctx := context.Background()
	if r := run(t, env, fakeSource{}); r.Status != domain.RunOK {
		t.Fatal(r.Error)
	}

	// reindex into a fresh index gives the same answers
	idx, err := bleve.Open(filepath.Join(t.TempDir(), "idx"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	mails, files, err := core.Reindex(ctx, env.Lake, idx, zerologNop())
	if err != nil || mails != 2 || files != 1 {
		t.Fatalf("reindex: mails=%d files=%d err=%v", mails, files, err)
	}
	res, _ := idx.Search(ctx, domain.Query{Text: "quote"})
	if res.Total != 1 || !strings.Contains(strings.Join(anyStrings(res.Hits[0].Fields["folder"]), ","), "acc/Archive") {
		t.Errorf("reindexed hit: %+v", res.Hits)
	}
	idx.Close()

	// snapshot, then restore into an empty data directory
	env.With(t, func(st out.StateStore, ix out.Indexer) {
		keys, err := core.Snapshot(ctx, env.Lake, st, ix, t.TempDir(), time.Now())
		if err != nil || len(keys) != 2 {
			t.Fatalf("snapshot: %v %v", keys, err)
		}
	})
	dir := t.TempDir()
	if _, err := core.Restore(ctx, env.Lake, filepath.Join(dir, "state.db"), filepath.Join(dir, "index")); err != nil {
		t.Fatal(err)
	}
	st, err := bbolt.Open(filepath.Join(dir, "state.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, _ := st.Get(domain.SourceBucket("acc"), domain.FolderStateKey("INBOX")); !strings.Contains(string(v), `"last_uid":3`) {
		t.Errorf("restored state = %s", v)
	}
	rix, err := bleve.Open(filepath.Join(dir, "index"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer rix.Close()
	if res, _ := rix.Search(ctx, domain.Query{Text: "electricity"}); res.Total != 1 {
		t.Errorf("restored index: %d hits", res.Total)
	}

	// restore refuses to overwrite
	if _, err := core.Restore(ctx, env.Lake, filepath.Join(dir, "state.db"), filepath.Join(dir, "x")); err == nil {
		t.Error("restore overwrote an existing state")
	}
}

func anyStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, e.(string))
		}
		return out
	}
	return nil
}

func zerologNop() zerolog.Logger { return zerolog.Nop() }
