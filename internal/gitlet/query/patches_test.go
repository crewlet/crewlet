package query_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/query"
	"github.com/crewlet/crewlet/internal/provenance"
)

// EVERY FILE'S PATCH (patches.go): a diff's or an interdiff's patches a page at a time in the
// stat's order, whole lines a page, each page carrying the stat's totals.

// patchesQ is a person's read of every file's patch of base..head.
func patchesQ(repo string, d gitlet.DiffQuery) gitlet.GitQuery {
	d.Patches = true
	return diffQ(repo, d)
}

// gitPatch is the person's git's own patch of base..head, renames found where asked and kept
// to paths where named, as a patches read writes it.
func gitPatch(x *fixture, base, head string, renames bool, paths ...string) string {
	x.t.Helper()
	args := []string{"-c", "core.quotePath=false", "diff-tree", "-r", "-p", "--full-index", "--no-color",
		"--no-ext-diff", "--no-textconv", "-U3"}
	if renames {
		args = append(args, "-M")
	}
	args = append(args, base, head)
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	return x.git(args...)
}

// patchesJoined is every page's patches' text, in order.
func patchesJoined(ps []gitlet.GitAnswer) string {
	var b strings.Builder
	for _, p := range ps {
		for _, fp := range p.Diff.Patches {
			b.WriteString(fp.Patch)
		}
	}
	return b.String()
}

// filePatches is each file's whole patch across the pages, in order, held to the pages'
// rules: a file goes on onto the next page exactly where the page before said it continues,
// files are numbered from 0 with no gap, and each page's Next names where its successor begins.
func filePatches(t *testing.T, ps []gitlet.GitAnswer) []gitlet.FilePatch {
	t.Helper()
	var out []gitlet.FilePatch
	goesOn := false
	for i, p := range ps {
		for j, fp := range p.Diff.Patches {
			switch {
			case j == 0 && goesOn:
				if last := &out[len(out)-1]; fp.File == last.File {
					last.Patch, last.RestOmitted = last.Patch+fp.Patch, fp.RestOmitted
					break
				}
				t.Fatalf("page %d begins with file %d, but page %d's last, file %d, continues", i, fp.File, i-1,
					out[len(out)-1].File)
			case fp.File != len(out):
				t.Fatalf("page %d holds file %d where file %d is next", i, fp.File, len(out))
			default:
				whole := fp
				whole.Continues = false
				out = append(out, whole)
			}
		}
		goesOn = false
		if n := len(p.Diff.Patches); n > 0 {
			goesOn = p.Diff.Patches[n-1].Continues
		}
		if p.Next == "" {
			if goesOn || i != len(ps)-1 {
				t.Errorf("page %d ends the read with a patch that continues %v", i, goesOn)
			}
			continue
		}
		var file, at int
		if _, err := fmt.Sscanf(p.Next, "w:%d:%d", &file, &at); err != nil {
			t.Fatalf("page %d's next %q is no patches cursor", i, p.Next)
		}
		last := p.Diff.Patches[len(p.Diff.Patches)-1]
		if wantFile := last.File + 1; (at == 1) == goesOn || (goesOn && file != wantFile) ||
			(!goesOn && file != wantFile+1) {
			t.Errorf("page %d ends with file %d continuing %v and names next %q", i, last.File, goesOn, p.Next)
		}
	}
	return out
}

