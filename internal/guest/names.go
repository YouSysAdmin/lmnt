package guest

import (
	"fmt"
	"regexp"
	"strings"
)

// Every name below is spliced into a shell script. Quote already makes that
// safe, the patterns here are the second, independent line of defence and
// also keep obviously wrong input (spaces, path traversal) out of the guest.
var (
	deviceNamePattern   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	fsTypePattern       = regexp.MustCompile(`^[a-z0-9]+$`)
	mountOptionsPattern = regexp.MustCompile(`^[A-Za-z0-9_@/.:+=,-]+$`)
	userNamePattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	serviceNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	mappingNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	labelPattern        = regexp.MustCompile(`^[A-Za-z0-9._-][A-Za-z0-9 ._-]{0,254}$`)
)

// ValidDeviceName reports whether name may follow "/dev/" in the guest: a
// plain block device such as "vdb1" or "loop0p2", or a device-mapper node
// written as "mapper/vg-lv".
func ValidDeviceName(name string) bool {
	name = strings.TrimPrefix(name, "mapper/")

	return deviceNamePattern.MatchString(name)
}

// devicePath validates name and returns its absolute path in the guest.
func devicePath(name string) (string, error) {
	if !ValidDeviceName(name) {
		return "", fmt.Errorf("%q is not a valid device name (expect e.g. vdb1 or mapper/vg-lv)", name)
	}

	return "/dev/" + name, nil
}

func checkFSType(fs string) error {
	if fs != "" && !fsTypePattern.MatchString(fs) {
		return fmt.Errorf("%q is not a valid file system type", fs)
	}

	return nil
}

func checkMountOptions(opts string) error {
	if opts != "" && !mountOptionsPattern.MatchString(opts) {
		return fmt.Errorf("mount options %q contain characters lmnt does not pass through", opts)
	}

	return nil
}

func checkUserName(name string) error {
	if !userNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a valid unix user name", name)
	}

	return nil
}

func checkServiceName(name string) error {
	if !serviceNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a valid service name", name)
	}

	return nil
}

// checkLabel accepts an empty label or one mkfs can carry. The per file
// system length limits (11 for vfat, 12 for xfs, 16 for ext4) are left to
// mkfs, which reports them itself.
func checkLabel(label string) error {
	if label != "" && !labelPattern.MatchString(label) {
		return fmt.Errorf("%q is not a valid file system label", label)
	}

	return nil
}

func checkMappingName(name string) error {
	if !mappingNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a valid device-mapper name", name)
	}

	return nil
}
