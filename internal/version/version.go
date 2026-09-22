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

// developmentSpellings are the versions that mean "this binary was not
// produced by the release pipeline and was not installed at a released
// version".
//
// TWO, and they come from the two ways a version is absent: `dev` is what
// [resolve] writes when there is no build info at all, and `(devel)` is what
// the toolchain records for a build from a source tree that is not at a tagged
// module version. Everything else — a stamped `v1.2.3`, a `go install @v0.1.0`,
// a pinned pseudo-version — is something somebody could be running.
var developmentSpellings = map[string]struct{}{
	"dev": {}, "(devel)": {},
}

// IsDevelopment reports whether this binary is a development build.
//
// AN ALLOWLIST RATHER THAN A DENYLIST OF RELEASE SHAPES, and the direction is
// the whole of it. Its caller is a bypass that must never run on something an
// operator deployed — internal/api/auth's development principal, which
// authenticates every request with no credential — so the question has to be
// "do I positively know this is a development build", not "does this fail to
// look like a release". A toolchain that spells the unversioned case some
// third way then refuses the bypass, which costs a developer one build flag;
// under the other direction it would silently enable it on whatever that
// spelling turned out to be.
func IsDevelopment() bool { return IsDevelopmentFor(resolved()) }

// IsDevelopmentFor is the rule over a version handed to it, split out from the
// process's own for [resolve]'s reason: [resolved] is a [sync.OnceValue] over
// link-time state, so no test can put a release version in front of the
// predicate that decides whether a bypass is available. Taking the version as
// an argument makes the rule exercisable; the accessor above has nothing left
// to decide.
func IsDevelopmentFor(v string) bool {
	_, ok := developmentSpellings[v]
	return ok
}

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
