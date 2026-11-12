package gitlet_test

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
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
func measured(n int64, head, base string, pushed, target uint64, touched ...string) gitlet.RevisionRow {
	return gitlet.RevisionRow{N: n, Head: oidOf(head), Position: atSeq(pushed),
		Facts: &gitlet.RevisionFactsRow{TargetRef: "refs/heads/main", TargetOID: oidOf("main"),
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
	unmeasured := gitlet.RevisionRow{N: 3, Head: oidOf("head-3"), Position: atSeq(30)}
	unrelated := measured(4, "head-4", "", 40, 38)
	for _, tc := range []struct {
		name  string
		rev   gitlet.RevisionRow
		state gitlet.ChangeState
		based bool
		want  gitlet.FilesGap
	}{
		{"a live change's unmeasured revision", unmeasured, gitlet.ChangeStateOpen, true, gitlet.FilesPending},
		{"a draft's unmeasured revision", unmeasured, gitlet.ChangeStateDraft, false, gitlet.FilesPending},
		{"a merged change's unmeasured revision", unmeasured, gitlet.ChangeStateMerged, true, gitlet.FilesUnrecorded},
		{"a closed change's unmeasured revision", unmeasured, gitlet.ChangeStateClosed, false,
			gitlet.FilesUnrecorded},
		{"an unrelated revision read from its base", unrelated, gitlet.ChangeStateOpen, true, gitlet.FilesUnrelated},
		{"an unrelated revision in an interdiff", unrelated, gitlet.ChangeStateOpen, false, ""},
		{"a measured revision", measured(2, "head-2", base, 20, 18), gitlet.ChangeStateMerged, true, ""},
	} {
		if got := tc.rev.FilesGap(tc.state, tc.based); got != tc.want {
			t.Errorf("%s: the gap is %q, want %q", tc.name, got, tc.want)
		}
		if tc.want != "" && !tc.want.Valid() {
			t.Errorf("%s: %q is not a gap the closed set names", tc.name, tc.want)
		}
	}

	rev := measured(2, "head-2", base, 20, 18, "a.go", "b.go")
	q, err := gitlet.RevisionDiff("repo-1", rev, nil, "a.go", "")
	want := gitlet.GitQuery{Kind: gitlet.GitDiff, RepoID: "repo-1", Diff: &gitlet.DiffQuery{Base: base, Head: rev.Head,
		File: "a.go", Renames: true}}
	if err != nil || !reflect.DeepEqual(q, want) {
		t.Errorf("revision 2's patch of a.go is read as %+v (%v), want %+v", q, err, want)
	}
	earlier := measured(1, "head-1", oidOf("older-base"), 10, 8, "a.go")
	q, err = gitlet.RevisionDiff("repo-1", rev, &earlier, "", "")
	want = gitlet.GitQuery{Kind: gitlet.GitInterdiff, RepoID: "repo-1", Interdiff: &gitlet.InterdiffQuery{
		FromBase: earlier.Facts.BaseOID, FromHead: earlier.Head, FromTouched: []string{"a.go"},
		ToBase: base, ToHead: rev.Head, ToTouched: []string{"a.go", "b.go"}}}
	if err != nil || !reflect.DeepEqual(q, want) {
		t.Errorf("what changed since revision 1 is read as %+v (%v), want %+v", q, err, want)
	}
	if q, err = gitlet.RevisionDiff("repo-1", unrelated, &earlier, "", ""); err != nil || q.Kind != gitlet.GitInterdiff {
		t.Errorf("what an unrelated revision changed since revision 1 is %+v (%v), want an interdiff", q, err)
	}

	for name, read := range map[string]func() (gitlet.GitQuery, error){
		"an unmeasured revision": func() (gitlet.GitQuery, error) {
			return gitlet.RevisionDiff("repo-1", unmeasured, nil, "", "")
		},
		"an unrelated revision from its base": func() (gitlet.GitQuery, error) {
			return gitlet.RevisionDiff("repo-1", unrelated, nil, "", "")
		},
		"since an unmeasured revision": func() (gitlet.GitQuery, error) {
			since := gitlet.RevisionRow{N: 1, Head: oidOf("head-1")}
			return gitlet.RevisionDiff("repo-1", rev, &since, "", "")
		},
		"since a later revision": func() (gitlet.GitQuery, error) {
			return gitlet.RevisionDiff("repo-1", earlier, &rev, "", "")
		},
	} {
		if q, err := read(); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("%s is read as %+v (%v), want refused before any Gitlet node is asked", name, q, err)
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
	unmeasured := gitlet.RevisionRow{N: 4, Head: oidOf("head-4"), Position: atSeq(40)}
	for _, tc := range []struct {
		name string
		own  statelog.Position
		revs []gitlet.RevisionRow
		want statelog.Freshness
	}{
		{"a target moved after the head", statelog.Position{}, []gitlet.RevisionRow{head, rev},
			statelog.FloorAt(statelog.ReadSession, atSeq(34))},
		{"this node's own later push", atSeq(50), []gitlet.RevisionRow{head, rev},
			statelog.FloorAt(statelog.ReadSession, atSeq(50))},
		{"an unmeasured revision's own record", statelog.Position{}, []gitlet.RevisionRow{rev, unmeasured},
			statelog.FloorAt(statelog.ReadSession, atSeq(40))},
		{"nothing held", statelog.Position{}, nil, statelog.Freshness{}},
	} {
		if got := gitlet.ObjectFloor(tc.own, tc.revs...); !reflect.DeepEqual(got, tc.want) {
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
	finding := func(id string, revision int64, side gitlet.Side) gitlet.FindingRow {
		return gitlet.FindingRow{ID: id, Revision: revision, Path: "retry.go", Side: side, BlobOID: oidOf(id),
			LineStart: 10, LineEnd: 12}
	}
	page := []gitlet.FindingRow{finding("here", 2, gitlet.SideNew), finding("old-side", 1, gitlet.SideOld),
		finding("new-side", 1, gitlet.SideNew)}

	rev := measured(2, "head-2", oidOf("base"), 20, 18)
	placed, reads := gitlet.PlaceFindings(rev, page)
	if want := map[string]gitlet.FindingPlace{"here": {Start: 10, End: 12}}; !maps.Equal(placed, want) {
		t.Errorf("placed without a read %v, want %v", placed, want)
	}
	want := []*gitlet.ReanchorQuery{{Base: oidOf("base"), Head: rev.Head, Anchors: []gitlet.LineAnchor{
		{ID: "old-side", Path: "retry.go", Side: gitlet.SideOld, BlobOID: oidOf("old-side"), Start: 10, End: 12},
		{ID: "new-side", Path: "retry.go", Side: gitlet.SideNew, BlobOID: oidOf("new-side"), Start: 10, End: 12}}}}
	if !reflect.DeepEqual(reads, want) {
		t.Fatalf("the re-anchorings asked are %+v, want %+v", reads, want)
	}
	reanchor := reads[0]
	if err := gitlet.PlaceAnchors(placed, reanchor, &gitlet.ReanchorAnswer{Anchors: []gitlet.Reanchored{
		{ID: "new-side", Start: 14, End: 16}, {ID: "old-side", Gone: true}}}); err != nil {
		t.Fatalf("a whole answer was refused: %v", err)
	}
	if want := map[string]gitlet.FindingPlace{"here": {Start: 10, End: 12}, "new-side": {Start: 14, End: 16},
		"old-side": {Gone: true}}; !maps.Equal(placed, want) {
		t.Errorf("placed %v, want %v", placed, want)
	}
	for name, answer := range map[string]*gitlet.ReanchorAnswer{
		"no answer":           nil,
		"one anchor short":    {Anchors: []gitlet.Reanchored{{ID: "new-side"}}},
		"an anchor not asked": {Anchors: []gitlet.Reanchored{{ID: "new-side"}, {ID: "here"}}},
		"one anchor twice":    {Anchors: []gitlet.Reanchored{{ID: "new-side"}, {ID: "new-side"}}},
	} {
		if err := gitlet.PlaceAnchors(map[string]gitlet.FindingPlace{}, reanchor, answer); err == nil {
			t.Errorf("%s: an answer that is not the anchors asked was folded in", name)
		}
	}

	unrelated := measured(2, "head-2", "", 20, 18)
	placed, reads = gitlet.PlaceFindings(unrelated, page)
	if placed["old-side"] != (gitlet.FindingPlace{Gone: true}) || len(reads) != 1 ||
		len(reads[0].Anchors) != 1 || reads[0].Anchors[0].ID != "new-side" || reads[0].Base != "" {
		t.Errorf("in a revision sharing no history placed %v and asked %+v, want the old side gone and the new "+
			"side one anchor", placed, reads)
	}
	unmeasured := gitlet.RevisionRow{N: 2, Head: oidOf("head-2"), Position: atSeq(20)}
	placed, reads = gitlet.PlaceFindings(unmeasured, page)
	if _, held := placed["old-side"]; held || len(reads) != 1 || len(reads[0].Anchors) != 1 {
		t.Errorf("in an unmeasured revision placed %v and asked %+v, want the old side unplaced", placed, reads)
	}
	if placed, reads = gitlet.PlaceFindings(rev, page[:1]); reads != nil || len(placed) != 1 {
		t.Errorf("a page of the revision's own findings asked %+v, want no read", reads)
	}
}

// A PAGE OF FINDINGS IS PLACED WHATEVER ITS SIZE: a Gitlet node's re-anchoring
// places at most gitlet.MaxReanchors anchors, and the reader serves a page of up
// to gitlet.MaxFindingsPerPage findings, every one of which may need one — so a
// page past one read is cut into reads that each pass the Gitlet node's own
// check, that together ask every anchor once, and that keep the anchors of
// one pair of blobs in one read wherever the page allows; and every read's
// answer is folded in, however the reads interleave, while a read that is
// refused answers that refusal — the first in the reads' order.
//
// Mutations: one read of every anchor → the read the Gitlet node refuses, red;
// cut the page in its own order → a pair split across reads, red; fold in only
// the first read's answer → findings unplaced, red; answer the last refusal →
// the wrong refusal, red.
func TestAPageOfFindingsIsPlacedWhateverItsSize(t *testing.T) {
	t.Parallel()
	rev := measured(2, "head-2", oidOf("base"), 20, 18)
	// A FULL PAGE raised on revision 1, interleaving its files, so a page
	// cut in its own order puts each file in every read: file i%5, each
	// file's findings on one blob of it.
	var page []gitlet.FindingRow
	for i := range gitlet.MaxFindingsPerPage {
		path := fmt.Sprintf("pkg/file%d.go", i%5)
		page = append(page, gitlet.FindingRow{ID: fmt.Sprintf("f%03d", i), Revision: 1, Path: path,
			Side: gitlet.SideNew, BlobOID: oidOf(path), LineStart: i + 1, LineEnd: i + 1})
	}
	placed, reads := gitlet.PlaceFindings(rev, page)
	if len(placed) != 0 {
		t.Errorf("placed %d findings of another revision without a read, want none", len(placed))
	}
	wantReads := (gitlet.MaxFindingsPerPage + gitlet.MaxReanchors - 1) / gitlet.MaxReanchors
	if len(reads) != wantReads {
		t.Fatalf("a page of %d findings asked %d re-anchorings, want %d of at most %d", len(page), len(reads),
			wantReads, gitlet.MaxReanchors)
	}
	asked := map[string]int{}
	pairIn := map[string]map[int]bool{}
	for i, q := range reads {
		if err := (gitlet.GitQuery{Kind: gitlet.GitReanchor, RepoID: "01920000-0000-7000-8000-000000000001",
			Asker:    gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ci"},
			Reanchor: q}).Validate(); err != nil {
			t.Errorf("re-anchoring %d is one no Gitlet node takes: %v", i, err)
		}
		for _, a := range q.Anchors {
			asked[a.ID]++
			if pairIn[a.BlobOID] == nil {
				pairIn[a.BlobOID] = map[int]bool{}
			}
			pairIn[a.BlobOID][i] = true
		}
	}
	for _, f := range page {
		if asked[f.ID] != 1 {
			t.Errorf("finding %s was asked in %d re-anchorings, want 1", f.ID, asked[f.ID])
		}
	}
	// 40 findings a file, 64 a read: a file is split across two reads at
	// most where its findings sit together, and every read holds every file
	// where they do not.
	for blob, in := range pairIn {
		if len(in) > 2 {
			t.Errorf("the pair of blob %s was diffed by %d reads, want at most 2", blob, len(in))
		}
	}

	ask := func(q *gitlet.ReanchorQuery) (*gitlet.ReanchorAnswer, error) {
		out := &gitlet.ReanchorAnswer{}
		for _, a := range q.Anchors {
			out.Anchors = append(out.Anchors, gitlet.Reanchored{ID: a.ID, Start: a.Start + 1, End: a.End + 1})
		}
		return out, nil
	}
	placed, err := gitlet.Place(rev, page, ask)
	if err != nil {
		t.Fatalf("a page answered whole was refused: %v", err)
	}
	for _, f := range page {
		if at, ok := placed[f.ID]; !ok || at.Start != f.LineStart+1 {
			t.Errorf("finding %s placed %+v (%t), want line %d", f.ID, at, ok, f.LineStart+1)
		}
	}

	// THE FIRST REFUSAL IN THE READS' ORDER, however late it arrives: the
	// first read is refused only once the second has been.
	first, second := errors.New("first"), errors.New("second")
	secondDone := make(chan struct{})
	refusing := func(q *gitlet.ReanchorQuery) (*gitlet.ReanchorAnswer, error) {
		switch q.Anchors[0].ID {
		case reads[0].Anchors[0].ID:
			<-secondDone
			return nil, first
		case reads[1].Anchors[0].ID:
			defer close(secondDone)
			return nil, second
		}
		return ask(q)
	}
	if _, err := gitlet.Place(rev, page, refusing); !errors.Is(err, first) {
		t.Errorf("a page two of whose reads were refused answered %v, want the first read's refusal", err)
	}
	misplacing := func(q *gitlet.ReanchorQuery) (*gitlet.ReanchorAnswer, error) {
		out, _ := ask(q)
		out.Anchors = out.Anchors[1:]
		return out, nil
	}
	if _, err := gitlet.Place(rev, page, misplacing); !errors.Is(err, gitlet.ErrMisplaced) {
		t.Errorf("a read answering one anchor short answered %v, want ErrMisplaced", err)
	}
}

// meeting is a barrier of two reads, each answering whether the other arrived
// while it waited: a read made only after the other has answered meets
// nobody. meetWait bounds the wait — far past a loaded runner's scheduling
// delay, and short enough that a read made alone fails the case quickly.
type meeting struct {
	arrived atomic.Int32
	both    chan struct{}
}

const meetWait = 5 * time.Second

func newMeeting() *meeting { return &meeting{both: make(chan struct{})} }

func (m *meeting) meet() bool {
	if m.arrived.Add(1) == 2 {
		close(m.both)
	}
	select {
	case <-m.both:
		return true
	case <-time.After(meetWait): //fixedwait:liveness
		return false
	}
}

// A CHANGE'S REVISIONS ARE READ AT ONCE: once its head is read, the revision
// asked about and the earlier one an interdiff reads from need nothing of
// each other, so neither waits for the other's answer — a node reading the
// estate through another pays one round trip for the two; the head is never
// read again; and of two refusals the one answered is the revision asked
// about's, however late it arrives.
//
// Mutations: AtOnce reads one after the other → the two reads never meet,
// red; read the head again → a read too many, red; answer the first refusal
// to arrive → the earlier revision's, red.
func TestAChangesRevisionsAreReadAtOnce(t *testing.T) {
	t.Parallel()
	head := measured(3, "head-3", oidOf("base"), 30, 28)
	m := newMeeting()
	var reads atomic.Int32
	rev, earlier, err := gitlet.ChangeRevisions(head, 2, 1, func(n int64) (gitlet.RevisionRow, error) {
		reads.Add(1)
		if !m.meet() {
			return gitlet.RevisionRow{}, fmt.Errorf("revision %d was read alone", n)
		}
		return measured(n, fmt.Sprintf("head-%d", n), oidOf("base"), uint64(10*n), uint64(10*n-2)), nil
	})
	if err != nil || rev.N != 2 || earlier == nil || earlier.N != 1 || reads.Load() != 2 {
		t.Fatalf("revisions 2 and 1 were read as %d, %+v in %d reads (%v), want each once, at once", rev.N, earlier,
			reads.Load(), err)
	}

	for _, tc := range []struct {
		name      string
		n, since  int64
		wantReads int32
	}{
		{"the head by its number", 3, 0, 0},
		{"the head by default", 0, 0, 0},
		{"the head since an earlier revision", 3, 1, 1},
	} {
		var reads atomic.Int32
		rev, earlier, err := gitlet.ChangeRevisions(head, tc.n, tc.since, func(n int64) (gitlet.RevisionRow, error) {
			reads.Add(1)
			return measured(n, fmt.Sprintf("head-%d", n), "", 0, 0), nil
		})
		if err != nil || rev.N != head.N || rev.Head != head.Head || reads.Load() != tc.wantReads ||
			(earlier != nil) != (tc.since != 0) {
			t.Errorf("%s: read revision %d and %+v in %d reads (%v), want the head unread and %d reads", tc.name,
				rev.N, earlier, reads.Load(), err, tc.wantReads)
		}
	}

	asked, since := errors.New("no revision 2"), errors.New("no revision 1")
	sinceDone := make(chan struct{})
	_, _, err = gitlet.ChangeRevisions(head, 2, 1, func(n int64) (gitlet.RevisionRow, error) {
		if n == 1 {
			defer close(sinceDone)
			return gitlet.RevisionRow{}, since
		}
		<-sinceDone
		return gitlet.RevisionRow{}, asked
	})
	if !errors.Is(err, asked) {
		t.Errorf("two refused revisions answered %v, want the revision asked about's refusal", err)
	}
}
