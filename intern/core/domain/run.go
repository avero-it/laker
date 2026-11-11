package domain

import (
	"fmt"
	"time"
)

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

// ManifestKey is where the manifest of a commit goes; a commit sent in
// pieces has one for each piece after the first, <run>-<seq>-p<part>.json.
func ManifestKey(adapter, run string, seq, part int) string {
	if part > 0 {
		return fmt.Sprintf("_runs/%s/%s-%04d-p%03d.json", adapter, run, seq, part)
	}
	return fmt.Sprintf("_runs/%s/%s-%04d.json", adapter, run, seq)
}

// Manifest is written to _runs/<adapter>/<run>-<seq>.json at every commit:
// what the commit added to the lake.
type Manifest struct {
	Run        string    `json:"run"`
	Seq        int       `json:"seq"`
	Part       int       `json:"part,omitempty"`
	Adapter    string    `json:"adapter"`
	Type       string    `json:"type"`
	At         time.Time `json:"at"`
	Files      []string  `json:"files"`
	Rows       int       `json:"rows"`
	BlobsNew   int       `json:"blobs_new"`
	BlobsDedup int       `json:"blobs_dedup"`
	Bytes      int64     `json:"bytes"`
}
