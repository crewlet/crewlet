package code

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S REVISIONS READ BY OBJECT ID: which commits a revision's files and
// a file's patch are read between, whether the rows hold what that read needs,
// where each of a change's findings is in the revision asked about, and the
// floor every such read is asked at — decided HERE, once, over the rows a
// `change` read answers, and read by every surface that shows a change's code:
// the code tools' code_get_change and the API's code_change question. Two
// copies of the rule would show a seat and a person two diffs of one revision,
// or one finding on two different lines.
//
// NOTHING HERE READS: each surface makes its own reads — through the estate
// router, as its caller — and says, in its own words, what it could not read.
// What is decided here is which reads an answer takes, and that the ones that
// need nothing of each other go AT ONCE ([AtOnce]): a revision read beside
// another once the head is read ([ChangeRevisions]), and the re-anchorings of
// one page of findings ([Place]). A node without the data role asks each read
// of the estate through another node, so every read made after another
// rather than beside it is one more round trip in the answer — and both
// surfaces make the same reads, in the same order, by the same rule.
//
// # Files from the recorded base
//
// A revision's files are what its head changes against the BASE its home
// recorded in the revision's facts (code_revision_facts) — the merge base of
// its head and its target's tip as the home read it — never the target's tip
// now, which moves under the change and would show another change's work as
// this one's. What changed BETWEEN two revisions is their INTERDIFF, the two
// heads' diff kept to the paths either revision touches (Gitlet node's
// `interdiff`), since a rebase onto a moved target changes every file the
// target's own commits touched and the change none of them.
//
// # Where a finding is now
//
// A finding names lines of the revision it was raised on. In another revision
// they are where the lines changed above them moved them — OUTDATED where its
// own lines changed, GONE where the revision holds no file at its path — which
// re-anchoring reads of the revision asked about place for a page of findings
// ([PlaceFindings]): one read for every [MaxReanchors] findings the rows do
// not place, asked at once, so a page of any size the reader serves
// ([MaxFindingsPerPage]) is placed, and a page the code tools read is one
// read.

// FilesGap is why a revision's files cannot be read from the facts its rows
// hold: a closed set with [FilesGap.Valid], "" for a revision whose files can.
type FilesGap string

const (
	// FilesPending is a LIVE change's revision whose facts its home has not
	// recorded yet: the home records each revision's facts against its
	// target within a minute of its push (home.Backstop), so a read of its
	// files clears by waiting.
	FilesPending FilesGap = "pending"
	// FilesUnrecorded is a revision of a change that ended — merged or
	// closed — before its home recorded the revision's facts, which it never
	// will: a change no longer live is measured no more.
	FilesUnrecorded FilesGap = "unrecorded"
	// FilesUnrelated is a revision whose head shares no history with its
	// target, so its facts record no base: its change is every file of its
	// head, which a read of its tree shows, and no diff from a base does.
	FilesUnrelated FilesGap = "unrelated"
)

// FilesGaps is the closed set, in the order a revision meets them.
func FilesGaps() []FilesGap { return []FilesGap{FilesPending, FilesUnrecorded, FilesUnrelated} }

// Valid reports a gap this build names.
func (g FilesGap) Valid() bool { return slices.Contains(FilesGaps(), g) }

// FilesGap is why r — a revision of a change in state — cannot have its files
// read from its facts, and "" where it can. based is a read FROM r's base, its
// own files or one of their patches, which a revision sharing no history with
// its target has none of; an interdiff names two revisions' bases as ids and
// reads neither, so it needs only that each revision's facts are recorded.
func (r RevisionRow) FilesGap(state ChangeState, based bool) FilesGap {
	switch {
	case r.Facts == nil && state.Live():
		return FilesPending
	case r.Facts == nil:
		return FilesUnrecorded
	case based && r.Facts.BaseOID == "":
		return FilesUnrelated
	}
	return ""
}

