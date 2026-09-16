package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/yousysadmin/lmnt/internal/target"
)

// luksOptions are the LUKS container flags shared by ls and run.
type luksOptions struct {
	container   string
	entireDrive bool
}

func (o *luksOptions) addFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.container, "luks-container", "", "open this guest device (without /dev/) as a LUKS container first, so the LVM volumes inside become visible")
	fs.BoolVarP(&o.entireDrive, "luks-container-entire-drive", "c", false, "the whole attached disk is a LUKS container (same as --luks-container with the disk's device)")
}

func (o *luksOptions) validate() error {
	if o.container != "" && o.entireDrive {
		return usagef("--luks-container and -c exclude each other")
	}

	return nil
}

// requested reports whether a container must be opened.
func (o *luksOptions) requested() bool { return o.container != "" || o.entireDrive }

// device resolves the container device once the attached disk's name is
// known. It returns "" when no container was requested.
func (o *luksOptions) device(diskDevice string) (string, error) {
	switch {
	case o.container != "":
		return o.container, nil
	case o.entireDrive && diskDevice == "":
		return "", usagef("-c needs an attached disk")
	case o.entireDrive:
		return diskDevice, nil
	default:
		return "", nil
	}
}

func (a *app) lsCommand() *cobra.Command {
	var luks luksOptions

	cmd := &cobra.Command{
		Use:   "ls <target>",
		Short: "List the block devices of a disk as the guest sees them",
		Long: `ls attaches the target to a guest and prints lsblk's view of it: the
device names to pass to "lmnt run", their sizes, file systems and labels.
LVM volumes show up as mapper/<vg>-<lv>.

Targets: img:<path> (disk image), dev:<path> (physical disk, needs root),
usb:<vendor>,<product> (USB device by hex IDs, QEMU only).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.report(a.ls(cmd.Context(), args[0], &luks))
		},
	}

	luks.addFlags(cmd.Flags())

	return cmd
}

func (a *app) ls(ctx context.Context, targetArg string, luks *luksOptions) error {
	if err := luks.validate(); err != nil {
		return err
	}

	t, err := target.Parse(targetArg)
	if err != nil {
		return usagef("target: %w", err)
	}

	s := &session{app: a, target: &t, luks: luks.requested()}

	return s.run(ctx, func(ctx context.Context, e *env) error {
		container, err := luks.device(e.DiskDevice)
		if err != nil {
			return err
		}

		if container != "" {
			if err := e.CheckDevice(container); err != nil {
				return err
			}

			err = e.Guest.OpenLUKSContainer(ctx, container)
			if err != nil {
				return err
			}
		}

		var devices []string
		if e.OnlyOwnDisk && e.DiskDevice != "" {
			devices = []string{e.DiskDevice}
		}

		out, err := e.Guest.ListBlockDevices(ctx, devices...)
		if err != nil {
			return fmt.Errorf("list block devices: %w", err)
		}

		if len(out) == 0 {
			_, _ = fmt.Fprintln(a.stdout, "(no block devices)")
			return nil
		}

		_, err = a.stdout.Write(out)

		return err
	})
}
