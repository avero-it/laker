package fs

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

var _ out.Prober = (*Source)(nil)

// probeLimit bounds the walk of a probe: it estimates, it does not scan.
const (
	probeMaxEntries = 10000
	probeMaxTime    = 5 * time.Second
)

var errEnough = errors.New("ERR218: enough")

// Probe checks that every root is a readable directory and counts what it
// holds, up to a bound.
func (s *Source) Probe(ctx context.Context) (domain.Probe, error) {
	var p domain.Probe
	var bad []error
	total, totalBytes := 0, int64(0)
	for _, root := range s.roots {
		st, err := os.Stat(root)
		switch {
		case err != nil:
			bad = append(bad, fmt.Errorf("ERR219: %s: %w", root, err))
			continue
		case !st.IsDir():
			bad = append(bad, fmt.Errorf("ERR21A: %s is not a directory", root))
			continue
		}
		files, bytes, unreadable, partial := 0, int64(0), 0, false
		deadline := time.Now().Add(probeMaxTime)
		err = filepath.WalkDir(root, func(path string, d iofs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				unreadable++
				if d != nil && d.IsDir() {
					return iofs.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if path != root && s.exclude.Match(rel) {
					return iofs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || s.exclude.Match(rel) {
				return nil
			}
			files++
			if info, err := d.Info(); err == nil {
				bytes += info.Size()
			}
			if files >= probeMaxEntries || time.Now().After(deadline) {
				partial = true
				return errEnough
			}
			return nil
		})
		if err != nil && !errors.Is(err, errEnough) {
			bad = append(bad, fmt.Errorf("ERR21B: %s: %w", root, err))
			continue
		}
		more := ""
		if partial {
			more = " (at least)"
		}
		line := fmt.Sprintf("%s: %d files%s, %d MB", root, files, more, bytes>>20)
		if unreadable > 0 {
			line += fmt.Sprintf(", %d unreadable", unreadable)
		}
		p.Details = append(p.Details, line)
		total += files
		totalBytes += bytes
	}
	p.Summary = fmt.Sprintf("%d of %d folders readable, %d files seen", len(s.roots)-len(bad), len(s.roots), total)
	return p, errors.Join(bad...)
}
