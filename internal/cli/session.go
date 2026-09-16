package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/yousysadmin/lmnt/internal/datadir"
	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/hostos"
	"github.com/yousysadmin/lmnt/internal/netx"
	"github.com/yousysadmin/lmnt/internal/provider"
	"github.com/yousysadmin/lmnt/internal/provider/docker"
	"github.com/yousysadmin/lmnt/internal/provider/qemu"
	"github.com/yousysadmin/lmnt/internal/target"
)

// session is one guest lifetime: what to attach and which ports to forward.
// run boots the provider and hands a ready environment to the command body.
type session struct {
	app    *app
	target *target.Target // nil when nothing is attached

	forwards []netx.Forward

	// luks raises the memory default.
	luks bool

	// readOnly attaches the target so the guest cannot write to it. The
	// mount and the share are made read-only by the command body.
	readOnly bool

	// register publishes the session in the data directory's registry so
	// other lmnt processes (the TUI) can list and stop it.
	register bool

	// prompt replaces the guest's terminal passphrase prompt, nil keeps it.
	prompt guest.PasswordPrompt

	// runner drives the provider, nil means provider.Run with its signal
	// handling. The TUI supplies provider.RunContext.
	runner runnerFunc

	// Set by run.
	id  string
	dir *datadir.Dir
}

// runnerFunc is the signature shared by provider.Run and provider.RunContext.
type runnerFunc func(ctx context.Context, logger *slog.Logger, p provider.Provider, task provider.Task) error

// env is what a command body gets once the guest is up.
type env struct {
	Guest *guest.Guest
	// Session is the registry record written so far (ID, PID, Started,
	// Provider, Target), a body that starts a share completes it with
	// UpdateSession. Its ID is empty when the session is not registered.
	Session datadir.Session
	Dir     *datadir.Dir
	// DiskDevice is the guest name of the attached disk, "" when none.
	DiskDevice string
	// OnlyOwnDisk means device listings should be restricted to DiskDevice,
	// because the guest's other block devices are its own system disk or the
	// container runtime's.
	OnlyOwnDisk bool
}

type taskFunc func(ctx context.Context, e *env) error

// run boots the selected provider, prepares the guest and runs task.
func (s *session) run(ctx context.Context, task taskFunc) error {
	if s.target != nil {
		err := s.checkTargetAccess()
		if err != nil {
			return err
		}
	}

	dir, err := s.app.openDataDir()
	if err != nil {
		return err
	}

	s.dir = dir

	if s.register && s.id == "" {
		s.id, err = datadir.NewSessionID()
		if err != nil {
			return err
		}
	}

	switch s.app.opts.provider {
	case providerDocker:
		return s.runDocker(ctx, task)
	default:
		return s.runQEMU(ctx, task)
	}
}

// checkTargetAccess enforces what a target needs from the host: root for
// real hardware, and a disk that is not in use by the host.
func (s *session) checkTargetAccess() error {
	t := s.target

	switch t.Kind {
	case target.Image:
		return nil
	case target.Device, target.USB:
		root, err := hostos.IsRoot()
		if err != nil {
			return fmt.Errorf("check privileges: %w", err)
		}

		if !root {
			return usagef("%s targets need root (administrator) privileges", t.Kind)
		}
	}

	if t.Kind == target.Device {
		err := hostos.ValidateDevicePath(t.Path)
		if err != nil {
			return usagef("%s: %w", t.Path, err)
		}

		mounted, err := hostos.DeviceMounted(t.Path)
		if err != nil {
			return fmt.Errorf("check whether %s is mounted: %w", t.Path, err)
		}

		if mounted {
			return fmt.Errorf("%s appears to be mounted on the host, unmount (eject) it first", t.Path)
		}
	}

	return nil
}

func (s *session) memoryMiB() int {
	if s.luks && !s.app.opts.memorySet && s.app.opts.memoryMiB < luksMemoryMiB {
		return luksMemoryMiB
	}

	return s.app.opts.memoryMiB
}

