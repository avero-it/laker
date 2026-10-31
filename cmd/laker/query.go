package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/adapters/indexer/bleve"
	"github.com/avero-it/laker/intern/adapters/state/bbolt"
	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/config"
)

var (
	shaRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")
)

// searcher is the local index or, while serve runs, its API.
type searcher interface {
	Search(ctx context.Context, q domain.Query) (domain.SearchResult, error)
	Document(ctx context.Context, id string) (map[string]any, error)
}

// openSearcher asks a running serve first, else opens the local index.
func openSearcher(cfg *config.Config) (searcher, func()) {
	if rm := newRemote(cfg); rm != nil {
		if _, err := rm.Status(context.Background()); err == nil {
			return rm, func() {}
		} else if !errors.Is(err, errNotServing) {
			fatalf("ERR064: %v", err)
		}
	}
	idx, err := bleve.Open(indexPath(cfg), cliWait)
	if err != nil {
		fatalf("ERR050: %v", err)
	}
	return idx, func() { idx.Close() }
}

func commandSearch(configPath string, words []string, kind string, limit int, asJSON bool) {
	cfg := mustConfig(configPath)
	idx, closeIdx := openSearcher(cfg)
	defer closeIdx()

	q := domain.Query{Text: strings.Join(words, " "), Kind: kind, Limit: limit}
	if !asJSON {
		q.Highlight = "ansi" // stripped below when stdout is not a terminal
	}
	res, err := idx.Search(context.Background(), q)
	if err != nil {
		fatalf("ERR051: %v", err)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return
	}

	loc, _ := time.LoadLocation(cfg.Common.Timezone)
	bold := color.New(color.Bold).SprintFunc()
	dim := color.New(color.FgHiBlack).SprintFunc()
	for _, h := range res.Hits {
		f := h.Fields
		sha := str(f["sha256"])
		switch str(f["kind"]) {
		case "mail":
			fmt.Printf("%s  %s  %s\n", dim(when(f["date"], loc)), str(f["from"]), bold(str(f["subject"])))
			fmt.Printf("    %s  %s\n", dim(str(f["folder"])), dim(sha))
		case "file":
			fmt.Printf("%s  %s  %s\n", dim(when(f["mtime"], loc)), humanSize(num(f["size"])), bold(str(f["path"])))
			fmt.Printf("    %s  %s\n", dim(str(f["source"])), dim(sha))
		}
		for _, field := range []string{"subject", "name", "body", "content"} {
			for _, frag := range h.Fragments[field] {
				if color.NoColor {
					frag = ansiRE.ReplaceAllString(frag, "")
				}
				fmt.Printf("    … %s\n", strings.Join(strings.Fields(frag), " "))
			}
		}
	}
	fmt.Printf("%s\n", dim(fmt.Sprintf("%d result(s) of %d in %s", len(res.Hits), res.Total, res.Took.Round(time.Millisecond))))
}

func commandShow(configPath, sha string) {
	cfg := mustConfig(configPath)
	if !shaRE.MatchString(sha) {
		fatalf("ERR052: %q is not a sha256", sha)
	}
	idx, closeIdx := openSearcher(cfg)
	defer closeIdx()
	ctx := context.Background()
	loc, _ := time.LoadLocation(cfg.Common.Timezone)

	doc, err := idx.Document(ctx, domain.MailDocID(sha))
	if err != nil {
		fatalf("ERR053: %v", err)
	}
	if doc != nil {
		for _, k := range []string{"date", "from", "to", "cc", "subject", "folder", "attachments", "message_id"} {
			v := str(doc[k])
			if k == "date" {
				v = when(doc[k], loc)
			}
			if v != "" {
				fmt.Printf("%-12s %s\n", k+":", v)
			}
		}
		fmt.Printf("\n%s\n", str(doc["body"]))
		return
	}

	res, err := idx.Search(ctx, domain.Query{Text: "sha256:" + sha, Kind: "file", Limit: 100})
	if err != nil {
		fatalf("ERR054: %v", err)
	}
	if len(res.Hits) == 0 {
		fatalf("ERR055: %s is not in the index (laker get still finds it if it is in the lake)", sha)
	}
	for _, h := range res.Hits {
		fmt.Printf("%s:%s  %s  %s\n", str(h.Fields["source"]), str(h.Fields["path"]), humanSize(num(h.Fields["size"])), when(h.Fields["mtime"], loc))
	}
}

