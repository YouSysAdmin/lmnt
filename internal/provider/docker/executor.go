package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/yousysadmin/lmnt/internal/guest"
)

// executor runs guest commands with `docker exec`.
type executor struct {
	container string
}

var _ guest.Executor = (*executor)(nil)

// stderrCap bounds how much of a command's stderr is kept for the error.
const stderrCap = 16 << 10

func (e *executor) Run(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", e.container, "sh", "-c", script)

	var stdout bytes.Buffer
	stderr := newTail(stderrCap)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return nil, commandError(ctx, script, err, stderr.String())
	}

	return stdout.Bytes(), nil
}

func (e *executor) Start(ctx context.Context, script string, streams guest.Streams) (guest.Process, error) {
	args := []string{"exec"}
	if streams.Stdin != nil {
		args = append(args, "--interactive")
	}
	args = append(args, e.container, "sh", "-c", script)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = streams.Stdin
	cmd.Stdout = streams.Stdout

	stderr := newTail(stderrCap)
	if streams.Stderr != nil {
		cmd.Stderr = io.MultiWriter(streams.Stderr, stderr)
	} else {
		cmd.Stderr = stderr
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker exec: %w", err)
	}

	return &process{ctx: ctx, cmd: cmd, script: script, stderr: stderr}, nil
}

func (e *executor) Shell(ctx context.Context, tty guest.Terminal) error {
	term := tty.Term
	if term == "" {
		term = "xterm"
	}

	cmd := exec.CommandContext(ctx, "docker", "exec", "--interactive", "--tty", "--env", "TERM="+term, e.container, "sh", "-l")
	cmd.Stdin = tty.Stdin
	cmd.Stdout = tty.Stdout
	cmd.Stderr = tty.Stderr

	err := cmd.Run()
	switch {
	case err == nil, ctx.Err() != nil:
		return nil
	}

	// The shell exiting non-zero is the user's business.
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil
	}

	return fmt.Errorf("docker exec shell: %w", err)
}

// process is a command started by executor.Start.
type process struct {
	ctx    context.Context
	cmd    *exec.Cmd
	script string
	stderr *tail
}

func (p *process) Wait() error {
	if err := p.cmd.Wait(); err != nil {
		return commandError(p.ctx, p.script, err, p.stderr.String())
	}

	return nil
}

// commandError turns an exec failure into a *guest.CommandError. When ctx
// ended first, the result also matches ctx.Err().
func commandError(ctx context.Context, script string, err error, stderr string) error {
	ce := &guest.CommandError{Script: script, ExitCode: -1, Stderr: stderr, Cause: err}

	switch exitErr, ok := errors.AsType[*exec.ExitError](err); {
	case ctx.Err() != nil:
		// The process was killed on our behalf, the exit status is noise.
		ce.Cause = ctx.Err()
	case ok && exitErr.ExitCode() >= 0:
		ce.ExitCode = exitErr.ExitCode()
		ce.Cause = nil
	}

	return ce
}

// tail keeps the last n bytes written to it.
type tail struct {
	buf   []byte
	limit int
}

func newTail(limit int) *tail { return &tail{limit: limit} }

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}

	return len(p), nil
}

func (t *tail) String() string { return string(t.buf) }
