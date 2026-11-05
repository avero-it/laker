package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/intern/coretest"
)

const (
	mailSHA = "1111111111111111111111111111111111111111111111111111111111111111"
	fileSHA = "2222222222222222222222222222222222222222222222222222222222222222"
)

func session(t *testing.T, state out.StateStore, index out.Indexer, e *coretest.Env) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	date := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	docs := map[string]any{
		domain.MailDocID(mailSHA): domain.MailDoc{Kind: "mail", SHA256: mailSHA, Date: date, From: "anna@example.com",
			Subject: "Quarterly invoice", Body: "Here is the invoice. " + strings.Repeat("x", maxText+10)},
		domain.FileDocID("docs", "/home/me/notes/todo.txt"): domain.FileDoc{Kind: "file", SHA256: fileSHA, Source: "docs",
			Path: "/home/me/notes/todo.txt", Name: "todo.txt", Ext: "txt", Size: 5, Mtime: date, Content: "buy milk"},
	}
	if err := index.Apply(ctx, docs, nil); err != nil {
		t.Fatal(err)
	}
	if err := state.Apply([]out.StateChange{{Bucket: domain.SourceBucket("docs"), Key: domain.InventoryKey("/home/me/notes/todo.txt"),
		Value: domain.MustJSON(domain.InventoryEntry{Size: 5, MtimeNano: date.UnixNano(), SHA256: fileSHA})}}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(Handler(Deps{Lake: e.Lake, Index: index, State: state, Location: time.UTC, Version: "test",
		Status: func() any { return []map[string]string{{"name": "docs"}} }}))
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if out != nil && !res.IsError {
		raw, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
	return res
}

func TestTools(t *testing.T) {
	e := coretest.New(t)
	e.With(t, func(state out.StateStore, index out.Indexer) {
		cs := session(t, state, index, e)

		tl, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, tool := range tl.Tools {
			names[tool.Name] = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("%s is not marked read-only", tool.Name)
			}
		}
		for _, n := range []string{"search", "get_document", "list_files", "file_history", "read_file", "status"} {
			if !names[n] {
				t.Errorf("missing tool %s", n)
			}
		}

		var s SearchOut
		call(t, cs, "search", map[string]any{"query": "invoice", "kind": "mail"}, &s)
		if s.Total != 1 || len(s.Hits) != 1 || s.Hits[0].Fields["subject"] != "Quarterly invoice" {
			t.Fatalf("search: %+v", s)
		}
		if _, ok := s.Hits[0].Fields["body"]; ok {
			t.Fatal("search hits carry the body")
		}
		if res := call(t, cs, "search", map[string]any{"kind": "pictures"}, nil); !res.IsError {
			t.Fatal("an unknown kind was accepted")
		}

		var d DocumentOut
		call(t, cs, "get_document", map[string]any{"id": mailSHA}, &d)
		if !d.Truncated || len(d.Fields["body"].(string)) > maxText+len("…") {
			t.Fatalf("document not truncated: %v", d.Truncated)
		}

		var l ListOut
		call(t, cs, "list_files", map[string]any{"source": "docs"}, &l)
		if l.Dir != "/home/me/notes" || len(l.Files) != 1 || l.Files[0].SHA256 != fileSHA {
			t.Fatalf("list_files: %+v", l)
		}

		var r ReadOut
		call(t, cs, "read_file", map[string]any{"source": "docs", "path": "/home/me/notes/todo.txt"}, &r)
		if r.Text != "buy milk" {
			t.Fatalf("read_file: %+v", r)
		}
		if res := call(t, cs, "read_file", map[string]any{"source": "docs", "path": "/nope"}, nil); !res.IsError {
			t.Fatal("a missing file was read")
		}

		var st StatusOut
		call(t, cs, "status", nil, &st)
		if st.Sources == nil {
			t.Fatal("status is empty")
		}
	})
}
