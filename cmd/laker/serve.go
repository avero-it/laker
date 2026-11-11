package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/oklog/ulid/v2"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/adapters/agentapi"
	"github.com/avero-it/laker/intern/adapters/indexer/bleve"
	"github.com/avero-it/laker/intern/adapters/mcp"
	"github.com/avero-it/laker/intern/adapters/oauth"
	"github.com/avero-it/laker/intern/adapters/state/bbolt"
	"github.com/avero-it/laker/intern/adapters/web"
	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/logger"
	"github.com/avero-it/laker/pkg/signaler"
	"github.com/avero-it/laker/pkg/version"
)

// serveWait is how long serve waits for a CLI command to release the stores.
const serveWait = 2 * time.Minute

// cliWait is how long a CLI command waits for a run of serve to finish.
const cliWait = 5 * time.Second

// fatalf prints a coded error on stderr and exits; busy stores get a hint.
func fatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, a := range args {
		if err, ok := a.(error); ok && (errors.Is(err, bbolt.ErrBusy) || errors.Is(err, bleve.ErrBusy)) {
			msg += "\n(an adapter of laker serve is running now: retry when it is done)"
		}
	}
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func commandServe(configPath string) {
	cfg := mustConfig(configPath)
	lf := newLoggers(cfg)
	defer lf.Close()
	defer closeExtractor()

	lg := lf.For("LAKER", color.New(color.FgWhite).SprintFunc())
	lg.Info().Str("version", version.GetVersion()).Str("config_hash", config.Hash(cfg)).Msg("INF001: starting Laker")

	ctx, cancel := signaler.Context(context.Background(), lf.For("SIG", color.New(color.FgHiBlack).SprintFunc()))
	defer cancel()

	if err := os.MkdirAll(cfg.Common.DataDir, 0o750); err != nil {
		lg.Error().Msgf("ERR001: cannot create the data directory %s: %v", cfg.Common.DataDir, err)
		return
	}

	lake, err := buildLake(cfg, lf.For("LAKE", color.New(color.FgGreen).SprintFunc()))
	if err != nil {
		lg.Error().Msgf("ERR002: cannot build the lake: %v", err)
		return
	}
	info, err := lake.Check(ctx)
	if err != nil {
		lg.Error().Msgf("ERR003: the lake is not reachable: %v", err)
		return
	}
	ev := lg.Info().Str("kind", info.Kind).Str("location", info.Location).Bool("object_lock", info.ObjectLock)
	if info.Retention != "" {
		ev = ev.Str("retention", info.Retention)
	}
	ev.Msg("INF002: lake ready")
	if info.Kind == "s3" && !info.ObjectLock {
		lg.Warn().Msg("WRN001: the bucket has no Object Lock: Laker never deletes, but the storage does not enforce it")
	}

	stores := buildStores(cfg, serveWait)
	runner := core.LocalRunner(lake, stores, lf.For("CORE", color.New(color.FgWhite).SprintFunc()))
	loc, _ := time.LoadLocation(cfg.Common.Timezone)

	// the adapters of this laker own their sources: no agent may write them
	if err := claimLocal(cfg, stores); err != nil {
		lg.Error().Msgf("ERR00C: %v", err)
		return
	}

	var jobs []core.Job
	var adapters []web.Adapter
	for _, a := range cfg.Adapters {
		adapters = append(adapters, web.Adapter{Name: a.Name, Type: a.Type, Schedule: a.Schedule, Enabled: a.Enabled})
		if !a.Enabled {
			continue
		}
		adapter, err := buildAdapter(cfg, a, lf)
		if err != nil {
			lg.Error().Msgf("ERR007: %v", err)
			return
		}
		var sched cron.Schedule // nil: manual, runs only on demand
		if a.Schedule != config.ScheduleManual {
			sched, _ = config.CronParser.Parse(a.Schedule)
		}
		jobs = append(jobs, core.Job{Name: a.Name, Schedule: sched, Run: func(ctx context.Context) {
			runner.Run(ctx, adapter, false)
		}})
	}
	var snapSched cron.Schedule
	if s := cfg.Common.Snapshot.Schedule; s != config.ScheduleManual {
		snapSched, _ = config.CronParser.Parse(s)
	}
	snapLog := lf.For("SNAP", color.New(color.FgHiBlack).SprintFunc())
	jobs = append(jobs, core.Job{Name: "snapshot", Schedule: snapSched, Run: func(ctx context.Context) {
		if err := snapshot(ctx, cfg, lake, stores, snapLog); err != nil {
			snapLog.Error().Msgf("ERR008: snapshot failed: %v", err)
		}
	}})
	adapters = append(adapters, web.Adapter{Name: "snapshot", Type: "snapshot", Schedule: cfg.Common.Snapshot.Schedule, Enabled: true})

	sch := core.NewScheduler(ctx, loc, lf.For("SCHED", color.New(color.FgYellow).SprintFunc()), jobs)

	webDone := make(chan struct{})
	if wc := cfg.Common.Web; wc.Enabled {
		// the web interface keeps the stores open for the life of serve;
		// the runs share them
		state, index, release, err := stores.Acquire()
		if err != nil {
			lg.Error().Msgf("ERR009: cannot open the local stores: %v", err)
			return
		}
		defer release()
		var srv *web.Server
		deps := web.Deps{
			Lake: lake, Index: index, State: state, Adapters: adapters, Runs: sch, LakeInfo: info,
			Location: loc, Token: wc.Token, Version: version.GetVersion(),
			Logger: lf.For("WEB", color.New(color.FgBlue).SprintFunc()),
			Agents: agentsHandler(cfg, lake, stores, state, lf), AgentsEnabled: cfg.Common.Agents.Enabled,
		}
		if cfg.Common.MCP.Enabled {
			grants, err := bbolt.Open(oauthPath(cfg), serveWait)
			if err != nil {
				lg.Error().Msgf("ERR00F: cannot open the OAuth store: %v", err)
				return
			}
			defer grants.Close()
			redirects := cfg.Common.MCP.RedirectURIs
			if len(redirects) == 0 {
				redirects = config.DefaultMCPRedirectURIs
			}
			mcpLog := lf.For("MCP", color.New(color.FgHiBlue).SprintFunc())
			if deps.OAuth, err = oauth.New(oauth.Options{
				PublicURL: wc.PublicURL, RedirectURIs: redirects, Token: wc.Token, Store: grants, Logger: mcpLog,
			}); err != nil {
				lg.Error().Msgf("ERR011: %v", err)
				return
			}
			deps.MCP = mcp.Handler(mcp.Deps{
				Lake: lake, Index: index, State: state, Location: loc, Version: version.GetVersion(),
				Status: func() any { return srv.Statuses() },
			})
			lg.Info().Str("url", deps.OAuth.Resource()).Msg("INF008: MCP enabled")
		}
		srv, err = web.New(deps)
		if err != nil {
			lg.Error().Msgf("ERR00A: %v", err)
			return
		}
		go func() {
			defer close(webDone)
			if err := srv.Serve(ctx, net.JoinHostPort(wc.Host, strconv.Itoa(wc.Port))); err != nil {
				lg.Error().Msgf("ERR00B: %v", err)
				cancel() // a serve without its interface is not what was configured
			}
		}()
	} else {
		close(webDone)
	}

	lg.Info().Int("jobs", len(jobs)).Msg("INF003: running, waiting for a signal")
	sch.Serve()
	<-webDone
	lg.Info().Msg("INF004: stopped")
}

