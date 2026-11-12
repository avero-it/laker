package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	switch {
	case errors.Is(err, bleve.ErrOutdated): // built by an older laker: replaced below
	case err != nil:
		fatalf("ERR071: %v", err)
	default:
		old.Close()
	}
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

	// the new index is not in any snapshot yet, though the lake has not changed
	if _, err := os.Stat(statePath(cfg)); err == nil {
		st, err := bbolt.Open(statePath(cfg), cliWait)
		if err == nil {
			err = core.SnapshotDue(st)
			st.Close()
		}
		if err != nil {
			lg.Warn().Msgf("WRN003: the next snapshot may be skipped, run laker snapshot after the next ingest: %v", err)
		}
	}
}

func commandStateRebuild(configPath string, force bool) {
	cfg := mustConfig(configPath)
	lf := newLoggers(cfg)
	defer lf.Close()
	lg := lf.For("STATE", color.New(color.FgHiBlack).SprintFunc())

	exists := false
	if _, err := os.Stat(statePath(cfg)); err == nil {
		if !force {
			fatalf("ERR075: %s exists; use --force to replace it", statePath(cfg))
		}
		// no other process may hold the state that is about to be replaced
		st, err := bbolt.Open(statePath(cfg), cliWait)
		if err != nil {
			fatalf("ERR076: %v", err)
		}
		st.Close()
		exists = true
	}
	if err := os.MkdirAll(cfg.Common.DataDir, 0o750); err != nil {
		fatalf("ERR078: %v", err)
	}

	lake, err := buildLake(cfg, lf.For("LAKE", color.New(color.FgGreen).SprintFunc()))
	if err != nil {
		fatalf("ERR079: %v", err)
	}
	// the rebuild writes in several transactions: it goes into a scratch file
	// that becomes the state only when complete, so a rebuild that fails
	// leaves the previous state (or none) in place
	scratch := statePath(cfg) + ".rebuild"
	if err := os.Remove(scratch); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fatalf("ERR077: %v", err)
	}
	st, err := bbolt.Open(scratch, cliWait)
	if err != nil {
		fatalf("ERR07A: %v", err)
	}
	n, err := core.RebuildState(context.Background(), lake, st)
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(scratch) // local scratch file, not the lake
		fatalf("ERR07B: %v", err)
	}
	if err := os.Rename(scratch, statePath(cfg)); err != nil {
		fatalf("ERR081: cannot put the rebuilt state in place: %v", err)
	}
	lg.Info().Int("changes", n).Bool("replaced", exists).Msg("INF071: state rebuilt from the lake")
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

// commandVerify reads the lake and reports what is missing or damaged; it
// opens neither the state nor the index.
func commandVerify(configPath string, hash int, asJSON bool) {
	cfg := mustConfig(configPath)
	lf := newLoggers(cfg)
	defer lf.Close()
	lg := lf.For("VERIFY", color.New(color.FgHiBlack).SprintFunc())
	ctx, cancel := signaler.Context(context.Background(), lf.For("SIG", color.New(color.FgHiBlack).SprintFunc()))
	defer cancel()

	lake, err := buildLake(cfg, lf.For("LAKE", color.New(color.FgGreen).SprintFunc()))
	if err != nil {
		fatalf("ERR0C0: %v", err)
	}
	opts := core.VerifyOptions{Hash: hash}
	if !asJSON { // the logs share stdout with the report: JSON goes alone
		opts.Progress = func(step string) { lg.Info().Msgf("INF00A: %s", step) }
	}
	rep, err := core.Verify(ctx, lake, opts)
	if err != nil {
		fatalf("ERR0C1: the lake could not be verified: %v", err)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		printVerify(os.Stdout, rep)
	}
	if !rep.OK() {
		lf.Close()
		os.Exit(1)
	}
}

func printVerify(w io.Writer, rep core.VerifyReport) {
	fmt.Fprintf(w, "%d manifests, %d Parquet files, %d contents (%d named by rows), %d hashed again\n",
		rep.Manifests, rep.ParquetFiles, rep.Blobs, rep.BlobRefs, rep.Hashed)
	problem := func(what string, p core.Problems) {
		if p.Count == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s: %d\n", what, p.Count)
		for _, e := range p.Examples {
			fmt.Fprintf(w, "  %s\n", e)
		}
		if p.Count > len(p.Examples) {
			fmt.Fprintf(w, "  … and %d more\n", p.Count-len(p.Examples))
		}
	}
	problem("MANIFESTS THAT CANNOT BE READ", rep.BadManifests)
	problem("FILES OF A MANIFEST MISSING FROM THE LAKE", rep.MissingFiles)
	problem("PARQUET FILES THAT CANNOT BE READ", rep.BadParquet)
	problem("CONTENTS NAMED BY A ROW MISSING FROM THE LAKE", rep.MissingBlobs)
	problem("CONTENTS THAT DO NOT HASH TO THEIR NAME", rep.CorruptBlobs)
	problem("OBJECTS UNDER blobs/ THAT ARE NOT CONTENTS", rep.MisplacedKeys)
	if rep.UnlistedFiles.Count > 0 || rep.UnreferencedBlobs > 0 {
		fmt.Fprintf(w, "\nleft by interrupted runs, harmless: %d Parquet files of no manifest (their rows are read all the same), %d contents no row names\n",
			rep.UnlistedFiles.Count, rep.UnreferencedBlobs)
	}
	if rep.OK() {
		fmt.Fprintln(w, "\nthe lake is sound")
	} else {
		fmt.Fprintln(w, "\nTHE LAKE IS NOT SOUND")
	}
}
