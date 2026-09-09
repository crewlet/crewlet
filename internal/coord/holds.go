package coord

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A TRIM HOLD is a live pin on the tail of every domain's log.
//
// # What it is for, and why nothing else can do its job
//
// The trim deletes a record once every counted node has committed past it and
// no other term blocks. That is the right rule for the fleet's own work and
// the wrong one for anything reading the log from OUTSIDE it: a backup copies
// the store, then the streams, and between those two the trim can delete
// records the store copy has not applied — so the artefact is a store at
// position P beside a log whose oldest surviving record is above P + 1, and
// nothing can ever replay the difference. The gap is silent, permanent, and
// only visible during a restore.
//
// A hold is what closes it. It is one of [TermMinHold]'s inputs and it blocks
// the trim at the position its owner names, for as long as its owner keeps
// saying so.
//
// # It is HEARTBEATED, and that is the whole safety property
//
// A pin that outlives its owner is worse than no pin: the trim stops, the log
// grows to its ceiling, and appends are refused — an outage caused by a
// backup that crashed weeks ago. So a hold carries the instant it was last
// renewed, the arithmetic ignores one older than its stale bound, and the
// normal path releases it. A crashed holder therefore costs a bounded window
// of retention and nothing else.
//
// # One key per owner
//
// The owner is a node and a purpose — `node-a/backup` — so a node taking two
// backups at once replaces its own hold rather than accumulating pins, and the
// debris a crash can leave is bounded at one key per owner rather than one per
// attempt.

// TrimHold is one live pin, as the register holds it.
type TrimHold struct {
	// Owner is who holds it: a node id and a purpose, joined. It is the
	// key, so a second hold by the same owner replaces the first.
	Owner string `json:"owner"`

	// At is when it was last renewed. The arithmetic ignores a hold older
	// than its stale bound, which is what stops a crashed holder pinning
	// the log for ever.
	At time.Time `json:"at"`

	// Streams is the position held per STREAM — a triple each, because a
	// bare sequence from before a reanchor names a dead number space and
	// would compare as if it were current.
	//
	// KEYED BY STREAM RATHER THAN BY DOMAIN, unlike [NodePositions], and
	// the difference is deliberate rather than an accident of whoever
	// wrote each one. A hold is taken by something copying a LOG — a
	// backup reads `statelog_cursor`, which is keyed on the stream — and
	// a position's whole number space belongs to the stream it came from.
	// The key repeats [Position.Stream] on purpose: a map whose key and
	// whose value's own name could disagree is one where a reader has to
	// decide which is authoritative, and here they never differ.
	Streams map[string]Position `json:"streams"`

	// Reason is what the hold is for, in the holder's own words. It is
	// what an operator reads when the trim is not advancing, and the one
	// thing the register cannot derive.
	Reason string `json:"reason,omitempty"`
}

// HoldRegister is the fleet's record of what is pinning the log.
//
// IT LIVES IN THE POSITIONS BUCKET, under its own key class, and that is a
// retention decision rather than a convenience: the positions register is the
// one bucket in this estate with NO age at all, and a hold must not expire on
// a timer either — an expiring hold would release the pin while its owner was
// still reading, which is the exact hole the hold exists to close. The bound
// on a crashed holder is the ARITHMETIC's stale window, which is a number the
// trim owns and can report, rather than a bucket setting nobody sees.
type HoldRegister interface {
	// PutHold writes or renews a hold, replacing the owner's own.
	//
	// LAST WRITER WINS, on the register's own rule: the only writer of an
	// owner's key is that owner.
	PutHold(ctx context.Context, h TrimHold) error

	// Holds reads every live pin.
	//
	// The whole set, because the only caller takes a minimum across it.
	Holds(ctx context.Context) ([]TrimHold, error)

	// ReleaseHold removes one. The NORMAL path — a hold is released when
	// the work that took it finishes, and the stale bound is only for the
	// holder that did not get to.
	ReleaseHold(ctx context.Context, owner string) error
}

// HoldOwner builds an owner from a node and a purpose.
//
// TWO PARTS, because "which node" and "what was it doing" are different
// questions and the operator reading a stuck trim needs both: a bare node id
// says a machine is pinning the log and not why, and a bare purpose says a
// backup is running somewhere.
func HoldOwner(nodeID, purpose string) string {
	return strings.TrimSpace(nodeID) + "/" + strings.TrimSpace(purpose)
}

// HoldKey is an owner's key in the register.
func HoldKey(owner string) string { return DocumentKey("hold", owner) }

// Validate reports why a hold cannot be written.
func (h TrimHold) Validate() error {
	if strings.TrimSpace(h.Owner) == "" {
		return fmt.Errorf("coord: a trim hold has no owner — it is the key, " +
			"so a hold without one would be written over somebody else's pin " +
			"and released by their work finishing")
	}
	if len(h.Streams) == 0 {
		return fmt.Errorf("coord: the trim hold held by %s names no stream — a "+
			"hold that pins nothing is indistinguishable from one whose owner "+
			"forgot the log it was reading", h.Owner)
	}
	for name, at := range h.Streams {
		if at.Stream == "" {
			return fmt.Errorf("coord: the trim hold held by %s pins %q at a "+
				"position with no stream — a bare sequence from before a "+
				"reanchor names a dead number space", h.Owner, name)
		}
	}
	return nil
}
