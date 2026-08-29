// Package version is the one place the engine's version lives.
//
// The value is stamped at build time by the release tooling
// (-ldflags "-X github.com/crewlet/crewlet/internal/version.value=v1.2.3").
// A build with no stamp reports the module's own build info, so a
// `go install`ed binary still names itself honestly instead of claiming to be
// a release it is not.
package version

import (
	"runtime/debug"
	"sync"
)

// value is overwritten at link time by the release build.
var value string

var resolved = sync.OnceValue(func() string { return resolve(value, moduleVersion()) })

// resolve is the rule, split out from both the cache above it and the
// runtime lookup beside it.
//
// sync.OnceValue is right for the caller — a version cannot change while a
// process runs — and debug.ReadBuildInfo answers about the binary a test
// happens to be running in. Between them, neither fallback branch was
// reachable from a test. Taking both inputs as arguments makes the rule a
// function of what it is given, and leaves the two adapters with nothing to
// decide.
func resolve(stamp, built string) string {
	if stamp != "" {
		return stamp
	}
	if built == "" {
		// Not "unknown" and never empty: a binary built outside the
		// release path is a development build, and saying so is more
		// use than admitting to a failed lookup.
		return "dev"
	}
	return built
}

// moduleVersion is the version the toolchain recorded, or empty when it
// recorded none.
func moduleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return info.Main.Version
}

// String returns the engine version.
func String() string { return resolved() }

// Revision returns the VCS commit the binary was built from, when the
// toolchain recorded one. Empty otherwise — never a guess.
func Revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}
