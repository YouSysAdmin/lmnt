// Package qemu runs the Linux guest as a QEMU virtual machine and, from the
// same building blocks, builds the Alpine system image the machine boots.
//
// A Machine boots a headless Alpine VM, brings it up over the serial console
// far enough to start sshd, and then serves guest commands over SSH. It
// implements provider.Provider. BuildImage produces the qcow2 system image
// that Machine expects.
package qemu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/hostos"
	"github.com/yousysadmin/lmnt/internal/netx"
	"github.com/yousysadmin/lmnt/internal/provider"
	"golang.org/x/crypto/ssh"
)

// Disk is a drive attached to the VM.
type Disk struct {
	// Path is the host file or device the drive is backed by.
	Path string
	// Format is the QEMU image format: "raw" for images and devices, "qcow2"
	// for the system image.
	Format string
	// SectorSize, when non-zero, sets the emulated logical and physical
	// sector size so a device is seen with its real geometry.
	SectorSize int
	// Snapshot discards all writes on shutdown, keeping the backing file
	// untouched. The system image is attached this way in normal use.
	Snapshot bool
	// HostDevice marks a drive backed by a raw host block device, which the
	// mount watchdog guards against a concurrent host mount.
	HostDevice bool
	// ReadOnly attaches the drive read-only: QEMU opens the backing file
	// without write access and the guest sees a read-only block device, so
	// nothing the guest does can change the disk.
	ReadOnly bool
}

// USBDevice identifies a USB device to pass through by vendor and product ID.
type USBDevice struct {
	VendorID  uint16
	ProductID uint16
}

// Config describes a VM to boot.
type Config struct {
	// SystemDisk is the bootable Alpine image.
	SystemDisk Disk
	// ExtraDisks are the user's disks, the first becomes DiskDevice "vdb".
	ExtraDisks []Disk
	// USB devices to pass through.
	USB []USBDevice

	// InstallISO, when set, boots this ISO instead of the system disk. Used
	// by BuildImage.
	InstallISO string
	// Firmware is the UEFI firmware file, required on arm64.
	Firmware string

	// MemoryMiB and CPUs default to 512 and the host CPU count.
	MemoryMiB int
	CPUs      int

	// Forwards are host-to-guest TCP forwards in addition to the internal
	// SSH forward.
	Forwards []netx.Forward
	// OpenNetwork lets the guest reach the outside network. Off by default.
	OpenNetwork bool

	// BootTimeout bounds waiting for the login prompt (default 60s).
	BootTimeout time.Duration
	// SetupTimeout bounds bringing sshd up after login (default 120s).
	SetupTimeout time.Duration
	// ShutdownTimeout bounds a graceful poweroff before the process is
	// terminated (default 20s).
	ShutdownTimeout time.Duration

	// Debug shows QEMU's display and passes its stderr through.
	Debug bool
	// InstallSSH runs "apk add openssh" during provisioning, for the install
	// ISO which lacks it.
	InstallSSH bool
}

func (c *Config) applyDefaults() {
	c.MemoryMiB = cmpPositive(c.MemoryMiB, 512)
	c.CPUs = cmpPositive(c.CPUs, runtime.NumCPU())
	c.BootTimeout = cmpDuration(c.BootTimeout, 60*time.Second)
	c.SetupTimeout = cmpDuration(c.SetupTimeout, 120*time.Second)
	c.ShutdownTimeout = cmpDuration(c.ShutdownTimeout, 20*time.Second)
}

func cmpPositive(v, def int) int {
	if v > 0 {
		return v
	}

	return def
}

func cmpDuration(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}

	return def
}

// Machine is a running (or runnable) QEMU virtual machine.
type Machine struct {
	logger *slog.Logger
	cfg    Config

	cmd     *exec.Cmd
	console *serialConsole
	key     sessionKey
	hostKey ssh.PublicKey
	sshPort uint16

	stderr *bytes.Buffer

	exited   chan struct{}
	exitOnce sync.Once
	exitErr  error

	stopOnce sync.Once
	started  atomic.Bool
}

// New prepares a Machine from cfg without starting it.
func New(logger *slog.Logger, cfg Config) (*Machine, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	cfg.applyDefaults()

	if cfg.SystemDisk.Path == "" {
		return nil, errors.New("qemu: a system disk is required")
	}

	return &Machine{
		logger: logger,
		cfg:    cfg,
		exited: make(chan struct{}),
	}, nil
}

// DiskDevice returns the guest device name of the user's first extra disk.
func (m *Machine) DiskDevice() string {
	if len(m.cfg.ExtraDisks) == 0 {
		return ""
	}

	return "vdb"
}

// Exited is closed once the VM process is gone.
func (m *Machine) Exited() <-chan struct{} { return m.exited }

// Err reports why the VM exited, meaningful only once Exited is closed.
func (m *Machine) Err() error {
	select {
	case <-m.exited:
		return m.exitErr
	default:
		return nil
	}
}

