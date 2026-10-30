package query_test

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/query"
)

// EVERY ANSWER FITS A PAGE. Over the widest tree the fixture holds — two
// thousand files with names of two hundred bytes — its longest line of text
// that costs six JSON bytes a character, and its largest diff, every kind read
// page by page at the shipped bound encodes within code.MaxGitAnswerBytes, so
// a page is always one tool answer, and the pages hold the whole: every entry,
// every changed file, every byte of the file.
func TestEveryAnswerFitsAPage(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	const files = 2000
	name := func(i int) string { return fmt.Sprintf("wide/%04d-%s", i, strings.Repeat("n", 195)) }
	tree := map[string]string{}
	for i := range files {
		tree[name(i)] = "v1\n"
	}
	var huge strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&huge, "%d %s\n", i, strings.Repeat("<\"\\\x01é", 60))
	}
	huge.WriteString(strings.Repeat("<&>", 40_000) + "\n")
	tree["huge.txt"] = huge.String()
	base := x.commit(tree, "base")
	for i := range files {
		tree[name(i)] = "v2\n"
	}
	tree["huge.txt"] = strings.Replace(huge.String(), "\n1000 ", "\nchanged 1000 ", 1)
	head := x.commit(tree, "head")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})
	limit := code.MaxGitAnswerBytes

	listing := pages(t, s, treeQ(repo, code.TreeQuery{Rev: head, Recursive: true}), limit)
	entries := 0
	for _, p := range listing {
		entries += len(p.Tree.Entries)
	}
	if entries != files+1 || len(listing) < 5 {
		t.Errorf("the recursive listing gave %d entries in %d pages, want %d in several", entries, len(listing),
			files+1)
	}
	pages(t, s, treeQ(repo, code.TreeQuery{Rev: head, Path: "wide"}), limit)

	text := pages(t, s, blobQ(repo, code.BlobQuery{Rev: head, Path: "huge.txt"}), limit)
	if joined(text) != tree["huge.txt"] {
		t.Errorf("huge.txt's %d pages concatenate to %d bytes, want its %d", len(text), len(joined(text)),
			len(tree["huge.txt"]))
	}

	stat := pages(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head}), limit)
	changed := 0
	for _, p := range stat {
		changed += len(p.Diff.Files)
	}
	if changed != files+1 {
		t.Errorf("the stat's pages hold %d files, want %d", changed, files+1)
	}
	pages(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, File: name(7)}), limit)
	if a := mustRead(t, s, compareQ(repo, base, head)); encoded(t, a) > limit {
		t.Errorf("the comparison encodes to %d bytes, past %d", encoded(t, a), limit)
	}
	tip, at := x.tip("refs/heads/main")
	if a := mustRead(t, s, landedQ(repo, base, "refs/heads/main", tip, at, true)); encoded(t, a) > limit {
		t.Errorf("the landed-in answer encodes to %d bytes, past %d", encoded(t, a), limit)
	}

	// A REVISION'S FACTS: past the bound they touch all, and at it their list
	// is the most paths of two hundred bytes code.MaxTouchedBytes holds —
	// one page all the same.
	if f := mustRead(t, s, factsQ(repo, head, base)); !f.RevisionFacts.TouchesAll || encoded(t, f) > limit {
		t.Errorf("the facts of a change to %d files touch all %t in %d bytes, want all, within %d", files+1,
			f.RevisionFacts.TouchesAll, encoded(t, f), limit)
	}
	most := 0
	for most < files && code.TouchedCost(names(name, most+1)) <= code.MaxTouchedBytes {
		most++
	}
	edit := func(n int, content string) string {
		for _, p := range names(name, n) {
			tree[p] = content
		}
		return x.commit(tree, fmt.Sprintf("%d files", n))
	}
	atBound := edit(most, "v3\n")
	f := mustRead(t, s, factsQ(repo, atBound, head))
	if f.RevisionFacts.TouchesAll || len(f.RevisionFacts.Touched) != most || encoded(t, f) > limit {
		t.Errorf("the facts of a change to the %d files a list holds list %d (touch all %t) in %d bytes, want "+
			"every one, within %d", most, len(f.RevisionFacts.Touched), f.RevisionFacts.TouchesAll, encoded(t, f),
			limit)
	}
	pastBound := edit(most+1, "v4\n")
	if f := mustRead(t, s, factsQ(repo, pastBound, atBound)); !f.RevisionFacts.TouchesAll {
		t.Errorf("the facts of a change to %d files list %d, want them to touch all", most+1,
			len(f.RevisionFacts.Touched))
	}

	// AN INTERDIFF kept to that list, a page at a time; and a re-anchoring of
	// the most anchors, their ids at their longest with every byte escaped and
	// their lines at their widest.
	pages(t, s, interdiffQ(repo, code.InterdiffQuery{FromBase: base, FromHead: head, ToBase: head,
		ToHead: atBound, ToTouched: f.RevisionFacts.Touched}), limit)
	var anchors []code.LineAnchor
	blob := strings.TrimSpace(x.git("rev-parse", atBound+":huge.txt"))
	for i := range code.MaxReanchors {
		anchors = append(anchors, code.LineAnchor{ID: fmt.Sprintf("%02d%s", i, strings.Repeat("<", 62)),
			Path: "huge.txt", Side: code.SideNew, BlobOID: blob, Start: math.MaxInt - 1, End: math.MaxInt - 1})
	}
	if a := mustRead(t, s, reanchorQ(repo, code.ReanchorQuery{Head: atBound, Anchors: anchors})); encoded(t, a) > limit {
		t.Errorf("the re-anchoring of %d anchors encodes to %d bytes, past %d", len(anchors), encoded(t, a), limit)
	}
}

