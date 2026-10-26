package query_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/query"
)

// A DIFF'S STAT IS FILTERED BY PATHS HERE, OVER GIT'S WHOLE RAW ANSWER. No
// path reaches git's command line — the stat's walk is the same diff-tree
// whatever the filter — and the filter admits a file equal to one of its
// paths or under one as a directory, on either side of a rename. The files,
// their statuses, ids and counts are git's own (the person's git's numstat of
// the same commits is the oracle); the totals are over every file the filter
// admits; and paged at a small knob, the pages concatenate to the stat, each
// carrying the totals.
func TestADiffStatIsFilteredByPathsInGo(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{"a/x": "1\n2\n3\n", "a/y": "y\n", "b/z": "z\n", "c": "c\n",
		"ab/w": "w\n"}, "base")
	if err := os.Remove(filepath.Join(x.work, "c")); err != nil {
		t.Fatal(err)
	}
	head := x.commit(map[string]string{"a/x": "1\nTWO\n3\nfour\n", "b/z": "zz\n", "a/new": "n\n", "ab/w": "W\n"},
		"head")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{Git: rec.on(x, 0)})

	numstat := numstatOf(t, x, base, head)
	for _, tc := range []struct {
		name  string
		paths []string
		want  []string
	}{
		{"none", nil, []string{"a/new", "a/x", "ab/w", "b/z", "c"}},
		{"a directory", []string{"a"}, []string{"a/new", "a/x"}},
		{"a file", []string{"b/z"}, []string{"b/z"}},
		{"two", []string{"a/x", "c"}, []string{"a/x", "c"}},
		{"nothing changed there", []string{"a/y"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, Paths: tc.paths}))
			d := a.Diff
			var got []string
			added, deleted := 0, 0
			for _, f := range d.Files {
				got = append(got, f.Path)
				n := numstat[f.Path]
				if f.Added != n[0] || f.Deleted != n[1] {
					t.Errorf("%s: +%d -%d, want git's +%d -%d", f.Path, f.Added, f.Deleted, n[0], n[1])
				}
				added, deleted = added+n[0], deleted+n[1]
			}
			if !slices.Equal(got, tc.want) || d.Total != len(tc.want) || d.Added != added || d.Deleted != deleted {
				t.Errorf("filter %v: files %v total %d +%d -%d, want %v total %d +%d -%d", tc.paths, got, d.Total,
					d.Added, d.Deleted, tc.want, len(tc.want), added, deleted)
			}
			if d.Base != base || d.Head != head {
				t.Errorf("base %s head %s, want %s %s", d.Base, d.Head, base, head)
			}
		})
	}

	t.Run("statuses and ids", func(t *testing.T) {
		a := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head}))
		byPath := map[string]code.DiffFile{}
		for _, f := range a.Diff.Files {
			byPath[f.Path] = f
		}
		blob := func(rev, p string) string { return strings.TrimSpace(x.git("rev-parse", rev+":"+p)) }
		for p, want := range map[string]code.DiffFile{
			"a/new": {Status: "A", NewMode: "100644", NewOID: blob(head, "a/new")},
			"c":     {Status: "D", OldMode: "100644", OldOID: blob(base, "c")},
			"a/x":   {Status: "M", OldMode: "100644", NewMode: "100644", OldOID: blob(base, "a/x"), NewOID: blob(head, "a/x")},
		} {
			got := byPath[p]
			got.Path, got.Added, got.Deleted = "", 0, 0
			if got != want {
				t.Errorf("%s = %+v, want %+v", p, got, want)
			}
		}
	})

	t.Run("no path reaches git", func(t *testing.T) {
		for _, c := range rec.ran() {
			if c.Args[0] != "diff-tree" {
				continue
			}
			for _, arg := range c.Args {
				if strings.Contains(arg, "a/") || arg == "a" || arg == "c" || arg == "b/z" {
					t.Errorf("diff-tree was handed %q: %q", arg, c.Args)
				}
			}
		}
	})

	t.Run("paged", func(t *testing.T) {
		q := diffQ(repo, code.DiffQuery{Base: base, Head: head})
		limit := 700 + x.decoration(q)
		paged := x.server(0, query.Options{})
		query.SetPageBytes(paged, limit)
		ps := pages(t, paged, q, limit)
		var got []string
		for _, p := range ps {
			for _, f := range p.Diff.Files {
				got = append(got, f.Path)
			}
			if p.Diff.Total != 5 {
				t.Errorf("a page's total %d, want the whole diff's 5", p.Diff.Total)
			}
		}
		if len(ps) < 2 || !slices.Equal(got, []string{"a/new", "a/x", "ab/w", "b/z", "c"}) {
			t.Errorf("%d pages of %v, want several pages of every file once", len(ps), got)
		}
	})
}

