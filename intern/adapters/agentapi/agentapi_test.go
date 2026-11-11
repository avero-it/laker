package agentapi_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/adapters/agentapi"
	"github.com/avero-it/laker/intern/adapters/agentclient"
	fssource "github.com/avero-it/laker/intern/adapters/sources/fs"
	"github.com/avero-it/laker/intern/adapters/state/bbolt"
	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/intern/coretest"
	"github.com/avero-it/laker/pkg/agentkey"
)

type server struct {
	env   *coretest.Env
	state out.StateStore
	index out.Indexer
	url   string
}

func newServer(t *testing.T) *server {
	t.Helper()
	env := coretest.New(t)
	state, index, release, err := env.Stores.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	api := agentapi.New(agentapi.Deps{
		Ingest: &core.LocalIngest{Lake: env.Lake, Stores: env.Stores}, State: state,
		TmpDir: filepath.Join(env.Dir, "tmp"), Logger: zerolog.Nop(),
	})
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return &server{env: env, state: state, index: index, url: ts.URL}
}

type agent struct {
	key    *agentkey.Key
	client *agentclient.Client
	runner *core.Runner
}

func newAgent(t *testing.T, url string) *agent {
	t.Helper()
	dir := t.TempDir()
	key, _, err := agentkey.LoadOrCreate(filepath.Join(dir, "agent.key"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := agentclient.New(url, key, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	c.Retries = 0
	runner := &core.Runner{Ingest: c, Logger: zerolog.Nop(), State: func() (out.StateStore, func(), error) {
		st, err := bbolt.Open(filepath.Join(dir, "state.db"), time.Second)
		if err != nil {
			return nil, nil, err
		}
		return st, func() { st.Close() }, nil
	}}
	return &agent{key: key, client: c, runner: runner}
}

func filesAdapter(t *testing.T, name, root string) core.Adapter {
	src, err := fssource.New(name, map[string]any{"paths": []any{root}}, 10, t.TempDir(), nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return core.Adapter{Name: name, Type: fssource.Type, Source: src}
}

func TestAgentLifecycle(t *testing.T) {
	srv := newServer(t)
	ctx := context.Background()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "report.txt"), []byte("annual report"), 0o644)

	ag := newAgent(t, srv.url)
	hello, err := ag.client.Hello(ctx, "home", "nas", "test")
	if err != nil || hello.Status != domain.AgentPending || hello.ID != ag.key.ID() {
		t.Fatalf("hello: %+v %v", hello, err)
	}

	// not approved: nothing gets in
	run := ag.runner.Run(ctx, filesAdapter(t, "nas-docs", root), false)
	if run.Status != domain.RunFailed || !strings.Contains(run.Error, "not approved") {
		t.Fatalf("pending agent run: %+v", run)
	}

	if err := core.SetAgentStatus(srv.state, ag.key.ID(), domain.AgentApproved); err != nil {
		t.Fatal(err)
	}
	run = ag.runner.Run(ctx, filesAdapter(t, "nas-docs", root), false)
	if run.Status != domain.RunOK || run.Stats.Rows != 1 || run.Stats.BlobsNew != 1 {
		t.Fatalf("approved agent run: %+v %s", run.Stats, run.Error)
	}

	// the server has everything: blob, parquet, index, run record, owner
	trees, _ := core.TreeAt(ctx, srv.env.Lake, "nas-docs", time.Time{})
	if len(trees["nas-docs"]) != 1 {
		t.Errorf("server tree: %v", trees)
	}
	if res, _ := srv.index.Search(ctx, domain.Query{Text: "report"}); res.Total != 1 {
		t.Errorf("server index: %d hits", res.Total)
	}
	if last, _ := core.LastRun(srv.state, "nas-docs"); last == nil || last.Status != domain.RunOK {
		t.Errorf("run record: %+v", last)
	}
	owners, _ := core.Owners(srv.state)
	if owners["nas-docs"].Agent != ag.key.ID() || owners["nas-docs"].Type != "fs" {
		t.Errorf("owners: %+v", owners)
	}

	// nothing new: nothing sent
	if run := ag.runner.Run(ctx, filesAdapter(t, "nas-docs", root), false); run.Stats.Rows != 0 {
		t.Errorf("second run rows = %d", run.Stats.Rows)
	}

	// another agent cannot write the same source
	other := newAgent(t, srv.url)
	other.client.Hello(ctx, "intruder", "x", "test")
	core.SetAgentStatus(srv.state, other.key.ID(), domain.AgentApproved)
	run = other.runner.Run(ctx, filesAdapter(t, "nas-docs", root), false)
	if run.Status != domain.RunFailed || !strings.Contains(run.Error, "409") {
		t.Errorf("source taken over: %+v", run)
	}

	// revoked: refused
	core.SetAgentStatus(srv.state, ag.key.ID(), domain.AgentRevoked)
	ag.client.Retries = 3 // a refusal is final: no retry
	start := time.Now()
	if _, err := ag.client.BlobExists(ctx, strings.Repeat("a", 64)); !errors.Is(err, agentclient.ErrNotApproved) {
		t.Errorf("revoked agent: %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("a refused request was retried (%s)", time.Since(start))
	}
}

// signed sends a request signed by key, as a hostile agent could.
func signed(t *testing.T, key *agentkey.Key, method, url, path string, body []byte, contentSHA string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url+path, bytes.NewReader(body))
	ts := time.Now().Unix()
	req.Header.Set(agentkey.HeaderAgent, key.ID())
	req.Header.Set(agentkey.HeaderTime, strconv.FormatInt(ts, 10))
	req.Header.Set(agentkey.HeaderContent, contentSHA)
	req.Header.Set(agentkey.HeaderSignature, key.Sign(method, path, ts, contentSHA))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A commit too large for one request arrives in pieces and ends up whole.
func TestLargeCommitTravelsInPieces(t *testing.T) {
	old := agentclient.MaxPart
	agentclient.MaxPart = 1 // every file and document in a request of its own
	defer func() { agentclient.MaxPart = old }()

	srv := newServer(t)
	ctx := context.Background()
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		os.WriteFile(filepath.Join(root, name+".txt"), []byte("quarterly figures of "+name), 0o644)
	}
	ag := newAgent(t, srv.url)
	if _, err := ag.client.Hello(ctx, "home", "nas", "test"); err != nil {
		t.Fatal(err)
	}
	if err := core.SetAgentStatus(srv.state, ag.key.ID(), domain.AgentApproved); err != nil {
		t.Fatal(err)
	}
	run := ag.runner.Run(ctx, filesAdapter(t, "nas-docs", root), false)
	if run.Status != domain.RunOK || run.Stats.Rows < 3 {
		t.Fatalf("run: %+v %s", run.Stats, run.Error)
	}

	trees, _ := core.TreeAt(ctx, srv.env.Lake, "nas-docs", time.Time{})
	if len(trees["nas-docs"]) != 3 {
		t.Errorf("server tree: %v", trees)
	}
	// the documents came each in a piece of its own, after the Parquet file
	if res, _ := srv.index.Search(ctx, domain.Query{Text: "txt"}); res.Total != 3 {
		t.Errorf("server index: %d hits, want 3", res.Total)
	}
	// only a piece that writes files has a manifest: here the first
	var manifests []string
	srv.env.Lake.List(ctx, "_runs/nas-docs/", func(k string) error { manifests = append(manifests, k); return nil })
	if len(manifests) != 1 || strings.Contains(manifests[0], "-p0") {
		t.Errorf("manifests: %v", manifests)
	}

	// the same content again: nothing new, nothing sent
	if run := ag.runner.Run(ctx, filesAdapter(t, "nas-docs", root), false); run.Stats.Rows != 0 {
		t.Errorf("second run rows = %d", run.Stats.Rows)
	}
}

func TestServerChecksWhatAgentsSend(t *testing.T) {
	srv := newServer(t)
	ctx := context.Background()
	ag := newAgent(t, srv.url)
	ag.client.Hello(ctx, "home", "nas", "test")
	core.SetAgentStatus(srv.state, ag.key.ID(), domain.AgentApproved)

	// a blob whose content is not its name
	fake := strings.Repeat("b", 64)
	if resp := signed(t, ag.key, http.MethodPut, srv.url, "/agent/v1/blobs/"+fake, []byte("other content"), fake); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("tampered blob: %d", resp.StatusCode)
	}
	if ok, _ := srv.env.Lake.Exists(ctx, "blobs/sha256/bb/bb/"+fake); ok {
		t.Error("tampered blob stored")
	}

	// a signature from another key
	intruder, _, _ := agentkey.LoadOrCreate(filepath.Join(t.TempDir(), "k"))
	req, _ := http.NewRequest(http.MethodHead, srv.url+"/agent/v1/blobs/"+fake, nil)
	ts := time.Now().Unix()
	req.Header.Set(agentkey.HeaderAgent, ag.key.ID())
	req.Header.Set(agentkey.HeaderTime, strconv.FormatInt(ts, 10))
	req.Header.Set(agentkey.HeaderContent, fake)
	req.Header.Set(agentkey.HeaderSignature, intruder.Sign(http.MethodHead, "/agent/v1/blobs/"+fake, ts, fake))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("forged signature: %d", resp.StatusCode)
	}

	// a commit writing outside its source
	err := ag.client.Commit(ctx, domain.Commit{Run: "r", Adapter: "home-docs", Type: "fs",
		Files: []domain.CommitFile{{Key: "files/inventory/source=someone-else/dt=2026-01-01/x.parquet", Data: []byte("x")}}})
	if err == nil || !strings.Contains(err.Error(), "outside the source") {
		t.Errorf("commit outside its source: %v", err)
	}
	err = ag.client.Commit(ctx, domain.Commit{Run: "r", Adapter: "home-docs", Type: "fs", Unindex: []string{"file:someone-else:/x"}})
	if err == nil {
		t.Error("unindex of another source accepted")
	}
}