// RevisionDiff is the read by object id of revision rev's files on repository
// repoID — from the base its facts record to its head, a file moved between
// them read as renamed — or, where since names an EARLIER revision, the
// interdiff of the two, kept to the paths either touches; with file (or
// fileB64, the same path as its exact bytes) one file's patch rather than the
// files. The read's asker, its floor ([ObjectFloor]) and its cursor are the
// caller's to set.
//
// It reads only facts the rows hold — every revision it names has them, and
// rev a base where since is nil ([RevisionRow.FilesGap] "" for each) — and is
// refused [ErrInvalid] otherwise, naming the revision, rather than sent with
// an empty id that every Gitlet node would refuse alike: a caller judges the gap
// first, in its own words.
func RevisionDiff(repoID string, rev RevisionRow, since *RevisionRow, file, fileB64 string) (GitQuery, error) {
	switch {
	case rev.Facts == nil:
		return GitQuery{}, fmt.Errorf("%w: revision %d holds no facts to read its files from", ErrInvalid, rev.N)
	case since == nil && rev.Facts.BaseOID == "":
		return GitQuery{}, fmt.Errorf("%w: revision %d shares no history with its target, so its facts record no "+
			"base to read its files from", ErrInvalid, rev.N)
	}
	q := GitQuery{RepoID: repoID}
	if since == nil {
		q.Kind, q.Diff = GitDiff, &DiffQuery{Base: rev.Facts.BaseOID, Head: rev.Head, File: file, FileB64: fileB64,
			Renames: true}
		return q, nil
	}
	switch {
	case since.N >= rev.N:
		return GitQuery{}, fmt.Errorf("%w: revision %d is not before revision %d: an interdiff reads what changed "+
			"from an earlier revision to a later one", ErrInvalid, since.N, rev.N)
	case since.Facts == nil:
		return GitQuery{}, fmt.Errorf("%w: revision %d holds no facts to read what changed since it", ErrInvalid,
			since.N)
	}
	q.Kind, q.Interdiff = GitInterdiff, &InterdiffQuery{
		FromBase: since.Facts.BaseOID, FromHead: since.Head, FromTouched: since.Facts.Touched,
		FromTouchesAll: since.Facts.TouchesAll,
		ToBase:         rev.Facts.BaseOID, ToHead: rev.Head, ToTouched: rev.Facts.Touched,
		ToTouchesAll: rev.Facts.TouchesAll, File: file, FileB64: fileB64}
	return q, nil
}

// ObjectFloor is the floor a read by object id of revisions revs is asked at:
// the furthest of own — the asking node's own writes to the repository
// (estate.CodeSeam.FloorOf) — the record that moved each revision's head and,
// where its facts are recorded, the record that moved its target to the tip
// its base was read from; or none, which reads the Gitlet node's copy as it
// stands. So the copy that answers holds every object the revisions' rows
// name.
//
// NEVER A ROW READ'S CHECKPOINT, which a linearizable read takes past its own
// barrier: a copy current to every push still sits below it, and a floor
// there would make the Gitlet node sync for nothing.
func ObjectFloor(own statelog.Position, revs ...RevisionRow) statelog.Freshness {
	floors := []statelog.Position{own}
	for _, r := range revs {
		floors = append(floors, r.Position)
		if r.Facts != nil {
			floors = append(floors, r.Facts.TargetPosition)
		}
	}
	at := statelog.Freshness{Floors: floors}.Floor(topics.CodeLogStream)
	if at.IsZero() {
		return statelog.Freshness{}
	}
	return statelog.FloorAt(statelog.ReadSession, at)
}

// RefsFloor is the floor a read by object id of the commits refs name is
// asked at — [ObjectFloor]'s rule for revisions named by refs rather than by a
// change's rows: the furthest of own (the asking node's own writes to the
// repository, and any floor of its caller's) and the record that last moved
// each ref ([ResolvedRef.Moved]), where the commit it names had landed at the
// latest; or none, which reads the Gitlet node's copy as it stands. A code tool's
// read and a question's read their revisions by it alike.
//
// NEVER THE RESOLVE'S CHECKPOINT ([RefAnswer.Position]), for ObjectFloor's
// reason: a linearizable resolve's is past its own barrier.
func RefsFloor(own statelog.Position, refs ...ResolvedRef) statelog.Freshness {
	floors := []statelog.Position{own}
	for _, r := range refs {
		floors = append(floors, r.Moved)
	}
	at := statelog.Freshness{Floors: floors}.Floor(topics.CodeLogStream)
	if at.IsZero() {
		return statelog.Freshness{}
	}
	return statelog.FloorAt(statelog.ReadSession, at)
}

