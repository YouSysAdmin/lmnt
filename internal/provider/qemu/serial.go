package qemu

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// loginPrompt is the tail getty prints, without a trailing newline, when it
// wants a username.
const loginPrompt = "login:"

// serialConsole talks to the guest over QEMU's -serial stdio. It runs one
// reader goroutine that splits the stream into lines and, separately,
// watches the still-incomplete line for the login prompt. Callers wait for
// the prompt, then send scripts and read their output back.
type serialConsole struct {
	out io.Writer

	lines    chan string
	loggedIn chan struct{}

	writeMu sync.Mutex

	loginOnce sync.Once
	login     chan struct{}
}

func newSerialConsole(stdin io.Writer, stdout io.Reader) *serialConsole {
	c := &serialConsole{
		out:   stdin,
		lines: make(chan string, 256),
		login: make(chan struct{}),
	}
	c.loggedIn = c.login

	go c.read(stdout)

	return c
}

// read consumes the guest's serial output. bufio.Scanner would wait for a
// newline and so miss the newline-less login prompt, hence the manual buffer.
func (c *serialConsole) read(r io.Reader) {
	defer close(c.lines)

	br := bufio.NewReader(r)
	var line strings.Builder

	for {
		b, err := br.ReadByte()
		if err != nil {
			if line.Len() > 0 {
				c.emit(line.String())
			}

			return
		}

		if b == '\n' {
			c.emit(strings.TrimRight(line.String(), "\r"))
			line.Reset()
			continue
		}

		line.WriteByte(b)

		if strings.Contains(line.String(), loginPrompt) {
			c.loginOnce.Do(func() { close(c.login) })
		}
	}
}

func (c *serialConsole) emit(s string) {
	select {
	case c.lines <- s:
	default:
		// A full buffer means nobody is reading boot chatter, dropping it is
		// fine, the sentinel-based protocol resynchronizes.
	}
}

// awaitLogin blocks until the guest shows a login prompt or ctx is done.
func (c *serialConsole) awaitLogin(ctx context.Context) error {
	select {
	case <-c.loggedIn:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// send writes s to the guest console.
func (c *serialConsole) send(s string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	_, err := io.WriteString(c.out, s)

	return err
}

// runScript sends a shell script over the console and waits for it to
// finish, returning the lines it printed between two sentinels and its exit
// status. The sentinels make the exchange robust against boot noise: the
// begin marker resynchronizes the reader, and the end marker carries the
// status.
func (c *serialConsole) runScript(ctx context.Context, script string) (output []string, exit int, err error) {
	token := newToken()
	begin := token + "-BEGIN"
	end := token + "-END:"

	// The script must be a single physical line: an interactive serial shell
	// parses one line at a time, and a multi-line block is easily corrupted
	// by boot noise. A leading newline flushes any half-typed line first, and
	// the sentinels fence the block's output and carry its exit status.
	if strings.ContainsAny(script, "\n\r") {
		return nil, 0, fmt.Errorf("serial script must be a single line")
	}

	wrapped := fmt.Sprintf("\necho %s; { %s ; }; echo %s$?\n", begin, script, end)
	if err := c.send(wrapped); err != nil {
		return nil, 0, fmt.Errorf("write script to serial: %w", err)
	}

	return c.collect(ctx, begin, end)
}

func (c *serialConsole) collect(ctx context.Context, begin, end string) ([]string, int, error) {
	var out []string
	started := false

	for {
		select {
		case <-ctx.Done():
			return out, 0, ctx.Err()
		case line, ok := <-c.lines:
			if !ok {
				return out, 0, fmt.Errorf("serial console closed before command finished")
			}

			switch {
			case !started:
				if strings.HasSuffix(line, begin) {
					started = true
				}
			case strings.Contains(line, end):
				_, status, _ := strings.Cut(line, end)
				code, convErr := parseStatus(status)
				if convErr != nil {
					return out, 0, convErr
				}

				return out, code, nil
			default:
				out = append(out, line)
			}
		}
	}
}

func parseStatus(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty exit status from serial console")
	}

	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("unexpected exit status %q from serial console", s)
		}
		n = n*10 + int(r-'0')
	}

	return n, nil
}