func commandRun(configPath, name string, dryRun bool) {
	cfg := mustConfig(configPath)
	var a *config.Adapter
	for i := range cfg.Adapters {
		if cfg.Adapters[i].Name == name {
			a = &cfg.Adapters[i]
		}
	}
	if a == nil {
		fatalf("ERR040: no adapter named %q in the configuration", name)
	}
	if !dryRun && runInServe(cfg, name) {
		return
	}

	lf := newLoggers(cfg)
	defer lf.Close()
	defer closeExtractor()
	ctx, cancel := signaler.Context(context.Background(), lf.For("SIG", color.New(color.FgHiBlack).SprintFunc()))
	defer cancel()

	if err := os.MkdirAll(cfg.Common.DataDir, 0o750); err != nil {
		fatalf("ERR041: cannot create the data directory: %v", err)
	}
	lake, err := buildLake(cfg, lf.For("LAKE", color.New(color.FgGreen).SprintFunc()))
	if err != nil {
		fatalf("ERR042: %v", err)
	}
	adapter, err := buildAdapter(cfg, *a, lf)
	if err != nil {
		fatalf("ERR043: %v", err)
	}
	runner := core.LocalRunner(lake, buildStores(cfg, cliWait), lf.For("CORE", color.New(color.FgWhite).SprintFunc()))
	run := runner.Run(ctx, adapter, dryRun)
	if run.Status != domain.RunOK {
		os.Exit(1)
	}
}

