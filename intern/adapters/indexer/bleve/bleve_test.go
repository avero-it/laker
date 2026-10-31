package bleve

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/avero-it/laker/intern/core/domain"
)

// The index analyzes text in Italian (the language of the lake's content):
// the words below are Italian on purpose.
func TestItalianStemming(t *testing.T) {
	idx, err := Open(filepath.Join(t.TempDir(), "index"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ctx := context.Background()
	doc := domain.FileDoc{Kind: "file", Source: "s", Path: "/d/fatture_2026.txt", Content: "Fatture di ottobre"}
	if err := idx.Apply(ctx, map[string]any{domain.FileDocID("s", doc.Path): doc}, nil); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"fattura", "fatture", "ottobre", "2026"} {
		res, err := idx.Search(ctx, domain.Query{Text: q})
		if err != nil || res.Total != 1 {
			t.Errorf("%s: %d hits, %v", q, res.Total, err)
		}
	}
}
