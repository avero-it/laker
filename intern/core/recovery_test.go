package core_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avero-it/laker/intern/adapters/state/bbolt"
	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/intern/coretest"
)

// Files are read in the order the runs wrote them, whatever their partition,
// and the sequence of a run is a number, not a string.
func TestParquetKeysFollowTheRuns(t *testing.T) {
	env := coretest.New(t)
	ctx := context.Background()
	const first, second = "01JAAAAAAAAAAAAAAAAAAAAAAA", "01JBBBBBBBBBBBBBBBBBBBBBBB"
	want := []string{
		"files/inventory/source=z/dt=2025-01-02/" + first + "-0000.parquet",
		"files/inventory/source=a/dt=2025-01-03/" + first + "-9999.parquet",
		"files/inventory/source=a/dt=2025-01-01/" + first + "-10000.parquet",
		"files/inventory/source=a/dt=2025-01-01/" + second + "-0000.parquet",
	}
	for _, k := range append([]string{"files/inventory/source=a/dt=2025-01-01/notes.txt"}, want...) {
		if _, err := env.Lake.PutIfAbsent(ctx, k, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}
	got, err := core.ParquetKeys(ctx, env.Lake, "files/inventory/", nil)
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("keys = %v, %v\nwant   %v", got, err, want)
	}
}

// A rebuild writes in several transactions and still restores everything.
func TestRebuildStateInSmallTransactions(t *testing.T) {
	defer core.SetStateBatch(2)()
	env := coretest.New(t)
	ctx := context.Background()
	if r := run(t, env, fakeSource{}); r.Status != domain.RunOK {
		t.Fatal(r.Error)
	}
	st, err := bbolt.Open(filepath.Join(t.TempDir(), "state.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n, err := core.RebuildState(ctx, env.Lake, st)
	// 2 folders, 2 messages, 1 file
	if err != nil || n != 5 {
		t.Fatalf("rebuild: %d changes, %v", n, err)
	}
	if v, _ := st.Get(domain.SourceBucket("acc"), domain.FolderStateKey("INBOX")); !strings.Contains(string(v), `"last_uid":3`) {
		t.Errorf("INBOX checkpoint = %s", v)
	}
	if v, _ := st.Get(domain.BucketMailLocations, strings.Repeat("a", 64)); !strings.Contains(string(v), "Archive") || !strings.Contains(string(v), "INBOX") {
		t.Errorf("locations of the message in two folders = %s", v)
	}
	if v, _ := st.Get(domain.SourceBucket("files"), domain.InventoryKey("/home/me/taxes/730_2025.pdf")); v == nil {
		t.Error("the file is not in the rebuilt inventory")
	}
}

// A snapshot can never be removed from the lake: one is taken only when a
// commit reached the lake since the last.
func TestSnapshotOnlyAfterAChange(t *testing.T) {
	env := coretest.New(t)
	ctx := context.Background()
	at := time.Date(2025, 10, 6, 4, 30, 0, 0, time.UTC)
	snap := func(day int) (keys []string, skipped bool) {
		t.Helper()
		env.With(t, func(st out.StateStore, ix out.Indexer) {
			var err error
			if keys, skipped, err = core.SnapshotIfChanged(ctx, env.Lake, st, ix, t.TempDir(), at.AddDate(0, 0, day)); err != nil {
				t.Fatal(err)
			}
		})
		return keys, skipped
	}

	// an empty lake gets its first snapshot, once
	if keys, skipped := snap(0); skipped || len(keys) != 2 {
		t.Fatalf("first snapshot: %v skipped=%v", keys, skipped)
	}
	if _, skipped := snap(1); !skipped {
		t.Error("a second snapshot of an unchanged lake was taken")
	}

	if r := run(t, env, fakeSource{}); r.Status != domain.RunOK {
		t.Fatal(r.Error)
	}
	if keys, skipped := snap(2); skipped || len(keys) != 2 {
		t.Errorf("snapshot after a commit: %v skipped=%v", keys, skipped)
	}
	if _, skipped := snap(3); !skipped {
		t.Error("a snapshot was taken though nothing was committed since")
	}

	// a run that fails commits nothing: still nothing to snapshot
	run(t, env, fakeSource{fail: true})
	if _, skipped := snap(4); !skipped {
		t.Error("a failed run caused a snapshot")
	}

	// after a reindex the index is new though the lake is not
	env.With(t, func(st out.StateStore, _ out.Indexer) {
		if err := core.SnapshotDue(st); err != nil {
			t.Fatal(err)
		}
	})
	if _, skipped := snap(5); skipped {
		t.Error("no snapshot after SnapshotDue")
	}

	var n int
	env.Lake.List(ctx, "_state/", func(string) error { n++; return nil })
	if n != 3 {
		t.Errorf("%d state snapshots in the lake, want 3", n)
	}
}
