package session

import (
	"context"
	"fmt"
)

// THE SECOND TABLE: which seat a validated person acts as.
//
// A person's identity is on the iam log and their seat is a seat of the
// company this node is running — the organisation of the configuration epoch
// it applied. So a binding resolves against the RUNNING ORG, and a seat missing
// from it is one of two answers:
//
//   - the seat is GONE, or it is not a human seat: the company this node runs
//     does not hold it as one. 403 NAMING THE SEAT — never a silent
//     fall-through to a seatless principal, which would reach every
//     person-scoped read as an empty answer and look like a person with
//     nothing assigned to them.
//   - this node CANNOT ANSWER: it runs no company yet, or the lookup failed.
//     503.
//
// THERE IS NO "NOT YET" ROW. The org changes only when a revision is
// activated, and every node applies the activation pointer within seconds of
// it moving; the window in which a node that has not applied a revision adding
// a seat answers 403 for that seat is that short, and it is not worth a
// position on every binding and a comparison on every request.
//
// AND THE SEATLESS ARM IS RESERVED FOR A PERSON WHO HOLDS NO BINDING. The org
// is never consulted for them: their login is the handle, which is exactly why
// internal/iam makes a login and a seat handle disjoint by grammar — the two
// namespaces meet in one author column and neither may be mistaken for the
// other.

// Chart is what the organisation this node is running says about one seat —
// the org chart of the configuration epoch it applied.
//
// DEFINED HERE, by the caller, and one method wide: what resolving a principal
// needs is whether one seat is there and what it is, and a seam that asked for
// more would be one the session path could stall on.
type Chart interface {
	// Seat resolves a binding's seat, named by its handle, in the running
	// organisation.
	//
	// A SEAT THE ORGANISATION DOES NOT HOLD answers not found and no
	// error. AN ERROR IS THE UNKNOWN ARM, never "no such seat": a node
	// running no company and a seat that does not exist are 503 and 403.
	Seat(ctx context.Context, handle string) (Seat, bool, error)
}

// Seat is what the chart says about one seat, as narrowly as this needs it.
type Seat struct {
	// Handle is the seat's handle, which is what an author column records.
	Handle string

	// Kind is agent or human. A person may only be bound to a human seat:
	// an agent seat has an inbox and a turn loop, and a person acting as
	// one would be a human writing under an agent's identity in every
	// audit row in the company.
	Kind string

	// Unit is the key of the unit the seat sits in, which a principal
	// carries so a gate does not re-walk the chart per request.
	Unit string

	// Name is the seat's display name, for a page that shows a person
	// which seat they are being given. Nothing decides on it.
	Name string
}

// SeatKindHuman is the only kind a person may be bound to.
const SeatKindHuman = "human"

// SeatRow is which row of the seat table a person landed on.
type SeatRow string

const (
	// SeatRowSeatless is a person who holds no binding. Their login is
	// the handle and the chart is never consulted.
	SeatRowSeatless SeatRow = "seatless"

	// SeatRowHeld is a seat the running organisation holds as a human
	// seat. Its handle is the actor.
	SeatRowHeld SeatRow = "held"

	// SeatRowGone is a seat the running organisation does not hold, or
	// holds as an agent seat.
	SeatRowGone SeatRow = "gone"

	// SeatRowStalled is a node that cannot say: it runs no company yet,
	// or the lookup failed.
	SeatRowStalled SeatRow = "stalled"
)

// SeatRows are the four, in the order the table states them.
var SeatRows = []SeatRow{
	SeatRowSeatless, SeatRowHeld, SeatRowGone, SeatRowStalled,
}

// seatTable is the design's second table, verbatim. One column, because a seat
// resolves or it does not — there is no request this answers differently for.
var seatTable = map[SeatRow]Answer{
	SeatRowSeatless: AnswerServe,
	SeatRowHeld:     AnswerServe,
	SeatRowGone:     AnswerRefuse,
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
// signing: it reads the running organisation and a person's row, and a signer
// that owned it would be a key holder with a reason to be on the org's read
// path.
//
// It is also what resolves a Tier A token's directory binding
// (internal/api/auth's SeatBindings), handed the machine row a token binds
// through: one seat, held by a cookie or by a token, must be one answer, and a
// second resolution beside this one is where the two would drift.
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
			Detail: "this node has no organisation to resolve " + person.Seat + " in"}
	}
	seat, found, err := chart.Seat(ctx, person.Seat)
	if err != nil {
		return Binding{Row: SeatRowStalled, Err: err,
			Detail: "this node could not read the organisation it runs"}
	}
	if found && seat.Kind == SeatKindHuman {
		return Binding{Row: SeatRowHeld, Seat: seat}
	}
	// ABSENT OR NOT A HUMAN SEAT. Both are the running company's own answer,
	// so no amount of waiting changes either, and 503 is reserved for what
	// waiting can fix.
	detail := fmt.Sprintf("seat %q", person.Seat)
	if !found {
		return Binding{Row: SeatRowGone, Detail: detail +
			" is not a seat of the company this node runs"}
	}
	return Binding{Row: SeatRowGone, Seat: seat, Detail: detail + fmt.Sprintf(
		" is a %q seat, and a person may only act as a %q one", seat.Kind,
		SeatKindHuman)}
}
