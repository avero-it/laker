package core

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

// CurrentTree is the tree of a source after its last run, from the local state.
func CurrentTree(state out.StateStore, source string) (domain.Tree, error) {
	t := domain.Tree{}
	err := state.ForEach(domain.SourceBucket(source), "inv:", func(k string, v []byte) error {
		var e domain.InventoryEntry
		if err := json.Unmarshal(v, &e); err != nil {
			return fmt.Errorf("ERR140: corrupt inventory entry %s: %w", k, err)
		}
		p := domain.InventoryPath(k)
		t[p] = domain.FileEntry{Path: p, SHA256: e.SHA256, Size: e.Size, Mtime: time.Unix(0, e.MtimeNano)}
		return nil
	})
	return t, err
}

// Tree is the tree of a source now or at a past moment. Now comes from the
// local state when this process runs the source, from the lake otherwise
// (the source of an agent keeps its inventory on the agent).
func Tree(ctx context.Context, lake out.Lake, state out.StateStore, source string, at time.Time) (domain.Tree, error) {
	if at.IsZero() && state != nil {
		t, err := CurrentTree(state, source)
		if err != nil || len(t) > 0 {
			return t, err
		}
	}
	trees, err := TreeAt(ctx, lake, source, at)
	if err != nil {
		return nil, err
	}
	if trees[source] == nil {
		return domain.Tree{}, nil
	}
	return trees[source], nil
}

type DirEntry struct {
	Name  string
	Files int
	Size  int64
}

// Listing is the content of one directory of a tree.
type Listing struct {
	Dir   string
	Dirs  []DirEntry
	Files []domain.FileEntry
}

// List lists dir in t; an empty dir means the deepest directory holding
// every file. ok is false if dir holds nothing.
func List(t domain.Tree, dir string) (Listing, bool) {
	if dir == "" {
		dir = CommonDir(t)
	}
	dir = strings.TrimSuffix(path.Clean(dir), "/")
	l := Listing{Dir: dir}
	dirs := map[string]*DirEntry{}
	for p, e := range t {
		rest, ok := strings.CutPrefix(p, dir+"/")
		if !ok {
			continue
		}
		if name, _, nested := strings.Cut(rest, "/"); nested {
			d := dirs[name]
			if d == nil {
				d = &DirEntry{Name: name}
				dirs[name] = d
			}
			d.Files++
			d.Size += e.Size
		} else {
			l.Files = append(l.Files, e)
		}
	}
	for _, d := range dirs {
		l.Dirs = append(l.Dirs, *d)
	}
	sort.Slice(l.Dirs, func(i, j int) bool { return l.Dirs[i].Name < l.Dirs[j].Name })
	sort.Slice(l.Files, func(i, j int) bool { return l.Files[i].Path < l.Files[j].Path })
	return l, len(l.Dirs)+len(l.Files) > 0
}

// CommonDir is the deepest directory holding every path of t.
func CommonDir(t domain.Tree) string {
	var prefix string
	first := true
	for p := range t {
		d := path.Dir(p)
		if first {
			prefix, first = d, false
			continue
		}
		for prefix != "/" && prefix != "." && !strings.HasPrefix(d+"/", prefix+"/") {
			prefix = path.Dir(prefix)
		}
	}
	return prefix
}

// FileHistory is every recorded change of one path, oldest first.
func FileHistory(ctx context.Context, lake out.Lake, source, p string) ([]domain.FileRow, error) {
	var rows []domain.FileRow
	err := EachFile(ctx, lake, source, zeroTime, func(r domain.FileRow) error {
		if r.Path == p || r.FromPath == p {
			rows = append(rows, r)
		}
		return nil
	})
	return rows, err
}

// ParseAt reads a date (the end of that day in loc) or an RFC 3339 instant.
func ParseAt(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("ERR141: %q is neither YYYY-MM-DD nor RFC 3339", s)
	}
	return d.AddDate(0, 0, 1).Add(-time.Nanosecond), nil
}
