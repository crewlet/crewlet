package coord

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A BACKUP POINT is what one owner's newest backup covers, and when it was
// taken — or, for the operator, what its newest acknowledgement of one log
// asserts.
//
// # Why the trim cannot work this out for itself
//
// [TermBackupFloor] refuses to let the log delete anything the newest backup
// does not hold, because the log is the only copy of what no node has applied
// yet. That term needs two facts — how far a backup reaches, and how old it is
// — and NEITHER is visible to the node evaluating it. A backup is written by
// `crewlet backup` into a directory the operator named on their own schedule,
// on whichever node they pointed it at; the node holding the trim duty may not
// be that node, may not have that directory, and cannot see a copy that has
// already left the host.
//
// So the taker publishes what it took and the trim reads it. Without this
// record the term has no input at all, and a term nobody could read BLOCKS —
// which is a fleet whose log never trims, correct and useless.
//
// # One key per owner, and the reader takes the NEWEST that covers each log
//
// A single shared key would be last-writer-wins across nodes, and the last
// writer is not the one with the newest backup: two nodes backing up on their
// own schedules would overwrite each other, so a fresh backup at sequence 900
// could be replaced by a stale one at 200 and the trim would go backwards for
// a reason nothing recorded. One key per owner makes the register a SET, and
// the reader picks by [BackupPoint.At] — which is a comparison it can make.
//
// It picks PER LOG ([NewestBackupOf]): the newest point that covers the log
// being asked about, rather than the newest point and then whether it covers
// it. The trim's backup term, the backup-age alarm and the backups screen all
// ask about one log at a time, and a point that does not reach a log says
// nothing about that log's copy — so the newest point overall, read as the
// answer for every log, blocks every log it happens not to cover however fresh
// an older point that does cover it is.
//
// # Why the operator is an owner like any other, filed per log
//
// Under `backup_floor: operator` the trim follows an explicit acknowledgement
// rather than the engine's own copy, because a backup is not a backup until it
// leaves the host and the engine cannot see that. That acknowledgement is a
// row here under [OperatorBackupOwner], written by `crewlet retention ack`,
// with the same shape and the same reader — so the two policies differ in
// WHICH rows are counted and in nothing else.
//
// AN ACKNOWLEDGEMENT NAMES ONE LOG, because a sequence belongs to the number
// space of the stream it came from, and so it is keyed by that log as well as
// by its owner ([BackupPointKey]). Keyed by the owner alone, as a node's copy
// is, each acknowledgement REPLACED the last: an operator who acknowledged the
// tracker's log, then the knowledge base's, was left with the knowledge base's
// alone, the tracker's blocked again, and the alarm — aging that one row —
// green. No sequence of acknowledgements ever covered more than one log.
// Merging them into one row instead would need a compare-and-set to keep two
// acknowledgements from losing each other, and an instant per log to age
// each, which is exactly what a row per log already is.
//
// # It has no TTL, for the register's own reason
//
// It lives in the positions bucket, which has no age at all. A backup point
// that expired would read as a fleet that has never backed up, which blocks
// the trim — so a bucket setting would silently become a retention policy, and
// the age this term actually cares about is already ON the row, compared
// against the operator's own `backup_max_age`.

// OperatorBackupOwner is the owner an operator acknowledgement is written
// under. It is not a node id, and cannot collide with one only because no node
// may take it: it has a node id's shape, and config.ValidNodeID refuses it
// wherever a node takes its id. A node running under it would announce its own
// copies here as the operator's, and nothing reading a row could tell the two
// apart.
const OperatorBackupOwner = "operator"

// BackupPoint is one node's newest backup, or the operator's newest
// acknowledgement of one log, as the register holds it.
type BackupPoint struct {
	// Owner is whose backup this is: a node id, or [OperatorBackupOwner].
	// It is the key — with the one stream it reaches, for an operator
	// acknowledgement ([BackupPointKey]) — so a second backup by the same
	// node replaces the first, and a second acknowledgement of the same
	// log replaces that log's first, which is correct: only the newest
	// one counts.
	Owner string `json:"owner"`

	// At is when the backup was TAKEN, not when this row was written. The
	// two differ for an operator acknowledgement, which is given after the
	// copy reached wherever it was going, and the term's age is measured
	// against the copy rather than against the paperwork.
	At time.Time `json:"at"`

	// Streams is how far the backup reaches per STREAM — a triple each,
	// because a bare sequence from before a reanchor names a dead number
	// space and would compare as if it were current.
	//
	// KEYED BY STREAM, for [TrimHold.Streams]' reason and taken from the
	// same place: the artefact's own manifest records where each applier
	// stood by stream, because `statelog_cursor` is keyed on the stream.
	Streams map[string]Position `json:"streams"`

	// Dir is where the artefact was written, for the operator reading a
	// blocked trim: "the newest backup is three days old" is an answer
	// somebody then has to go and find the directory for.
	Dir string `json:"dir,omitempty"`

	// Verified reports whether the taker checked the copy it wrote. An
	// UNVERIFIED point is a file that exists and may not restore, and the
	// trim deliberately does not count one — deleting the log's only copy
	// of a record against a backup nobody opened is the failure this whole
	// term exists to prevent.
	Verified bool `json:"verified"`

	// Bytes is how large the artefact is — every database copy and every
	// stream snapshot in it — as the taker measured what it wrote. Zero on
	// an operator acknowledgement, which asserts a copy the engine never
	// saw: absent rather than empty, which a reader says rather than
	// printing a size of nothing.
	Bytes int64 `json:"bytes,omitempty"`
}

