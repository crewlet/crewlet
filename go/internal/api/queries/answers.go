package queries

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/store"
)

// Page sizes. Two numbers per listing, and both are load-bearing: the default
// is what a dashboard gets when it names none, and the ceiling is what stops
// one tab pulling the whole log through a process every other tab shares.
const (
	// DefaultEventPage matches what one screen of the activity feed shows.
	// Larger would spend a round trip on rows nobody scrolls to; smaller
	// would make the first scroll a second query.
	DefaultEventPage = 100

	// MaxEventPage bounds it. Chosen against the feed ring the projection
	// keeps — asking for more than the live buffer holds is a request the
	// store answers and the screen cannot use.
	MaxEventPage = livestate.EventFeedLimit
)

// Sources are what the answers read from.
//
// Every field is optional, and an absent one makes its questions report
// themselves unavailable rather than answer emptily. A standalone API with no
// store and an engine mid-boot are both real, and "there is no event log here"
// and "the event log is empty" are answers a screen must be able to tell apart.
type Sources struct {
	State  *livestate.LiveState
	Events *store.EventLog

	// Health answers the stream question, which is deliberately not called
	// health: a query must never share a name with a push kind, or a
	// reader of the protocol has to know which direction a frame was
	// travelling to know what it means.
	Health func() any
}

// ErrUnavailable is a question this process cannot answer because the thing it
// reads from is not wired here.
//
// Distinct from an empty answer, and the distinction is the point: a dashboard
// that drew "no events" for "this node has no event log" would report a quiet
// company during a misconfiguration.
var ErrUnavailable = errors.New("queries: not available on this node")

// Register wires every question these sources can answer.
//
// A question whose source is missing is NOT registered, so it comes back as
// unknown rather than as a failure — the honest answer for a node that does not
// have that surface at all.
func Register(r *Registry, s Sources) {
	if s.State != nil {
		r.Register("agent", s.agent)
		r.Register("tokens", s.tokens)
	}
	if s.Events != nil {
		r.Register("events", s.events)
		r.Register("event", s.event)
		r.Register("trace", s.trace)
	}
	if s.Health != nil {
		r.Register("stream", s.stream)
	}
}

// agent answers one seat's live state.
func (s Sources) agent(_ context.Context, p Params) (any, error) {
	role := p.String("role")
	if role == "" {
		return nil, fmt.Errorf("%w: agent needs a role", ErrBadParams)
	}
	overlay := s.State.AgentOverlay(role)
	if overlay == nil {
		// A seat the projection has never seen. Not an error: a role
		// configured and never spawned is exactly this, and a 404 there
		// would make a healthy new company look broken.
		return map[string]any{"role": role, "live": nil}, nil
	}
	return map[string]any{"role": role, "live": overlay}, nil
}

// tokens answers the live spend window.
func (s Sources) tokens(_ context.Context, _ Params) (any, error) {
	records := s.State.SpendRecords()
	return map[string]any{
		"window_hours": int(livestate.LiveSpendWindow / time.Hour),
		"records":      records,
	}, nil
}

// stream answers the engine's health.
func (s Sources) stream(_ context.Context, _ Params) (any, error) {
	return s.Health(), nil
}

// events answers a page of the log.
//
// The filters are the store's own, passed through rather than re-implemented:
// a listing this surface filtered itself would page differently from one the
// store filtered, and the difference shows up as rows that vanish when a reader
// scrolls.
func (s Sources) events(ctx context.Context, p Params) (any, error) {
	q := store.ListQuery{
		Limit:        Clamp(p.Int("limit", 0), DefaultEventPage, MaxEventPage),
		Type:         p.String("type"),
		Source:       p.String("source"),
		Category:     p.String("category"),
		TraceID:      p.String("trace_id"),
		Actor:        p.String("actor"),
		RelatedAgent: p.String("agent"),
	}
	if before := p.String("before_id"); before != "" {
		at, err := time.Parse(time.RFC3339Nano, p.String("before_time"))
		if err != nil {
			return nil, fmt.Errorf("%w: before_id needs a before_time: %v", ErrBadParams, err)
		}
		q.Before = &store.Cursor{Time: at, ID: before}
	}

	rows, err := s.Events.List(ctx, q)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"events": rows,
		// The cursor the caller pages with next, echoed rather than left
		// for a client to assemble: (time, id) is the table's key and a
		// client that built it from the last row's fields would be
		// reimplementing the one thing that must not drift.
		"next": cursorOf(rows),
		// A page shorter than the limit does NOT mean history is
		// exhausted when a related-agent filter is set: that filter
		// over-fetches and post-filters, so only a zero-row page ends
		// the walk. Saying so beats a client inferring it wrongly.
		"exhausted": len(rows) == 0,
	}, nil
}

// cursorOf is the position a caller resumes from, or nil at the end.
func cursorOf(rows []store.EventRecord) any {
	if len(rows) == 0 {
		return nil
	}
	last := rows[len(rows)-1]
	return map[string]any{
		"before_time": last.Time.UTC().Format(time.RFC3339Nano),
		"before_id":   last.ID,
	}
}

// event answers one row, payload included.
func (s Sources) event(ctx context.Context, p Params) (any, error) {
	id := p.String("id")
	if id == "" {
		return nil, fmt.Errorf("%w: event needs an id", ErrBadParams)
	}
	rec, err := s.Events.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// trace answers every row sharing one trace.
func (s Sources) trace(ctx context.Context, p Params) (any, error) {
	id := p.String("trace_id")
	if id == "" {
		return nil, fmt.Errorf("%w: trace needs a trace_id", ErrBadParams)
	}
	rows, err := s.Events.Trace(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"trace_id": id, "events": rows}, nil
}