// A FILTERED STAT COUNTS ONLY THE FILES ITS PATHS ADMIT. What a stat costs is
// its line counts — git diffs the text of every pair it counts — so a stat
// naming paths has git count the files they admit and no other: the walk of
// the two trees (diff-tree's raw records, which count nothing) is filtered
// here, and the admitted records alone go to ONE diff-pairs on its standard
// input, so a stat of one directory of a wide diff costs git that directory's
// diffs rather than the whole diff's. The counts are git's own — the person's
// numstat of the same commits is the oracle — a stat naming no paths is still
// the one diff-tree that writes both halves, and a filter admitting nothing
// counts nothing at all.
//
// Mutation: count the stat's lines over every record the walk read — git is
// handed files the paths do not admit, red.
func TestAFilteredStatCountsOnlyTheFilesItAdmits(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{"src/a.go": "a\n", "src/b.go": "b\nb\n", "srcs/c.go": "c\n",
		"docs/x.md": "x\n", "top.txt": "t\n"}, "base")
	head := x.commit(map[string]string{"src/a.go": "A\n", "src/b.go": "b\nB\nb\n", "srcs/c.go": "C\n",
		"docs/x.md": "X\n", "top.txt": "T\nT\n"}, "head")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	numstat := numstatOf(t, x, base, head)

	before := rec.count()
	d := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, Paths: []string{"src", "top.txt"}})).Diff
	var got []string
	added, deleted := 0, 0
	for _, f := range d.Files {
		got = append(got, f.Path)
		if n := numstat[f.Path]; f.Added != n[0] || f.Deleted != n[1] {
			t.Errorf("%s: +%d -%d, want git's +%d -%d", f.Path, f.Added, f.Deleted, n[0], n[1])
		}
		added, deleted = added+numstat[f.Path][0], deleted+numstat[f.Path][1]
	}
	want := []string{"src/a.go", "src/b.go", "top.txt"}
	if !slices.Equal(got, want) || d.Total != 3 || d.Added != added || d.Deleted != deleted {
		t.Errorf("the stat of src and top.txt: %v total %d +%d -%d, want %v total 3 +%d -%d", got, d.Total, d.Added,
			d.Deleted, want, added, deleted)
	}
	counting := 0
	for _, r := range rec.since(before) {
		if !slices.Contains(r.cmd.Args, "--numstat") {
			continue
		}
		counting++
		if r.cmd.Args[0] != "diff-pairs" {
			t.Errorf("a filtered stat counted lines with git %q, which counts every file of the diff", r.cmd.Args)
			continue
		}
		if fed := fedPaths(t, r.stdin); !slices.Equal(fed, want) {
			t.Errorf("diff-pairs counted %v, want only the files the paths admit, %v", fed, want)
		}
	}
	if counting != 1 {
		t.Errorf("a filtered stat ran %d processes that count lines, want one diff-pairs", counting)
	}

	t.Run("no paths: one diff-tree writes both halves", func(t *testing.T) {
		before := rec.count()
		if d := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head})).Diff; d.Total != 5 {
			t.Errorf("the whole stat's total %d, want the diff's 5", d.Total)
		}
		var trees int
		for _, r := range rec.since(before) {
			switch r.cmd.Args[0] {
			case "diff-pairs":
				t.Errorf("a stat naming no paths ran git %q beside its diff-tree", r.cmd.Args)
			case "diff-tree":
				trees++
				if !slices.Contains(r.cmd.Args, "--numstat") {
					t.Errorf("a stat naming no paths ran git %q, which counts nothing", r.cmd.Args)
				}
			}
		}
		if trees != 1 {
			t.Errorf("a stat naming no paths ran %d diff-trees, want one", trees)
		}
	})

	t.Run("nothing admitted: nothing counted", func(t *testing.T) {
		before := rec.count()
		d := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, Paths: []string{"src/a"}})).Diff
		if len(d.Files) != 0 || d.Total != 0 || d.Added != 0 || d.Deleted != 0 {
			t.Errorf("a stat whose paths admit nothing = %+v, want nothing", d)
		}
		for _, r := range rec.since(before) {
			if r.cmd.Args[0] == "diff-pairs" {
				t.Errorf("a stat whose paths admit nothing ran git %q", r.cmd.Args)
			}
		}
	})
}

