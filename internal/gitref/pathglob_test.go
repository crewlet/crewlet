package gitref_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitref"
)

// deepPath is a tree path deeper and longer than any ref can be — 152
// components, 306 bytes — so the walk is held to git and to its rule past
// every bound Pattern.Match holds a ref to.
var deepPath = "d/" + strings.Repeat("x/", 150) + "y.go"

// pathTree is every file TestOnePathGlobRuleMatchesTreePaths commits: the
// root's files, a dotfile, nested directories, a component a glob names twice
// over (web/web), a name holding a space, and deepPath.
var pathTree = []string{
	"README.md", "Makefile", ".gitignore", "main.go",
	"docs/a.md", "docs/guides/b.md", "docs/guides/deep/c.md",
	"web/app.go", "web/app_test.go", "web/internal/x.go", "web/web/web.go",
	"internal/code/doc.go", "internal/code/cache/sync.go",
	"a b/c.go",
	deepPath,
}

// ONE RULE, OVER A REF AND OVER A TREE'S PATH: a path glob is Pattern's rule
// read over the paths of a repository's tree, and that is held three ways.
//
//   - TO GIT: git reads a glob pathspec (`:(glob)…`) against a tree's paths by
//     wildmatch with WM_PATHNAME — * within one component, ** whole components,
//     one or more as the last — and its listing of a real repository's index
//     IS the rule for a glob of wildcards, so one column is both answers, each
//     row WRITTEN DOWN and re-measured on every run, so the table cannot drift
//     from the git it describes. For a literal, git lists the path and every
//     path below a directory of that name, and a path glob the identical path
//     alone — the one place this package is stricter — and the test derives
//     that difference from git's own listing rather than from a second column.
//   - TO THE RULE written out plainly ([rule], the one TestPatternMatchAgreesWithItsRule
//     holds Pattern to), over every glob of up to four components drawn from
//     seven against every path of up to four drawn from four: every placement
//     of * and ** against every split of a short path, where Pattern.Match on
//     the same glob and path under refs/heads/ must say the same — one rule,
//     whichever of the two walks asks it.
//   - PAST A REF'S BOUND: a path is a tree's and has none, so globs are asked
//     about paths longer and deeper than any ref, the rule answering for each.
//
// Mutation: read a trailing ** as zero components or more (drop readGlob's
// rewrite) — README.md/** matches the file README.md, red; advance the walk's
// backtrack a byte rather than a component — **/a/** finds the a inside ba,
// red.
func TestOnePathGlobRuleMatchesTreePaths(t *testing.T) {
	t.Parallel()
	c := gittest.NewClient(t)
	dir := c.Init(t)
	// ONE COMMENT IN EVERY FILE: a .gitignore holding a name — its own, or a
	// directory's of the tree — would keep what it names out of the index the
	// table lists.
	files := make(map[string]string, len(pathTree))
	for _, p := range pathTree {
		files[p] = "# nothing\n"
	}
	c.Commit(t, dir, files, "the tree the globs read")
	gitLists := func(glob string) []string {
		out := c.Run(t, dir, "ls-files", "-z", "--", ":(glob)"+glob)
		var listed []string
		for p := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
			if p != "" {
				listed = append(listed, p)
			}
		}
		return listed
	}
	matches := func(glob string) []string {
		parsed, err := gitref.ParsePathGlob(glob)
		if err != nil {
			t.Fatalf("ParsePathGlob(%q) refused a glob the table asks git about: %v", glob, err)
		}
		var matched []string
		for _, p := range pathTree {
			if parsed.Match(p) {
				matched = append(matched, p)
			}
		}
		return matched
	}

	goFiles := []string{"main.go", "web/app.go", "web/app_test.go", "web/internal/x.go", "web/web/web.go",
		"internal/code/doc.go", "internal/code/cache/sync.go", "a b/c.go", deepPath}
	for _, tc := range []struct {
		glob string
		want []string
	}{
		{"*", []string{"README.md", "Makefile", ".gitignore", "main.go"}},
		{"*.go", []string{"main.go"}}, // the root's alone: * never crosses a /
		{".*", []string{".gitignore"}},
		{"**", pathTree},
		{"**/*.go", goFiles},
		{"docs/**", []string{"docs/a.md", "docs/guides/b.md", "docs/guides/deep/c.md"}},
		{"docs/*", []string{"docs/a.md"}},
		{"docs/**/*.md", []string{"docs/a.md", "docs/guides/b.md", "docs/guides/deep/c.md"}}, // zero components
		{"**/guides/**", []string{"docs/guides/b.md", "docs/guides/deep/c.md"}},
		{"web/*_test.go", []string{"web/app_test.go"}},
		{"web/**", []string{"web/app.go", "web/app_test.go", "web/internal/x.go", "web/web/web.go"}},
		{"**/web/*.go", []string{"web/app.go", "web/app_test.go", "web/web/web.go"}},
		{"web/**/web.go", []string{"web/web/web.go"}},
		{"*/*/*.go", []string{"web/internal/x.go", "web/web/web.go", "internal/code/doc.go"}},
		{"internal/**/sync.go", []string{"internal/code/cache/sync.go"}},
		{"**/code/**", []string{"internal/code/doc.go", "internal/code/cache/sync.go"}},
		{"a b/*", []string{"a b/c.go"}},
		{"d/**/x/**/y.go", []string{deepPath}},
		{"d/**/x/x/**/y.go", []string{deepPath}},
		{"d/**/z/**/y.go", nil}, // backtracks through every component of deepPath and finds nothing
		{"README.md/**", nil},   // a trailing ** is one or more, never the prefix itself
	} {
		if listed := gitLists(tc.glob); !sameSet(listed, tc.want) {
			t.Errorf("git ls-files :(glob)%s lists %v, the table says %v", tc.glob, listed, tc.want)
		}
		if matched := matches(tc.glob); !sameSet(matched, tc.want) {
			t.Errorf("PathGlob(%s) matches %v, git lists %v", tc.glob, matched, tc.want)
		}
	}

	// Literals: git lists the path and every path below it up to a '/'; a
	// path glob matches the identical path alone.
	for _, tc := range []struct {
		glob string
		git  []string
	}{
		{"Makefile", []string{"Makefile"}},
		{"docs/a.md", []string{"docs/a.md"}},
		{"docs", []string{"docs/a.md", "docs/guides/b.md", "docs/guides/deep/c.md"}},
		{"web/web", []string{"web/web/web.go"}},
	} {
		listed := gitLists(tc.glob)
		if !sameSet(listed, tc.git) {
			t.Errorf("git ls-files :(glob)%s lists %v, the table says %v", tc.glob, listed, tc.git)
		}
		var exact []string
		for _, p := range listed {
			switch {
			case p == tc.glob:
				exact = append(exact, p)
			case strings.HasPrefix(p, tc.glob+"/"):
			default:
				t.Errorf("git ls-files :(glob)%s lists %s, neither that path nor one below it — the "+
					"literal rule this package is stricter than is not git's any more", tc.glob, p)
			}
		}
		if matched := matches(tc.glob); !sameSet(matched, exact) {
			t.Errorf("PathGlob(%s) matches %v, want only the identical path: %v (git lists %v)",
				tc.glob, matched, exact, listed)
		}
	}

	// Refused here and taken by git — where this package is stricter: the
	// metacharacters of another dialect, which git's wildmatch reads as
	// metacharacters and a path glob would read as themselves, and a leading
	// !, which gitignore reads as a negation and a pathspec as a name.
	for _, tc := range []struct {
		glob string
		git  []string
	}{
		{"?ain.go", []string{"main.go"}},
		{"[m]ain.go", []string{"main.go"}},
		{`\main.go`, []string{"main.go"}},
		{"!main.go", nil},
	} {
		if listed := gitLists(tc.glob); !sameSet(listed, tc.git) {
			t.Errorf("git ls-files :(glob)%s lists %v, the table says %v", tc.glob, listed, tc.git)
		}
		if _, err := gitref.ParsePathGlob(tc.glob); err == nil {
			t.Errorf("ParsePathGlob(%q) took it: a glob another dialect reads otherwise than this "+
				"grammar would is refused", tc.glob)
		}
	}

	// THE RULE, ON EVERY SHORT SHAPE, and the ref walk saying the same.
	paths := sequences([]string{"a", "b", "ab", "ba"}, 4)
	for _, g := range sequences([]string{"a", "b", "*", "a*", "*b", "*a*", "**"}, 4) {
		glob := strings.Join(g, "/")
		parsed, err := gitref.ParsePathGlob(glob)
		if err != nil {
			t.Fatalf("ParsePathGlob(%s) refused it: %v", glob, err)
		}
		pattern, ok := gitref.ParsePattern("refs/heads/" + glob)
		if !ok {
			t.Fatalf("ParsePattern(refs/heads/%s) refused it", glob)
		}
		for _, p := range paths {
			p := strings.Join(p, "/")
			want := rule(g, strings.Split(p, "/"))
			if got := parsed.Match(p); got != want {
				t.Errorf("PathGlob.Match: %s against %s is %v, the rule says %v", glob, p, got, want)
			}
			if got := pattern.Match("refs/heads/" + p); got != want {
				t.Errorf("Pattern.Match: refs/heads/%s against refs/heads/%s is %v, the rule says %v",
					glob, p, got, want)
			}
		}
	}

	// PAST A REF'S BOUND: a 300-byte component, a 2 KiB path of 1 025
	// components, each asked about by globs whose parts sit at its start, its
	// end and between two **s.
	long := []string{
		deepPath,
		strings.Repeat("w", 300) + "/y.go",
		"d/" + strings.Repeat("x/", 1023) + "y.go",
		"d/" + strings.Repeat("x/", 1023) + "z/y.go",
	}
	for _, glob := range []string{"**/*.go", "d/**", "d/**/y.go", "d/**/x/**/y.go", "d/**/z/**/y.go",
		"**/z/*.go", "*/y.go", "w*/y.go", "*w/*", "d/*/x/**", "**/x/x/x/**/*.go", "*.go", "**/q.go"} {
		parsed, err := gitref.ParsePathGlob(glob)
		if err != nil {
			t.Fatalf("ParsePathGlob(%s) refused it: %v", glob, err)
		}
		for _, p := range long {
			want := rule(strings.Split(glob, "/"), strings.Split(p, "/"))
			if got := parsed.Match(p); got != want {
				t.Errorf("PathGlob.Match: %s against a path of %d bytes is %v, the rule says %v",
					glob, len(p), got, want)
			}
		}
	}
}

