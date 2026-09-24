package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/store"
)

// FleetEvents is how the history answers read the fleet's event stores.
//
// DECLARED HERE, by the consumer, and every method answers a
// [eventfan.Coverage] beside its answer. That signature is the enforcement
// ADR-0021 names: a store answers a page and nothing about who it is missing,
// so a bare *store.EventLog does not satisfy this and a read that quietly
// describes one node does not compile. [eventfan.Fleet] is the implementation.
type FleetEvents interface {
	List(ctx context.Context, q store.ListQuery) (eventfan.Listing, eventfan.Coverage, error)
	Histogram(ctx context.Context, q store.HistogramQuery) (store.EventHistogram, eventfan.Coverage, error)
	ByID(ctx context.Context, id string) (store.EventRecord, eventfan.Coverage, error)
	Trace(ctx context.Context, id string) (eventfan.Trace, eventfan.Coverage, error)
	Turn(ctx context.Context, id string) (eventfan.TurnDetail, eventfan.Coverage, error)
	Turns(ctx context.Context, q store.TurnQuery) (eventfan.TurnPage, eventfan.Coverage, error)
	Phases(ctx context.Context, role string, limit int, before *store.Cursor) (eventfan.Listing, eventfan.Coverage, error)
	SeatPhases(ctx context.Context, agentID, role string, before *store.Cursor) (eventfan.Listing, eventfan.Coverage, error)
}

var _ FleetEvents = (*eventfan.Fleet)(nil)

// EventAnswer is the `event` answer: the record, with the coverage of the
// search for it beside its own fields.
//
// EMBEDDED, so the record's fields stay where every reader already finds them
// and `coverage` is one more key — additive, as every answer's evolution is.
type EventAnswer struct {
	store.EventRecord
	Coverage eventfan.Coverage `json:"coverage"`
}

// SeriesAnswer is the `event_series` answer: the fleet's summed axis, and which
// nodes it was summed over.
type SeriesAnswer struct {
	store.EventHistogram
	Coverage eventfan.Coverage `json:"coverage"`
}
