package code_test

import (
	"errors"
	"maps"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S REVISIONS READ BY OBJECT ID (revisionread.go): the one rule both
// the code tools and the API's change question read a revision's files and
// place its findings by.

// atSeq is a position on the code log's first generation.
func atSeq(seq uint64) statelog.Position {
	return statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: seq}
}

// measured is revision n of a change, pushed at the record at pushed, its head
// headed, and its facts recorded against a target its home read at target —
// base its merge base, "" for a history sharing none with the target.
func measured(n int64, head, base string, pushed, target uint64, touched ...string) code.RevisionRow {
	return code.RevisionRow{N: n, Head: oidOf(head), Position: atSeq(pushed),
		Facts: &code.RevisionFactsRow{TargetRef: "refs/heads/main", TargetOID: oidOf("main"),
			TargetPosition: atSeq(target), BaseOID: base, Touched: touched}}
}

// A REVISION'S FILES ARE READ FROM THE BASE ITS FACTS RECORD, or not at all:
// a revision with no facts is pending on a live change — its home records them
// within a minute — and unrecorded on one that ended; one whose facts record
// no base shares no history with its target and has no base to read its files
// from, though an interdiff of it, which reads no base, still reads; and the
// read is the diff from the recorded base to the head, renames read as such,
// or the two revisions' interdiff kept to the paths either touches.
//
// Mutations: read an ended change's revision with no facts as pending → red;
// diff a revision from the target's tip rather than its base → red; drop the
// later revision's touched paths from the interdiff → red.
func TestARevisionsFilesAreReadFromTheBaseItsFactsRecord(t *testing.T) {
	t.Parallel()
	base := oidOf("base")
	unmeasured := code.RevisionRow{N: 3, Head: oidOf("head-3"), Position: atSeq(30)}
	unrelated := measured(4, "head-4", "", 40, 38)
	for _, tc := range []struct {
		name  string
		rev   code.RevisionRow
		state code.ChangeState
		based bool
		want  code.FilesGap
	}{
		{"a live change's unmeasured revision", unmeasured, code.ChangeStateOpen, true, code.FilesPending},
		{"a draft's unmeasured revision", unmeasured, code.ChangeStateDraft, false, code.FilesPending},
		{"a merged change's unmeasured revision", unmeasured, code.ChangeStateMerged, true, code.FilesUnrecorded},
		{"a closed change's unmeasured revision", unmeasured, code.ChangeStateClosed, false,
			code.FilesUnrecorded},
		{"an unrelated revision read from its base", unrelated, code.ChangeStateOpen, true, code.FilesUnrelated},
		{"an unrelated revision in an interdiff", unrelated, code.ChangeStateOpen, false, ""},
		{"a measured revision", measured(2, "head-2", base, 20, 18), code.ChangeStateMerged, true, ""},
	} {
		if got := tc.rev.FilesGap(tc.state, tc.based); got != tc.want {
			t.Errorf("%s: the gap is %q, want %q", tc.name, got, tc.want)
		}
		if tc.want != "" && !tc.want.Valid() {
			t.Errorf("%s: %q is not a gap the closed set names", tc.name, tc.want)
		}
	}

	rev := measured(2, "head-2", base, 20, 18, "a.go", "b.go")
	q, err := code.RevisionDiff("repo-1", rev, nil, "a.go", "")
	want := code.GitQuery{Kind: code.GitDiff, RepoID: "repo-1", Diff: &code.DiffQuery{Base: base, Head: rev.Head,
		File: "a.go", Renames: true}}
	if err != nil || !reflect.DeepEqual(q, want) {
		t.Errorf("revision 2's patch of a.go is read as %+v (%v), want %+v", q, err, want)
	}
	earlier := measured(1, "head-1", oidOf("older-base"), 10, 8, "a.go")
	q, err = code.RevisionDiff("repo-1", rev, &earlier, "", "")
	want = code.GitQuery{Kind: code.GitInterdiff, RepoID: "repo-1", Interdiff: &code.InterdiffQuery{
		FromBase: earlier.Facts.BaseOID, FromHead: earlier.Head, FromTouched: []string{"a.go"},
		ToBase: base, ToHead: rev.Head, ToTouched: []string{"a.go", "b.go"}}}
	if err != nil || !reflect.DeepEqual(q, want) {
		t.Errorf("what changed since revision 1 is read as %+v (%v), want %+v", q, err, want)
	}
	if q, err = code.RevisionDiff("repo-1", unrelated, &earlier, "", ""); err != nil || q.Kind != code.GitInterdiff {
		t.Errorf("what an unrelated revision changed since revision 1 is %+v (%v), want an interdiff", q, err)
	}

	for name, read := range map[string]func() (code.GitQuery, error){
		"an unmeasured revision": func() (code.GitQuery, error) {
			return code.RevisionDiff("repo-1", unmeasured, nil, "", "")
		},
		"an unrelated revision from its base": func() (code.GitQuery, error) {
			return code.RevisionDiff("repo-1", unrelated, nil, "", "")
		},
		"since an unmeasured revision": func() (code.GitQuery, error) {
			since := code.RevisionRow{N: 1, Head: oidOf("head-1")}
			return code.RevisionDiff("repo-1", rev, &since, "", "")
		},
		"since a later revision": func() (code.GitQuery, error) {
			return code.RevisionDiff("repo-1", earlier, &rev, "", "")
		},
	} {
		if q, err := read(); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("%s is read as %+v (%v), want refused before any git node is asked", name, q, err)
		}
	}
}

