package core_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/avero-it/laker/pkg/parquetx"
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

// The monitor of an adapter hears how every real run ended, and nothing of a
// dry run.
func TestRunsReportToTheirHeartbeat(t *testing.T) {
	env := coretest.New(t)
	ctx := context.Background()
	var beats []string
	env.Runner.Beat = func(_ context.Context, url string, ok bool) error {
		beats = append(beats, url+map[bool]string{true: " ok", false: " fail"}[ok])
		return context.DeadlineExceeded // an unreachable monitor fails no run
	}
	adapter := func(src out.Source, heartbeat string) core.Adapter {
		return core.Adapter{Name: "acc", Type: "fake", Source: src, Heartbeat: heartbeat}
	}

	if r := env.Runner.Run(ctx, adapter(fakeSource{}, "https://up/x"), true); r.Status != domain.RunOK {
		t.Fatal(r.Error)
	}
	if r := env.Runner.Run(ctx, adapter(fakeSource{}, "https://up/x"), false); r.Status != domain.RunOK {
		t.Fatal(r.Error)
	}
	env.Runner.Run(ctx, adapter(fakeSource{fail: true}, "https://up/x"), false)
	env.Runner.Run(ctx, adapter(fakeSource{}, ""), false) // no monitor
	if got, want := strings.Join(beats, ", "), "https://up/x ok, https://up/x fail"; got != want {
		t.Errorf("heartbeats = %q, want %q", got, want)
	}
}

// honestSource stores two contents under their real hashes and names both.
type honestSource struct{}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (honestSource) Run(ctx context.Context, rs out.RunSink) error {
	date := time.Date(2025, 10, 6, 8, 0, 0, 0, time.UTC)
	for _, body := range []string{"a message", "a document"} {
		if _, err := rs.PutBlob(ctx, sum(body), int64(len(body)), strings.NewReader(body)); err != nil {
			return err
		}
	}
	parquetx.Append(rs, domain.MailPrefix("acc", date), domain.MailRow{SHA256: sum("a message"), Account: "acc", Folder: "INBOX", UID: 1, SeenAt: date, RunID: rs.RunID()})
	// a message too large to be stored names no content
	parquetx.Append(rs, domain.MailPrefix("acc", date), domain.MailRow{Account: "acc", Folder: "INBOX", UID: 2, SeenAt: date, RunID: rs.RunID(), Note: "too large"})
	parquetx.Append(rs, domain.FilePrefix("acc", date), domain.FileRow{Source: "acc", Path: "/doc.txt", Op: domain.FileAdd, SHA256: sum("a document"), SeenAt: date, RunID: rs.RunID()})
	return nil
}

func TestVerify(t *testing.T) {
	env := coretest.New(t)
	ctx := context.Background()
	if r := run(t, env, honestSource{}); r.Status != domain.RunOK {
		t.Fatal(r.Error)
	}
	all := core.VerifyOptions{Hash: -1}

	rep, err := core.Verify(ctx, env.Lake, all)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.Manifests != 1 || rep.ParquetFiles != 2 || rep.Blobs != 2 || rep.BlobRefs != 2 || rep.Hashed != 2 ||
		rep.UnlistedFiles.Count != 0 || rep.UnreferencedBlobs != 0 {
		t.Fatalf("a sound lake: %+v", rep)
	}
	if rep, _ := core.Verify(ctx, env.Lake, core.VerifyOptions{Hash: 1}); rep.Hashed != 1 {
		t.Errorf("a sample of 1 hashed %d", rep.Hashed)
	}
	if rep, _ := core.Verify(ctx, env.Lake, core.VerifyOptions{}); rep.Hashed != 0 || !rep.OK() {
		t.Errorf("no hashing asked: %+v", rep)
	}

	// what interrupted runs leave is noted, not a problem
	put := func(key, content string) {
		t.Helper()
		if _, err := env.Lake.PutIfAbsent(ctx, key, strings.NewReader(content), int64(len(content))); err != nil {
			t.Fatal(err)
		}
	}
	orphan, _ := domain.BlobKey(sum("uploaded, never committed"))
	put(orphan, "uploaded, never committed")
	if rep, _ := core.Verify(ctx, env.Lake, all); !rep.OK() || rep.UnreferencedBlobs != 1 {
		t.Errorf("an unreferenced content: %+v", rep)
	}

	// the fake source names a file content it never stored, and stores
	// contents under names that are not their hashes
	if r := run(t, env, fakeSource{}); r.Status != domain.RunOK {
		t.Fatal(r.Error)
	}
	// a manifest naming a file that is not there, one that is not JSON, a
	// Parquet file that is not Parquet, and a stray object among the contents
	put("_runs/acc/01JZZZZZZZZZZZZZZZZZZZZZZZ-0000.json", `{"run":"01JZZZZZZZZZZZZZZZZZZZZZZZ","files":["mail/messages/account=acc/month=2025-10/gone.parquet"]}`)
	put("_runs/acc/01JZZZZZZZZZZZZZZZZZZZZZZZ-0001.json", "not json")
	put("files/inventory/source=acc/dt=2025-10-07/01JZZZZZZZZZZZZZZZZZZZZZZZ-0002.parquet", "not parquet")
	put("blobs/sha256/stray.txt", "x")

	rep, err = core.Verify(ctx, env.Lake, all)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("a damaged lake was found sound")
	}
	if rep.BadManifests.Count != 1 || rep.MissingFiles.Count != 1 || rep.BadParquet.Count != 1 || rep.MisplacedKeys.Count != 1 {
		t.Errorf("manifests and files: %+v", rep)
	}
	if rep.MissingBlobs.Count != 1 || !strings.HasPrefix(rep.MissingBlobs.Examples[0], strings.Repeat("c", 64)+" named by files/inventory/source=files/") {
		t.Errorf("missing contents: %+v", rep.MissingBlobs)
	}
	if rep.CorruptBlobs.Count != 2 || rep.Hashed != 5 {
		t.Errorf("corrupt contents: %+v of %d hashed", rep.CorruptBlobs, rep.Hashed)
	}
	if rep.UnlistedFiles.Count != 1 {
		t.Errorf("unlisted files: %+v", rep.UnlistedFiles)
	}
}
