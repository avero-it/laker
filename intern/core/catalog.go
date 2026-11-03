package core

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/parquetx"
)

// The catalog reads back what the runs wrote. Everything derived (index,
// state, a tree at a past date) is computed from here.

func readParquet[T any](ctx context.Context, lake out.Lake, key string) ([]T, error) {
	rc, err := lake.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("ERR110: cannot read %s: %w", key, err)
	}
	rows, err := parquetx.Read[T](b)
	if err != nil {
		return nil, fmt.Errorf("ERR111: %s: %w", key, err)
	}
	return rows, nil
}

// EachMail visits every mail row in the lake.
func EachMail(ctx context.Context, lake out.Lake, fn func(domain.MailRow) error) error {
	var keys []string
	err := lake.List(ctx, "mail/messages/", func(k string) error {
		if strings.HasSuffix(k, ".parquet") {
			keys = append(keys, k)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, k := range keys {
		rows, err := readParquet[domain.MailRow](ctx, lake, k)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := fn(r); err != nil {
				return err
			}
		}
	}
	return nil
}

// EachFile visits the inventory rows of source ("" for every source) in the
// order they were seen, up to until (zero: no limit).
func EachFile(ctx context.Context, lake out.Lake, source string, until time.Time, fn func(domain.FileRow) error) error {
	prefix := "files/inventory/"
	if source != "" {
		prefix += "source=" + source + "/"
	}
	var keys []string
	err := lake.List(ctx, prefix, func(k string) error {
		if !strings.HasSuffix(k, ".parquet") {
			return nil
		}
		if !until.IsZero() {
			// dt=YYYY-MM-DD partitions after the limit cannot hold earlier rows
			if dt := strings.TrimPrefix(path.Base(path.Dir(k)), "dt="); dt > until.UTC().Format("2006-01-02") {
				return nil
			}
		}
		keys = append(keys, k)
		return nil
	})
	if err != nil {
		return err
	}

	var all []domain.FileRow
	for _, k := range keys {
		rows, err := readParquet[domain.FileRow](ctx, lake, k)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if until.IsZero() || !r.SeenAt.After(until) {
				all = append(all, r)
			}
		}
	}
	// run ids are ULIDs: within one instant, the later run wins
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].SeenAt.Equal(all[j].SeenAt) {
			return all[i].SeenAt.Before(all[j].SeenAt)
		}
		return all[i].RunID < all[j].RunID
	})
	for _, r := range all {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// EachText visits every extracted text in the lake.
func EachText(ctx context.Context, lake out.Lake, fn func(domain.TextRow) error) error {
	var keys []string
	err := lake.List(ctx, "files/text/", func(k string) error {
		if strings.HasSuffix(k, ".parquet") {
			keys = append(keys, k)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, k := range keys {
		rows, err := readParquet[domain.TextRow](ctx, lake, k)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := fn(r); err != nil {
				return err
			}
		}
	}
	return nil
}

// EachChat visits every chat message row, in the order they were seen.
func EachChat(ctx context.Context, lake out.Lake, fn func(domain.ChatRow) error) error {
	var keys []string
	err := lake.List(ctx, "chat/messages/", func(k string) error {
		if strings.HasSuffix(k, ".parquet") {
			keys = append(keys, k)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var all []domain.ChatRow
	for _, k := range keys {
		rows, err := readParquet[domain.ChatRow](ctx, lake, k)
		if err != nil {
			return err
		}
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].SeenAt.Equal(all[j].SeenAt) {
			return all[i].SeenAt.Before(all[j].SeenAt)
		}
		return all[i].RunID < all[j].RunID
	})
	for _, r := range all {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

var zeroTime time.Time

// TreeAt is the tree of each source at the given moment (zero: latest).
func TreeAt(ctx context.Context, lake out.Lake, source string, at time.Time) (map[string]domain.Tree, error) {
	trees := map[string]domain.Tree{}
	err := EachFile(ctx, lake, source, at, func(r domain.FileRow) error {
		t, ok := trees[r.Source]
		if !ok {
			t = domain.Tree{}
			trees[r.Source] = t
		}
		t.Apply(r)
		return nil
	})
	return trees, err
}