// A DIFF'S PATCHES, PAGE BY PAGE, ARE GIT'S PATCH: forty files changed every way git writes
// a patch (modified, added, deleted, renamed, made executable, binary, made a link, quoted
// and UTF-8 paths, an empty file added), read a small page at a time, join to git's own
// patch byte for byte; each file is one FilePatch named as the stat names it (the link's two
// headers one patch); every page carries the stat's totals and an empty Files; and each page
// ends where its Next says, at a line or a file's start.
//
// Mutation: split files at `diff --git` headers (the link reads as two), or answer totals
// of the page → red.
func TestADiffsPatchesPageByPageAreGitsPatch(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	var long strings.Builder
	for i := range 60 {
		fmt.Fprintf(&long, "a line %d of a file that moves\n", i)
	}
	files := map[string]string{"gone.txt": "one\ntwo\n", "run.sh": "echo\n", "bin.dat": "a\x00b",
		"kind.txt": "a file\n", "moved/old.txt": long.String(), "sp ace.txt": "old\n", "q\"uote.txt": "old\n",
		"é.txt": "old\n"}
	for i := range 20 {
		files[fmt.Sprintf("mod/%02d.txt", i)] = strings.Repeat(fmt.Sprintf("line of %d\n", i), 8)
	}
	base := x.commit(files, "base")
	for _, gone := range []string{"gone.txt", "kind.txt", "moved/old.txt"} {
		if err := os.Remove(filepath.Join(x.work, gone)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(x.work, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("run.sh", filepath.Join(x.work, "kind.txt")); err != nil {
		t.Fatal(err)
	}
	changed := map[string]string{"bin.dat": "a\x00c", "moved/new.txt": strings.Replace(long.String(), "line 7 ",
		"line seven ", 1), "sp ace.txt": "new\n", "q\"uote.txt": "new\n", "é.txt": "new\n", "new.txt": "fresh\n",
		"empty.txt": ""}
	for i := range 20 {
		changed[fmt.Sprintf("mod/%02d.txt", i)] = strings.Repeat(fmt.Sprintf("line of %d\n", i), 4) + "changed\n"
	}
	for i := range 10 {
		changed[fmt.Sprintf("add/%02d.txt", i)] = fmt.Sprintf("added %d\n", i)
	}
	head := x.commit(changed, "head")
	repo := x.repo(gitlettest.RepoName)
	q := patchesQ(repo, gitlet.DiffQuery{Base: base, Head: head, Renames: true})
	limit := 1<<10 + x.decoration(q)
	s := x.server(0, query.Options{})
	query.SetPageBytes(s, limit)

	stat := mustRead(t, x.server(0, query.Options{}), diffQ(repo, gitlet.DiffQuery{Base: base, Head: head,
		Renames: true})).Diff
	if stat.Total != 40 || len(stat.Files) != 40 {
		t.Fatalf("the stat lists %d files of %d, want the case's 40", len(stat.Files), stat.Total)
	}
	ps := pages(t, s, q, limit)
	if len(ps) < 10 {
		t.Fatalf("the patches are %d pages at their bound, want a case of many", len(ps))
	}
	if got, want := patchesJoined(ps), gitPatch(x, base, head, true); got != want {
		t.Errorf("the pages' patches joined =\n%s\nwant git's own\n%s", got, want)
	}
	for i, p := range ps {
		if d := p.Diff; d.Files == nil || len(d.Files) != 0 || d.Total != stat.Total || d.Added != stat.Added ||
			d.Deleted != stat.Deleted {
			t.Errorf("page %d holds files %v and totals %d +%d -%d, want none and the stat's %d +%d -%d", i,
				d.Files, d.Total, d.Added, d.Deleted, stat.Total, stat.Added, stat.Deleted)
		}
	}
	whole := filePatches(t, ps)
	if len(whole) != len(stat.Files) {
		t.Fatalf("the pages hold %d files' patches, want the stat's %d", len(whole), len(stat.Files))
	}
	for i, fp := range whole {
		f := stat.Files[i]
		if fp.Path != f.Path || fp.PathB64 != f.PathB64 || fp.Status != f.Status {
			t.Errorf("patch %d names %q %q, want the stat's %q %q", i, fp.Path, fp.Status, f.Path, f.Status)
		}
		headers := strings.Count(fp.Patch, "diff --git ")
		if want := 1 + btoi(f.Status == "T"); headers != want {
			t.Errorf("the patch of %q holds %d headers, want %d", f.Path, headers, want)
		}
	}
	// THE PAGES END BOTH WAYS: some at a file's end, some inside one.
	atFile, inside := 0, 0
	for _, p := range ps[:len(ps)-1] {
		if strings.HasSuffix(p.Next, ":1") {
			atFile++
		} else {
			inside++
		}
	}
	if atFile == 0 || inside == 0 {
		t.Errorf("of %d pages, %d end at a file's start and %d inside one: want both", len(ps), atFile, inside)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A FILE'S PATCH GOES ON ON THE NEXT PAGE: a patch several pages long is a FilePatch of the
// same file on each, continuing on all but its last, its parts joined its own patch; a page
// ending at a file's end leaves its last patch whole, its Next the next file's first byte.
//
// Mutation: set continues on every page's last patch → red.
func TestAFilesPatchGoesOnOnTheNextPage(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	var b strings.Builder
	for i := range 120 {
		fmt.Fprintf(&b, "line %03d of the long file\n", i)
	}
	files := map[string]string{"a.txt": "a\n", "b.txt": b.String()}
	for i := range 30 {
		files[fmt.Sprintf("c/%02d.txt", i)] = strings.Repeat("c", i+1) + "\n"
	}
	base := x.commit(files, "base")
	files["b.txt"] = strings.ToUpper(b.String())
	for p := range files {
		if p != "b.txt" {
			files[p] = strings.ToUpper(files[p])
		}
	}
	head := x.commit(files, "head")
	repo := x.repo(gitlettest.RepoName)
	q := patchesQ(repo, gitlet.DiffQuery{Base: base, Head: head})
	limit := 1<<10 + x.decoration(q)
	s := x.server(0, query.Options{})
	query.SetPageBytes(s, limit)
	ps := pages(t, s, q, limit)
	var holding []int
	for i, p := range ps {
		for _, fp := range p.Diff.Patches {
			if fp.File == 1 {
				holding = append(holding, i)
				if fp.Continues != (i < len(ps)-1 && ps[i+1].Diff.Patches[0].File == 1) {
					t.Errorf("page %d's part of b.txt continues %v", i, fp.Continues)
				}
			}
		}
	}
	if len(holding) < 3 || holding[len(holding)-1]-holding[0] != len(holding)-1 {
		t.Fatalf("b.txt's patch is on pages %v, want three or more in a row", holding)
	}
	whole := filePatches(t, ps)
	want := mustRead(t, x.server(0, query.Options{}), diffQ(repo, gitlet.DiffQuery{Base: base, Head: head,
		File: "b.txt"})).Diff.Patch
	if len(whole) != 32 || whole[1].Patch != want {
		t.Errorf("b.txt's parts joined =\n%s\nwant its own patch\n%s", whole[1].Patch, want)
	}
	atFile := 0
	for _, p := range ps {
		if strings.HasSuffix(p.Next, ":1") {
			atFile++
		}
	}
	if atFile == 0 {
		t.Error("no page ends at a file's end: the case cannot tell a whole last patch from one going on")
	}
}

// PATHS NARROW A DIFF'S PATCHES: the pages hold exactly the files the same stat lists, in
// its order and numbered among them, their patches git's for those paths, the totals the
// filtered stat's; and git is fed those files' records alone.
//
// Mutation: feed every record and filter the output → red.
func TestPathsNarrowADiffsPatches(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{}
	for _, p := range []string{"docs/a.md", "internal/a/x.go", "internal/a/y/z.go", "internal/ab.go",
		"internal/b/w.go", "top.txt"} {
		files[p] = "one\n"
	}
	base := x.commit(files, "base")
	for p := range files {
		files[p] = "two\n"
	}
	head := x.commit(files, "head")
	repo := x.repo(gitlettest.RepoName)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0), Cache: memoless{x.f.Nodes()[0].Cache()}})
	narrow := []string{"internal/a"}
	a := mustRead(t, s, patchesQ(repo, gitlet.DiffQuery{Base: base, Head: head, Paths: narrow}))
	stat := mustRead(t, s, diffQ(repo, gitlet.DiffQuery{Base: base, Head: head, Paths: narrow})).Diff
	if a.Next != "" || a.Diff.Total != stat.Total || a.Diff.Added != stat.Added || a.Diff.Deleted != stat.Deleted {
		t.Errorf("the narrowed patches = %+v, want one page of the filtered stat's totals %+v", a.Diff, stat)
	}
	got := filePatches(t, []gitlet.GitAnswer{a})
	if len(got) != 2 || len(stat.Files) != 2 {
		t.Fatalf("the narrowed patches hold %d files and the stat %d, want internal/a's 2", len(got),
			len(stat.Files))
	}
	for i, fp := range got {
		if fp.Path != stat.Files[i].Path {
			t.Errorf("patch %d is of %q, want the stat's %q", i, fp.Path, stat.Files[i].Path)
		}
	}
	if got, want := patchesJoined([]gitlet.GitAnswer{a}), gitPatch(x, base, head, false, "internal/a"); got != want {
		t.Errorf("the narrowed patches =\n%s\nwant git's own for internal/a\n%s", got, want)
	}
	var fed []string
	for _, r := range rec.since(0) {
		if r.cmd.Args[0] == "diff-pairs" && slices.Contains(r.cmd.Args, "-p") {
			fed = append(fed, fedPaths(t, strings.ReplaceAll(r.stdin, "\x00\x00", "\x00"))...)
		}
	}
	if !slices.Equal(fed, []string{"internal/a/x.go", "internal/a/y/z.go"}) {
		t.Errorf("the patches' diff-pairs was fed %q, want internal/a's two files alone", fed)
	}
}

// AN INTERDIFF'S PATCHES ARE ITS FILES' PATCHES: a change touching three files, rebased
// over a target that moved twenty others, reads the three files' patches, each the one-file
// interdiff patch of it, and none of the target's.
//
// Mutation: walk the heads unfiltered → the target's files are read, red.
func TestAnInterdiffsPatchesAreItsFilesPatches(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{"c/1.txt": "one\n", "c/2.txt": "one\n", "c/3.txt": "one\n"}
	for i := range 20 {
		files[fmt.Sprintf("t/%02d.txt", i)] = "one\n"
	}
	base := x.commit(files, "base")
	x.branch("change", base)
	first := x.p.Commit(x.work, map[string]string{"c/1.txt": "two\n", "c/2.txt": "two\n", "c/3.txt": "two\n"},
		"the change's first revision")
	x.git("checkout", "-q", "main")
	moved := map[string]string{}
	for i := range 20 {
		moved[fmt.Sprintf("t/%02d.txt", i)] = "two\n"
	}
	target := x.commit(moved, "the target moves")
	x.git("checkout", "-q", "change")
	x.git("rebase", "-q", "main")
	second := x.p.Commit(x.work, map[string]string{"c/1.txt": "three\n", "c/2.txt": "three\n",
		"c/3.txt": "three\n"}, "the change's second revision")
	x.git("branch", "first", first)
	x.push(gitlettest.RepoName, "refs/heads/change", "refs/heads/first")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	from, to := x.facts(s, repo, first, base), x.facts(s, repo, second, target)
	q := gitlet.InterdiffQuery{FromBase: from.MergeBase, FromHead: first, ToBase: to.MergeBase, ToHead: second,
		FromTouched: from.Touched, ToTouched: to.Touched}
	all := q
	all.Patches = true
	a := mustRead(t, s, interdiffQ(repo, all))
	var want strings.Builder
	for _, f := range []string{"c/1.txt", "c/2.txt", "c/3.txt"} {
		one := q
		one.File = f
		want.WriteString(mustRead(t, s, interdiffQ(repo, one)).Diff.Patch)
	}
	got := filePatches(t, []gitlet.GitAnswer{a})
	if a.Kind != gitlet.GitInterdiff || len(got) != 3 || a.Diff.Total != 3 || a.Next != "" ||
		patchesJoined([]gitlet.GitAnswer{a}) != want.String() {
		t.Errorf("the interdiff's patches = %+v, want c's three files' interdiff patches\n%s", a.Diff, want.String())
	}
}

// A PATCHES PAGE STARTS AT ITS CURSOR'S FILE AND RUNS NO GIT BEFORE IT: a page from the 30th
// of forty files feeds diff-pairs the 30th to the 40th file's records alone; a cursor at the
// very end of a file's patch begins the page at the next file; a cursor whose byte is no
// line's start, one past a patch's end, one past the files, one of no file and another
// kind's are refused as no cursor the read issues.
//
// Mutation: feed every record from the first, or refuse a cursor at a patch's end → red.
func TestAPatchesPageStartsAtItsCursorsFileAndRunsNoGitBeforeIt(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{}
	var names []string
	for i := range 40 {
		names = append(names, fmt.Sprintf("f/%02d.txt", i))
		files[names[i]] = "one\n"
	}
	base := x.commit(files, "base")
	for p := range files {
		files[p] = "two\n"
	}
	head := x.commit(files, "head")
	repo := x.repo(gitlettest.RepoName)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0), Cache: memoless{x.f.Nodes()[0].Cache()}})
	q := patchesQ(repo, gitlet.DiffQuery{Base: base, Head: head})
	q.Cursor = "w:30:1"
	a := mustRead(t, s, q)
	if len(a.Diff.Patches) == 0 || a.Diff.Patches[0].File != 29 || a.Diff.Patches[0].Path != names[29] {
		t.Fatalf("the page from w:30:1 = %+v, want it to begin with %s", a.Diff.Patches, names[29])
	}
	var fed []string
	for _, r := range rec.since(0) {
		if r.cmd.Args[0] == "diff-pairs" {
			fed = append(fed, fedPaths(t, strings.ReplaceAll(r.stdin, "\x00\x00", "\x00"))...)
		}
	}
	if !slices.Equal(fed, names[29:]) {
		t.Errorf("diff-pairs was fed %q, want the 30th file's record on", fed)
	}
	end := len(a.Diff.Patches[0].Patch)
	q.Cursor = fmt.Sprintf("w:30:%d", end+1)
	if b := mustRead(t, s, q); len(b.Diff.Patches) == 0 || b.Diff.Patches[0].File != 30 {
		t.Errorf("the page from the end of %s's patch = %+v, want it to begin with %s", names[29], b.Diff.Patches,
			names[30])
	}
	for _, bad := range []string{"w:30:7", fmt.Sprintf("w:30:%d", end+2), "w:41:1", "w:0:1", "w:30", "p:5"} {
		q.Cursor = bad
		if _, err := s.Read(t.Context(), q); !errors.Is(err, gitlet.ErrBadCursor) {
			t.Errorf("cursor %q answered %v, want ErrBadCursor", bad, err)
		}
	}
}

