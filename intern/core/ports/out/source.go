package out

import (
	"context"
	"io"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/pkg/parquetx"
)

// Source reads what is new at one origin (a mailbox, a tree, a database) and
// hands it to the RunSink of the run.
type Source interface {
	Run(ctx context.Context, rs RunSink) error
}

// RunSink is what a run writes through. Blobs go to the lake at once (they are
// idempotent by content); rows, index documents and state changes are staged
// and land together at Commit, state last, so a crash replays at most the
// uncommitted part.
type RunSink interface {
	parquetx.Tables

	RunID() string
	// PutBlob stores size bytes of r under the blob key of sha256 (lowercase
	// hex, computed by the caller over exactly those bytes).
	PutBlob(ctx context.Context, sha256 string, size int64, r io.Reader) (created bool, err error)

	IndexMail(doc domain.MailDoc)
	IndexFile(doc domain.FileDoc)
	UnindexFile(id string)

	// State is a key/value space private to the adapter.
	GetState(key string) ([]byte, error)
	ForEachState(prefix string, fn func(key string, value []byte) error) error
	SetState(key string, value []byte)
	DeleteState(key string)

	Commit(ctx context.Context) error
}
