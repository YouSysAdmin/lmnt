package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/target"
)

const (
	// minImageSize leaves room for a LUKS2 header (16 MiB by default) and a
	// small file system on top of it.
	minImageSize = 32 << 20
	// imageSectorSize is the alignment every image size must respect.
	imageSectorSize = 512
)

// createOptions are the flags of the create command.
type createOptions struct {
	size    string
	fsType  string
	label   string
	encrypt bool
	force   bool
}

func (a *app) createCommand() *cobra.Command {
	opts := createOptions{fsType: "ext4"}

	cmd := &cobra.Command{
		Use:   "create <path>",
		Short: "Create a disk image file, optionally LUKS-encrypted",
		Long: fmt.Sprintf(`create makes a new disk image and puts a file system in it, so it can be
mounted with "lmnt run" right away. With --encrypt the whole image becomes a
LUKS2 container and the file system goes inside it.

The image is a sparse raw file: it is created at its full size but only takes
up disk space as it fills. Sizes take the suffixes K, M, G and T (powers of
1024), a bare number is bytes.

No partition table is written, so the image is one volume: mount it with
"lmnt run img:<path>", or "lmnt run -l img:<path> <device>" when it is
encrypted.

File systems: %s.`, strings.Join(guest.FSTypes(), ", ")),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.report(a.create(cmd.Context(), args[0], &opts))
		},
	}

	f := cmd.Flags()
	f.StringVarP(&opts.size, "size", "s", "", "size of the image, e.g. 10G (required)")
	f.BoolVarP(&opts.encrypt, "encrypt", "e", false, "wrap the image in a LUKS2 container")
	f.StringVar(&opts.fsType, "fs", opts.fsType, "file system to create: "+strings.Join(guest.FSTypes(), ", "))
	f.StringVar(&opts.label, "label", "", "file system label")
	f.BoolVarP(&opts.force, "force", "f", false, "overwrite an existing file")

	return cmd
}

func (a *app) create(ctx context.Context, path string, opts *createOptions) (err error) {
	// Everything that can be judged without touching the disk is judged
	// first, so a typo costs nothing and never leaves a file behind.
	size, err := parseSize(opts.size)
	if err != nil {
		return usagef("--size: %w", err)
	}

	if !slices.Contains(guest.FSTypes(), opts.fsType) {
		return usagef("--fs: cannot create a %q file system (supported: %s)", opts.fsType, strings.Join(guest.FSTypes(), ", "))
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return usagef("image path %q: %w", path, err)
	}

	if err := a.claimPath(abs, opts.force); err != nil {
		return err
	}

	if err := createSparseFile(abs, size); err != nil {
		return err
	}

	// A half-made image is worse than none: anything short of a finished
	// file system takes the file with it.
	defer func() {
		if err != nil {
			if rmErr := os.Remove(abs); rmErr != nil && !os.IsNotExist(rmErr) {
				a.logger.Warn("Could not remove the unfinished image", "path", abs, "error", rmErr)
			}
		}
	}()

	t := target.Target{Kind: target.Image, Path: abs}

	s := &session{app: a, target: &t, luks: opts.encrypt}
	if opts.encrypt {
		s.prompt = confirmedPrompt(guest.TerminalPrompt)
	}

	err = s.run(ctx, func(ctx context.Context, e *env) error {
		return e.Guest.Format(ctx, guest.FormatRequest{
			Device: e.DiskDevice,
			FSType: opts.fsType,
			Label:  opts.label,
			LUKS:   opts.encrypt,
		})
	})
	if err != nil {
		return err
	}

	a.printCreated(abs, size, opts)

	return nil
}

// claimPath makes sure abs is free to write, removing an existing file only
// when the user asked for it.
func (a *app) claimPath(abs string, force bool) error {
	st, err := os.Stat(abs)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("check %s: %w", abs, err)
	case !st.Mode().IsRegular():
		return usagef("%s exists and is not a regular file", abs)
	case !force:
		return usagef("%s already exists, pass --force to overwrite it", abs)
	}

	a.logger.Warn("Overwriting an existing file", "path", abs)

	if err := os.Remove(abs); err != nil {
		return fmt.Errorf("remove %s: %w", abs, err)
	}

	return nil
}

// createSparseFile makes a new file of the given size whose blocks are only
// allocated as they are written. 0600: the contents are the user's.
func createSparseFile(abs string, size int64) error {
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // the path is the user's own argument: naming the image is the point of the command
	if err != nil {
		return fmt.Errorf("create %s: %w", abs, err)
	}

	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		_ = os.Remove(abs)

		return fmt.Errorf("size %s to %s: %w", abs, formatSize(size), err)
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(abs)

		return fmt.Errorf("create %s: %w", abs, err)
	}

	return nil
}

func (a *app) printCreated(abs string, size int64, opts *createOptions) {
	kind := opts.fsType
	if opts.encrypt {
		kind = "LUKS2, " + kind
	}

	// No device name: the image is one whole volume, so run mounts the
	// attached disk itself, whatever this provider happens to call it.
	mount := "lmnt run img:" + abs
	if opts.encrypt {
		mount = "lmnt run -l img:" + abs
	}

	_, _ = fmt.Fprintf(a.stdout, "Created %s (%s, %s).\n\n  %s\n", abs, formatSize(size), kind, mount)
}

// confirmedPrompt asks for a new passphrase twice and returns it only when
// both entries match, the way any tool that creates a secret should.
func confirmedPrompt(ask guest.PasswordPrompt) guest.PasswordPrompt {
	return func(prompt string) ([]byte, error) {
		secret, err := ask(prompt)
		if err != nil {
			return nil, err
		}

		if len(secret) == 0 {
			clear(secret)
			return nil, fmt.Errorf("the passphrase is empty")
		}

		again, err := ask("Repeat passphrase: ")
		if err != nil {
			clear(secret)
			return nil, err
		}

		defer clear(again)

		if !bytes.Equal(secret, again) {
			clear(secret)
			return nil, fmt.Errorf("the passphrases do not match")
		}

		return secret, nil
	}
}

// sizeUnits are the accepted suffixes, longest first so "KiB" is matched
// before "K".
var sizeUnits = []struct {
	suffix string
	factor int64
}{
	{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
}

// parseSize turns "10G", "512MiB" or a bare byte count into bytes. Suffixes
// are powers of 1024, as in truncate(1) and qemu-img.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("a size is required, e.g. 10G")
	}

	digits, factor := s, int64(1)

	for _, u := range sizeUnits {
		if rest, ok := strings.CutSuffix(strings.ToUpper(s), strings.ToUpper(u.suffix)); ok {
			digits, factor = rest, u.factor
			break
		}
	}

	n, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size like 10G, 512M or a byte count", s)
	}

	if n <= 0 {
		return 0, fmt.Errorf("%q is not positive", s)
	}

	if n > (1<<62)/factor {
		return 0, fmt.Errorf("%q is too large", s)
	}

	size := n * factor

	if size%imageSectorSize != 0 {
		return 0, fmt.Errorf("%q is not a multiple of %d bytes", s, imageSectorSize)
	}

	if size < minImageSize {
		return 0, fmt.Errorf("%q is smaller than the %s minimum", s, formatSize(minImageSize))
	}

	return size, nil
}

// formatSize renders a byte count in the units parseSize accepts.
func formatSize(n int64) string {
	units := []struct {
		suffix string
		factor int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}}

	for _, u := range units {
		if n >= u.factor && n%u.factor == 0 {
			return strconv.FormatInt(n/u.factor, 10) + " " + u.suffix
		}
	}

	return strconv.FormatInt(n, 10) + " B"
}
