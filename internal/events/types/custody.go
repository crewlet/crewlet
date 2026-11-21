package types

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/events"
)

// Custody: what a node without the `data` role publishes reaches a data node's
// event log through the broker (ADR-0025).

func init() {
	events.Register[CustodyBatch]()
}

// CustodyBatch is a batch of the events a node without `data` published, on
// their way into exactly one data node's event log.
//
// THE EVENTS WHOLE, envelope and all, not rendered rows: the data node renders
// each with the function its own publish listener uses, so the row is the one
// the stateless node would have written. An envelope round-trips the codec
// losslessly, so an event of a type this build does not know arrives intact and
// is rendered, or left out, by the build that reads it.
//
// The batch's identity is the ENVELOPE's id: an unconfirmed batch is resent
// under it, which the broker collapses within its duplicate window, and a data
// node claims custody of that id, never of an inner event's id (its row's).
type CustodyBatch struct {
	// Events is each event as published, in the order it was published.
	Events []*events.Event `json:"events"`
}

// EventType is the "custody_batch" wire type.
func (CustodyBatch) EventType() string { return "custody_batch" }

// Summary counts the batch, which is all that can be said of it whole.
func (b CustodyBatch) Summary() string {
	return fmt.Sprintf("Custody batch of %d events", len(b.Events))
}
