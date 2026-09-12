package coord

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A BACKUP POINT is what one owner's newest backup covers, and when it was
// taken.
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
// # One key per owner, and the reader takes the NEWEST
//
// A single shared key would be last-writer-wins across nodes, and the last
// writer is not the one with the newest backup: two nodes backing up on their
// own schedules would overwrite each other, so a fresh backup at sequence 900
// could be replaced by a stale one at 200 and the trim would go backwards for
// a reason nothing recorded. One key per owner makes the register a SET, and
// the reader picks by [BackupPoint.At] — which is a comparison it can make.
//
// # Why the operator is an owner like any other
//
// Under `backup_floor: operator` the trim follows an explicit acknowledgement
// rather than the engine's own copy, because a backup is not a backup until it
// leaves the host and the engine cannot see that. That acknowledgement is a
// row here under [OperatorBackupOwner], written by `crewlet retention ack`,
// with the same shape and the same reader — so the two policies differ in
// WHICH rows are counted and in nothing else.
//
// # It has no TTL, for the register's own reason
//
// It lives in the positions bucket, which has no age at all. A backup point
// that expired would read as a fleet that has never backed up, which blocks
// the trim — so a bucket setting would silently become a retention policy, and
// the age this term actually cares about is already ON the row, compared
// against the operator's own `backup_max_age`.

// OperatorBackupOwner is the owner an operator acknowledgement is written
// under. It is not a node id, and cannot collide with one: a node id is a
// derived identifier and this is a reserved word.
const OperatorBackupOwner = "operator"

// BackupPoint is one owner's newest backup, as the register holds it.
type BackupPoint struct {
	// Owner is whose backup this is: a node id, or [OperatorBackupOwner].
	// It is the key, so a second backup by the same owner replaces the
	// first — which is correct, because only the newest one counts.
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
}

// BackupRegister is the fleet's record of what has been backed up.
//
// A FOURTH key class in the positions bucket, beside the node rows, the holds
// and the published floors, on the same rule all three share: every one of
// them answers what the log may delete, and every one needs the same
// retention, which is none.
type BackupRegister interface {
	// PutBackupPoint writes or replaces one owner's newest backup.
	PutBackupPoint(ctx context.Context, p BackupPoint) error

	// BackupPoints reads every owner's row.
	//
	// The whole set, because the reader takes a maximum over it by
	// instant: a per-owner read would need to know the owners, and the
	// question is exactly "who has backed up".
	BackupPoints(ctx context.Context) ([]BackupPoint, error)

	// ForgetBackupPoint removes one, for an owner that has gone.
	ForgetBackupPoint(ctx context.Context, owner string) error
}

// BackupPointKey is an owner's key in the register.
func BackupPointKey(owner string) string { return DocumentKey("backup", owner) }

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
	return nil
}

// NewestBackup is the freshest point in a set, and whether there is one.
//
// THE READER'S HALF of the one-key-per-owner decision, written once here
// rather than at each caller: the trim takes it, `crewlet retention status`
// prints it and the backup alarm ages it, and three copies of "pick the
// newest" is three places for one of them to pick the last instead.
//
// An UNVERIFIED point is skipped, on [BackupPoint.Verified]'s own reason.
func NewestBackup(points []BackupPoint) (BackupPoint, bool) {
	var best BackupPoint
	var found bool
	for _, p := range points {
		if !p.Verified {
			continue
		}
		if !found || p.At.After(best.At) {
			best, found = p, true
		}
	}
	return best, found
}
