package session

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SECOND THREE-VALUED TABLE, and it has the same shape as the first for
// the same reason: a seat is a row in a domain that lags INDEPENDENTLY.
//
// A person's identity is on the iam log and their seat is on the chart log.
// Nothing orders the two, so a node can be perfectly current on one and a
// minute behind on the other — which means a seat missing from this node's
// chart view is not one answer but three:
//
//   - the seat is GONE: this node's chart position covers the position the
//     binding was decided at, so it has seen everything that decision saw, and
//     the seat is not there. 403 NAMING THE SEAT — never a silent fall-through
//     to a seatless principal, which would reach every person-scoped read as
//     an empty answer and look like a person with nothing assigned to them.
//   - this node has NOT SEEN THE HIRE: its chart position is below the
//     binding's. 503, under the same stall grace the session table uses.
//   - this node CANNOT ANSWER: the chart applier has stalled past that grace,
//     or the view is not built. 503.
//
// AND THE SEATLESS ARM IS RESERVED FOR A PERSON WHO HOLDS NO BINDING. The
// chart is never consulted for them: their login is the handle, which is
// exactly why internal/iam makes a login and a seat handle disjoint by
// grammar — the two namespaces meet in one author column and neither may be
// mistaken for the other.

// Chart is what this node's pinned org-chart view says about one seat.
//
// DEFINED HERE, by the caller, and two methods wide. The chart domain exports
// a reader with a dozen; what resolving a principal needs is whether one seat
// is there and how far this node has got, and a seam that asked for more would
// be one the session path could stall on.
type Chart interface {
	// Seat resolves a seat reference — a handle, or one the seat used to
	// answer to — to the handle it is known by now.
	//
	// A REFERENCE RATHER THAN A LOOKUP, because a rename moves a handle
	// and every written-down reference to a seat survives one through the
	// chart's own former-key resolution. An implementation that queried
	// the handle column directly would report every renamed seat as gone,
	// which is a 403 for everybody bound to one.
	//
	// AN ERROR IS THE UNKNOWN ARM, never "no such seat": a view that is
	// not built and a seat that does not exist are 503 and 403.
	Seat(ctx context.Context, ref string) (Seat, bool, error)

	// Position is how far this node's chart applier has committed,
	// packed, and Lag how far behind it is.
	//
	// IT IS THE POSITION THE VIEW WAS BUILT AT, never the applier's own
	// checkpoint, and the difference is a spurious 403. A pinned view
	// lags its applier by a coalescing window, so a seat created inside
	// that window is absent from the view while a checkpoint-derived
	// position already covers the binding that names it — and this
	// function's answer is exactly what decides "the seat is gone"
	// against "this node has not seen the hire". Reporting the
	// checkpoint turns every hire into a brief 403 for the person who
	// was just given the seat.
	Position(ctx context.Context) (position uint64, lag time.Duration, err error)
}

// Seat is what the chart says about one seat, as narrowly as this needs it.
type Seat struct {
	// Handle is the seat as it is addressed NOW, which is what an author
	// column records — not the reference that resolved to it.
	Handle string

	// Kind is agent or human. A person may only be bound to a human seat:
	// an agent seat has an inbox and a turn loop, and a person acting as
	// one would be a human writing under an agent's identity in every
	// audit row in the company.
	Kind string

	// Unit is the path of the unit the seat sits in, which a principal
	// carries so a gate does not re-walk the chart per request.
	Unit string

	// Tombstoned reports a reference the chart holds a REMOVAL record for.
	//
	// THE CHART DELETES THE ROW AND WRITES A TOMBSTONE, so an
	// implementation answers this by reading that tombstone and reports
	// `found` TRUE beside it — which is the whole value of the field: a
	// tombstone is CONCLUSIVE where an absent row is not, so a node can
	// answer 403 from it without first establishing that it has caught up
	// with the binding.
	Tombstoned bool
}

// SeatKindHuman is the only kind a person may be bound to.
const SeatKindHuman = "human"

// SeatRow is which row of the seat table a person landed on.
type SeatRow string

const (
	// SeatRowSeatless is a person who holds no binding. Their login is
	// the handle and the chart is never consulted.
	SeatRowSeatless SeatRow = "seatless"

	// SeatRowHeld is a seat present in the view, human, and not
	// tombstoned. Its handle is the actor.
	SeatRowHeld SeatRow = "held"

	// SeatRowGone is a seat absent or tombstoned on a node whose chart
	// position COVERS the position the binding was decided at.
	SeatRowGone SeatRow = "gone"

	// SeatRowBehind is a seat absent on a node whose chart position is
	// BELOW the binding's, within the stall grace.
	SeatRowBehind SeatRow = "behind"

	// SeatRowStalled is a chart applier past the stall grace, or a view
	// that could not be read at all.
	SeatRowStalled SeatRow = "stalled"
)

// SeatRows are the five, in the order the design's table states them.
var SeatRows = []SeatRow{
	SeatRowSeatless, SeatRowHeld, SeatRowGone, SeatRowBehind, SeatRowStalled,
}

