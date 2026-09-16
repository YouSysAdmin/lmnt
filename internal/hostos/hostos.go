// Package hostos wraps the macOS specifics lmnt needs: privilege checks, raw
// block device inspection, detecting whether a device is mounted on the
// host, process-group handling for the child processes it spawns, and the
// advisory file locks the session registry uses for liveness.
package hostos

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// IsRoot reports whether the process runs with an effective UID of zero.
func IsRoot() (bool, error) {
	return os.Geteuid() == 0, nil
}

// ValidateDevicePath checks that path names a block or character device.
func ValidateDevicePath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.Mode()&os.ModeDevice == 0 {
		return fmt.Errorf("%s is not a device (mode %v)", path, info.Mode())
	}

	return nil
}

// dkiocGetBlockSize is DKIOCGETBLOCKSIZE from <sys/disk.h>: _IOR('d', 24, uint32_t).
const dkiocGetBlockSize = 0x40046418

// DeviceSectorSize returns the logical sector size of the block device at
// path, in bytes.
func DeviceSectorSize(path string) (int, error) {
	f, err := os.Open(path) //#nosec G304 -- path is a caller-supplied device node, opened read-only
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	raw, err := unix.IoctlGetInt(int(f.Fd()), dkiocGetBlockSize)
	if err != nil {
		return 0, fmt.Errorf("query sector size of %s: %w", path, err)
	}

	// The ioctl fills a uint32, IoctlGetInt hands back a wider int whose
	// upper half is untouched zero, so masking is enough.
	size := int(uint32(raw)) //#nosec G115 -- the ioctl fills a uint32, masking is exact

	if size <= 0 {
		return 0, fmt.Errorf("device %s reports a sector size of %d", path, size)
	}

	return size, nil
}

// DeviceMounted reports whether the device at path, or any partition of it,
// appears in the host's mount table. It is a safety net, not a guarantee:
// it parses the output of mount(8).
func DeviceMounted(path string) (bool, error) {
	out, err := exec.Command("mount").Output()
	if err != nil {
		return false, fmt.Errorf("list mounts: %w", err)
	}

	candidates := []string{filepath.Clean(path)}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != candidates[0] {
		candidates = append(candidates, resolved)
	}

	return mountTableMentions(out, candidates), nil
}

// mountTableMentions scans mount(8) output for sources that are one of the
// devices or a partition of one ("/dev/disk4" also matches "/dev/disk4s2").
func mountTableMentions(mountOutput []byte, devices []string) bool {
	sc := bufio.NewScanner(bytes.NewReader(mountOutput))
	for sc.Scan() {
		source, _, ok := strings.Cut(sc.Text(), " on ")
		if !ok {
			continue
		}

		for _, dev := range devices {
			if source == dev || strings.HasPrefix(source, dev) && partitionSuffix.MatchString(source[len(dev):]) {
				return true
			}
		}
	}

	return false
}

// partitionSuffix is what macOS appends to a disk's name for its slices:
// "s1" for a partition, "s1s1" for a synthesized APFS volume.
var partitionSuffix = regexp.MustCompile(`^(s[0-9]+)+$`)

// Detach places cmd in its own process group so that a Ctrl+C aimed at lmnt
// is not delivered to the child as well, lmnt shuts children down itself.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Terminate sends SIGTERM to the process group led by pid, as created by
// Detach.
func Terminate(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("terminate process group %d: %w", pid, err)
	}

	return nil
}

// ProcessAlive reports whether a process with the given PID exists.
func ProcessAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("invalid pid %d", pid)
	}

	err := syscall.Kill(pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, fmt.Errorf("probe pid %d: %w", pid, err)
	}
}

// LockFile creates path and holds an exclusive advisory lock on it until the
// returned function is called, which also removes the file. Another process
// can tell the owner is alive with FileLocked, which is immune to PID reuse.
func LockFile(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // callers pass a path inside lmnt's own data directory
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	return func() {
		_ = os.Remove(path)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// FileLocked reports whether some process holds the lock taken by LockFile.
// A missing file counts as unlocked.
func FileLocked(path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // callers pass a path inside lmnt's own data directory
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open lock file: %w", err)
	}
	defer func() { _ = f.Close() }()

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	switch {
	case err == nil:
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return true, nil
	default:
		return false, fmt.Errorf("probe lock %s: %w", path, err)
	}
}
