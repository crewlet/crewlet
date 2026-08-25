package queries

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

var log = logging.Get("api.queries")

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

	// Company reads the CURRENT epoch, for the questions answered from
	// configuration rather than from a store. A function, not a value: an
	// apply replaces the epoch, and an answer bound to the one this
	// process booted on would describe a company that is no longer running.
	Company func() *config.Company

	// Coord is the lease table — the fleet's one shared answer to "which
	// node holds what". Nil leaves the fleet question unregistered, which
	// is honest for a process with no coordination backend.
	Coord coord.Backend

	// Plane is the control plane, for the config columns of the fleet view.
	Plane coord.Plane

	// Runs is the schedule dispatch ledger. Nil still answers the
	// schedules question — the configured schedules are a projection of
	// the org — with an empty history, because "no ledger" and "nothing
	// has fired" are told apart by the answer's own shape.
	Runs ScheduleRuns

	// Conversations is what each seat already said in each thread it works.
	Conversations ledgerstore.Conversations

	// Diary and Episodes are a seat's memory.
	Diary    *learning.Diary
	Episodes *learning.Episodes

	// Budget is the DURABLE token counter — what the engine actually
	// enforces against, across every node and every restart. Nil answers
	// the budget surface with `durable: false` rather than zeros: "nobody
	// looked" and "nothing was spent" are different facts, and only one of
	// them is a measurement.
	Budget *store.Budgets

	// Sandbox is the durable record of detached coding runs. Nil leaves
	// the question unregistered, which is honest for a node with no
	// sandbox backend: without one no run can be parked, so there is
	// nothing this question could describe.
	Sandbox PendingRuns

	// Config serves the config family, and every one of those is
	// operator-gated: reading the document exposes the whole company.
	Config *configapi.Service

	// Routed names the integrations whose deliveries can wake a seat, or
	// nil when this process cannot say — a standalone API has no engine to
	// ask. The app populates it from its NodeRuntime; nil here is not an
	// error and not "none route", and the integrations answer keeps those
	// three apart rather than folding them into a boolean.
	Routed func() []string

	// Verifiable names the integrations whose resolved material could
	// accept a delivery, or nil when this process cannot say. Populated
	// from the same NodeRuntime as Routed, and kept apart from it for the
	// same reason: "would a delivery be verified" and "would a verified
	// delivery reach anyone" fail independently, and an operator staring at
	// a silent integration has to know which half broke.
	Verifiable func() []string

	// NodeID names this node in the fleet answer, so a reader can tell
	// which row is the one they are talking to.
	NodeID string

	// Now is injectable so a test can pin the lease countdowns and the
	// next-run projection.
	Now func() time.Time
}

// ScheduleRuns is the dispatch history a schedules answer reads.
//
// Declared here rather than imported so this package depends on the shape and
// not on the ledger. Satisfied by the SQL ledger and its memory twin alike.
type ScheduleRuns interface {
	Recent(ctx context.Context, limit int) ([]schedule.Run, error)
}

// clock reads the injected time, or the wall clock.
func (s Sources) clock() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now()
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
	if s.Coord != nil {
		r.Register("fleet", s.fleet)
	}
	if s.Company != nil {
		// Gated on the COMPANY, not on the durable counter: the caps are
		// what the screen is about, and a node with a company and no store
		// answers "these are the ceilings, and nobody can read the usage"
		// — which is a real state an operator needs to see, and is not the
		// same as the question being unavailable here.
		r.Register("budgets", s.budgets)
		// Both are projections of the epoch: what the company DECLARES,
		// which is a different question from what it has done.
		r.Register("schedules", s.schedules)
		r.Register("integrations", s.integrations)
	}
	if s.Sandbox != nil {
		r.Register("sandbox_runs", s.sandboxRuns)
	}
	if s.Conversations != nil {
		r.Register("conversations", s.conversations)
	}
	if s.Diary != nil || s.Episodes != nil {
		r.Register("agent_memory", s.agentMemory)
	}
	if s.Config != nil {
		// OPERATOR-ONLY, all three. Reading the config document exposes
		// the whole company — its org chart, which integrations are
		// wired, and every ${VAR} reference by name — which is what makes
		// /config the one prefix never eligible for anonymous read.
		r.RegisterOperator("config", s.configDocument)
		r.RegisterOperator("config_audit", s.configAudit)
		r.RegisterOperator("config_diff", s.configDiff)
		r.RegisterOperator("config_entities", s.configEntities)
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
// tokens answers the spend breakdown.
//
// TWO SOURCES, one aggregation. The live projection holds the records for its
// own window and answers instantly; any other window is a scan of the event
// store. Both are folded by internal/tokens, so the number a reader sees when
// they change the window is comparable with the one they were looking at — a
// second implementation for the second source is precisely how those two came
// to disagree before.
//
// The live window is the default because it is what the dashboard opens on: a
// page load that scanned the store would put a query on the critical path of
// every tab, for an answer already in memory.
func (s Sources) tokens(ctx context.Context, p Params) (any, error) {
	opts := tokens.Options{
		Handles:     s.RoleHandles(),
		AgentRole:   p.String("agent_role"),
		RecentTurns: Clamp(p.Int("recent_turns", 0), tokens.DefaultRecentTurns, tokens.MaxRecentTurns),
	}
	live := livestate.LiveSpendWindowDays()
	days := p.Int("since_days", live)

	// The live window, unfiltered, is the one the projection can answer.
	if days == live && opts.AgentRole == "" {
		opts.SinceDays = live
		return tokens.Aggregate(s.State.SpendRecords(), opts), nil
	}
	if s.Events == nil {
		// No event store: the honest answer for a window this node cannot
		// see is an EMPTY rollup labelled with the window asked for, not
		// the live one relabelled — which would put a week's heading over
		// an hour's numbers.
		opts.SinceDays = days
		return tokens.Aggregate(nil, opts), nil
	}
	records, err := s.Events.PhaseTokens(ctx, store.PhaseTokenQuery{
		SinceDays: days, AgentRole: opts.AgentRole,
	})
	if err != nil {
		return nil, err
	}
	// Reported as the store CLAMPED it, not as asked: a request for a year
	// answered over thirty days and labelled a year is a lie about the
	// numbers beside it.
	opts.SinceDays = Clamp(days, store.DefaultPhaseTokenDays, store.MaxPhaseTokenDays)
	return tokens.Aggregate(records, opts), nil
}

// RoleHandles maps each seat's role name to its handle, for the per-agent
// rollup's cross-links. Empty when this node has no company — a standalone API
// links to nothing rather than guessing a handle.
//
// Exported because the live stream needs the same map for the rollup it
// pushes: two derivations of "which handle is this role" is how a pushed row
// and a queried one come to link to different pages.
func (s Sources) RoleHandles() map[string]string {
	out := map[string]string{}
	if s.Company == nil {
		return out
	}
	company := s.Company()
	if company == nil {
		return out
	}
	for _, role := range company.Roles {
		if role.Name == "" {
			continue
		}
		// The SAME derivation the org uses, not a re-spelling of it: a
		// handle that differs from the seat's real one is a cross-link
		// to a page that does not exist.
		handle := role.Handle
		if handle == "" {
			handle = org.Slugify(role.Name)
		}
		out[role.Name] = handle
	}
	return out
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
			return nil, fmt.Errorf("%w: before_id needs a before_time: %w", ErrBadParams, err)
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