// A PATCHES RUN STOPS PAST ITS PAGES: the run of a wide diff's first page cuts it and
// sixteen pages after it, each answered again from the memo; the page after those is a run
// of its own, which feeds git the files from its cursor's on.
//
// Mutation: cut to the end of the diff → the seventeenth page after the first is a hit, red.
func TestAPatchesRunStopsPastItsPages(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{}
	for i := range 400 {
		files[fmt.Sprintf("f/%03d.txt", i)] = "one\n"
	}
	base := x.commit(files, "base")
	for p := range files {
		files[p] = "two\n"
	}
	head := x.commit(files, "head")
	repo := x.repo(gitlettest.RepoName)
	x.settle(0, repo)
	q := patchesQ(repo, gitlet.DiffQuery{Base: base, Head: head})
	limit := 1<<10 + x.decoration(q)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	query.SetPageBytes(s, limit)
	a := mustRead(t, s, q)
	for i := 1; i <= query.MaxPatchesRunPages; i++ {
		q.Cursor = a.Next
		before := rec.count()
		if a = mustRead(t, s, q); !a.Memo || rec.count() != before {
			t.Fatalf("page %d past the first ran %v, want it from the first run's memo", i, kinds(rec, before))
		}
	}
	q.Cursor = a.Next
	before := rec.count()
	if a = mustRead(t, s, q); a.Memo {
		t.Fatalf("page %d past the first came from the memo, want a run of its own", query.MaxPatchesRunPages+1)
	}
	var file int
	if _, err := fmt.Sscanf(q.Cursor, "w:%d:", &file); err != nil {
		t.Fatal(err)
	}
	for _, r := range rec.since(before) {
		if r.cmd.Args[0] == "diff-pairs" {
			if fed := fedPaths(t, strings.ReplaceAll(r.stdin, "\x00\x00", "\x00")); len(fed) != 400-file+1 ||
				fed[0] != fmt.Sprintf("f/%03d.txt", file-1) {
				t.Errorf("the later run fed %d files from %q, want the %d from file %d", len(fed), fed[0],
					400-file+1, file)
			}
		}
	}
}

