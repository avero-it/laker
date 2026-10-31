package domain

import (
	"strings"
	"testing"
)

func TestBlobKey(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	got, err := BlobKey(sum)
	if err != nil {
		t.Fatal(err)
	}
	if want := "blobs/sha256/ab/ab/" + sum; got != want {
		t.Errorf("BlobKey = %s, want %s", got, want)
	}
	if _, err := BlobKey(strings.ToUpper(sum)); err == nil {
		t.Error("uppercase hex accepted")
	}
}

func TestValidKey(t *testing.T) {
	for _, k := range []string{"a", "mail/messages/x.parquet", "_runs/a/b.json"} {
		if err := ValidKey(k); err != nil {
			t.Errorf("ValidKey(%q) = %v", k, err)
		}
	}
	for _, k := range []string{"", "/a", "a/", "a//b", "../a", "a/./b", "a\\b"} {
		if err := ValidKey(k); err == nil {
			t.Errorf("ValidKey(%q) accepted", k)
		}
	}
}
