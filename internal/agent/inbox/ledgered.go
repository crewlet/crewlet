package inbox

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
)

// ledgeredTypes names the inbox trigger types the completion ledger covers.
//
// The types that RUN A TURN, the only work whose duplication costs anything
// outward-facing (a second Slack reply, issue comment or commit). Informational
// types are logged and dropped.
//
// Deliberately CLOSED rather than "everything not informational". A type absent
// from it is not refused; it contributes nothing to the work key and is never
// recorded, so its redeliveries re-run: safe for a cheap turn, wrong for a
// costly one, so a new trigger type must be added here, not merely published.
var ledgeredTypes = map[string]bool{
	types.TaskAssigned{}.EventType():         true,
	types.WorkPickupType:                     true,
	types.DelegatedReviewType:                true,
	types.ExternalNotification{}.EventType(): true,
	types.A2ARequestType:                     true,
	types.A2AMessageType:                     true,
}

// Ledgered reports whether the completion ledger records this event type.
//
// Both A2A hops are covered, and exempting them is the mistake to avoid. It is
// tempting while the content rides a process-local queue that a turn drains
// DESTRUCTIVELY: a re-run on any node then finds an empty channel and tells the
// agent nobody sent anything, so neither branch of a
// short-circuit-or-re-run choice can be honoured and the ledger stays out
// of it. The content rides the durable wake event here, which makes an A2A
// trigger re-runnable and therefore ledgerable like any other.
func Ledgered(eventType string) bool { return ledgeredTypes[eventType] }

// LedgeredTypes lists the covered types, sorted.
//
// Exported so the set can be asserted ABOUT rather than restated: a guard that
// iterates its own copy of the names proves only that the copy is
// self-consistent, and would pass while the real set carried a name nothing
// publishes. Diagnostics read it for the same reason an operator asks which
// triggers are protected from redelivery.
func LedgeredTypes() []string { return slices.Sorted(maps.Keys(ledgeredTypes)) }
