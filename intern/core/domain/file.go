package domain

import (
	"path"
	"strings"
	"time"
)

// File operations recorded in files/inventory.
const (
	FileAdd    = "add"
	FileModify = "modify"
	FileDelete = "delete"
	// FileMove: Path now holds the content FromPath had; FromPath is gone.
	FileMove = "move"
)

// FileRow is one change seen in a scanned tree, as stored in files/inventory.
// Replaying the rows of a source in order gives the tree at any moment.
type FileRow struct {
	Source   string    `parquet:"source,id(1)"` // adapter name
	Path     string    `parquet:"path,id(2)"`   // absolute path on the scanned host
	Op       string    `parquet:"op,id(3)"`
	FromPath string    `parquet:"from_path,id(4)"`
	SHA256   string    `parquet:"sha256,id(5)"`
	Size     int64     `parquet:"size,id(6)"`
	Mtime    time.Time `parquet:"mtime,id(7)"`
	Mode     int64     `parquet:"mode,id(8)"`
	SeenAt   time.Time `parquet:"seen_at,id(9)"`
	RunID    string    `parquet:"run_id,id(10)"`
}

// FileDoc is a file present in a tree, in the search index.
type FileDoc struct {
	Kind   string    `json:"kind"`
	SHA256 string    `json:"sha256"`
	Source string    `json:"source"`
	Path   string    `json:"path"`
	Name   string    `json:"name"`
	Ext    string    `json:"ext"`
	Size   int64     `json:"size"`
	Mtime  time.Time `json:"mtime"`
	// Content is the extracted text, empty for formats without an extractor.
	Content string `json:"content,omitempty"`
}

func (FileDoc) Type() string { return "file" }

// TextRow is the text extracted from a content, stored in files/text so the
// index can be rebuilt without extracting again. One row per content and
// source, whatever the number of paths holding it.
type TextRow struct {
	SHA256      string    `parquet:"sha256,id(1)"`
	Source      string    `parquet:"source,id(2)"`
	Name        string    `parquet:"name,id(3)"` // a file name it was seen with
	Format      string    `parquet:"format,id(4)"`
	Chars       int64     `parquet:"chars,id(5)"`
	Text        string    `parquet:"text,id(6)"`
	Error       string    `parquet:"error,id(7)"`
	ExtractedAt time.Time `parquet:"extracted_at,id(8)"`
	RunID       string    `parquet:"run_id,id(9)"`
}

// TextPrefix is where the extracted texts of a source live in the lake.
func TextPrefix(source string, day time.Time) string {
	return "files/text/source=" + source + "/dt=" + day.UTC().Format("2006-01-02")
}

// TextStateKey marks a content whose text is already in the lake.
func TextStateKey(sha string) string { return "text:" + sha }

// FileDocID is the index id of a path of a source.
func FileDocID(source, p string) string { return "file:" + source + ":" + p }

func NewFileDoc(r FileRow) FileDoc {
	return FileDoc{
		Kind: "file", SHA256: r.SHA256, Source: r.Source, Path: r.Path,
		Name: path.Base(r.Path), Ext: strings.TrimPrefix(strings.ToLower(path.Ext(r.Path)), "."),
		Size: r.Size, Mtime: r.Mtime,
	}
}

// FilePrefix is where the inventory rows of a source live in the lake.
func FilePrefix(source string, day time.Time) string {
	return "files/inventory/source=" + source + "/dt=" + day.UTC().Format("2006-01-02")
}

// FileEntry is a file in a tree at some moment.
type FileEntry struct {
	Path   string
	SHA256 string
	Size   int64
	Mtime  time.Time
}

// Tree replays inventory rows into the files present after them.
type Tree map[string]FileEntry

func (t Tree) Apply(r FileRow) {
	switch r.Op {
	case FileDelete:
		delete(t, r.Path)
	case FileMove:
		delete(t, r.FromPath)
		fallthrough
	default:
		t[r.Path] = FileEntry{Path: r.Path, SHA256: r.SHA256, Size: r.Size, Mtime: r.Mtime}
	}
}
