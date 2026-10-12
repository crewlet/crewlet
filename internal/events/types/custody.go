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
// THE EVENTS WHOLE, envelope and all, rather than rendered rows: the data node
// renders each one with the function its own publish listener uses, so a
// stateless node's row is the row it would have written itself. An envelope
// round-trips through the codec losslessly whatever its type, so an event a
// newer stateless node published that the data node's build does not know
// arrives intact and is rendered — or left out — by the build that has to read
// it, exactly as one of its own would be.
//
// The batch's identity is the ENVELOPE's id. The publisher sends a batch it was
// not told was stored again under the same id, which the broker collapses
// within its duplicate window, and that id is what a data node claims custody
// of — never the id of an event inside it, which is the identity of that
// event's row.
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
