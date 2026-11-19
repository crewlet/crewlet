package static_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
)

// GIT MUST NEVER TEXT-MERGE THE COMMITTED DASHBOARD, NOR THE ARTIFACT TREE.
//
// Two branches rebuilding the content-hashed trees would have git text-merge
// minified JavaScript, splicing conflict markers into bundles; `.gitattributes`
// turns that off. ASSERTED AS AN EFFECT through `git check-attr`, never as a
// substring: text PASSES a commented-out directive or a later `merge=union`,
// and FAILS a leading slash or a broader `static/** -merge`. A GO TEST:
// `check-attr` reads no history, so it works on the shallow clone. THROUGH
// gittest's CLIENT, so a host `* merge=union` cannot decide, handed the
// checkout by ModuleCheckout (an unreadable tree fails, never skips).
// RE-MUTATING: deleting the file is no mutation (`check-attr` falls back to the
// index); use a `#`, an overriding rule, a wrong glob or `git rm --cached`.
func TestGitWillNotTextMergeTheEmbeddedTrees(t *testing.T) {
	t.Parallel()
	git, root := gittest.NewClient(t).ModuleCheckout(t)

	// SHAPES, NOT TODAY'S NAMES. `check-attr` resolves a path STRING against
	// the attribute stack and never stats it, so naming a file that cannot
	// exist is not only safe, it is what stops this gate rotting: every real
	// name under assets/ carries a hash that `make dashboard` changes.
	governed := []string{
		"static/dashboard/index.html",
		"static/dashboard/assets/index-0000000000.js",
		"static/dashboard/assets/index-0000000000.css",
		"static/dashboard/assets/react-0000000000.js",
		"static/protocol/protocol.js",
		"static/dashboard/fonts/geist-latin.woff2",
		"static/artifacts/runtime/1.0.0/runtime.js",
		"static/artifacts/runtime/1.0.0/THIRD_PARTY_NOTICES.txt",
		"static/artifacts/kit/0.18.0/kit.js",
		"static/artifacts/kit/0.18.0/fonts/geist-latin.woff2",
	}
	for _, path := range governed {
		if got := mergeAttr(t, git, root, path); got != "unset" {
			t.Errorf("git check-attr merge %s = %q, want \"unset\"\n"+
				"a text merge of this path writes conflict markers into a "+
				"generated file; `static/dashboard/** -merge`, `static/artifacts/** -merge` "+
				"and `static/protocol/** -merge` in .gitattributes are what stop it, "+
				"and the only correct resolution is `make dashboard`", path, got)
		}
	}

	// THE OTHER SIDE, because an attribute scoped too widely is its own bug:
	// the dashboard's SOURCE is hand-written and must merge normally, and so
	// must the Go beside the tree, which the build does not write.
	for _, path := range []string{
		"dashboard/src/app/App.tsx",
		"dashboard/vite.config.ts",
		"dashboard/vite.runtime.config.ts",
		"dashboard/src/runtime/runtime.ts",
		"static/static.go",
		"internal/api/dashboard.go",
	} {
		if got := mergeAttr(t, git, root, path); got != "unspecified" {
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
func mergeAttr(t *testing.T, git *gittest.Client, root, path string) string {
	t.Helper()
	// "<path>: merge: <value>", and a path may itself contain ": ".
	line := strings.TrimSpace(git.Run(t, root, "check-attr", "merge", "--", path))
	idx := strings.LastIndex(line, ": ")
	if idx < 0 {
		t.Fatalf("git check-attr merge -- %s: unparsable answer %q", path, line)
	}
	return line[idx+2:]
}