// A STAT THE DEADLINE ENDS NAMES WHAT NARROWS IT. A stat's process the read's
// deadline ended is code.ErrSliceTooLarge naming the narrowing — never
// unready, since another node would take as long — and the narrowing named is
// one that does narrow it: a stat naming no paths is told to name some, since
// a stat that names them counts the files they admit and no other; a
// filtered stat whose count ran out, to name fewer; a filtered stat's walk of
// the two trees, which no path narrows, to diff commits closer together; and
// a comparison, whose stat is of its merge base and head, to diff those two
// naming paths.
//
// Mutation: refuse every stat "diff commits closer together", as the stat did
// before it counted only what its paths admit — the stat naming no paths and
// the comparison are red.
func TestAStatTheDeadlineEndsNamesWhatNarrowsIt(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{"src/a.go": "a\n", "docs/x.md": "x\n"}, "base")
	head := x.commit(map[string]string{"src/a.go": "A\n", "docs/x.md": "X\n"}, "head")
	repo := x.repo(codetest.RepoName)
	counts := func(args []string) bool { return args[0] == "diff-tree" && slices.Contains(args, "--numstat") }
	walks := func(args []string) bool { return args[0] == "diff-tree" && !slices.Contains(args, "--numstat") }
	pairs := func(args []string) bool { return args[0] == "diff-pairs" }
	src := []string{"src"}
	for _, tc := range []struct {
		name string
		// ends is the process the deadline ends.
		ends func(args []string) bool
		q    code.GitQuery
		want string
	}{
		{"a stat naming no paths", counts, diffQ(repo, code.DiffQuery{Base: base, Head: head}),
			": name paths, or diff commits closer together"},
		{"a filtered stat's count", pairs, diffQ(repo, code.DiffQuery{Base: base, Head: head, Paths: src}),
			": name fewer paths, or diff commits closer together"},
		{"a filtered stat's walk", walks, diffQ(repo, code.DiffQuery{Base: base, Head: head, Paths: src}),
			": diff commits closer together"},
		{"a comparison", counts, compareQ(repo, base, head),
			": diff the merge base and head naming paths, or compare commits closer together"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := x.server(0, query.Options{Git: deadlined{Pool: x.f.Nodes()[0].Pool(), ends: tc.ends}})
			_, err := s.Read(t.Context(), tc.q)
			if !errors.Is(err, code.ErrSliceTooLarge) || !strings.HasSuffix(err.Error(), tc.want) {
				t.Errorf("a read whose deadline ended = %v, want code.ErrSliceTooLarge ending %q", err, tc.want)
			}
		})
	}
}

// deadlined is a query.Pool on which every process ends picks answers as one
// its deadline ended does — gitproc.ErrDeadline, having run nothing — and
// every other runs on the pool beneath.
type deadlined struct {
	query.Pool
	ends func(args []string) bool
}

func (d deadlined) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if d.ends(c.Args) {
		return gitproc.Result{}, fmt.Errorf("git %s ran past its deadline: %w", c.Args[0], gitproc.ErrDeadline)
	}
	return d.Pool.Run(ctx, c)
}

// fedPaths is the paths of the raw records in, -z's form: each record's path,
// or a rename's two, in the order fed.
func fedPaths(t *testing.T, in string) []string {
	t.Helper()
	recs := strings.Split(strings.TrimSuffix(in, "\x00"), "\x00")
	var out []string
	for i := 0; i < len(recs); {
		f := strings.Fields(recs[i])
		if !strings.HasPrefix(recs[i], ":") || len(f) != 5 {
			t.Fatalf("diff-pairs was fed %q, which is no raw record", recs[i])
		}
		n := 1
		if f[4][0] == 'R' || f[4][0] == 'C' {
			n = 2
		}
		if i+1+n > len(recs) {
			t.Fatalf("diff-pairs was fed a record of %q without its paths", recs[i])
		}
		out = append(out, recs[i+1:i+1+n]...)
		i += 1 + n
	}
	return out
}

