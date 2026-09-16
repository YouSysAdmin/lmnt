package cleantext

import "testing"

func TestStrip(t *testing.T) {
	cases := map[string]string{
		"plain":                           "plain",
		"\x1b[31mred\x1b[0m":              "red",
		"\x1b]0;title\x07text":            "text",
		"bell\x07 and\x08 backspace":      "bell and backspace",
		"keeps\nnewlines\tand tabs":       "keeps\nnewlines\tand tabs",
		"login: \x1b[?2004h":              "login: ",
		"\x1b(Bcharset select":            "charset select",
		"unicode ünïcödé is printable":    "unicode ünïcödé is printable",
		"cr\r\nlf":                        "cr\nlf",
		"\x1b[1;32mgreen\x1b[m\x1b[K end": "green end",
	}

	for in, want := range cases {
		if got := Strip(in); got != want {
			t.Errorf("Strip(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLine(t *testing.T) {
	cases := map[string]string{
		"one":                      "one",
		"one\ntwo\n":               "one | two",
		"\n\none\n\n\ntwo\n":       "one | two",
		"  spaced   out  \n":       "spaced out",
		"\x1b[31merr\x1b[0m\nmore": "err | more",
		"":                         "",
	}

	for in, want := range cases {
		if got := Line(in); got != want {
			t.Errorf("Line(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("abcdef", 4); got != "abc…" {
		t.Errorf("got %q", got)
	}
	if got := Truncate("abc", 4); got != "abc" {
		t.Errorf("got %q", got)
	}
	if got := Truncate("ünï", 2); got != "ü…" {
		t.Errorf("got %q", got)
	}
	if got := Truncate("abc", 0); got != "" {
		t.Errorf("got %q", got)
	}
}
