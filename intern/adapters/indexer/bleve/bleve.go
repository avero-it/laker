// Package bleve is the full-text Indexer on a local Bleve index, with Italian
// analysis (stemming, elision, stop words) on every text field.
package bleve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/keyword"
	"github.com/blevesearch/bleve/v2/analysis/lang/it"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/regexp"
	"github.com/blevesearch/bleve/v2/mapping"
	_ "github.com/blevesearch/bleve/v2/search/highlight/highlighter/ansi" // Query.Highlight "ansi"
	"github.com/blevesearch/bleve/v2/search/query"
	bolterrors "go.etcd.io/bbolt/errors"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

var _ out.Indexer = (*Index)(nil)

type Index struct{ idx bleve.Index }

// ErrBusy means another process holds the index.
var ErrBusy = errors.New("ERR510: the search index is in use by another laker process")

// Open opens the index at path, creating it if missing, waiting up to timeout
// for another process to release it.
func Open(path string, timeout time.Duration) (*Index, error) {
	cfg := map[string]interface{}{"bolt_timeout": timeout.String()}

	idx, err := bleve.OpenUsing(path, cfg)
	if errors.Is(err, bleve.ErrorIndexPathDoesNotExist) {
		idx, err = bleve.NewUsing(path, newMapping(), bleve.Config.DefaultIndexType, bleve.Config.DefaultKVStore, cfg)
	}
	if err != nil {
		if isTimeout(err) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("ERR511: cannot open the index %s: %w", path, err)
	}
	return &Index{idx: idx}, nil
}

// isTimeout tells a lock held by another process: scorch does not always wrap
// bbolt's ErrTimeout with %w, so the text is checked too.
func isTimeout(err error) bool {
	return errors.Is(err, bolterrors.ErrTimeout) || strings.HasSuffix(err.Error(), bolterrors.ErrTimeout.Error())
}

func newMapping() mapping.IndexMapping {
	text := func(analyzer string, store bool) *mapping.FieldMapping {
		f := bleve.NewTextFieldMapping()
		f.Analyzer = analyzer
		f.Store = store
		f.IncludeTermVectors = store // highlighting needs them
		return f
	}
	kw := func() *mapping.FieldMapping {
		f := bleve.NewKeywordFieldMapping()
		f.Analyzer = keyword.Name
		return f
	}
	date := bleve.NewDateTimeFieldMapping()
	num := bleve.NewNumericFieldMapping()

	m := bleve.NewIndexMapping()
	// One analyzer for every text field and for free-text queries, so a word
	// matches the same way wherever it is: split on every non-alphanumeric
	// character (the standard tokenizer keeps "contratto.txt" and
	// "anna.smith@example.com" whole), then the Italian filters.
	must(m.AddCustomTokenizer(wordsTokenizer, map[string]interface{}{
		"type": regexp.Name, "regexp": `[\p{L}\p{N}]+`,
	}))
	must(m.AddCustomAnalyzer(textAnalyzer, map[string]interface{}{
		"type": custom.Name, "tokenizer": wordsTokenizer,
		"token_filters": []string{lowercase.Name, it.ElisionName, it.StopName, it.LightStemmerName},
	}))

	mail := bleve.NewDocumentMapping()
	mail.AddFieldMappingsAt("kind", kw())
	mail.AddFieldMappingsAt("sha256", kw())
	mail.AddFieldMappingsAt("account", kw())
	mail.AddFieldMappingsAt("folder", kw())
	mail.AddFieldMappingsAt("message_id", kw())
	mail.AddFieldMappingsAt("date", date)
	mail.AddFieldMappingsAt("from", text(textAnalyzer, true))
	mail.AddFieldMappingsAt("to", text(textAnalyzer, true))
	mail.AddFieldMappingsAt("cc", text(textAnalyzer, true))
	mail.AddFieldMappingsAt("subject", text(textAnalyzer, true))
	mail.AddFieldMappingsAt("body", text(textAnalyzer, true))
	mail.AddFieldMappingsAt("attachments", text(textAnalyzer, true))
	mail.AddFieldMappingsAt("size", num)

	file := bleve.NewDocumentMapping()
	file.AddFieldMappingsAt("kind", kw())
	file.AddFieldMappingsAt("sha256", kw())
	file.AddFieldMappingsAt("source", kw())
	file.AddFieldMappingsAt("ext", kw())
	file.AddFieldMappingsAt("path", text(textAnalyzer, true))
	file.AddFieldMappingsAt("name", text(textAnalyzer, true))
	file.AddFieldMappingsAt("content", text(textAnalyzer, true))
	file.AddFieldMappingsAt("size", num)
	file.AddFieldMappingsAt("mtime", date)

	m.AddDocumentMapping("mail", mail)
	m.AddDocumentMapping("file", file)
	m.DefaultAnalyzer = textAnalyzer
	return m
}

