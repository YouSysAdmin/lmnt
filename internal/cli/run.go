package cli

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yousysadmin/lmnt/internal/datadir"
	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/netx"
	"github.com/yousysadmin/lmnt/internal/share"
	"github.com/yousysadmin/lmnt/internal/target"
)

// runOptions are the flags of `lmnt run`.
type runOptions struct {
	luks          luksOptions
	luksVolume    bool
	readOnly      bool
	mountOptions  string
	shareName     string
	sharePassword string
	askPassword   bool
	listen        string
	ftpPublicIP   string
	shell         bool
	detach        bool
}

func (a *app) runCommand() *cobra.Command {
	opts := runOptions{}

	cmd := &cobra.Command{
		Use:   "run <target> [device] [fstype]",
		Short: "Mount a disk in the guest and share it with the host",
		Long: `run attaches the target, mounts the given device (or the whole disk when
no device is given) inside the guest and starts a file share for it. It then
waits, press Ctrl+C to unmount, stop the guest and clean up.

device is a name under /dev in the guest as printed by "lmnt ls": vdb1,
loop0p2, mapper/vg-root. fstype forces the file system type (mount -t).

Shares: smb (default, Finder can open it), ftp, sftp. The share
user is always "lmnt", its password is generated and printed at start unless
--share-password or --ask-share-password gives one.`,
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.report(a.run(cmd.Context(), args, &opts))
		},
	}

	f := cmd.Flags()
	opts.luks.addFlags(f)
	f.BoolVarP(&opts.luksVolume, "luks", "l", false, "the device is a LUKS volume: open it (passphrase prompted) and mount the plaintext")
	f.BoolVarP(&opts.readOnly, "read-only", "r", false, "attach the disk, mount the file system and serve the share read-only, nothing can change the disk")
	f.StringVarP(&opts.mountOptions, "mount-options", "o", "", "options for mount -o (e.g. noatime,subvol=@home)")
	f.StringVarP(&opts.shareName, "share", "s", "smb", "file share protocol: "+strings.Join(share.Names(), ", "))
	f.StringVar(&opts.sharePassword, "share-password", "", "password for the share user (default: a fresh random one)")
	f.BoolVar(&opts.askPassword, "ask-share-password", false, "type the share password instead of passing it on the command line, where the process list would show it")
	f.StringVar(&opts.listen, "listen", netx.Loopback.String(), "host address the share listens on")
	f.StringVar(&opts.ftpPublicIP, "ftp-public-ip", "", "address the FTP server advertises for passive connections (defaults to --listen)")
	f.BoolVar(&opts.shell, "shell", false, "also open an interactive guest shell, the session ends when it exits")
	f.BoolVarP(&opts.detach, "detach", "D", false, "keep the mount running in the background and return the terminal, stop it later with `lmnt stop`")

	return cmd
}

