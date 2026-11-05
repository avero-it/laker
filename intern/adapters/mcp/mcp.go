// Package mcp serves the lake to MCP clients (Claude connectors among them)
// over Streamable HTTP, read-only: search, documents, file trees, file
// history and run status. It runs inside `laker serve`, on the stores the web
// interface holds open; the oauth package decides who gets through.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

const (
	// maxText caps a body or file text returned by a tool. Claude takes about
	// 150,000 characters per result, which carries the output twice (as text
	// and as structured content).
	maxText = 60_000
	// maxField caps every other string field in search hits.
	maxField = 500
	maxLimit = 50
	maxFiles = 500
)

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Deps struct {
	Lake     out.Lake
	Index    out.Indexer
	State    out.StateStore
	Location *time.Location
	// Status is what GET /api/status answers.
	Status  func() any
	Version string
}

// Handler is the MCP endpoint. It is stateless: every POST carries a whole
// request, so restarts and replicas need no session affinity.
func Handler(d Deps) http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "laker", Title: "Laker", Version: d.Version}, &mcp.ServerOptions{
		Instructions: "Laker is the owner's personal data lake: mail, files, chats and small databases, append-only. " +
			"Use search first (query-string syntax: words, field:value, +must, -not, \"phrases\"), then get_document " +
			"or read_file for the full text. Dates are in " + d.Location.String() + ".",
	})
	t := tools{d}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	mcp.AddTool(s, &mcp.Tool{Name: "search", Annotations: ro, Description: "Full-text search over mail, files and chats, " +
		"in Italian and English. Fields: subject, body, from, to, cc, folder, account, date (mail); path, name, ext, content, " +
		"source, mtime (files); chat, from, text, date (chats). Empty query: latest first."}, t.search)
	mcp.AddTool(s, &mcp.Tool{Name: "get_document", Annotations: ro, Description: "The whole document of a search hit by its id " +
		"(mail:<sha>, file:<source>:<path>, chat:...), or a mail by its sha256; long text is truncated."}, t.document)
	mcp.AddTool(s, &mcp.Tool{Name: "list_files", Annotations: ro, Description: "Directories and files of a file source, " +
		"now or at a past date."}, t.listFiles)
	mcp.AddTool(s, &mcp.Tool{Name: "file_history", Annotations: ro, Description: "Every recorded change of a path " +
		"of a file source, oldest first."}, t.fileHistory)
	mcp.AddTool(s, &mcp.Tool{Name: "read_file", Annotations: ro, Description: "The text extracted from a file of a source " +
		"(only formats with an extractor), truncated if long."}, t.readFile)
	mcp.AddTool(s, &mcp.Tool{Name: "status", Annotations: ro, Description: "The configured sources with their schedule " +
		"and last run."}, t.status)

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true,
		// behind the reverse proxy every request comes from localhost with
		// the public Host; the bearer token is the protection
		DisableLocalhostProtection: true,
	})
}

func ptr[T any](v T) *T { return &v }

type tools struct{ Deps }

type SearchIn struct {
	Query  string `json:"query,omitempty" jsonschema:"query-string search; empty lists the latest documents"`
	Kind   string `json:"kind,omitempty" jsonschema:"mail, file or chat; empty searches all"`
	Limit  int    `json:"limit,omitempty" jsonschema:"hits to return, 10 by default, at most 50"`
	Offset int    `json:"offset,omitempty" jsonschema:"hits to skip, for the next page"`
}

type Hit struct {
	ID        string              `json:"id"`
	Score     float64             `json:"score"`
	Fields    map[string]any      `json:"fields"`
	Fragments map[string][]string `json:"fragments,omitempty"`
}

type SearchOut struct {
	Total uint64 `json:"total"`
	Hits  []Hit  `json:"hits"`
}

