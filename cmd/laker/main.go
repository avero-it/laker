package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	_ "time/tzdata" // schedules run in common.timezone on hosts without a tz database

	"github.com/fatih/color"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/logger"
	"github.com/avero-it/laker/pkg/version"
)

func main() {
	if err := setupCLI().Execute(); err != nil {
		os.Exit(2)
	}
}

func setupCLI() *cobra.Command {
	var configPath string

	rootCmd := &cobra.Command{
		Use:          "laker",
		Short:        "Laker - personal data lake",
		Long:         "Laker ingests mailboxes, files and databases into an append-only lake on S3, and makes all of it searchable.",
		SilenceUsage: true,
	}
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "", "configuration file (default: /etc/laker/config.yaml, then ./config.yaml)")

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the scheduler with every enabled adapter",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			if check, _ := cmd.Flags().GetBool("check"); check {
				commandCheck(configPath)
				return
			}
			commandServe(configPath)
		},
	}
	serveCmd.Flags().Bool("check", false, "Run the init phase, reach the lake, print "+checkResultMarker+"{json} and exit, without serving")

	runCmd := &cobra.Command{
		Use:   "run <adapter>",
		Short: "Run one adapter now, once",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			dry, _ := cmd.Flags().GetBool("dry-run")
			commandRun(configPath, args[0], dry)
		},
	}
	runCmd.Flags().Bool("dry-run", false, "Read and compare, write nothing (no blob, row, index or state)")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the adapters with their schedule and last run",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) { commandList(configPath) },
	}

	searchCmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search in mail and files",
		Long: "Search the index. Free words match subject, body, attachments, file names and paths (Italian stemming).\n" +
			"Fields: from: to: subject: body: account: folder: path: name: ext: kind:mail|file. " +
			"+word must match, -word must not, \"a phrase\", date:>\"2025-01-01\".",
		Args: cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			kind, _ := cmd.Flags().GetString("kind")
			limit, _ := cmd.Flags().GetInt("limit")
			asJSON, _ := cmd.Flags().GetBool("json")
			commandSearch(configPath, args, kind, limit, asJSON)
		},
	}
	searchCmd.Flags().String("kind", "", "mail or file")
	searchCmd.Flags().Int("limit", 20, "maximum number of results")
	searchCmd.Flags().Bool("json", false, "print the results as JSON")

	showCmd := &cobra.Command{
		Use:   "show <sha256>",
		Short: "Show a message (headers and text) or where a file content is",
		Args:  cobra.ExactArgs(1),
		Run:   func(cmd *cobra.Command, args []string) { commandShow(configPath, args[0]) },
	}

	getCmd := &cobra.Command{
		Use:   "get <sha256 | source:path>",
		Short: "Download an original (.eml or file) from the lake",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			at, _ := cmd.Flags().GetString("at")
			outPath, _ := cmd.Flags().GetString("output")
			commandGet(configPath, args[0], at, outPath)
		},
	}
	getCmd.Flags().String("at", "", "version of source:path at this date (YYYY-MM-DD or RFC 3339)")
	getCmd.Flags().StringP("output", "o", "", "write to this file instead of stdout")

	lsCmd := &cobra.Command{
		Use:   "ls <source>[:<path>]",
		Short: "Browse the files of a source, now or at a past date",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			at, _ := cmd.Flags().GetString("at")
			commandLs(configPath, args[0], at)
		},
	}
	lsCmd.Flags().String("at", "", "show the tree as it was at this date (YYYY-MM-DD or RFC 3339)")

	reindexCmd := &cobra.Command{
		Use:   "reindex",
		Short: "Rebuild the search index from the lake",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) { commandReindex(configPath) },
	}

	stateCmd := &cobra.Command{Use: "state", Short: "Local state commands"}
	stateRebuildCmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the local state (IMAP checkpoints, file inventories) from the lake",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			force, _ := cmd.Flags().GetBool("force")
			commandStateRebuild(configPath, force)
		},
	}
	stateRebuildCmd.Flags().Bool("force", false, "Replace the existing local state file")
	stateCmd.AddCommand(stateRebuildCmd)

	snapshotCmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Copy the local state and search index to the lake now",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) { commandSnapshot(configPath) },
	}
	restoreCmd := &cobra.Command{
		Use:   "restore",
		Short: "Download the latest snapshot of the state and index from the lake",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) { commandRestore(configPath) },
	}

	viewsCmd := &cobra.Command{
		Use:   "views",
		Short: "Print the DuckDB SQL that exposes the lake as views (mail, files, database tables)",
		Long: "Print SQL for DuckDB: views over every message, the file history and the extracted texts and, for each\n" +
			"database table, <name> (latest rows), <name>_history (every change) and <name>_at(ts) (rows at a date).\n" +
			"Usage: laker views > lake.sql && AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… duckdb -init lake.sql",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			secrets, _ := cmd.Flags().GetBool("with-secrets")
			commandViews(configPath, secrets)
		},
	}
	viewsCmd.Flags().Bool("with-secrets", false, "Write the S3 keys in the SQL instead of reading them from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")

	agentsCmd := &cobra.Command{
		Use:   "agents",
		Short: "List the laker-agents that contacted this laker",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) { commandAgentsList(configPath) },
	}
	agentsCmd.AddCommand(&cobra.Command{
		Use:   "approve <id>",
		Short: "Let an agent send data (check the id against what laker-agent shows at start)",
		Args:  cobra.ExactArgs(1),
		Run:   func(cmd *cobra.Command, args []string) { commandAgentsSet(configPath, args[0], domain.AgentApproved) },
	}, &cobra.Command{
		Use:   "revoke <id>",
		Short: "Refuse every further request of an agent",
		Args:  cobra.ExactArgs(1),
		Run:   func(cmd *cobra.Command, args []string) { commandAgentsSet(configPath, args[0], domain.AgentRevoked) },
	})

	typesCmd := &cobra.Command{
		Use:   "types",
		Short: "List the adapter types this build knows",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			for _, t := range sortedTypes() {
				fmt.Println(t)
			}
		},
	}

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) { commandVersion() },
	}

	configCmd := &cobra.Command{Use: "config", Short: "Configuration management commands"}
	configCreateCmd := &cobra.Command{
		Use:   "create",
		Short: "Write an example config.yaml in the working directory",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			force, _ := cmd.Flags().GetBool("force")
			commandConfigCreate(force)
		},
	}
	configCreateCmd.Flags().Bool("force", false, "Overwrite an existing config.yaml")
	configValidateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Load and validate the configuration without starting anything",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) { commandConfigValidate(configPath) },
	}
	configCmd.AddCommand(configCreateCmd, configValidateCmd)

	rootCmd.AddCommand(serveCmd, runCmd, listCmd, searchCmd, showCmd, getCmd, lsCmd,
		reindexCmd, stateCmd, snapshotCmd, restoreCmd, viewsCmd, agentsCmd, typesCmd, versionCmd, configCmd)
	return rootCmd
}