// seatTable is the design's second table, verbatim. One column, because a seat
// resolves or it does not — there is no request this answers differently for.
var seatTable = map[SeatRow]Answer{
	SeatRowSeatless: AnswerServe,
	SeatRowHeld:     AnswerServe,
	SeatRowGone:     AnswerRefuse,
	SeatRowBehind:   AnswerUnavailable,
	SeatRowStalled:  AnswerUnavailable,
}

// CodeNoSeat is what a seat refusal tells the caller.
//
// ITS OWN CODE, unlike the session table's single one, and the asymmetry is
// deliberate: a session refusal must disclose nothing, because an
// unauthenticated caller is holding the cookie. A seat refusal is served to
// somebody whose session has ALREADY validated — they are who they say they
// are — and "the seat you are bound to no longer exists" is the sentence that
// gets them fixed instead of retrying for an hour.
const CodeNoSeat = "seat_unavailable"

// Binding is how a validated person resolved to a seat.
type Binding struct {
	// Row is the table row.
	Row SeatRow

	// Seat is the resolved seat, on [SeatRowHeld] only.
	Seat Seat

	// Detail NAMES THE SEAT on a refusal, which the design states
	// explicitly: "403 forbidden naming the seat". A person locked out
	// because a seat was removed needs to know which one, and so does
	// whoever removed it.
	Detail string

	// Err is the read failure behind [SeatRowStalled], when there was one.
	Err error
}

// Answer is what this binding permits.
func (b Binding) Answer() Answer {
	answer, known := seatTable[b.Row]
	if !known {
		return AnswerRefuse
	}
	return answer
}

// Code is what a refusal or a 503 tells the caller.
func (b Binding) Code() string {
	switch b.Answer() {
	case AnswerUnavailable:
		return CodeUnavailable
	case AnswerRefuse:
		return CodeNoSeat
	}
	return ""
}

// Handle is the name this principal acts under: the seat's when they hold one,
// and empty when they do not.
//
// EMPTY IS A REAL ANSWER HERE AND ONLY HERE. It is reached from
// [SeatRowSeatless] and from nowhere else — never as a fall-through from a
// seat that could not be resolved — because a person the chart could not
// answer for who was quietly given an empty handle would write audit rows
// under nobody's name and read every person-scoped query as empty.
func (b Binding) Handle() string {
	if b.Row == SeatRowHeld {
		return b.Seat.Handle
	}
	return ""
}

// ResolveSeat answers which seat a validated person acts as.
//
// A FREE FUNCTION AND NOT A METHOD ON [Signer], because nothing here is about
// signing: it reads a chart view and a person's row, and a signer that owned
// it would be a key holder with a reason to be on the chart's read path.
func ResolveSeat(ctx context.Context, chart Chart, person PersonRow) Binding {
	if person.Seat == "" {
		return Binding{Row: SeatRowSeatless}
	}
	if chart == nil {
		// NO CHART SEAM IS NOT "no seat". A node wired without one
		// cannot answer the question at all, which is the unknown arm —
		// and answering it as seatless would hand somebody bound to a
		// lead's seat an empty handle and serve the request.
		return Binding{Row: SeatRowStalled,
			Detail: "this node has no chart view to resolve " + person.Seat + " in"}
	}
	position, lag, err := chart.Position(ctx)
	if err != nil {
		return Binding{Row: SeatRowStalled, Err: err,
			Detail: "this node could not read the chart's own position"}
	}
	if lag > statelog.StallGrace {
		return Binding{Row: SeatRowStalled,
			Detail: fmt.Sprintf("the chart applier is %s behind, past the %s "+
				"stall grace", lag, statelog.StallGrace)}
	}
	seat, found, err := chart.Seat(ctx, person.Seat)
	if err != nil {
		return Binding{Row: SeatRowStalled, Err: err,
			Detail: "this node could not read the chart view"}
	}
	if found && !seat.Tombstoned && seat.Kind == SeatKindHuman {
		return Binding{Row: SeatRowHeld, Seat: seat}
	}

	// ABSENT, TOMBSTONED, OR NOT A HUMAN SEAT — and the three share an
	// arm because the question the position then settles is the same one:
	// has this node seen what the binding was decided against?
	detail := fmt.Sprintf("seat %q", person.Seat)
	switch {
	case !found:
		detail += " is not in this node's chart view"
	case seat.Tombstoned:
		detail += " is tombstoned"
	default:
		detail += fmt.Sprintf(" is a %q seat, and a person may only act as a "+
			"%q one", seat.Kind, SeatKindHuman)
	}
	// A WRONG-KIND SEAT IS NEVER A 503. The row is present and says what
	// it is, so no amount of catching up changes the answer — and 503 is
	// reserved for what waiting can fix.
	if found {
		return Binding{Row: SeatRowGone, Seat: seat, Detail: detail}
	}
	if position >= person.SeatAt {
		return Binding{Row: SeatRowGone, Detail: detail +
			", and this node's chart position covers the binding's"}
	}
	return Binding{Row: SeatRowBehind, Detail: detail +
		", and this node has not applied the chart as far as the binding"}
}
