package qemu

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yousysadmin/lmnt/internal/netx"
)

// property is one key=value element of a comma-separated QEMU option. An
// empty Value emits the bare key ("hvf", "restrict=on").
type property struct {
	Key   string
	Value string
}

// argv accumulates a QEMU command line.
type argv struct {
	list []string
}

// value appends "-name value".
func (a *argv) value(name, value string) {
	a.list = append(a.list, "-"+name, value)
}

// props appends "-name k=v,k=v", escaping commas in values the way QEMU's
// option parser expects (a literal comma is written as two).
func (a *argv) props(name string, props ...property) {
	parts := make([]string, 0, len(props))
	for _, p := range props {
		if p.Value == "" {
			parts = append(parts, escapeProperty(p.Key))
			continue
		}

		parts = append(parts, escapeProperty(p.Key)+"="+escapeProperty(p.Value))
	}

	a.list = append(a.list, "-"+name, strings.Join(parts, ","))
}

func escapeProperty(s string) string {
	return strings.ReplaceAll(s, ",", ",,")
}

// commandLine renders cfg into the QEMU binary name and its arguments for a
// macOS host of the given architecture (runtime.GOARCH: "arm64" or "amd64",
// a parameter so the builder can be tested for both on one machine).
// sshPort is the loopback port forwarded to the guest's sshd.
func commandLine(cfg Config, goarch string, sshPort uint16) (binary string, args []string, err error) {
	var a argv

	switch goarch {
	case "amd64":
		binary = "qemu-system-x86_64"
	case "arm64":
		binary = "qemu-system-aarch64"
		a.props("machine", property{Key: "type", Value: "virt"})
		if cfg.Firmware == "" {
			return "", nil, errors.New("an arm64 virtual machine needs UEFI firmware (Config.Firmware)")
		}
	default:
		return "", nil, fmt.Errorf("unsupported host architecture %s", goarch)
	}

	a.props("accel", property{Key: "hvf"})
	a.value("cpu", "host")

	a.value("m", strconv.Itoa(cfg.MemoryMiB))
	a.value("smp", strconv.Itoa(cfg.CPUs))
	a.value("serial", "stdio")

	if cfg.Firmware != "" {
		a.value("bios", filepath.Clean(cfg.Firmware))
	}

	if !cfg.Debug {
		a.value("display", "none")
	}

	installing := cfg.InstallISO != ""
	if installing {
		a.value("cdrom", filepath.Clean(cfg.InstallISO))
		a.value("boot", "d")
	}

	if err := addNetworking(&a, cfg, sshPort); err != nil {
		return "", nil, err
	}

	disks := append([]Disk{cfg.SystemDisk}, cfg.ExtraDisks...)
	for i, d := range disks {
		bootable := i == 0 && !installing
		addDisk(&a, i, d, bootable)
	}

	if len(cfg.USB) > 0 {
		a.props("device", property{Key: "nec-usb-xhci"}, property{Key: "id", Value: "xhci"})
		for _, u := range cfg.USB {
			a.props("device",
				property{Key: "usb-host"},
				property{Key: "vendorid", Value: fmt.Sprintf("0x%04x", u.VendorID)},
				property{Key: "productid", Value: fmt.Sprintf("0x%04x", u.ProductID)},
			)
		}
	}

	return binary, a.list, nil
}

// addNetworking emits the user-mode NIC with its port forwards. User-mode
// networking is what the host talks SSH over, unless OpenNetwork is set the
// guest cannot reach anything else.
func addNetworking(a *argv, cfg Config, sshPort uint16) error {
	user := []property{{Key: "user"}, {Key: "id", Value: "net0"}}
	if !cfg.OpenNetwork {
		user = append(user, property{Key: "restrict", Value: "on"})
	}

	forwards := append([]netx.Forward{{
		Host:      netip.AddrPortFrom(netx.Loopback, sshPort),
		GuestPort: 22,
	}}, cfg.Forwards...)

	for _, f := range forwards {
		if !f.Host.Addr().Is4() {
			return fmt.Errorf("port forward %s: qemu user networking forwards ipv4 addresses only", f)
		}

		user = append(user, property{
			Key:   "hostfwd",
			Value: fmt.Sprintf("tcp:%s:%d-:%d", f.Host.Addr(), f.Host.Port(), f.GuestPort),
		})
	}

	a.props("netdev", user...)
	a.props("device", property{Key: "virtio-net-pci"}, property{Key: "netdev", Value: "net0"})

	return nil
}

// addDisk emits a virtio-blk drive. Disks appear in the guest in emission
// order: the first is vda, the next vdb, and so on.
func addDisk(a *argv, index int, d Disk, bootable bool) {
	id := "disk" + strconv.Itoa(index)

	drive := []property{
		{Key: "file", Value: filepath.Clean(d.Path)},
		{Key: "format", Value: d.Format},
		{Key: "if", Value: "none"},
		{Key: "id", Value: id},
	}
	if d.Snapshot {
		drive = append(drive, property{Key: "snapshot", Value: "on"})
	}
	if d.ReadOnly {
		drive = append(drive, property{Key: "readonly", Value: "on"})
	}

	device := []property{{Key: "virtio-blk-pci"}, {Key: "drive", Value: id}}
	if bootable {
		device = append(device, property{Key: "bootindex", Value: "0"})
	}
	if d.SectorSize > 0 {
		size := strconv.Itoa(d.SectorSize)
		device = append(device,
			property{Key: "logical_block_size", Value: size},
			property{Key: "physical_block_size", Value: size},
		)
	}

	a.props("drive", drive...)
	a.props("device", device...)
}
