package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/yousysadmin/lmnt/internal/cleantext"
	"github.com/yousysadmin/lmnt/internal/datadir"
	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/hostos"
	"github.com/yousysadmin/lmnt/internal/share"
)

// detachEnv carries the session id from a detaching parent to the child it
// spawns. Its presence is what tells the child it is the detached one, so the
// child can be started with exactly the arguments the user typed instead of a
// reconstructed command line that could drift from the flag set.
const detachEnv = "LMNT_DETACHED_ID"

// detachPollInterval is how often the parent looks for its child's record
// while waiting for the share to come up.
const detachPollInterval = 250 * time.Millisecond

// detachedID returns the session id this process was detached with, or "" in
// a normal foreground process.
func detachedID() string {
	id := os.Getenv(detachEnv)
	if !datadir.ValidSessionID(id) {
		return ""
	}

	return id
}

// runDetached starts the same command again in a background process that
// survives this terminal, waits until its share is up, and reports it.
func (a *app) runDetached(ctx context.Context, opts *runOptions) error {
	dir, err := a.openDataDir()
	if err != nil {
		return err
	}

	id, err := datadir.NewSessionID()
	if err != nil {
		return err
	}

	// The child has no terminal, so whatever it will ask for has to be
	// collected here and handed over out of band.
	secrets, err := collectPassphrases(opts)
	if err != nil {
		return err
	}
	defer func() {
		for _, s := range secrets {
			clear(s)
		}
	}()

	if records, err := dir.Sessions(); err == nil {
		if err := dir.PruneSessionLogs(records); err != nil {
			a.logger.Warn("Cannot tidy old session logs", "error", err)
		}
	}

	logPath, err := dir.SessionLogPath(id)
	if err != nil {
		return err
	}

	cmd, logFile, err := a.detachedCommand(id, logPath, a.args)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("prepare the background process: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the background process: %w", err)
	}

	writePassphrases(stdin, secrets)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	a.logger.Info("Starting the mount in the background", "id", id, "pid", cmd.Process.Pid, "log", logPath)

	record, err := a.awaitShare(ctx, dir, id, exited)
	if err != nil {
		// The log is many lines, a logger would escape them into one.
		a.printLogTail(logPath)

		return err
	}

	a.printDetached(record, logPath)

	return nil
}

// detachedCommand builds the background process: the same executable, the
// same arguments, its output in the session's log file, and its own process
// group so this terminal's Ctrl+C never reaches it.
func (a *app) detachedCommand(id, logPath string, args []string) (*exec.Cmd, *os.File, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("find the lmnt executable: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // logPath comes from datadir, inside lmnt's own directory
	if err != nil {
		return nil, nil, fmt.Errorf("create the session log: %w", err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), detachEnv+"="+id)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	hostos.Detach(cmd)

	return cmd, logFile, nil
}

// collectPassphrases asks for everything the detached child will need, in the
// order it will ask: the share password first, then the LUKS container and
// the volume, matching what run and guest.Mount do.
func collectPassphrases(opts *runOptions) (secrets [][]byte, err error) {
	defer func() {
		if err != nil {
			for _, s := range secrets {
				clear(s)
			}

			secrets = nil
		}
	}()

	if opts.askPassword {
		secret, err := confirmedPrompt(guest.TerminalPrompt)(sharePasswordPrompt)
		if err != nil {
			return nil, err
		}

		if err := share.ValidatePassword(string(secret)); err != nil {
			clear(secret)

			return nil, fmt.Errorf("share password: %w", err)
		}

		secrets = append(secrets, secret)
	}

	var wanted []string

	if opts.luks.requested() {
		wanted = append(wanted, "Passphrase for the LUKS container: ")
	}

	if opts.luksVolume {
		wanted = append(wanted, "Passphrase for the LUKS volume: ")
	}

	for _, prompt := range wanted {
		secret, err := guest.TerminalPrompt(prompt)
		if err != nil {
			return nil, err
		}

		secrets = append(secrets, secret)
	}

	return secrets, nil
}

// writePassphrases hands the secrets to the child one line at a time and
// closes the pipe, so a child that asks for more than it was given fails
// instead of blocking forever. It takes ownership: each secret is scrubbed
// once it is in the pipe, and callers must not read them afterwards.
func writePassphrases(stdin interface {
	Write([]byte) (int, error)
	Close() error
}, secrets [][]byte,
) {
	defer func() { _ = stdin.Close() }()

	for _, secret := range secrets {
		line := append(bytes.Clone(secret), '\n')
		_, err := stdin.Write(line)

		clear(line)
		clear(secret)

		// A broken pipe means the child is already gone, awaitShare reports
		// why, with the log to back it up.
		if err != nil {
			return
		}
	}
}

// stdinPrompt reads passphrases a detaching parent wrote to this process's
// stdin, in the order guest.Mount asks for them.
func stdinPrompt() guest.PasswordPrompt {
	r := bufio.NewReader(os.Stdin)

	return func(string) ([]byte, error) {
		line, err := r.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			return nil, fmt.Errorf("this background mount needs a passphrase it was not given: %w", err)
		}

		return bytes.TrimRight(line, "\r\n"), nil
	}
}

