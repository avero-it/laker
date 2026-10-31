// laker-agent runs sources near the data (a computer, a NAS, an office
// network) and sends what they read to laker serve. It has an identity of its
// own: an Ed25519 key created on the first start, whose ID laker must approve
// before it accepts anything.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"text/tabwriter"
	"time"
	_ "time/tzdata" // schedules run in agent.timezone on hosts without a tz database

	"github.com/fatih/color"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/avero-it/laker/intern/adapters/agentclient"
	"github.com/avero-it/laker/intern/adapters/sources"
	"github.com/avero-it/laker/intern/adapters/state/bbolt"
	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/agentkey"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/extract"
	"github.com/avero-it/laker/pkg/logger"
	"github.com/avero-it/laker/pkg/signaler"
	"github.com/avero-it/laker/pkg/version"
)

// helloEvery is how often a pending agent asks again whether it is approved.
const helloEvery = 30 * time.Second

func main() {
	if err := setupCLI().Execute(); err != nil {
		os.Exit(2)
	}
}

func setupCLI() *cobra.Command {
	var configPath string
	root := &cobra.Command{
		Use:          "laker-agent",
		Short:        "laker-agent - sends data to laker",
		Long:         "laker-agent runs sources near the data and sends what they read to laker serve, once laker approves its ID.",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "configuration file (default: /etc/laker-agent/config.yaml, then ./laker-agent.yaml)")

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Show the ID, wait for approval, then run the adapters on their schedules",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) { commandServe(configPath) },
	}
	idCmd := &cobra.Command{
		Use:   "id",
		Short: "Print the ID of this agent (creating its key on the first use)",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) { commandID(configPath) },
	}
	runCmd := &cobra.Command{
		Use:   "run <adapter>",
		Short: "Run one adapter now, once (laker-agent serve must not be running)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			dry, _ := cmd.Flags().GetBool("dry-run")
			commandRun(configPath, args[0], dry)
		},
	}
	runCmd.Flags().Bool("dry-run", false, "Read and compare, send nothing")
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the adapters with their last run",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) { commandList(configPath) },
	}
	versionCmd := &cobra.Command{
		Use:  "version",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("laker-agent %s (%s, %s)\n", version.GetVersion(), version.GetCommitHash(), runtime.Version())
		},
	}
	configCmd := &cobra.Command{Use: "config", Short: "Configuration management commands"}
	generate := &cobra.Command{
		Use:     "generate",
		Aliases: []string{"create"},
		Short:   "Write a configuration with one documented, disabled adapter of every type",
		Args:    cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out, _ := cmd.Flags().GetString("output")
			force, _ := cmd.Flags().GetBool("force")
			disc, _ := cmd.Flags().GetBool("discover")
			commandGenerate(out, force, disc)
		},
	}
	generate.Flags().StringP("output", "o", "laker-agent.yaml", "file to write")
	generate.Flags().Bool("force", false, "Overwrite an existing file")
	generate.Flags().Bool("discover", false, "Also add what laker-agent config discover finds on this machine (disabled, no secret)")
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Check the configuration, then connect to every enabled adapter",
		Long: "Check the form of the configuration, then probe every enabled adapter: log in to the mailboxes and list\n" +
			"their folders, read the directories, connect to the databases and list their tables. Nothing is sent to laker.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			offline, _ := cmd.Flags().GetBool("offline")
			asJSON, _ := cmd.Flags().GetBool("json")
			commandValidate(configPath, offline, asJSON)
		},
	}
	validate.Flags().Bool("offline", false, "Check the form only, connect to nothing")
	validate.Flags().Bool("json", false, "Print the result as JSON")

	discoverCmd := &cobra.Command{
		Use:   "discover",
		Short: "Find mail accounts, folders and databases on this machine and add them as adapters",
		Long: "Look, read-only and locally, for mail accounts (Thunderbird, Evolution), document folders, network shares,\n" +
			"Postgres and MySQL databases (also in Docker containers). On a terminal, each one is proposed in turn;\n" +
			"each credential found is shown masked and saved in laker-agent.env only if you confirm it. Nothing is sent.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			asJSON, _ := cmd.Flags().GetBool("json")
			write, _ := cmd.Flags().GetString("write")
			interactive, _ := cmd.Flags().GetBool("interactive")
			commandDiscover(configPath, write, asJSON, interactive)
		},
	}
	discoverCmd.Flags().Bool("json", false, "Print what was found as JSON (secrets never included) and write nothing")
	discoverCmd.Flags().String("write", "", "configuration file to add the adapters to (default: the current one, or laker-agent.yaml)")
	discoverCmd.Flags().Bool("interactive", false, "Ask questions on standard input even when it is not a terminal")
	configCmd.AddCommand(generate, discoverCmd, validate)
	root.AddCommand(serveCmd, idCmd, runCmd, listCmd, versionCmd, configCmd)
	return root
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func mustConfig(path string) *config.AgentConfig {
	path, err := config.LocateAgent(path)
	if err != nil {
		fatalf("FTL800: %v", err)
	}
	if err := loadEnvFile(envPath(path)); err != nil {
		fatalf("FTL803: %v", err)
	}
	cfg, err := config.LoadAgent(path)
	if err == nil {
		err = config.ValidateAgent(cfg, sources.Types)
	}
	if err == nil {
		for _, a := range cfg.Adapters {
			if !a.Enabled {
				continue // checked once enabled: its variables may not be set yet
			}
			if _, berr := sources.Build(a, sources.Env{CommitEvery: 1}, zerolog.Nop()); berr != nil {
				err = errors.Join(err, fmt.Errorf("ERR802: adapter %s: %w", a.Name, berr))
			}
		}
	}
	if err != nil {
		fatalf("FTL801: invalid configuration:\n%v", err)
	}
	return cfg
}

