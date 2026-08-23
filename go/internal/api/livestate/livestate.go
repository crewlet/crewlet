// Package livestate is the dashboard's in-memory projection of what every seat
// is doing right now.
//
// It consumes the engine event stream — the same feed the WebSocket fan-out
// reads — and maintains, per agent role: the seat's live state, its current
// task, phase and iteration, cumulative token totals, and the IN-FLIGHT LLM
// call. It solves two problems, and both are worth stating because they are why
// this exists at all rather than the dashboard querying the store.
//
// REFRESH SURVIVAL. agent_turn_progress events are stream-only — the event
// store drops them — so the durable record of a turn appears only once its
// phase completes. A dashboard that rebuilt agent history from the store on
// every reconnect would lose any call mid-flight the moment someone hit
// refresh. Holding the call here and shipping it in the snapshot every client
// gets on connect is what makes the live row survive that.
//
// NO PER-READ DATABASE SCAN. Re-deriving seat state from a thirty-day event
// scan on every /agents request, every snapshot and every WebSocket connect
// does not scale. Here it is maintained incrementally and read in O(1).
//
// ORDERING. The events arrive over a broker that guarantees order only within a
// topic, and different event types are different topics. Every state transition
// is therefore gated on the event timestamp: an older event can never clobber
// newer state. The in-flight call is gated on round progression within a turn.
//
// CONCURRENCY. The Python this replaces relied on a single-threaded event loop
// and took no lock, saying so explicitly. That reasoning does not survive the
// port: here the stream feeds this from its own goroutine while HTTP handlers
// and WebSocket sends read it, so every method takes the mutex. The lock is not
// a precaution, it is the thing that makes the projection safe to read at all.
package livestate

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("api.livestate")

const (
	// EventFeedLimit is how many persisted-category events the projection
	// retains for the activity feed — and the number a snapshot ships.
	//
	// ONE number: the ring, the hydration read and the snapshot all derive
	// from it. They used to be three (400 retained, 150 sent, 250 kept
	// client-side), so a tab streamed its feed up to 250 rows and then a
	// refresh visibly snapped it back to 150, while 250 of the server's
	// rows could never be delivered at all.
	EventFeedLimit = 400

	// dedupeLimit caps the id sets that stop a hydrated turn being counted
	// again when the same turn also arrives on the live stream. The window
	// they need to cover is the hydration overlap plus any redelivery —
	// minutes, not the process lifetime.
	dedupeLimit = 8000

	// LiveSpendWindow is what the in-memory rollup covers. Deliberately
	// shorter than the store's token window: "live" means what is
	// happening now, and the rollup is re-aggregated from its records each
	// time it is pushed, so its cost is set by how many records the window
	// holds. Any wider window the Tokens view offers is a store query.
	LiveSpendWindow = 24 * time.Hour

	// spendRecordLimit is a memory and latency backstop on retained
	// per-phase records. The real bound is the window above; this only
	// binds for an org emitting more than this in a day. Truncation drops
	// the OLDEST records, so an org past the cap sees a rollup covering
	// slightly less than a day rather than a wrong total.
	spendRecordLimit = 8000

	// sandboxEntryMaxAge is how long an in-flight sandbox entry survives
	// without a completion.
	//
	// Every other structure here is explicitly bounded and this one was
	// not, while its input stream is lossy in both directions — the
	// insert already accounts for a MISSED start, and a missed completion
	// has the mirror-image effect with nothing to compensate it. The cost
	// is not the memory: it is a ghost entry in the Running Sandboxes
	// panel, a false report of work in flight that no operator can clear
	// and no restart of the engine fixes.
	//
	// Twelve hours, against a detached run whose own ceiling is the turn's
	// token budget plus a buffer, and a clarification pause bounded by its
	// own TTL. Long enough that a genuinely long job is never swept from
	// under an operator watching it; short enough that a lost run does not
	// outlive the working day it started in. Eviction is a display
	// correction, never a control action.
	sandboxEntryMaxAge = 12 * time.Hour
)

