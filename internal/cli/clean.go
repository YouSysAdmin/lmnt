package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yousysadmin/lmnt/internal/version"
)

func (a *app) cleanCommand() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Delete the data directory (guest image, downloads)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.report(a.clean(cmd.Context(), yes))
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")

	return cmd
}

func (a *app) clean(_ context.Context, yes bool) error {
	dir, err := a.openDataDir()
	if err != nil {
		return err
	}

	// Listing the registry drops records of sessions that are gone.
	if live, err := dir.Sessions(); err != nil {
		a.logger.Warn("Cannot read the mount registry", "error", err)
	} else if len(live) > 0 {
		return fmt.Errorf("%d lmnt mount(s) still running, stop them first", len(live))
	}

	if !yes {
		_, _ = fmt.Fprintf(a.stderr, "Delete %s and everything in it? [y/N] ", dir.Path())

		answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return fmt.Errorf("read answer: %w", err)
		}

		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			a.logger.Info("Aborted")
			return nil
		}
	}

	err = dir.RemoveAll()
	if err != nil {
		return fmt.Errorf("remove data dir: %w", err)
	}

	a.logger.Info("Removed the data directory", "path", dir.Path())

	return nil
}

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the lmnt version",
		Args:  cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			_, _ = fmt.Fprintf(a.stdout, "lmnt %s %s/%s %s\n", version.String(), runtime.GOOS, runtime.GOARCH, runtime.Version())
		},
	}
}