func (s *session) runDocker(ctx context.Context, task taskFunc) error {
	var imagePath string
	if s.target != nil {
		if s.target.Kind != target.Image {
			return usagef("--provider docker can only attach disk images (img:<path>): the container runtime's VM cannot see host disks. Use --provider qemu for %s targets", s.target.Kind)
		}

		imagePath = s.target.Path
	}

	c, err := docker.New(s.app.logger.With("component", "docker"), docker.Config{
		ImagePath: imagePath,
		ReadOnly:  s.readOnly,
		Forwards:  s.forwards,
		Debug:     s.app.opts.debug,
	})
	if err != nil {
		return fmt.Errorf("prepare container: %w", err)
	}

	return s.drive(ctx, c, true, task)
}

func (s *session) runQEMU(ctx context.Context, task taskFunc) error {
	dir := s.dir

	exists, err := dir.VMImageExists()
	if err != nil {
		return err
	}

	if !exists {
		return fmt.Errorf("the guest image is not built yet, run `lmnt build` first")
	}

	firmware, err := dir.EnsureFirmware(ctx)
	if err != nil {
		return fmt.Errorf("firmware: %w", err)
	}

	cfg := qemu.Config{
		SystemDisk:   qemu.Disk{Path: dir.VMImagePath(), Format: "qcow2", Snapshot: true},
		Firmware:     firmware,
		MemoryMiB:    s.memoryMiB(),
		Forwards:     s.forwards,
		OpenNetwork:  s.app.opts.openNetwork,
		BootTimeout:  s.app.opts.bootTimeout,
		SetupTimeout: s.app.opts.setupTimeout,
		Debug:        s.app.opts.debug,
	}

	if s.target != nil {
		err = s.attachTarget(&cfg)
		if err != nil {
			return err
		}
	}

	m, err := qemu.New(s.app.logger.With("component", "qemu"), cfg)
	if err != nil {
		return fmt.Errorf("prepare virtual machine: %w", err)
	}

	// The VM's own system disk (vda) is of no interest to the user.
	return s.drive(ctx, m, true, task)
}

// attachTarget turns the target into QEMU disks or USB devices.
func (s *session) attachTarget(cfg *qemu.Config) error {
	t := s.target

	switch t.Kind {
	case target.Image:
		cfg.ExtraDisks = append(cfg.ExtraDisks, qemu.Disk{Path: t.Path, Format: "raw", ReadOnly: s.readOnly})
	case target.Device:
		sector := s.app.opts.sectorSize
		if sector == 0 {
			detected, err := hostos.DeviceSectorSize(t.Path)
			if err != nil {
				return fmt.Errorf("detect sector size of %s (use --sector-size to set it): %w", t.Path, err)
			}

			sector = detected
		} else {
			s.app.logger.Warn("Using a forced sector size, a wrong value makes the disk look corrupt inside the guest", "sector-size", sector)
		}

		s.app.logger.Warn("Passing a physical disk to the guest. Never mount it on the host while lmnt runs, lmnt kills the guest if that happens, but data may already be damaged.", "device", t.Path)

		cfg.ExtraDisks = append(cfg.ExtraDisks, qemu.Disk{Path: t.Path, Format: "raw", SectorSize: sector, HostDevice: true, ReadOnly: s.readOnly})
	case target.USB:
		s.app.logger.Warn("USB passthrough on macOS is unreliable, prefer dev:")

		if s.readOnly {
			s.app.logger.Warn("A passed-through USB device cannot be attached read-only, only the mount and the share are read-only")
		}

		cfg.USB = append(cfg.USB, qemu.USBDevice{VendorID: t.VendorID, ProductID: t.ProductID})
	}

	return nil
}

// stopPollInterval is how often a registered session looks for a stop
// request from another lmnt process.
const stopPollInterval = 500 * time.Millisecond

// watchStopRequest ends the session when another process asks it to, which is
// how `lmnt stop` and the TUI reach a session they do not own. The returned
// function ends the watch.
func (s *session) watchStopRequest(ctx context.Context, cancel context.CancelCauseFunc) func() {
	done := make(chan struct{})

	go func() {
		ticker := time.NewTicker(stopPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				requested, err := s.dir.StopRequested(s.id)
				if err != nil {
					s.app.logger.Warn("Cannot check for a stop request", "error", err)
					continue
				}

				if requested {
					s.app.logger.Info("Another lmnt process asked this mount to stop")
					cancel(provider.ErrInterrupted)

					return
				}
			}
		}
	}()

	return func() { close(done) }
}

