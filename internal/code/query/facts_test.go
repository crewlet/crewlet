package query_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
		case f.Classes.Complete == tc.all:
			// A LIST CLASSES EVERY PATH IT NAMES — none here is Go — and one
			// that touches all names none, so its classes are never whole.
			t.Errorf("%s: the classes are complete = %t, want %t", tc.name, f.Classes.Complete, !tc.all)
		case f.BaseTree == "" || f.HeadTree == "":
			t.Errorf("%s: the facts name trees %q and %q, want both, whatever the list", tc.name, f.BaseTree,
				f.HeadTree)
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
		{"the paths' walk ran out the budget", "--raw", false},
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
	if c.Args[0] != "diff-tree" || !slices.Contains(c.Args, "--raw") {
		return p.Pool.Run(ctx, c)
	}
	header := ":100644 100644 " + strings.Repeat("1", 40) + " " + strings.Repeat("2", 40) + " M\x00"
	if _, err := c.Stdout.Write([]byte(header + "a.txt\x00" + header + p.path + "\x00")); err != nil {
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

// commitStaged writes files into the work tree, runs stage for what a
// written file cannot say — a mode, a deletion, a link — stages every change,
// then commits and pushes main through the first git node, answering the
// commit.
func (x *fixture) commitStaged(files map[string]string, stage func(), message string) string {
	x.t.Helper()
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(x.work, rel), []byte(content), 0o644); err != nil {
			x.t.Fatalf("write %s: %v", rel, err)
		}
	}
	if stage != nil {
		stage()
	}
	x.git("add", "-A")
	x.git("commit", "-q", "-m", message)
	x.push(codetest.RepoName, "main")
	return x.p.Head(x.work)
}

// link points the work tree's link at target, replacing it.
func (x *fixture) link(link, target string) {
	x.t.Helper()
	path := filepath.Join(x.work, link)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		x.t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		x.t.Fatal(err)
	}
}

// classesOf is what f's classes say of each path it touches, by path: its
// class, and nothing for a path of none.
func classesOf(f *code.RevisionFactsAnswer) map[string]code.ChangeClass {
	out := map[string]code.ChangeClass{}
	for _, at := range f.Classes.WhitespaceOnly {
		out[f.Touched[at]] = code.ChangeClassWhitespaceOnly
	}
	for _, at := range f.Classes.CommentOnly {
		out[f.Touched[at]] = code.ChangeClassCommentOnly
	}
	return out
}

// A GO CHANGE IS CLASSED FROM ITS TWO VERSIONS. A change touching four files
// — one only reindented, one whose doc comment alone is rewritten, one whose
// code changes, and a text file — has the first classed whitespace_only and
// the second comment_only, each by its place in the touched list and in the
// list of its narrowest class, the third and the text file classed nothing,
// and its classification complete. Its facts name the trees of its base and
// its head, as git names them: what tells two revisions holding the same
// code apart from two whose diffs merely look alike.
//
// Mutations: lex each path's head against itself — the code change reads as
// whitespace, red; name the head's tree as the base's — the base tree is not
// git's, red.
func TestAGoChangeIsClassedFromItsTwoVersions(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{
		"a.go":  "package p\n\nfunc a() int {\n\treturn 1\n}\n",
		"b.go":  "package p\n\n// b answers two.\nfunc b() int { return 2 }\n",
		"c.go":  "package p\n\nfunc c() int { return 3 }\n",
		"d.txt": "d\n",
	}, "base")
	head := x.commit(map[string]string{
		"a.go":  "package p\n\nfunc a() int {\n        return 1\n}\n",
		"b.go":  "package p\n\n// b answers two, whatever it is asked.\nfunc b() int { return 2 }\n",
		"c.go":  "package p\n\nfunc c() int { return 4 }\n",
		"d.txt": "e\n",
	}, "head")
	repo := x.repo(codetest.RepoName)
	f := x.facts(x.server(0, query.Options{}), repo, head, base)

	want := map[string]code.ChangeClass{"a.go": code.ChangeClassWhitespaceOnly, "b.go": code.ChangeClassCommentOnly}
	switch got := classesOf(f); {
	case !slices.Equal(f.Touched, []string{"a.go", "b.go", "c.go", "d.txt"}):
		t.Fatalf("the change touches %v, want its four files", f.Touched)
	case !reflect.DeepEqual(got, want):
		t.Errorf("the change's paths are classed %v, want %v", got, want)
	case !reflect.DeepEqual(f.Classes, code.RevisionClasses{WhitespaceOnly: []int{0}, CommentOnly: []int{1},
		Complete: true}):
		t.Errorf("the classes = %+v, want a.go's place whitespace only, b.go's comment only, and complete",
			f.Classes)
	}
	if want := strings.TrimSpace(x.git("rev-parse", base+"^{tree}")); f.BaseTree != want {
		t.Errorf("the facts name the base's tree %s, want git's %s", f.BaseTree, want)
	}
	if want := strings.TrimSpace(x.git("rev-parse", head+"^{tree}")); f.HeadTree != want {
		t.Errorf("the facts name the head's tree %s, want git's %s", f.HeadTree, want)
	}
}

