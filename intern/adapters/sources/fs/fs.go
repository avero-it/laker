// Package fs is the source that copies the files of local directory trees
// into the lake: each content once as a blob, each change as an inventory row.
package fs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/extract"
	"github.com/avero-it/laker/pkg/glob"
	"github.com/avero-it/laker/pkg/parquetx"
)

const Type = "fs"

// inMemory is the largest file hashed and uploaded from memory; bigger files
// are first copied to a local temporary file, so the bytes uploaded are
// exactly the bytes hashed even if the file changes meanwhile.
const inMemory = 64 << 20

type Params struct {
	// Paths are absolute directories to scan; symlinks are not followed.
	Paths []string `yaml:"paths"`
	// Exclude are patterns on paths relative to each root: * within a
	// segment, ** across segments ("**/.git/**", "**/*.tmp").
	Exclude []string `yaml:"exclude"`
	// Extract the text of PDF, Office, OpenDocument, HTML and text files for
	// the search (default true), up to ExtractMaxSize bytes (default 50 MB).
	Extract        *bool `yaml:"extract"`
	ExtractMaxSize int64 `yaml:"extractmaxsize"`
}

type Source struct {
	name        string
	extractor   *extract.Extractor // nil: no extraction
	extractMax  int64
	roots       []string
	exclude     glob.Set
	commitEvery int
	tmpDir      string
	logger      zerolog.Logger
	now         func() time.Time
}

var _ out.Source = (*Source)(nil)

// New builds the source; x extracts the text of the files (shared by every
// source of the process, it may be nil to disable extraction).
func New(name string, params map[string]any, commitEvery int, tmpDir string, x *extract.Extractor, logger zerolog.Logger) (*Source, error) {
	var p Params
	if err := config.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if len(p.Paths) == 0 {
		return nil, fmt.Errorf("ERR210: paths is required")
	}
	s := &Source{name: name, exclude: glob.CompileAll(p.Exclude), commitEvery: commitEvery, tmpDir: tmpDir, logger: logger, now: time.Now}
	if p.Extract == nil || *p.Extract {
		s.extractor = x
	}
	s.extractMax = p.ExtractMaxSize
	if s.extractMax == 0 {
		s.extractMax = 50 << 20
	}
	if s.extractMax > inMemory {
		return nil, fmt.Errorf("ERR217: extractmaxsize cannot exceed %d bytes", inMemory)
	}
	for _, r := range p.Paths {
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("ERR211: path %q must be absolute", r)
		}
		s.roots = append(s.roots, filepath.Clean(r))
	}
	return s, nil
}

type scan struct {
	*Source
	rs      out.RunSink
	seen    map[string]bool
	added   map[string]domain.FileRow // sha256 → row of a file added in this run
	changed int
}

func (s *Source) Run(ctx context.Context, rs out.RunSink) error {
	sc := &scan{Source: s, rs: rs, seen: map[string]bool{}, added: map[string]domain.FileRow{}}

	var scanned []string
	for _, root := range s.roots {
		if err := sc.walk(ctx, root); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// an unreadable root keeps its files: nothing under it is deleted
			s.logger.Error().Str("root", root).Msgf("ERR212: cannot scan: %v", err)
			continue
		}
		scanned = append(scanned, root)
	}

	if err := sc.deletions(ctx, scanned); err != nil {
		return err
	}
	return rs.Commit(ctx)
}

