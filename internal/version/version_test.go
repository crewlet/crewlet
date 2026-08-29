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
	// The directory is not asserted: every surface here is at the
	// repository root now, so naming "/" would only restate what the
	// entries already say and would break the day one of them legitimately
	// moves. What must not silently vanish is the ENTRY.
}

// releaseFile reads a repository-root file. The path is relative to this
// package, which is three levels down.
func releaseFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(raw)
}

// THE GENERATED NOTES CANNOT SILENTLY DROP A PULL REQUEST.
//
// GitHub writes each release body from the pull requests merged since the
// previous tag, and .github/release.yml sorts that list into categories. A
// pull request matching NO category is omitted — not flagged, not appended,
// omitted — so without a catch-all the notes are missing whatever nobody
// labelled. Those notes are the only record a release has, and the omission
// is discoverable only by reading a release and noticing an absence.
func TestTheGeneratedNotesHaveACatchAllCategory(t *testing.T) {
	t.Parallel()
	if config := releaseFile(t, ".github/release.yml"); !strings.Contains(config, `- "*"`) {
		t.Error(".github/release.yml has no catch-all category, so a pull " +
			"request with no labels is dropped from the release notes")
	}
}

// GITHUB WRITES THE NOTES, NOT GORELEASER.
//
// goreleaser's default changelog is assembled from commit SUBJECTS. Ours
// must come from pull request TITLES, because a pull request squash-merges
// into one commit and the title is what CONTRIBUTING.md asks contributors to
// write as a release note. Flip this to any other mode and .github/release.yml
// becomes a dead file: the categories keep being maintained and stop
// appearing anywhere.
func TestTheReleaseNotesComeFromGitHub(t *testing.T) {
	t.Parallel()
	if config := releaseFile(t, ".goreleaser.yaml"); !strings.Contains(config, "use: github-native") {
		t.Error(".goreleaser.yaml does not use github-native release notes, " +
			"so the body would be built from commit subjects and " +
			".github/release.yml would shape nothing")
	}
}

// A PRE-RELEASE NEVER BECOMES THE DEFAULT DOWNLOAD.
//
// Two surfaces hand an unpinned user whatever the newest release is: GitHub's
// "Latest" badge and the image's `latest` tag. A release candidate taking
// either one ships it to everyone who pinned nothing, and both are set at
// release time from this file alone.
func TestAPreReleaseTakesNeitherLatest(t *testing.T) {
	t.Parallel()
	config := releaseFile(t, ".goreleaser.yaml")
	if !strings.Contains(config, "prerelease: auto") {
		t.Error(".goreleaser.yaml does not flag pre-releases, so an rc tag " +
			"would become GitHub's Latest release")
	}
	if !strings.Contains(config, "{{ if not .Prerelease }}latest{{ end }}") {
		t.Error(".goreleaser.yaml does not guard the `latest` image tag on " +
			"the release being stable, so `docker pull crewlet` would serve " +
			"a pre-release")
	}
}

// EXACTLY ONE WORKFLOW PUBLISHES A TAG.
//
// Two workflows triggered by `v*` both try to create one GitHub Release for
// one tag, and the loser fails the run after its artifacts are already built.
// That is how the rewrite's pipeline and the one it replaced coexisted, and
// the whole reason the tag trigger had to move in the same commit that
// deleted the old workflow.
func TestOnlyOneWorkflowIsTriggeredByAVersionTag(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("globbing the workflows: %v", err)
	}
	var triggered []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "tags:") && strings.Contains(trimmed, `"v*"`) {
				triggered = append(triggered, filepath.Base(path))
				break
			}
		}
	}
	if len(triggered) != 1 {
		t.Errorf("workflows triggered by a v* tag = %v, want exactly one — "+
			"two of them race to create one GitHub Release", triggered)
	}
}

// --- the dependency auto-merge -------------------------------------------

// A BUMP IS APPROVED ONLY WHILE BOTH HALVES OF THE GUARD HOLD.
//
// .github/workflows/dependabot-merge.yml approves a pull request and queues
// it to merge with no person in the loop, so what it refuses to do that for
// is the whole of its safety. Checking the AUTHOR is what limits it to a
// bump. Checking the ACTOR is what stops it approving a commit a PERSON
// pushed onto a Dependabot branch — anyone with write access can push one,
// and dropping that half turns the workflow into a way to have your own
// change approved by the repository itself. Neither omission has a symptom:
// the workflow keeps running, and it runs on the wrong pull requests.
func TestTheAutoMergeGuardChecksBothAuthorAndActor(t *testing.T) {
	t.Parallel()
	workflow := releaseFile(t, ".github/workflows/dependabot-merge.yml")
	for _, want := range []string{
		"github.event.pull_request.user.login == 'dependabot[bot]'",
		"github.actor == 'dependabot[bot]'",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("dependabot-merge.yml does not gate on %s, so it would "+
				"approve and merge a pull request Dependabot did not write",
				want)
		}
	}
}

// THE UNATTENDED MERGE WAITS FOR CI, AND LANDS AS ONE COMMIT.
//
// `--auto` is what makes merging without a reviewer safe: it queues the pull
// request behind the checks `main`'s protection rule requires rather than
// merging it on the spot. Losing that one word is a silent change of meaning
// — the step still succeeds and the bump still lands, only now before
// anything has reported on it. `--squash` is the other half: a pull request
// becomes ONE commit whose subject is its title, which is what CONTRIBUTING.md
// asks a title to be written for and what the generated release notes carry.
func TestTheAutoMergeQueuesBehindTheRequiredChecks(t *testing.T) {
	t.Parallel()
	workflow := releaseFile(t, ".github/workflows/dependabot-merge.yml")
	if !strings.Contains(workflow, "gh pr merge --auto --squash") {
		t.Error("dependabot-merge.yml does not run `gh pr merge --auto " +
			"--squash`, so a bump would merge without waiting for CI, or " +
			"would land as something other than the one commit its title " +
			"describes")
	}
}
