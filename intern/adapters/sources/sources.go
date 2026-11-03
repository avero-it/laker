// Package sources builds the source adapters from their configuration; laker
// and laker-agent share it, so both run exactly the same sources.
package sources

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rs/zerolog"

	fssource "github.com/avero-it/laker/intern/adapters/sources/fs"
	"github.com/avero-it/laker/intern/adapters/sources/imap"
	"github.com/avero-it/laker/intern/adapters/sources/sqldb"
	"github.com/avero-it/laker/intern/adapters/sources/telegram"
	"github.com/avero-it/laker/intern/core/domain"
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
	telegram.Type:      true,
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
	// DataDir keeps what a source must remember beyond its state, such as
	// the Telegram session.
	DataDir   string
	TmpDir    string
	Extractor *extract.Extractor
}

// Stubs are one documented adapter of every type, for `config generate`.
func Stubs() []config.GenAdapter {
	return []config.GenAdapter{imap.Stub(), fssource.Stub(), sqldb.Stub(sqldb.TypePostgres), sqldb.Stub(sqldb.TypeMySQL), telegram.Stub()}
}

// ProbeTimeout bounds one live check.
const ProbeTimeout = 30 * time.Second

// Probe builds the adapter and checks that it reaches its origin.
func Probe(ctx context.Context, a config.Adapter, env Env) (domain.Probe, error) {
	src, err := Build(a, env, zerolog.Nop())
	if err != nil {
		return domain.Probe{}, err
	}
	p, ok := src.(out.Prober)
	if !ok {
		return domain.Probe{Summary: "no live check for this type"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	return p.Probe(ctx)
}

func Build(a config.Adapter, env Env, lg zerolog.Logger) (out.Source, error) {
	switch a.Type {
	case imap.Type:
		return imap.New(a.Name, a.Params, env.CommitEvery, lg)
	case fssource.Type:
		return fssource.New(a.Name, a.Params, env.CommitEvery, env.TmpDir, env.Extractor, lg)
	case sqldb.TypePostgres, sqldb.TypeMySQL:
		return sqldb.New(a.Name, a.Type, a.Params, env.CommitEvery, lg)
	case telegram.Type:
		return telegram.New(a.Name, a.Params, env.CommitEvery, env.DataDir, env.TmpDir, lg)
	default:
		return nil, fmt.Errorf("ERR2F0: unknown adapter type %q", a.Type)
	}
}
