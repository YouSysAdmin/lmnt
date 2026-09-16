// Package version exposes the build's version string. It is set at link time
// with -X github.com/yousysadmin/lmnt/internal/version.Version=<tag> and
// falls back to module build info for `go install` builds.
package version

import "runtime/debug"

// Version is the release tag or "dev" when the binary was built without one.
var Version = "dev"

// String returns the effective version: the linked-in value, or the module
// version recorded by the Go toolchain when available.
func String() string {
	if Version != "dev" {
		return Version
	}

	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	return Version
}