// awaitShare waits for the child to publish a mounted session, or to die
// trying.
func (a *app) awaitShare(ctx context.Context, dir *datadir.Dir, id string, exited <-chan error) (datadir.Session, error) {
	// The child boots a guest and starts a share, its own timeouts bound
	// that, and this one only has to outlast them.
	deadline := time.After(a.opts.setupTimeout + a.opts.bootTimeout + time.Minute)

	ticker := time.NewTicker(detachPollInterval)
	defer ticker.Stop()

	for {
		record, ok, err := findSession(dir, id)
		if err != nil {
			return datadir.Session{}, err
		}

		if ok && record.Mounted {
			return record, nil
		}

		select {
		case err := <-exited:
			return datadir.Session{}, fmt.Errorf("the background mount stopped before its share was up: %w", orExited(err))
		case <-deadline:
			return datadir.Session{}, fmt.Errorf("the background mount did not come up in time, it is still running, and its log is %s", a.sessionLog(id))
		case <-ctx.Done():
			return datadir.Session{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func orExited(err error) error {
	if err == nil {
		return errors.New("it exited cleanly")
	}

	return err
}

// findSession looks up one live session by id.
func findSession(dir *datadir.Dir, id string) (datadir.Session, bool, error) {
	records, err := dir.Sessions()
	if err != nil {
		return datadir.Session{}, false, err
	}

	for _, r := range records {
		if r.ID == id {
			return r, true, nil
		}
	}

	return datadir.Session{}, false, nil
}

// failureLogLines is how much of a failed mount's log is worth showing.
const failureLogLines = 8

// printLogTail shows the end of a failed mount's log as plain lines. It goes
// to stderr next to the error rather than through the logger, which would
// escape the line breaks into one unreadable string.
func (a *app) printLogTail(path string) {
	raw, err := os.ReadFile(path) //nolint:gosec // path comes from datadir, inside lmnt's own directory
	if err != nil {
		return
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > failureLogLines {
		lines = lines[len(lines)-failureLogLines:]
	}

	var b strings.Builder
	for _, line := range lines {
		if line = strings.TrimSpace(cleantext.Line(line)); line != "" {
			b.WriteString("  " + line + "\n")
		}
	}

	if b.Len() == 0 {
		return
	}

	_, _ = fmt.Fprintf(a.stderr, "\nLast lines of %s:\n\n%s\n", path, b.String())
}

// sessionLog is the log path for id, best effort, for error messages.
func (a *app) sessionLog(id string) string {
	dir, err := a.openDataDir()
	if err != nil {
		return "in the sessions directory"
	}

	path, err := dir.SessionLogPath(id)
	if err != nil {
		return "in the sessions directory"
	}

	return path
}

func (a *app) printDetached(record datadir.Session, logPath string) {
	_, _ = fmt.Fprintf(a.stdout, "\n%s share is up in the background as %s.\n\n%s\n\n  Log:      %s\n  Stop it:  lmnt stop %s\n",
		strings.ToUpper(record.Share), record.ID, formatShareInfo(shareInfoOf(record)), logPath, record.ID)
}

// mountArgs renders a mount the TUI collected as a `lmnt run --detach`
// command line. It has to name every global option that changes what the
// mount does, so adding one to globalOptions means adding it here.
func mountArgs(opts globalOptions, spec mountSpec, choice mountChoice) []string {
	args := []string{"run", "--detach", spec.target.String(), choice.Device}

	if choice.FSType != "" {
		args = append(args, choice.FSType)
	}

	args = append(args,
		"--share", spec.shareName,
		"--provider", spec.provider,
		"--data-dir", opts.dataDir,
		"--boot-timeout", opts.bootTimeout.String(),
		"--setup-timeout", opts.setupTimeout.String(),
	)

	// Only an explicit --memory is passed on: naming it would also switch off
	// the automatic rise to luksMemoryMiB that a LUKS mount depends on.
	if opts.memorySet {
		args = append(args, "--memory", strconv.Itoa(opts.memoryMiB))
	}

	if opts.sectorSize != 0 {
		args = append(args, "--sector-size", strconv.Itoa(opts.sectorSize))
	}

	if opts.openNetwork {
		args = append(args, "--open-network")
	}

	if choice.Options != "" {
		args = append(args, "--mount-options", choice.Options)
	}

	if choice.LUKS {
		args = append(args, "--luks")
	}

	if spec.readOnly {
		args = append(args, "--read-only")
	}

	// The password itself goes over stdin: a command line is public.
	if len(spec.sharePassword) > 0 {
		args = append(args, "--ask-share-password")
	}

	switch {
	case spec.luksEntire:
		args = append(args, "--luks-container-entire-drive")
	case spec.luksContainer != "":
		args = append(args, "--luks-container", spec.luksContainer)
	}

	return args
}

// spawnDetached starts a background mount from an explicit command line and
// hands it the passphrases it will ask for. It returns once the child is
// running, the caller waits for it to publish a mounted record.
func (a *app) spawnDetached(dir *datadir.Dir, id string, args []string, secrets [][]byte) error {
	logPath, err := dir.SessionLogPath(id)
	if err != nil {
		return err
	}

	cmd, logFile, err := a.detachedCommand(id, logPath, args)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("prepare the background process: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the background process: %w", err)
	}

	writePassphrases(stdin, secrets)

	// Nothing waits for this child: it outlives whoever started it. Reaping
	// is the init process's job once this one exits.
	go func() { _ = cmd.Wait() }()

	return nil
}