// names is the first n of the fixture's wide paths, by name.
func names(name func(int) string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = name(i)
	}
	return out
}

// A PAGE IS CUT AGAINST THE WHOLE ANSWER. Every field an answer carries is in
// the envelope a page is measured against — the repository and the position
// Read stamps on it after the kind has answered included — so swept across
// bounds a byte apart in effect (a prime step over a range of several items'
// worth), where some page somewhere lands within a few bytes of its bound,
// every page of every kind still encodes within it.
func TestAPageIsCutAgainstTheWholeAnswer(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	tree := map[string]string{}
	var lines strings.Builder
	for i := range 300 {
		fmt.Fprintf(&lines, "line %d %s\n", i, strings.Repeat("<x", i%17))
	}
	for i := range 60 {
		tree[fmt.Sprintf("dir/%02d-%s", i, strings.Repeat("p", 60+i))] = "v1\n"
	}
	tree["lines.txt"] = lines.String()
	base := x.commit(tree, "base")
	for i := range 60 {
		tree[fmt.Sprintf("dir/%02d-%s", i, strings.Repeat("p", 60+i))] = "v2\n"
	}
	tree["lines.txt"] = strings.ReplaceAll(lines.String(), "<x", "<y")
	head := x.commit(tree, "head")
	repo := x.repo(codetest.RepoName)
	// The sweep is of the room each kind's own content has: a diff names two
	// commits, the most any read here decorates, so every bound leaves at
	// least that room beside its provenance.
	deco := x.decoration(diffQ(repo, code.DiffQuery{Base: base, Head: head}))
	for limit := 900 + deco; limit <= 1600+deco; limit += 41 {
		s := x.server(0, query.Options{})
		query.SetPageBytes(s, limit)
		pages(t, s, treeQ(repo, code.TreeQuery{Rev: head, Recursive: true}), limit)
		pages(t, s, blobQ(repo, code.BlobQuery{Rev: head, Path: "lines.txt"}), limit)
		pages(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head}), limit)
		pages(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, File: "lines.txt"}), limit)
	}
}

// A CLASSED FACTS ANSWER IS ONE PAGE: the room a revision's touched list has
// is what its answer leaves with every other field at its widest — its
// classes naming every place the list may hold among them — so swept across
// page bounds a few bytes apart around the bound at which the list of a
// change whose every file is classed just fits, every answer encodes within
// its page: a list its classes would take past the page touches all, and
// classes nothing, instead.
//
// Mutation: leave the classes out of the envelope the list's room is measured
// against — a bound where the list fits alone and not beside its classes
// answers past its page, red.
func TestAClassedFactsAnswerIsOnePage(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	const files = 40
	before, after := map[string]string{}, map[string]string{}
	for i := range files {
		p := fmt.Sprintf("w/%02d-%s.go", i, strings.Repeat("g", 20))
		before[p], after[p] = "package p\n\nvar x = 1\n", "package p\n\nvar x  = 1\n"
	}
	base := x.commit(before, "base")
	head := x.commit(after, "head")
	repo := x.repo(codetest.RepoName)
	whole := mustRead(t, x.server(0, query.Options{}), factsQ(repo, head, base))
	if f := whole.RevisionFacts; f.TouchesAll || len(f.Classes.WhitespaceOnly) != files || !f.Classes.Complete {
		t.Fatalf("the premise: the facts of %d files reformatted list %d paths (touch all %t) and class %d (%t)",
			files, len(f.Touched), f.TouchesAll, len(f.Classes.WhitespaceOnly), f.Classes.Complete)
	}
	size := encoded(t, whole)
	listed, all := 0, 0
	for limit := size - 400; limit <= size+200; limit += 7 {
		s := x.server(0, query.Options{})
		query.SetPageBytes(s, limit)
		query.SetTouchedBounds(s, files, code.MaxTouchedBytes)
		a := mustRead(t, s, factsQ(repo, head, base))
		if n := encoded(t, a); n > limit {
			t.Errorf("at a page of %d bytes the facts encode to %d, listing %d paths and classing %d", limit, n,
				len(a.RevisionFacts.Touched), len(a.RevisionFacts.Classes.WhitespaceOnly))
		}
		if a.RevisionFacts.TouchesAll {
			all++
		} else {
			listed++
		}
	}
	if listed == 0 || all == 0 {
		t.Fatalf("the sweep listed the paths at %d bounds and touched all at %d: it crossed no bound", listed, all)
	}
}

// A PAGE IS COSTED IN THE ENCODER'S OWN TERMS: what the page cutter counts
// for any text — every ASCII byte, the escaped ones among them, runes of two,
// three and four bytes, U+2028 and U+2029, and bytes that are not UTF-8, alone,
// at the text's end or cutting a rune short — is exactly what encoding/json
// writes for it.
func TestTheTextCostIsJSONs(t *testing.T) {
	t.Parallel()
	alphabet := []string{"a", "<", ">", "&", "\"", "\\", "\n", "\t", "\b", "\f", "\x00", "\x01", "\x1f",
		"\x7f", "é", "€", "😀", " ", " ", "\xff", "\xe9", "\xe2\x82"}
	for b := range utf8.RuneSelf {
		alphabet = append(alphabet, string([]byte{byte(b)}))
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		var s strings.Builder
		for range rng.IntN(40) {
			s.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		out, err := json.Marshal(s.String())
		if err != nil {
			t.Fatal(err)
		}
		if got, want := query.TextCost([]byte(s.String())), len(out)-2; got != want {
			t.Fatalf("the page costs %q at %d bytes; encoding/json writes %d", s.String(), got, want)
		}
	}
}
