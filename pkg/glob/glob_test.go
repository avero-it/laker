package glob

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"[Gmail]/All Mail", "[Gmail]/All Mail", true},
		{"[Gmail]/All Mail", "G/All Mail", false},
		{"**", "INBOX/sub/x", true},
		{"INBOX", "INBOX", true},
		{"Archive/*", "Archive/2024", true},
		{"Archive/*", "Archive/2024/01", false},
		{"Archive/**", "Archive/2024/01", true},
		{"**/.git/**", ".git", true},
		{"**/.git/**", "a/b/.git/config", true},
		{"**/.git/**", "a/b/.gitignore", false},
		{"**/.DS_Store", ".DS_Store", true},
		{"**/.DS_Store", "x/.DS_Store", true},
		{"*.tmp", "a.tmp", true},
		{"*.tmp", "d/a.tmp", false},
	}
	for _, c := range cases {
		if got := Compile(c.pattern).Match(c.s); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}
