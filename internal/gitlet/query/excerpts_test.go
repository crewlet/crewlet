package query_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// diffFile is one file of the person's own whole-file diff: its status, whether git called it
// binary, and its lines with their marks and numbers.
type diffFile struct {
	status string
	binary bool
	lines  []gitlet.ExcerptLine
}

// wholeDiff is the person's own `git diff -U1000000 -M` of base and head, outside any read: each
// file by its new path and by its old path.
func (x *fixture) wholeDiff(base, head string) (byNew, byOld map[string]*diffFile) {
	x.t.Helper()
	byNew, byOld = map[string]*diffFile{}, map[string]*diffFile{}
	var f *diffFile
	inHunk, oldAt, newAt := false, 0, 0
	for _, line := range strings.Split(x.git("diff", "-U1000000", "-M", "--no-color", base, head), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git a/"):
			a, b, _ := strings.Cut(strings.TrimPrefix(line, "diff --git a/"), " b/")
			f, inHunk = &diffFile{status: "M"}, false
			byOld[a], byNew[b] = f, f
		case inHunk && line != "" && strings.ContainsRune(" +-", rune(line[0])):
			l := gitlet.ExcerptLine{Mark: line[:1], Text: line[1:]}
			if line[0] != '+' {
				l.Old, oldAt = oldAt, oldAt+1
			}
			if line[0] != '-' {
				l.New, newAt = newAt, newAt+1
			}
			f.lines = append(f.lines, l)
		case strings.HasPrefix(line, "new file mode"):
			f.status = "A"
		case strings.HasPrefix(line, "deleted file mode"):
			f.status = "D"
		case strings.HasPrefix(line, "rename from"):
			f.status = "R"
		case strings.HasPrefix(line, "Binary files"):
			f.binary = true
		case strings.HasPrefix(line, "@@ "):
			h, ok := gitlet.ParseHunk([]byte(line))
			if !ok {
				x.t.Fatalf("the person's diff wrote %q, which is no hunk header", line)
			}
			inHunk, oldAt, newAt = true, h.OldStart, h.NewStart
		}
	}
	return byNew, byOld
}

// fileLines is path's lines at commit, each marked as the side given: kept (both numbers),
// added (its new one) or deleted (its old one).
func (x *fixture) fileLines(commit, path, mark string) []gitlet.ExcerptLine {
	x.t.Helper()
	var out []gitlet.ExcerptLine
	for i, text := range strings.Split(strings.TrimSuffix(x.git("show", commit+":"+path), "\n"), "\n") {
		l := gitlet.ExcerptLine{Mark: mark, Text: text}
		if mark != "+" {
			l.Old = i + 1
		}
		if mark != "-" {
			l.New = i + 1
		}
		out = append(out, l)
	}
	return out
}

// window is the excerpt the window rule cuts from lines: from the first to the last line whose
// number on side lies within around lines of start to end, every line between them kept, and
// at most gitlet.MaxExcerptLines of them.
func window(id, status string, lines []gitlet.ExcerptLine, side gitlet.Side, start, end, around int) gitlet.Excerpt {
	lo, hi := max(1, start-around), end+around
	first, last := -1, -1
	for i, l := range lines {
		n := l.New
		if side == gitlet.SideOld {
			n = l.Old
		}
		if n != 0 && n >= lo && n <= hi {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	e := gitlet.Excerpt{ID: id, Status: status, Lines: []gitlet.ExcerptLine{}}
	if first < 0 {
		return e
	}
	e.Lines = slices.Clone(lines[first : last+1])
	if len(e.Lines) > gitlet.MaxExcerptLines {
		e.Lines, e.More = e.Lines[:gitlet.MaxExcerptLines], true
	}
	return e
}

// blobAt is path's blob at commit.
func (x *fixture) blobAt(commit, path string) string {
	x.t.Helper()
	return strings.TrimSpace(x.git("rev-parse", commit+":"+path))
}

// jsonOf is v's encoded length.
func jsonOf(t *testing.T, v any) int {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return len(out)
}

// excerptsOf is every excerpt of q, its pages read in turn.
func excerptsOf(t *testing.T, s *query.Server, q gitlet.GitQuery) []gitlet.Excerpt {
	t.Helper()
	var out []gitlet.Excerpt
	for _, a := range pages(t, s, q, gitlet.MaxGitAnswerBytes) {
		out = append(out, a.Excerpts.Excerpts...)
	}
	return out
}

// sameExcerpts reports each excerpt of got that is not want's, by its id.
func sameExcerpts(t *testing.T, what string, got, want []gitlet.Excerpt) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s answered %d excerpts, want %d", what, len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("%s: the excerpt of %s is\n\t%+v\nwant\n\t%+v", what, want[i].ID, got[i], want[i])
		}
	}
}

