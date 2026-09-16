package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/yousysadmin/lmnt/internal/datadir"
	"github.com/yousysadmin/lmnt/internal/provider"
)

// stopWaitTimeout bounds how long `lmnt stop` waits for a mount to actually
// go away: the owner's own shutdown budget plus a moment to write its record.
const stopWaitTimeout = provider.StopTimeout + 10*time.Second

func (a *app) mountsCommand() *cobra.Command {
	var verbose bool

	cmd := &cobra.Command{
		Use:     "mounts",
		Aliases: []string{"ps"},
		Short:   "List the mounts running on this machine",
		Long: `mounts lists every lmnt mount currently running, whether it was started
here in the background with "lmnt run --detach", in another terminal, or in
the terminal UI. Records of mounts whose process is gone are cleaned up on
the way.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.report(a.mounts(cmd.Context(), verbose))
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "also show the share URL, user and password")

	return cmd
}

func (a *app) mounts(_ context.Context, verbose bool) error {
	records, err := a.liveMounts()
	if err != nil {
		return err
	}

	if len(records) == 0 {
		_, _ = fmt.Fprintln(a.stdout, "(no mounts are running)")
		return nil
	}

	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(w, "ID\tPID\tPROVIDER\tTARGET\tDEVICE\tACCESS\tSHARE\tUP\tAGE")

	for _, r := range records {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.PID, r.Provider, orDash(r.Target), orDash(r.Device),
			shortAccess(r), orDash(r.Share), yesNo(r.Mounted), age(r.Started))
	}

	if err := w.Flush(); err != nil {
		return err
	}

	if verbose {
		for _, r := range records {
			if r.URL == "" {
				continue
			}

			_, _ = fmt.Fprintf(a.stdout, "\n%s\n%s", r.ID, formatShareInfo(shareInfoOf(r)))
		}
	}

	return nil
}

// shortAccess is the ACCESS column: "ro" or "rw" once the mount is up, a
// dash while it is still booting and the record does not say yet.
func shortAccess(r datadir.Session) string {
	switch {
	case !r.Mounted:
		return "—"
	case r.ReadOnly:
		return "ro"
	default:
		return "rw"
	}
}

func (a *app) stopCommand() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "stop [id...]",
		Short: "Stop mounts running in the background",
		Long: `stop asks a running mount to unmount and shut its guest down, the same
way Ctrl+C does in the terminal that started it. IDs come from "lmnt mounts".

The mount is asked, not killed: it unmounts the file system and powers the
guest off before it goes, so nothing is left half-written on the disk.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.report(a.stop(cmd.Context(), args, all))
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "stop every running mount")

	return cmd
}

func (a *app) stop(ctx context.Context, ids []string, all bool) error {
	switch {
	case all && len(ids) > 0:
		return usagef("--all takes no ids")
	case !all && len(ids) == 0:
		return usagef("name the mounts to stop, or pass --all (see `lmnt mounts`)")
	}

	records, err := a.liveMounts()
	if err != nil {
		return err
	}

	if all {
		ids = make([]string, 0, len(records))
		for _, r := range records {
			ids = append(ids, r.ID)
		}

		if len(ids) == 0 {
			_, _ = fmt.Fprintln(a.stdout, "(no mounts are running)")
			return nil
		}
	}

	dir, err := a.openDataDir()
	if err != nil {
		return err
	}

	for _, id := range ids {
		if !slices.ContainsFunc(records, func(r datadir.Session) bool { return r.ID == id }) {
			return usagef("no mount %q is running (see `lmnt mounts`)", id)
		}
	}

	for _, id := range ids {
		if err := dir.RequestStop(id); err != nil {
			return err
		}

		a.logger.Info("Asked the mount to stop", "id", id)
	}

	return a.awaitStopped(ctx, dir, ids)
}

// awaitStopped waits for the asked mounts to disappear from the registry, so
// the command does not return while a disk is still attached.
func (a *app) awaitStopped(ctx context.Context, dir *datadir.Dir, ids []string) error {
	deadline := time.After(stopWaitTimeout)

	ticker := time.NewTicker(detachPollInterval)
	defer ticker.Stop()

	for {
		var pending []string

		for _, id := range ids {
			_, ok, err := findSession(dir, id)
			if err != nil {
				return err
			}

			if ok {
				pending = append(pending, id)
			}
		}

		if len(pending) == 0 {
			_, _ = fmt.Fprintf(a.stdout, "Stopped %s.\n", strings.Join(ids, ", "))
			return nil
		}

		select {
		case <-deadline:
			return fmt.Errorf("still shutting down after %v: %s", stopWaitTimeout, strings.Join(pending, ", "))
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// liveMounts lists the running mounts, newest last.
func (a *app) liveMounts() ([]datadir.Session, error) {
	dir, err := a.openDataDir()
	if err != nil {
		return nil, err
	}

	records, err := dir.Sessions()
	if err != nil {
		return nil, fmt.Errorf("list mounts: %w", err)
	}

	slices.SortFunc(records, func(x, y datadir.Session) int { return x.Started.Compare(y.Started) })

	return records, nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}

// age renders how long a mount has been running, coarsely.
func age(t time.Time) string {
	d := time.Since(t).Truncate(time.Second)

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
