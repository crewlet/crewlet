package static_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// GIT MUST NEVER TEXT-MERGE THE COMMITTED DASHBOARD.
//
// The tree this package embeds is build output whose names are content-hashed,
// so two branches that both rebuild write different paths for the same chunk.
// Git reads that as a rename/rename, three-way merges 979 KB of minified
// JavaScript, and — measured on this tree — splices nine conflict-marker lines
// into the artefacts: three into each side's entry bundle and three into
// index.html. Both bundles then fail `node --check`. The conflict itself is
// unavoidable and fine; what is not fine is git leaving a corrupted bundle in
// the working tree for someone to stage.
//
// `.gitattributes` turns that off with one directive, and this is what holds
// it there. ASSERTED AS AN EFFECT, through `git check-attr`, never as a
// substring of `.gitattributes`. A text assertion is wrong on four of the five
// realistic ways that line gets broken: it PASSES on a commented-out directive
// and on a later `merge=union` overriding it under last-match-wins, and FAILS
// on the same rule spelled with a leading slash or a tab, and on a
// legitimately broader `static/** -merge`. That is the exact shape
// internal/version deleted eleven tests of in 90d3b05f, whose record requires
// any re-add to be structural rather than a string match.
//
// A GO TEST RATHER THAN BASH, which is the opposite of scripts/check-signoff.sh
// and for a reason that does not transfer: that gate reads git HISTORY, which
// no Go test can reach ergonomically. `check-attr` reads the attribute stack —
// working tree, index and config — and never touches history, so it answers
// the same on the shallow clone ci.yml's test job checks out.
//
// IF YOU RE-MUTATE THIS GATE, deleting `.gitattributes` from the working tree
// is not the mutation it looks like: `check-attr` FALLS BACK TO THE INDEX, so
// a tracked-but-deleted file still answers `unset` and this stays green. That
// is correct — what every clone gets is the committed state — but it means the
// honest mutations are a `#` in front of the directive, a later rule
// overriding it, a wrong glob, or `git rm --cached` as well as the delete. All
// four were run against this gate and all four fail it.
func TestGitWillNotTextMergeTheEmbeddedDashboard(t *testing.T) {
	t.Parallel()

	// SHAPES, NOT TODAY'S NAMES. `check-attr` resolves a path STRING against
	// the attribute stack and never stats it, so naming a file that cannot
	// exist is not only safe, it is what stops this gate rotting: every real
	// name under assets/ carries a hash that `make dashboard` changes.
	governed := []string{
		"static/dashboard/index.html",
		"static/dashboard/assets/index-0000000000.js",
		"static/dashboard/assets/index-0000000000.css",
		"static/dashboard/assets/react-0000000000.js",
		"static/dashboard/protocol.js",
		"static/dashboard/fonts/inter-latin.woff2",
	}
	for _, path := range governed {
		if got := mergeAttr(t, path); got != "unset" {
			t.Errorf("git check-attr merge %s = %q, want \"unset\"\n"+
				"a text merge of this path writes conflict markers into a "+
				"generated file; `static/dashboard/** -merge` in .gitattributes "+
				"is what stops it, and the only correct resolution is "+
				"`make dashboard`", path, got)
		}
	}

	// THE OTHER SIDE, because an attribute scoped too widely is its own bug:
	// the dashboard's SOURCE is hand-written and must merge normally, and so
	// must the icon static.go embeds from one directory up, which the build
	// does not write.
	for _, path := range []string{
		"dashboard/src/app/App.tsx",
		"dashboard/vite.config.ts",
		"static/crewlet-icon.svg",
		"internal/api/dashboard.go",
	} {
		if got := mergeAttr(t, path); got != "unspecified" {
			t.Errorf("git check-attr merge %s = %q, want \"unspecified\" — "+
				"this path is not build output and must merge normally", path, got)
		}
	}
}

// mergeAttr is the `merge` attribute git resolves for one path.
//
// FROM THE REPOSITORY ROOT, explicitly. `check-attr` resolves its arguments
// against the CURRENT DIRECTORY and `go test` runs a package binary in the
// package's own directory — so without this the gate would ask about
// `static/static/dashboard/...` and read `unspecified` for everything, failing
// for a reason that has nothing to do with the attribute.
func mergeAttr(t *testing.T, path string) string {
	t.Helper()
	root := repoRoot(t)

	cmd := exec.Command("git", "check-attr", "merge", "--", path)
	cmd.Dir = root
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git check-attr merge -- %s: %v (%s)", path, err, errb.String())
	}
	// "<path>: merge: <value>", and a path may itself contain ": ".
	line := strings.TrimSpace(out.String())
	idx := strings.LastIndex(line, ": ")
	if idx < 0 {
		t.Fatalf("git check-attr merge -- %s: unparsable answer %q", path, line)
	}
	return line[idx+2:]
}

// repoRoot is the working tree's top level.
//
// Asked of git rather than walked up from the test binary's directory: the
// answer is the same one `check-attr` resolves paths against, so the two
// cannot disagree about where the tree starts.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git working tree, so there is no attribute stack to read: %v", err)
	}
	return strings.TrimSpace(string(out))
}