// AN EXCERPT IS ITS RANGE'S LINES WITH THEIR DIFF MARKS: each window, on either side and at
// every context, equals the slice of the person's own whole-file diff the window rule cuts.
//
// Mutation: diff at -U3, or number a deleted line on the new side → red.
func TestAnExcerptIsItsRangesLinesWithTheirDiffMarks(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	base := x.commit(map[string]string{"f.txt": numbered(60, nil, 0)}, "sixty lines")
	changed := numbered(60, map[int]string{10: "line ten"}, 0)
	changed = strings.Replace(changed, "line 30\nline 31\n", "new thirty\n", 1)
	changed = strings.Replace(changed, "line 50\n", "line 50\nadded one\nadded two\n", 1)
	head := x.commit(map[string]string{"f.txt": changed}, "three changes")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	byNew, byOld := x.wholeDiff(base, head)
	newBlob, oldBlob := x.blobAt(head, "f.txt"), x.blobAt(base, "f.txt")

	anchor := func(id string, side gitlet.Side, start, end int) gitlet.ExcerptAnchor {
		blob := newBlob
		if side == gitlet.SideOld {
			blob = oldBlob
		}
		return gitlet.ExcerptAnchor{LineAnchor: gitlet.LineAnchor{ID: id, Path: "f.txt", Side: side, BlobOID: blob,
			Start: start, End: end}, Base: base, Head: head}
	}
	anchors := []gitlet.ExcerptAnchor{
		anchor("added", gitlet.SideNew, 50, 51),
		anchor("hunk edge", gitlet.SideNew, 7, 7),
		anchor("deleted", gitlet.SideOld, 30, 30),
		anchor("far", gitlet.SideNew, 20, 20),
		anchor("a range", gitlet.SideNew, 9, 12),
		anchor("old side kept", gitlet.SideOld, 12, 12),
	}
	for _, around := range []int{0, gitlet.DefaultExcerptContext, gitlet.MaxExcerptContext} {
		var want []gitlet.Excerpt
		for _, a := range anchors {
			f := byNew["f.txt"]
			if a.Side == gitlet.SideOld {
				f = byOld["f.txt"]
			}
			want = append(want, window(a.ID, "M", f.lines, a.Side, a.Start, a.End, around))
		}
		q := gitlet.ExcerptsQuery{Anchors: anchors, Context: new(around)}
		if around == gitlet.DefaultExcerptContext {
			q.Context = nil // the default, unasked
		}
		got := excerptsOf(t, s, excerptsQ(repo, q))
		sameExcerpts(t, fmt.Sprintf("context %d", around), got, want)
	}
	// The premise: the far line's window holds no change, and the deleted line's the line
	// that replaced it.
	far := window("far", "M", byNew["f.txt"].lines, gitlet.SideNew, 20, 20, 3)
	deleted := window("deleted", "M", byOld["f.txt"].lines, gitlet.SideOld, 30, 30, 3)
	if len(far.Lines) != 7 || slices.ContainsFunc(far.Lines, func(l gitlet.ExcerptLine) bool { return l.Mark != " " }) ||
		!slices.ContainsFunc(deleted.Lines, func(l gitlet.ExcerptLine) bool { return l.Text == "new thirty" }) {
		t.Fatalf("the premise: the far window %+v, the deleted one %+v", far, deleted)
	}
}