// A PATH GLOB IS REFUSED FOR WHAT IT WOULD READ AS AND NOT DO, each refusal a
// sentence that says which rule: an empty or a root-anchored glob, a directory
// written as one, a component no tree holds, ** inside a component, a
// metacharacter of another dialect, a negation, a control character — and a
// glob of wildcards past MaxPathGlobComponents components or any glob past
// MaxPathGlobBytes.
func TestAPathGlobIsRefusedForWhatItWouldReadAs(t *testing.T) {
	t.Parallel()
	for _, glob := range []string{
		"README.md",
		"*",
		"**",
		"**/*.go",
		"docs/**",
		"a/**/b",
		"web/*_test.go",
		".github/workflows/*.yml",
		"a b/c d.go",            // a space is a name's
		"a]b",                   // ] is no metacharacter on its own
		"x/!y",                  // nor ! past a glob's start
		"é/*",                   // UTF-8 beyond ASCII
		"a/b/c/d/e/f/g/*",       // exactly MaxPathGlobComponents
		"a/b/c/d/e/f/g/h/i/j/k", // a literal is bounded only by its bytes
		strings.Repeat("a", gitref.MaxPathGlobBytes),                // exactly MaxPathGlobBytes
		"**/" + strings.Repeat("a*", (gitref.MaxPathGlobBytes-3)/2), // a glob of a piece a byte, at its bound
	} {
		if _, err := gitref.ParsePathGlob(glob); err != nil {
			t.Errorf("ParsePathGlob(%q) refused a path glob: %v", glob, err)
		}
	}
	for _, tc := range []struct {
		glob, why string
	}{
		{"", "empty"},
		{"/docs/**", "relative to the repository's root"},
		{"docs/", "dir/**"},
		{"a//b", "empty component"},
		{"./a", ". component"},
		{"a/../b", ".. component"},
		{"a**b", "whole component"},
		{"**.go", "whole component"},
		{"a/***", "whole component"},
		{"a?.go", `"?"`},
		{"[ab].go", `"["`},
		{`a\*`, `"\\"`},
		{"!docs/**", "negation"},
		{"a\tb", "control character"},
		{"a\x7fb", "control character"},
		{"\xff", "UTF-8"},
		{"a/b/c/d/e/f/g/h/*", "components"},
		{strings.Repeat("a", gitref.MaxPathGlobBytes+1), "bytes"},
	} {
		_, err := gitref.ParsePathGlob(tc.glob)
		switch {
		case err == nil:
			t.Errorf("ParsePathGlob(%q) took it, want a refusal saying %q", tc.glob, tc.why)
		case !strings.Contains(err.Error(), tc.why):
			t.Errorf("ParsePathGlob(%q) = %q, want a refusal saying %q", tc.glob, err, tc.why)
		}
	}
}

