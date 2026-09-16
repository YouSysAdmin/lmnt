package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/netx"
	"github.com/yousysadmin/lmnt/internal/target"
	"golang.org/x/term"
)

func (a *app) shellCommand() *cobra.Command {
	var forwards string

	cmd := &cobra.Command{
		Use:   "shell [target]",
		Short: "Open a root shell in the guest",
		Long: `shell boots the guest, optionally with a target attached, and gives you a
root shell in it: for formatting disks, repairing file systems, or looking
around. The guest is thrown away when the shell exits.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.report(a.shell(cmd.Context(), args, forwards))
		},
	}

	cmd.Flags().StringVar(&forwards, "forward", "", "extra TCP forwards into the guest, comma separated: <host port>:<guest port> or <ip>:<host port>:<guest port>")

	return cmd
}

func (a *app) shell(ctx context.Context, args []string, forwardSpecs string) error {
	s := &session{app: a, register: true}

	if len(args) == 1 {
		t, err := target.Parse(args[0])
		if err != nil {
			return usagef("target: %w", err)
		}

		s.target = &t
	}

	for _, spec := range splitList(forwardSpecs) {
		f, err := netx.ParseForward(spec)
		if err != nil {
			return usagef("--forward: %w", err)
		}

		s.forwards = append(s.forwards, f)
	}

	return s.run(ctx, func(ctx context.Context, e *env) error {
		if e.DiskDevice != "" {
			a.logger.Info("The disk is attached", "device", "/dev/"+e.DiskDevice)
		}

		return a.interactiveShell(ctx, e.Guest.Executor())
	})
}

// interactiveShell puts the host terminal into raw mode and attaches the
// guest's shell to it.
func (a *app) interactiveShell(ctx context.Context, ex guest.Executor) error {
	in := int(os.Stdin.Fd())
	if !term.IsTerminal(in) {
		return fmt.Errorf("stdin is not a terminal")
	}

	width, height, err := term.GetSize(in)
	if err != nil {
		width, height = 80, 24
	}

	state, err := term.MakeRaw(in)
	if err != nil {
		return fmt.Errorf("switch terminal to raw mode: %w", err)
	}

	defer func() {
		if err := term.Restore(in, state); err != nil {
			a.logger.Warn("Failed to restore the terminal", "error", err)
		}
	}()

	termName := os.Getenv("TERM")
	if termName == "" {
		termName = "xterm-256color"
	}

	err = ex.Shell(ctx, guest.Terminal{
		Term:   termName,
		Width:  width,
		Height: height,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		return fmt.Errorf("guest shell: %w", err)
	}

	return nil
}