// A LINE TOO LONG FOR A PAGE OMITS THE REST OF ITS FILE'S PATCH, NOT THE FILES AFTER IT: a
// file with a line longer than one answer, pages in, is its patch up to that line marked
// rest_omitted, and the pages go on with git's own patches of every later file, all kept by
// the first read's run. A read from the long line's start answers the file so marked with no
// text; one the budget ends inside that line goes on at the next file.
//
// Mutation: refuse the read at the long line, or let a page the budget ends inside it go on
// inside the file → red.
func TestALineTooLongOmitsTheRestOfItsFileAlone(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	var a, b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&a, "line %02d of a\n", i)
		fmt.Fprintf(&b, "line %02d of b\n", i)
	}
	long := strings.Repeat("x", 3000)
	files := map[string]string{"a.txt": a.String(), "b.txt": b.String(), "c.txt": "c one\n" + long + "\nc three\n",
		"d.txt": "d\n", "e.txt": "e\n"}
	base := x.commit(files, "base")
	for p := range files {
		files[p] = strings.ToUpper(files[p])
	}
	head := x.commit(files, "head")
	repo := x.repo(gitlettest.RepoName)
	x.settle(0, repo)
	q := patchesQ(repo, gitlet.DiffQuery{Base: base, Head: head})
	limit := 1<<10 + x.decoration(q)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	query.SetPageBytes(s, limit)

	cWhole := gitPatch(x, base, head, false, "c.txt")
	prefix := cWhole[:strings.Index(cWhole, "\n-"+long)+1]
	ps := pages(t, s, q, limit)
	whole := filePatches(t, ps)
	if len(whole) != 5 || !whole[2].RestOmitted || whole[2].Patch != prefix {
		t.Fatalf("the pages hold %d files, c.txt's %+v, want all 5 and c.txt's patch up to its long line, "+
			"rest omitted, %q", len(whole), whole[2:min(3, len(whole))], prefix)
	}
	var others strings.Builder
	for i, fp := range whole {
		if i != 2 {
			others.WriteString(fp.Patch)
			if fp.RestOmitted {
				t.Errorf("%s's patch omits its rest, want only c.txt's", fp.Path)
			}
		}
	}
	if want := gitPatch(x, base, head, false, "a.txt", "b.txt", "d.txt", "e.txt"); others.String() != want {
		t.Errorf("the other files' patches =\n%s\nwant git's own\n%s", others.String(), want)
	}
	if slices.ContainsFunc(ps[0].Diff.Patches, func(fp gitlet.FilePatch) bool { return fp.File == 2 }) {
		t.Error("c.txt is on the first page, want its long line pages in")
	}
	for i, p := range ps {
		if i > 0 && !p.Memo {
			t.Errorf("page %d ran git again, want every page from the first read's run", i)
		}
	}

	ref := x.server(0, query.Options{Cache: memoless{x.f.Nodes()[0].Cache()}})
	query.SetPageBytes(ref, limit)
	at := q
	at.Cursor = fmt.Sprintf("w:3:%d", len(prefix)+1)
	rest := pages(t, ref, at, limit)
	if fp := rest[0].Diff.Patches[0]; fp.File != 2 || fp.Patch != "" || !fp.RestOmitted || fp.Continues {
		t.Errorf("the page from c.txt's long line begins %+v, want c.txt marked rest omitted and no text", fp)
	}
	if got, want := patchesJoined(rest), gitPatch(x, base, head, false, "d.txt", "e.txt"); got != want {
		t.Errorf("the pages from c.txt's long line hold\n%s\nwant d.txt's and e.txt's\n%s", got, want)
	}

	cut := x.server(0, query.Options{Git: cutShort{Pool: x.f.Nodes()[0].Pool(), after: len(prefix) + 2000},
		Cache: memoless{x.f.Nodes()[0].Cache()}})
	query.SetPageBytes(cut, limit)
	at.Cursor = "w:3:1"
	short := mustRead(t, cut, at)
	if fp := short.Diff.Patches; len(fp) != 1 || fp[0].Patch != prefix || !fp[0].RestOmitted || fp[0].Continues ||
		short.Next != "w:4:1" {
		t.Errorf("the page the budget ended inside c.txt's long line = %+v next %q, want c.txt up to it, rest "+
			"omitted, and next w:4:1", fp, short.Next)
	}
}

