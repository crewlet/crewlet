package query_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/query"
)

// patchID is the person's own git's patch-id, under mode, of the diff from base
// to head written as a revision's facts write it: what the facts are held to.
func (x *fixture) patchID(base, head, mode string) string {
	x.t.Helper()
	diff := x.git("diff-tree", "-r", "-p", "--full-index", "--no-renames", "--no-color", "-U3", base, head)
	out, stderr, err := x.p.Git().Try(x.t, x.work, strings.NewReader(diff), "patch-id", mode)
	if err != nil {
		x.t.Fatalf("git patch-id %s: %v\n%s", mode, err, stderr)
	}
	id, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	return id
}

// numbered is a file of lines "line 1" to "line n", each line i that changed
// names written as it says, under top lines "top 1" to "top <top>".
func numbered(n int, changed map[int]string, top int) string {
	var b strings.Builder
	for i := 1; i <= top; i++ {
		fmt.Fprintf(&b, "top %d\n", i)
	}
	for i := 1; i <= n; i++ {
		if s, ok := changed[i]; ok {
			b.WriteString(s + "\n")
			continue
		}
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

// THE PATCH-ID SEES WHITESPACE. Two changes to one line that differ only in
// how the line is indented — four spaces and a tab, which is Python's
// difference between two programs — are two changes: their facts' ids
// differ, though `--stable` reads the two as one, which is what makes the
// case able to fail. And a fact's id is git's own `patch-id --verbatim` of the
// cumulative diff from the base, which the person's git computes alike.
//
// Mutation: take the id with --stable — the two ids are equal, red.
func TestThePatchIDSeesWhitespace(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{"a.py": "def f():\n    return 1\n"}, "base")
	x.branch("spaces", base)
	spaces := x.p.Commit(x.work, map[string]string{"a.py": "def f():\n    return 2\n"}, "spaces")
	x.branch("tabs", base)
	tabs := x.p.Commit(x.work, map[string]string{"a.py": "def f():\n\treturn 2\n"}, "a tab")
	x.push(codetest.RepoName, "refs/heads/spaces", "refs/heads/tabs")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	if x.patchID(base, spaces, "--stable") != x.patchID(base, tabs, "--stable") {
		t.Fatal("the fixture's two changes differ past their whitespace: --stable reads them apart, and the " +
			"case could not fail")
	}
	sp, tb := x.facts(s, repo, spaces, base), x.facts(s, repo, tabs, base)
	switch {
	case sp.PatchID == "" || tb.PatchID == "" || sp.PatchIDOmitted || tb.PatchIDOmitted:
		t.Fatalf("the facts carry ids %q and %q (omitted %t, %t), want both", sp.PatchID, tb.PatchID,
			sp.PatchIDOmitted, tb.PatchIDOmitted)
	case sp.PatchID == tb.PatchID:
		t.Errorf("a change indented by spaces and one by a tab share the id %s: whitespace is a change", sp.PatchID)
	}
	for _, f := range []*code.RevisionFactsAnswer{sp, tb} {
		if want := x.patchID(base, f.Head, "--verbatim"); f.PatchID != want {
			t.Errorf("the facts of %s carry the id %s, want git's patch-id --verbatim of its diff, %s", f.Head,
				f.PatchID, want)
		}
		if f.MergeBase != base || f.HeadInTarget || !slices.Equal(f.Touched, []string{"a.py"}) || f.TouchesAll {
			t.Errorf("the facts of %s = %+v, want base %s and a.py alone touched", f.Head, f, base)
		}
	}
}

// A PURE REBASE KEEPS ITS PATCH-ID. A change rebased onto a target that moved
// three lines in above it and changed another file is the same change: its
// facts name the new base, and the same id, since an id leaves out the line
// numbers a hunk is at. So is the change NOT rebased, against that target: it
// is measured from where its history meets the target's, never from the
// target's tip. A rebase onto a target that changed a line beside the
// change's own — inside the lines its hunk shows — is a change a reviewer has
// not seen, and its id is another.
//
// Mutation: take the paths and the id of the diff from the target's tip
// rather than from the merge base — the change not rebased then touches the
// target's b.txt and carries another id, red.
func TestAPureRebaseKeepsItsPatchID(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{"a.txt": numbered(40, nil, 0), "b.txt": "b\n"}, "base")
	x.branch("change", base)
	change := map[int]string{30: "line thirty, changed"}
	first := x.p.Commit(x.work, map[string]string{"a.txt": numbered(40, change, 0)}, "the change")
	// Pushed before the rebase replaces it: no ref names it after.
	x.push(codetest.RepoName, "refs/heads/change")

	x.git("checkout", "-q", "main")
	moved := x.commit(map[string]string{"a.txt": numbered(40, nil, 3), "b.txt": "b, moved on\n"}, "the target moves")
	x.git("checkout", "-q", "change")
	x.git("rebase", "-q", "main")
	rebased := x.p.Head(x.work)

	x.branch("near", base)
	near := x.p.Commit(x.work, map[string]string{"a.txt": numbered(40, map[int]string{28: "line 28, edited"}, 0)},
		"a target that edits beside the change")
	x.branch("change-near", first)
	x.git("rebase", "-q", "near")
	nearRebased := x.p.Head(x.work)
	x.push(codetest.RepoName, "refs/heads/change", "refs/heads/near", "refs/heads/change-near")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	f1, f2 := x.facts(s, repo, first, base), x.facts(s, repo, rebased, moved)
	switch {
	case f1.MergeBase != base || f2.MergeBase != moved:
		t.Errorf("the bases are %s and %s, want %s and the moved target %s", f1.MergeBase, f2.MergeBase, base, moved)
	case !slices.Equal(f1.Touched, []string{"a.txt"}) || !slices.Equal(f2.Touched, []string{"a.txt"}):
		t.Errorf("the touched paths are %v and %v, want a.txt alone in each: the target's b.txt is not the "+
			"change's", f1.Touched, f2.Touched)
	case f1.PatchID == "" || f1.PatchID != f2.PatchID:
		t.Errorf("the change and its pure rebase carry the ids %q and %q, want one id", f1.PatchID, f2.PatchID)
	}
	if f3 := x.facts(s, repo, nearRebased, near); f3.PatchID == "" || f3.PatchID == f1.PatchID {
		t.Errorf("the change rebased beside an edit of its context carries the id %q, want another than %q",
			f3.PatchID, f1.PatchID)
	}
	if f4 := x.facts(s, repo, first, moved); f4.MergeBase != base || f4.PatchID != f1.PatchID ||
		!slices.Equal(f4.Touched, []string{"a.txt"}) {
		t.Errorf("the change against the target that moved on without it = %+v, want it measured from %s: a.txt "+
			"alone, and the id %s", f4, base, f1.PatchID)
	}
}

