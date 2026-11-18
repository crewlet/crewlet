package query_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// paths is every file a diff answer's pages list, in order.
func paths(a gitlet.GitAnswer) []string {
	var out []string
	for _, f := range a.Diff.Files {
		out = append(out, f.Path)
	}
	return out
}

// THE INTERDIFF KEEPS TO THE CHANGE'S PATHS. A change edits `a*`; its target
// edits `ab`, and the change, rebased onto it, edits `a*` again. The interdiff
// is `a*` alone: `ab` differs between the heads only through the rebase, and
// `a*` is a file's name, never a glob taking `ab` in. Where either revision
// touches all, or the two share one base, it is the heads' whole diff. One
// file's patch keeps to the same paths: `ab`'s is none.
//
// Mutations: touched paths matched as globs, or no path kept out — `ab` kept,
// red; a filter where the bases match — diff-pairs counts the shared base's
// files, red.
func TestTheInterdiffKeepsToTheChangesPaths(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
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
	x.push(gitlettest.RepoName, "refs/heads/change", "refs/heads/beside")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})

	from, to := x.facts(s, repo, first, base), x.facts(s, repo, second, moved)
	if !slices.Equal(from.Touched, []string{"a*"}) || !slices.Equal(to.Touched, []string{"a*"}) {
		t.Fatalf("the revisions touch %v and %v, want a* alone in each", from.Touched, to.Touched)
	}
	if whole := paths(mustRead(t, s, diffQ(repo, gitlet.DiffQuery{Base: first, Head: second}))); !slices.Equal(whole,
		[]string{"a*", "ab"}) {
		t.Fatalf("the heads' whole diff lists %v, want a* and the target's ab: the case could not fail", whole)
	}
	q := gitlet.InterdiffQuery{FromBase: from.MergeBase, FromHead: first, ToBase: to.MergeBase, ToHead: second,
		FromTouched: from.Touched, ToTouched: to.Touched}
	a := mustRead(t, s, interdiffQ(repo, q))
	if got := paths(a); a.Kind != gitlet.GitInterdiff || a.Diff.Base != first || a.Diff.Head != second ||
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
	shared := gitlet.InterdiffQuery{FromBase: from.MergeBase, FromHead: first, ToBase: other.MergeBase, ToHead: beside,
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

// AN INTERDIFF'S GIT IS THE CHANGE'S, NEVER THE TARGET'S. A change rebased over
// a target that moved thirty files adds a file, deletes one, makes one
// executable, turns one into a link and edits one: its interdiff runs no
// diff-tree, feeds one lookup two lines a touched path and none of the
// target's, and answers exactly the heads' whole diff kept to the touched
// paths (status, modes, ids, counts) and one file's patch as the heads' own.
//
// Mutations: the heads walked for an exact filter — a diff-tree runs, red; a
// mode change read as a change of type — m.sh is T, red; a change of type read
// as a modification — t.txt is M, red.
func TestAnInterdiffsGitIsTheChangesNeverTheTargets(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	base := x.commit(map[string]string{"a.txt": "v1\n", "b.txt": "b\n", "m.sh": "echo\n", "t.txt": "t\n"},
		"base")
	x.branch("change", base)
	first := x.commitStaged(map[string]string{"a.txt": "v2\n"}, nil, "the change's first revision")
	x.git("checkout", "-q", "main")
	churn := map[string]string{}
	for i := range 30 {
		churn[fmt.Sprintf("z/%02d.txt", i)] = fmt.Sprintf("target %d\n", i)
	}
	moved := x.commit(churn, "the target moves thirty files of its own")
	x.git("checkout", "-q", "change")
	x.git("rebase", "-q", "main")
	second := x.commitStaged(map[string]string{"a.txt": "v3\n", "n.txt": "new\n"}, func() {
		if err := os.Remove(filepath.Join(x.work, "b.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(x.work, "m.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
		x.link("t.txt", "a.txt")
	}, "the change's second revision")
	x.git("branch", "first-revision", first)
	x.push(gitlettest.RepoName, "refs/heads/change", "refs/heads/first-revision")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})

	from, to := x.facts(s, repo, first, base), x.facts(s, repo, second, moved)
	touched := []string{"a.txt", "b.txt", "m.sh", "n.txt", "t.txt"}
	if from.MergeBase == to.MergeBase || !slices.Equal(to.Touched, touched) {
		t.Fatalf("the revisions are based on %s and %s, the later touching %v; want two bases and %v",
			from.MergeBase, to.MergeBase, to.Touched, touched)
	}
	whole := mustRead(t, s, diffQ(repo, gitlet.DiffQuery{Base: first, Head: second}))
	var want []gitlet.DiffFile
	for _, f := range whole.Diff.Files {
		if slices.Contains(touched, f.Path) {
			want = append(want, f)
		}
	}
	if len(whole.Diff.Files) != len(want)+len(churn) {
		t.Fatalf("the heads' whole diff lists %d files, want the %d touched and the target's %d: the case "+
			"could not fail", len(whole.Diff.Files), len(want), len(churn))
	}

	rec := &recording{}
	lean := x.server(0, query.Options{Git: rec.on(x, 0)})
	q := gitlet.InterdiffQuery{FromBase: from.MergeBase, FromHead: first, ToBase: to.MergeBase, ToHead: second,
		FromTouched: from.Touched, ToTouched: to.Touched}
	a := mustRead(t, lean, interdiffQ(repo, q))
	if !reflect.DeepEqual(a.Diff.Files, want) || a.Diff.Total != len(want) {
		t.Errorf("the interdiff lists\n\t%+v\nwant the heads' whole diff kept to the touched paths\n\t%+v",
			a.Diff.Files, want)
	}
	one := q
	one.File = "t.txt"
	patch := mustRead(t, lean, interdiffQ(repo, one))
	own := mustRead(t, s, diffQ(repo, gitlet.DiffQuery{Base: first, Head: second, File: "t.txt"}))
	if patch.Diff.Patch == "" || patch.Diff.Patch != own.Diff.Patch {
		t.Errorf("the interdiff's patch of t.txt is %q, want the heads' own %q", patch.Diff.Patch,
			own.Diff.Patch)
	}
	for _, c := range rec.ran() {
		if c.Args[0] == "diff-tree" {
			t.Errorf("the interdiff walked the heads: %v", c.Args)
		}
	}
	for _, in := range rec.fed("cat-file") {
		if lines := strings.Count(in, "\x00"); strings.Contains(in, "z/") || lines > 2*len(touched) {
			t.Errorf("a lookup of the interdiff was fed %d lines (%q), want two a touched path and none of the "+
				"target's", lines, in)
		}
	}
}
