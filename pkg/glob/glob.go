// Package glob matches slash-separated paths against patterns where *
// matches within one segment, ** across segments and ? one character.
// Every other character, [ and ] included, is literal: IMAP folders such as
// "[Gmail]/All Mail" match themselves.
package glob

import (
	"regexp"
	"strings"
)

type Pattern struct{ re *regexp.Regexp }

func Compile(pattern string) Pattern {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "/**") && i+3 == len(pattern):
			b.WriteString("(?:/.*)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			i++
		}
	}
	b.WriteString("$")
	return Pattern{re: regexp.MustCompile(b.String())}
}

func (p Pattern) Match(s string) bool { return p.re.MatchString(s) }

// Set matches if any of its patterns does.
type Set []Pattern

func CompileAll(patterns []string) Set {
	s := make(Set, 0, len(patterns))
	for _, p := range patterns {
		s = append(s, Compile(p))
	}
	return s
}

func (s Set) Match(str string) bool {
	for _, p := range s {
		if p.Match(str) {
			return true
		}
	}
	return false
}
