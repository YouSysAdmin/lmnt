package guest

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"":                  "''",
		"plain":             "plain",
		"/dev/mapper/vg-lv": "/dev/mapper/vg-lv",
		"subvol=@home,ro":   "subvol=@home,ro",
		"has space":         "'has space'",
		"it's":              `'it'\''s'`,
		"$(rm -rf /)":       "'$(rm -rf /)'",
		"a;b":               "'a;b'",
		"new\nline":         "'new\nline'",
		"unicode ü":         "'unicode ü'",
		"back\\slash":       "'back\\slash'",
		"`tick`":            "'`tick`'",
		"tilde~":            "'tilde~'",
		"star*":             "'star*'",
		"'":                 `''\'''`,
	}

	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestQuoteAll(t *testing.T) {
	got := QuoteAll("mount", "-o", "ro,noexec", "/dev/vdb1", "/mnt point")
	want := "mount -o ro,noexec /dev/vdb1 '/mnt point'"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCommandError(t *testing.T) {
	err := &CommandError{Script: "mount /dev/vdb1 /mnt", ExitCode: 32, Stderr: "\x1b[31mmount: /mnt: wrong fs type\x1b[0m\n"}
	msg := err.Error()
	for _, want := range []string{"status 32", "wrong fs type", "mount /dev/vdb1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "\x1b") {
		t.Errorf("escape sequence leaked into %q", msg)
	}

	wrapped := fmt.Errorf("mount: %w", err)
	if ExitCode(wrapped) != 32 {
		t.Errorf("ExitCode(wrapped) = %d", ExitCode(wrapped))
	}
	if ExitCode(errors.New("other")) != -1 {
		t.Error("ExitCode of a plain error should be -1")
	}

	cause := errors.New("connection reset")
	err = &CommandError{Script: "x", ExitCode: -1, Cause: cause}
	if !errors.Is(err, cause) {
		t.Error("Cause should be unwrapped")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("got %q", err.Error())
	}
}
