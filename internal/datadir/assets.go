package datadir

import (
	"compress/bzip2"
	"io"
	"runtime"
	"strconv"
)

const (
	// AlpineVersion is the Alpine Linux release the guest image is built from.
	AlpineVersion = "3.24.1"

	// alpineBranch is the release branch directory on the Alpine mirror.
	alpineBranch = "v3.24"

	// VMImageRevision is bumped whenever the guest image contents change
	// (package set, install steps), forcing a rebuild of existing images.
	VMImageRevision = 1

	alpineMirror = "https://dl-cdn.alpinelinux.org/alpine/"
)

// asset is a downloadable file with a pinned digest.
type asset struct {
	// Name is the file name inside the data directory.
	Name string
	URL  string
	// SHA256 is the hex digest of the file as stored (after Decode).
	SHA256 string
	// Decode optionally transforms the HTTP body before it is stored and
	// hashed, e.g. to decompress it.
	Decode func(io.Reader) io.Reader
}

// AlpineArch is Alpine's name for the CPU architecture lmnt runs on.
func AlpineArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}

	return "x86_64"
}

var alpineISODigests = map[string]string{
	"aarch64": "c81699152db11d2a6dbb7d75348d632fcf5811eff414d7e71876a8bb6d48bc02",
	"x86_64":  "e73a6241bd5f3c5c2d4d38c02cc52c378c0415a7c888bd292066bf36e0f41a39",
}

// alpineISO describes the "virt" flavour installer ISO for this machine.
func alpineISO() asset {
	arch := AlpineArch()
	file := "alpine-virt-" + AlpineVersion + "-" + arch + ".iso"

	return asset{
		Name:   file,
		URL:    alpineMirror + alpineBranch + "/releases/" + arch + "/" + file,
		SHA256: alpineISODigests[arch],
	}
}

// aarch64Firmware is the EDK2 UEFI firmware shipped in the QEMU source tree,
// compressed with bzip2. The digest is of the decompressed file.
var aarch64Firmware = asset{
	Name:   "edk2-aarch64-code.fd",
	URL:    "https://github.com/qemu/qemu/raw/92ec7805190313c9e628f8fc4eb4f932c15247bd/pc-bios/edk2-aarch64-code.fd.bz2",
	SHA256: "47765fe344818cbc464b1c14ae658fb4b854f5c2ceffa982411731eb4865594d",
	Decode: bzip2.NewReader,
}

// vmImageName is the file name of the built guest image, e.g.
// "alpine-3.24.1-aarch64-lmnt1.qcow2".
func vmImageName() string {
	return "alpine-" + AlpineVersion + "-" + AlpineArch() + "-lmnt" + strconv.Itoa(VMImageRevision) + ".qcow2"
}