func keyPath(cfg *config.AgentConfig) string   { return filepath.Join(cfg.Agent.DataDir, "agent.key") }
func statePath(cfg *config.AgentConfig) string { return filepath.Join(cfg.Agent.DataDir, "state.db") }
func tmpDir(cfg *config.AgentConfig) string    { return filepath.Join(cfg.Agent.DataDir, "tmp") }

func newLoggers(cfg *config.AgentConfig) *logger.Factory {
	lf, err := logger.New(version.AppInfo{
		Name: "Laker-agent-" + cfg.Agent.Name, Description: "Laker agent", AppFunction: "Agent",
		GoVersion: version.GetGoVersion(), Version: version.GetVersion(),
		CommitHash: version.GetCommitHash(), CommitTimestamp: version.GetCommitTimestamp(),
	}, logger.Options{
		Verbose: cfg.Agent.Log.Verbose, Use: cfg.Agent.Log.Use,
		BetterstackToken: cfg.Agent.Log.Betterstack.SourceToken, BetterstackHost: cfg.Agent.Log.Betterstack.IngestingHost,
	})
	if err != nil {
		fatalf("FTL802: cannot create the logger: %v", err)
	}
	return lf
}

func loadKey(cfg *config.AgentConfig) (*agentkey.Key, bool) {
	key, created, err := agentkey.LoadOrCreate(keyPath(cfg))
	if err != nil {
		fatalf("ERR803: %v", err)
	}
	return key, created
}

func commandID(configPath string) {
	cfg := mustConfig(configPath)
	key, _ := loadKey(cfg)
	fmt.Println(key.ID())
}

// agent is what serve and run share.
type agent struct {
	cfg       *config.AgentConfig
	lf        *logger.Factory
	key       *agentkey.Key
	client    *agentclient.Client
	runner    *core.Runner
	extractor *extract.Extractor
}

func newAgent(cfg *config.AgentConfig, lf *logger.Factory, wait time.Duration) *agent {
	if err := os.MkdirAll(cfg.Agent.DataDir, 0o700); err != nil {
		fatalf("ERR804: %v", err)
	}
	key, _ := loadKey(cfg)
	client, err := agentclient.New(cfg.Agent.Server, key, lf.For("SEND", color.New(color.FgGreen).SprintFunc()))
	if err != nil {
		fatalf("ERR805: %v", err)
	}
	stores := &core.Stores{OpenState: func() (out.StateStore, error) { return bbolt.Open(statePath(cfg), wait) }}
	runner := &core.Runner{
		Ingest: client,
		State: func() (out.StateStore, func(), error) {
			st, _, release, err := stores.Acquire()
			return st, release, err
		},
		Logger: lf.For("CORE", color.New(color.FgWhite).SprintFunc()),
	}
	return &agent{cfg: cfg, lf: lf, key: key, client: client, runner: runner, extractor: extract.New()}
}

func (a *agent) adapter(c config.Adapter) (core.Adapter, error) {
	src, err := sources.Build(c, sources.Env{
		CommitEvery: a.cfg.Agent.Defaults.CommitEvery, TmpDir: tmpDir(a.cfg), Extractor: a.extractor,
	}, a.lf.For("SRC:"+c.Name, color.New(color.FgCyan).SprintFunc()))
	if err != nil {
		return core.Adapter{}, fmt.Errorf("ERR806: adapter %s: %w", c.Name, err)
	}
	return core.Adapter{Name: c.Name, Type: c.Type, Source: src, Timeout: a.cfg.Agent.Defaults.Timeout}, nil
}