// AN EXCERPT OF A RENAMED FILE IS DIFFED AGAINST WHERE IT WAS, as the person's `git diff -M`
// pairs it; a path both sides hold reads as modified.
//
// Mutation: skip the rename walk (every line of b.go added), or walk without -M → red.
func TestAnExcerptOfARenamedFileIsDiffedAgainstWhereItWas(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	base := x.commit(map[string]string{"a.go": numbered(20, nil, 0), "d.go": numbered(15, nil, 3),
		"keep.go": "keep\n"}, "two files")
	x.git("mv", "a.go", "b.go")
	x.git("mv", "d.go", "e.go")
	head := x.commit(map[string]string{"b.go": numbered(20, map[int]string{8: "line eight"}, 0),
		"d.go": "a new d\n"}, "moved")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	byNew, byOld := x.wholeDiff(base, head)
	if byNew["b.go"].status != "R" || byNew["d.go"].status != "M" || byNew["e.go"].status != "A" {
		t.Fatalf("the premise: the person's diff reads b.go %+v, d.go %+v, e.go %+v", byNew["b.go"],
			byNew["d.go"], byNew["e.go"])
	}
	anchor := func(id, path string, side gitlet.Side, at string, line int) gitlet.ExcerptAnchor {
		return gitlet.ExcerptAnchor{LineAnchor: gitlet.LineAnchor{ID: id, Path: path, Side: side,
			BlobOID: x.blobAt(at, path), Start: line, End: line}, Base: base, Head: head}
	}
	anchors := []gitlet.ExcerptAnchor{
		anchor("moved", "b.go", gitlet.SideNew, head, 8),
		anchor("where it was", "a.go", gitlet.SideOld, base, 8),
		anchor("written in its place", "d.go", gitlet.SideNew, head, 1),
		anchor("moved past a new file", "e.go", gitlet.SideNew, head, 5),
	}
	want := []gitlet.Excerpt{
		window("moved", "R", byNew["b.go"].lines, gitlet.SideNew, 8, 8, 3),
		window("where it was", "R", byOld["a.go"].lines, gitlet.SideOld, 8, 8, 3),
		window("written in its place", "M", byNew["d.go"].lines, gitlet.SideNew, 1, 1, 3),
		window("moved past a new file", "A", byNew["e.go"].lines, gitlet.SideNew, 5, 5, 3),
	}
	sameExcerpts(t, "the renamed files", excerptsOf(t, s, excerptsQ(repo, gitlet.ExcerptsQuery{Anchors: anchors})),
		want)
	if n := len(want[0].Lines); n != 8 || !slices.ContainsFunc(want[0].Lines, func(l gitlet.ExcerptLine) bool {
		return l.Mark == "+" && l.Text == "line eight"
	}) {
		t.Fatalf("the premise: the rename's window is %+v, want its changed line among kept ones", want[0])
	}
}

// AN EXCERPT OF AN ADDED, A DELETED OR AN UNCHANGED FILE is every line added, deleted or
// kept; a revision that shares no history with its target adds every line.
//
// Mutation: hand diff-pairs two identical blobs for the unchanged file → red.
func TestAnExcerptOfAnAddedDeletedOrUnchangedFile(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	base := x.commit(map[string]string{"g.txt": numbered(9, nil, 0), "h.txt": numbered(12, nil, 0)}, "two")
	x.git("rm", "-q", "g.txt")
	head := x.commit(map[string]string{"n.txt": "alpha\nbeta\ngamma\ndelta\nepsilon\nzeta\n"},
		"one added, one deleted")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	if byNew, byOld := x.wholeDiff(base, head); byNew["n.txt"].status != "A" || byOld["g.txt"].status != "D" {
		t.Fatalf("the premise: the person's diff reads n.txt %+v and g.txt %+v", byNew["n.txt"], byOld["g.txt"])
	}
	anchor := func(id, path string, side gitlet.Side, at, b string, line int) gitlet.ExcerptAnchor {
		return gitlet.ExcerptAnchor{LineAnchor: gitlet.LineAnchor{ID: id, Path: path, Side: side,
			BlobOID: x.blobAt(at, path), Start: line, End: line}, Base: b, Head: head}
	}
	anchors := []gitlet.ExcerptAnchor{
		anchor("added", "n.txt", gitlet.SideNew, head, base, 2),
		anchor("deleted", "g.txt", gitlet.SideOld, base, base, 5),
		anchor("unchanged", "h.txt", gitlet.SideNew, head, base, 6),
		anchor("unchanged old side", "h.txt", gitlet.SideOld, base, base, 11),
		anchor("unrelated", "h.txt", gitlet.SideNew, head, "", 6),
	}
	want := []gitlet.Excerpt{
		window("added", "A", x.fileLines(head, "n.txt", "+"), gitlet.SideNew, 2, 2, 3),
		window("deleted", "D", x.fileLines(base, "g.txt", "-"), gitlet.SideOld, 5, 5, 3),
		window("unchanged", "", x.fileLines(head, "h.txt", " "), gitlet.SideNew, 6, 6, 3),
		window("unchanged old side", "", x.fileLines(base, "h.txt", " "), gitlet.SideOld, 11, 11, 3),
		window("unrelated", "A", x.fileLines(head, "h.txt", "+"), gitlet.SideNew, 6, 6, 3),
	}
	sameExcerpts(t, "the files", excerptsOf(t, s, excerptsQ(repo, gitlet.ExcerptsQuery{Anchors: anchors})), want)
	if len(want[2].Lines) != 7 || len(want[3].Lines) != 5 {
		t.Fatalf("the premise: the unchanged windows are %+v and %+v", want[2], want[3])
	}
}

