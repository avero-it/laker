package domain

import "time"

type RunStatus string

const (
	RunOK      RunStatus = "ok"
	RunFailed  RunStatus = "failed"
	RunSkipped RunStatus = "skipped"
)

// Run is one execution of an adapter.
type Run struct {
	ID        string        `json:"id"`
	Adapter   string        `json:"adapter"`
	Type      string        `json:"type"`
	StartedAt time.Time     `json:"started_at"`
	EndedAt   time.Time     `json:"ended_at"`
	Status    RunStatus     `json:"status"`
	Stats     RunStats      `json:"stats"`
	Error     string        `json:"error,omitempty"`
	DryRun    bool          `json:"dry_run,omitempty"`
	Duration  time.Duration `json:"-"`
}

type RunStats struct {
	Rows       int   `json:"rows"`
	Files      int   `json:"files"`
	BlobsNew   int   `json:"blobs_new"`
	BlobsDedup int   `json:"blobs_dedup"`
	Bytes      int64 `json:"bytes"`
	Commits    int   `json:"commits"`
}

// Manifest is written to _runs/<adapter>/<run>-<seq>.json at every commit:
// what the commit added to the lake.
type Manifest struct {
	Run        string    `json:"run"`
	Seq        int       `json:"seq"`
	Adapter    string    `json:"adapter"`
	Type       string    `json:"type"`
	At         time.Time `json:"at"`
	Files      []string  `json:"files"`
	Rows       int       `json:"rows"`
	BlobsNew   int       `json:"blobs_new"`
	BlobsDedup int       `json:"blobs_dedup"`
	Bytes      int64     `json:"bytes"`
}