func (a *app) run(ctx context.Context, args []string, opts *runOptions) error {
	if err := opts.luks.validate(); err != nil {
		return err
	}

	t, err := target.Parse(args[0])
	if err != nil {
		return usagef("target: %w", err)
	}

	var device, fstype string
	if len(args) > 1 {
		device = args[1]
	}
	if len(args) > 2 {
		fstype = args[2]
	}

	if device == "" && opts.luks.requested() {
		return usagef("with a LUKS container the device to mount must be given explicitly, e.g. mapper/vg-root")
	}

	if opts.sharePassword != "" && opts.askPassword {
		return usagef("--share-password and --ask-share-password exclude each other")
	}

	if opts.sharePassword != "" {
		if err := share.ValidatePassword(opts.sharePassword); err != nil {
			return usagef("--share-password: %w", err)
		}
	}

	backend, ok := share.Lookup(opts.shareName)
	if !ok {
		return usagef("--share: unknown protocol %q (want %s)", opts.shareName, strings.Join(share.Names(), ", "))
	}

	listen, err := netip.ParseAddr(opts.listen)
	if err != nil {
		return usagef("--listen: %w", err)
	}

	ftpPublic := listen
	if opts.ftpPublicIP != "" {
		ftpPublic, err = netip.ParseAddr(opts.ftpPublicIP)
		if err != nil {
			return usagef("--ftp-public-ip: %w", err)
		}
	}

	if opts.detach && detachedID() == "" {
		if opts.shell {
			return usagef("--detach and --shell exclude each other: a background mount has no terminal to open a shell on")
		}

		return a.runDetached(ctx, opts)
	}

	plan, err := backend.Plan(share.Options{
		Listen:      listen,
		FTPPublicIP: ftpPublic,
		ReadOnly:    opts.readOnly,
	})
	if err != nil {
		return fmt.Errorf("plan %s share: %w", backend.Name(), err)
	}

	// A detached child has no terminal: everything it would ask for comes in
	// on stdin, from the parent that does have one, in the order it is read
	// here - the share password first, then what guest.Mount asks for.
	var handedOver guest.PasswordPrompt
	if detachedID() != "" {
		handedOver = stdinPrompt()
	}

	password, err := sharePassword(opts, handedOver)
	if err != nil {
		return err
	}

	s := &session{
		app:      a,
		target:   &t,
		forwards: plan.Forwards,
		luks:     opts.luksVolume || opts.luks.requested(),
		readOnly: opts.readOnly,
		register: true,
		id:       detachedID(),
	}

	if handedOver != nil {
		s.prompt = handedOver
	}

	return s.run(ctx, func(ctx context.Context, e *env) error {
		dev := device
		if dev == "" {
			if e.DiskDevice == "" {
				return usagef("nothing to mount: no disk is attached")
			}

			dev = e.DiskDevice
		}

		if err := e.CheckDevice(dev); err != nil {
			return err
		}

		container, err := opts.luks.device(e.DiskDevice)
		if err != nil {
			return err
		}

		if err := e.CheckDevice(container); err != nil {
			return err
		}

		err = e.Guest.Mount(ctx, guest.MountRequest{
			Device:        dev,
			FSType:        fstype,
			Options:       opts.mountOptions,
			LUKS:          opts.luksVolume,
			LUKSContainer: container,
			ReadOnly:      opts.readOnly,
		})
		if err != nil {
			return fmt.Errorf("mount: %w", err)
		}

		info, err := backend.Start(ctx, e.Guest, share.Credentials{User: share.User, Password: password})
		if err != nil {
			return fmt.Errorf("start %s share: %w", backend.Name(), err)
		}

		e.UpdateSession(func(rec *datadir.Session) {
			rec.Mounted = true
			rec.Device = dev
			rec.FSType = fstype
			rec.ReadOnly = info.ReadOnly
			rec.Share = backend.Name()
			rec.URL = info.URL
			rec.User = info.User
			rec.Password = info.Password
			rec.Hints = info.Hints
		})

		a.printShareInfo(backend.Name(), info)

		if opts.shell {
			a.logger.Info("Opening a guest shell, the share stays up until it exits")

			return a.interactiveShell(ctx, e.Guest.Executor())
		}

		<-ctx.Done()

		return nil
	})
}

// sharePasswordPrompt is what the user is asked, both here and in a parent
// collecting the password for a background mount.
const sharePasswordPrompt = "Password for the share user " + share.User + ": "

// sharePassword decides what the share is protected with: the password given
// on the command line, one typed at the terminal (or handed over by a
// detaching parent), or a fresh random one.
func sharePassword(opts *runOptions, handedOver guest.PasswordPrompt) (string, error) {
	if opts.sharePassword != "" {
		return opts.sharePassword, nil
	}

	if !opts.askPassword {
		password, err := share.NewPassword()
		if err != nil {
			return "", fmt.Errorf("generate share password: %w", err)
		}

		return password, nil
	}

	ask := confirmedPrompt(guest.TerminalPrompt)
	if handedOver != nil {
		// The parent already asked, twice, and checked the answer.
		ask = handedOver
	}

	secret, err := ask(sharePasswordPrompt)
	if err != nil {
		return "", err
	}

	defer clear(secret)

	if err := share.ValidatePassword(string(secret)); err != nil {
		return "", fmt.Errorf("share password: %w", err)
	}

	return string(secret), nil
}

// accessMode names a share's access mode the way the CLI and the TUI print it.
func accessMode(readOnly bool) string {
	if readOnly {
		return "read-only"
	}

	return "read-write"
}

func (a *app) printShareInfo(name string, info share.Info) {
	_, _ = fmt.Fprintf(a.stdout, "\n%s share is up. Press Ctrl+C to unmount and stop.\n\n%s\n", strings.ToUpper(name), formatShareInfo(info))
}

// formatShareInfo renders the connection details as aligned rows, one per
// line, for the CLI banner and the TUI detail pane alike.
// shareInfoOf rebuilds the share details from a registry record, so a mount
// this process did not start prints the same way as one it did.
func shareInfoOf(rec datadir.Session) share.Info {
	return share.Info{URL: rec.URL, User: rec.User, Password: rec.Password, ReadOnly: rec.ReadOnly, Hints: rec.Hints}
}

func formatShareInfo(info share.Info) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "  URL:      %s\n", info.URL)
	fmt.Fprintf(&sb, "  User:     %s\n", info.User)
	fmt.Fprintf(&sb, "  Password: %s\n", info.Password)
	fmt.Fprintf(&sb, "  Access:   %s\n", accessMode(info.ReadOnly))

	for _, h := range info.Hints {
		fmt.Fprintf(&sb, "  Note:     %s\n", h)
	}

	return sb.String()
}