// A READ BY OBJECT ID IS FLOORED AT THE RECORDS THAT MOVED ITS REVISIONS: the
// furthest of the asking node's own writes to the repository, each revision's
// own record and the record that moved its target to the tip its base was read
// from — so the copy that answers holds every object the rows name — and no
// floor at all where none of them is held.
//
// Mutation: drop the target's record from the floor → a revision measured
// against a tip pushed after it is read from a copy that may not hold its
// base, red.
func TestAReadByObjectIDIsFlooredAtTheRecordsThatMovedItsRevisions(t *testing.T) {
	t.Parallel()
	head := measured(3, "head-3", oidOf("base"), 30, 34)
	rev := measured(2, "head-2", oidOf("base"), 20, 18)
	unmeasured := code.RevisionRow{N: 4, Head: oidOf("head-4"), Position: atSeq(40)}
	for _, tc := range []struct {
		name string
		own  statelog.Position
		revs []code.RevisionRow
		want statelog.Freshness
	}{
		{"a target moved after the head", statelog.Position{}, []code.RevisionRow{head, rev},
			statelog.FloorAt(statelog.ReadSession, atSeq(34))},
		{"this node's own later push", atSeq(50), []code.RevisionRow{head, rev},
			statelog.FloorAt(statelog.ReadSession, atSeq(50))},
		{"an unmeasured revision's own record", statelog.Position{}, []code.RevisionRow{rev, unmeasured},
			statelog.FloorAt(statelog.ReadSession, atSeq(40))},
		{"nothing held", statelog.Position{}, nil, statelog.Freshness{}},
	} {
		if got := code.ObjectFloor(tc.own, tc.revs...); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: floored at %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A FINDING IS PLACED WITHOUT A READ WHERE THE ROWS SAY WHERE IT IS: one raised
// on the revision asked about is where it was raised; one on the old side of a
// revision sharing no history with its target is gone, its old side the empty
// tree; one on the old side of a revision with no facts yet has no place; and
// every other is ONE anchor of one re-anchoring of the revision, its base
// beside its head, whose answer places each anchor asked — and nothing but
// them.
//
// Mutations: place an old-side finding of an unmeasured revision as gone →
// red; anchor a finding raised on the revision asked about → red; accept an
// answer that placed an anchor nobody asked → red.
func TestAFindingIsPlacedWithoutAReadWhereTheRowsSay(t *testing.T) {
	t.Parallel()
	finding := func(id string, revision int64, side code.Side) code.FindingRow {
		return code.FindingRow{ID: id, Revision: revision, Path: "retry.go", Side: side, BlobOID: oidOf(id),
			LineStart: 10, LineEnd: 12}
	}
	page := []code.FindingRow{finding("here", 2, code.SideNew), finding("old-side", 1, code.SideOld),
		finding("new-side", 1, code.SideNew)}

	rev := measured(2, "head-2", oidOf("base"), 20, 18)
	placed, reanchor := code.PlaceFindings(rev, page)
	if want := map[string]code.FindingPlace{"here": {Start: 10, End: 12}}; !maps.Equal(placed, want) {
		t.Errorf("placed without a read %v, want %v", placed, want)
	}
	want := &code.ReanchorQuery{Base: oidOf("base"), Head: rev.Head, Anchors: []code.LineAnchor{
		{ID: "old-side", Path: "retry.go", Side: code.SideOld, BlobOID: oidOf("old-side"), Start: 10, End: 12},
		{ID: "new-side", Path: "retry.go", Side: code.SideNew, BlobOID: oidOf("new-side"), Start: 10, End: 12}}}
	if !reflect.DeepEqual(reanchor, want) {
		t.Fatalf("the re-anchoring asked is %+v, want %+v", reanchor, want)
	}
	if err := code.PlaceAnchors(placed, reanchor, &code.ReanchorAnswer{Anchors: []code.Reanchored{
		{ID: "new-side", Start: 14, End: 16}, {ID: "old-side", Gone: true}}}); err != nil {
		t.Fatalf("a whole answer was refused: %v", err)
	}
	if want := map[string]code.FindingPlace{"here": {Start: 10, End: 12}, "new-side": {Start: 14, End: 16},
		"old-side": {Gone: true}}; !maps.Equal(placed, want) {
		t.Errorf("placed %v, want %v", placed, want)
	}
	for name, answer := range map[string]*code.ReanchorAnswer{
		"no answer":           nil,
		"one anchor short":    {Anchors: []code.Reanchored{{ID: "new-side"}}},
		"an anchor not asked": {Anchors: []code.Reanchored{{ID: "new-side"}, {ID: "here"}}},
		"one anchor twice":    {Anchors: []code.Reanchored{{ID: "new-side"}, {ID: "new-side"}}},
	} {
		if err := code.PlaceAnchors(map[string]code.FindingPlace{}, reanchor, answer); err == nil {
			t.Errorf("%s: an answer that is not the anchors asked was folded in", name)
		}
	}

	unrelated := measured(2, "head-2", "", 20, 18)
	placed, reanchor = code.PlaceFindings(unrelated, page)
	if placed["old-side"] != (code.FindingPlace{Gone: true}) || reanchor == nil ||
		len(reanchor.Anchors) != 1 || reanchor.Anchors[0].ID != "new-side" || reanchor.Base != "" {
		t.Errorf("in a revision sharing no history placed %v and asked %+v, want the old side gone and the new "+
			"side one anchor", placed, reanchor)
	}
	unmeasured := code.RevisionRow{N: 2, Head: oidOf("head-2"), Position: atSeq(20)}
	placed, reanchor = code.PlaceFindings(unmeasured, page)
	if _, held := placed["old-side"]; held || reanchor == nil || len(reanchor.Anchors) != 1 {
		t.Errorf("in an unmeasured revision placed %v and asked %+v, want the old side unplaced", placed, reanchor)
	}
	if placed, reanchor = code.PlaceFindings(rev, page[:1]); reanchor != nil || len(placed) != 1 {
		t.Errorf("a page of the revision's own findings asked %+v, want no read", reanchor)
	}
}