// AN EXCERPT OF A BINARY FILE HAS NO LINES: a pair git calls binary is marked so, its lines an
// empty list, never absent.
//
// Mutation: drop the cutter's binary arm (the excerpt is not marked) → red.
func TestAnExcerptOfABinaryFileHasNoLines(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	base := x.commit(map[string]string{"bin.dat": "a\x00b\n"}, "binary")
	head := x.commit(map[string]string{"bin.dat": "a\x00c\n"}, "binary again")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	a := mustRead(t, s, excerptsQ(repo, gitlet.ExcerptsQuery{Anchors: []gitlet.ExcerptAnchor{{
		LineAnchor: gitlet.LineAnchor{ID: "bin", Path: "bin.dat", Side: gitlet.SideNew,
			BlobOID: x.blobAt(head, "bin.dat"), Start: 1, End: 1}, Base: base, Head: head}}}))
	want := []gitlet.Excerpt{{ID: "bin", Status: "M", Binary: true, Lines: []gitlet.ExcerptLine{}}}
	sameExcerpts(t, "the binary file", a.Excerpts.Excerpts, want)
	if !strings.Contains(mustJSON(t, a), `"lines":[]`) {
		t.Errorf("the binary file's excerpt encodes as %s, with no list of lines", mustJSON(t, a))
	}
}

// AN EXCERPT IS BOUNDED IN ITS LINES AND EACH LINE: at most gitlet.MaxExcerptLines and more,
// and a line past gitlet.MaxExcerptLineBytes described by its size, with no text.
//
// Mutation: hold pending lines unbounded, or keep a long line's text → red.
func TestAnExcerptIsBoundedInItsLinesAndEachLine(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	long := strings.Repeat("m", 5<<10)
	base := x.commit(map[string]string{"big.txt": numbered(120, nil, 0), "ins.txt": numbered(20, nil, 0),
		"min.js": "a\n" + long + "\nb\n"}, "files")
	var inserted strings.Builder
	for i := range 10_000 {
		fmt.Fprintf(&inserted, "inserted %d\n", i)
	}
	head := x.commit(map[string]string{"ins.txt": strings.Replace(numbered(20, nil, 0), "line 10\n",
		"line 10\n"+inserted.String(), 1)}, "an insertion")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	_, byOld := x.wholeDiff(base, head)
	anchor := func(id, path string, side gitlet.Side, at string, start, end int) gitlet.ExcerptAnchor {
		return gitlet.ExcerptAnchor{LineAnchor: gitlet.LineAnchor{ID: id, Path: path, Side: side,
			BlobOID: x.blobAt(at, path), Start: start, End: end}, Base: base, Head: head}
	}
	got := excerptsOf(t, s, excerptsQ(repo, gitlet.ExcerptsQuery{Context: new(0), Anchors: []gitlet.ExcerptAnchor{
		anchor("long range", "big.txt", gitlet.SideNew, head, 1, 100),
		anchor("insertion", "ins.txt", gitlet.SideOld, base, 10, 11),
		anchor("minified", "min.js", gitlet.SideNew, head, 2, 2),
	}}))
	want := []gitlet.Excerpt{
		window("long range", "", x.fileLines(head, "big.txt", " "), gitlet.SideNew, 1, 100, 0),
		window("insertion", "M", byOld["ins.txt"].lines, gitlet.SideOld, 10, 11, 0),
		{ID: "minified", Lines: []gitlet.ExcerptLine{{Mark: " ", Old: 2, New: 2, TextBytes: len(long)}}},
	}
	sameExcerpts(t, "the bounded excerpts", got, want)
	for _, e := range want[:2] {
		if len(e.Lines) != gitlet.MaxExcerptLines || !e.More {
			t.Fatalf("the premise: %s's window is %d lines, more %t", e.ID, len(e.Lines), e.More)
		}
	}
}

