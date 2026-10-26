package query_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/query"
)

// A DIFF'S STAT IS FILTERED BY PATHS HERE, OVER GIT'S WHOLE ANSWER. No path
// reaches git's command line — the stat's one diff-tree is the same process
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

// A FILE'S PATCH NAMES ITS PATHS, AS GIT'S OWN PATCH OF THE COMMITS DOES. The
// person's own `git diff --full-index` of the two commits, for the one file,
// is the oracle, byte for byte: a file modified (git's hunks of its two
// blobs, their header lines naming the paths rather than the blobs), added
// and deleted (hunks written here, the blob's lines under one header, with
// git's `\ No newline at end of file`), made executable alone (no hunk), a
// binary pair, an empty file added, and paths git quotes. The patch's counts
// are its own lines', paged at a small knob it concatenates to the whole, and
// a file the diff does not touch has no patch.
func TestAFilesPatchNamesItsPaths(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	var long strings.Builder
	for i := range 200 {
		long.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	base := x.commit(map[string]string{"mod.txt": long.String(), "gone.txt": "one\ntwo", "run.sh": "echo\n",
		"bin.dat": "a\x00b", "same.txt": "same\n", "nl\nname.txt": "old\n", "q\"uote.txt": "old\n"}, "base")
	if err := os.Remove(filepath.Join(x.work, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(x.work, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(long.String(), "line 7\n", "line seven\n", 1)
	edited = strings.Replace(edited, "line 150\n", "", 1)
	head := x.commit(map[string]string{"mod.txt": edited + "tail", "new.txt": "fresh\nlines\n",
		"bin.dat": "a\x00c", "empty.txt": "", "nl\nname.txt": "new\n", "q\"uote.txt": "new\n"}, "head")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	for _, file := range []string{"mod.txt", "new.txt", "gone.txt", "run.sh", "bin.dat", "empty.txt",
		"nl\nname.txt", "q\"uote.txt"} {
		t.Run(strconv.Quote(file), func(t *testing.T) {
			want := x.git("-c", "core.quotePath=false", "diff", "--no-color", "--full-index", base, head, "--", file)
			a := mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, File: file}))
			d := a.Diff
			if d.Patch != want {
				t.Errorf("the patch of %q =\n%s\nwant git's own\n%s", file, d.Patch, want)
			}
			if len(d.Files) != 1 || d.Total != 1 {
				t.Fatalf("the patch's files %+v total %d, want its one file", d.Files, d.Total)
			}
			added, deleted := 0, 0
			if i := strings.Index(want, "\n@@"); i >= 0 {
				for line := range strings.Lines(want[i+1:]) {
					switch {
					case strings.HasPrefix(line, "+"):
						added++
					case strings.HasPrefix(line, "-"):
						deleted++
					}
				}
			}
			if f := d.Files[0]; f.Added != added || f.Deleted != deleted || f.Binary != (file == "bin.dat") {
				t.Errorf("the patch's file %+v, want +%d -%d binary %v", f, added, deleted, file == "bin.dat")
			}
		})
	}

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
// git's own rename header and hunk.
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
}