// TOUCHED PATHS PAST THE BOUND TOUCH EVERYTHING. A revision's list of the files
// it touches is whole or not given: at its bounds it is every path, in git's
// order; a path past the bound on its paths, or a byte past the bound on its
// JSON, and the facts say the revision touches all, with no list — never a
// list cut at the bound, which would leave the files past it out of all that
// matches against it — and its id is taken all the same. A path JSON cannot
// carry exactly, bytes that are no UTF-8, touches all too.
//
// Mutation: keep the list past the bound on its paths (drop the count's
// check) — the second row red.
func TestTouchedPathsPastTheBoundTouchEverything(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{"keep.txt": "k\n"}, "base")
	want := []string{"d/f1.txt", "d/f2.txt", "e.txt", "f3.txt", "z/f4.txt"}
	files := map[string]string{}
	for _, p := range want {
		files[p] = p + "\n"
	}
	head := x.commit(files, "five files")
	repo := x.repo(codetest.RepoName)
	cost := code.TouchedCost(want)
	for _, tc := range []struct {
		name         string
		paths, bytes int
		all          bool
	}{
		{"at both bounds", len(want), cost, false},
		{"a path past", len(want) - 1, cost, true},
		{"a byte past", len(want), cost - 1, true},
	} {
		s := x.server(0, query.Options{})
		query.SetTouchedBounds(s, tc.paths, tc.bytes)
		f := x.facts(s, repo, head, base)
		switch {
		case f.TouchesAll != tc.all:
			t.Errorf("%s: touches all = %t, want %t", tc.name, f.TouchesAll, tc.all)
		case tc.all && len(f.Touched) != 0:
			t.Errorf("%s: the facts touch all and list %v, want no list", tc.name, f.Touched)
		case !tc.all && !slices.Equal(f.Touched, want):
			t.Errorf("%s: the facts list %v, want %v", tc.name, f.Touched, want)
		case f.PatchID == "" || f.PatchIDOmitted:
			t.Errorf("%s: the facts carry no id (%q, omitted %t): a list's bound is no patch's", tc.name, f.PatchID,
				f.PatchIDOmitted)
		}
	}

	odd := x.commit(map[string]string{"caf\xe9.txt": "x\n"}, "a path that is no UTF-8")
	if f := x.facts(x.server(0, query.Options{}), repo, odd, head); !f.TouchesAll || len(f.Touched) != 0 {
		t.Errorf("a change to a path that is no UTF-8 lists %v (touches all %t), want it to touch all, with no "+
			"list", f.Touched, f.TouchesAll)
	}
}

