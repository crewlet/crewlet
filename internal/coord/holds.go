package coord

import (
	"context"
	"fmt"
	"strings"
)

// A TRIM HOLD is a live pin on the tail of every domain's log, one of
// [TermMinHold]'s inputs, for a reader OUTSIDE the fleet's own work: a backup
// copies the store and then the streams, and between the two the trim can
// delete records the store copy has not applied, a silent, permanent gap
// found only at restore.
//
// It is HEARTBEATED, and that is the whole safety property: a pin outliving
// its owner would stop the trim until the log hit its ceiling and refused
// appends. So the trim ignores a hold it has watched go unrenewed past its
// stale bound, and the normal path releases it. One key per owner, a node and
// a purpose (`node-a/backup`), so a second backup replaces its node's hold and
// a crash leaves at most one key per owner.

// TrimHold is one live pin, as the register holds it.
type TrimHold struct {
	// Owner is who holds it: a node id and a purpose, joined. It is the
	// key, so a second hold by the same owner replaces the first.
	Owner string `json:"owner"`

	// Streams is the position held per STREAM, a triple each: a bare
	// sequence from before a reanchor names a dead number space.
	//
	// KEYED BY STREAM, unlike [NodePositions], on purpose: a hold is taken
	// by something copying a LOG (a backup reads `statelog_cursor`, keyed
	// on the stream), and a position's number space is its stream's. The
	// key always equals [Position.Stream].
	Streams map[string]Position `json:"streams"`

	// Reason is what the hold is for, in the holder's own words. It is
	// what an operator reads when the trim is not advancing, and the one
	// thing the register cannot derive.
	Reason string `json:"reason,omitempty"`

	// Revision is the store's revision of the write this hold was read
	// from: filled by every [HoldRegister.Holds], never written. A renewal
	// is a new write with a new revision whichever member leads, which is
	// how the trim tells a renewed hold from one left standing. Zero on a
	// hold nothing read back.
	Revision uint64 `json:"-"`
}

// HoldRegister is the fleet's record of what is pinning the log.
//
// A key class in the positions bucket, which has NO age ([PositionRegister]):
// an expiring hold would release the pin while its owner still read, the
// hole it exists to close. A crashed holder is bounded instead by the trim's
// watch of each hold's writes against a stale window the trim owns and reports.
type HoldRegister interface {
	// PutHold writes or renews a hold, replacing the owner's own. Last
	// writer wins: the only writer of an owner's key is that owner.
	PutHold(ctx context.Context, h TrimHold) error

	// Holds reads every live pin, the whole set, because the only caller
	// takes a minimum across it: each as the store's leader holds it, with
	// its revision.
	Holds(ctx context.Context) ([]TrimHold, error)

	// ReleaseHold removes one: the NORMAL path, when the work that took it
	// finishes; the trim's stale bound is only for a holder that did not get to.
	ReleaseHold(ctx context.Context, owner string) error
}

// HoldOwner builds an owner from a node and a purpose: an operator reading a
// stuck trim needs both which machine pins the log and why.
func HoldOwner(nodeID, purpose string) string {
	return strings.TrimSpace(nodeID) + "/" + strings.TrimSpace(purpose)
}

// MaxHoldOwnerBytes bounds a hold's owner: a node id, a slash and the
// holder's purpose, a word of its own ([HoldOwner]).
const MaxHoldOwnerBytes = 128

// MaxHoldReasonBytes bounds a hold's reason: one sentence its holder states
// as a constant (a backup's, a join's).
const MaxHoldReasonBytes = 256

// HoldKey is an owner's key in the register.
func HoldKey(owner string) string { return DocumentKey(HoldClass, owner) }

// HoldClass is the key class a trim hold is filed under in the positions
// register.
const HoldClass = "hold"

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
	switch {
	case !plainWithin(h.Owner, MaxHoldOwnerBytes):
		return fmt.Errorf("coord: a trim hold's owner is past %d plain bytes: it is a node id "+
			"and a purpose (HoldOwner)", MaxHoldOwnerBytes)
	case len(h.Streams) > MaxLogDomains:
		return fmt.Errorf("coord: the trim hold held by %s pins %d logs, past the %d a hold holds",
			h.Owner, len(h.Streams), MaxLogDomains)
	case len(h.Reason) > MaxHoldReasonBytes:
		return fmt.Errorf("coord: the trim hold held by %s gives a reason past %d bytes",
			h.Owner, MaxHoldReasonBytes)
	}
	for name, at := range h.Streams {
		if at.Stream == "" {
			return fmt.Errorf("coord: the trim hold held by %s pins %q at a "+
				"position with no stream — a bare sequence from before a "+
				"reanchor names a dead number space", h.Owner, name)
		}
		if name != at.Stream || !ValidStreamName(name) {
			return fmt.Errorf("coord: the trim hold held by %s pins %q under %q: each position is "+
				"filed under its own stream's name, a log's", h.Owner, at.Stream, name)
		}
	}
	return nil
}
