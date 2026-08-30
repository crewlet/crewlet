package version

import "testing"

// What a binary says it is.

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
