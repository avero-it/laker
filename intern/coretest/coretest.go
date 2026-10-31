// Package coretest wires a real Runner on temporary directories for tests:
// an fs lake, a bbolt state and a Bleve index.
package coretest

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/adapters/indexer/bleve"
	"github.com/avero-it/laker/intern/adapters/lake/fs"
	"github.com/avero-it/laker/intern/adapters/state/bbolt"
	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/ports/out"
)

type Env struct {
	Dir    string
	Lake   *fs.Lake
	Stores *core.Stores
	Runner *core.Runner
}

func New(t *testing.T) *Env {
	t.Helper()
	dir := t.TempDir()
	lake := fs.New(filepath.Join(dir, "lake"), zerolog.Nop())
	stores := &core.Stores{
		OpenState: func() (out.StateStore, error) { return bbolt.Open(filepath.Join(dir, "state.db"), time.Second) },
		OpenIndex: func() (out.Indexer, error) { return bleve.Open(filepath.Join(dir, "index"), time.Second) },
	}
	return &Env{Dir: dir, Lake: lake, Stores: stores, Runner: core.LocalRunner(lake, stores, zerolog.Nop())}
}

// With runs fn with the stores open.
func (e *Env) With(t *testing.T, fn func(state out.StateStore, index out.Indexer)) {
	t.Helper()
	st, idx, release, err := e.Stores.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fn(st, idx)
}