func (m *Machine) finish(err error) {
	m.exitOnce.Do(func() {
		m.exitErr = err
		close(m.exited)
	})
}

// Start boots the VM and returns an Executor once it accepts SSH commands.
func (m *Machine) Start(ctx context.Context) (guest.Executor, error) {
	if !m.started.CompareAndSwap(false, true) {
		return nil, errors.New("qemu: machine already started")
	}

	if err := m.guardDevicesUnmounted(); err != nil {
		m.finish(nil)
		return nil, err
	}

	ex, err := m.boot(ctx)
	if err != nil {
		// Tear down whatever came up, ensure Exited is closed.
		_ = m.Stop(context.Background())
		m.finish(errWithStderr(err, m.stderr))
		return nil, err
	}

	return ex, nil
}

func (m *Machine) boot(ctx context.Context) (guest.Executor, error) {
	key, err := newSessionKey()
	if err != nil {
		return nil, err
	}
	m.key = key

	sshPort, err := netx.FreePort()
	if err != nil {
		return nil, fmt.Errorf("reserve ssh port: %w", err)
	}
	m.sshPort = sshPort

	binary, args, err := commandLine(m.cfg, runtime.GOARCH, sshPort)
	if err != nil {
		return nil, err
	}

	if resolved, err := exec.LookPath(binary); err == nil {
		binary = resolved
	} else {
		return nil, fmt.Errorf("qemu binary %s not found in PATH, install QEMU", binary)
	}

	m.logger.Debug("Launching QEMU", "binary", binary, "args", args)

	cmd := exec.Command(binary, args...)
	hostos.Detach(cmd)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open qemu stdin: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open qemu stdout: %w", err)
	}

	m.stderr = &bytes.Buffer{}
	if m.cfg.Debug {
		cmd.Stderr = io.MultiWriter(m.stderr, debugWriter{m.logger})
	} else {
		cmd.Stderr = m.stderr
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start qemu: %w", err)
	}
	m.cmd = cmd

	// Reap the process and close Exited when it goes.
	go func() {
		err := cmd.Wait()
		m.finish(errWithStderr(err, m.stderr))
	}()

	go m.watchDevices(ctx)

	m.console = newSerialConsole(stdinPipe, stdoutPipe)

	ex, err := m.provision(ctx)
	if err != nil {
		return nil, err
	}

	return ex, nil
}

