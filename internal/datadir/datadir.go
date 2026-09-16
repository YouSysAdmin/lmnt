// Package datadir manages lmnt's per-user data directory (~/.lmnt by
// default). It holds the downloaded Alpine installer ISO, the EFI firmware
// needed on Apple silicon, the built virtual machine image, and the registry
// of running sessions.
//
// Downloads are verified against pinned SHA-256 digests and are written to a
// temporary ".part" file first, so a crash or a checksum mismatch never
// leaves a half-written asset behind.
package datadir

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Dir is an opened data directory. Create one with Open.
type Dir struct {
	logger *slog.Logger
	root   string
}

// DefaultPath returns where lmnt keeps its data unless told otherwise:
// "~/.lmnt". When the home directory cannot be determined it falls back to
// "lmnt-data" in the current directory.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "lmnt-data"
	}

	return filepath.Join(home, ".lmnt")
}

// Open makes sure the directory at path exists (creating it with mode 0700)
// and returns a handle to it.
func Open(logger *slog.Logger, path string) (*Dir, error) {
	if path == "" {
		return nil, errors.New("data directory path is empty")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory %q: %w", path, err)
	}

	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	return &Dir{logger: logger, root: abs}, nil
}

// Path is the absolute path of the directory.
func (d *Dir) Path() string { return d.root }

// join returns the absolute path of a name inside the directory. It refuses
// anything that would escape the directory.
func (d *Dir) join(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty file name")
	}

	full := filepath.Join(d.root, name)

	rel, err := filepath.Rel(d.root, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside the data directory", name)
	}

	return full, nil
}

// VMImagePath is where the built Alpine virtual machine image lives. The
// name encodes the Alpine release, the CPU architecture and lmnt's image
// revision, so that changing any of them leads to a rebuild rather than to
// running a stale image.
func (d *Dir) VMImagePath() string {
	return filepath.Join(d.root, vmImageName())
}

// VMImageExists reports whether the virtual machine image has been built.
func (d *Dir) VMImageExists() (bool, error) {
	info, err := os.Stat(d.VMImagePath())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("stat vm image: %w", err)
	case !info.Mode().IsRegular():
		return false, fmt.Errorf("%s exists but is not a regular file", d.VMImagePath())
	default:
		return true, nil
	}
}

// EnsureAlpineISO returns the path of the Alpine installer ISO for this
// machine's architecture, downloading and verifying it when needed. An ISO
// that is already present is re-verified against the pinned digest.
func (d *Dir) EnsureAlpineISO(ctx context.Context) (string, error) {
	return d.ensure(ctx, alpineISO())
}

// EnsureFirmware returns the path of the EDK2 UEFI firmware QEMU needs to
// boot an aarch64 guest, downloading it when needed. On architectures that
// boot without extra firmware it returns "" and nil.
func (d *Dir) EnsureFirmware(ctx context.Context) (string, error) {
	if runtime.GOARCH != "arm64" {
		return "", nil
	}

	return d.ensure(ctx, aarch64Firmware)
}

// Remove deletes a single file inside the directory. A missing file is not
// an error.
func (d *Dir) Remove(path string) error {
	name := path
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(d.root, path)
		if err != nil {
			return fmt.Errorf("%q is outside the data directory", path)
		}

		name = rel
	}

	full, err := d.join(name)
	if err != nil {
		return err
	}

	err = os.Remove(full)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", full, err)
	}

	return nil
}

// RemoveAll deletes the directory and everything in it.
func (d *Dir) RemoveAll() error {
	if err := os.RemoveAll(d.root); err != nil {
		return fmt.Errorf("remove data directory: %w", err)
	}

	return nil
}