// cutShort is a query.Pool whose diff-pairs printing patches writes the first after bytes
// of git's output and then answers as one its deadline ended does; every other process
// runs on the pool beneath.
type cutShort struct {
	query.Pool
	after int
}

func (c cutShort) Run(ctx context.Context, cmd gitproc.Cmd) (gitproc.Result, error) {
	if cmd.Args[0] != "diff-pairs" || !slices.Contains(cmd.Args, "-p") {
		return c.Pool.Run(ctx, cmd)
	}
	out := cmd.Stdout
	var all bytes.Buffer
	cmd.Stdout = &all
	res, err := c.Pool.Run(ctx, cmd)
	if err != nil {
		return res, err
	}
	_, _ = out.Write(all.Bytes()[:min(c.after, all.Len())])
	return gitproc.Result{}, fmt.Errorf("git diff-pairs ran past its deadline: %w", gitproc.ErrDeadline)
}

// A PATCHES PAGE THE BUDGET ENDS IS ANSWERED AND NOT KEPT: a page git's deadline ended
// partway holds what was cut, its Next the line after, which reads on to git's whole patch,
// and a second read runs git again; with nothing cut it is refused naming the narrowing. A
// deadline after the asked page is whole answers it, and keeps the pages cut before it.
//
// Mutation: memoize the cut-short page → the second read is a hit, red.
func TestAPatchesPageTheBudgetEndsIsAnsweredAndNotKept(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{}
	for i := range 30 {
		files[fmt.Sprintf("f/%02d.txt", i)] = "one\n"
	}
	base := x.commit(files, "base")
	for p := range files {
		files[p] = "two\n"
	}
	head := x.commit(files, "head")
	repo := x.repo(gitlettest.RepoName)
	x.settle(0, repo)
	q := patchesQ(repo, gitlet.DiffQuery{Base: base, Head: head})
	limit := 1<<10 + x.decoration(q)
	server := func(after int) (*query.Server, *recording) {
		rec := &recording{}
		s := x.server(0, query.Options{Git: rec.on(x, 0)})
		rec.Pool = cutShort{Pool: x.f.Nodes()[0].Pool(), after: after}
		query.SetPageBytes(s, limit)
		return s, rec
	}
	ref := x.server(0, query.Options{Cache: memoless{x.f.Nodes()[0].Cache()}})
	query.SetPageBytes(ref, limit)
	whole := gitPatch(x, base, head, false)

	s, rec := server(150)
	a := mustRead(t, s, q)
	if len(a.Diff.Patches) == 0 || a.Next == "" || a.Diff.Total != 30 {
		t.Fatalf("the page cut short = %+v next %q, want what was cut and a next", a.Diff, a.Next)
	}
	rest := q
	rest.Cursor = a.Next
	if got := patchesJoined(append([]gitlet.GitAnswer{a}, pages(t, ref, rest, limit)...)); got != whole {
		t.Errorf("the cut page and the pages from its next =\n%s\nwant git's whole patch\n%s", got, whole)
	}
	before := rec.count()
	if again := mustRead(t, s, q); again.Memo || rec.count() == before {
		t.Error("a page the budget cut short was kept in the memo")
	}

	s, _ = server(0)
	if _, err := s.Read(t.Context(), q); !errors.Is(err, gitlet.ErrSliceTooLarge) ||
		!strings.Contains(err.Error(), "with file") {
		t.Errorf("a patches read the deadline ended before any line = %v, want ErrSliceTooLarge naming file", err)
	}

	s, rec = server(len(whole) / 2)
	a = mustRead(t, s, q)
	q.Cursor = a.Next
	before = rec.count()
	if next := mustRead(t, s, q); !next.Memo || rec.count() != before {
		t.Errorf("the page after a whole asked page ran %v, want it from the memo", kinds(rec, before))
	}
}

