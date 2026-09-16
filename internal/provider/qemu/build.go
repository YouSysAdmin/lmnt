package qemu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yousysadmin/lmnt/internal/guest"
)

// buildTimeout bounds the whole image build. Package installation over a slow
// mirror is the long pole.
const buildTimeout = 20 * time.Minute

// GuestPackages are the Alpine packages the guest needs to mount the range of
// filesystems lmnt supports and to serve the file shares.
var GuestPackages = []string{
	"openssh", "lvm2", "device-mapper", "cryptsetup", "util-linux",
	"e2fsprogs", "xfsprogs", "btrfs-progs", "f2fs-tools", "ntfs-3g", "dosfstools",
	"samba", "vsftpd",
}

// BuildConfig describes an image build.
type BuildConfig struct {
	// ISO is the Alpine install ISO to boot.
	ISO string
	// Firmware is the UEFI firmware file, required on arm64.
	Firmware string
	// Output is the qcow2 file to create. It must not already exist.
	Output string
	// Debug shows QEMU's output during the build.
	Debug bool
}

// BuildImage installs Alpine onto a fresh qcow2 image at cfg.Output. It
// creates the image, boots the installer over the network, runs the Alpine
// setup, adds lmnt's package set, and powers off. The output file is removed
// if the build fails.
func BuildImage(ctx context.Context, logger *slog.Logger, cfg BuildConfig) (err error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	if cfg.ISO == "" {
		return errors.New("build: an install ISO is required")
	}
	if cfg.Output == "" {
		return errors.New("build: an output path is required")
	}
	if _, statErr := os.Stat(cfg.Output); statErr == nil {
		return fmt.Errorf("build: %s already exists", cfg.Output)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("build: stat output: %w", statErr)
	}

	if err := createImage(ctx, cfg.Output); err != nil {
		return err
	}

	defer func() {
		if err != nil {
			_ = os.Remove(cfg.Output)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()

	machine, err := New(logger, Config{
		SystemDisk:   Disk{Path: cfg.Output, Format: "qcow2"},
		InstallISO:   cfg.ISO,
		Firmware:     cfg.Firmware,
		OpenNetwork:  true,
		InstallSSH:   true,
		Debug:        cfg.Debug,
		SetupTimeout: 3 * time.Minute,
	})
	if err != nil {
		return err
	}

	ex, err := machine.Start(ctx)
	if err != nil {
		return fmt.Errorf("boot installer: %w", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer stopCancel()
		_ = machine.Stop(stopCtx)
	}()

	logger.Info("Installing Alpine Linux onto the image, this takes a few minutes")
	if _, err := ex.Run(ctx, installScript()); err != nil {
		return fmt.Errorf("run installer: %w", err)
	}

	logger.Info("Image build complete", "path", cfg.Output)

	return nil
}

func createImage(ctx context.Context, output string) error {
	cmd := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2", filepath.Clean(output), "1G")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return nil
}

// installScript is the sequence run over SSH inside the booted installer. It
// mirrors a manual "setup-alpine" for a sys install onto /dev/vda, then
// chroots in to add lmnt's packages and lock sshd down to key auth.
func installScript() string {
	pkgs := guest.QuoteAll(GuestPackages...)

	var b strings.Builder
	b.WriteString("set -e\n")
	// Fresh repositories, first mirror, no interactive prompt.
	b.WriteString(": > /etc/apk/repositories\n")
	b.WriteString("setup-apkrepos -c -1\n")
	// A sys install writes a partitioned, bootable system onto vda.
	b.WriteString("printf 'y\\n' | setup-disk -m sys /dev/vda\n")
	// Add lmnt's tools inside the installed system.
	b.WriteString("mount /dev/vda3 /mnt\n")
	b.WriteString("mount -t proc none /mnt/proc\n")
	b.WriteString("mount --bind /dev /mnt/dev\n")
	b.WriteString("chroot /mnt apk add " + pkgs + "\n")
	// Key-only SSH, and stop the installer's DHCP interface config from
	// leaking into the image.
	b.WriteString("printf 'PasswordAuthentication no\\n' >> /mnt/etc/ssh/sshd_config\n")
	b.WriteString(": > /mnt/etc/network/interfaces\n")
	b.WriteString("umount /mnt/dev /mnt/proc /mnt 2>/dev/null || true\n")
	b.WriteString("sync\n")
	b.WriteString("poweroff &\n")

	return b.String()
}
