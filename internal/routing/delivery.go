package routing

import (
	"slices"
	"time"
)

// Delivery is what a reader can say about who one record reached.
//
// A NAMED TYPE rather than two booleans, for the reason every closed set in
// the engine is one: an unknown value off the wire is a value rather than a
// panic, and a surface switching on one cannot render a combination that does
// not exist.
type Delivery string

const (
	// DeliveryQuiet is a record that carried no notification at all — or
	// no record by that id. It is not a failure: most field edits announce
	// nothing, and neither claimed to reach anybody, so neither has
	// anything missing.
	DeliveryQuiet Delivery = "quiet"

	// DeliveryReached is the ordinary answer: the recipient rows are here
	// and the reader names them.
	DeliveryReached Delivery = "reached"

	// DeliveryNobody is announced, inside the retained window, and no rows:
	// the routing genuinely resolved to nobody. Every candidate was the
	// actor themself, or has left the company.
	DeliveryNobody Delivery = "nobody"

	// DeliverySwept is announced, no rows, and older than the retention
	// horizon — so their absence is not evidence. The sweep is entitled to
	// have taken them and the applier is entitled never to have written
	// them, and neither leaves a trace saying which.
	DeliverySwept Delivery = "swept"

	// DeliveryUnknown is announced, no rows, and no horizon stated, so
	// there is nothing to date the absence against. Honest rather than
	// useful, which is the right way round: the alternative is telling an
	// operator that a change reached nobody on no evidence at all.
	DeliveryUnknown Delivery = "unknown"
)

// Deliveries is the closed set, in the order a reader meets them.
var Deliveries = []Delivery{
	DeliveryQuiet, DeliveryReached, DeliveryNobody, DeliverySwept,
	DeliveryUnknown,
}

// Valid reports whether d is one this build knows.
func (d Delivery) Valid() bool { return slices.Contains(Deliveries, d) }

// Evidence is what a reader holds about one record's recipients, which is
// everything [DeliveryOf] decides on.
type Evidence struct {
	// Held is whether a record with this id exists here at all. A record
	// id that names nothing — a link from an old page, a record not yet
	// replayed this far — is an ordinary thing for a caller to hold.
	Held bool

	// Announced is whether the record carried a notification: what its
	// applier concluded when it routed, and deliberately NOT whether
	// anybody was woken — the applier holds no roster, so it cannot know.
	Announced bool

	// Recipients is how many recipient rows the reader found for it.
	Recipients int

	// At is the record's AUTHORED instant.
	At time.Time

	// RetainedFrom is the instant the caller's stated retention horizon
	// falls on — the sweep's own cutoff. Zero when the caller stated none.
	//
	// THE HORIZON IS THE CALLER'S, read when it asks, for the reason a read
	// takes `now` from its caller: it is a company setting, the epoch moves
	// under a reader that outlives it, and a reader holding a stale horizon
	// would date a set against a retention nobody is running.
	RetainedFrom time.Time
}

// DeliveryOf is the one place the three empty states are told apart.
//
// An announced record with no rows is ambiguous between three things — a
// routing that resolved to nobody, rows a retention sweep took, and not
// knowing which — and THE HORIZON separates them: a record older than it is
// one whose rows the sweep was entitled to take and the applier entitled
// never to write, so their absence is not evidence; a record newer than it
// would still have its rows, so their absence IS evidence that nobody was
// reached; and with no horizon stated there is nothing to date it by.
//
// A FUNCTION OVER VALUES rather than a branch inside a domain's read, so the
// rule is exercisable without a database and every domain that keeps
// recipient rows answers it alike.
func DeliveryOf(e Evidence) Delivery {
	switch {
	case e.Recipients > 0:
		return DeliveryReached
	case !e.Held || !e.Announced:
		return DeliveryQuiet
	case e.RetainedFrom.IsZero():
		return DeliveryUnknown
	case e.At.Before(e.RetainedFrom):
		return DeliverySwept
	default:
		return DeliveryNobody
	}
}