// provision drives the guest from the login prompt to a working SSH server
// and returns an Executor for it.
func (m *Machine) provision(ctx context.Context) (guest.Executor, error) {
	bootCtx, cancel := context.WithTimeout(ctx, m.cfg.BootTimeout)
	defer cancel()

	m.logger.Info("Waiting for the guest to boot")
	if err := m.console.awaitLogin(bootCtx); err != nil {
		return nil, fmt.Errorf("wait for guest login prompt: %w", err)
	}

	setupCtx, cancel2 := context.WithTimeout(ctx, m.cfg.SetupTimeout)
	defer cancel2()

	if err := m.console.send("root\n"); err != nil {
		return nil, fmt.Errorf("log into guest: %w", err)
	}

	// Give login(1) a moment to hand control to the root shell before the
	// setup script is typed, so the first bytes are not eaten by the prompt.
	select {
	case <-time.After(time.Second):
	case <-setupCtx.Done():
		return nil, setupCtx.Err()
	}

	m.logger.Info("Setting up the guest network and ssh server")
	out, code, err := m.console.runScript(setupCtx, m.provisionScript())
	if err != nil {
		return nil, fmt.Errorf("run guest setup: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("guest setup exited with status %d: %s", code, strings.Join(out, " | "))
	}

	hostKey, err := hostKeyFromOutput(out)
	if err != nil {
		return nil, err
	}
	m.hostKey = hostKey

	cfg := sshClientConfig(m.key, hostKey)
	if _, err := dialSSH(setupCtx, m.sshPort, cfg); err != nil {
		return nil, err
	}

	m.logger.Info("Guest is ready")

	return &sshExecutor{port: m.sshPort, cfg: cfg}, nil
}

// hostKeyMarker fences the guest host key in the setup output.
const hostKeyMarker = "LMNT-HOSTKEY:"

// provisionScript brings up networking, installs the session key, starts
// sshd and prints the guest host key on a marked line. It is a single line:
// steps that must succeed are chained with && and optional ones tolerate
// failure, because the serial protocol runs one line at a time.
func (m *Machine) provisionScript() string {
	steps := []string{
		"ifconfig eth0 up",
		"ifconfig lo up",
		"(udhcpc -q -i eth0 >/dev/null 2>&1 || udhcpc -q >/dev/null 2>&1)",
	}
	if m.cfg.InstallSSH {
		steps = append(steps, "apk add openssh >/dev/null 2>&1")
	}
	steps = append(steps,
		"(ssh-keygen -A >/dev/null 2>&1 || true)",
		"mkdir -p /root/.ssh",
		"printf '%s\\n' "+guest.Quote(m.key.authorized)+" > /root/.ssh/authorized_keys",
		"chmod 700 /root/.ssh",
		"chmod 600 /root/.ssh/authorized_keys",
		"(rc-update add sshd >/dev/null 2>&1 || true)",
		"(rc-service sshd restart >/dev/null 2>&1 || /usr/sbin/sshd >/dev/null 2>&1)",
		"printf '%s%s\\n' "+guest.Quote(hostKeyMarker)+" \"$(cat /etc/ssh/ssh_host_ed25519_key.pub)\"",
	)

	return strings.Join(steps, " && ")
}

func hostKeyFromOutput(lines []string) (ssh.PublicKey, error) {
	for _, line := range lines {
		_, key, ok := strings.Cut(line, hostKeyMarker)
		if !ok {
			continue
		}

		return parseHostKey(strings.TrimSpace(key))
	}

	return nil, errors.New("guest did not report its host key")
}

// Stop shuts the VM down, gracefully if SSH is up, and returns when the
// process is gone or ctx expires.
func (m *Machine) Stop(ctx context.Context) error {
	if m.cmd == nil || m.cmd.Process == nil {
		m.finish(nil)
		return nil
	}

	var err error
	m.stopOnce.Do(func() {
		err = m.shutdown(ctx)
	})

	select {
	case <-m.exited:
	case <-ctx.Done():
		return fmt.Errorf("wait for guest to exit: %w", ctx.Err())
	}

	return err
}

func (m *Machine) shutdown(ctx context.Context) error {
	// Prefer a clean poweroff so guest filesystems are flushed.
	if m.sshPort != 0 && m.key.signer != nil && m.hostKey != nil {
		graceCtx, cancel := context.WithTimeout(ctx, m.cfg.ShutdownTimeout)
		defer cancel()

		m.logger.Info("Powering the guest off")
		if err := m.poweroff(graceCtx); err == nil {
			select {
			case <-m.exited:
				return nil
			case <-graceCtx.Done():
				m.logger.Warn("Guest did not power off in time, terminating it")
			}
		} else {
			m.logger.Warn("Could not power the guest off cleanly, terminating it", "error", err)
		}
	}

	if err := hostos.Terminate(m.cmd.Process.Pid); err != nil {
		// It may already be gone, that is fine.
		if m.cmd.ProcessState == nil {
			return fmt.Errorf("terminate guest: %w", err)
		}
	}

	return nil
}

func (m *Machine) poweroff(ctx context.Context) error {
	client, err := dialSSH(ctx, m.sshPort, sshClientConfig(m.key, m.hostKey))
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	// poweroff drops the connection as it runs, so its error says nothing.
	_ = sess.Run("poweroff")

	return nil
}

// watchDevices aborts the VM the instant a passed-through host device shows
// up mounted on the host, to avoid two systems writing the same disk.
func (m *Machine) watchDevices(ctx context.Context) {
	var devices []string
	for _, d := range append([]Disk{m.cfg.SystemDisk}, m.cfg.ExtraDisks...) {
		if d.HostDevice {
			devices = append(devices, d.Path)
		}
	}
	if len(devices) == 0 {
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.exited:
			return
		case <-ticker.C:
			for _, dev := range devices {
				mounted, err := hostos.DeviceMounted(dev)
				if err != nil {
					m.logger.Warn("Could not check whether a passed device is mounted", "device", dev, "error", err)
					continue
				}
				if mounted {
					m.logger.Error("A passed-through device was mounted on the host, killing the guest to prevent corruption", "device", dev)
					if m.cmd != nil && m.cmd.Process != nil {
						_ = m.cmd.Process.Kill()
					}

					return
				}
			}
		}
	}
}

func (m *Machine) guardDevicesUnmounted() error {
	for _, d := range append([]Disk{m.cfg.SystemDisk}, m.cfg.ExtraDisks...) {
		if !d.HostDevice {
			continue
		}

		mounted, err := hostos.DeviceMounted(d.Path)
		if err != nil {
			return fmt.Errorf("check whether %s is mounted: %w", d.Path, err)
		}
		if mounted {
			return fmt.Errorf("%s is mounted on the host, unmount it first", d.Path)
		}
	}

	return nil
}

func errWithStderr(err error, stderr *bytes.Buffer) error {
	if err == nil {
		return nil
	}

	if stderr == nil || stderr.Len() == 0 {
		return err
	}

	return fmt.Errorf("%w (qemu stderr: %s)", err, strings.TrimSpace(stderr.String()))
}

// debugWriter forwards QEMU stderr lines to the logger.
type debugWriter struct{ logger *slog.Logger }

func (w debugWriter) Write(p []byte) (int, error) {
	w.logger.Debug("qemu", "line", strings.TrimRight(string(p), "\n"))

	return len(p), nil
}

var _ provider.Provider = (*Machine)(nil)
