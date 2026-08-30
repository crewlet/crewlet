package version

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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

// --- shared helpers, used by makefile_test.go ---------------------------

// releaseFile reads a repository-root file, found by walking up rather than
// by counting "../.." — these assertions are about a package that can move,
// and a relative hop that has to be edited alongside the move is one more way
// for them to go quiet.
func releaseFile(t *testing.T, name string) string {
	t.Helper()
	root, _ := moduleRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(raw)
}

// moduleRoot walks up from this package until it finds the go.mod that owns
// it, and returns that directory with the module path it declares.
func moduleRoot(t *testing.T) (dir, modulePath string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locating this package: %v", err)
	}
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					return dir, strings.TrimSpace(rest)
				}
			}
			t.Fatalf("%s declares no module path", filepath.Join(dir, "go.mod"))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above this package")
		}
		dir = parent
	}
}

// workflowFiles is every workflow GitHub would run.
//
// BOTH suffixes. GitHub reads .yml and .yaml alike, so a check that globs one
// of them is blind to half the directory — and blind in the direction that
// matters, since the file nobody remembered to name consistently is exactly
// the one an assertion is looking for.
func workflowFiles(t *testing.T) []string {
	t.Helper()
	root, _ := moduleRoot(t)
	var paths []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		matched, err := filepath.Glob(filepath.Join(root, ".github", "workflows", pattern))
		if err != nil {
			t.Fatalf("globbing the workflows: %v", err)
		}
		paths = append(paths, matched...)
	}
	slices.Sort(paths)
	return paths
}

// yamlInlineList splits a `[a, b]` flow sequence into its members.
func yamlInlineList(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	var out []string
	for _, field := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(field); v != "" {
			out = append(out, v)
		}
	}
	return out
}
