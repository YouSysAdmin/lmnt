package cli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"github.com/yousysadmin/lmnt/internal/provider"
	"golang.org/x/term"
)

func (a *app) tuiCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Interactive terminal UI: see, add and stop mounts",
		Long: `tui opens a full-screen terminal interface. It lists every lmnt mount on
this machine, including those started with "lmnt run" in other terminals,
with their share URL and password, and lets you add mounts step by step:
the guest boots once, you pick the device from its lsblk view, enter a LUKS
passphrase if needed, and the share starts.

The global flags (--provider, --memory, --data-dir, ...) are the defaults for
new mounts. Mounts started here end when the TUI quits.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.report(a.tui(cmd.Context()))
		},
	}
}

// tui runs the Bubble Tea program and tears down whatever it started.
func (a *app) tui(ctx context.Context) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return usagef("lmnt tui needs an interactive terminal")
	}

	// While the screen is ours nothing may write to stderr, so the app
	// logger goes to a ring buffer for the duration.
	appLogs := newLineRing(logTailLines)
	cliLogger := a.logger
	a.logger = slog.New(slog.NewTextHandler(appLogs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	defer func() { a.logger = cliLogger }()

	dir, err := a.openDataDir()
	if err != nil {
		return err
	}

	rt := &tuiRuntime{
		app:     a,
		dir:     dir,
		ctx:     ctx,
		appLogs: appLogs,
		started: map[string]time.Time{},
	}

	rt.spawn = rt.startMount

	program := tea.NewProgram(newTUIModel(rt), tea.WithContext(ctx), tea.WithoutSignalHandler())
	rt.program = program

	// Bubble Tea's own handler would leave the screen in a mess. Ours ends the
	// program cleanly. Background mounts keep running: they belong to their
	// own processes, and a signal to this one is not an answer to whether
	// they should be unmounted.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)

	go func() {
		for range signals {
			program.Send(signalQuitMsg{})
		}
	}()

	_, runErr := program.Run()

	// Whatever way the program ended, the listing guest must not outlive it.
	rt.shutdownListing(provider.StopTimeout + 15*time.Second)

	if errors.Is(runErr, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}

	return runErr
}
