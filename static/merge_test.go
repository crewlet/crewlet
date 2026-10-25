package static_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// GIT MUST NEVER TEXT-MERGE THE COMMITTED DASHBOARD, NOR THE ARTIFACT TREE.
//
// The artifact tree (the page runtime and the artifact kit) is the same
// source's third build target and is held by the same rule: two branches that
// both rebuild it would text-merge minified JavaScript just the same.
//
// The dashboard this package embeds is build output whose names are content-hashed,
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
// THROUGH gittest's CLIENT, never the developer's own git — the one way a test
// drives git (gitproc's TestOnlyGitprocExecsGit) — and here for this gate's own
// reason too: the attribute stack includes the global attributes file
// (core.attributesFile, ~/.config/git/attributes by default) and the host's
// /etc/gitattributes, and a `* merge=union` in either answers for every path
// below, turning the "unspecified" side red on one machine and green on the
// next. The client has a HOME of its own and reads no system attributes, so
// what it reads is the checkout's — what every clone gets.
//
// IF YOU RE-MUTATE THIS GATE, deleting `.gitattributes` from the working tree
// is not the mutation it looks like: `check-attr` FALLS BACK TO THE INDEX, so
// a tracked-but-deleted file still answers `unset` and this stays green. That
// is correct — what every clone gets is the committed state — but it means the
// honest mutations are a `#` in front of the directive, a later rule
// overriding it, a wrong glob, or `git rm --cached` as well as the delete. All
// four were run against this gate and all four fail it.
//
// THE CHECKOUT IS TRUSTED BY NAME. The client takes the developer's TRUST in
// a checkout another user owns (safe.directory, which a container running as
// root over a bind mount needs) from the machine no more than it takes their
// attributes, so the gate is handed this module's checkout through gittest's
// ModuleCheckout: that path alone is trusted, git confirms it is the working
// tree's top level, and a tree it cannot read fails the gate rather than
// skipping it, with an ownership refusal named apart from a tree that is no
// checkout.
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
		"static/dashboard/protocol.js",
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
				"generated file; `static/dashboard/** -merge` and "+
				"`static/artifacts/** -merge` in .gitattributes are what stop it, "+
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