func commandGet(configPath, what, at, outPath string) {
	cfg := mustConfig(configPath)
	ctx := context.Background()
	lake, err := buildLake(cfg, zerolog.Nop())
	if err != nil {
		fatalf("ERR056: %v", err)
	}

	sha := what
	if !shaRE.MatchString(what) {
		source, p, ok := strings.Cut(what, ":")
		if !ok || p == "" {
			fatalf("ERR057: give a sha256 or source:path")
		}
		e, found := lookup(ctx, cfg, lake, source, path.Clean(p), at)
		if !found {
			fatalf("ERR058: %s is not in %s at that time", p, source)
		}
		sha = e.SHA256
	}

	key, _ := domain.BlobKey(sha)
	rc, err := lake.Get(ctx, key)
	if err != nil {
		fatalf("ERR059: %v", err)
	}
	defer rc.Close()

	var w io.Writer = os.Stdout
	if outPath != "" {
		f, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			fatalf("ERR05A: %v", err)
		}
		defer f.Close()
		w = f
	}
	if _, err := io.Copy(w, rc); err != nil {
		fatalf("ERR05B: %v", err)
	}
}

// tree is the files of source now or at a past date, from a running serve,
// else from the local state and the lake.
func tree(ctx context.Context, cfg *config.Config, lake out.Lake, source, at string) domain.Tree {
	var t time.Time
	if at != "" {
		loc, _ := time.LoadLocation(cfg.Common.Timezone)
		var err error
		if t, err = core.ParseAt(at, loc); err != nil {
			fatalf("ERR05C: %v", err)
		}
	}
	if rm := newRemote(cfg); rm != nil {
		tr, err := rm.Tree(ctx, source, t)
		if err == nil {
			return tr
		}
		if !errors.Is(err, errNotServing) {
			fatalf("ERR05D: %v", err)
		}
	}
	if !t.IsZero() {
		tr, err := core.Tree(ctx, lake, nil, source, t)
		if err != nil {
			fatalf("ERR05E: %v", err)
		}
		return tr
	}
	state, err := bbolt.Open(statePath(cfg), cliWait)
	if err != nil {
		fatalf("ERR05F: %v", err)
	}
	defer state.Close()
	tr, err := core.Tree(ctx, lake, state, source, time.Time{})
	if err != nil {
		fatalf("ERR065: %v", err)
	}
	return tr
}

func lookup(ctx context.Context, cfg *config.Config, lake out.Lake, source, p, at string) (domain.FileEntry, bool) {
	e, ok := tree(ctx, cfg, lake, source, at)[p]
	return e, ok
}

func commandLs(configPath, what, at string) {
	cfg := mustConfig(configPath)
	ctx := context.Background()
	lake, err := buildLake(cfg, zerolog.Nop())
	if err != nil {
		fatalf("ERR060: %v", err)
	}
	source, dir, _ := strings.Cut(what, ":")
	t := tree(ctx, cfg, lake, source, at)
	if len(t) == 0 {
		fatalf("ERR061: no files for %s at that time", source)
	}

	l, ok := core.List(t, dir)
	if !ok {
		fatalf("ERR062: %s is not a directory of %s", dir, source)
	}

	loc, _ := time.LoadLocation(cfg.Common.Timezone)
	dim := color.New(color.FgHiBlack).SprintFunc()
	blue := color.New(color.FgBlue, color.Bold).SprintFunc()
	fmt.Println(dim(source + ":" + l.Dir))
	for _, d := range l.Dirs {
		fmt.Printf("%9s  %-16s  %-12s  %s %s\n", humanSize(d.Size), "", "", blue(d.Name+"/"), dim(fmt.Sprintf("(%d files)", d.Files)))
	}
	for _, e := range l.Files {
		fmt.Printf("%9s  %-16s  %-12s  %s\n", humanSize(e.Size), e.Mtime.In(loc).Format("2006-01-02 15:04"), dim(e.SHA256[:12]), path.Base(e.Path))
	}
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, str(e))
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(x)
	}
}

func num(v any) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}

func when(v any, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, str(v))
	if err != nil {
		return str(v)
	}
	return t.In(loc).Format("2006-01-02 15:04")
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