// endsAt is a query.Pool that ends the runs holding one argument as gitproc
// ends a run at its deadline, and runs every other on the pool beneath.
type endsAt struct {
	query.Pool
	arg string
}

func (p endsAt) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if !slices.Contains(c.Args, p.arg) {
		return p.Pool.Run(ctx, c)
	}
	return gitproc.Result{Exit: -1, Took: c.Deadline}, fmt.Errorf("gitproc: git %s ran past its %v deadline and "+
		"was killed: %w", c.Args[0], c.Deadline, gitproc.ErrDeadline)
}

// A PATCH-ID PAST ITS BOUND IS OMITTED, AND ONLY IT. A change whose diff is a
// byte past the bound on a diff the facts identify has its id omitted — an id
// that equals nothing, so no approval carries across it — while one at the
// bound has its id; and a patch or an id that ran out the read's budget of git
// is omitted too, the rest of the facts answered: a base and a list of paths
// cost little and a change cannot do without them. Those two fail the read
// as any read's git does, too large where they ran its whole budget.
//
// Mutations: take the id past the bound (drop the spool's check) — the
// byte-past row red; fail the read where the patch ran out its budget — the
// patch row red.
func TestAPatchIDPastItsBoundIsOmitted(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{"a.txt": "one\n"}, "base")
	head := x.commit(map[string]string{"a.txt": "two\n", "b.txt": "added\n"}, "head")
	repo := x.repo(codetest.RepoName)
	size := int64(len(x.git("diff-tree", "-r", "-p", "--full-index", "--no-renames", "--no-color", "-U3", base,
		head)))
	want := x.patchID(base, head, "--verbatim")
	for _, tc := range []struct {
		name  string
		bound int64
		id    string
	}{{"at the bound", size, want}, {"a byte past", size - 1, ""}} {
		s := x.server(0, query.Options{})
		query.SetPatchIDBytes(s, tc.bound)
		f := x.facts(s, repo, head, base)
		if f.PatchID != tc.id || f.PatchIDOmitted != (tc.id == "") ||
			!slices.Equal(f.Touched, []string{"a.txt", "b.txt"}) {
			t.Errorf("%s: the facts = %+v, want the id %q, omitted where it is none, and both paths", tc.name, f,
				tc.id)
		}
	}

	node := x.f.Nodes()[0]
	for _, tc := range []struct {
		name, arg string
		omitted   bool // else too large
	}{
		{"the patch ran out the budget", "-p", true},
		{"its id ran out the budget", "--verbatim", true},
		{"the merge base ran out the budget", "merge-base", false},
		{"the paths' walk ran out the budget", "--name-only", false},
	} {
		s := x.server(0, query.Options{Git: endsAt{Pool: node.Pool(), arg: tc.arg}})
		a, err := s.Read(t.Context(), factsQ(repo, head, base))
		switch {
		case tc.omitted && err != nil:
			t.Errorf("%s: the facts = %v, want them answered with the id omitted", tc.name, err)
		case tc.omitted && (!a.RevisionFacts.PatchIDOmitted || a.RevisionFacts.PatchID != "" ||
			a.RevisionFacts.MergeBase != base || len(a.RevisionFacts.Touched) != 2):
			t.Errorf("%s: the facts = %+v, want the base and both paths, the id omitted", tc.name, a.RevisionFacts)
		case !tc.omitted && !errors.Is(err, code.ErrSliceTooLarge):
			t.Errorf("%s: the facts = %v (%+v), want the read too large", tc.name, err, a.RevisionFacts)
		}
	}
}

