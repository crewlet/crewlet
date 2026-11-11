package notify

import (
	"errors"
	"fmt"
)

// DeferReason is why a parser handed a delivery back rather than parse it:
// what tells a roll's ordinary traffic apart from a delivery no node can read.
type DeferReason string

// DeferNewerRecord is a relayed record whose version a newer build writes,
// which this build's domain refuses to decode (its `ErrFutureVersion`). A peer
// on the newer build reads it, so the delivery is handed back to the fleet for
// that peer, never acked here as a payload no parser can read (crewlet#993).
const DeferNewerRecord DeferReason = "newer_record"

// Valid reports whether r is one of the reasons this build names.
func (r DeferReason) Valid() bool { return r == DeferNewerRecord }

// Deferral is a delivery this node cannot parse and a peer can: what a
// [Parser] answers in place of an error it would otherwise return, for a
// record its domain refuses as newer than this build.
//
// NEVER ACKED. Every node takes inbound mail from ONE fleet-wide consumer
// group ([InboundGroup]), so a delivery this node acks is one no peer ever
// sees: the wake a newer build would have derived from the record is gone
// fleet-wide. It is the change feed's deferral (internal/changefeed,
// crewlet#842) on the other side of the relay, where the record a newer build
// wrote reaches a parser rather than a translator: the newer node's feed read
// the record and relayed its wake, and whichever node wins the delivery is the
// one that parses it.
//
// PASSED, NEVER DEFERRED: [Service.Handle] hands the delivery back to the group
// with [queue.Pass], which neither quiesces this node — nothing is wrong with
// it, only with this delivery here — nor reads as a failure, and which spaces
// the delivery's returns as a failure's are spaced, a second doubling to
// thirty, so the older nodes of a roll cannot spin it between them. The
// group's 25 deliveries offer it to the fleet for about ten minutes, and one no
// node has parsed by then is dead-lettered with its copy, where nothing
// replays it — unlike the change feed's hour, which a replay follows. A roll is
// minutes (ADR-0109), and the newer node whose feed relayed the record is
// already in the group.
type Deferral struct {
	Reason DeferReason
	// Err is what the parser met, for the log line.
	Err error
}

func (d *Deferral) Error() string {
	if d.Err == nil {
		return "notify: deferred to a peer (" + string(d.Reason) + ")"
	}
	return fmt.Sprintf("notify: deferred to a peer (%s): %v", d.Reason, d.Err)
}

func (d *Deferral) Unwrap() error { return d.Err }

// Defer is err as a deferral for reason: what a parser answers for a delivery
// this node cannot read and a peer can.
func Defer(reason DeferReason, err error) error {
	return &Deferral{Reason: reason, Err: err}
}

// deferralOf is the deferral err carries, if any.
func deferralOf(err error) (*Deferral, bool) {
	var d *Deferral
	if errors.As(err, &d) {
		return d, true
	}
	return nil, false
}