// eventState maps an event type to the coarse seat state it implies.
var eventState = map[string]string{
	"agent_spawned":         "idle",
	"task_started":          "working",
	"task_completed":        "idle",
	"task_failed":           "idle",
	"agent_terminated":      "terminated",
	"agent_turn_progress":   "working",
	"agent_phase_started":   "working",
	"agent_phase_completed": "working",
	"reflection_completed":  "idle",
	"llm_unavailable":       "afk",
	"turn.guard_breach":     "afk",
	"budget_exhausted":      "afk",
}

// afkEvents are the engine-detected failures that flip a seat to afk and carry
// a cause the dashboard renders as a status quip.
var afkEvents = map[string]struct{}{
	"llm_unavailable":   {},
	"turn.guard_breach": {},
	"budget_exhausted":  {},
}

// sandboxEvents feed the running-sandboxes panel: started → tracked;
// clarification → awaiting input; completed → dropped.
var sandboxEvents = map[string]struct{}{
	"sandbox_run_started":             {},
	"sandbox_clarification_requested": {},
	"sandbox_run_completed":           {},
}

// agentLive is the incrementally-maintained state of one seat.
type agentLive struct {
	role      string
	runtimeID string
	state     string

	currentTask      string
	currentPhase     string
	currentIteration int

	inputTokens  int
	outputTokens int
	totalTokens  int

	afkReason string
	lastError *ErrorInfo
	liveCall  *LiveCall
	budget    *Meter

	// stateTS is the instant of the last state-affecting event applied —
	// the reorder guard. Internal bookkeeping, never re-emitted.
	stateTS stamp
}

func (a *agentLive) overlay() Overlay {
	return Overlay{
		State:            a.state,
		RuntimeID:        a.runtimeID,
		CurrentTask:      optional(a.currentTask),
		CurrentPhase:     optional(a.currentPhase),
		CurrentIteration: a.currentIteration,
		InputTokens:      a.inputTokens,
		OutputTokens:     a.outputTokens,
		TotalTokens:      a.totalTokens,
		LiveCall:         a.liveCall.clone(),
		LastError:        a.lastError.clone(),
		Budget:           a.budget.clone(),
		AFKReason:        a.afkReason,
	}
}

// optional renders "" as JSON null, which is what the dashboard reads as "no
// current task" rather than as an empty-named one.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// LiveState is a mirror of every seat's current state.
type LiveState struct {
	mu sync.Mutex

	agents map[string]*agentLive

	// sandboxes are in-flight detached jobs, keyed by kick-off turn id.
	sandboxes map[string]*SandboxEntry

	// feed is a chronological ring of persisted-category events.
	feed      []FeedRow
	feedLimit int

	// countedTurns and countedPhases are the id sets that stop a hydrated
	// turn being counted twice against a streamed one.
	countedTurns  *boundedSet[struct{}]
	countedPhases *boundedSet[struct{}]

	// finishedCalls maps a phase invocation to the instant its completion
	// landed.
	//
	// A phase publishes its last progress round and its completed event
	// back to back on DIFFERENT topics, and the API consumes those through
	// one wildcard subscription where cross-topic order is not guaranteed.
	// A progress round arriving after its own completion would find no
	// live call to match and seed a fresh one — an in-flight row for a
	// phase that finished, which nothing would ever clear.
	//
	// The timestamp is what makes this safe for a SUSPENDED Execute phase:
	// it publishes a completion checkpoint under these exact coordinates
	// and then, when the detached run lands, resumes the same loop and
	// streams more rounds under them. Those rounds are strictly newer than
	// the checkpoint, so only a round at or before it is dropped.
	finishedCalls *boundedSet[stamp]

	// spend holds per-phase RECORDS rather than a folded rollup, so the
	// aggregation has exactly one implementation instead of the three it
	// had — the endpoint's, a re-implementation in the browser, and
	// whatever a reconnect left behind.
	spend []SpendRecord

	budget OrgBudget

	// now is injectable so a test can pin the clock the sandbox sweep
	// reads. Nil takes the wall clock.
	now func() time.Time
}

