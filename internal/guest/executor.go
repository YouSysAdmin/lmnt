// Package guest is lmnt's view of the Linux system that does the disk work.
//
// Two things live here. Executor is the narrow interface a provider (QEMU
// VM, container) implements to run shell commands in the guest. Guest builds
// on it with the operations lmnt actually needs: activating LVM, opening
// LUKS, mounting, writing config files, starting services and managing
// users. Nothing in this package knows how the guest is run.
package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/yousysadmin/lmnt/internal/cleantext"
)

// Executor runs commands inside the guest. Every command is a POSIX shell
// snippet interpreted by the guest's /bin/sh as root.
type Executor interface {
	// Run executes script and returns its stdout. A non-zero exit status is
	// reported as a *CommandError. Cancellation and deadlines come from ctx
	// alone.
	Run(ctx context.Context, script string) ([]byte, error)

	// Start launches script with the given streams attached and returns
	// without waiting for it. Nil streams are allowed. It exists for commands
	// that consume input while they run, such as a passphrase prompt.
	Start(ctx context.Context, script string, io Streams) (Process, error)

	// Shell attaches an interactive login shell to a host terminal and
	// returns when the shell exits or ctx is done. The shell's own exit
	// status is not an error.
	Shell(ctx context.Context, tty Terminal) error
}

// Process is a command started with Executor.Start.
type Process interface {
	// Wait blocks until the command exits. A non-zero exit status is a
	// *CommandError, when ctx expired first the error also matches ctx.Err().
	Wait() error
}

// Streams are the standard streams attached to a started command.
type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Terminal describes the host terminal an interactive shell is attached to.
type Terminal struct {
	// Term is the TERM value to export in the guest, e.g. "xterm-256color".
	Term          string
	Width, Height int

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// CommandError is returned when a guest command exits with a non-zero
// status. Stderr holds whatever the command printed, Error renders it as a
// single sanitized line so it can be logged as is.
type CommandError struct {
	// Script is the command that failed, possibly abbreviated.
	Script string
	// ExitCode is the process exit status, or -1 when unknown (for example
	// the connection dropped).
	ExitCode int
	Stderr   string
	// Cause is the transport-level error, if any.
	Cause error
}

func (e *CommandError) Error() string {
	var sb strings.Builder

	sb.WriteString("guest command ")
	sb.WriteString(cleantext.Truncate(cleantext.Line(e.Script), 60))

	switch {
	case e.ExitCode >= 0:
		fmt.Fprintf(&sb, " exited with status %d", e.ExitCode)
	case e.Cause != nil:
		sb.WriteString(" failed: ")
		sb.WriteString(e.Cause.Error())
	default:
		sb.WriteString(" failed")
	}

	if out := cleantext.Line(e.Stderr); out != "" {
		sb.WriteString(": ")
		sb.WriteString(cleantext.Truncate(out, 400))
	}

	return sb.String()
}

func (e *CommandError) Unwrap() error { return e.Cause }

// ExitCode returns the exit status carried by err if it is a *CommandError,
// and -1 otherwise.
func ExitCode(err error) int {
	if ce, ok := errors.AsType[*CommandError](err); ok {
		return ce.ExitCode
	}

	return -1
}

// Quote wraps s in single quotes so that the guest shell takes it literally.
// It is safe for any byte sequence, including one containing single quotes.
func Quote(s string) string {
	if s == "" {
		return "''"
	}

	if !strings.ContainsFunc(s, needsQuoting) {
		return s
	}

	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func needsQuoting(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	case r == '-', r == '_', r == '.', r == '/', r == ':', r == '@', r == '+', r == '=', r == ',':
		return false
	default:
		return true
	}
}

// QuoteAll quotes every argument and joins them with spaces.
func QuoteAll(args ...string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = Quote(a)
	}

	return strings.Join(quoted, " ")
}
