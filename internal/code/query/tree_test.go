package query_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/query"
)

// A TREE IS LISTED BY ITS ID, AS GIT LISTS IT. The root of a commit, one of
// its directories, the same recursively, and a tree named by its own id or an
// abbreviation of it: every entry once, in git's own order and with git's own
// modes, ids and sizes — the person's own git listing the same objects is the
// oracle — and paged at a 1 KiB knob, the pages concatenate to the listing,
// each within its bound and each carrying the whole listing's Total. A path
// naming a file, or nothing, is no directory.
func TestATreeIsListedByItsOid(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	files := map[string]string{"README.md": "hello\n", "src/a.go": "package a\n", "src/deep/b.go": "package b\n",
		"docs/x.md": "x\n", "run.sh": "#!/bin/sh\n"}
	for i := range 40 {
		files[fmt.Sprintf("many/file-with-a-long-name-%03d.txt", i)] = strconv.Itoa(i) + "\n"
	}
	x.commit(files, "one")
	if err := os.Chmod(filepath.Join(x.work, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	head := x.commit(nil, "executable")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	for _, tc := range []struct {
		name string
		q    code.TreeQuery
		git  []string
		dir  string
	}{
		{"the root", code.TreeQuery{Rev: head}, []string{head}, ""},
		{"a directory", code.TreeQuery{Rev: head, Path: "src"}, []string{head + ":src"}, "src"},
		{"recursively", code.TreeQuery{Rev: head, Recursive: true}, []string{"-r", head}, ""},
		{"a directory, recursively", code.TreeQuery{Rev: head, Path: "src", Recursive: true},
			[]string{"-r", head + ":src"}, "src"},
		{"by an abbreviation", code.TreeQuery{Rev: head[:9], Path: "many"}, []string{head + ":many"}, "many"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := lsTree(t, x, tc.dir, tc.git...)
			a := mustRead(t, s, treeQ(repo, tc.q))
			if a.Kind != code.GitTree || a.Tree == nil || a.Rev != head {
				t.Fatalf("answer %+v, want a listing of %s", a, head)
			}
			if got := a.Tree.Entries; !equalEntries(got, want) {
				t.Errorf("entries =\n%v\nwant git's own\n%v", got, want)
			}
			if a.Tree.Total != len(want) || a.Next != "" {
				t.Errorf("total %d next %q, want %d and no next page", a.Tree.Total, a.Next, len(want))
			}
			if a.Tree.Path != tc.dir {
				t.Errorf("path %q, want %q", a.Tree.Path, tc.dir)
			}
		})
	}

	t.Run("by the tree's own id", func(t *testing.T) {
		treeID := strings.TrimSpace(x.git("rev-parse", head+":src"))
		a := mustRead(t, s, treeQ(repo, code.TreeQuery{Rev: treeID}))
		if a.Tree.Tree != treeID || !equalEntries(a.Tree.Entries, lsTree(t, x, "", treeID)) {
			t.Errorf("the listing of tree %s = %+v", treeID, a.Tree)
		}
	})

	t.Run("paged at 1 KiB", func(t *testing.T) {
		paged := x.server(0, query.Options{})
		query.SetPageBytes(paged, 1<<10)
		want := lsTree(t, x, "", "-r", head)
		var got []code.TreeEntry
		ps := pages(t, paged, treeQ(repo, code.TreeQuery{Rev: head, Recursive: true}), 1<<10)
		for _, p := range ps {
			got = append(got, p.Tree.Entries...)
			if p.Tree.Total != len(want) {
				t.Errorf("a page's total %d, want the whole listing's %d", p.Tree.Total, len(want))
			}
		}
		if len(ps) < 3 || !equalEntries(got, want) {
			t.Errorf("%d pages listing %d entries, want several pages listing git's %d once each, in order",
				len(ps), len(got), len(want))
		}
	})

	t.Run("a path that is no directory", func(t *testing.T) {
		for _, p := range []string{"README.md", "nothing/here"} {
			_, err := s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: head, Path: p}))
			if !errors.Is(err, code.ErrNotATree) || !strings.Contains(err.Error(), p) {
				t.Errorf("listing %q = %v, want ErrNotATree naming it", p, err)
			}
		}
	})

	t.Run("a cursor no listing issued", func(t *testing.T) {
		for _, c := range []string{"e:0", "e:07", "e:-1", "l:3", "e:999"} {
			q := treeQ(repo, code.TreeQuery{Rev: head})
			q.Cursor = c
			if _, err := s.Read(t.Context(), q); !errors.Is(err, code.ErrBadCursor) {
				t.Errorf("cursor %q = %v, want ErrBadCursor", c, err)
			}
		}
	})
}

// lsTree is the person's git's own listing of args, the paths made the
// repository's under dir.
func lsTree(t *testing.T, x *fixture, dir string, args ...string) []code.TreeEntry {
	t.Helper()
	out := x.git(append([]string{"ls-tree", "-z", "-l"}, args...)...)
	var entries []code.TreeEntry
	for rec := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, _ := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		e := code.TreeEntry{Mode: f[0], Type: f[1], OID: f[2], Path: path}
		if dir != "" {
			e.Path = dir + "/" + path
		}
		if f[3] != "-" {
			e.Size, _ = strconv.ParseInt(f[3], 10, 64)
		}
		entries = append(entries, e)
	}
	return entries
}

// equalEntries reports a and b equal, entry for entry.
func equalEntries(a, b []code.TreeEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// joined is every page's text, in order.
func joined(ps []code.GitAnswer) string {
	var b bytes.Buffer
	for _, p := range ps {
		switch {
		case p.Blob != nil:
			b.WriteString(p.Blob.Text)
		case p.Diff != nil:
			b.WriteString(p.Diff.Patch)
		}
	}
	return b.String()
}
