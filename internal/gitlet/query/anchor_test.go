package query_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// AN ANCHOR ON A CHANGED LINE IS OUTDATED. Each range is placed by the diff
// between the blob it was read in and the file at its path now: moved by two
// lines inserted above, unmoved by changes below; OUTDATED where a line of its
// own was replaced, alone or among others, or the file is binary on either
// side; GONE where no file is at the path; unmoved where the file is its blob.
// An old-side range is placed in the base, never the head. One diff-pairs
// diffs every pair, handed numbers for paths, never an anchor's.
//
// Mutation: place an anchor whose own line changed as moved (Reanchor's
// overlap read as above it) — the changed and spanning rows red.
func TestAnAnchorOnAChangedLineIsOutdated(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	first := x.commit(map[string]string{"f.txt": numbered(20, nil, 0), "g.txt": "g\n", "h.txt": "h1\nh2\n",
		"bin.dat": "a\x00b"}, "the revision the anchors were read in")
	x.git("rm", "-q", "g.txt")
	second := x.commit(map[string]string{"f.txt": numbered(20, map[int]string{10: "line ten"}, 2),
		"bin.dat": "a\x00c"}, "the revision they are placed in")
	x.branch("older", first)
	base := x.p.Commit(x.work, map[string]string{"f.txt": numbered(20, nil, 1)}, "a base with one line above")
	x.push(gitlettest.RepoName, "refs/heads/older")
	repo := x.repo(gitlettest.RepoName)
	blob := func(path string) string { return strings.TrimSpace(x.git("rev-parse", first+":"+path)) }
	f, g, h, bin := blob("f.txt"), blob("g.txt"), blob("h.txt"), blob("bin.dat")

	anchors := []gitlet.LineAnchor{
		{ID: "moved", Path: "f.txt", Side: gitlet.SideNew, BlobOID: f, Start: 5, End: 6},
		{ID: "changed", Path: "f.txt", Side: gitlet.SideNew, BlobOID: f, Start: 10, End: 10},
		{ID: "spanning", Path: "f.txt", Side: gitlet.SideNew, BlobOID: f, Start: 9, End: 11},
		{ID: "below", Path: "f.txt", Side: gitlet.SideNew, BlobOID: f, Start: 12, End: 15},
		{ID: "gone", Path: "g.txt", Side: gitlet.SideNew, BlobOID: g, Start: 1, End: 1},
		{ID: "same", Path: "h.txt", Side: gitlet.SideNew, BlobOID: h, Start: 2, End: 2},
		{ID: "binary", Path: "bin.dat", Side: gitlet.SideNew, BlobOID: bin, Start: 1, End: 1},
		{ID: "old side", Path: "f.txt", Side: gitlet.SideOld, BlobOID: f, Start: 5, End: 6},
	}
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	a := mustRead(t, s, reanchorQ(repo, gitlet.ReanchorQuery{Base: base, Head: second, Anchors: anchors}))
	want := []gitlet.Reanchored{
		{ID: "moved", Start: 7, End: 8},
		{ID: "changed", Start: 12, End: 12, Outdated: true},
		{ID: "spanning", Start: 11, End: 13, Outdated: true},
		{ID: "below", Start: 14, End: 17},
		{ID: "gone", Gone: true},
		{ID: "same", Start: 2, End: 2},
		{ID: "binary", Start: 1, End: 1, Outdated: true},
		{ID: "old side", Start: 6, End: 7},
	}
	if !slices.Equal(a.Reanchor.Anchors, want) {
		t.Errorf("the anchors placed in %s are\n\t%+v\nwant\n\t%+v", second, a.Reanchor.Anchors, want)
	}
	if pairs := rec.fed("diff-pairs"); len(pairs) != 1 || strings.Contains(pairs[0], "f.txt") {
		t.Errorf("the anchors were diffed by %d diff-pairs fed %q, want one, fed no path", len(pairs), pairs)
	}

	// In the revision they were read in, every anchor is where it was.
	same := mustRead(t, s, reanchorQ(repo, gitlet.ReanchorQuery{Head: first, Anchors: anchors[:7]}))
	for i, r := range same.Reanchor.Anchors {
		if an := anchors[i]; r != (gitlet.Reanchored{ID: an.ID, Start: an.Start, End: an.End}) {
			t.Errorf("anchor %s placed in the revision it was read in = %+v, want lines %d to %d", an.ID, r,
				an.Start, an.End)
		}
	}
}