// waitApproval presents the agent until laker approves it.
func (a *agent) waitApproval(ctx context.Context, lg zerolog.Logger) error {
	host, _ := os.Hostname()
	for {
		reply, err := a.client.Hello(ctx, a.cfg.Agent.Name, host, version.GetVersion())
		switch {
		case err != nil:
			lg.Warn().Msgf("WRN800: laker is not reachable, retrying: %v", err)
		case reply.Status == domain.AgentApproved:
			lg.Info().Msg("INF801: approved by laker")
			return nil
		case reply.Status == domain.AgentPending:
			lg.Warn().Str("id", reply.ID).Msg("WRN801: waiting for approval: laker agents approve " + reply.ID)
		default:
			lg.Error().Str("id", reply.ID).Msgf("ERR807: laker has %s this agent", reply.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(helloEvery):
		}
	}
}

func commandServe(configPath string) {
	cfg := mustConfig(configPath)
	lf := newLoggers(cfg)
	defer lf.Close()
	_, created := loadKey(cfg)
	a := newAgent(cfg, lf, 2*time.Minute)
	defer a.extractor.Close()
	lg := lf.For("AGENT", color.New(color.FgWhite).SprintFunc())

	fmt.Printf("\n  laker-agent %q\n  ID:     %s\n  server: %s\n\n", cfg.Agent.Name, a.key.ID(), cfg.Agent.Server)
	if created {
		fmt.Printf("  new identity created in %s: keep this file, the ID depends on it\n\n", keyPath(cfg))
	}
	lg.Info().Str("id", a.key.ID()).Str("server", cfg.Agent.Server).Str("version", version.GetVersion()).Msg("INF800: starting")

	ctx, cancel := signaler.Context(context.Background(), lf.For("SIG", color.New(color.FgHiBlack).SprintFunc()))
	defer cancel()
	if err := a.waitApproval(ctx, lg); err != nil {
		return
	}

	loc, _ := time.LoadLocation(cfg.Agent.Timezone)
	var jobs []core.Job
	for _, c := range cfg.Adapters {
		if !c.Enabled {
			continue
		}
		ad, err := a.adapter(c)
		if err != nil {
			lg.Error().Msgf("ERR808: %v", err)
			return
		}
		var sched cron.Schedule
		if c.Schedule != config.ScheduleManual {
			sched, _ = config.CronParser.Parse(c.Schedule)
		}
		jobs = append(jobs, core.Job{Name: c.Name, Schedule: sched, Run: func(ctx context.Context) {
			if run := a.runner.Run(ctx, ad, false); agentclient.NotApproved(run.Error) {
				lg.Error().Msg("ERR809: laker no longer accepts this agent")
			}
		}})
	}
	lg.Info().Int("jobs", len(jobs)).Msg("INF802: running")
	core.NewScheduler(ctx, loc, lf.For("SCHED", color.New(color.FgYellow).SprintFunc()), jobs).Serve()
	lg.Info().Msg("INF803: stopped")
}

func commandRun(configPath, name string, dryRun bool) {
	cfg := mustConfig(configPath)
	var c *config.Adapter
	for i := range cfg.Adapters {
		if cfg.Adapters[i].Name == name {
			c = &cfg.Adapters[i]
		}
	}
	if c == nil {
		fatalf("ERR80A: no adapter named %q in the configuration", name)
	}
	lf := newLoggers(cfg)
	defer lf.Close()
	a := newAgent(cfg, lf, 5*time.Second)
	defer a.extractor.Close()
	ad, err := a.adapter(*c)
	if err != nil {
		fatalf("ERR80B: %v", err)
	}
	ctx, cancel := signaler.Context(context.Background(), lf.For("SIG", color.New(color.FgHiBlack).SprintFunc()))
	defer cancel()
	if !dryRun {
		host, _ := os.Hostname()
		reply, err := a.client.Hello(ctx, cfg.Agent.Name, host, version.GetVersion())
		if err != nil {
			fatalf("ERR821: laker is not reachable: %v", err)
		}
		if reply.Status != domain.AgentApproved {
			fatalf("ERR820: this agent is %s on laker; approve it there with: laker agents approve %s", reply.Status, reply.ID)
		}
	}
	if run := a.runner.Run(ctx, ad, dryRun); run.Status != domain.RunOK {
		os.Exit(1)
	}
}

func commandList(configPath string) {
	cfg := mustConfig(configPath)
	state, err := bbolt.Open(statePath(cfg), 5*time.Second)
	if err != nil {
		fatalf("ERR80C: %v", err)
	}
	defer state.Close()
	loc, _ := time.LoadLocation(cfg.Agent.Timezone)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "NAME\tTYPE\tENABLED\tSCHEDULE\tLAST RUN\tSTATUS\tROWS")
	for _, c := range cfg.Adapters {
		last, status, rows := "-", "-", "-"
		if r, err := core.LastRun(state, c.Name); err == nil && r != nil {
			last, status, rows = r.EndedAt.In(loc).Format("2006-01-02 15:04"), string(r.Status), fmt.Sprint(r.Stats.Rows)
		}
		fmt.Fprintf(w, "%s\t%s\t%v\t%s\t%s\t%s\t%s\n", c.Name, c.Type, c.Enabled, c.Schedule, last, status, rows)
	}
}