// THE BASE IS WHERE THE HISTORIES MEET. A head its target holds already is in
// its target, its base the head itself, and it changes nothing: no path, no
// id. A head whose history shares no commit with its target's has no merge
// base, and its change is measured from the empty tree — every file of the
// head, and git's own id of that diff.
//
// Mutation: answer HeadInTarget from the head equalling the target's tip — an
// ancestor of the tip is read as a change, red.
func TestTheBaseIsWhereTheHistoriesMeet(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	first := x.commit(map[string]string{"a.txt": "1\n"}, "first")
	second := x.commit(map[string]string{"a.txt": "2\n"}, "second")
	x.git("checkout", "-q", "--orphan", "other")
	x.git("rm", "-q", "-r", "-f", ".")
	orphan := x.p.Commit(x.work, map[string]string{"b.txt": "b\n", "c.txt": "c\n"}, "another history")
	x.push(codetest.RepoName, "refs/heads/other")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	if f := x.facts(s, repo, first, second); !f.HeadInTarget || f.MergeBase != first || len(f.Touched) != 0 ||
		f.TouchesAll || f.PatchID != "" || f.PatchIDOmitted {
		t.Errorf("the facts of %s, which its target %s holds = %+v, want it in its target, changing nothing",
			first, second, f)
	}
	f := x.facts(s, repo, orphan, second)
	const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	switch {
	case f.MergeBase != "" || f.HeadInTarget:
		t.Errorf("the facts of a history sharing nothing with its target = %+v, want no merge base", f)
	case !slices.Equal(f.Touched, []string{"b.txt", "c.txt"}):
		t.Errorf("a history sharing nothing touches %v, want every file of its head", f.Touched)
	case f.PatchID != x.patchID(emptyTree, orphan, "--verbatim"):
		t.Errorf("a history sharing nothing carries the id %q, want git's of its diff from the empty tree", f.PatchID)
	}
}

// unnameable is a query.Pool whose walk of a change's touched paths lists one
// ordinary path and then path, as git lists one of a tree a client built with
// git's plumbing rather than from a work tree, and that runs every other
// process on the pool beneath.
type unnameable struct {
	query.Pool
	path string
}

func (p unnameable) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if !slices.Contains(c.Args, "--name-only") {
		return p.Pool.Run(ctx, c)
	}
	if _, err := c.Stdout.Write([]byte("a.txt\x00" + p.path + "\x00")); err != nil {
		return gitproc.Result{}, err
	}
	return gitproc.Result{}, nil
}

// A PATH NO READ COULD NAME BACK TOUCHES ALL: a tree built with git's plumbing
// may hold a path past code.MaxGitPathBytes, which no work tree could, and the
// revise that records a revision's facts refuses a touched list holding one —
// so the facts list no such path, and touch all, as past the list's bounds,
// rather than leave the change due to its home with every revise refused.
//
// Mutation: drop nameable from the walk's stop, and the facts list the path.
func TestAPathNoReadCouldNameTouchesAll(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{"keep.txt": "k\n"}, "base")
	head := x.commit(map[string]string{"a.txt": "a\n"}, "one file")
	repo := x.repo(codetest.RepoName)
	long := strings.Repeat("d/", code.MaxGitPathBytes/2) + "f.txt"
	if _, err := code.GitPath("touched", long, "", false); err == nil {
		t.Fatalf("the premise: a path of %d bytes is one a read by oid names", len(long))
	}
	s := x.server(0, query.Options{Git: unnameable{Pool: x.f.Nodes()[0].Pool(), path: long}})
	if f := x.facts(s, repo, head, base); !f.TouchesAll || len(f.Touched) != 0 {
		t.Fatalf("the facts of a change touching a path no read names list %d paths (touches all %t), want "+
			"them to touch all, with no list", len(f.Touched), f.TouchesAll)
	}
}
