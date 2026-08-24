package version

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What a binary says it is, and what keeps that true.

// A BINARY WITH NO STAMP NAMES ITSELF HONESTLY rather than reporting an
// empty string. `go install` and a plain `go build` produce one, and a
// version field that renders empty reads as a broken page rather than as an
// unreleased build.
func TestAnUnstampedBuildStillReportsSomething(t *testing.T) {
	t.Parallel()
	// The suite itself is an unstamped build, which is exactly the case.
	if got := String(); got == "" {
		t.Error("an unstamped build reported no version at all")
	}
}

// THE STAMP WINS over whatever the toolchain recorded. It is the only thing
// the release build can set, and a build info that disagreed with it would
// be the module version rather than the tag.
func TestAStampedValueWins(t *testing.T) {
	t.Parallel()
	if got := resolve("v9.9.9", "v0.0.1"); got != "v9.9.9" {
		t.Errorf("version = %q, want the stamped value", got)
	}
}

// AN UNSTAMPED BUILD REPORTS WHAT THE TOOLCHAIN RECORDED, which is how a
// `go install`ed binary names itself honestly.
func TestNoStampFallsBackToBuildInfo(t *testing.T) {
	t.Parallel()
	if got := resolve("", "v1.4.0"); got != "v1.4.0" {
		t.Errorf("version = %q, want the recorded module version", got)
	}
}

// NEITHER SOURCE IS A REAL ANSWER, and the answer is still not an empty
// string: a version field that renders empty reads as a broken page rather
// than as a build from someone's checkout.
func TestNothingRecordedStillNamesTheBuild(t *testing.T) {
	t.Parallel()
	if got := resolve("", ""); got != "dev" {
		t.Errorf("version = %q, want dev", got)
	}
}

// --- the release configuration -------------------------------------------

// THE LDFLAGS STAMP NAMES THIS PACKAGE, and nothing else notices when it
// stops.
//
// Rename `value`, move this package, change the module path, and the -X flag
// silently applies to nothing: the link succeeds, the binary builds, and
// every release reports the build-info fallback instead of its tag. There is
// no runtime symptom to catch it and no test but this one.
func TestTheReleaseConfigStampsThisVariable(t *testing.T) {
	t.Parallel()
	config := releaseFile(t, ".goreleaser.yaml")
	const want = "-X github.com/crewlet/crewlet/internal/version.value="
	if !strings.Contains(config, want) {
		t.Errorf(".goreleaser.yaml does not stamp %q, so every release would "+
			"report the unstamped fallback", want)
	}
}

// THE MATRIX IS A PLAIN GOOS/GOARCH LOOP ONLY WHILE THE BUILD IS PURE GO.
//
// A dependency that needs cgo does not fail here — it fails on whichever
// cross target the release machine cannot build for, at tag time. Asserting
// the flag keeps the intent visible; `go build` with it set on every target
// is what proves the intent holds.
func TestTheReleaseBuildIsPureGo(t *testing.T) {
	t.Parallel()
	if config := releaseFile(t, ".goreleaser.yaml"); !strings.Contains(config, "CGO_ENABLED=0") {
		t.Error(".goreleaser.yaml does not set CGO_ENABLED=0")
	}
}

// THE IMAGE COPIES THE RIGHT ARCHITECTURE'S BINARY.
//
// goreleaser stages each platform's binary under its own TARGETPLATFORM
// directory, so a Dockerfile that copies a bare path builds nothing at all —
// and it fails inside the release, after every binary is already built.
func TestTheImageCopiesPerPlatform(t *testing.T) {
	t.Parallel()
	dockerfile := releaseFile(t, "Dockerfile")
	if !strings.Contains(dockerfile, "ARG TARGETPLATFORM") ||
		!strings.Contains(dockerfile, "${TARGETPLATFORM}/crewlet") {
		t.Error("the Dockerfile does not copy from ${TARGETPLATFORM}, so the " +
			"image build cannot find the binary goreleaser staged")
	}
}

// EVERY DEPENDENCY SURFACE IS WATCHED.
//
// CLAUDE.md's rule, asserted because nothing reports the omission: a
// manifest with no Dependabot entry produces no pull requests, which looks
// exactly like a manifest with nothing to update. The Go module went the
// length of the rewrite that way.
func TestEveryDependencySurfaceHasADependabotEntry(t *testing.T) {
	t.Parallel()
	config := releaseFile(t, ".github/dependabot.yml")
	for _, want := range []string{"gomod", "docker"} {
		if !strings.Contains(config, "package-ecosystem: "+want) {
			t.Errorf("dependabot.yml has no %q entry, so that surface is "+
				"watched by nothing", want)
		}
	}
	if !strings.Contains(config, `directory: "/go"`) {
		t.Error("the gomod entry does not name the module's directory")
	}
}

// releaseFile reads a repository-root file. The path is relative to this
// package, which is three levels down.
func releaseFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(raw)
}