// BackupRegister is the fleet's record of what has been backed up.
//
// A FOURTH key class in the positions bucket, beside the node rows, the holds
// and the published floors, on the same rule all three share: every one of
// them answers what the log may delete, and every one needs the same
// retention, which is none.
type BackupRegister interface {
	// PutBackupPoint writes or replaces the row at the point's key
	// ([BackupPointKey]): one node's newest backup, or the operator's
	// newest acknowledgement of one log — leaving its acknowledgements of
	// every other log standing.
	PutBackupPoint(ctx context.Context, p BackupPoint) error

	// BackupPoints reads every row: one per node that has backed up, and
	// one per log the operator has acknowledged.
	//
	// The whole set, because the reader takes a maximum over it by
	// instant: a per-owner read would need to know the owners, and the
	// question is exactly "who has backed up".
	BackupPoints(ctx context.Context) ([]BackupPoint, error)

	// ForgetBackupPoint removes every row an owner holds, for an owner
	// that has gone — the operator's acknowledgement of every log, under
	// [OperatorBackupOwner].
	ForgetBackupPoint(ctx context.Context, owner string) error
}

// BackupPointKey is a point's key in the register: its owner's, and for an
// operator acknowledgement its owner's and the one log it acknowledges.
//
// A NODE'S POINT IS ONE ARTEFACT, which covers every log the node ran at one
// instant, so the node is the identity and its next backup replaces the row.
// The operator's is not an artefact at all but an assertion about ONE log,
// made whenever that log's copy left the host — so the log is part of its
// identity, and acknowledging another log writes another row rather than
// withdrawing this one. [BackupPoint.Validate] is what holds an
// acknowledgement to the one log its key names.
func BackupPointKey(p BackupPoint) string {
	if p.Owner == OperatorBackupOwner {
		for stream := range p.Streams {
			return DocumentKey("backup", p.Owner, stream)
		}
	}
	return DocumentKey("backup", p.Owner)
}

// Validate reports why a backup point cannot be written.
func (p BackupPoint) Validate() error {
	if strings.TrimSpace(p.Owner) == "" {
		return fmt.Errorf("coord: a backup point has no owner — it is the key, " +
			"so a point without one would be written over another owner's and " +
			"read as theirs")
	}
	if p.At.IsZero() {
		return fmt.Errorf("coord: the backup point held by %s carries no instant: "+
			"the trim's backup term compares its age against `backup_max_age`, "+
			"and a zero instant is read as a backup taken in 1970 — which "+
			"blocks the trim for ever rather than failing loudly", p.Owner)
	}
	if len(p.Streams) == 0 {
		return fmt.Errorf("coord: the backup point held by %s reaches no stream: "+
			"a backup that covers nothing is indistinguishable from one whose "+
			"taker forgot to say what it covered, and the trim would read the "+
			"second as a floor of zero", p.Owner)
	}
	for name, at := range p.Streams {
		if at.Stream == "" {
			return fmt.Errorf("coord: the backup point held by %s reaches %q at "+
				"a position with no stream — a bare sequence from before a "+
				"reanchor names a dead number space", p.Owner, name)
		}
	}
	// AN ACKNOWLEDGEMENT IS FILED UNDER THE ONE LOG IT NAMES
	// ([BackupPointKey]), so one reaching two would be filed under either
	// and replace that log's own acknowledgement with a claim about both.
	if p.Owner == OperatorBackupOwner && len(p.Streams) != 1 {
		return fmt.Errorf("coord: an operator acknowledgement reaches %d streams: "+
			"it is filed under the one log it acknowledges, so it must name "+
			"exactly one — acknowledge each log on its own", len(p.Streams))
	}
	return nil
}

// CountedBackups is the points a backup-floor policy takes the word of:
// every owner's under `engine`, and the operator's acknowledgement alone under
// `operator` (pass operatorOnly), which is the one fact the engine cannot see
// for itself — that a copy has left the host.
//
// ONE RULE, because three readers apply it and they must agree: the trim's
// backup term, the backup-age alarm and the `backups` answer's `counted`
// flag. The alarm used to age the newest point of ANY owner, so under
// `operator` a fleet whose nodes backed up nightly and whose operator never
// acknowledged one kept a green alarm beside a trim that could not move.
func CountedBackups(points []BackupPoint, operatorOnly bool) []BackupPoint {
	if !operatorOnly {
		return points
	}
	var out []BackupPoint
	for _, p := range points {
		if p.Owner == OperatorBackupOwner {
			out = append(out, p)
		}
	}
	return out
}

// NewestBackupOf is the freshest point in a set that covers one stream, and
// whether there is one.
//
// THE READER'S HALF of the one-key-per-owner decision, written once here
// rather than at each caller: the trim takes it, the backup alarm ages it and
// the `backups` answer marks it, and three copies of "pick the newest" is
// three places for one of them to pick the last instead.
//
// PER STREAM, because every one of those questions is about one log: the
// trim's backup term bounds one log's purge, and a point that does not reach
// that log is no backup of it at all. Taking the newest point first and then
// asking whether it covers the log answers "no backup" for every log a newer
// point happens to leave out — which, with the operator's acknowledgements
// filed one log apiece, is every log but the last one acknowledged.
//
// An UNVERIFIED point is skipped, on [BackupPoint.Verified]'s own reason.
func NewestBackupOf(points []BackupPoint, stream string) (BackupPoint, bool) {
	var best BackupPoint
	var found bool
	for _, p := range points {
		if !p.Verified {
			continue
		}
		if _, covers := p.Streams[stream]; !covers {
			continue
		}
		if !found || p.At.After(best.At) {
			best, found = p, true
		}
	}
	return best, found
}
