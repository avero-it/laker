package out

import (
	"context"
	"io"

	"github.com/avero-it/laker/intern/core/domain"
)

// Lake is the append-only store everything ingested ends up in.
//
// There is no Delete and no overwrite, by contract: nothing written to the
// lake is ever removed or changed. Keys are deterministic (content hashes,
// run IDs), so writing the same key twice is harmless and reports created=false.
type Lake interface {
	// PutIfAbsent writes size bytes from r at key, unless key exists already,
	// in which case the object is left untouched and created is false.
	PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) (created bool, err error)
	Exists(ctx context.Context, key string) (bool, error)
	// Get opens the object at key; the caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List calls fn for every key starting with prefix, in lexical order.
	List(ctx context.Context, prefix string, fn func(key string) error) error
	// Check reaches the lake without writing to it.
	Check(ctx context.Context) (domain.LakeInfo, error)
}
