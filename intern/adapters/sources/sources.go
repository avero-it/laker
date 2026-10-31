// Package sources builds the source adapters from their configuration; laker
// and laker-agent share it, so both run exactly the same sources.
package sources

import (
	"fmt"
	"sort"

	"github.com/rs/zerolog"

	fssource "github.com/avero-it/laker/intern/adapters/sources/fs"
	"github.com/avero-it/laker/intern/adapters/sources/imap"
	"github.com/avero-it/laker/intern/adapters/sources/sqldb"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/extract"
)

// Types are the adapter types this build knows.
var Types = map[string]bool{
	imap.Type:          true,
	fssource.Type:      true,
	sqldb.TypePostgres: true,
	sqldb.TypeMySQL:    true,
}

func Sorted() []string {
	types := make([]string, 0, len(Types))
	for t := range Types {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// Env is what the sources share in a process.
type Env struct {
	CommitEvery int
	TmpDir      string
	Extractor   *extract.Extractor
}

func Build(a config.Adapter, env Env, lg zerolog.Logger) (out.Source, error) {
	switch a.Type {
	case imap.Type:
		return imap.New(a.Name, a.Params, env.CommitEvery, lg)
	case fssource.Type:
		return fssource.New(a.Name, a.Params, env.CommitEvery, env.TmpDir, env.Extractor, lg)
	case sqldb.TypePostgres, sqldb.TypeMySQL:
		return sqldb.New(a.Name, a.Type, a.Params, env.CommitEvery, lg)
	default:
		return nil, fmt.Errorf("ERR2F0: unknown adapter type %q", a.Type)
	}
}