func appInfo(cfg *config.Config) version.AppInfo {
	return version.AppInfo{
		Name:            "Laker-" + cfg.Common.ID.Name,
		Description:     "Laker personal data lake",
		AppFunction:     "Ingestion",
		GoVersion:       version.GetGoVersion(),
		Version:         version.GetVersion(),
		CommitHash:      version.GetCommitHash(),
		CommitTimestamp: version.GetCommitTimestamp(),
	}
}

func consoleLogger() zerolog.Logger {
	return logger.Console("LAKER", color.New(color.FgWhite).SprintFunc())
}

// loadConfig finds, decodes and validates the configuration, adapter params
// included.
func loadConfig(path string) (*config.Config, error) {
	path, err := config.Locate(path)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := config.Validate(cfg, sourceTypes); err != nil {
		return nil, err
	}
	var errs []error
	for _, a := range cfg.Adapters {
		if _, err := buildSource(cfg, a, zerolog.Nop()); err != nil {
			errs = append(errs, fmt.Errorf("ERR013: adapter %s: %w", a.Name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// newLoggers builds the logger factory of a command that works on the lake.
func newLoggers(cfg *config.Config) *logger.Factory {
	lf, err := logger.New(appInfo(cfg), logger.Options{
		Verbose:          cfg.Common.Log.Verbose,
		Use:              cfg.Common.Log.Use,
		BetterstackToken: cfg.Common.Log.Betterstack.SourceToken,
		BetterstackHost:  cfg.Common.Log.Betterstack.IngestingHost,
	})
	if err != nil {
		lg := consoleLogger()
		lg.Fatal().Msgf("FTL002: cannot create the logger: %v", err)
	}
	return lf
}

// mustConfig loads the configuration or exits.
func mustConfig(path string) *config.Config {
	cfg, err := loadConfig(path)
	if err != nil {
		lg := consoleLogger()
		lg.Fatal().Msgf("FTL001: invalid configuration:\n%v", err)
	}
	return cfg
}

func commandVersion() {
	fmt.Printf("Laker - personal data lake\n\n")
	fmt.Printf("Version:        %s\n", version.GetVersion())
	fmt.Printf("Git Commit:     %s\n", version.GetCommitHash())
	fmt.Printf("Build Time:     %s\n", version.GetCommitTimestamp())
	fmt.Printf("Go Version:     %s\n", runtime.Version())
}

func commandConfigCreate(force bool) {
	lg := consoleLogger()
	const path = "config.yaml"
	if _, err := os.Stat(path); err == nil && !force {
		lg.Error().Msgf("ERR004: %s exists, use --force to overwrite it", path)
		os.Exit(1)
	}
	if err := os.WriteFile(path, config.Example, 0o600); err != nil {
		lg.Error().Msgf("ERR005: cannot write %s: %v", path, err)
		os.Exit(1)
	}
	lg.Info().Msgf("INF005: wrote %s", path)
}

func commandConfigValidate(configPath string) {
	lg := consoleLogger()
	cfg, err := loadConfig(configPath)
	if err != nil {
		lg.Error().Msgf("ERR006: invalid configuration:\n%v", err)
		os.Exit(1)
	}
	lg.Info().Str("config_hash", config.Hash(cfg)).Int("adapters", len(cfg.Adapters)).Msg("INF006: configuration is valid")
}