func (sc *scan) walk(ctx context.Context, root string) error {
	return filepath.WalkDir(root, func(p string, d iofs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			if p == root {
				return err
			}
			sc.logger.Warn().Str("path", p).Msgf("WRN210: skipped: %v", err)
			sc.keepUnder(p)
			if d != nil && d.IsDir() {
				return iofs.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p != root && sc.exclude.Match(rel) {
				return iofs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || sc.exclude.Match(rel) {
			return nil
		}
		return sc.file(ctx, p, d)
	})
}

// keepUnder marks every known file under an unreadable path as seen, so the
// scan does not report them deleted.
func (sc *scan) keepUnder(p string) {
	sc.seen[p] = true
	_ = sc.rs.ForEachState(domain.InventoryKey(p+string(filepath.Separator)), func(k string, _ []byte) error {
		sc.seen[domain.InventoryPath(k)] = true
		return nil
	})
}

func (sc *scan) file(ctx context.Context, p string, d iofs.DirEntry) error {
	sc.seen[p] = true
	info, err := d.Info()
	if err != nil {
		sc.logger.Warn().Str("path", p).Msgf("WRN211: cannot stat: %v", err)
		return nil
	}

	key := domain.InventoryKey(p)
	var prev *domain.InventoryEntry
	if b, err := sc.rs.GetState(key); err != nil {
		return err
	} else if b != nil {
		prev = &domain.InventoryEntry{}
		if err := json.Unmarshal(b, prev); err != nil {
			return fmt.Errorf("ERR213: corrupt state of %s: %w", p, err)
		}
		if prev.Size == info.Size() && prev.MtimeNano == info.ModTime().UnixNano() {
			return nil
		}
	}

	sha, size, data, err := sc.store(ctx, p)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var pathErr *iofs.PathError
		if errors.As(err, &pathErr) {
			sc.logger.Warn().Str("path", p).Msgf("WRN212: cannot read: %v", err)
			return nil
		}
		return err
	}

	entry := domain.InventoryEntry{Size: size, MtimeNano: info.ModTime().UnixNano(), SHA256: sha}
	sc.rs.SetState(key, domain.MustJSON(entry))
	if prev != nil && prev.SHA256 == sha {
		return nil // touched, same content: nothing to record
	}

	row := domain.FileRow{
		Source: sc.name, Path: p, Op: domain.FileModify, SHA256: sha, Size: size,
		Mtime: info.ModTime().UTC(), Mode: int64(info.Mode().Perm()), SeenAt: sc.now().UTC(), RunID: sc.rs.RunID(),
	}
	if prev == nil {
		row.Op = domain.FileAdd
		sc.added[sha] = row
	}
	sc.record(row)
	doc := domain.NewFileDoc(row)
	doc.Content = sc.text(ctx, row, data)
	sc.rs.IndexFile(doc)

	sc.changed++
	if sc.changed%sc.commitEvery == 0 {
		return sc.rs.Commit(ctx)
	}
	return nil
}

func (sc *scan) record(row domain.FileRow) {
	parquetx.Append(sc.rs, domain.FilePrefix(sc.name, row.SeenAt), row)
}

// text extracts the text of a new content for the index, and stores it in
// the lake the first time this source sees the content.
func (sc *scan) text(ctx context.Context, row domain.FileRow, data []byte) string {
	if sc.extractor == nil || data == nil || row.Size > sc.extractMax || !extract.Supports(row.Path) {
		return ""
	}
	text, err := sc.extractor.Text(ctx, row.Path, data)
	if err != nil {
		sc.logger.Warn().Str("path", row.Path).Msgf("WRN213: no text extracted: %v", err)
	}

	key := domain.TextStateKey(row.SHA256)
	if done, _ := sc.rs.GetState(key); done == nil {
		tr := domain.TextRow{
			SHA256: row.SHA256, Source: sc.name, Name: filepath.Base(row.Path), Format: extract.Ext(row.Path),
			Chars: int64(len(text)), Text: text, ExtractedAt: row.SeenAt, RunID: row.RunID,
		}
		if err != nil {
			tr.Error = err.Error()
		}
		parquetx.Append(sc.rs, domain.TextPrefix(sc.name, row.SeenAt), tr)
		sc.rs.SetState(key, []byte{1})
	}
	return text
}

// store hashes the file and puts its content in the lake; files that fit in
// memory are returned too, for the text extraction.
func (sc *scan) store(ctx context.Context, p string) (string, int64, []byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, nil, err
	}

	if st.Size() <= inMemory {
		b, err := io.ReadAll(f)
		if err != nil {
			return "", 0, nil, err
		}
		sum := sha256.Sum256(b)
		sha := hex.EncodeToString(sum[:])
		_, err = sc.rs.PutBlob(ctx, sha, int64(len(b)), bytes.NewReader(b))
		return sha, int64(len(b)), b, err
	}

	if err := os.MkdirAll(sc.tmpDir, 0o750); err != nil {
		return "", 0, nil, fmt.Errorf("ERR214: %w", err)
	}
	tmp, err := os.CreateTemp(sc.tmpDir, "fs-")
	if err != nil {
		return "", 0, nil, fmt.Errorf("ERR215: %w", err)
	}
	defer os.Remove(tmp.Name()) // local scratch copy, not the lake
	defer tmp.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), f)
	if err != nil {
		return "", 0, nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", 0, nil, fmt.Errorf("ERR216: %w", err)
	}
	sha := hex.EncodeToString(h.Sum(nil))
	_, err = sc.rs.PutBlob(ctx, sha, n, tmp)
	return sha, n, nil, err
}

// deletions records the known files under the scanned roots that are gone; a
// gone file whose content reappeared elsewhere in this run is a move.
func (sc *scan) deletions(ctx context.Context, roots []string) error {
	type gone struct {
		path string
		sha  string
	}
	var deleted []gone
	err := sc.rs.ForEachState("inv:", func(k string, v []byte) error {
		p := domain.InventoryPath(k)
		if sc.seen[p] || !under(p, roots) {
			return nil
		}
		var e domain.InventoryEntry
		_ = json.Unmarshal(v, &e)
		deleted = append(deleted, gone{p, e.SHA256})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(deleted, func(i, j int) bool { return deleted[i].path < deleted[j].path })

	for _, g := range deleted {
		row := domain.FileRow{Source: sc.name, Path: g.path, Op: domain.FileDelete, SHA256: g.sha, SeenAt: sc.now().UTC(), RunID: sc.rs.RunID()}
		if add, ok := sc.added[g.sha]; ok {
			delete(sc.added, g.sha)
			row = add
			row.Op, row.FromPath = domain.FileMove, g.path
		}
		sc.record(row)
		sc.rs.DeleteState(domain.InventoryKey(g.path))
		sc.rs.UnindexFile(domain.FileDocID(sc.name, g.path))

		sc.changed++
		if sc.changed%sc.commitEvery == 0 {
			if err := sc.rs.Commit(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func under(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
