package guest

import (
	"context"
	"fmt"
)

// Device-mapper names used for LUKS mappings.
const (
	// LUKSMapping is the mapping of the volume that gets mounted.
	LUKSMapping = "cryptmnt"
	// LUKSContainerMapping is the mapping of a container opened first so
	// that the volumes inside it (typically LVM) become visible.
	LUKSContainerMapping = "cryptcontainer"
)

// MountRequest says what to mount at MountPoint.
type MountRequest struct {
	// Device is the guest device name without "/dev/", e.g. "vdb1" or
	// "mapper/vg-root".
	Device string
	// FSType forces a file system type, empty lets the kernel detect it.
	FSType string
	// Options is passed to mount -o verbatim.
	Options string
	// LUKS unlocks Device with cryptsetup and mounts the mapping instead.
	LUKS bool
	// LUKSContainer, when set, is a device to unlock first and scan for
	// LVM volumes before Device itself is looked at.
	LUKSContainer string
	// ReadOnly mounts with -o ro and opens LUKS mappings read-only, so the
	// guest never writes to the disk. The provider should attach the disk
	// read-only as well, this is the file-system half of that promise.
	ReadOnly bool
}

// OpenLUKSContainer unlocks device as LUKSContainerMapping and activates the
// volume groups found inside it. Passphrases come from g.Prompt.
func (g *Guest) OpenLUKSContainer(ctx context.Context, device string) error {
	return g.openLUKSContainer(ctx, device, false)
}

func (g *Guest) openLUKSContainer(ctx context.Context, device string, readOnly bool) error {
	if err := g.openLUKS(ctx, device, LUKSContainerMapping, nil, readOnly); err != nil {
		return fmt.Errorf("open luks container: %w", err)
	}

	if err := g.ActivateLVM(ctx); err != nil {
		return fmt.Errorf("open luks container: %w", err)
	}

	return nil
}

// Mount makes req's device available at MountPoint.
func (g *Guest) Mount(ctx context.Context, req MountRequest) error {
	if req.Device == "" {
		return fmt.Errorf("mount: no device given")
	}

	dev, err := devicePath(req.Device)
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}

	if err := checkFSType(req.FSType); err != nil {
		return fmt.Errorf("mount: %w", err)
	}

	if err := checkMountOptions(req.Options); err != nil {
		return fmt.Errorf("mount: %w", err)
	}

	if req.LUKSContainer != "" {
		if err := g.openLUKSContainer(ctx, req.LUKSContainer, req.ReadOnly); err != nil {
			return fmt.Errorf("mount: %w", err)
		}
	}

	if req.LUKS {
		if err := g.openLUKS(ctx, req.Device, LUKSMapping, nil, req.ReadOnly); err != nil {
			return fmt.Errorf("mount: %w", err)
		}

		dev = "/dev/mapper/" + LUKSMapping
	}

	args := []string{"mount"}
	if req.FSType != "" {
		args = append(args, "-t", req.FSType)
	}

	// "ro" goes last: with mount -o the later of two conflicting options
	// wins, so a user-supplied "rw" cannot undo the read-only request.
	options := req.Options
	if req.ReadOnly {
		options = appendOption(options, "ro")
	}

	if options != "" {
		args = append(args, "-o", options)
	}

	args = append(args, dev, MountPoint)

	g.logger.Info("Mounting", "device", dev, "fstype", orAuto(req.FSType), "options", orDefault(options))

	if _, err := g.Run(ctx, QuoteAll(args...)); err != nil {
		return fmt.Errorf("mount %s: %w", dev, err)
	}

	return nil
}

// appendOption adds opt to a comma-separated mount option list.
func appendOption(list, opt string) string {
	if list == "" {
		return opt
	}

	return list + "," + opt
}

func orAuto(s string) string {
	if s == "" {
		return "auto"
	}

	return s
}

func orDefault(s string) string {
	if s == "" {
		return "defaults"
	}

	return s
}
