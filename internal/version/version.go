// Package version exposes the running binary's semver to other
// packages without dragging in a dependency on cmd/bones. main.go
// sets it via Set() at startup; everything else calls Get().
//
// The default ("dev") is what unset GoReleaser ldflags produce, so
// development builds and tests see a stable value. Drift checks
// treat "dev" specially — they do not warn against "dev" because
// the workspace stamp would always look stale during local hacking.
package version

var current = "dev"

// Set records the running binary's version. Called once from
// cmd/bones/main.go after ldflags-injected globals are read.
// Callers other than main should not invoke this.
func Set(v string) {
	if v != "" {
		current = v
	}
}

// Get returns the running binary's version, or "dev" if Set was
// never called.
func Get() string {
	return current
}

// IsDev reports whether the running binary is an unstamped build
// (Set was never called or was called with "dev"). Drift checks
// suppress warnings against dev binaries.
func IsDev() bool {
	return current == "dev" || current == ""
}
