// Package version reports the build version.
//
// Release builds may set it explicitly:
//
//	-ldflags "-X github.com/ytjohn/fastener/internal/version.Version=v0.1.0"
//
// Otherwise String falls back to the module version Go records at build time,
// so `go install github.com/ytjohn/fastener/cmd/fastener@v0.1.0` reports v0.1.0.
// A plain `go build` from a checkout reports "dev".
package version

import "runtime/debug"

// Version is set by -ldflags; empty means "use the build info".
var Version = ""

// String returns the version to display and record.
func String() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}
