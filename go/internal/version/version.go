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

var resolved = sync.OnceValue(func() string {
	if value != "" {
		return value
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "dev"
	}
	return info.Main.Version
})

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
