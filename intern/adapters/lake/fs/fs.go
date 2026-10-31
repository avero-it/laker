// Package fs is a Lake on a local directory, for development and tests.
package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

// tmpPrefix marks a file still being written; List never shows it.
const tmpPrefix = ".laker-tmp-"

var _ out.Lake = (*Lake)(nil)

type Lake struct {
	root   string
	logger zerolog.Logger
}

func New(root string, logger zerolog.Logger) *Lake {
	return &Lake{root: root, logger: logger}
}

func (l *Lake) file(key string) (string, error) {
	if err := domain.ValidKey(key); err != nil {
		return "", err
	}
	return filepath.Join(l.root, filepath.FromSlash(key)), nil
}

// PutIfAbsent writes to a temporary file and hard-links it into place: the
// link fails if the name exists, so an object is never replaced, not even by
// a concurrent writer. Objects are made read-only.
func (l *Lake) PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) (bool, error) {
	dst, err := l.file(key)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dst); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, fmt.Errorf("ERR300: cannot create the directory of %s: %w", key, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), tmpPrefix+"*")
	if err != nil {
		return false, fmt.Errorf("ERR301: cannot create a temporary file for %s: %w", key, err)
	}
	defer os.Remove(tmp.Name()) // the temporary name only; the object stays linked

	n, err := io.Copy(tmp, contextReader{ctx, r})
	if err == nil && n != size {
		err = fmt.Errorf("ERR302: wrote %d bytes, expected %d", n, size)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o444)
	}
	if err != nil {
		return false, fmt.Errorf("ERR303: cannot write %s: %w", key, err)
	}

	if err := os.Link(tmp.Name(), dst); err != nil {
		if errors.Is(err, iofs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("ERR304: cannot link %s into place: %w", key, err)
	}
	return true, nil
}

func (l *Lake) Exists(_ context.Context, key string) (bool, error) {
	f, err := l.file(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(f)
	if errors.Is(err, iofs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ERR305: cannot stat %s: %w", key, err)
	}
	return true, nil
}

func (l *Lake) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f, err := l.file(key)
	if err != nil {
		return nil, err
	}
	rc, err := os.Open(f)
	if err != nil {
		return nil, fmt.Errorf("ERR306: cannot open %s: %w", key, err)
	}
	return rc, nil
}

// List walks the directory the prefix points into and keeps the keys that
// start with prefix, the way an S3 prefix matches.
func (l *Lake) List(ctx context.Context, prefix string, fn func(key string) error) error {
	dir := prefix
	if !strings.HasSuffix(dir, "/") {
		dir = path.Dir(dir)
	}
	start := filepath.Join(l.root, filepath.FromSlash(dir))

	err := filepath.WalkDir(start, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, iofs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		return fn(key)
	})
	if err != nil {
		return fmt.Errorf("ERR307: cannot list %q: %w", prefix, err)
	}
	return nil
}

func (l *Lake) Check(_ context.Context) (domain.LakeInfo, error) {
	info := domain.LakeInfo{Kind: "fs", Location: l.root}

	st, err := os.Stat(l.root)
	if err != nil {
		return info, fmt.Errorf("ERR308: lake directory: %w", err)
	}
	if !st.IsDir() {
		return info, fmt.Errorf("ERR309: lake path %s is not a directory", l.root)
	}
	if err := syscall.Access(l.root, 0x2 /* W_OK */); err != nil {
		return info, fmt.Errorf("ERR30A: lake directory %s is not writable: %w", l.root, err)
	}
	return info, nil
}

// contextReader stops a long copy when ctx is cancelled.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
