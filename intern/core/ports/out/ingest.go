package out

import (
	"context"
	"io"

	"github.com/avero-it/laker/intern/core/domain"
)

// Ingest is where a run delivers what it read: blobs, parquet files, index
// changes and the run record. laker serve ingests its own adapters in-process;
// an agent ingests through laker serve's API.
type Ingest interface {
	BlobExists(ctx context.Context, sha256 string) (bool, error)
	// PutBlob stores size bytes of r, whose SHA-256 is sha256; created is
	// false if the content was already in the lake.
	PutBlob(ctx context.Context, sha256 string, size int64, r io.Reader) (created bool, err error)
	Commit(ctx context.Context, c domain.Commit) error
	Finish(ctx context.Context, run domain.Run) error
}