type failingSource struct{}

func (failingSource) Run(context.Context, out.RunSink) error {
	return errors.New("cannot connect to postgres://reader:hunter2@db.local/erp")
}

// The credentials of an agent's sources never reach laker, not even inside
// an error message.
func TestRunErrorsAreRedacted(t *testing.T) {
	srv := newServer(t)
	ctx := context.Background()
	ag := newAgent(t, srv.url)
	ag.client.Hello(ctx, "home", "nas", "test")
	core.SetAgentStatus(srv.state, ag.key.ID(), domain.AgentApproved)

	run := ag.runner.Run(ctx, core.Adapter{Name: "home-db", Type: "postgres", Source: failingSource{}}, false)
	if run.Status != domain.RunFailed || strings.Contains(run.Error, "hunter2") {
		t.Fatalf("local run record: %+v", run)
	}
	// the failed run reaches laker, redacted
	last, _ := core.LastRun(srv.state, "home-db")
	if last == nil || last.Status != domain.RunFailed {
		t.Fatalf("failed run not recorded on laker: %+v", last)
	}
	// and the client redacts on its own, whatever the run record holds
	run.Error = "cannot connect to postgres://reader:hunter2@db.local/erp"
	if err := ag.client.Finish(ctx, run); err != nil {
		t.Fatal(err)
	}
	last, _ = core.LastRun(srv.state, "home-db")
	if last == nil || strings.Contains(last.Error, "hunter2") || !strings.Contains(last.Error, "reader:***@db.local") {
		t.Errorf("run record on laker: %+v", last)
	}
}