// THE WIDEST EXCERPT FITS A PAGE: every line and number at its widest, in a real read's
// envelope, is within gitlet.MaxGitAnswerBytes, and a real read answers it in one page.
//
// Mutation: raise MaxExcerptLineBytes to 2 KiB → red.
func TestTheWidestExcerptFitsAPage(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	// Each `<` is six bytes in JSON: 170 and four letters are 1 024.
	text := strings.Repeat("<", 170) + "abcd"
	if query.TextCost([]byte(text)) != gitlet.MaxExcerptLineBytes {
		t.Fatalf("the premise: a line of %d bytes encodes to %d", len(text), query.TextCost([]byte(text)))
	}
	head := x.commit(map[string]string{"wide.txt": strings.Repeat(text+"\n", gitlet.MaxExcerptLines)}, "wide")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	a := mustRead(t, s, excerptsQ(repo, gitlet.ExcerptsQuery{Anchors: []gitlet.ExcerptAnchor{{
		LineAnchor: gitlet.LineAnchor{ID: "wide", Path: "wide.txt", Side: gitlet.SideNew,
			BlobOID: x.blobAt(head, "wide.txt"), Start: 1, End: gitlet.MaxExcerptLines}, Head: head}}}))
	e := a.Excerpts.Excerpts[0]
	if len(e.Lines) != gitlet.MaxExcerptLines || e.Lines[0].Text != text || a.Next != "" {
		t.Fatalf("the widest real excerpt answered %d lines, the first %q, next %q", len(e.Lines),
			e.Lines[0].Text, a.Next)
	}
	envelope := encoded(t, a) - jsonOf(t, e)
	widest := gitlet.Excerpt{ID: strings.Repeat("<", 64), Status: "M", Binary: true, More: true}
	for range gitlet.MaxExcerptLines {
		widest.Lines = append(widest.Lines, gitlet.ExcerptLine{Mark: "-", Old: gitlet.MaxAnchorLine,
			New: gitlet.MaxAnchorLine, Text: text})
	}
	if n := envelope + jsonOf(t, widest) + len(`"next":"x:9223372036854775807",`); n >
		gitlet.MaxGitAnswerBytes {
		t.Errorf("the widest excerpt answers %d bytes, past the %d one answer holds", n, gitlet.MaxGitAnswerBytes)
	}
}

// EXCERPTS ARE PAGED WHOLE: each page whole excerpts within its bound, read in turn equal to
// one unbounded page; a cursor past the anchors or at the first is refused.
//
// Mutation: cut a page inside an excerpt, or issue x:0 → red.
func TestExcerptsArePagedWhole(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	const n = gitlet.MaxExcerptAnchors
	var file strings.Builder
	for i := range n * gitlet.MaxExcerptLines {
		fmt.Fprintf(&file, "%05d %s\n", i, strings.Repeat("a", 300))
	}
	head := x.commit(map[string]string{"long.txt": file.String()}, "long")
	repo := x.repo(gitlettest.RepoName)
	blob := x.blobAt(head, "long.txt")
	var anchors []gitlet.ExcerptAnchor
	for i := range n {
		anchors = append(anchors, gitlet.ExcerptAnchor{LineAnchor: gitlet.LineAnchor{ID: fmt.Sprintf("a%02d", i),
			Path: "long.txt", Side: gitlet.SideNew, BlobOID: blob, Start: i*gitlet.MaxExcerptLines + 1,
			End: (i + 1) * gitlet.MaxExcerptLines}, Head: head})
	}
	q := excerptsQ(repo, gitlet.ExcerptsQuery{Anchors: anchors, Context: new(0)})
	s := x.server(0, query.Options{})
	paged := pages(t, s, q, gitlet.MaxGitAnswerBytes)
	if len(paged) < 2 {
		t.Fatalf("the premise: %d anchors answered %d page", n, len(paged))
	}
	var all []gitlet.Excerpt
	for _, p := range paged {
		all = append(all, p.Excerpts.Excerpts...)
		if p.Next != "" && p.Next != "x:"+strconv.Itoa(len(all)) {
			t.Errorf("a page ending after %d excerpts names next %q", len(all), p.Next)
		}
	}
	unbounded := x.server(0, query.Options{})
	query.SetPageBytes(unbounded, 1<<30)
	whole := mustRead(t, unbounded, q)
	if whole.Next != "" {
		t.Fatalf("an unbounded page names next %q", whole.Next)
	}
	sameExcerpts(t, "the pages read in turn", all, whole.Excerpts.Excerpts)
	for i, e := range all {
		if e.ID != anchors[i].ID || len(e.Lines) != gitlet.MaxExcerptLines || e.More {
			t.Fatalf("excerpt %d is %s of %d lines, more %t", i, e.ID, len(e.Lines), e.More)
		}
	}

	// A cursor no page began at at this bound reads from its anchor.
	from := q
	from.Cursor = "x:5"
	sameExcerpts(t, "the pages from anchor 5", excerptsOf(t, s, from), all[5:])
	for _, c := range []string{"x:0", "x:" + strconv.Itoa(n), "e:1"} {
		bad := q
		bad.Cursor = c
		if _, err := s.Read(t.Context(), bad); !errors.Is(err, gitlet.ErrBadCursor) {
			t.Errorf("cursor %q answered %v, want ErrBadCursor", c, err)
		}
	}
}

