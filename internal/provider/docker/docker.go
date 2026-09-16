// Package docker runs lmnt's Linux guest as a privileged container through
// the docker CLI (Docker Desktop, OrbStack, Docker Engine or anything else
// that speaks the same command line).
//
// The container provider can only work with disk image files: the container
// runtime's own Linux VM has no access to the host's physical disks. An image
// is bind-mounted into the container and attached to a loop device, which
// then plays the role of the user's disk (DiskDevice reports "loopN").
package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yousysadmin/lmnt/internal/cleantext"
	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/netx"
	"github.com/yousysadmin/lmnt/internal/provider"
)

//go:embed Dockerfile
var dockerfile []byte

const (
	// imageRepo is the repository part of the guest image reference.
	imageRepo = "lmnt-guest"

	// containerLabel marks every container this package starts, so that
	// leftovers can be found with `docker ps --filter label=app=lmnt`.
	containerLabel = "app=lmnt"

	// imageMountPoint is where the user's disk image is bind-mounted inside
	// the container.
	imageMountPoint = "/lmnt/disk.img"

	// stopGrace is how long `docker stop` lets PID 1 exit on its own.
	stopGrace = 10 * time.Second
)

// ImageRef is the tag of the guest image: the repository plus a short hash
// of the embedded Dockerfile. Editing the Dockerfile therefore yields a new
// tag and an automatic rebuild, while older images are left alone.
var ImageRef = sync.OnceValue(func() string {
	sum := sha256.Sum256(dockerfile)
	return imageRepo + ":" + hex.EncodeToString(sum[:6])
})

// Config describes the container to start.
type Config struct {
	// ImagePath is the disk image file on the host to attach. Empty means no
	// disk, which is enough for an interactive shell.
	ImagePath string

	// ReadOnly binds the image read-only and attaches it with losetup
	// --read-only, so the container cannot write to it.
	ReadOnly bool

	// Forwards are published as `-p` mappings.
	Forwards []netx.Forward

	// Debug additionally streams the docker CLI's stderr to the terminal.
	Debug bool
}

// BuildOptions controls EnsureImage.
type BuildOptions struct {
	// Force rebuilds the image even when it exists, without the build cache.
	Force bool
	// Debug streams the build output to the terminal.
	Debug bool
}

// Available reports whether a docker CLI can be found in PATH.
func Available() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// EnsureImage makes sure the guest image exists locally, building it from
// the embedded Dockerfile when it does not.
func EnsureImage(ctx context.Context, logger *slog.Logger, opts BuildOptions) error {
	if !Available() {
		return errors.New("docker CLI not found in PATH")
	}

	ref := ImageRef()

	if !opts.Force {
		if err := exec.CommandContext(ctx, "docker", "image", "inspect", ref).Run(); err == nil {
			return nil
		}
	}

	logger.Info("Building the guest container image (one-time)", "image", ref)

	args := []string{"build", "--tag", ref}
	if opts.Force {
		args = append(args, "--no-cache", "--pull")
	}
	args = append(args, "-")

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = bytes.NewReader(dockerfile)

	var log bytes.Buffer
	cmd.Stdout = &log
	cmd.Stderr = &log
	if opts.Debug {
		cmd.Stdout = io.MultiWriter(&log, os.Stderr)
		cmd.Stderr = cmd.Stdout
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %w: %s", err, cleantext.Truncate(cleantext.Line(log.String()), 600))
	}

	logger.Info("Guest container image ready", "image", ref)

	return nil
}

// Container is a privileged guest container. It implements
// provider.Provider.
type Container struct {
	logger *slog.Logger
	cfg    Config
	name   string

	// loopDevice is the loop device the image is attached to ("loop0"),
	// set during Start.
	loopDevice string

	// running is set once `docker run` succeeded, i.e. there is something
	// to stop.
	running bool

	// stopping is closed at the start of Stop so that the exit watcher
	// knows the exit was requested.
	stopping   chan struct{}
	stopOnce   sync.Once
	stopResult error

	exited   chan struct{}
	exitOnce sync.Once
	exitErr  error
}

var _ provider.Provider = (*Container)(nil)

