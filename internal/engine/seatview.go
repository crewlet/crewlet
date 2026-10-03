package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// SeatView answers [session.Chart] — which seat a signed-in person acts as —
// against the org chart this node has applied.
//
// It reads this node's own committed chart rows, at [statelog.ReadStale]: a
// seat the rows do not hold is not a seat anybody may act as here.
type SeatView struct{ reader *chart.Reader }

var _ session.Chart = SeatView{}

// SeatViewOf is the seam over one engine, or the zero value over an engine with
// no core runtime, which holds no chart rows.
//
// THE ZERO VALUE IS NOT NIL, and the difference is what such an engine
// answers. A nil [session.Chart] would make every bound person seatless — the
// one arm [session.Binding.Handle] is documented never to be reached by a
// fall-through — so a view with no reader answers UNKNOWN to every seat
// question instead, which is 503 and says come back to a node that can tell.
func SeatViewOf(e *Engine) SeatView {
	if e == nil {
		return SeatView{}
	}
	return SeatView{reader: e.Chart()}
}

// errNoChartDomain is what a view with no chart reader answers.
var errNoChartDomain = errors.New("engine: this engine holds no org chart, so " +
	"it cannot say which seat anybody holds")

// Seat resolves a binding's seat — by its IDENTITY, the handle it was created
// under — to the seat as it is known now.
//
// AN IDENTITY LOOKUP AND NOT AN ADDRESS ONE ([chart.Reader.SeatByIdentity]),
// because a binding names the seat by its identity (ADR-0027). The ROW and
// nothing else: this runs on every signed-in request and on every binding the
// dangling-binding alarm classifies, and neither reads the seats it manages or
// its history. A removed seat is simply absent.
func (v SeatView) Seat(ctx context.Context, identity string) (session.Seat, bool, error) {
	if v.reader == nil {
		return session.Seat{}, false, errNoChartDomain
	}
	seat, err := v.reader.SeatByIdentity(ctx, identity,
		statelog.Freshness{Level: statelog.ReadStale})
	switch {
	case err == nil:
		return session.Seat{
			Handle: seat.Handle,
			Kind:   string(seat.Kind),
			Unit:   seat.UnitKey,
		}, true, nil
	case errors.Is(err, chart.ErrNotFound):
		return session.Seat{}, false, nil
	}
	// THE UNKNOWN ARM, never "no such seat": an unreadable estate and an
	// absent row are 503 and 403, and the sentinel above is the only thing
	// that tells them apart.
	return session.Seat{}, false, fmt.Errorf(
		"engine: read the seat created under %q: %w", identity, err)
}

// Version names the chart rows [SeatView.Seat] answers from: the position this
// node's chart applier has committed, which moves whenever a row does.
func (v SeatView) Version(context.Context) (uint64, error) {
	if v.reader == nil {
		return 0, errNoChartDomain
	}
	return uint64(v.reader.At().Packed()), nil
}
