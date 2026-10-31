package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/fatih/color"
	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/adapters/indexer/bleve"
	"github.com/avero-it/laker/intern/adapters/lake/fs"
	"github.com/avero-it/laker/intern/adapters/lake/s3"
	"github.com/avero-it/laker/intern/adapters/sources"
	"github.com/avero-it/laker/intern/adapters/state/bbolt"
	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/extract"
	"github.com/avero-it/laker/pkg/logger"
)

// sourceTypes are the adapter types this build knows.
var sourceTypes = sources.Types

// extractor is shared by every source of the process; its PDF engine starts
// on the first PDF and is released by closeExtractor.
var extractor = extract.New()

func closeExtractor() { extractor.Close() }

func sortedTypes() []string { return sources.Sorted() }

func buildLake(cfg *config.Config, lg zerolog.Logger) (out.Lake, error) {
	switch l := cfg.Common.Lake; l.Use {
	case "s3":
		return s3.New(s3.Options{
			Endpoint:       l.S3.Endpoint,
			Region:         l.S3.Region,
			Bucket:         l.S3.Bucket,
			AccessKey:      l.S3.AccessKey,
			SecretKey:      l.S3.SecretKey,
			PathStyle:      l.S3.PathStyle,
			ConditionalPut: l.S3.ConditionalPut,
		}, lg), nil
	case "fs":
		return fs.New(l.FS.Path, lg), nil
	default:
		return nil, fmt.Errorf("ERR010: unsupported lake %q", l.Use)
	}
}

func buildSource(cfg *config.Config, a config.Adapter, lg zerolog.Logger) (out.Source, error) {
	return sources.Build(a, sources.Env{CommitEvery: cfg.Common.Defaults.CommitEvery, TmpDir: tmpDir(cfg), Extractor: extractor}, lg)
}

// buildAdapter is buildSource with what the runner needs around it.
func buildAdapter(cfg *config.Config, a config.Adapter, lf *logger.Factory) (core.Adapter, error) {
	src, err := buildSource(cfg, a, lf.For("SRC:"+a.Name, color.New(color.FgCyan).SprintFunc()))
	if err != nil {
		return core.Adapter{}, fmt.Errorf("ERR012: adapter %s: %w", a.Name, err)
	}
	return core.Adapter{Name: a.Name, Type: a.Type, Source: src, Timeout: cfg.Common.Defaults.Timeout}, nil
}

func statePath(cfg *config.Config) string { return filepath.Join(cfg.Common.DataDir, "state.db") }
func indexPath(cfg *config.Config) string { return filepath.Join(cfg.Common.DataDir, "index") }
func tmpDir(cfg *config.Config) string    { return filepath.Join(cfg.Common.DataDir, "tmp") }

// buildStores opens the state and the index lazily; wait is how long to wait
// for another laker process to release them.
func buildStores(cfg *config.Config, wait time.Duration) *core.Stores {
	return &core.Stores{
		OpenState: func() (out.StateStore, error) { return bbolt.Open(statePath(cfg), wait) },
		OpenIndex: func() (out.Indexer, error) { return bleve.Open(indexPath(cfg), wait) },
	}
}