// numstatOf is the person's git's numstat of base..head: added and deleted by
// path.
func numstatOf(t *testing.T, x *fixture, base, head string) map[string][2]int {
	t.Helper()
	out := map[string][2]int{}
	for line := range strings.Lines(x.git("diff-tree", "-r", "--numstat", base, head)) {
		f := strings.Fields(line)
		added, _ := strconv.Atoi(f[0])
		deleted, _ := strconv.Atoi(f[1])
		out[f[2]] = [2]int{added, deleted}
	}
	return out
}

// on is the recording standing in front of the i-th git node's pool.
func (r *recording) on(x *fixture, i int) query.Pool {
	r.Pool = x.f.Nodes()[i].Pool()
	return r
}

// A FILE'S PATCH IS GIT'S OWN PATCH OF THE COMMITS FOR IT. The person's own
// `git diff --full-index` of the two commits, for the one file, is the oracle,
// byte for byte: a file modified (hunks under a header naming the paths),
// added and deleted (against /dev/null, with git's `\ No newline at end of
// file`), made executable alone (no hunk), a binary pair and a binary file
// added (git's one line), an empty file added, a file made a symbolic link
// (the deletion and the creation git prints for a change of type), paths git
// quotes and a UTF-8 path it writes as it is, and a submodule added, moved to
// another commit and removed. The patch's counts are its own lines', paged at
// a small knob it concatenates to the whole, and a file the diff does not
// touch has no patch.
func TestAFilesPatchNamesItsPaths(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	var long strings.Builder
	for i := range 200 {
		long.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	base := x.commit(map[string]string{"mod.txt": long.String(), "gone.txt": "one\ntwo", "run.sh": "echo\n",
		"bin.dat": "a\x00b", "same.txt": "same\n", "nl\nname.txt": "old\n", "q\"uote.txt": "old\n",
		"kind.txt": "a file\n", "é.txt": "old\n"}, "base")
	if err := os.Remove(filepath.Join(x.work, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(x.work, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(x.work, "kind.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("mod.txt", filepath.Join(x.work, "kind.txt")); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(long.String(), "line 7\n", "line seven\n", 1)
	edited = strings.Replace(edited, "line 150\n", "", 1)
	head := x.commit(map[string]string{"mod.txt": edited + "tail", "new.txt": "fresh\nlines\n",
		"bin.dat": "a\x00c", "newbin.dat": "x\x00y", "empty.txt": "", "nl\nname.txt": "new\n", "q\"uote.txt": "new\n",
		"é.txt": "new\n"}, "head")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	// held is the patch of file between from and to, held to git's own.
	held := func(t *testing.T, from, to, file string, binary bool) {
		t.Helper()
		want := x.git("-c", "core.quotePath=false", "diff", "--no-color", "--full-index", from, to, "--", file)
		a := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: from, Head: to, File: file}))
		d := a.Diff
		if d.Patch != want {
			t.Errorf("the patch of %q =\n%s\nwant git's own\n%s", file, d.Patch, want)
		}
		if len(d.Files) != 1 || d.Total != 1 {
			t.Fatalf("the patch's files %+v total %d, want its one file", d.Files, d.Total)
		}
		added, deleted, hunks := 0, 0, false
		for line := range strings.Lines(want) {
			switch {
			case strings.HasPrefix(line, "diff --git "):
				hunks = false
			case strings.HasPrefix(line, "@@"):
				hunks = true
			case hunks && strings.HasPrefix(line, "+"):
				added++
			case hunks && strings.HasPrefix(line, "-"):
				deleted++
			}
		}
		if f := d.Files[0]; f.Added != added || f.Deleted != deleted || f.Binary != binary {
			t.Errorf("the patch's file %+v, want +%d -%d binary %v", f, added, deleted, binary)
		}
	}
	for _, file := range []string{"mod.txt", "new.txt", "gone.txt", "run.sh", "bin.dat", "newbin.dat",
		"empty.txt", "kind.txt", "nl\nname.txt", "q\"uote.txt", "é.txt"} {
		t.Run(strconv.Quote(file), func(t *testing.T) {
			held(t, base, head, file, file == "bin.dat" || file == "newbin.dat")
		})
	}

	t.Run("a submodule", func(t *testing.T) {
		gitlink := func(commit, message string) string {
			x.git("update-index", "--add", "--cacheinfo", "160000,"+commit+",sub")
			x.git("commit", "-q", "-m", message)
			x.push(codetest.RepoName, "main")
			return strings.TrimSpace(x.git("rev-parse", "HEAD"))
		}
		added := gitlink(base, "a submodule")
		moved := gitlink(head, "the submodule moved")
		x.git("rm", "-q", "--cached", "sub")
		x.git("commit", "-q", "-m", "the submodule removed")
		x.push(codetest.RepoName, "main")
		removed := strings.TrimSpace(x.git("rev-parse", "HEAD"))
		held(t, head, added, "sub", false)
		held(t, added, moved, "sub", false)
		held(t, moved, removed, "sub", false)
	})

	t.Run("paged", func(t *testing.T) {
		q := diffQ(repo, code.DiffQuery{Base: base, Head: head, File: "mod.txt"})
		limit := 1<<10 + x.decoration(q)
		paged := x.server(0, query.Options{})
		query.SetPageBytes(paged, limit)
		ps := pages(t, paged, q, limit)
		want := x.git("diff", "--no-color", "--full-index", base, head, "--", "mod.txt")
		if len(ps) < 2 || joined(ps) != want {
			t.Errorf("%d pages concatenate to\n%s\nwant several of\n%s", len(ps), joined(ps), want)
		}
	})

	t.Run("untouched", func(t *testing.T) {
		a := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, File: "same.txt"}))
		if a.Diff.Patch != "" || len(a.Diff.Files) != 0 || a.Diff.Total != 0 {
			t.Errorf("the patch of a file the diff does not touch = %+v, want none", a.Diff)
		}
	})
}