func (t tools) search(ctx context.Context, _ *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, SearchOut, error) {
	switch in.Kind {
	case "", "mail", "file", "chat":
	default:
		return nil, SearchOut{}, fmt.Errorf("ERRA80: kind %q must be mail, file or chat", in.Kind)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, maxLimit)
	res, err := t.Index.Search(ctx, domain.Query{Text: in.Query, Kind: in.Kind, Limit: limit, Offset: max(in.Offset, 0)})
	if err != nil {
		return nil, SearchOut{}, fmt.Errorf("ERRA81: %w", err)
	}
	o := SearchOut{Total: res.Total, Hits: []Hit{}}
	for _, h := range res.Hits {
		o.Hits = append(o.Hits, Hit{ID: h.ID, Score: h.Score, Fields: t.fields(h.Fields, maxField, true), Fragments: h.Fragments})
	}
	return nil, o, nil
}

// bodies are the long text fields, left out of search hits.
var bodies = map[string]bool{"body": true, "content": true}

// fields copies the stored fields of a document, without the English
// analyzer copies, capping strings at max and dates in the configured zone.
func (t tools) fields(f map[string]any, max int, skipBodies bool) map[string]any {
	o := map[string]any{}
	for k, v := range f {
		if strings.HasSuffix(k, "_en") || (skipBodies && bodies[k]) {
			continue
		}
		o[k] = t.value(v, max)
	}
	return o
}

func (t tools) value(v any, max int) any {
	switch x := v.(type) {
	case string:
		if ts, err := time.Parse(time.RFC3339Nano, x); err == nil && strings.Contains(x, "T") {
			return ts.In(t.Location).Format(time.RFC3339)
		}
		return truncate(x, max)
	case []any:
		l := make([]any, len(x))
		for i, e := range x {
			l[i] = t.value(e, max)
		}
		return l
	}
	return v
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

type DocumentIn struct {
	ID string `json:"id" jsonschema:"the id of a search hit, or the sha256 of a mail"`
}

type DocumentOut struct {
	ID        string         `json:"id"`
	Fields    map[string]any `json:"fields"`
	Truncated bool           `json:"truncated"`
}

func (t tools) document(ctx context.Context, _ *mcp.CallToolRequest, in DocumentIn) (*mcp.CallToolResult, DocumentOut, error) {
	id := strings.TrimSpace(in.ID)
	if shaRE.MatchString(id) {
		id = domain.MailDocID(id)
	}
	doc, err := t.Index.Document(ctx, id)
	if err != nil {
		return nil, DocumentOut{}, fmt.Errorf("ERRA82: %w", err)
	}
	if doc == nil {
		return nil, DocumentOut{}, fmt.Errorf("ERRA83: %s is not in the index", in.ID)
	}
	o := DocumentOut{ID: id, Fields: t.fields(doc, maxText, false)}
	for k := range bodies {
		if s, ok := doc[k].(string); ok && len(s) > maxText {
			o.Truncated = true
		}
	}
	return nil, o, nil
}

type ListIn struct {
	Source string `json:"source" jsonschema:"the name of a file source, as status lists it"`
	Dir    string `json:"dir,omitempty" jsonschema:"absolute directory; empty: the deepest directory holding every file"`
	At     string `json:"at,omitempty" jsonschema:"YYYY-MM-DD (end of that day) or RFC 3339; empty: now"`
}

type DirOut struct {
	Name  string `json:"name"`
	Files int    `json:"files"`
	Size  int64  `json:"size"`
}

type FileOut struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mtime  string `json:"mtime"`
}

type ListOut struct {
	Dir       string    `json:"dir"`
	Dirs      []DirOut  `json:"dirs"`
	Files     []FileOut `json:"files"`
	Truncated bool      `json:"truncated"`
}

func (t tools) at(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return core.ParseAt(s, t.Location)
}

func (t tools) tree(ctx context.Context, source, at string) (domain.Tree, error) {
	when, err := t.at(at)
	if err != nil {
		return nil, err
	}
	tr, err := core.Tree(ctx, t.Lake, t.State, source, when)
	if err != nil {
		return nil, fmt.Errorf("ERRA84: %w", err)
	}
	if len(tr) == 0 {
		return nil, fmt.Errorf("ERRA85: no files for %s at that time", source)
	}
	return tr, nil
}

func (t tools) file(e domain.FileEntry) FileOut {
	return FileOut{Path: e.Path, SHA256: e.SHA256, Size: e.Size, Mtime: e.Mtime.In(t.Location).Format(time.RFC3339)}
}