// New builds an empty projection.
func New(opts ...Option) *LiveState {
	s := &LiveState{
		agents:        map[string]*agentLive{},
		sandboxes:     map[string]*SandboxEntry{},
		feedLimit:     EventFeedLimit,
		countedTurns:  newBoundedSet[struct{}](dedupeLimit),
		countedPhases: newBoundedSet[struct{}](dedupeLimit),
		finishedCalls: newBoundedSet[stamp](dedupeLimit),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Option configures a projection.
type Option func(*LiveState)

// WithFeedLimit overrides how many feed rows are retained.
func WithFeedLimit(n int) Option {
	return func(s *LiveState) {
		if n > 0 {
			s.feedLimit = n
		}
	}
}

// WithClock pins the clock the sandbox sweep reads.
func WithClock(now func() time.Time) Option {
	return func(s *LiveState) { s.now = now }
}

func (s *LiveState) clock() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now()
}

// --- read side ---------------------------------------------------------- //

// MergeAgents overlays live state onto each static config row.
//
// Roles with no live entry are returned as-is, which the dashboard renders
// offline. Order follows the input.
func (s *LiveState) MergeAgents(static []map[string]any) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]map[string]any, 0, len(static))
	for _, row := range static {
		merged := make(map[string]any, len(row)+12)
		for k, v := range row {
			merged[k] = v
		}
		role, _ := row["role"].(string)
		if live := s.agents[role]; live != nil {
			mergeOverlay(merged, live.overlay())
		}
		out = append(out, merged)
	}
	return out
}

// AgentOverlay returns the live overlay for one role, or nil.
func (s *LiveState) AgentOverlay(role string) *Overlay {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.agents[role]
	if live == nil {
		return nil
	}
	o := live.overlay()
	return &o
}

// RuntimeIDFor returns the running instance id for a role, or "".
func (s *LiveState) RuntimeIDFor(role string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if live := s.agents[role]; live != nil {
		return live.runtimeID
	}
	return ""
}

// RecentEvents returns feed rows newest-first, capped at limit.
func (s *LiveState) RecentEvents(limit int) []FeedRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.feed) {
		limit = len(s.feed)
	}
	out := make([]FeedRow, 0, limit)
	for i := len(s.feed) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.feed[i])
	}
	return out
}

// ActiveSandboxes returns in-flight detached jobs, oldest-first.
//
// Oldest-first so the longest-running job — the one most likely to need
// attention, such as one blocked on a clarification — sorts to the top of the
// panel.
//
// Entries past sandboxEntryMaxAge are dropped on the way out. The set is
// cleared by a completion event, and an event stream that can miss a start can
// miss a completion too. Swept on READ rather than on a timer because this is a
// display projection: the correction is only ever observed here, and a
// projection does not need a loop of its own to stop lying.
func (s *LiveState) ActiveSandboxes() []SandboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepStaleSandboxes()

	out := make([]SandboxEntry, 0, len(s.sandboxes))
	for _, entry := range s.sandboxes {
		out = append(out, *entry)
	}
	slices.SortFunc(out, func(a, b SandboxEntry) int {
		return strings.Compare(a.StartedAt, b.StartedAt)
	})
	return out
}

func (s *LiveState) sweepStaleSandboxes() {
	if len(s.sandboxes) == 0 {
		return
	}
	now := s.clock()
	var stale []string
	for turnID, entry := range s.sandboxes {
		if newStamp(entry.StartedAt).olderThan(now, sandboxEntryMaxAge) {
			stale = append(stale, turnID)
		}
	}
	for _, turnID := range stale {
		delete(s.sandboxes, turnID)
	}
	if len(stale) > 0 {
		log.Info("sandbox_projection_entries_expired",
			"count", len(stale),
			"max_age", sandboxEntryMaxAge,
			"hint", "no sandbox_run_completed arrived for these runs; the "+
				"dashboard was showing them as still in flight")
	}
}

