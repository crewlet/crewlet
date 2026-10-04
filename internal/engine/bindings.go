package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A SEAT BINDING THAT DANGLES, and the one rule that says so.
//
// # The residue a bind leaves
//
// A person's binding lives on the IDENTITY log and the seat it names in the
// company this node is running. A bind checks the seat against the running org
// when it is made, and that check is ADVISORY: the org can drop the seat
// afterwards, and a revision applied elsewhere first is one this node has not
// seen. The residue — a binding to a seat the running company does not hold as
// a human seat — is LEGAL rather than corruption, and nothing clears it but a
// record: an unbind, or a bind to another seat. It is reported in one place,
// `crewlet iam check` and `GET /iam/check` (`binding_dangling`, naming the
// seat), and it is ENFORCED, which is not reporting, on the request path
// (`403 seat_unavailable`) and at the company write (`409 seat_held`).
//
// # Why it is a finding and never an alarm
//
// An alarm answers whether a NODE is doing its job (ADR-0015). A person bound
// to a seat the company no longer holds is a fact about the company's CONTENT:
// every node would raise it at once, for a state no node can repair, and its
// age is nowhere in any record — so an alarm over it had to keep a clock of
// its own per node. The report names who and why when somebody asks, which is
// all a residue a record repairs needs.
//
// # Why the rule is the REQUEST PATH's own table
//
// [session.ResolveSeat] is what decides, per request, whether a signed-in
// person is served, refused 403 naming their seat, or held off 503 — and a
// binding is dangling exactly when that table would refuse them for want of the
// seat. A second predicate written here ("does the chart hold this
// handle") is how the report came to miss a binding to an AGENT seat: it asked
// whether the row existed, the request path asks whether it is a human seat,
// and a person the request path refused on every call was one the report said
// nothing about. So the report asks this function, and this function asks the
// table.

// bindingProbeBudget bounds one person's seat lookup.
//
// TWO SECONDS, and it is a per-ROW budget on a report that may cover every
// binding in the company — so the number is what one local SQL read on a busy
// node costs at its worst rather than what a network call would. A lookup that
// cannot answer inside it is the unknown arm, which the report counts as
// unchecked.
const bindingProbeBudget = 2 * time.Second

// errSeatUnknown is the unknown arm when the resolver has no read failure of
// its own to name: a node running no company at all.
var errSeatUnknown = errors.New("engine: this node runs no company, so it " +
	"cannot say whether that seat exists")

// DanglingBinding classifies one person's seat binding against the company this
// node runs: whether it dangles, and if so the sentence every surface prints —
// which seat, why, and what to do.
//
// THREE-VALUED: dangling, not dangling, or an error when this node cannot tell
// — it runs no company yet, or the lookup failed. The report counts the third
// as unchecked rather than guessing, because reporting a binding as dangling on
// a node that could not say sends an administrator to unbind somebody whose
// seat is perfectly there.
//
// ITS SHAPE IS THE REPORT'S SEAM, internal/api/iamapi's Bindings, so the
// method value is what the report is wired with and nothing adapts between
// them.
func (e *Engine) DanglingBinding(ctx context.Context, row iamdomain.PersonRow) (
	dangling bool, detail string, err error) {

	return danglingBinding(ctx, SeatViewOf(e), row.Binding())
}

// danglingBinding is the rule, over any organisation.
func danglingBinding(ctx context.Context, chart session.Chart, b iamdomain.SeatBinding) (
	bool, string, error) {

	if b.Seat == "" {
		// NO BINDING. A removed person has no row to be asked about at
		// all: a removal deletes it and leaves the tombstone.
		return false, "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, bindingProbeBudget)
	defer cancel()
	binding := session.ResolveSeat(ctx, chart, session.PersonRow{
		Found: true, Stage: b.Stage, Seat: b.Seat,
	})
	switch binding.Row {
	case session.SeatRowGone:
		return true, binding.Detail + "; unbind them, or bind them to " +
			"another seat", nil
	case session.SeatRowStalled:
		cause := binding.Err
		if cause == nil {
			cause = errSeatUnknown
		}
		return false, "", fmt.Errorf("engine: resolve %q's seat %q: %s: %w",
			b.Person, b.Seat, binding.Detail, cause)
	}
	return false, "", nil
}
