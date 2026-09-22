package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// SeatView answers [session.Chart] — which seat a signed-in person acts as —
// against the org chart this node has applied.
//
// # Why it reads the chart domain and not the running company
//
// [ChartAuthority] asks the RUNNING company's org (`e.Company().Org`), because
// what it needs is the tree with lead inheritance and manages-expansion
// applied, which is a derivation the chart rows do not hold. This needs
// something else entirely, and the difference is a spurious 403.
//
// The running org is composed at an EPOCH. It is rebuilt when a revision is
// applied, not when a chart record lands, so a seat created a second ago is
// absent from it while the chart applier has long since committed the row —
// and `e.Company().ChartAt` names the position that composition read, which is
// BEHIND the chart's own checkpoint by however long it has been since the last
// apply. A person hired between two epochs would be told their seat is gone,
// on a node that has the seat, for as long as nothing else triggered a
// recompose.
//
// So this reads the chart domain directly, at [statelog.ReadStale] — this
// node's own committed rows — and takes its position from the same reader.
// The two then come from ONE source with nothing between them, which is what
// makes [session.Chart]'s central distinction sound: a seat absent from a
// view whose position covers the binding is GONE, and one absent from a view
// below it is a node that has not seen the hire. Reading rows from one place
// and a position from another is how those two answers swap.
type SeatView struct{ reader *chart.Reader }

var _ session.Chart = SeatView{}

// SeatViewOf is the seam over one engine, or the zero value on a node that
// runs no chart domain.
//
// THE ZERO VALUE IS NOT NIL, and the difference is what a satellite answers. A
// nil [session.Chart] would make every bound person seatless — the one arm
// [session.Binding.Handle] is documented never to be reached by a
// fall-through — so a node with no reader answers UNKNOWN to every seat
// question instead, which is 503 and says come back to a node that can tell.
func SeatViewOf(e *Engine) SeatView {
	if e == nil {
		return SeatView{}
	}
	return SeatView{reader: e.Chart()}
}

// errNoChartDomain is what a node that applies no chart records answers.
var errNoChartDomain = errors.New("engine: this node runs no org chart, so it " +
	"cannot say which seat anybody holds")

// Seat resolves a reference to the seat it names now.
//
// A REFERENCE RATHER THAN A HANDLE LOOKUP, which [chart.Reader.Seat] already
// is: it resolves a former handle through the chart's own rename trail, so a
// person bound to a seat that was renamed keeps resolving rather than being
// told their seat is gone.
//
// AN ABSENT SEAT IS PROBED FOR A TOMBSTONE, because a tombstone is CONCLUSIVE
// where an absence is not. [session.ResolveSeat] can answer 403 from one
// without first establishing that this node has caught up with the binding,
// and that is the difference between refusing a leaver immediately and making
// them wait out a grace that will never change the answer.
func (v SeatView) Seat(ctx context.Context, ref string) (session.Seat, bool, error) {
	if v.reader == nil {
		return session.Seat{}, false, errNoChartDomain
	}
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	detail, err := v.reader.Seat(ctx, ref, fresh)
	switch {
	case err == nil:
		return session.Seat{
			Handle: detail.Seat.Handle,
			Kind:   string(detail.Seat.Kind),
			Unit:   detail.Seat.UnitKey,
		}, true, nil
	case !errors.Is(err, chart.ErrNotFound):
		// THE UNKNOWN ARM, never "no such seat": an unreadable estate
		// and an absent row are 503 and 403, and the sentinel above is
		// the only thing that tells them apart.
		return session.Seat{}, false, fmt.Errorf(
			"engine: read the seat %q: %w", ref, err)
	}
	removal, found, _, err := v.reader.Removed(ctx,
		chart.ObjectRef{Kind: chart.KindSeat, ID: ref}, fresh)
	if err != nil {
		return session.Seat{}, false, fmt.Errorf(
			"engine: read the removal of the seat %q: %w", ref, err)
	}
	if !found {
		// ABSENT AND NOT TOMBSTONED. Reported as not found with no
		// error, which is what sends the caller to the position
		// comparison: this is either a seat that never existed or a
		// hire this node has not applied, and only the position can
		// say which.
		return session.Seat{}, false, nil
	}
	return session.Seat{
		Handle:     removal.Object.ID,
		Tombstoned: true,
	}, true, nil
}

// Position is how far this node's chart applier has committed, and how far
// behind the log it is.
func (v SeatView) Position(context.Context) (uint64, time.Duration, error) {
	if v.reader == nil {
		return 0, 0, errNoChartDomain
	}
	at := v.reader.At()
	return uint64(at.Packed()), v.reader.Lag(), nil
}
