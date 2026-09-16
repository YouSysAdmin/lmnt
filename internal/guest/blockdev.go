package guest

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// BlockDevice is one entry of the guest's block device tree as reported by
// lsblk: a disk, a partition, or a device-mapper node (LVM logical volume,
// opened LUKS volume).
type BlockDevice struct {
	Name string `json:"name"`
	// Path is the device node, e.g. "/dev/vdb1" or "/dev/mapper/vgt-data".
	Path string `json:"path"`
	// Size is lsblk's human-readable size ("92M", "1.8T").
	Size   string `json:"size"`
	FSType string `json:"fstype"`
	Label  string `json:"label"`
	// Type is lsblk's TYPE column: disk, part, loop, lvm, crypt, …
	Type string `json:"type"`
	// Depth is the nesting level in the tree, 0 for a top-level device.
	Depth    int           `json:"-"`
	Children []BlockDevice `json:"children"`
}

// Device is the name to hand to Mount: Path without its "/dev/" prefix, so a
// partition gives "vdb1" and a logical volume "mapper/vgt-data".
func (d BlockDevice) Device() string {
	return strings.TrimPrefix(d.Path, "/dev/")
}

// IsContainer reports whether the device holds other devices rather than a
// mountable file system: an LVM physical volume or a LUKS container.
func (d BlockDevice) IsContainer() bool {
	return d.FSType == "LVM2_member" || d.FSType == "crypto_LUKS"
}

// ParseBlockDevices decodes lsblk's JSON output (lsblk -J) into a flat,
// depth-first list with Depth set. Entries whose Device name lmnt would
// refuse are dropped.
func ParseBlockDevices(raw []byte) ([]BlockDevice, error) {
	var doc struct {
		BlockDevices []BlockDevice `json:"blockdevices"`
	}

	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse lsblk output: %w", err)
	}

	var flat []BlockDevice
	flatten(doc.BlockDevices, 0, &flat)

	return flat, nil
}

func flatten(devs []BlockDevice, depth int, out *[]BlockDevice) {
	for _, d := range devs {
		children := d.Children
		d.Children = nil
		d.Depth = depth

		if ValidDeviceName(d.Device()) {
			*out = append(*out, d)
		}

		flatten(children, depth+1, out)
	}
}

// BlockDevices returns the guest's block device tree for the given devices,
// or for the whole guest minus loop, CD-ROM and floppy devices when none are
// given. It is ListBlockDevices for programs rather than people.
func (g *Guest) BlockDevices(ctx context.Context, devices ...string) ([]BlockDevice, error) {
	args := []string{"lsblk", "--json", "--output", "NAME,PATH,SIZE,FSTYPE,LABEL,TYPE"}

	if len(devices) == 0 {
		args = append(args, "--exclude", "2,7,11")
	}

	for _, d := range devices {
		p, err := devicePath(d)
		if err != nil {
			return nil, fmt.Errorf("list block devices: %w", err)
		}

		args = append(args, p)
	}

	out, err := g.Run(ctx, QuoteAll(args...))
	if err != nil {
		return nil, fmt.Errorf("list block devices: %w", err)
	}

	devs, err := ParseBlockDevices(out)
	if err != nil {
		return nil, fmt.Errorf("list block devices: %w", err)
	}

	return devs, nil
}

// ValidMountOptions reports whether opts may be passed to mount -o, the
// empty string is valid and means the defaults.
func ValidMountOptions(opts string) error {
	return checkMountOptions(opts)
}