func (t tools) listFiles(ctx context.Context, _ *mcp.CallToolRequest, in ListIn) (*mcp.CallToolResult, ListOut, error) {
	tr, err := t.tree(ctx, in.Source, in.At)
	if err != nil {
		return nil, ListOut{}, err
	}
	l, ok := core.List(tr, in.Dir)
	if !ok {
		return nil, ListOut{}, fmt.Errorf("ERRA86: %s is not a directory of %s", in.Dir, in.Source)
	}
	o := ListOut{Dir: l.Dir, Dirs: []DirOut{}, Files: []FileOut{}}
	for _, d := range l.Dirs {
		o.Dirs = append(o.Dirs, DirOut{Name: d.Name, Files: d.Files, Size: d.Size})
	}
	for i, e := range l.Files {
		if i == maxFiles {
			o.Truncated = true
			break
		}
		o.Files = append(o.Files, t.file(e))
	}
	return nil, o, nil
}

type HistoryIn struct {
	Source string `json:"source" jsonschema:"the name of a file source"`
	Path   string `json:"path" jsonschema:"absolute path of the file"`
}

type ChangeOut struct {
	Op       string `json:"op"`
	Path     string `json:"path"`
	FromPath string `json:"from_path,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Size     int64  `json:"size"`
	Mtime    string `json:"mtime"`
	SeenAt   string `json:"seen_at"`
}

type HistoryOut struct {
	Changes []ChangeOut `json:"changes"`
}

func (t tools) fileHistory(ctx context.Context, _ *mcp.CallToolRequest, in HistoryIn) (*mcp.CallToolResult, HistoryOut, error) {
	rows, err := core.FileHistory(ctx, t.Lake, in.Source, path.Clean(in.Path))
	if err != nil {
		return nil, HistoryOut{}, fmt.Errorf("ERRA87: %w", err)
	}
	o := HistoryOut{Changes: []ChangeOut{}}
	for _, r := range rows {
		o.Changes = append(o.Changes, ChangeOut{Op: r.Op, Path: r.Path, FromPath: r.FromPath, SHA256: r.SHA256, Size: r.Size,
			Mtime: r.Mtime.In(t.Location).Format(time.RFC3339), SeenAt: r.SeenAt.In(t.Location).Format(time.RFC3339)})
	}
	return nil, o, nil
}

type ReadIn struct {
	Source string `json:"source" jsonschema:"the name of a file source"`
	Path   string `json:"path" jsonschema:"absolute path of the file"`
	At     string `json:"at,omitempty" jsonschema:"YYYY-MM-DD or RFC 3339 for a past version; empty: now"`
}

type ReadOut struct {
	File      FileOut `json:"file"`
	Text      string  `json:"text"`
	Truncated bool    `json:"truncated"`
}

var errNoText = errors.New("ERRA88: no text: the format has no extractor, or this version is not the indexed one")

func (t tools) readFile(ctx context.Context, _ *mcp.CallToolRequest, in ReadIn) (*mcp.CallToolResult, ReadOut, error) {
	p := path.Clean(in.Path)
	tr, err := t.tree(ctx, in.Source, in.At)
	if err != nil {
		return nil, ReadOut{}, err
	}
	e, ok := tr[p]
	if !ok {
		return nil, ReadOut{}, fmt.Errorf("ERRA89: %s is not in %s at that time", p, in.Source)
	}
	// the index holds the text of the current version of each path
	doc, err := t.Index.Document(ctx, domain.FileDocID(in.Source, p))
	if err != nil {
		return nil, ReadOut{}, fmt.Errorf("ERRA8A: %w", err)
	}
	text, _ := doc["content"].(string)
	if doc == nil || doc["sha256"] != e.SHA256 || text == "" {
		return nil, ReadOut{}, errNoText
	}
	return nil, ReadOut{File: t.file(e), Text: truncate(text, maxText), Truncated: len(text) > maxText}, nil
}

type StatusIn struct{}

type StatusOut struct {
	Sources any `json:"sources"`
}

func (t tools) status(context.Context, *mcp.CallToolRequest, StatusIn) (*mcp.CallToolResult, StatusOut, error) {
	return nil, StatusOut{Sources: t.Status()}, nil
}
