package coord

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A BACKUP POINT is what one owner's newest backup covers and when it was
// taken, or what the operator's newest acknowledgement of one log asserts.
// [TermBackupFloor] keeps the log from deleting what the newest backup does not
// hold, and the node evaluating it can see neither fact: `crewlet backup` writes
// where the operator pointed it, maybe on another node, maybe already off the
// host. So the taker publishes the point and the trim reads it; without it the
// term has no input and BLOCKS, and the log never trims.
//
// Under `backup_floor: operator` the trim follows the operator's acknowledgement
// (the engine cannot see a copy leave the host): a row under
// [OperatorBackupOwner], written by `crewlet retention ack`, of the same shape
// and reader, so the policies differ only in WHICH rows count ([CountedBackups]).

// OperatorBackupOwner is the owner an operator acknowledgement is written
// under. It has a node id's shape and collides with none only because
// config.ValidNodeID refuses it as one: a node running under it would announce
// its copies as the operator's, indistinguishably.
const OperatorBackupOwner = "operator"

// BackupPoint is one node's newest backup, or the operator's newest
// acknowledgement of one log, as the register holds it.
type BackupPoint struct {
	// Owner is a node id or [OperatorBackupOwner], and the key with it
	// ([BackupPointKey]), so only the newest point under a key counts.
	Owner string `json:"owner"`

	// At is when the backup was TAKEN, not when this row was written: an
	// operator acknowledges after the copy reached its destination, and the
	// term ages the copy, not the paperwork.
	At time.Time `json:"at"`

	// Streams is how far the backup reaches, a [Position] per STREAM: a bare
	// sequence from before a reanchor names a dead number space. Keyed by
	// stream for [TrimHold.Streams]' reason: the artefact's manifest records
	// each applier from `statelog_cursor`, which is keyed on the stream.
	Streams map[string]Position `json:"streams"`

	// Dir is where the artefact was written, so an operator reading a
	// blocked trim can go and find it.
	Dir string `json:"dir,omitempty"`

	// Verified reports whether the taker checked the copy it wrote. The trim
	// does not count an UNVERIFIED point: deleting the log's only copy of a
	// record against a backup nobody opened is the failure this term exists
	// to prevent.
	Verified bool `json:"verified"`

	// Bytes is the artefact's size as the taker measured it, every database
	// copy and stream snapshot. Zero on an operator acknowledgement, which
	// asserts a copy the engine never saw: a reader says it is absent rather
	// than printing a size of nothing.
	Bytes int64 `json:"bytes,omitempty"`
}

// BackupRegister is the fleet's record of what has been backed up: a fourth key
// class in the positions bucket, beside the node rows, the holds and the
// published floors, since all four answer what the log may delete. Like them it
// has NO TTL: an expired point would read as a fleet that never backed up and
// block the trim, and the age that matters is on the row ([BackupPoint.At],
// against `backup_max_age`).
type BackupRegister interface {
	// PutBackupPoint writes or replaces the row at [BackupPointKey], leaving
	// the operator's acknowledgements of other logs standing.
	PutBackupPoint(ctx context.Context, p BackupPoint) error

	// BackupPoints reads every row: one per node that has backed up, one per
	// log the operator has acknowledged. The whole set, because the reader
	// takes a maximum over it and the question is exactly "who has backed up".
	BackupPoints(ctx context.Context) ([]BackupPoint, error)

	// ForgetBackupPoint removes every row an owner holds (for
	// [OperatorBackupOwner], every log's acknowledgement). The trim's duty
	// calls it for a node evicted from every log that counts nodes: a copy on
	// a host the operator let go is none to trim against, and nothing else
	// would remove its row.
	ForgetBackupPoint(ctx context.Context, owner string) error
}

// MaxBackupDirBytes bounds the directory a point names: refused where a
// backup is asked for (internal/backup's Take) rather than cut, since a
// person goes to find it. A kibibyte is past any path an operator types, and
// six of it as stored, at its widest, beside a point's logs is a small value.
const MaxBackupDirBytes = 1 << 10

// BackupPointKey is a point's key: its owner, and for an operator
// acknowledgement also the one log it acknowledges. One key per owner makes the
// register a SET read by [BackupPoint.At]: a shared key is last-writer-wins, and
// two nodes on their own schedules could let a stale backup overwrite a fresh
// one and move the trim backwards. A node's point is one artefact covering every
// log, so its next backup replaces it; an acknowledgement asserts ONE log, so
// acknowledging another writes another row rather than withdrawing this one (one
// merged row would need a compare-and-set and an instant per log, which is what
// a row per log is). [BackupPoint.Validate] holds it to that one log.
func BackupPointKey(p BackupPoint) string {
	if p.Owner == OperatorBackupOwner {
		for stream := range p.Streams {
			return DocumentKey(BackupPointClass, p.Owner, stream)
		}
	}
	return DocumentKey(BackupPointClass, p.Owner)
}

// BackupPointClass is the key class a backup point is filed under in the
// positions register, under its owner and, for an acknowledgement, its log.
const BackupPointClass = "backup"

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
	switch {
	case !plainWithin(p.Owner, MaxNodeIDBytes):
		return fmt.Errorf("coord: a backup point's owner is no node id: at most %d plain bytes",
			MaxNodeIDBytes)
	case len(p.Streams) > MaxLogDomains:
		return fmt.Errorf("coord: the backup point held by %s reaches %d logs, past the %d a point "+
			"holds", p.Owner, len(p.Streams), MaxLogDomains)
	case len(p.Dir) > MaxBackupDirBytes:
		return fmt.Errorf("coord: the backup point held by %s names a directory past %d bytes",
			p.Owner, MaxBackupDirBytes)
	}
	for name, at := range p.Streams {
		if at.Stream == "" {
			return fmt.Errorf("coord: the backup point held by %s reaches %q at "+
				"a position with no stream — a bare sequence from before a "+
				"reanchor names a dead number space", p.Owner, name)
		}
		if name != at.Stream || !ValidStreamName(name) {
			return fmt.Errorf("coord: the backup point held by %s reaches %q under %q: each "+
				"position is filed under its own stream's name, a log's", p.Owner, at.Stream, name)
		}
	}
	// Filed under the one log it names ([BackupPointKey]), an acknowledgement
	// reaching two would replace that log's own with a claim about both.
	if p.Owner == OperatorBackupOwner && len(p.Streams) != 1 {
		return fmt.Errorf("coord: an operator acknowledgement reaches %d streams: "+
			"it is filed under the one log it acknowledges, so it must name "+
			"exactly one — acknowledge each log on its own", len(p.Streams))
	}
	return nil
}

// CountedBackups is the points a backup-floor policy takes the word of: every
// owner's under `engine`, only the operator's acknowledgements under `operator`
// (pass operatorOnly), the one fact the engine cannot see: a copy left the host.
// ONE RULE for its three readers, the trim's backup term, the backup-age alarm
// and the `backups` answer's `counted` flag: an alarm aging any owner's point
// would stay green under `operator` beside a trim that cannot move.
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

// NewestBackupOf is the freshest point in a set that covers stream, and whether
// there is one: the reader's half of one key per owner, written once for the
// trim, the backup alarm and the `backups` answer. PER STREAM, because each asks
// about one log and a point not reaching it is no backup of it: the newest point
// overall would answer "no backup" for every log it leaves out, which, with
// acknowledgements filed per log, is all but the last one acknowledged. An
// UNVERIFIED point is skipped ([BackupPoint.Verified]).
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
