package core

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
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

// parquetKeys lists the Parquet files under prefix that keep accepts (nil:
// all), in the order the runs wrote them. A file is named <run>-<seq>: run
// ids are ULIDs, so their order is the order of the runs, and seq numbers the
// files of one run. Runs of one source never overlap, so reading the files in
// this order gives the rows of each source in the order they were seen,
// without holding more than one file in memory.
func parquetKeys(ctx context.Context, lake out.Lake, prefix string, keep func(key string) bool) ([]string, error) {
	type file struct {
		key, run string
		seq      int
	}
	var files []file
	err := lake.List(ctx, prefix, func(k string) error {
		if !strings.HasSuffix(k, ".parquet") || (keep != nil && !keep(k)) {
			return nil
		}
		f := file{key: k, run: strings.TrimSuffix(path.Base(k), ".parquet")}
		if i := strings.LastIndexByte(f.run, '-'); i >= 0 {
			if n, err := strconv.Atoi(f.run[i+1:]); err == nil {
				f.run, f.seq = f.run[:i], n
			}
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if a.run != b.run {
			return a.run < b.run
		}
		if a.seq != b.seq {
			return a.seq < b.seq
		}
		return a.key < b.key
	})
	keys := make([]string, len(files))
	for i, f := range files {
		keys[i] = f.key
	}
	return keys, nil
}

// eachRow visits the rows of the files at keys, one file at a time. T may
// name only some columns of the files: the others are not decoded. With seen,
// the rows of each file are visited in the order they were seen.
func eachRow[T any](ctx context.Context, lake out.Lake, keys []string, seen func(T) time.Time, fn func(T) error) error {
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := readParquet[T](ctx, lake, k)
		if err != nil {
			return err
		}
		if seen != nil {
			sort.SliceStable(rows, func(i, j int) bool { return seen(rows[i]).Before(seen(rows[j])) })
		}
		for _, r := range rows {
			if err := fn(r); err != nil {
				return err
			}
		}
	}
	return nil
}

// each visits every row under prefix, in the order of parquetKeys.
func each[T any](ctx context.Context, lake out.Lake, prefix string, seen func(T) time.Time, fn func(T) error) error {
	keys, err := parquetKeys(ctx, lake, prefix, nil)
	if err != nil {
		return err
	}
	return eachRow(ctx, lake, keys, seen, fn)
}

// EachMail visits every mail row in the lake.
func EachMail(ctx context.Context, lake out.Lake, fn func(domain.MailRow) error) error {
	return each(ctx, lake, "mail/messages/", nil, fn)
}

// mailRef is where a message was seen: what the rebuilds need of a mail row
// when they do not need its text.
type mailRef struct {
	SHA256      string    `parquet:"sha256"`
	Account     string    `parquet:"account"`
	Folder      string    `parquet:"folder"`
	UIDValidity int64     `parquet:"uidvalidity"`
	UID         int64     `parquet:"uid"`
	SeenAt      time.Time `parquet:"seen_at"`
}

func eachMailRef(ctx context.Context, lake out.Lake, fn func(mailRef) error) error {
	return each(ctx, lake, "mail/messages/", nil, fn)
}

// EachFile visits the inventory rows of source ("" for every source) in the
// order they were seen, up to until (zero: no limit).
func EachFile(ctx context.Context, lake out.Lake, source string, until time.Time, fn func(domain.FileRow) error) error {
	prefix := "files/inventory/"
	if source != "" {
		prefix += "source=" + source + "/"
	}
	keys, err := parquetKeys(ctx, lake, prefix, func(k string) bool {
		if until.IsZero() {
			return true
		}
		// dt=YYYY-MM-DD partitions after the limit cannot hold earlier rows
		dt := strings.TrimPrefix(path.Base(path.Dir(k)), "dt=")
		return dt <= until.UTC().Format("2006-01-02")
	})
	if err != nil {
		return err
	}
	seen := func(r domain.FileRow) time.Time { return r.SeenAt }
	return eachRow(ctx, lake, keys, seen, func(r domain.FileRow) error {
		if !until.IsZero() && r.SeenAt.After(until) {
			return nil
		}
		return fn(r)
	})
}

// EachText visits every extracted text in the lake.
func EachText(ctx context.Context, lake out.Lake, fn func(domain.TextRow) error) error {
	return each(ctx, lake, "files/text/", nil, fn)
}

// textRef names an extracted text without carrying it.
type textRef struct {
	SHA256 string `parquet:"sha256"`
	Source string `parquet:"source"`
}

func eachTextRef(ctx context.Context, lake out.Lake, fn func(textRef) error) error {
	return each(ctx, lake, "files/text/", nil, fn)
}

// EachChat visits every chat message row, in the order they were seen.
func EachChat(ctx context.Context, lake out.Lake, fn func(domain.ChatRow) error) error {
	return each(ctx, lake, "chat/messages/", func(r domain.ChatRow) time.Time { return r.SeenAt }, fn)
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