// FindingPlace is where a finding's lines are in the revision a read asks
// about: moved by the lines changed above them; OUTDATED where its own lines
// changed — then where they would be had only the lines above moved; GONE
// where the revision holds no file at its path.
type FindingPlace struct {
	Start    int  `json:"start,omitempty"`
	End      int  `json:"end,omitempty"`
	Outdated bool `json:"outdated,omitempty"`
	Gone     bool `json:"gone,omitempty"`
}

// PlaceFindings is where each of findings is in revision rev, by the
// finding's id, as far as the rows say without a read — one raised on rev
// where it was raised; one on the OLD side of a revision whose facts record no
// base gone, since its old side is the empty tree, which holds no file — and
// the re-anchorings that place every other, none where none is left, which a
// caller asks of the repository's home Gitlet node at the floor of rev and of
// the change's head revision ([ObjectFloor]) and folds in with
// [PlaceAnchors] — [Place] does both. A finding on the old side of a revision
// whose facts are not recorded yet has no base to place it in: it is in
// neither, and stays unplaced.
//
// EACH RE-ANCHORING PLACES AT MOST [MaxReanchors] ANCHORS, the most one Gitlet
// node's read takes ([ReanchorQuery]). A finding is at most one anchor, so a
// page of up to MaxReanchors findings is one read in the page's order, and a
// larger one — the reader serves up to [MaxFindingsPerPage], four reads'
// worth — is cut into reads of MaxReanchors, its anchors first put in the
// order of their path, side and blob: a read runs one diff of every distinct
// pair of blobs its anchors name, so the anchors of one pair sharing a read
// diff each pair in as few of the reads as the page allows.
func PlaceFindings(rev RevisionRow, findings []FindingRow) (map[string]FindingPlace, []*ReanchorQuery) {
	placed := make(map[string]FindingPlace, len(findings))
	base := ""
	if rev.Facts != nil {
		base = rev.Facts.BaseOID
	}
	var anchors []LineAnchor
	for _, f := range findings {
		switch {
		case f.Revision == rev.N:
			placed[f.ID] = FindingPlace{Start: f.LineStart, End: f.LineEnd}
		case f.Side == SideOld && rev.Facts == nil:
		case f.Side == SideOld && base == "":
			placed[f.ID] = FindingPlace{Gone: true}
		default:
			anchors = append(anchors, LineAnchor{ID: f.ID, Path: f.Path, Side: f.Side, BlobOID: f.BlobOID,
				Start: f.LineStart, End: f.LineEnd})
		}
	}
	switch {
	case len(anchors) == 0:
		return placed, nil
	case len(anchors) <= MaxReanchors:
		return placed, []*ReanchorQuery{{Base: base, Head: rev.Head, Anchors: anchors}}
	}
	// STABLE, so the anchors of one pair keep the page's order among them.
	slices.SortStableFunc(anchors, func(a, b LineAnchor) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), strings.Compare(string(a.Side), string(b.Side)),
			strings.Compare(a.BlobOID, b.BlobOID))
	})
	var reads []*ReanchorQuery
	for chunk := range slices.Chunk(anchors, MaxReanchors) {
		reads = append(reads, &ReanchorQuery{Base: base, Head: rev.Head, Anchors: chunk})
	}
	return placed, reads
}