// THE ZERO PATH GLOB MATCHES NOTHING: a glob nobody parsed owns no path.
func TestTheZeroPathGlobMatchesNothing(t *testing.T) {
	t.Parallel()
	var zero gitref.PathGlob
	for _, p := range append([]string{"", "a", deepPath}, pathTree...) {
		if zero.Match(p) {
			t.Errorf("the zero PathGlob matches %q: a glob nobody parsed owns nothing", p)
		}
	}
}

// A PATH GLOB ANSWERS WHATEVER IT IS HANDED, AND ALLOCATES NOTHING: Match is
// asked about every path a revision touches by every owner's globs, so a
// glob's walk takes no garbage on any shape — between two **s on the deepest
// path included — and a string no tree path is (an empty component, a leading
// or trailing '/', nothing at all) is an answer, never a panic.
func TestMatchingAPathAllocatesNothingAndNeverPanics(t *testing.T) {
	// NOT PARALLEL: testing.AllocsPerRun refuses a parallel test, whose
	// neighbours' allocations it would count.
	deepest := "d/" + strings.Repeat("x/", 2047) + "y.go"
	malformed := []string{"", "/", "//", "a//b", "/a", "a/", strings.Repeat("/", 600), "d//x//y.go"}
	for _, glob := range []string{"README.md", "*", "**", "**/*.go", "d/**/y.go", "d/**/x/**/y.go",
		"d/**/z/**/y.go", "**/x/**", "a/*/b/**/c/*", "*a*/**/*b*/**/c*"} {
		parsed, err := gitref.ParsePathGlob(glob)
		if err != nil {
			t.Fatalf("ParsePathGlob(%s) refused it: %v", glob, err)
		}
		if n := testing.AllocsPerRun(100, func() { parsed.Match(deepest) }); n != 0 {
			t.Errorf("%s: PathGlob.Match allocates %v times a call", glob, n)
		}
		for _, p := range malformed {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s against %q panics: %v", glob, p, r)
					}
				}()
				parsed.Match(p)
			}()
		}
	}
}
