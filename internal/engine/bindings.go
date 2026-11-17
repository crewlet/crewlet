package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A SEAT BINDING DANGLES when its seat is not a HUMAN seat of the running
// company. The check when a binding is made is ADVISORY (the org changes
// after, or a revision applied elsewhere is unseen here), so the residue is
// legal and only a record clears it: a person is moved or removed (ADR-0032),
// a service account unbound or moved. Reported by `crewlet iam check` and
// `GET /iam/check` (`binding_dangling`); enforced on the request path
// (`403 seat_unavailable`) and at the company write (`409 seat_held`). A
// finding, never an alarm: it is the company's CONTENT, which every node would
// raise at once and none can repair (ADR-0015). The rule is
// [session.ResolveSeat]'s table, never a predicate of its own: asking whether
// the chart holds the handle misses a person bound to an AGENT seat.

// bindingProbeBudget bounds one person's seat lookup. Per ROW, on a report that
// may cover every binding, so it is one local SQL read on a busy node at its
// worst, not a network call. A lookup past it is the unknown arm: unchecked.
const bindingProbeBudget = 2 * time.Second

// errSeatUnknown is the unknown arm when the resolver has no read failure of
// its own to name: a node running no company at all.
var errSeatUnknown = errors.New("engine: this node runs no company, so it " +
	"cannot say whether that seat exists")

// DanglingBinding classifies one person's seat binding against the company this
// node runs: whether it dangles, and if so the sentence every surface prints —
// which seat, why, and what to do.
//
// THREE-VALUED: an error when this node cannot tell (no company yet, or the
// lookup failed), which the report counts as unchecked: a guess of dangling
// sends an administrator to move somebody whose seat is there.
//
// Its shape is internal/api/iamapi's Bindings seam, so the method value is
// wired in with nothing adapting between them.
func (e *Engine) DanglingBinding(ctx context.Context, row iamdomain.PersonRow) (
	dangling bool, detail string, err error) {

	return danglingBinding(ctx, SeatViewOf(e), row.Binding())
}

// danglingBinding is the rule, over any organisation.
func danglingBinding(ctx context.Context, chart session.Chart, b iamdomain.SeatBinding) (
	bool, string, error) {

	if b.Seat == "" {
		// NO BINDING, nothing to dangle: the request path serves the row
		// under its login (a service account's; a person always holds a
		// seat, ADR-0032), and widening the rule would make it disagree
		// with the seat table. A removed person has no row at all.
		return false, "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, bindingProbeBudget)
	defer cancel()
	binding := session.ResolveSeat(ctx, chart, session.PersonRow{
		Found: true, Stage: b.Stage, Seat: b.Seat,
	})
	switch binding.Row {
	case session.SeatRowGone:
		return true, binding.Detail + danglingRemedy(b.Kind), nil
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

// danglingRemedy is the one-record repair, which depends on WHO is bound. A
// person is never told to unbind, which the directory refuses
// (`seat_required`, ADR-0032): a person, and a kind this build does not name,
// get a move or a removal. A service account's seat is optional, so it gets
// an unbind or a move.
func danglingRemedy(kind iam.Kind) string {
	if kind == iam.KindMachine {
		return ". Unbind them, or move them to another human seat"
	}
	return ". Move them to another human seat, or remove them"
}
