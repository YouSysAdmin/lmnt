// Package cli is the lmnt command-line interface: the cobra command tree,
// flag handling, logging setup and the orchestration that turns a command
// into a guest session. It is the only package that reads flags, prints to
// the terminal or decides the process exit code.
package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/yousysadmin/lmnt/internal/datadir"
	"github.com/yousysadmin/lmnt/internal/provider"
)

// Exit codes.
const (
	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 2
	exitInterrupted = 130
)

// Provider names accepted by --provider.
const (
	providerQEMU   = "qemu"
	providerDocker = "docker"
)

const (
	defaultMemoryMiB = 512
	// LUKS key derivation (argon2id) is memory-hungry, a 512 MiB guest fails
	// to open most volumes created with default cryptsetup settings.
	luksMemoryMiB = 2048
)

// globalOptions are the persistent flags shared by every command.
type globalOptions struct {
	provider     string
	dataDir      string
	memoryMiB    int
	sectorSize   int
	openNetwork  bool
	bootTimeout  time.Duration
	setupTimeout time.Duration
	debug        bool
	logLevel     string

	// memorySet records whether --memory was given explicitly, so the LUKS
	// default can be applied without overriding a user's choice.
	memorySet bool
}

// app carries what every command needs: options, logger and the data dir.
type app struct {
	opts   globalOptions
	logger *slog.Logger
	stdout *os.File
	stderr *os.File

	// args is what this process was invoked with, so `run --detach` can
	// start its background child with exactly the same command line.
	args []string
}

// Main runs the CLI with the given arguments and returns the process exit
// code.
func Main(args []string) int {
	a := &app{stdout: os.Stdout, stderr: os.Stderr, args: args}

	root := a.rootCommand()
	root.SetArgs(args)

	err := root.Execute()
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, provider.ErrInterrupted):
		return exitInterrupted
	case isUsageError(err):
		return exitUsage
	default:
		return exitFailure
	}
}

// usageError marks errors caused by wrong invocation rather than failure.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func isUsageError(err error) bool {
	_, ok := errors.AsType[*usageError](err)
	return ok
}

func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "lmnt",
		Short: "Mount Linux-native disks on macOS",
		Long: `lmnt runs a small Alpine Linux guest, mounts a disk there and shares it
back to the host over SMB, FTP or SFTP. LVM, LUKS and every file system Linux
can mount work without reimplementing anything on the host side.

The guest is either a QEMU virtual machine (--provider qemu, the default),
which can also take over physical disks and USB devices, or a privileged
container (--provider docker), which is quicker to start but limited to disk
image files.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return a.setup(cmd)
		},
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&a.opts.provider, "provider", "p", providerQEMU, `what runs the Linux side: "qemu" or "docker"`)
	pf.StringVarP(&a.opts.dataDir, "data-dir", "d", datadir.DefaultPath(), "directory for the VM image and downloads")
	pf.IntVarP(&a.opts.memoryMiB, "memory", "m", defaultMemoryMiB, fmt.Sprintf("guest memory in MiB (raised to %d when LUKS is used)", luksMemoryMiB))
	pf.IntVar(&a.opts.sectorSize, "sector-size", 0, "override the logical sector size of a dev: target (0 = detect, 512 for disks written by older tools)")
	pf.BoolVar(&a.opts.openNetwork, "open-network", false, "let the guest reach the network (default: isolated except forwarded ports)")
	pf.DurationVar(&a.opts.bootTimeout, "boot-timeout", 60*time.Second, "how long to wait for the guest to boot")
	pf.DurationVar(&a.opts.setupTimeout, "setup-timeout", 120*time.Second, "how long to wait for the guest to become reachable (>= boot timeout)")
	pf.BoolVar(&a.opts.debug, "debug", false, "show the guest console/QEMU display and pass provider diagnostics through")
	pf.StringVar(&a.opts.logLevel, "log-level", "info", "log verbosity: debug, info, warn, error")

	root.AddCommand(
		a.buildCommand(),
		a.createCommand(),
		a.lsCommand(),
		a.runCommand(),
		a.mountsCommand(),
		a.stopCommand(),
		a.shellCommand(),
		a.cleanCommand(),
		a.tuiCommand(),
		a.versionCommand(),
	)

	return root
}

// setup validates the global flags and builds the logger.
func (a *app) setup(cmd *cobra.Command) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(a.opts.logLevel)); err != nil {
		return usagef("--log-level: %w", err)
	}

	if a.opts.debug && level > slog.LevelDebug {
		level = slog.LevelDebug
	}

	a.logger = slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: level}))

	switch a.opts.provider {
	case providerQEMU, providerDocker:
	default:
		return usagef("--provider: unknown provider %q (want %s or %s)", a.opts.provider, providerQEMU, providerDocker)
	}

	if a.opts.memoryMiB <= 0 {
		return usagef("--memory must be positive")
	}

	if a.opts.sectorSize < 0 || a.opts.sectorSize%512 != 0 || a.opts.sectorSize > 65536 {
		return usagef("--sector-size must be a multiple of 512 up to 65536")
	}

	if a.opts.setupTimeout < a.opts.bootTimeout {
		return usagef("--setup-timeout must not be shorter than --boot-timeout")
	}

	a.opts.memorySet = cmd.Flags().Changed("memory")

	return nil
}

// openDataDir opens (creating if needed) the data directory.
func (a *app) openDataDir() (*datadir.Dir, error) {
	d, err := datadir.Open(a.logger.With("component", "datadir"), a.opts.dataDir)
	if err != nil {
		return nil, fmt.Errorf("open data dir %q: %w", a.opts.dataDir, err)
	}

	return d, nil
}

// report logs err at the right level and rewraps it for Main. Interruptions
// are the user's decision and not reported as failures.
func (a *app) report(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, provider.ErrInterrupted) {
		a.logger.Info("Session ended")
		return err
	}

	a.logger.Error(capitalize(err.Error()))

	return err
}

func capitalize(s string) string {
	if s == "" {
		return s
	}

	return strings.ToUpper(s[:1]) + s[1:]
}

// splitList splits a comma-separated flag value, dropping empty items.
func splitList(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}

	return out
}
