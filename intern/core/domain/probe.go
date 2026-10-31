package domain

// Probe is what a live check of a source found: it could connect, and what
// it would read.
type Probe struct {
	Summary string   `json:"summary"`
	Details []string `json:"details,omitempty"`
}
