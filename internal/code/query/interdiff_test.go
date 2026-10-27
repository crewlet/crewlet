package query_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/query"
)

// paths is every file a diff answer's pages list, in order.
func paths(a code.GitAnswer) []string {
	var out []string
	for _, f := range a.Diff.Files {
		out = append(out, f.Path)
	}
	return out
}

// THE INTERDIFF KEEPS TO THE CHANGE'S PATHS. A change edits the file `a*`;
// its target then edits `ab`, and the change, rebased onto it, edits `a*`
// again. What the change changed between its two revisions is `a*` alone:
// `ab` differs between the two heads only because the rebase brought the
// target's edit in, and neither revision's facts list it — and `a*` is a
// file's name, never a pattern that would take `ab` in with it. Where either
// revision touches all, or the two share one base, the interdiff is the
// heads' whole diff. One file's patch is kept to the same paths: `ab`'s is
// none.
//
// Mutations: match the touched paths as globs — `ab` is kept, red; keep no
// path out — `ab` is kept, red; filter where the bases match — the shared
// base's read counts its files with diff-pairs, red.
func TestTheInterdiffKeepsToTheChangesPaths(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{"a*": "v1\n", "ab": "v1\n", "c.txt": "c\n"}, "base")
	x.branch("change", base)
	first := x.p.Commit(x.work, map[string]string{"a*": "v2\n"}, "the change's first revision")
	x.git("checkout", "-q", "main")
	moved := x.commit(map[string]string{"ab": "v2\n"}, "the target edits ab")
	x.git("checkout", "-q", "change")
	x.git("rebase", "-q", "main")
	second := x.p.Commit(x.work, map[string]string{"a*": "v3\n"}, "the change's second revision")
	x.branch("beside", first)
	beside := x.p.Commit(x.work, map[string]string{"a*": "v4\n", "c.txt": "c2\n"}, "a revision on the same base")
	x.push(codetest.RepoName, "refs/heads/change", "refs/heads/beside")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	from, to := x.facts(s, repo, first, base), x.facts(s, repo, second, moved)
	if !slices.Equal(from.Touched, []string{"a*"}) || !slices.Equal(to.Touched, []string{"a*"}) {
		t.Fatalf("the revisions touch %v and %v, want a* alone in each", from.Touched, to.Touched)
	}
	if whole := paths(mustRead(t, s, diffQ(repo, code.DiffQuery{Base: first, Head: second}))); !slices.Equal(whole,
		[]string{"a*", "ab"}) {
		t.Fatalf("the heads' whole diff lists %v, want a* and the target's ab: the case could not fail", whole)
	}
	q := code.InterdiffQuery{FromBase: from.MergeBase, FromHead: first, ToBase: to.MergeBase, ToHead: second,
		FromTouched: from.Touched, ToTouched: to.Touched}
	a := mustRead(t, s, interdiffQ(repo, q))
	if got := paths(a); a.Kind != code.GitInterdiff || a.Diff.Base != first || a.Diff.Head != second ||
		!slices.Equal(got, []string{"a*"}) || a.Diff.Total != 1 {
		t.Errorf("the interdiff = %s of %s..%s listing %v of %d, want an interdiff of %s..%s listing a* alone",
			a.Kind, a.Diff.Base, a.Diff.Head, got, a.Diff.Total, first, second)
	}

	all := q
	all.ToTouched, all.ToTouchesAll = nil, true
	if got := paths(mustRead(t, s, interdiffQ(repo, all))); !slices.Equal(got, []string{"a*", "ab"}) {
		t.Errorf("an interdiff whose later revision touches all lists %v, want the heads' whole diff", got)
	}
	// TWO REVISIONS ON ONE BASE differ only in files one of them touches, so
	// their interdiff is read as the heads' plain diff: one diff-tree, and no
	// diff-pairs counting the files a filter admitted.
	other := x.facts(s, repo, beside, moved)
	if other.MergeBase != base || !slices.Equal(other.Touched, []string{"a*", "c.txt"}) {
		t.Fatalf("the revision beside the first = %+v, want it based on %s, touching a* and c.txt", other, base)
	}
	rec := &recording{}
	plain := x.server(0, query.Options{Git: rec.on(x, 0)})
	shared := code.InterdiffQuery{FromBase: from.MergeBase, FromHead: first, ToBase: other.MergeBase, ToHead: beside,
		FromTouched: from.Touched, ToTouched: other.Touched}
	if got := paths(mustRead(t, plain, interdiffQ(repo, shared))); !slices.Equal(got, []string{"a*", "c.txt"}) {
		t.Errorf("an interdiff of two revisions on one base lists %v, want every file their heads differ in", got)
	}
	if counted := rec.fed("diff-pairs"); len(counted) != 0 {
		t.Errorf("an interdiff of two revisions on one base ran %d diff-pairs, want its heads' plain diff",
			len(counted))
	}

	for _, tc := range []struct {
		file string
		kept bool
	}{{"a*", true}, {"ab", false}} {
		one := q
		one.File = tc.file
		got := mustRead(t, s, interdiffQ(repo, one))
		if kept := len(got.Diff.Files) == 1 && got.Diff.Patch != ""; kept != tc.kept {
			t.Errorf("the interdiff's patch of %s holds %d files (%q), want it kept %t", tc.file,
				len(got.Diff.Files), got.Diff.Patch, tc.kept)
		}
	}
}
