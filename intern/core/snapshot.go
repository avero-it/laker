package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

// Snapshots of the derived data are kept in the lake, append-only:
// _state/<ts>.db (the bbolt file) and _index/<ts>.tar.gz (the Bleve index).
const (
	statePrefix = "_state/"
	indexPrefix = "_index/"
	tsLayout    = "20060102T150405Z"
)

// Snapshot uploads a consistent copy of the state and of the index; tmp is a
// local scratch directory, emptied afterwards.
func Snapshot(ctx context.Context, lake out.Lake, state out.StateStore, index out.Indexer, tmp string, now time.Time) ([]string, error) {
	ts := now.UTC().Format(tsLayout)
	work, err := os.MkdirTemp(tmp, "snapshot-")
	if err != nil {
		return nil, fmt.Errorf("ERR120: %w", err)
	}
	defer os.RemoveAll(work) // local scratch, not the lake

	stateFile := filepath.Join(work, "state.db")
	f, err := os.Create(stateFile)
	if err != nil {
		return nil, fmt.Errorf("ERR121: %w", err)
	}
	_, err = state.Snapshot(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("ERR122: cannot copy the state: %w", err)
	}

	indexDir := filepath.Join(work, "index")
	if err := index.Snapshot(indexDir); err != nil {
		return nil, err
	}
	archive := filepath.Join(work, "index.tar.gz")
	if err := tarGz(indexDir, archive); err != nil {
		return nil, err
	}

	var keys []string
	for key, file := range map[string]string{
		statePrefix + ts + ".db":     stateFile,
		indexPrefix + ts + ".tar.gz": archive,
	} {
		if err := putFile(ctx, lake, key, file); err != nil {
			return keys, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// SnapshotIfChanged is Snapshot, skipped when no commit reached the lake
// since the last one: a snapshot can never be removed from the lake, so an
// identical one is only a cost. Checkpoints and run records alone do not
// count as a change: a run that found nothing new is not worth a snapshot.
func SnapshotIfChanged(ctx context.Context, lake out.Lake, state out.StateStore, index out.Indexer, tmp string, now time.Time) (keys []string, skipped bool, err error) {
	// read before copying: a commit landing meanwhile is for the next one
	change, err := state.Get(domain.BucketMeta, domain.MetaLakeChange)
	if err != nil {
		return nil, false, fmt.Errorf("ERR132: %w", err)
	}
	mark := append([]byte("at:"), change...) // never empty, also before the first commit
	of, err := state.Get(domain.BucketMeta, domain.MetaSnapshotOf)
	if err != nil {
		return nil, false, fmt.Errorf("ERR133: %w", err)
	}
	if bytes.Equal(of, mark) {
		return nil, true, nil
	}
	if keys, err = Snapshot(ctx, lake, state, index, tmp, now); err != nil {
		return keys, false, err
	}
	if err := state.Apply([]out.StateChange{{Bucket: domain.BucketMeta, Key: domain.MetaSnapshotOf, Value: mark}}); err != nil {
		return keys, false, fmt.Errorf("ERR134: cannot record the snapshot: %w", err)
	}
	return keys, false, nil
}

// SnapshotDue makes the next SnapshotIfChanged take a snapshot whatever the
// lake says: for when the derived data was rebuilt, not ingested.
func SnapshotDue(state out.StateStore) error {
	return state.Apply([]out.StateChange{{Bucket: domain.BucketMeta, Key: domain.MetaSnapshotOf}})
}

// Restore downloads the latest snapshots into statePath and indexDir, which
// must not exist.
func Restore(ctx context.Context, lake out.Lake, statePath, indexDir string) ([]string, error) {
	stateKey, err := latest(ctx, lake, statePrefix, ".db")
	if err != nil {
		return nil, err
	}
	indexKey, err := latest(ctx, lake, indexPrefix, ".tar.gz")
	if err != nil {
		return nil, err
	}
	for _, p := range []string{statePath, indexDir} {
		if _, err := os.Stat(p); err == nil {
			return nil, fmt.Errorf("ERR123: %s exists, move it away before restoring", p)
		}
	}

	if err := getFile(ctx, lake, stateKey, statePath); err != nil {
		return nil, err
	}
	rc, err := lake.Get(ctx, indexKey)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if err := untarGz(rc, indexDir); err != nil {
		return nil, err
	}
	return []string{stateKey, indexKey}, nil
}

func latest(ctx context.Context, lake out.Lake, prefix, suffix string) (string, error) {
	var last string
	err := lake.List(ctx, prefix, func(k string) error {
		if strings.HasSuffix(k, suffix) && k > last {
			last = k
		}
		return nil
	})
	if err == nil && last == "" {
		err = fmt.Errorf("ERR124: no snapshot under %s", prefix)
	}
	return last, err
}

func putFile(ctx context.Context, lake out.Lake, key, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("ERR125: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("ERR126: %w", err)
	}
	_, err = lake.PutIfAbsent(ctx, key, f, st.Size())
	return err
}

func getFile(ctx context.Context, lake out.Lake, key, file string) error {
	rc, err := lake.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		return fmt.Errorf("ERR127: %w", err)
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("ERR128: %w", err)
	}
	_, err = io.Copy(f, rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("ERR129: cannot download %s: %w", key, err)
	}
	return nil
}

func tarGz(dir, file string) error {
	f, err := os.Create(file)
	if err != nil {
		return fmt.Errorf("ERR12A: %w", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err != nil {
		return fmt.Errorf("ERR12B: cannot archive the index: %w", err)
	}
	return f.Close()
}

func untarGz(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("ERR12C: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ERR12D: %w", err)
		}
		target := filepath.Join(dir, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
			return fmt.Errorf("ERR12E: unsafe path in archive: %s", hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("ERR12F: %w", err)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("ERR130: %w", err)
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("ERR131: %w", err)
		}
	}
}