// New validates cfg and prepares a container. Nothing is started until
// Start is called.
func New(logger *slog.Logger, cfg Config) (*Container, error) {
	if cfg.ImagePath != "" {
		abs, err := filepath.Abs(cfg.ImagePath)
		if err != nil {
			return nil, fmt.Errorf("resolve image path: %w", err)
		}

		if err := checkImageFile(abs); err != nil {
			return nil, err
		}

		// The bind mount is passed as a comma separated option list, and
		// docker offers no way to escape a comma in the source path.
		if strings.Contains(abs, ",") {
			return nil, fmt.Errorf("image path %q contains a comma, which docker's --mount syntax cannot express", abs)
		}

		cfg.ImagePath = abs
	}

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, fmt.Errorf("generate container name: %w", err)
	}

	return &Container{
		logger:   logger,
		cfg:      cfg,
		name:     "lmnt-" + hex.EncodeToString(suffix[:]),
		stopping: make(chan struct{}),
		exited:   make(chan struct{}),
	}, nil
}

// checkImageFile insists on a regular file, with a dedicated message for
// the likely mistake of passing a device path.
func checkImageFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("disk image: %w", err)
	}

	switch {
	case info.Mode()&os.ModeDevice != 0:
		return fmt.Errorf("%s is a device, the container provider can only attach disk image files because the container runtime cannot see host disks. Use --provider qemu for physical devices", path)
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	default:
		return nil
	}
}

// Name is the docker container name.
func (c *Container) Name() string { return c.name }

// Start builds the image if needed, starts the container and attaches the
// disk image. On failure everything created is torn down before returning.
func (c *Container) Start(ctx context.Context) (guest.Executor, error) {
	ex, err := c.start(ctx)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopGrace+stopGrace)
		defer cancel()

		if stopErr := c.Stop(cleanupCtx); stopErr != nil {
			c.logger.Debug("Cleanup after a failed start reported an error", "error", stopErr.Error())
		}

		return nil, err
	}

	return ex, nil
}

func (c *Container) start(ctx context.Context) (guest.Executor, error) {
	if !Available() {
		return nil, errors.New("docker CLI not found in PATH")
	}

	if err := EnsureImage(ctx, c.logger, BuildOptions{Debug: c.cfg.Debug}); err != nil {
		return nil, fmt.Errorf("guest image: %w", err)
	}

	c.logger.Info("Starting the guest container", "name", c.name)

	id, err := c.docker(ctx, runArgs(c.name, c.cfg, ImageRef())...)
	if err != nil {
		return nil, fmt.Errorf("docker run: %w", err)
	}

	c.running = true
	c.logger.Debug("Guest container started", "id", strings.TrimSpace(string(id)))

	go c.watch()

	if c.cfg.ImagePath != "" {
		out, err := c.docker(ctx, "exec", c.name, "sh", "-c", attachScript(c.cfg.ReadOnly))
		if err != nil {
			return nil, fmt.Errorf("attach disk image: %w", err)
		}

		c.loopDevice, err = parseLoopName(out)
		if err != nil {
			return nil, fmt.Errorf("attach disk image: %w", err)
		}

		c.logger.Info("Disk image attached", "image", c.cfg.ImagePath, "device", "/dev/"+c.loopDevice)
	}

	return &executor{container: c.name}, nil
}

// runArgs is the `docker run` command line for a container called name.
func runArgs(name string, cfg Config, image string) []string {
	args := []string{
		"run", "--detach", "--rm", "--privileged",
		// --init puts a real init in front of sleep, so `docker stop` ends the
		// container at once instead of waiting out the grace period.
		"--init",
		"--name", name,
		"--label", containerLabel,
	}

	if cfg.ImagePath != "" {
		mount := "type=bind,source=" + cfg.ImagePath + ",target=" + imageMountPoint
		if cfg.ReadOnly {
			mount += ",readonly"
		}

		args = append(args, "--mount", mount)
	}

	for _, f := range cfg.Forwards {
		args = append(args, "--publish", f.Host.Addr().String()+":"+strconv.Itoa(int(f.Host.Port()))+":"+strconv.Itoa(int(f.GuestPort)))
	}

	// OpenRC refuses to start services on a system it did not boot unless
	// the softlevel marker exists, sleep keeps PID 1 alive for docker exec.
	return append(args, image, "sh", "-c", initScript)
}

const initScript = "mkdir -p /run/openrc && : > /run/openrc/softlevel && exec sleep infinity"

