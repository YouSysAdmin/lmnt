package qemu

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/yousysadmin/lmnt/internal/netx"
)

// find returns the value following the first occurrence of "-flag".
func find(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}

	return args[i+1]
}

func TestCommandLineDarwinArm64(t *testing.T) {
	cfg := Config{
		SystemDisk: Disk{Path: "/data/system.qcow2", Format: "qcow2", Snapshot: true},
		ExtraDisks: []Disk{{Path: "/data/disk.img", Format: "raw"}},
		Firmware:   "/data/edk2.fd",
		MemoryMiB:  1024,
		CPUs:       4,
	}

	bin, args, err := commandLine(cfg, "arm64", 22022)
	if err != nil {
		t.Fatal(err)
	}
	if bin != "qemu-system-aarch64" {
		t.Errorf("binary = %s", bin)
	}

	joined := strings.Join(args, " ")
	wants := []string{
		"-machine type=virt",
		"-accel hvf",
		"-cpu host",
		"-m 1024",
		"-smp 4",
		"-serial stdio",
		"-bios /data/edk2.fd",
		"-display none",
		"-netdev user,id=net0,restrict=on,hostfwd=tcp:127.0.0.1:22022-:22",
		"-device virtio-net-pci,netdev=net0",
		"snapshot=on",
		"bootindex=0",
	}
	for _, w := range wants {
		if !strings.Contains(joined, w) {
			t.Errorf("args missing %q\ngot: %s", w, joined)
		}
	}

	// The extra disk is disk1 and must not be bootable.
	if v := find(args, "-drive"); !strings.Contains(strings.Join(args, " "), "id=disk1") {
		t.Errorf("no disk1 drive, first drive was %q", v)
	}
}

func TestCommandLineAmd64DeviceAndUSB(t *testing.T) {
	cfg := Config{
		SystemDisk: Disk{Path: "/sys.qcow2", Format: "qcow2"},
		ExtraDisks: []Disk{{Path: "/dev/disk4", Format: "raw", SectorSize: 4096, HostDevice: true}},
		USB:        []USBDevice{{VendorID: 0x1234, ProductID: 0x5678}},
		MemoryMiB:  512, CPUs: 1,
	}

	bin, args, err := commandLine(cfg, "amd64", 2222)
	if err != nil {
		t.Fatal(err)
	}
	if bin != "qemu-system-x86_64" {
		t.Errorf("binary = %s", bin)
	}

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-machine") {
		t.Errorf("amd64 must not get the virt machine type: %s", joined)
	}
	for _, w := range []string{
		"-accel hvf",
		"-cpu host",
		"logical_block_size=4096,physical_block_size=4096",
		"nec-usb-xhci",
		"usb-host,vendorid=0x1234,productid=0x5678",
	} {
		if !strings.Contains(joined, w) {
			t.Errorf("args missing %q\ngot: %s", w, joined)
		}
	}
}

func TestCommandLineReadOnlyDisk(t *testing.T) {
	cfg := Config{
		SystemDisk: Disk{Path: "/sys.qcow2", Format: "qcow2", Snapshot: true},
		ExtraDisks: []Disk{{Path: "/disk.img", Format: "raw", ReadOnly: true}},
		MemoryMiB:  512, CPUs: 1,
	}

	_, args, err := commandLine(cfg, "amd64", 2222)
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "file=/disk.img,format=raw,if=none,id=disk1,readonly=on") {
		t.Errorf("the user's disk is not attached read-only: %s", joined)
	}

	if strings.Contains(joined, "id=disk0,readonly=on") || strings.Contains(joined, "id=disk0,snapshot=on,readonly=on") {
		t.Errorf("the system disk must stay writable (snapshot): %s", joined)
	}
}

func TestCommandLineExtraForwardsAndOpenNetwork(t *testing.T) {
	cfg := Config{
		SystemDisk:  Disk{Path: "/sys.qcow2", Format: "qcow2"},
		Firmware:    "/fw.fd",
		OpenNetwork: true,
		Forwards:    []netx.Forward{{Host: netip.MustParseAddrPort("0.0.0.0:9000"), GuestPort: 445}},
		MemoryMiB:   512, CPUs: 1,
	}

	_, args, err := commandLine(cfg, "arm64", 2222)
	if err != nil {
		t.Fatal(err)
	}

	netdev := find(args, "-netdev")
	if strings.Contains(netdev, "restrict=on") {
		t.Error("OpenNetwork should drop restrict=on")
	}
	if !strings.Contains(netdev, "hostfwd=tcp:0.0.0.0:9000-:445") {
		t.Errorf("missing extra forward: %s", netdev)
	}
}

func TestCommandLineArm64NeedsFirmware(t *testing.T) {
	cfg := Config{SystemDisk: Disk{Path: "/sys.qcow2", Format: "qcow2"}, MemoryMiB: 512, CPUs: 1}
	if _, _, err := commandLine(cfg, "arm64", 22); err == nil {
		t.Error("arm64 without firmware should error")
	}
	if _, _, err := commandLine(cfg, "386", 22); err == nil {
		t.Error("unknown architecture accepted")
	}
}

func TestCommandLineInstallMode(t *testing.T) {
	cfg := Config{
		SystemDisk: Disk{Path: "/out.qcow2", Format: "qcow2"},
		InstallISO: "/alpine.iso",
		Firmware:   "/fw.fd",
		MemoryMiB:  512, CPUs: 1,
	}

	_, args, err := commandLine(cfg, "arm64", 22)
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-cdrom /alpine.iso") || !strings.Contains(joined, "-boot d") {
		t.Errorf("install mode missing cdrom/boot: %s", joined)
	}
	if strings.Contains(joined, "bootindex=0") {
		t.Error("install mode must not set a disk bootindex")
	}
}

func TestEscapeProperty(t *testing.T) {
	cfg := Config{
		SystemDisk: Disk{Path: "/weird,name.qcow2", Format: "qcow2"},
		Firmware:   "/fw.fd",
		MemoryMiB:  512, CPUs: 1,
	}

	_, args, err := commandLine(cfg, "arm64", 22)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(strings.Join(args, " "), "file=/weird,,name.qcow2") {
		t.Errorf("comma in path not doubled: %v", args)
	}
}
