package bleve

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/avero-it/laker/intern/core/domain"
)

func open(t *testing.T) *Index {
	t.Helper()
	idx, err := Open(filepath.Join(t.TempDir(), "index"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

func hits(t *testing.T, idx *Index, q string) uint64 {
	t.Helper()
	res, err := idx.Search(context.Background(), domain.Query{Text: q, Highlight: "html"})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return res.Total
}

// The lake holds Italian and English text, often in the same message: both
// must be found with their inflections. The Italian words are on purpose.
func TestBilingualSearch(t *testing.T) {
	idx := open(t)
	docs := map[string]any{
		"mail:1":              domain.MailDoc{Kind: "mail", SHA256: "1", Subject: "Fatture di ottobre", Body: "Le fatture sono in allegato, perché servono.", From: "Anna <anna@example.com>"},
		"mail:2":              domain.MailDoc{Kind: "mail", SHA256: "2", Subject: "Invoices for October", Body: "The houses were sold; see the attached invoices.", From: "John <john@example.com>"},
		"mail:3":              domain.MailDoc{Kind: "mail", SHA256: "3", Subject: "Riunione / meeting", Body: "Ciao, the meeting about the contratti is moved."},
		"file:s:/d/Città.txt": domain.FileDoc{Kind: "file", Source: "s", Path: "/d/Città.txt", Name: "Città.txt", Content: "running reports"},
	}
	if err := idx.Apply(context.Background(), docs, nil); err != nil {
		t.Fatal(err)
	}

	for q, want := range map[string]uint64{
		"fattura":             1, // Italian plural
		"invoice":             1, // English plural
		"house":               1,
		"contratto":           1, // Italian inside an English message
		"meeting":             1,
		"perche":              1, // accents folded
		"citta":               1,
		"report":              1,
		"run":                 1, // English stemming
		"subject:invoice":     1, // fielded, English
		"subject:fattura":     1, // fielded, Italian
		`"attached invoices"`: 1, // phrase
		"invoice -october":    0,
		"+meeting +contratti": 1,
		"from:john":           1,
		"ottobre october":     2, // either language
	} {
		if got := hits(t, idx, q); got != want {
			t.Errorf("%s: %d hits, want %d", q, got, want)
		}
	}
}

func TestOutdatedIndexIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index")
	idx, err := Open(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	idx.idx.SetInternal([]byte(mappingKey), []byte("1"))
	idx.Close()
	if _, err := Open(path, time.Second); !errors.Is(err, ErrOutdated) {
		t.Fatalf("err = %v, want ErrOutdated", err)
	}
}
