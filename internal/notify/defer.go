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

// DeferNewerSource is a FIRST-PARTY delivery (types.RawWebhook.FirstParty)
// from a source this build neither parses nor declares: one a newer build's
// producer added — a state-log domain given a wake feed, say — which a peer on
// that build parses, so the delivery is handed back to the fleet for it, never
// acked here as a source nothing parses ([Service.Handle]).
const DeferNewerSource DeferReason = "newer_source"

// Valid reports whether r is one of the reasons this build names.
func (r DeferReason) Valid() bool { return r == DeferNewerRecord || r == DeferNewerSource }

// Deferral is a delivery this node cannot parse and a peer can: what a
// [Parser] answers instead of an error for a record newer than this build
// (also [Service.Handle]'s for a [DeferNewerSource]). NEVER ACKED: every node
// shares ONE consumer group ([InboundGroup]), so an ack means no peer sees it
// and the newer build's wake is gone fleet-wide. PASSED with [queue.Pass] (no quiesce, no failure; returns spaced as a
// failure's are, to thirty seconds) and REPLAYED, since nothing steers a pass
// to a reading node: all 25 deliveries (about ten minutes) can go to older
// ones. The dead letter ([queue.RetiredTo]) is replayed and dropped by a node
// that reads it (replay.go). STILL LOST: a copy whose keep fails at every
// delivery past its budget (jetstream's keepRetries), and one no reading node
// replays before retention ends (`inbound_dead_lettered` names it).
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