// AN EXCERPTS HUNK IS THE WHOLE FILE: diff-pairs asks for gitlet.MaxAnchorLine lines of context.
//
// Mutation: change either the argument or the bound → red.
func TestAnExcerptsHunkIsTheWholeFile(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	head := x.commit(map[string]string{"f": "one\n"}, "one")
	repo := x.repo(gitlettest.RepoName)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	mustRead(t, s, excerptsQ(repo, gitlet.ExcerptsQuery{Anchors: []gitlet.ExcerptAnchor{{
		LineAnchor: gitlet.LineAnchor{ID: "f", Path: "f", Side: gitlet.SideNew, BlobOID: x.blobAt(head, "f"),
			Start: 1, End: 1}, Head: head}}}))
	var diffs [][]string
	for _, c := range rec.ran() {
		if c.Args[0] == "diff-pairs" {
			diffs = append(diffs, c.Args)
		}
	}
	if len(diffs) != 1 || !slices.Contains(diffs[0], "-U"+strconv.Itoa(gitlet.MaxAnchorLine)) {
		t.Errorf("the excerpts read ran diff-pairs %q, want one at -U%d", diffs, gitlet.MaxAnchorLine)
	}
}

// A SECOND EXCERPTS READ RUNS NO GIT: the first run memoized every page, so the same read and
// its second page are answered from the memo.
//
// Mutation: memoize the asked page alone (the second page runs git) → red.
func TestASecondExcerptsReadRunsNoGit(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	var file strings.Builder
	for i := range 400 {
		fmt.Fprintf(&file, "%03d %s\n", i, strings.Repeat("<", 120))
	}
	base := x.commit(map[string]string{"f.txt": file.String()}, "base")
	head := x.commit(map[string]string{"f.txt": strings.Replace(file.String(), "200 ", "two hundred ", 1)},
		"head")
	repo := x.repo(gitlettest.RepoName)
	blob := x.blobAt(head, "f.txt")
	var anchors []gitlet.ExcerptAnchor
	for i := range 10 {
		anchors = append(anchors, gitlet.ExcerptAnchor{LineAnchor: gitlet.LineAnchor{ID: fmt.Sprint(i),
			Path: "f.txt", Side: gitlet.SideNew, BlobOID: blob, Start: i*40 + 1, End: i*40 + 40}, Base: base,
			Head: head})
	}
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	q := excerptsQ(repo, gitlet.ExcerptsQuery{Anchors: anchors})
	first := mustRead(t, s, q)
	if first.Next == "" || first.Memo {
		t.Fatalf("the premise: the first read names next %q, memo %t", first.Next, first.Memo)
	}
	ran := rec.count()
	again := mustRead(t, s, q)
	second := q
	second.Cursor = first.Next
	next := mustRead(t, s, second)
	if more := rec.since(ran); len(more) != 0 {
		t.Errorf("the reads after the first ran git %d times, want none", len(more))
	}
	if !again.Memo || !next.Memo || !reflect.DeepEqual(again.Excerpts, first.Excerpts) ||
		len(next.Excerpts.Excerpts) == 0 {
		t.Errorf("the memo answered %+v and the second page %+v", again.Excerpts, next.Excerpts)
	}
}
