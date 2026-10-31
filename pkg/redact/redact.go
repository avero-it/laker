// Package redact removes credentials from text that leaves its process: run
// errors sent by an agent to laker, log lines.
package redact

import "regexp"

var patterns = []struct {
	re   *regexp.Regexp
	with string
}{
	// scheme://user:secret@host
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/@\s]*:)[^@\s]*@`), "${1}***@"},
	// user:secret@tcp(host) — MySQL DSN
	{regexp.MustCompile(`([^\s:/@]+:)[^@\s]*@(tcp|unix|udp)\(`), "${1}***@${2}("},
	// password=secret, pwd: secret, passwd="secret"
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|apikey|api_key)(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&]+)`), "${1}${2}***"},
}

// String returns s with every recognizable credential replaced by ***.
func String(s string) string {
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.with)
	}
	return s
}
