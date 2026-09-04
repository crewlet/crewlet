package queries

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/knowledge"
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
	Health func(ctx context.Context) any

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

	// Diary, Episodes and Skills are a seat's memory. Skills is the half
	// that had no query at all: learning.Skills.List exists and is tested,
	// and nothing served it, so a seat's synthesized skills were written,
	// versioned, loadable by the agent itself — and invisible to the
	// operator whose tokens paid for them.
	Diary    *learning.Diary
	Episodes *learning.Episodes
	Skills   *learning.Skills

	// Channels is the fleet's agent-to-agent authorization record. A
	// consumer-defined interface rather than the whole coord.Fleet: this
	// surface reads open channels and nothing else, and a source that could
	// reach the activation pointer would eventually be given a reason to.
	Channels interface {
		OpenChannels(ctx context.Context) ([]coord.Channel, error)
	}

	// Knowledge resolves the company's ONE knowledge backend, behind the
	// same seam a seat's Plan phase searches through — so an operator
	// asking "what would an agent find" gets the answer an agent would
	// get, rather than one from an index somebody has to keep fresh.
	//
	// A FUNCTION, not a value, for the reason [Sources.Company] is one: an
	// apply REPLACES the searcher. A credential rotation, a retired
	// integration block, a knowledge backend repaired after a failed boot
	// — each rebuilds it, and a value captured when the API was assembled
	// keeps searching with the credential that was revoked, against the
	// wiki that was removed, or answers "no backend" forever for a company
	// that has had one since its second minute. Nil, and a nil answer from
	// it, both mean the same thing and leave the question unregistered.
	Knowledge func() knowledge.Searcher

	// Budget is the DURABLE token counter — what the engine actually
	// enforces against, across every node and every restart. Nil answers
	// the budget surface with `durable: false` rather than zeros: "nobody
	// looked" and "nothing was spent" are different facts, and only one of
	// them is a measurement.
	//
	// A consumer-defined interface rather than the whole coord.Fleet: the
	// budget screen reads spend and nothing else, and a source that could
	// reach the activation pointer would eventually be given a reason to.
	Budget interface {
		Usage(ctx context.Context) ([]coord.Usage, error)
	}

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
	Routed func(ctx context.Context) []string

	// Verifiable names the integrations whose resolved material could
	// accept a delivery, or nil when this process cannot say. Populated
	// from the same NodeRuntime as Routed, and kept apart from it for the
	// same reason: "would a delivery be verified" and "would a verified
	// delivery reach anyone" fail independently, and an operator staring at
	// a silent integration has to know which half broke.
	Verifiable func(ctx context.Context) []string

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
		// A turn is its own question, not a slice of the trace: one trace
		// can span several turns and one turn several traces. See the
		// answer, and migration 0014 which made it askable at all.
		r.Register("turn", s.turn)
		// The company's phase records, with their payloads. `events` cannot
		// serve this: its listing never selects the payload, and a phase
		// record without one has no prompts, no response and no decision.
		r.Register("phases", s.phases)
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
		// Gated on the COMPANY, not on the searcher, for the same reason
		// budgets is: "this company has no knowledge backend configured" is
		// a fact the company alone establishes, and it is a far more useful
		// answer than an unknown query. A nil searcher IS the answer here,
		// not the absence of one.
		r.Register("knowledge", s.knowledgeSearch)
	}
	if s.Sandbox != nil {
		r.Register("sandbox_runs", s.sandboxRuns)
	}
	if s.Diary != nil || s.Episodes != nil || s.Skills != nil {
		r.Register("agent_memory", s.agentMemory)
	}
	if s.Channels != nil {
		r.Register("a2a_channels", s.a2aChannels)
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
func (s Sources) agent(ctx context.Context, p Params) (any, error) {
	// EITHER NAME, and the seat may be addressed by handle or by role.
	//
	// The dashboard sends `id`, carrying the handle — `query("agent", {id})`
	// from the seat page, and /agents/{id} from the REST table — while the
	// projection keys its overlays by ROLE NAME, which is what the engine's
	// `agents` push carries. Reading only `role` meant every seat page
	// answered 400 and rendered its error state; the client is the
	// compatibility reference for a frame's shape, so the answer
	// takes what the client sends and resolves it.
	seat := firstOf(p.String("id"), p.String("role"))
	role := s.roleOf(seat)
	if role == "" {
		return nil, fmt.Errorf("%w: agent needs a handle or a role", ErrBadParams)
	}
	// LIVE STATE AND HISTORY, which are two different sources and always
	// were: the projection holds the call in flight, the event store holds
	// the ones that finished. The answer carried only the first, so a seat
	// page showed the round happening now and nothing before it — while the
	// spend chart beside it, which reads the store, reported every phase
	// the turn had already completed. Two panels on one screen disagreeing
	// about whether a seat had done anything.
	//
	// A seat the projection has never seen is NOT an error: a role
	// configured and never spawned is exactly that, and a 404 there would
	// make a healthy new company look broken. Its history is answered the
	// same way.
	history, next := s.phaseHistory(ctx, seat, role, p)
	answer := map[string]any{
		"role": role,
		"live": nil,
		// The rows are `store.EventRecord`s, PAYLOAD NESTED — the same
		// shape `event`, `trace` and `turn` answer with. They used to be
		// the payload flattened with an id and a timestamp merged in,
		// which meant one client had to carry two readers for one kind of
		// thing and a field added to the envelope reached three screens
		// and not the fourth.
		"llm_history": history,
		// The cursor the caller pages with, or "" at the end of the
		// record. Its absence is why a seat's transcript was a hard fifty
		// rows with no way past them, while the events it was made of sat
		// in the store addressable by id.
		"next": next,
	}
	// ASSIGNED ONLY WHEN THERE IS ONE, because a nil *Overlay stored in an
	// any is not nil: every `live != nil` check above this layer would read
	// a seat that has never run as one that has. It marshals to null either
	// way, so the client never saw it and only a Go caller would — which is
	// exactly the kind of trap that survives until something depends on it.
	if overlay := s.State.AgentOverlay(role); overlay != nil {
		answer["live"] = overlay
	}
	return answer, nil
}

// phaseHistory is the seat's finished calls, newest first, as the rows the
// dashboard renders beside the live one.
//
// BEST EFFORT. The live half is the answer's point and it comes from memory;
// refusing the whole seat page because the event log could not be read would
// turn a degraded history into no screen at all. An unreadable log and a seat
// that has not run yet both render as "no invocations", which is the same
// thing a reader can see for themselves from the phase chart above it.
//
// Each row is the event's PAYLOAD with the envelope's timestamp merged in: the
// payload's field names are already the client's — turn_id, phase, iteration,
// model, response, tool_executions, total_tokens, cost_usd — because the same
// shape drives the live row, and the timestamp is the one field that lives on
// the envelope rather than inside it.
func (s Sources) phaseHistory(ctx context.Context, seat, role string, p Params) ([]store.EventRecord, string) {
	if s.Events == nil {
		return []store.EventRecord{}, ""
	}
	var before *store.Cursor
	if id := p.String("before_id"); id != "" {
		at, err := time.Parse(time.RFC3339Nano, p.String("before_time"))
		if err == nil {
			before = &store.Cursor{Time: at, ID: id}
		}
		// A malformed cursor falls back to the newest page rather than
		// failing: this is best effort, and refusing the whole seat page
		// over a bad query parameter would turn a paging bug into no
		// screen at all.
	}
	records, err := s.Events.AgentPhases(ctx, s.agentIDOf(seat), role, before)
	if err != nil {
		log.WarnContext(ctx, "agent_history_unavailable", "seat", seat, "error", err)
		return []store.EventRecord{}, ""
	}
	if records == nil {
		return []store.EventRecord{}, ""
	}
	// The cursor is the LAST row's key, echoed rather than left for a client
	// to assemble: (time, id) is the table's key, and a client rebuilding it
	// from a rendered timestamp would lose the sub-second precision the
	// tiebreak depends on.
	last := records[len(records)-1]
	next := ""
	if len(records) == store.AgentPhaseLimit {
		next = last.Time.UTC().Format(time.RFC3339Nano) + "|" + last.ID
	}
	return records, next
}

// firstOf returns the first non-empty value.
func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// roleOf resolves a seat identifier to the ROLE NAME the projection keys on.
//
// A handle resolves through the org; anything else is passed through as a role
// name, so a caller that already had one is unaffected. An unknown identifier
// comes back unchanged rather than empty: the answer for a seat the projection
// has never seen is a live-state-free row, not an error, and a company that
// renamed a role should not turn a bookmarked page into a failure.
func (s Sources) roleOf(id string) string {
	if id == "" || s.Company == nil {
		return id
	}
	company := s.Company()
	if company == nil {
		return id
	}
	organization, err := company.Organization()
	if err != nil {
		return id
	}
	if role := organization.AgentSeatByHandle(id); role != nil {
		return role.Name
	}
	return id
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
func (s Sources) stream(ctx context.Context, _ Params) (any, error) {
	return s.Health(ctx), nil
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
	// SAYS WHEN IT CUT. EventLog.Trace stops at store.MaxTraceEvents and its
	// own doc asks the caller to report that — a trace shown short with no
	// note reads as a complete causal chain that simply ends, which is the
	// one thing a reader must not conclude from it. Additive, so a client
	// that predates the field is unaffected.
	return map[string]any{
		"trace_id":  id,
		"events":    rows,
		"truncated": len(rows) >= store.MaxTraceEvents,
	}, nil
}
