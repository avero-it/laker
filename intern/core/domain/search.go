package domain

import "time"

// Query is a search in the index. Text uses the query-string syntax:
// free words, field:value, +must, -not, "phrases".
type Query struct {
	Text   string
	Kind   string // "", "mail" or "file"
	Limit  int
	Offset int
	// Highlight styles the matched terms in the fragments: "ansi", "html" or
	// "" for plain fragments.
	Highlight string
}

type SearchResult struct {
	Total uint64
	Took  time.Duration
	Hits  []Hit
}

type Hit struct {
	ID        string              `json:"id"`
	Score     float64             `json:"score"`
	Fields    map[string]any      `json:"fields"`
	Fragments map[string][]string `json:"fragments,omitempty"`
}
