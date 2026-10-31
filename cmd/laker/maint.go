package main

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/fatih/color"

	"github.com/avero-it/laker/intern/adapters/indexer/bleve"
	"github.com/avero-it/laker/intern/adapters/state/bbolt"
	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/pkg/signaler"
)

// The local state and index are derived data in the data directory, never
// part of the lake: replacing them here removes nothing from the lake.

func commandReindex(configPath string) {
	cfg := mustConfig(configPath)
	lf := newLoggers(cfg)
	defer lf.Close()
	lg := lf.For("INDEX", color.New(color.FgMagenta).SprintFunc())
	ctx, cancel := signaler.Context(context.Background(), lf.For("SIG", color.New(color.FgHiBlack).SprintFunc()))
	defer cancel()

	lake, err := buildLake(cfg, lf.For("LAKE", color.New(color.FgGreen).SprintFunc()))
	if err != nil {
		fatalf("ERR070: %v", err)
	}

	// take the lock of the current index before replacing it
	old, err := bleve.Open(indexPath(cfg), cliWait)
	if err != nil {
		fatalf("ERR071: %v", err)
	}
	old.Close()
	if err := os.RemoveAll(indexPath(cfg)); err != nil {
		fatalf("ERR072: cannot remove the local index: %v", err)
	}

	idx, err := bleve.Open(indexPath(cfg), cliWait)
	if err != nil {
		fatalf("ERR073: %v", err)
	}
	defer idx.Close()
	mails, files, err := core.Reindex(ctx, lake, idx, lg)
	if err != nil {
		fatalf("ERR074: reindex failed: %v", err)
	}
	lg.Info().Int("mails", mails).Int("files", files).Msg("INF070: index rebuilt from the lake")
}

func commandStateRebuild(configPath string, force bool) {
	cfg := mustConfig(configPath)
	lf := newLoggers(cfg)
	defer lf.Close()
	lg := lf.For("STATE", color.New(color.FgHiBlack).SprintFunc())

	if _, err := os.Stat(statePath(cfg)); err == nil {
		if !force {
			fatalf("ERR075: %s exists; use --force to replace it", statePath(cfg))
		}
		st, err := bbolt.Open(statePath(cfg), cliWait)
		if err != nil {
			fatalf("ERR076: %v", err)
		}
		st.Close()
		if err := os.Remove(statePath(cfg)); err != nil {
			fatalf("ERR077: %v", err)
		}
	}
	if err := os.MkdirAll(cfg.Common.DataDir, 0o750); err != nil {
		fatalf("ERR078: %v", err)
	}

	lake, err := buildLake(cfg, lf.For("LAKE", color.New(color.FgGreen).SprintFunc()))
	if err != nil {
		fatalf("ERR079: %v", err)
	}
	st, err := bbolt.Open(statePath(cfg), cliWait)
	if err != nil {
		fatalf("ERR07A: %v", err)
	}
	defer st.Close()
	n, err := core.RebuildState(context.Background(), lake, st)
	if err != nil {
		fatalf("ERR07B: %v", err)
	}
	lg.Info().Int("keys", n).Msg("INF071: state rebuilt from the lake")
}

func commandSnapshot(configPath string) {
	cfg := mustConfig(configPath)
	if runInServe(cfg, "snapshot") {
		return
	}
	lf := newLoggers(cfg)
	defer lf.Close()
	lake, err := buildLake(cfg, lf.For("LAKE", color.New(color.FgGreen).SprintFunc()))
	if err != nil {
		fatalf("ERR07C: %v", err)
	}
	if err := snapshot(context.Background(), cfg, lake, buildStores(cfg, cliWait), lf.For("SNAP", color.New(color.FgHiBlack).SprintFunc())); err != nil {
		fatalf("ERR07D: %v", err)
	}
}

func commandRestore(configPath string) {
	cfg := mustConfig(configPath)
	lf := newLoggers(cfg)
	defer lf.Close()
	lg := lf.For("SNAP", color.New(color.FgHiBlack).SprintFunc())

	for _, p := range []string{statePath(cfg), indexPath(cfg)} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			fatalf("ERR07E: %s exists: restore only fills an empty data directory", p)
		}
	}
	lake, err := buildLake(cfg, lf.For("LAKE", color.New(color.FgGreen).SprintFunc()))
	if err != nil {
		fatalf("ERR07F: %v", err)
	}
	keys, err := core.Restore(context.Background(), lake, statePath(cfg), indexPath(cfg))
	if err != nil {
		fatalf("ERR080: %v", err)
	}
	lg.Info().Strs("keys", keys).Msg("INF072: restored; runs after the snapshot are read again at the next run (blobs are not duplicated)")
}
