package inbox

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
)

// ledgeredTypes names the inbox trigger types the completion ledger covers.
//
// These are the types that RUN A TURN, which is the only work whose
// duplication costs anything outward-facing — a second Slack reply, a second
// comment on an issue, a second commit. The informational types are logged and
// dropped, so recording them would be bookkeeping about nothing.
//
// The set is deliberately CLOSED rather than "everything that is not
// informational". A type absent from it is not refused anywhere; it simply
// contributes nothing to the work key and is never recorded, so its
// redeliveries re-run. That is the safe direction for a type whose turn is
// cheap and the wrong one for a type whose turn is not, which is why adding a
// new trigger type means adding it here and not merely publishing it.
var ledgeredTypes = map[string]bool{
	types.TaskAssigned{}.EventType():         true,
	types.ExternalNotification{}.EventType(): true,
	types.A2ARequestType:                     true,
	types.A2AMessageType:                     true,
}

// Ledgered reports whether the completion ledger records this event type.
//
// Both A2A hops are covered. They were exempt in the Python this replaces
// while the content rode a process-local queue that a turn drained
// DESTRUCTIVELY: a re-run on any node found an empty channel and told the
// agent nobody had sent anything, so neither branch of a
// short-circuit-or-re-run choice could be honoured and the ledger stayed out
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