// Place is where each of findings is in revision rev, by the finding's id:
// [PlaceFindings]'s rows, and every re-anchoring it names asked through ask
// AT ONCE ([AtOnce]) — none needs another's answer — and folded in with
// [PlaceAnchors]. ask is the caller's own read by object id, asked at the
// floor of rev and of the change's head revision ([ObjectFloor]); its error,
// the first in the reads' order, is answered as it is, and an answer placing
// another set than its anchors is [ErrMisplaced].
func Place(rev RevisionRow, findings []FindingRow, ask func(*ReanchorQuery) (*ReanchorAnswer, error)) (
	map[string]FindingPlace, error) {

	placed, reads := PlaceFindings(rev, findings)
	answers := make([]*ReanchorAnswer, len(reads))
	each := make([]func() error, len(reads))
	for i, q := range reads {
		each[i] = func() (err error) {
			answers[i], err = ask(q)
			return err
		}
	}
	if err := AtOnce(each...); err != nil {
		return nil, err
	}
	// FOLDED IN ONCE EVERY READ HAS ANSWERED: placed is one map, which no
	// read may write while another does.
	for i, q := range reads {
		if err := PlaceAnchors(placed, q, answers[i]); err != nil {
			return nil, err
		}
	}
	return placed, nil
}

// ChangeRevisions is the revisions a read of a change by object id names once
// its head revision is read: revision n — the head itself, with no read,
// where n is 0 or the head's own number — and, where since is not 0, revision
// since, each read through one AT ONCE ([AtOnce]): neither needs the other's
// row, so a node reading the estate through another pays one round trip for
// the two rather than one each. one reads a revision at the floor of the
// head's read, refusing one the change does not hold in its caller's words;
// its first error in the order n, since is answered as it is.
func ChangeRevisions(head RevisionRow, n, since int64, one func(n int64) (RevisionRow, error)) (RevisionRow,
	*RevisionRow, error) {

	rev := head
	var earlier *RevisionRow
	var reads []func() error
	if n != 0 && n != head.N {
		reads = append(reads, func() (err error) {
			rev, err = one(n)
			return err
		})
	}
	if since != 0 {
		earlier = &RevisionRow{}
		reads = append(reads, func() (err error) {
			*earlier, err = one(since)
			return err
		})
	}
	if err := AtOnce(reads...); err != nil {
		return RevisionRow{}, nil, err
	}
	return rev, earlier, nil
}

// AtOnce runs each of reads in a goroutine of its own — on the caller's for a
// single read — waits for every one, and answers the first error in their
// ORDER, not the first to arrive, so the refusal a caller is told is the same
// however the reads interleave.
//
// IT CANCELS NONE: each read is bounded by its own answer's deadline, and a
// read cancelled because another failed would answer a cancellation — which a
// surface tells as a condition of the node, and a seat's turn records —
// beside the refusal that was the real one.
func AtOnce(reads ...func() error) error {
	if len(reads) == 1 {
		return reads[0]()
	}
	errs := make([]error, len(reads))
	var wg sync.WaitGroup
	for i, read := range reads {
		wg.Go(func() { errs[i] = read() })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// ErrMisplaced is a re-anchoring's answer that placed another set than the
// anchors asked ([PlaceAnchors]): no Gitlet node of this build gives one, so the
// caller treats it as its own fault rather than a finding with no place.
var ErrMisplaced = errors.New("code: a re-anchoring's answer is not the anchors asked")

// PlaceAnchors writes into placed where a re-anchoring's answer put each
// anchor asked placed, refusing an answer that placed any other set than the
// anchors asked ([ErrMisplaced]).
func PlaceAnchors(placed map[string]FindingPlace, asked *ReanchorQuery, answer *ReanchorAnswer) error {
	got := 0
	if answer != nil {
		got = len(answer.Anchors)
	}
	if got != len(asked.Anchors) {
		return fmt.Errorf("%w: it placed %d of the %d anchors asked", ErrMisplaced, got, len(asked.Anchors))
	}
	want := make(map[string]bool, len(asked.Anchors))
	for _, a := range asked.Anchors {
		want[a.ID] = true
	}
	for _, a := range answer.Anchors {
		if !want[a.ID] {
			return fmt.Errorf("%w: it placed anchor %q, which was not asked", ErrMisplaced, a.ID)
		}
		delete(want, a.ID)
		placed[a.ID] = FindingPlace{Start: a.Start, End: a.End, Outdated: a.Outdated, Gone: a.Gone}
	}
	return nil
}
