package fs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/intern/coretest"
	"github.com/avero-it/laker/pkg/extract"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanHistoryAndBrowse(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "docs/contract.txt"), "contract v1")
	write(t, filepath.Join(root, "docs/copy.txt"), "contract v1") // same content
	write(t, filepath.Join(root, "photos/sea.jpg"), "jpeg")
	write(t, filepath.Join(root, ".git/config"), "excluded")

	env := coretest.New(t)
	ctx := context.Background()
	src, err := New("files", map[string]any{"paths": []any{root}, "exclude": []any{"**/.git/**"}}, 2, filepath.Join(env.Dir, "tmp"), nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	a := core.Adapter{Name: "files", Type: Type, Source: src}

	t1 := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	src.now = func() time.Time { return t1 }
	run := env.Runner.Run(ctx, a, false)
	if run.Status != domain.RunOK || run.Stats.Rows != 3 || run.Stats.BlobsNew != 2 || run.Stats.BlobsDedup != 1 {
		t.Fatalf("first run = %+v %s", run.Stats, run.Error)
	}

	// unchanged tree: nothing recorded
	if run := env.Runner.Run(ctx, a, false); run.Stats.Rows != 0 {
		t.Fatalf("second run rows = %d, want 0", run.Stats.Rows)
	}

	// modify, delete, move, touch
	write(t, filepath.Join(root, "docs/contract.txt"), "contract v2")
	os.Remove(filepath.Join(root, "docs/copy.txt"))
	os.Mkdir(filepath.Join(root, "photos/2025"), 0o755)
	os.Rename(filepath.Join(root, "photos/sea.jpg"), filepath.Join(root, "photos/2025/sea.jpg"))
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(root, "photos/2025/sea.jpg"), later, later)

	t2 := time.Date(2026, 2, 10, 12, 0, 0, 0, time.UTC)
	src.now = func() time.Time { return t2 }
	run = env.Runner.Run(ctx, a, false)
	if run.Status != domain.RunOK {
		t.Fatal(run.Error)
	}

	ops := map[string]string{}
	err = core.EachFile(ctx, env.Lake, "files", t2, func(r domain.FileRow) error {
		if r.SeenAt.Equal(t2) {
			ops[strings.TrimPrefix(r.Path, root)] = r.Op
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"/docs/contract.txt": "modify", "/docs/copy.txt": "delete", "/photos/2025/sea.jpg": "move"}
	for p, op := range want {
		if ops[p] != op {
			t.Errorf("%s: op = %q, want %q (all: %v)", p, ops[p], op, ops)
		}
	}

	// the tree in between still has the old files
	trees, err := core.TreeAt(ctx, env.Lake, "files", t1.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	old := trees["files"]
	if len(old) != 3 || old[filepath.Join(root, "docs/copy.txt")].SHA256 == "" {
		t.Errorf("tree at t1: %v", old)
	}
	trees, _ = core.TreeAt(ctx, env.Lake, "files", time.Time{})
	now := trees["files"]
	if len(now) != 2 || now[filepath.Join(root, "photos/2025/sea.jpg")].Size != 4 {
		t.Errorf("tree now: %v", now)
	}

	env.With(t, func(_ out.StateStore, index out.Indexer) {
		res, _ := index.Search(ctx, domain.Query{Text: "contract", Kind: "file"})
		if res.Total != 1 {
			t.Errorf("search contract: %d hits, want 1 (copy.txt was deleted)", res.Total)
		}
		res, _ = index.Search(ctx, domain.Query{Text: "sea"})
		if res.Total != 1 || !strings.Contains(res.Hits[0].Fields["path"].(string), "2025") {
			t.Errorf("search sea: %+v", res.Hits)
		}
	})
}

func TestUnreadableRootDeletesNothing(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "a")
	env := coretest.New(t)
	ctx := context.Background()
	src, _ := New("files", map[string]any{"paths": []any{root}}, 10, t.TempDir(), nil, zerolog.Nop())
	a := core.Adapter{Name: "files", Type: Type, Source: src}
	env.Runner.Run(ctx, a, false)

	os.Rename(root, root+".away")
	t.Cleanup(func() { os.Rename(root+".away", root) })
	if run := env.Runner.Run(ctx, a, false); run.Stats.Rows != 0 {
		t.Fatalf("rows = %d: a missing root must not delete its files", run.Stats.Rows)
	}
}

func TestExtractedTextIsSearchableAndRebuilt(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "note.md"), "# Meeting\nWe discuss the quote for the renovation.")
	write(t, filepath.Join(root, "copy/note.md"), "# Meeting\nWe discuss the quote for the renovation.")
	write(t, filepath.Join(root, "photo.jpg"), "jpeg")

	x := extract.New()
	defer x.Close()
	env := coretest.New(t)
	ctx := context.Background()
	src, err := New("files", map[string]any{"paths": []any{root}}, 10, t.TempDir(), x, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if run := env.Runner.Run(ctx, core.Adapter{Name: "files", Type: Type, Source: src}, false); run.Status != domain.RunOK {
		t.Fatal(run.Error)
	}

	texts := 0
	core.EachText(ctx, env.Lake, func(r domain.TextRow) error { texts++; return nil })
	if texts != 1 {
		t.Errorf("text rows = %d, want 1 (same content twice)", texts)
	}

	search := func(idx out.Indexer) uint64 {
		res, err := idx.Search(ctx, domain.Query{Text: "renovation", Kind: "file"})
		if err != nil {
			t.Fatal(err)
		}
		return res.Total
	}
	env.With(t, func(_ out.StateStore, idx out.Indexer) {
		if n := search(idx); n != 2 {
			t.Errorf("search in content: %d hits, want 2", n)
		}
	})

	fresh := coretest.New(t)
	fresh.With(t, func(st out.StateStore, idx out.Indexer) {
		if _, _, err := core.Reindex(ctx, env.Lake, idx, zerolog.Nop()); err != nil {
			t.Fatal(err)
		}
		if n := search(idx); n != 2 {
			t.Errorf("after reindex: %d hits, want 2", n)
		}
		core.RebuildState(ctx, env.Lake, st)
		var count int
		st.ForEach(domain.SourceBucket("files"), "text:", func(string, []byte) error { count++; return nil })
		if count != 1 {
			t.Errorf("rebuilt text keys = %d", count)
		}
	})
}
