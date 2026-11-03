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
	"github.com/blevesearch/bleve/v2/analysis/char/asciifolding"
	"github.com/blevesearch/bleve/v2/analysis/lang/en"
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
	created := false
	if errors.Is(err, bleve.ErrorIndexPathDoesNotExist) {
		idx, err = bleve.NewUsing(path, newMapping(), bleve.Config.DefaultIndexType, bleve.Config.DefaultKVStore, cfg)
		created = true
	}
	if err != nil {
		if isTimeout(err) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("ERR511: cannot open the index %s: %w", path, err)
	}
	if created {
		if err := idx.SetInternal([]byte(mappingKey), []byte(mappingVersion)); err != nil {
			idx.Close()
			return nil, fmt.Errorf("ERR51B: %w", err)
		}
	} else if v, _ := idx.GetInternal([]byte(mappingKey)); string(v) != mappingVersion {
		idx.Close()
		return nil, ErrOutdated
	}
	return &Index{idx: idx}, nil
}

// isTimeout tells a lock held by another process: scorch does not always wrap
// bbolt's ErrTimeout with %w, so the text is checked too.
func isTimeout(err error) bool {
	return errors.Is(err, bolterrors.ErrTimeout) || strings.HasSuffix(err.Error(), bolterrors.ErrTimeout.Error())
}

// textFields are the analyzed fields; each is indexed twice, in Italian
// under its name and in English under name+"_en" (only the first is stored).
var textFields = map[string]bool{
	"from": true, "to": true, "cc": true, "subject": true, "body": true, "attachments": true,
	"path": true, "name": true, "content": true,
	"chat": true, "text": true, "media": true,
}

func newMapping() mapping.IndexMapping {
	text := func(name, analyzer string, store bool) *mapping.FieldMapping {
		f := bleve.NewTextFieldMapping()
		f.Name = name
		f.Analyzer = analyzer
		f.Store = store
		f.IncludeTermVectors = store // highlighting needs them
		return f
	}
	bilingual := func(d *mapping.DocumentMapping, field string) {
		d.AddFieldMappingsAt(field, text(field, italianAnalyzer, true), text(field+englishSuffix, englishAnalyzer, false))
	}
	kw := func() *mapping.FieldMapping {
		f := bleve.NewKeywordFieldMapping()
		f.Analyzer = keyword.Name
		return f
	}
	date := bleve.NewDateTimeFieldMapping()
	num := bleve.NewNumericFieldMapping()

	m := bleve.NewIndexMapping()
	// The content is in Italian and English, often mixed in one message, so
	// every text is analyzed both ways and every query asks both. Both split
	// on every non-alphanumeric character (the standard tokenizer keeps
	// "contract.txt" and "anna.smith@example.com" whole) and fold accents
	// ("perche" finds "perché"), then apply the filters of their language.
	must(m.AddCustomTokenizer(wordsTokenizer, map[string]interface{}{
		"type": regexp.Name, "regexp": `[\p{L}\p{N}]+`,
	}))
	must(m.AddCustomAnalyzer(italianAnalyzer, map[string]interface{}{
		"type": custom.Name, "char_filters": []string{asciifolding.Name}, "tokenizer": wordsTokenizer,
		"token_filters": []string{lowercase.Name, it.ElisionName, it.StopName, it.LightStemmerName},
	}))
	must(m.AddCustomAnalyzer(englishAnalyzer, map[string]interface{}{
		"type": custom.Name, "char_filters": []string{asciifolding.Name}, "tokenizer": wordsTokenizer,
		"token_filters": []string{lowercase.Name, en.StopName, en.SnowballStemmerName},
	}))

	mail := bleve.NewDocumentMapping()
	mail.AddFieldMappingsAt("kind", kw())
	mail.AddFieldMappingsAt("sha256", kw())
	mail.AddFieldMappingsAt("account", kw())
	mail.AddFieldMappingsAt("folder", kw())
	mail.AddFieldMappingsAt("message_id", kw())
	mail.AddFieldMappingsAt("date", date)
	for _, f := range []string{"from", "to", "cc", "subject", "body", "attachments"} {
		bilingual(mail, f)
	}
	mail.AddFieldMappingsAt("size", num)

	file := bleve.NewDocumentMapping()
	file.AddFieldMappingsAt("kind", kw())
	file.AddFieldMappingsAt("sha256", kw())
	file.AddFieldMappingsAt("source", kw())
	file.AddFieldMappingsAt("ext", kw())
	for _, f := range []string{"path", "name", "content"} {
		bilingual(file, f)
	}
	file.AddFieldMappingsAt("size", num)
	file.AddFieldMappingsAt("mtime", date)

	chat := bleve.NewDocumentMapping()
	chat.AddFieldMappingsAt("kind", kw())
	chat.AddFieldMappingsAt("source", kw())
	chat.AddFieldMappingsAt("chat_id", kw())
	chat.AddFieldMappingsAt("blobs", kw())
	chat.AddFieldMappingsAt("msg_key", kw())
	chat.AddFieldMappingsAt("deleted", bleve.NewBooleanFieldMapping())
	chat.AddFieldMappingsAt("msg_id", num)
	chat.AddFieldMappingsAt("date", date)
	for _, f := range []string{"chat", "from", "text", "media"} {
		bilingual(chat, f)
	}
	m.AddDocumentMapping("chat", chat)

	m.AddDocumentMapping("mail", mail)
	m.AddDocumentMapping("file", file)
	m.DefaultAnalyzer = italianAnalyzer
	return m
}

