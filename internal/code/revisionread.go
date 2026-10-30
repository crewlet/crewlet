package code

import (
	"fmt"
	"slices"

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
// PURE OVER THE ROWS: nothing here reads, so each surface makes its own reads
// — through the estate router, as its caller — and says, in its own words,
// what it could not read.
//
// # Files from the recorded base
//
// A revision's files are what its head changes against the BASE its home
// recorded in the revision's facts (code_revision_facts) — the merge base of
// its head and its target's tip as the home read it — never the target's tip
// now, which moves under the change and would show another change's work as
// this one's. What changed BETWEEN two revisions is their INTERDIFF, the two
// heads' diff kept to the paths either revision touches (git node's
// `interdiff`), since a rebase onto a moved target changes every file the
// target's own commits touched and the change none of them.
//
// # Where a finding is now
//
// A finding names lines of the revision it was raised on. In another revision
// they are where the lines changed above them moved them — OUTDATED where its
// own lines changed, GONE where the revision holds no file at its path — which
// ONE re-anchoring read of the revision asked about places for a page of
// findings ([PlaceFindings]).

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
// an empty id that every git node would refuse alike: a caller judges the gap
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
// its base was read from; or none, which reads the git node's copy as it
// stands. So the copy that answers holds every object the revisions' rows
// name.
//
// NEVER A ROW READ'S CHECKPOINT, which a linearizable read takes past its own
// barrier: a copy current to every push still sits below it, and a floor
// there would make the git node sync for nothing.
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
// the ONE re-anchoring that places every other, nil where none is left, which
// a caller asks of the repository's home git node at the floor of rev and of
// the change's head revision ([ObjectFloor]) and folds in with
// [PlaceAnchors]. A finding on the old side of a revision whose facts are not
// recorded yet has no base to place it in: it is in neither, and stays
// unplaced.
//
// A caller placing a page of at most [MaxReanchors] findings asks one read,
// since each finding is at most one anchor.
func PlaceFindings(rev RevisionRow, findings []FindingRow) (map[string]FindingPlace, *ReanchorQuery) {
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
	if len(anchors) == 0 {
		return placed, nil
	}
	return placed, &ReanchorQuery{Base: base, Head: rev.Head, Anchors: anchors}
}

// PlaceAnchors writes into placed where a re-anchoring's answer put each
// anchor asked placed, refusing an answer that placed any other set than the
// anchors asked — which no git node of this build gives, so the caller treats
// it as its own fault rather than a finding with no place.
func PlaceAnchors(placed map[string]FindingPlace, asked *ReanchorQuery, answer *ReanchorAnswer) error {
	got := 0
	if answer != nil {
		got = len(answer.Anchors)
	}
	if got != len(asked.Anchors) {
		return fmt.Errorf("code: a re-anchoring placed %d of the %d anchors asked", got, len(asked.Anchors))
	}
	want := make(map[string]bool, len(asked.Anchors))
	for _, a := range asked.Anchors {
		want[a.ID] = true
	}
	for _, a := range answer.Anchors {
		if !want[a.ID] {
			return fmt.Errorf("code: a re-anchoring placed anchor %q, which was not asked", a.ID)
		}
		delete(want, a.ID)
		placed[a.ID] = FindingPlace{Start: a.Start, End: a.End, Outdated: a.Outdated, Gone: a.Gone}
	}
	return nil
}
