// Package target parses the first positional argument of lmnt's commands:
// what the guest gets access to. Parsing is purely syntactic plus a file
// check for images; root checks and device inspection are the caller's job.
package target

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Kind is the type of a Target.
type Kind int

const (
	// Image is a disk image file on the host, attached as a whole disk.
	Image Kind = iota + 1
	// Device is a physical block device on the host.
	Device
	// USB is a USB device identified by vendor and product id.
	USB
)

// String returns the prefix used on the command line.
func (k Kind) String() string {
	switch k {
	case Image:
		return "img"
	case Device:
		return "dev"
	case USB:
		return "usb"
	default:
		return "unknown"
	}
}

// Target is a parsed passthrough argument.
type Target struct {
	Kind Kind
	// Path is set for Image and Device: absolute and cleaned.
	Path string
	// VendorID and ProductID are set for USB.
	VendorID  uint16
	ProductID uint16
}

// String renders the target in the syntax Parse accepts.
func (t Target) String() string {
	switch t.Kind {
	case USB:
		return fmt.Sprintf("usb:%04x,%04x", t.VendorID, t.ProductID)
	case Image, Device:
		return t.Kind.String() + ":" + t.Path
	default:
		return ""
	}
}

// ErrSyntax is wrapped by every error caused by a malformed argument.
var ErrSyntax = errors.New("bad target syntax")

// Parse accepts "img:<path>", "dev:<path>" or "usb:<vendor>,<product>" (hex
// ids, an optional 0x prefix). Only the first colon separates the kind from
// the value.
func Parse(s string) (Target, error) {
	kind, value, ok := strings.Cut(s, ":")
	if !ok || value == "" {
		return Target{}, fmt.Errorf("%w: %q, want img:<path>, dev:<path> or usb:<vendor>,<product>", ErrSyntax, s)
	}

	switch strings.ToLower(kind) {
	case "img":
		return parseImage(value)
	case "dev":
		return parseDevice(value)
	case "usb":
		return parseUSB(value)
	default:
		return Target{}, fmt.Errorf("%w: unknown kind %q in %q (want img, dev or usb)", ErrSyntax, kind, s)
	}
}

func parseImage(value string) (Target, error) {
	path, err := filepath.Abs(value)
	if err != nil {
		return Target{}, fmt.Errorf("image path %q: %w", value, err)
	}

	st, err := os.Stat(path)
	if err != nil {
		return Target{}, fmt.Errorf("image %s: %w", path, err)
	}

	if !st.Mode().IsRegular() {
		if st.Mode()&os.ModeDevice != 0 {
			return Target{}, fmt.Errorf("image %s is a device, use dev:%s", path, value)
		}

		return Target{}, fmt.Errorf("image %s is not a regular file", path)
	}

	return Target{Kind: Image, Path: path}, nil
}

func parseDevice(value string) (Target, error) {
	path := filepath.Clean(value)
	if !filepath.IsAbs(path) {
		return Target{}, fmt.Errorf("device path %q must be absolute", value)
	}

	return Target{Kind: Device, Path: path}, nil
}

func parseUSB(value string) (Target, error) {
	vendor, product, ok := strings.Cut(value, ",")
	if !ok {
		return Target{}, fmt.Errorf("%w: usb wants <vendor>,<product>, got %q", ErrSyntax, value)
	}

	vid, err := parseHexID(vendor)
	if err != nil {
		return Target{}, fmt.Errorf("%w: usb vendor id: %w", ErrSyntax, err)
	}

	pid, err := parseHexID(product)
	if err != nil {
		return Target{}, fmt.Errorf("%w: usb product id: %w", ErrSyntax, err)
	}

	return Target{Kind: USB, VendorID: vid, ProductID: pid}, nil
}

func parseHexID(s string) (uint16, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.ToLower(s), "0x")

	if s == "" {
		return 0, errors.New("empty")
	}

	v, err := strconv.ParseUint(s, 16, 16)
	if err != nil {
		return 0, fmt.Errorf("%q is not a 16-bit hex number", s)
	}

	return uint16(v), nil
}
