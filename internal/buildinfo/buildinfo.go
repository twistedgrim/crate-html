// Package buildinfo exposes version information shared by the crate client
// and crated daemon.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// DevelopmentVersion is used for binaries built from a working tree without
// release ldflags.
const DevelopmentVersion = "0.1.0-dev"

// Version is replaced in release builds with:
//
//	-X github.com/Twistedgrim/crate-html/internal/buildinfo.Version=v0.2.0
var Version = DevelopmentVersion

// Current returns the stamped release version. For binaries installed with
// `go install ...@version`, it falls back to the module version embedded by the
// Go toolchain.
func Current() string {
	if Version != DevelopmentVersion {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return DevelopmentVersion
}

// Display returns a consistently v-prefixed version for human-facing output.
func Display(version string) string {
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}
