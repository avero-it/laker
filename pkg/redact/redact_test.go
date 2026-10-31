package redact

import "testing"

func TestString(t *testing.T) {
	for in, want := range map[string]string{
		"failed to connect to postgres://laker:s3cr3t@db:5432/erp": "failed to connect to postgres://laker:***@db:5432/erp",
		"dsn laker:pw@tcp(127.0.0.1:3306)/blog refused":            "dsn laker:***@tcp(127.0.0.1:3306)/blog refused",
		"host=db user=x password=hunter2 sslmode=disable":          "host=db user=x password=*** sslmode=disable",
		`login failed: Password: "a b c"`:                          `login failed: Password: ***`,
		"nothing to hide at imap.example.com:993":                  "nothing to hide at imap.example.com:993",
		"mailto:me@example.com":                                    "mailto:me@example.com",
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}