// bilingual rewrites a parsed query so every text match is asked in both
// languages: an unfielded match against everything (_all) with each
// analyzer, a fielded one against the field and its English twin.
func bilingual(q query.Query) query.Query {
	either := func(a, b query.Query) query.Query { return bleve.NewDisjunctionQuery(a, b) }
	switch x := q.(type) {
	case *query.BooleanQuery:
		x.Must, x.Should, x.MustNot = bilingualOrNil(x.Must), bilingualOrNil(x.Should), bilingualOrNil(x.MustNot)
		return x
	case *query.ConjunctionQuery:
		for i := range x.Conjuncts {
			x.Conjuncts[i] = bilingual(x.Conjuncts[i])
		}
		return x
	case *query.DisjunctionQuery:
		for i := range x.Disjuncts {
			x.Disjuncts[i] = bilingual(x.Disjuncts[i])
		}
		return x
	case *query.MatchQuery:
		switch {
		case x.FieldVal == "":
			it, en := *x, *x
			it.Analyzer, en.Analyzer = italianAnalyzer, englishAnalyzer
			return either(&it, &en)
		case textFields[x.FieldVal]:
			en := *x
			en.FieldVal = x.FieldVal + englishSuffix
			return either(x, &en)
		}
	case *query.MatchPhraseQuery:
		switch {
		case x.FieldVal == "":
			it, en := *x, *x
			it.Analyzer, en.Analyzer = italianAnalyzer, englishAnalyzer
			return either(&it, &en)
		case textFields[x.FieldVal]:
			en := *x
			en.FieldVal = x.FieldVal + englishSuffix
			return either(x, &en)
		}
	}
	return q
}

func bilingualOrNil(q query.Query) query.Query {
	if q == nil {
		return nil
	}
	return bilingual(q)
}

const (
	wordsTokenizer  = "laker_words"
	italianAnalyzer = "laker_it"
	englishAnalyzer = "laker_en"
	englishSuffix   = "_en"

	// mappingVersion changes whenever newMapping does: an index built with
	// another mapping must be rebuilt (laker reindex).
	mappingVersion = "4"
	mappingKey     = "laker_mapping"
)

// ErrOutdated means the index was built by an older mapping.
var ErrOutdated = errors.New("ERR519: the search index was built by an older version of laker: run laker reindex")

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
		parsed, err := bleve.NewQueryStringQuery(q.Text).Parse()
		if err != nil {
			return domain.SearchResult{}, fmt.Errorf("ERR51A: invalid query: %w", err)
		}
		bq = bilingual(parsed)
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
	req.Highlight.Fields = []string{"subject", "body", "path", "name", "content", "text"}
	switch {
	case len(q.Sort) > 0:
		req.SortBy(q.Sort)
	case q.Text == "":
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
