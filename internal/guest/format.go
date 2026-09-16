package guest

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// FormatRequest describes what to create on a freshly attached, blank disk.
// The whole device is used: no partition table is written, so a LUKS volume
// made here is opened later with `lmnt run -l <target> <device>`.
type FormatRequest struct {
	// Device is the guest device name without "/dev/", e.g. "vdb" or "loop0".
	Device string
	// FSType is the file system to create, one of FSTypes.
	FSType string
	// Label is the volume label, empty for none.
	Label string
	// LUKS wraps the device in a LUKS2 container and puts the file system
	// inside it.
	LUKS bool
}

// mkfsSpec is how one file system is created: the tool, the flags that keep
// it quiet and let it overwrite whatever was there, and the option that sets
// the label.
type mkfsSpec struct {
	tool      string
	args      []string
	labelFlag string
}

// mkfsTools covers the file systems the guest image can create. It ships
// e2fsprogs, xfsprogs, btrfs-progs, f2fs-tools and dosfstools, anything else
// needs `apk add` in a `lmnt shell` session.
var mkfsTools = map[string]mkfsSpec{
	"ext2":  {tool: "mkfs.ext2", args: []string{"-q", "-F"}, labelFlag: "-L"},
	"ext3":  {tool: "mkfs.ext3", args: []string{"-q", "-F"}, labelFlag: "-L"},
	"ext4":  {tool: "mkfs.ext4", args: []string{"-q", "-F"}, labelFlag: "-L"},
	"xfs":   {tool: "mkfs.xfs", args: []string{"-q", "-f"}, labelFlag: "-L"},
	"btrfs": {tool: "mkfs.btrfs", args: []string{"-q", "-f"}, labelFlag: "-L"},
	"f2fs":  {tool: "mkfs.f2fs", args: []string{"-q", "-f"}, labelFlag: "-l"},
	"vfat":  {tool: "mkfs.vfat", labelFlag: "-n"},
}

// FSTypes lists the file systems Format can create, sorted.
func FSTypes() []string {
	return slices.Sorted(maps.Keys(mkfsTools))
}

// Format creates a file system on req.Device, optionally inside a new LUKS2
// container. Everything on the device is lost. The passphrase for a LUKS
// volume comes from g.Prompt and is asked for once.
func (g *Guest) Format(ctx context.Context, req FormatRequest) error {
	dev, err := devicePath(req.Device)
	if err != nil {
		return fmt.Errorf("format: %w", err)
	}

	if err := checkFSType(req.FSType); err != nil {
		return fmt.Errorf("format: %w", err)
	}

	spec, ok := mkfsTools[req.FSType]
	if !ok {
		return fmt.Errorf("format: cannot create a %q file system (supported: %s)", req.FSType, strings.Join(FSTypes(), ", "))
	}

	if err := checkLabel(req.Label); err != nil {
		return fmt.Errorf("format: %w", err)
	}

	if req.LUKS {
		closeLUKS, err := g.encrypt(ctx, req.Device, dev)
		if err != nil {
			return err
		}
		defer closeLUKS()

		dev = "/dev/mapper/" + LUKSMapping
	}

	if err := g.mkfs(ctx, spec, dev, req.Label); err != nil {
		return err
	}

	if _, err := g.Run(ctx, "sync"); err != nil {
		return fmt.Errorf("format: sync: %w", err)
	}

	return nil
}

// encrypt puts a LUKS2 container on dev and opens it as LUKSMapping. The
// returned function closes the mapping again, so the file system created
// inside it is flushed before the guest goes away.
func (g *Guest) encrypt(ctx context.Context, device, dev string) (func(), error) {
	if g.Prompt == nil {
		return nil, fmt.Errorf("format %s: %w", dev, ErrNoPrompt)
	}

	secret, err := g.Prompt(fmt.Sprintf("New passphrase for %s: ", dev))
	if err != nil {
		return nil, fmt.Errorf("format %s: %w", dev, err)
	}
	defer scrub(secret)

	if err := g.formatLUKS(ctx, dev, secret); err != nil {
		return nil, err
	}

	// The same secret unlocks what was just created, so no second prompt.
	unlock := func(string) ([]byte, error) { return bytes.Clone(secret), nil }

	if err := g.OpenLUKS(ctx, device, LUKSMapping, unlock); err != nil {
		return nil, err
	}

	return func() {
		if err := g.CloseLUKS(ctx, LUKSMapping); err != nil {
			g.logger.Warn("Could not close the LUKS mapping, the guest is torn down anyway", "error", err)
		}
	}, nil
}

func (g *Guest) mkfs(ctx context.Context, spec mkfsSpec, dev, label string) error {
	args := append([]string{spec.tool}, spec.args...)
	if label != "" {
		args = append(args, spec.labelFlag, label)
	}

	args = append(args, dev)

	g.logger.Info("Creating a file system", "device", dev, "tool", spec.tool, "label", orNone(label))

	ctx, cancel := context.WithTimeout(ctx, g.FormatTimeout)
	defer cancel()

	if _, err := g.ex.Run(ctx, QuoteAll(args...)); err != nil {
		return fmt.Errorf("create file system on %s: %w", dev, err)
	}

	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}

	return s
}