// Budget returns the org-wide meter. Zero-valued when none is reporting.
func (s *LiveState) Budget() OrgBudget {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.budget
}

// --- write side --------------------------------------------------------- //

// Apply updates the projection from one serialized envelope, reporting what
// moved so the stream service can push the RESULT of applying it rather than
// the raw event.
func (s *LiveState) Apply(env Envelope) Change {
	s.mu.Lock()
	defer s.mu.Unlock()

	payload := env.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	var change Change

	// The live token meters. Stream-only for the same reason the in-flight
	// call is, and one stronger: these figures describe ONE engine run, so
	// a persisted copy replayed from history would show a dead process's
	// counters as the current ones.
	if env.Type == "budget_reported" {
		return s.applyBudget(payload)
	}

	// The in-flight call is stream-only: update it, but never let it into
	// the persisted-event buffer.
	if env.Type == "agent_turn_progress" {
		if role := s.applyProgress(env, payload); role != "" {
			change.agentMoved(role)
		}
		return change
	}

	// Everything else carrying a category is a persisted event — mirror it
	// into the activity buffer.
	if env.Category != "" {
		s.recordEvent(env, payload)
		change.Events = true
	}

	// Detached sandbox lifecycle, then stop: these do not drive the seat
	// state machine below.
	if _, ok := sandboxEvents[env.Type]; ok {
		s.applySandbox(env, payload)
		change.Sandboxes = true
		return change
	}

	if env.Type == "agent_phase_completed" {
		change.Tokens = s.foldSpend(env, payload)
	}

	role := str(payload, "role", "agent_role")
	if role == "" {
		return change
	}
	agent := s.ensureAgent(role)
	if id := str(payload, "agent_id"); id != "" {
		agent.runtimeID = id
	}

	if env.Type == "agent_turn_completed" {
		s.addTurnTokens(agent, env, payload)
		change.agentMoved(role)
	}
	if s.applyState(agent, env, payload) {
		change.agentMoved(role)
	}
	return change
}

