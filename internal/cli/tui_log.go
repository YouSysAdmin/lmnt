package cli

import (
	"strings"
	"sync"
)

// lineRing keeps the last n lines written to it. slog's text handler writes
// one record per Write call, so wrapping a ring as the handler's io.Writer
// gives a per-session log tail without a custom slog.Handler.
type lineRing struct {
	mu    sync.Mutex
	lines []string
	head  int // index of the oldest line when full
	full  bool

	// notify receives a token after every write, never blocking the writer.
	notify chan struct{}
}

func newLineRing(n int) *lineRing {
	return &lineRing{lines: make([]string, 0, max(n, 1)), notify: make(chan struct{}, 1)}
}

// Write records p as one line (trailing newline dropped).
func (r *lineRing) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\r\n")

	r.mu.Lock()
	if len(r.lines) < cap(r.lines) {
		r.lines = append(r.lines, line)
	} else {
		r.lines[r.head] = line
		r.head = (r.head + 1) % cap(r.lines)
		r.full = true
	}
	r.mu.Unlock()

	select {
	case r.notify <- struct{}{}:
	default:
	}

	return len(p), nil
}

// Snapshot returns the retained lines, oldest first.
func (r *lineRing) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.lines))
	if !r.full {
		return append(out, r.lines...)
	}

	out = append(out, r.lines[r.head:]...)

	return append(out, r.lines[:r.head]...)
}

// Tail returns up to n most recent lines, oldest first.
func (r *lineRing) Tail(n int) []string {
	all := r.Snapshot()
	if len(all) <= n {
		return all
	}

	return all[len(all)-n:]
}