// damaged is a query.Pool whose diff-pairs printing patches runs whole and exits 0 but names
// damage it read around, as git over a truncated index does; every other process runs as is.
type damaged struct{ query.Pool }

func (d damaged) Run(ctx context.Context, cmd gitproc.Cmd) (gitproc.Result, error) {
	res, err := d.Pool.Run(ctx, cmd)
	if err == nil && cmd.Args[0] == "diff-pairs" && slices.Contains(cmd.Args, "-p") {
		res.Damage = &gitproc.Damage{Kind: gitproc.ErrCorrupt, Message: "index file is too small"}
	}
	return res, err
}

// A DAMAGED PATCHES RUN GIVES NO PAGE: a diff-pairs that exits 0 over damage it read around
// answers ErrGitUnready{syncing} and keeps nothing, so the read once the copy is whole runs
// git again.
//
// Mutation: answer the pages of a run whose git exited 0 over damage → red.
func TestADamagedPatchesRunGivesNoPage(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	base := x.commit(map[string]string{"a.txt": "one\n", "b.txt": "one\n"}, "base")
	head := x.commit(map[string]string{"a.txt": "two\n", "b.txt": "two\n"}, "head")
	repo := x.repo(gitlettest.RepoName)
	x.settle(0, repo)
	q := patchesQ(repo, gitlet.DiffQuery{Base: base, Head: head})
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	healthy := rec.Pool
	rec.Pool = damaged{healthy}
	if _, err := s.Read(t.Context(), q); !unready(err, gitlet.UnreadySyncing) || !strings.Contains(err.Error(),
		"damage") {
		t.Fatalf("a patches read whose git named damage = %v, want ErrGitUnready{syncing} naming it", err)
	}
	rec.Pool = healthy
	if a := mustRead(t, s, q); a.Memo || len(a.Diff.Patches) != 2 {
		t.Errorf("the read after a damaged run = %+v (memo %v), want both patches from a run of its own",
			a.Diff.Patches, a.Memo)
	}
}

