package fs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func put(t *testing.T, l *Lake, key, body string) bool {
	t.Helper()
	created, err := l.PutIfAbsent(context.Background(), key, strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("PutIfAbsent(%s) error = %v", key, err)
	}
	return created
}

func TestPutNeverOverwrites(t *testing.T) {
	l := New(t.TempDir(), zerolog.Nop())
	ctx := context.Background()

	if !put(t, l, "a/b/c.txt", "first") {
		t.Fatal("first put not created")
	}
	if put(t, l, "a/b/c.txt", "second") {
		t.Fatal("second put reported created")
	}

	rc, err := l.Get(ctx, "a/b/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "first" {
		t.Errorf("content = %q, want first", b)
	}

	if ok, _ := l.Exists(ctx, "a/b/c.txt"); !ok {
		t.Error("Exists = false")
	}
	if ok, _ := l.Exists(ctx, "a/b/missing"); ok {
		t.Error("Exists(missing) = true")
	}
}

func TestPutRejectsShortBodyAndLeavesNothing(t *testing.T) {
	root := t.TempDir()
	l := New(root, zerolog.Nop())
	if _, err := l.PutIfAbsent(context.Background(), "x", strings.NewReader("abc"), 10); err == nil {
		t.Fatal("expected an error on a short body")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("left %d file(s) behind", len(entries))
	}
}

func TestObjectsAreReadOnly(t *testing.T) {
	root := t.TempDir()
	l := New(root, zerolog.Nop())
	put(t, l, "ro", "x")
	st, err := os.Stat(filepath.Join(root, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o222 != 0 {
		t.Errorf("mode = %v, want read-only", st.Mode())
	}
}

func TestListMatchesLikeS3Prefix(t *testing.T) {
	l := New(t.TempDir(), zerolog.Nop())
	for _, k := range []string{"mail/a/1", "mail/a/2", "mail/ab/3", "files/1"} {
		put(t, l, k, k)
	}

	var got []string
	if err := l.List(context.Background(), "mail/a", func(k string) error { got = append(got, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if want := "mail/a/1,mail/a/2,mail/ab/3"; strings.Join(got, ",") != want {
		t.Errorf("List(mail/a) = %v, want %s", got, want)
	}

	got = nil
	_ = l.List(context.Background(), "nothing/", func(k string) error { got = append(got, k); return nil })
	if len(got) != 0 {
		t.Errorf("List(nothing/) = %v", got)
	}
}

func TestRejectsKeysOutsideTheRoot(t *testing.T) {
	l := New(t.TempDir(), zerolog.Nop())
	if _, err := l.PutIfAbsent(context.Background(), "../escape", strings.NewReader("x"), 1); err == nil {
		t.Fatal("key outside the root accepted")
	}
}
