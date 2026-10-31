package agentkey

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKeyAndSignature(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.key")
	k, created, err := LoadOrCreate(path)
	if err != nil || !created {
		t.Fatal(err)
	}
	again, created, err := LoadOrCreate(path)
	if err != nil || created || again.ID() != k.ID() {
		t.Fatalf("reload: %v %v", created, err)
	}
	if id := k.ID(); !strings.HasPrefix(id, "lkr-") || len(id) != 39 {
		t.Errorf("id = %q", id)
	}

	now := time.Now()
	sig := k.Sign("POST", "/agent/v1/commit", now.Unix(), "abc")
	if err := Verify(k.Public(), sig, "POST", "/agent/v1/commit", now.Unix(), "abc", now); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][3]string{{"PUT", "/agent/v1/commit", "abc"}, {"POST", "/agent/v1/finish", "abc"}, {"POST", "/agent/v1/commit", "abd"}} {
		if Verify(k.Public(), sig, bad[0], bad[1], now.Unix(), bad[2], now) == nil {
			t.Errorf("signature accepted for %v", bad)
		}
	}
	if Verify(k.Public(), sig, "POST", "/agent/v1/commit", now.Unix(), "abc", now.Add(10*time.Minute)) == nil {
		t.Error("old request accepted")
	}
	other, _, _ := LoadOrCreate(filepath.Join(t.TempDir(), "k"))
	if Verify(other.Public(), sig, "POST", "/agent/v1/commit", now.Unix(), "abc", now) == nil {
		t.Error("signature of another key accepted")
	}
}