// drive is the part shared by both providers: provider.Run plus guest
// preparation (LVM activation) before the task.
func (s *session) drive(ctx context.Context, p provider.Provider, onlyOwnDisk bool, task taskFunc) error {
	runner := s.runner
	if runner == nil {
		runner = provider.Run
	}

	// A registered session can be stopped by anyone who can see its record,
	// so the cancellation has to be reachable from outside this call.
	if s.register {
		var cancel context.CancelCauseFunc

		ctx, cancel = context.WithCancelCause(ctx)
		defer cancel(nil)

		// A stale request from a previous session must not stop this one.
		if err := s.dir.ClearStop(s.id); err != nil {
			return err
		}

		defer s.watchStopRequest(ctx, cancel)()
	}

	// The record is published before the guest boots and withdrawn only
	// after it is gone, so it covers the whole session: a mount that is
	// still booting is listed, and `lmnt stop` does not report success while
	// the disk is still attached.
	record, unregister, err := s.publish()
	if err != nil {
		return err
	}
	defer unregister()

	return runner(ctx, s.app.logger, p, func(ctx context.Context, ex guest.Executor) error {
		g := guest.New(s.app.logger.With("component", "guest"), ex)
		if s.prompt != nil {
			g.Prompt = s.prompt
		}

		err := g.ActivateLVM(ctx)
		if err != nil {
			return fmt.Errorf("activate lvm: %w", err)
		}

		return task(ctx, &env{
			Guest:       g,
			Session:     record,
			Dir:         s.dir,
			DiskDevice:  p.DiskDevice(),
			OnlyOwnDisk: onlyOwnDisk,
		})
	})
}

// publish writes the session's registry record and takes its liveness lock.
// The returned function removes both. Unregistered sessions get no-ops.
func (s *session) publish() (datadir.Session, func(), error) {
	if !s.register {
		return datadir.Session{}, func() {}, nil
	}

	record := datadir.Session{
		ID:       s.id,
		PID:      os.Getpid(),
		Started:  time.Now(),
		Provider: s.app.opts.provider,
	}
	if s.target != nil {
		record.Target = s.target.String()
	}

	release, err := s.dir.LockSession(s.id)
	if err != nil {
		return datadir.Session{}, nil, fmt.Errorf("register session: %w", err)
	}

	if err := s.dir.SaveSession(record); err != nil {
		release()
		return datadir.Session{}, nil, fmt.Errorf("register session: %w", err)
	}

	return record, release, nil
}

// CheckDevice rejects a device name that belongs to the other provider's
// naming scheme: QEMU calls the attached disk vdb and its partitions vdb1,
// the container runtime calls them loop0 and loop0p1. Left alone, the
// mismatch surfaces as a bare "device does not exist" from inside the guest,
// and with LUKS only after a passphrase has already been typed.
//
// Only the clearly wrong case is refused. Any other name may still be a real
// device in the guest, so it is passed through untouched.
func (e *env) CheckDevice(name string) error {
	if name == "" || e.DiskDevice == "" || strings.HasPrefix(name, "mapper/") {
		return nil
	}

	if strings.HasPrefix(name, e.DiskDevice) {
		return nil
	}

	otherScheme := (strings.HasPrefix(e.DiskDevice, "loop") && strings.HasPrefix(name, "vd")) ||
		(strings.HasPrefix(e.DiskDevice, "vd") && strings.HasPrefix(name, "loop"))

	if !otherScheme {
		return nil
	}

	return usagef("%q is not what this provider calls the attached disk, it is %s here — run `lmnt ls` to see the device names", name, e.DiskDevice)
}

// UpdateSession rewrites the registry record with the share details. It is
// a no-op for unregistered sessions.
func (e *env) UpdateSession(update func(*datadir.Session)) {
	if e.Session.ID == "" {
		return
	}

	update(&e.Session)

	if err := e.Dir.SaveSession(e.Session); err != nil {
		e.Guest.Logger().Warn("Failed to update the session record", "error", err)
	}
}