// EVERY PATCH ON A PAGE FITS A TOOL'S FENCE: a page of hundreds of one-line patches at the
// shipped bound, each patch fenced as code_diff fences it, still encodes within
// gitlet.MaxGitAnswerBytes, the tool's answer less what it wraps a page in.
//
// Mutation: drop the fence from a patch's cost → red.
func TestEveryPatchOnAPageFitsAToolsFence(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{}
	for i := range 400 {
		files[fmt.Sprintf("f/%03d.txt", i)] = "one\n"
	}
	base := x.commit(files, "base")
	for p := range files {
		files[p] = "two\n"
	}
	head := x.commit(files, "head")
	repo := x.repo(gitlettest.RepoName)
	a := mustRead(t, x.server(0, query.Options{}), patchesQ(repo, gitlet.DiffQuery{Base: base, Head: head}))
	if a.Next == "" || len(a.Diff.Patches) < 100 {
		t.Fatalf("the first page holds %d patches and next %q, want a full page of many", len(a.Diff.Patches),
			a.Next)
	}
	fenced := a
	d := *a.Diff
	d.Patches = slices.Clone(d.Patches)
	for i := range d.Patches {
		d.Patches[i].Patch = provenance.Repository(provenance.SourceRepoPatch).Fence(provenance.NewBoundary(),
			d.Patches[i].Patch)
	}
	fenced.Diff = &d
	if n := encoded(t, fenced); n > gitlet.MaxGitAnswerBytes {
		t.Errorf("the page with its %d patches fenced encodes to %d bytes, past the %d a tool's page holds",
			len(d.Patches), n, gitlet.MaxGitAnswerBytes)
	}
}

