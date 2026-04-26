// Package buildver derives a version string for build artifacts. It
// prefers an ldflag-injected value, falls back to runtime/debug build
// info, and finally to "devel" so callers always get something printable.
package buildver

import "runtime/debug"

// String returns "<name>/<version>". If override is non-empty it wins;
// otherwise we read the module's embedded version (set by `go install
// …@vX.Y.Z` or by VCS info on a `go build` inside a checkout).
func String(name, override string) string {
	v := override
	if v == "" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
	}
	if v == "" {
		v = "devel"
	}
	return name + "/" + v
}
