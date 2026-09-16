// Package cleantext makes untrusted terminal output safe to put into log
// lines and error messages: it strips ANSI escape sequences and control
// characters, and can collapse multi-line output onto a single line.
package cleantext

import (
	"regexp"
	"strings"
	"unicode"
)

// ansiSeq matches CSI/OSC escape sequences as emitted by shells, getty and
// most command-line tools.
var ansiSeq = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[()*+][0-9A-Za-z]|[@-Z\\-_])`)

// Strip removes escape sequences and every non-printable rune except
// newlines and tabs.
func Strip(s string) string {
	s = ansiSeq.ReplaceAllString(s, "")

	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n', r == '\t':
			return r
		case unicode.IsPrint(r):
			return r
		default:
			return -1
		}
	}, s)
}

// Line renders s as a single printable line: escape sequences and control
// characters are dropped, line breaks become " | ", and runs of whitespace
// collapse. It is meant for embedding command output into an error message.
func Line(s string) string {
	s = Strip(s)

	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' })
	for i, p := range parts {
		parts[i] = strings.Join(strings.Fields(p), " ")
	}

	// Drop empty segments left by blank lines.
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}

	return strings.Join(out, " | ")
}

// Truncate shortens s to at most n runes, marking the cut with an ellipsis.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}

	runes := []rune(s)
	if len(runes) <= n {
		return s
	}

	if n <= 1 {
		return "…"
	}

	return string(runes[:n-1]) + "…"
}