func commandList(configPath string) {
	cfg := mustConfig(configPath)
	loc, _ := time.LoadLocation(cfg.Common.Timezone)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	if rm := newRemote(cfg); rm != nil {
		st, err := rm.Status(context.Background())
		if err == nil {
			fmt.Fprintln(w, "NAME\tTYPE\tENABLED\tSCHEDULE\tNEXT\tLAST RUN\tSTATUS\tROWS")
			for _, a := range st {
				next, last, status, rows := "-", "-", "-", "-"
				if !a.Next.IsZero() {
					next = a.Next.In(loc).Format("2006-01-02 15:04")
				}
				if a.Last != nil {
					last, status, rows = a.Last.EndedAt.In(loc).Format("2006-01-02 15:04"), string(a.Last.Status), fmt.Sprint(a.Last.Stats.Rows)
				}
				if a.Running {
					status = "running"
				}
				fmt.Fprintf(w, "%s\t%s\t%v\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Type, a.Enabled, a.Schedule, next, last, status, rows)
			}
			return
		}
		if !errors.Is(err, errNotServing) {
			fatalf("ERR046: %v", err)
		}
	}

	state, err := bbolt.Open(statePath(cfg), cliWait)
	if err != nil {
		fatalf("ERR044: %v", err)
	}
	defer state.Close()

	fmt.Fprintln(w, "NAME\tTYPE\tENABLED\tSCHEDULE\tNEXT\tLAST RUN\tSTATUS\tROWS")
	for _, a := range cfg.Adapters {
		next := "-"
		if a.Enabled && a.Schedule != config.ScheduleManual {
			if s, err := config.CronParser.Parse(a.Schedule); err == nil {
				next = s.Next(time.Now().In(loc)).Format("2006-01-02 15:04")
			}
		}
		last, status, rows := "-", "-", "-"
		if r, err := core.LastRun(state, a.Name); err == nil && r != nil {
			last = r.EndedAt.In(loc).Format("2006-01-02 15:04")
			status = string(r.Status)
			rows = fmt.Sprint(r.Stats.Rows)
		}
		fmt.Fprintf(w, "%s\t%s\t%v\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Type, a.Enabled, a.Schedule, next, last, status, rows)
	}
}

// snapshot copies the state and index to the lake, holding the stores while
// it does, unless nothing reached the lake since the last one.
func snapshot(ctx context.Context, cfg *config.Config, lake out.Lake, stores *core.Stores, lg zerolog.Logger) error {
	state, index, release, err := stores.Acquire()
	if err != nil {
		return err
	}
	defer release()
	if err := os.MkdirAll(tmpDir(cfg), 0o750); err != nil {
		return fmt.Errorf("ERR045: %w", err)
	}
	run := domain.Run{ID: ulid.Make().String(), Adapter: "snapshot", Type: "snapshot", StartedAt: time.Now(), Status: domain.RunOK}
	keys, skipped, err := core.SnapshotIfChanged(ctx, lake, state, index, tmpDir(cfg), run.StartedAt)
	run.EndedAt, run.Stats.Files = time.Now(), len(keys)
	if err != nil {
		run.Status, run.Error = domain.RunFailed, err.Error()
	}
	// recorded like the runs of the adapters, for list and the status page
	if serr := state.Apply([]out.StateChange{{Bucket: domain.BucketRuns, Key: "snapshot", Value: domain.MustJSON(run)}}); serr != nil {
		lg.Warn().Msgf("WRN002: cannot record the snapshot: %v", serr)
	}
	if err != nil {
		return err
	}
	if skipped {
		lg.Info().Msg("INF009: nothing reached the lake since the last snapshot, none taken")
		return nil
	}
	lg.Info().Strs("keys", keys).Msg("INF007: snapshot stored in the lake")
	return nil
}

func claimLocal(cfg *config.Config, stores *core.Stores) error {
	state, _, release, err := stores.Acquire()
	if err != nil {
		return err
	}
	defer release()
	for _, a := range cfg.Adapters {
		if err := core.Claim(state, a.Name, domain.Owner{Agent: domain.LocalOwner, Type: a.Type}); err != nil {
			return fmt.Errorf("ERR00D: adapter %s: %w", a.Name, err)
		}
	}
	return nil
}

// agentsHandler is the API the agents send to, nil when they are disabled.
func agentsHandler(cfg *config.Config, lake out.Lake, stores *core.Stores, state out.StateStore, lf *logger.Factory) http.Handler {
	if !cfg.Common.Agents.Enabled {
		return nil
	}
	return agentapi.New(agentapi.Deps{
		Ingest: &core.LocalIngest{Lake: lake, Stores: stores}, State: state, TmpDir: tmpDir(cfg),
		Logger: lf.For("AGENTS", color.New(color.FgMagenta).SprintFunc()),
	}).Handler()
}