// A RENAME IS FOUND WHEN ASKED, AND ONLY THEN. Without Renames a file moved
// with an edit is one deleted and one added, as diff-tree's plumbing answers;
// with it, one renamed file naming where it was, its similarity, and its patch
// git's own rename header and hunk — and a stat filtered by where the file was
// still finds it, its counts the rename's.
func TestARenameIsFoundWhenAsked(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	var body strings.Builder
	for i := range 40 {
		body.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	base := x.commit(map[string]string{"old/name.go": body.String()}, "base")
	if err := os.RemoveAll(filepath.Join(x.work, "old")); err != nil {
		t.Fatal(err)
	}
	head := x.commit(map[string]string{"new/name.go": body.String() + "one more\n"}, "moved")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	plain := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head})).Diff
	if len(plain.Files) != 2 || plain.Files[0].Status != "A" || plain.Files[1].Status != "D" {
		t.Errorf("without renames %+v, want new/name.go added and old/name.go deleted", plain.Files)
	}
	moved := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, Renames: true})).Diff
	if len(moved.Files) != 1 {
		t.Fatalf("with renames %+v, want one file", moved.Files)
	}
	f := moved.Files[0]
	if f.Status != "R" || f.Path != "new/name.go" || f.OldPath != "old/name.go" || f.Similarity < 90 ||
		f.Added != 1 || f.Deleted != 0 {
		t.Errorf("the renamed file %+v, want R new/name.go from old/name.go, +1", f)
	}
	patch := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, Renames: true,
		File: "new/name.go"})).Diff.Patch
	want := x.git("diff", "--no-color", "--full-index", "-M", base, head, "--", "old/name.go", "new/name.go")
	if patch != want {
		t.Errorf("the rename's patch =\n%s\nwant git's own\n%s", patch, want)
	}
	filtered := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, Renames: true,
		Paths: []string{"old"}})).Diff
	if len(filtered.Files) != 1 || filtered.Files[0] != f || filtered.Total != 1 || filtered.Added != 1 ||
		filtered.Deleted != 0 {
		t.Errorf("filtered by where it was %+v, want the renamed file %+v and its +1", filtered, f)
	}
}
