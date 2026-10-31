package out

import (
	"context"
	"io"

	"github.com/avero-it/laker/intern/core/domain"
)

// StateChange sets Key in Bucket to Value, or deletes it if Value is nil.
type StateChange struct {
	Bucket string
	Key    string
	Value  []byte
}

// StateStore is the local key/value state (checkpoints, inventories). It is
// derived from the lake and can be rebuilt from it.
type StateStore interface {
	Get(bucket, key string) ([]byte, error)
	ForEach(bucket, prefix string, fn func(key string, value []byte) error) error
	// Apply makes all changes atomically.
	Apply(changes []StateChange) error
	// Snapshot writes a consistent copy of the whole store.
	Snapshot(w io.Writer) (int64, error)
	Close() error
}

// Indexer is the full-text index, derived from the lake.
type Indexer interface {
	// Apply upserts docs (id → document) and removes ids in one batch.
	Apply(ctx context.Context, docs map[string]any, deletes []string) error
	Search(ctx context.Context, q domain.Query) (domain.SearchResult, error)
	// Document returns the stored fields of id, nil if it is not indexed.
	Document(ctx context.Context, id string) (map[string]any, error)
	// Snapshot copies the index, online, into the directory dir.
	Snapshot(dir string) error
	Close() error
}