const (
	wordsTokenizer = "laker_words"
	textAnalyzer   = "laker_text"
)

func must(err error) {
	if err != nil {
		panic(err) // static mapping: a failure is a programming error
	}
}

func (i *Index) Apply(_ context.Context, docs map[string]any, deletes []string) error {
	if len(docs) == 0 && len(deletes) == 0 {
		return nil
	}
	b := i.idx.NewBatch()
	for _, id := range deletes {
		b.Delete(id)
	}
	for id, doc := range docs {
		if err := b.Index(id, doc); err != nil {
			return fmt.Errorf("ERR512: cannot index %s: %w", id, err)
		}
	}
	if err := i.idx.Batch(b); err != nil {
		return fmt.Errorf("ERR513: cannot apply the index batch: %w", err)
	}
	return nil
}

func (i *Index) Search(ctx context.Context, q domain.Query) (domain.SearchResult, error) {
	var bq query.Query = bleve.NewMatchAllQuery()
	if q.Text != "" {
		bq = bleve.NewQueryStringQuery(q.Text)
	}
	if q.Kind != "" {
		kind := bleve.NewTermQuery(q.Kind)
		kind.SetField("kind")
		bq = bleve.NewConjunctionQuery(bq, kind)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	req := bleve.NewSearchRequestOptions(bq, limit, q.Offset, false)
	req.Fields = []string{"*"}
	if q.Highlight != "" {
		req.Highlight = bleve.NewHighlightWithStyle(q.Highlight)
	} else {
		req.Highlight = bleve.NewHighlight()
	}
	req.Highlight.Fields = []string{"subject", "body", "path", "name", "content"}
	if q.Text == "" {
		req.SortBy([]string{"-date", "-mtime"})
	}

	res, err := i.idx.SearchInContext(ctx, req)
	if err != nil {
		return domain.SearchResult{}, fmt.Errorf("ERR514: search failed: %w", err)
	}

	out := domain.SearchResult{Total: res.Total, Took: res.Took}
	for _, h := range res.Hits {
		out.Hits = append(out.Hits, domain.Hit{ID: h.ID, Score: h.Score, Fields: h.Fields, Fragments: h.Fragments})
	}
	return out, nil
}

func (i *Index) Document(ctx context.Context, id string) (map[string]any, error) {
	req := bleve.NewSearchRequest(bleve.NewDocIDQuery([]string{id}))
	req.Fields = []string{"*"}
	res, err := i.idx.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("ERR515: cannot read %s: %w", id, err)
	}
	if len(res.Hits) == 0 {
		return nil, nil
	}
	return res.Hits[0].Fields, nil
}

func (i *Index) Snapshot(dir string) error {
	c, ok := i.idx.(bleve.IndexCopyable)
	if !ok {
		return fmt.Errorf("ERR516: this index type cannot be copied online")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("ERR517: %w", err)
	}
	if err := c.CopyTo(bleve.FileSystemDirectory(dir)); err != nil {
		return fmt.Errorf("ERR518: cannot copy the index: %w", err)
	}
	return nil
}

func (i *Index) Close() error { return i.idx.Close() }
