package guest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"time"
)

// MountPoint is where the user's file system is mounted inside the guest.
const MountPoint = "/mnt"

const (
	// DefaultTimeout bounds the quick operations: listing devices, mounting,
	// starting a service.
	DefaultTimeout = 15 * time.Second

	// DefaultLUKSTimeout bounds a LUKS open once the passphrase is in. Key
	// derivation with a large memory cost is slow in a small guest.
	DefaultLUKSTimeout = 60 * time.Second

	// DefaultFormatTimeout bounds mkfs, which on a large disk takes far
	// longer than the quick commands.
	DefaultFormatTimeout = 5 * time.Minute
)

// Guest performs lmnt's work inside the Linux guest through an Executor. It
// knows Alpine's tools (OpenRC, BusyBox, cryptsetup, LVM) and nothing about
// how the guest itself is run.
type Guest struct {
	logger *slog.Logger
	ex     Executor

	// Timeout is the budget for each quick command issued through Run.
	Timeout time.Duration

	// LUKSTimeout is the budget for cryptsetup after the passphrase has
	// been provided.
	LUKSTimeout time.Duration

	// FormatTimeout is the budget for creating a file system.
	FormatTimeout time.Duration

	// Prompt asks for LUKS passphrases when a caller does not supply its
	// own prompt. New sets it to TerminalPrompt.
	Prompt PasswordPrompt
}

// New returns a Guest driving ex with the default timeouts.
func New(logger *slog.Logger, ex Executor) *Guest {
	return &Guest{
		logger:        logger,
		ex:            ex,
		Timeout:       DefaultTimeout,
		LUKSTimeout:   DefaultLUKSTimeout,
		FormatTimeout: DefaultFormatTimeout,
		Prompt:        TerminalPrompt,
	}
}

// Executor exposes the underlying command transport, for callers that need
// an interactive shell or long-running processes.
func (g *Guest) Executor() Executor {
	return g.ex
}

// Run executes script under the quick-command budget and returns its
// stdout.
func (g *Guest) Run(ctx context.Context, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()

	return g.ex.Run(ctx, script)
}

// feed runs script with stdin attached, under the quick-command budget, and
// folds whatever the command wrote to stderr into the returned error.
func (g *Guest) feed(ctx context.Context, script string, stdin io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()

	return feed(ctx, g.ex, script, stdin)
}

func feed(ctx context.Context, ex Executor, script string, stdin io.Reader) error {
	var stderr bytes.Buffer

	p, err := ex.Start(ctx, script, Streams{Stdin: stdin, Stderr: &stderr})
	if err != nil {
		return err
	}

	err = p.Wait()
	if err == nil {
		return nil
	}

	if ce, ok := errors.AsType[*CommandError](err); ok {
		if ce.Stderr == "" {
			ce.Stderr = stderr.String()
		}

		return ce
	}

	return &CommandError{Script: script, ExitCode: -1, Stderr: stderr.String(), Cause: err}
}

// WriteFile creates or replaces path in the guest with data and the given
// permission bits. Only the guest's shell is needed, so it works with any
// Executor.
func (g *Guest) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	if path == "" || !strings.HasPrefix(path, "/") {
		return fmt.Errorf("write file: %q is not an absolute guest path", path)
	}

	p := Quote(path)
	script := fmt.Sprintf("umask 077 && cat > %s && chmod %04o %s", p, mode.Perm(), p)

	if err := g.feed(ctx, script, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// ActivateLVM makes every volume group visible under /dev/mapper. A guest
// without udev (a container) also needs the device nodes created by hand,
// which dmsetup does when it is installed.
func (g *Guest) ActivateLVM(ctx context.Context) error {
	const script = "vgchange -ay && { ! command -v dmsetup >/dev/null 2>&1 || dmsetup mknodes; }"

	if _, err := g.Run(ctx, script); err != nil {
		return fmt.Errorf("activate lvm: %w", err)
	}

	return nil
}

// ListBlockDevices returns lsblk's table for the given devices, or for the
// whole guest minus loop, CD-ROM and floppy devices when none are given.
func (g *Guest) ListBlockDevices(ctx context.Context, devices ...string) ([]byte, error) {
	args := []string{"lsblk", "--output", "NAME,SIZE,FSTYPE,LABEL"}

	if len(devices) == 0 {
		args = append(args, "--exclude", "2,7,11")
	}

	for _, d := range devices {
		p, err := devicePath(d)
		if err != nil {
			return nil, fmt.Errorf("list block devices: %w", err)
		}

		args = append(args, p)
	}

	out, err := g.Run(ctx, QuoteAll(args...))
	if err != nil {
		return nil, fmt.Errorf("list block devices: %w", err)
	}

	return out, nil
}

// EnableService starts an OpenRC service. Adding it to a runlevel is not
// needed for a one-session guest, so it is not done.
func (g *Guest) EnableService(ctx context.Context, name string) error {
	if err := checkServiceName(name); err != nil {
		return fmt.Errorf("enable service: %w", err)
	}

	if _, err := g.Run(ctx, QuoteAll("rc-service", name, "start")); err != nil {
		return fmt.Errorf("start service %s: %w", name, err)
	}

	return nil
}

// Logger returns the guest's logger, for callers that want their messages
// next to the guest's own.
func (g *Guest) Logger() *slog.Logger { return g.logger }