// A MODE CHANGE IS NEVER CLASSED, and neither is any path a lexer has no two
// versions of a Go file for: a Go file whose comment changed beside its
// executable bit (a change no lexer sees, to whether it runs), one added, one
// deleted, and a symbolic link named like a Go file whose target moved by a
// space — a blob that lexes as whitespace, which is no Go source — each has
// no class, while a Go file whose comment alone changed beside them is
// comment_only, so the classification ran, and is complete.
//
// Mutation: class a path whatever its modes (drop the mode terms of
// lexableEntry) — m.go reads comment_only and link.go whitespace_only, red.
func TestAModeChangeIsNeverClassed(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commitStaged(map[string]string{
		"c.go":    "package p\n\n// c one\nfunc c() {}\n",
		"gone.go": "package p\n\nfunc gone() {}\n",
		"m.go":    "package p\n\n// m one\nfunc m() {}\n",
	}, func() { x.link("link.go", "a.go") }, "base")
	head := x.commitStaged(map[string]string{
		"c.go": "package p\n\n// c two\nfunc c() {}\n",
		"m.go": "package p\n\n// m two\nfunc m() {}\n",
		"n.go": "package p\n\n// n\nfunc n() {}\n",
	}, func() {
		x.link("link.go", "a.go ")
		if err := os.Remove(filepath.Join(x.work, "gone.go")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(x.work, "m.go"), 0o755); err != nil {
			t.Fatal(err)
		}
	}, "head")
	repo := x.repo(codetest.RepoName)
	f := x.facts(x.server(0, query.Options{}), repo, head, base)

	if raw := x.git("diff-tree", "-r", "--raw", "--no-renames", base, head); !strings.Contains(raw,
		":100644 100755 ") || !strings.Contains(raw, ":120000 120000 ") {
		t.Fatalf("the fixture changed no mode and no link:\n%s", raw)
	}
	want := map[string]code.ChangeClass{"c.go": code.ChangeClassCommentOnly}
	switch got := classesOf(f); {
	case !slices.Equal(f.Touched, []string{"c.go", "gone.go", "link.go", "m.go", "n.go"}):
		t.Fatalf("the change touches %v, want its five paths", f.Touched)
	case !reflect.DeepEqual(got, want):
		t.Errorf("the change's paths are classed %v, want only %v: a mode change, an added or deleted file "+
			"and a link are never classed", got, want)
	case !f.Classes.Complete:
		t.Error("the classification is incomplete, want every path judged")
	}
}

// A BOUND CUTS CLASSIFICATION INCOMPLETE. Three Go files whose comments alone
// change are lexed in the touched list's order while the Go they take stays
// within the bound in all: at the bound every one is classed and the
// classification complete; a byte short and the last is never read — its
// class unknown, not none — so the classes say they are INCOMPLETE, the first
// two classed all the same. A version past the bound on one file leaves that
// file unread with no class, as the lexer gives none past it, and the rest
// whole. And a classification that runs out what the read's budget of git
// left is answered incomplete, the facts' base, paths, trees and id with it:
// a class only relaxes, so classes cut short never fail the facts.
//
// Mutations: class past the bound (drop the total's check) — the byte-short
// row is complete, red; fail the read where the lexing ran out the budget —
// the last row red.
func TestABoundCutsClassificationIncomplete(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	before := map[string]string{
		"a.go": "package p\n\n// a\nfunc a() {}\n",
		"b.go": "package p\n\n// b, the longest of the three files by far\nfunc b() {}\n",
		"c.go": "package p\n\n// c\nfunc c() {}\n",
	}
	after := map[string]string{}
	total := 0
	for p, src := range before {
		after[p] = strings.Replace(src, "// ", "// so said ", 1)
		total += len(src) + len(after[p])
	}
	base := x.commit(before, "base")
	head := x.commit(after, "head")
	repo := x.repo(codetest.RepoName)
	for _, tc := range []struct {
		name        string
		blob, bytes int64
		classed     []int
		complete    bool
	}{
		{"at the bound", code.MaxClassedBlobBytes, int64(total), []int{0, 1, 2}, true},
		{"a byte short", code.MaxClassedBlobBytes, int64(total - 1), []int{0, 1}, false},
		{"a version past its bound", int64(len(after["b.go"]) - 1), code.MaxClassedBytes, []int{0, 2}, true},
	} {
		s := x.server(0, query.Options{})
		query.SetClassedBounds(s, tc.blob, tc.bytes)
		f := x.facts(s, repo, head, base)
		if !slices.Equal(f.Classes.CommentOnly, tc.classed) || len(f.Classes.WhitespaceOnly) != 0 ||
			f.Classes.Complete != tc.complete {
			t.Errorf("%s: the classes = %+v, want %v comment only, complete = %t", tc.name, f.Classes,
				tc.classed, tc.complete)
		}
	}

	s := x.server(0, query.Options{Git: endsAt{Pool: x.f.Nodes()[0].Pool(), arg: "--batch"}})
	a, err := s.Read(t.Context(), factsQ(repo, head, base))
	switch {
	case err != nil:
		t.Fatalf("the facts of a classification out of the budget = %v, want them answered", err)
	case !a.RevisionFacts.Classes.IsZero():
		t.Errorf("a classification out of the budget answered %+v, want no class and incomplete",
			a.RevisionFacts.Classes)
	case a.RevisionFacts.PatchID == "" || len(a.RevisionFacts.Touched) != 3 || a.RevisionFacts.HeadTree == "":
		t.Errorf("a classification out of the budget answered the facts %+v, want the rest of them whole",
			a.RevisionFacts)
	}
}