// attachScript attaches the bind-mounted image to a free loop device and
// prints the device name. A container's /dev is a plain tmpfs without udev,
// so the loop node itself and the partition nodes the kernel discovers have
// to be created by hand. Loop devices are shared kernel objects across all
// containers on the runtime VM, hence "first free" rather than a fixed one.
// With readOnly the loop device refuses writes, matching the read-only bind
// mount of the image.
func attachScript(readOnly bool) string {
	flags := "--find --partscan --show"
	if readOnly {
		flags = "--read-only " + flags
	}

	return `set -e
i=0
while [ "$i" -lt 16 ]; do
	[ -e "/dev/loop$i" ] || mknod "/dev/loop$i" b 7 "$i"
	i=$((i + 1))
done
dev=$(losetup ` + flags + ` ` + imageMountPoint + `)

name=${dev##*/}
for sys in /sys/block/"$name"/"$name"p*; do
	[ -d "$sys" ] || continue
	part=${sys##*/}
	[ -e "/dev/$part" ] && continue
	IFS=: read -r major minor < "$sys/dev"
	mknod "/dev/$part" b "$major" "$minor"
done
printf '%s\n' "$name"
`
}

var loopNamePattern = regexp.MustCompile(`^loop[0-9]+$`)

// parseLoopName extracts the device name printed by attachScript.
func parseLoopName(out []byte) (string, error) {
	name := strings.TrimSpace(string(out))
	if !loopNamePattern.MatchString(name) {
		return "", fmt.Errorf("losetup printed %q instead of a loop device name", cleantext.Truncate(cleantext.Line(name), 80))
	}

	return name, nil
}

// teardownScript undoes what a session may have set up, innermost first.
// Every step is best effort: the disk may never have been mounted.
func teardownScript(loopDevice string) string {
	var sb strings.Builder

	sb.WriteString("umount /mnt 2>/dev/null\n")
	sb.WriteString("cryptsetup close cryptmnt 2>/dev/null\n")
	sb.WriteString("vgchange -an 2>/dev/null\n")
	sb.WriteString("cryptsetup close cryptcontainer 2>/dev/null\n")
	if loopNamePattern.MatchString(loopDevice) {
		sb.WriteString("losetup -d /dev/" + loopDevice + " 2>/dev/null\n")
	}
	sb.WriteString("exit 0\n")

	return sb.String()
}

// watch blocks until the container exits and records whether that was
// asked for.
func (c *Container) watch() {
	out, err := exec.Command("docker", "wait", c.name).Output()

	select {
	case <-c.stopping:
		c.finish(nil)
	default:
		status := strings.TrimSpace(string(out))
		if err != nil {
			c.finish(fmt.Errorf("guest container disappeared: %w", err))
		} else {
			c.finish(fmt.Errorf("guest container exited unexpectedly with status %s", status))
		}
	}
}

// finish closes Exited exactly once and records the reason.
func (c *Container) finish(err error) {
	c.exitOnce.Do(func() {
		c.exitErr = err
		close(c.exited)
	})
}

// docker runs a docker CLI command and returns its stdout. Stderr ends up in
// the error.
func (c *Container) docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if c.cfg.Debug {
		cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)
	}

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, cleantext.Truncate(cleantext.Line(stderr.String()), 400))
	}

	return stdout.Bytes(), nil
}

// Exited is closed once the container is gone.
func (c *Container) Exited() <-chan struct{} { return c.exited }

// Err reports why the container exited, nil when Stop asked for it.
func (c *Container) Err() error {
	select {
	case <-c.exited:
		return c.exitErr
	default:
		return nil
	}
}

// DiskDevice is the loop device the image is attached to, e.g. "loop0", or
// "" when no image was given.
func (c *Container) DiskDevice() string { return c.loopDevice }

// Stop releases the disk and stops the container. It is safe to call before
// Start, repeatedly and concurrently, later callers wait for the first and
// share its result.
func (c *Container) Stop(ctx context.Context) error {
	c.stopOnce.Do(func() {
		close(c.stopping)
		c.stopResult = c.stop(ctx)
	})

	return c.stopResult
}

func (c *Container) stop(ctx context.Context) error {
	if !c.running {
		c.finish(nil)
		return nil
	}

	c.logger.Info("Stopping the guest container", "name", c.name)

	if _, err := c.docker(ctx, "exec", c.name, "sh", "-c", teardownScript(c.loopDevice)); err != nil {
		c.logger.Debug("Guest teardown reported an error", "error", err.Error())
	}

	_, err := c.docker(ctx, "stop", "--timeout", strconv.Itoa(int(stopGrace.Seconds())), c.name)
	if err != nil && !strings.Contains(err.Error(), "No such container") {
		return err
	}

	// The exit watcher normally gets here first; this covers the case where
	// it is not running.
	select {
	case <-c.exited:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(stopGrace):
		c.finish(nil)
	}

	return nil
}