// applyState applies a state-affecting event, gated on the reorder guard, and
// reports whether the seat moved.
func (s *LiveState) applyState(agent *agentLive, env Envelope, payload map[string]any) bool {
	if _, ok := eventState[env.Type]; !ok {
		return false
	}
	ts := newStamp(env.Timestamp)

	// Reorder guard: a strictly-older event must not clobber newer state.
	// EQUAL timestamps pass — same-instant bursts are ordinary, and the
	// later-applied wins, which matches the store's own id tiebreak
	// closely enough for a dashboard.
	if !ts.empty() && !agent.stateTS.empty() && ts.before(agent.stateTS) {
		return false
	}
	if !ts.empty() {
		agent.stateTS = ts
	}

	switch {
	case env.Type == "agent_spawned":
		// A spawn is a NEW instance of the seat, so whatever stopped the
		// last one is not this one's state. Without this the sticky-AFK
		// hold outlives an engine restart and a healthy seat renders as
		// broken until it happens to do some work.
		if agent.state == "offline" || agent.state == "terminated" || agent.state == "afk" {
			agent.state = "idle"
			agent.afkReason = ""
			agent.lastError = nil
			agent.liveCall = nil
		}

	case env.Type == "task_started":
		agent.state = "working"
		agent.currentTask = str(payload, "task_id")
		agent.afkReason = ""
		agent.lastError = nil

	case env.Type == "task_completed" || env.Type == "task_failed":
		agent.currentTask = ""
		agent.currentPhase = ""
		agent.currentIteration = 0
		// A task that failed says so. TaskFailed carries the error and
		// nothing else recorded it, so a task that died for a reason the
		// engine does not treat as AFK — an unhandled handler exception,
		// a rejected delegation — left the seat looking like a healthy
		// idle one, with the cause visible only as one line in the feed.
		if env.Type == "task_failed" {
			agent.lastError = &ErrorInfo{
				Kind:    "task_failed",
				Message: str(payload, "error"),
				TurnID:  str(payload, "turn_id"),
				At:      env.Timestamp,
				EventID: env.ID,
			}
		}
		// An engine-detected failure publishes its AFK event and
		// TaskFailed microseconds apart, in that order. Forcing idle here
		// would erase the cause the instant it was set — which is why an
		// agent whose provider died still showed as a healthy idle seat,
		// and why a reload showed the same. A seat leaves AFK only when
		// it does real work again.
		if agent.state != "afk" {
			agent.state = "idle"
			agent.afkReason = ""
			agent.liveCall = nil
		}

	case env.Type == "agent_phase_started":
		agent.state = "working"
		agent.afkReason = ""
		// A new phase is real forward progress: whatever killed the last
		// one is history now.
		agent.lastError = nil
		agent.currentPhase = str(payload, "phase")
		agent.currentIteration = num(payload, "iteration")
		agent.liveCall = beginCall(env, payload)

	case env.Type == "agent_phase_completed":
		agent.state = "working"
		agent.afkReason = ""
		if flag(payload, "failed") {
			s.recordPhaseFailure(agent, env, payload)
		}
		s.finishLiveCall(agent, env, payload)

	case env.Type == "reflection_completed":
		agent.state = "idle"
		agent.currentPhase = ""
		agent.currentIteration = 0
		agent.liveCall = nil

	case env.Type == "agent_terminated":
		agent.state = "terminated"
		agent.liveCall = nil

	default:
		if _, ok := afkEvents[env.Type]; !ok {
			return true
		}
		agent.state = "afk"
		if kind := str(payload, "kind"); kind != "" {
			agent.afkReason = kind
		} else {
			agent.afkReason = env.Type
		}
		// A call already frozen as failed is the most informative thing
		// on the seat's page — the prompt it died on, the tools that had
		// run, the error. The AFK event that follows a failed phase would
		// otherwise wipe it a moment later.
		if agent.liveCall == nil || !agent.liveCall.Failed {
			agent.liveCall = nil
		}
		kind := str(payload, "last_error_kind", "kind")
		if kind == "" {
			kind = env.Type
		}
		agent.lastError = &ErrorInfo{
			Kind:    kind,
			Message: str(payload, "last_error", "detail", "error"),
			Phase:   agent.currentPhase,
			TurnID:  str(payload, "turn_id"),
			At:      env.Timestamp,
			EventID: env.ID,
		}
	}
	return true
}

func (s *LiveState) ensureAgent(role string) *agentLive {
	agent := s.agents[role]
	if agent == nil {
		agent = &agentLive{role: role, state: "offline"}
		s.agents[role] = agent
	}
	return agent
}

func (s *LiveState) recordEvent(env Envelope, payload map[string]any) {
	row := FeedRow{
		ID: env.ID, Type: env.Type, Timestamp: env.Timestamp,
		Source: env.Source, Actor: env.Actor, Summary: env.Summary,
		Category: env.Category, TraceID: env.TraceID, SpanID: env.SpanID,
		ParentSpanID: env.ParentSpanID, Topic: env.Topic,
		Failed: types.Failed(env.Type, flag(payload, "failed"), false),
	}
	s.feed = append(s.feed, row)
	if len(s.feed) > s.feedLimit {
		// Re-sliced forward, which is enough for the same reason it is in
		// boundedSet: the remaining capacity shrinks with every drop, so
		// the next append past it reallocates and releases the evicted
		// rows with the old array.
		s.feed = s.feed[len(s.feed)-s.feedLimit:]
	}
}

func (s *LiveState) addTurnTokens(agent *agentLive, env Envelope, payload map[string]any) {
	if env.ID != "" {
		if s.countedTurns.has(env.ID) {
			return
		}
		s.countedTurns.put(env.ID, struct{}{})
	}
	agent.inputTokens += num(payload, "input_tokens")
	agent.outputTokens += num(payload, "output_tokens")
	agent.totalTokens += num(payload, "total_tokens")
}

// callKey is the identity of one phase invocation, shared by both its events.
func callKey(turnID, phase string, iteration int) string {
	return turnID + "|" + phase + "|" + strconv.Itoa(iteration)
}
