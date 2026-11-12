package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/adapters/indexer/bleve"
	"github.com/avero-it/laker/intern/adapters/lake/fs"
	"github.com/avero-it/laker/intern/adapters/lake/s3"
	"github.com/avero-it/laker/intern/adapters/state/bbolt"
	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/config"
)

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

func statePath(cfg *config.Config) string { return filepath.Join(cfg.Common.DataDir, "state.db") }
func indexPath(cfg *config.Config) string { return filepath.Join(cfg.Common.DataDir, "index") }

// oauthPath holds the OAuth clients and tokens of MCP: not derived from the
// lake, so neither snapshotted nor rebuilt.
func oauthPath(cfg *config.Config) string { return filepath.Join(cfg.Common.DataDir, "oauth.db") }
func tmpDir(cfg *config.Config) string    { return filepath.Join(cfg.Common.DataDir, "tmp") }

// buildStores opens the state and the index lazily; wait is how long to wait
// for another laker process to release them.
func buildStores(cfg *config.Config, wait time.Duration) *core.Stores {
	return &core.Stores{
		OpenState: func() (out.StateStore, error) { return bbolt.Open(statePath(cfg), wait) },
		OpenIndex: func() (out.Indexer, error) { return bleve.Open(indexPath(cfg), wait) },
	}
}
