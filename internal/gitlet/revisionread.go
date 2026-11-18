package gitlet

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

// A change's revisions read by object id (doc.go's "A change's revisions are
// read by the ids their rows record"): which commits a revision's files and
// patches are read between, where its findings are, and the floor such a read
// is asked at — decided here once for every surface showing a change's code
// (code_get_change, the API's code_change), so a seat and a person never see
// two diffs of one revision or one finding on two lines. Nothing here reads:
// each surface reads through the estate router as its caller and words its own
// gaps. Reads needing nothing of each other go [AtOnce], since on a node
// without the data role each read made after another is one more round trip.

// A revision's files are its head against the base its facts record
// (code_revision_facts: the merge base with its target's tip as the home read
// it), never the target's tip now, which moves under the change and would show
// another change's work as this one's. An interdiff keeps to the paths either
// revision touches, since a rebase onto a moved target changes every file the
// target's own commits touched, none of them the change's. Findings are placed
// by [PlaceFindings].

// FilesGap is why a revision's files cannot be read from the facts its rows
// hold: a closed set with [FilesGap.Valid], "" for a revision whose files can.
type FilesGap string

const (
	// FilesPending is a LIVE change's revision whose facts its home has not
	// recorded yet, which it does within a minute of the push
	// (home.Backstop): the gap clears by waiting.
	FilesPending FilesGap = "pending"
	// FilesUnrecorded is a revision of a change that ended before its home
	// recorded its facts, which it never will: an ended change is not measured.
	FilesUnrecorded FilesGap = "unrecorded"
	// FilesUnrelated is a revision whose head shares no history with its
	// target, so its facts record no base: its change is every file of its
	// head, which a read of its tree shows and no diff does.
	FilesUnrelated FilesGap = "unrelated"
)

// FilesGaps is the closed set, in the order a revision meets them.
func FilesGaps() []FilesGap { return []FilesGap{FilesPending, FilesUnrecorded, FilesUnrelated} }

// Valid reports a gap this build names.
func (g FilesGap) Valid() bool { return slices.Contains(FilesGaps(), g) }

// FilesGap is why r, a revision of a change in state, cannot have its files
// read from its facts, and "" where it can. based is a read FROM r's base (its
// files or a patch); an interdiff reads neither base, so it needs only that
// each revision's facts are recorded.
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
// repoID, from the base its facts record to its head with renames, or, where
// since names an EARLIER revision, the two's interdiff kept to the paths
// either touches; with file (or fileB64, the same path as exact bytes) one
// file's patch. The caller sets the asker, floor ([ObjectFloor]) and cursor.
// A revision lacking the facts read ([RevisionRow.FilesGap]) is refused
// [ErrInvalid], naming it, rather than sent with an empty id every Gitlet node
// would refuse alike: a caller judges the gap first, in its own words.
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

// ObjectFloor is the floor a read by object id of revisions revs is asked at,
// so the answering copy holds every object their rows name: the furthest of
// own (the asking node's own writes, estate.CodeSeam.FloorOf), the record that
// moved each revision's head and, where its facts are recorded, the record
// that moved its target to the tip its base was read from; or none, reading
// the copy as it stands. NEVER a row read's checkpoint: a linearizable read's
// sits past its own barrier, above a copy current to every push, so the Gitlet
// node would sync for nothing.
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

// RefsFloor is [ObjectFloor]'s rule for commits named by refs rather than a
// change's rows: the furthest of own (the asking node's own writes and any
// floor of its caller's) and the record that last moved each ref
// ([ResolvedRef.Moved]), by which its commit had landed; or none. NEVER the
// resolve's checkpoint ([RefAnswer.Position]), for ObjectFloor's reason.
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

// PlaceFindings places findings in revision rev, by id, as far as the rows say
// without a read — one raised on rev where it was raised; one on the OLD side
// of a revision with no recorded base gone, its old side the empty tree; one on
// the old side of a revision with no facts yet unplaced, having no base — and
// returns the re-anchorings that place the rest, nil where none is left, which
// [Place] asks and folds in. Each takes at most [MaxReanchors] anchors, one
// Gitlet node read's limit ([ReanchorQuery]); a larger page (up to
// [MaxFindingsPerPage]) is cut after sorting by path, side and blob, since a
// read runs one diff per distinct blob pair, so each pair spans as few reads
// as the page allows.
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

// Place is where each of findings is in revision rev, by id: [PlaceFindings]'s
// rows plus every re-anchoring it names, asked through ask [AtOnce] (none needs
// another's answer) and folded in with [PlaceAnchors]. ask is the caller's
// read by object id at the floor of rev and the change's head revision
// ([ObjectFloor]); its first error in the reads' order is answered as it is,
// and an answer placing another set than its anchors is [ErrMisplaced].
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
	// Folded in once every read has answered: no read may write placed while
	// another does.
	for i, q := range reads {
		if err := PlaceAnchors(placed, q, answers[i]); err != nil {
			return nil, err
		}
	}
	return placed, nil
}

// ChangeRevisions is the revisions a read of a change by object id names once
// its head revision is read: revision n (the head itself, unread, where n is 0
// or the head's number) and, where since is not 0, revision since, read
// through one [AtOnce], so a node reading the estate through another pays one
// round trip for both. one reads at the head read's floor and refuses a
// revision the change lacks in its caller's words; its first error in the
// order n, since is answered as it is.
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

// AtOnce runs each of reads in its own goroutine (the caller's for a single
// read), waits for all, and answers the first error in their ORDER, not of
// arrival, so the refusal told is the same however they interleave. It cancels
// none: each is bounded by its own deadline, and a read cancelled because
// another failed would report a cancellation (told as a node condition,
// recorded in a seat's turn) beside the real refusal.
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
// anchors asked ([PlaceAnchors]): no Gitlet node of this build gives one, so
// the caller treats it as its own fault, not a finding with no place.
var ErrMisplaced = errors.New("gitlet: a re-anchoring's answer is not the anchors asked")

// PlaceAnchors writes into placed where a re-anchoring's answer put each
// anchor asked, refusing an answer that placed any other set ([ErrMisplaced]).
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