// A SECOND PATCHES READ RUNS NO GIT: the same page again and the next page's cursor are
// answered from the memo, each exactly the page a read from its cursor cuts; and a stat of
// the same diff read first is no patches page.
//
// Mutation: key patches pages as stat pages → the stat's page answers the patches read, red.
func TestASecondPatchesReadRunsNoGit(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{}
	for i := range 60 {
		files[fmt.Sprintf("f/%02d.txt", i)] = "one\n"
	}
	base := x.commit(files, "base")
	for p := range files {
		files[p] = "two\n"
	}
	head := x.commit(files, "head")
	repo := x.repo(gitlettest.RepoName)
	x.settle(0, repo)
	q := patchesQ(repo, gitlet.DiffQuery{Base: base, Head: head})
	limit := 1<<10 + x.decoration(q)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	query.SetPageBytes(s, limit)
	ref := x.server(0, query.Options{Cache: memoless{x.f.Nodes()[0].Cache()}})
	query.SetPageBytes(ref, limit)

	stat := q
	stat.Diff = &gitlet.DiffQuery{Base: base, Head: head}
	mustRead(t, s, stat)
	first := mustRead(t, s, q)
	if len(first.Diff.Patches) == 0 {
		t.Fatalf("the patches read after a stat = %+v, want patches", first.Diff)
	}
	before := rec.count()
	if again := mustRead(t, s, q); !again.Memo || rec.count() != before ||
		!reflect.DeepEqual(unstamped(again), unstamped(first)) {
		t.Errorf("the first page read again ran %v, want the same page from the memo", kinds(rec, before))
	}
	for i := range 5 {
		want := mustRead(t, ref, q)
		got := mustRead(t, s, q)
		if !got.Memo || rec.count() != before || !reflect.DeepEqual(unstamped(got), unstamped(want)) {
			t.Errorf("page %d =\n%+v\nwant from the memo the page a read from its cursor cuts\n%+v", i, got, want)
		}
		if q.Cursor = got.Next; q.Cursor == "" {
			t.Fatalf("the patches ended after %d pages, want a case of more", i+1)
		}
	}
}
