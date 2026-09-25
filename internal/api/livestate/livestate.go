// Package livestate is the dashboard's in-memory projection of what every seat
// is doing right now.
//
// It consumes the engine event stream — the same feed the WebSocket fan-out
// reads — and maintains, per agent role: the seat's live state, the TURN it is
// on and the stage of it (context, a phase, or parked on a coding run), the
// last turn it ended, its current phase and iteration, its live token meter,
// and the IN-FLIGHT LLM call. Beside the seats it keeps the coding runs in
// flight, reconciled against their durable record (sandbox.go).
// What a seat has SPENT is not held per seat: it is the per-agent row of the
// spend rollup, folded from the same records by internal/tokens, so a seat
// card and the Spend screen cannot disagree about one seat.
//
// It solves two problems, and both are worth stating because they are why this
// exists at all rather than the dashboard querying the store.
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
// CONCURRENCY. A projection like this is often written lock-free on the
// grounds that a single-threaded scheduler makes every mutation atomic. That
// reasoning does not hold here: the stream feeds this from its own goroutine
// while HTTP handlers and WebSocket sends read it, so every method takes the
// mutex. The lock is not
// a precaution, it is the thing that makes the projection safe to read at all.
package livestate

import (
	"strconv"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events/types"
)

const (
	// EventFeedLimit is how many persisted-category events the projection
	// retains for the activity feed — and the number a snapshot ships.
	//
	// ONE number: the ring, the startup seed's read and the snapshot all
	// derive from it. They used to be three (400 retained, 150 sent, 250 kept
	// client-side), so a tab streamed its feed up to 250 rows and then a
	// refresh visibly snapped it back to 150, while 250 of the server's
	// rows could never be delivered at all. The dashboard's own copy is
	// `MAX_EVENTS` in `contract/wire.ts`, held to this one by
	// TestTheDashboardKeepsTheFeedTheEngineKeeps.
	EventFeedLimit = 400

	// dedupeLimit caps the finished-call guard, whose keys are phase
	// invocations rather than rows of any structure here, so nothing else
	// bounds them. The window it needs to cover is the gap between a
	// phase's last progress round and its completion: seconds, not the
	// process lifetime.
	dedupeLimit = 8000

	// LiveSpendWindow is what the in-memory rollup covers. Deliberately
	// shorter than the store's token window: "live" means what is
	// happening now, and the rollup is re-aggregated from its records each
	// time it is pushed, so its cost is set by how many records the window
	// holds. Any wider window the Tokens view offers is a store query.
	LiveSpendWindow = 24 * time.Hour

	// SpendRecordLimit is a memory and latency backstop on retained
	// per-phase records. The real bound is the window above; this only
	// binds for an org emitting more than this in a day. Truncation drops
	// the OLDEST records, so an org past the cap sees a rollup covering
	// slightly less than a day rather than a wrong total.
	//
	// Exported because the startup seed reads no more than this from the
	// store: a record past the cap would be dropped on arrival, so reading
	// it costs the seed's time budget and buys nothing.
	SpendRecordLimit = 8000
)

// eventState maps an event type to the coarse seat state it implies.
//
// agent_turn_progress is deliberately ABSENT even though it plainly implies
// "working": Apply handles that type on its own branch and returns before
// applyState is ever reached, so an entry here would be read by nothing.
// applyProgress sets the state itself, after its discard guards.
var eventState = map[string]string{
	"agent_spawned":         "idle",
	"agent_terminated":      "terminated",
	"agent_phase_started":   "working",
	"agent_phase_completed": "working",
	// THE END OF THE WORK, and reflection_completed is only the end of what
	// FOLLOWS it. Reflection is the trailing sentinel for the auxiliary phases
	// a learning pass emits, and it was the only entry here that anything
	// publishes AND that returns a seat to idle — but the reflector returns
	// without publishing it on five paths (no workers configured, an unknown
	// role, a per-role `learning_enabled: false`, a spent token budget, a
	// redelivery it has already marked). A company running with learning off
	// takes the first of those on every turn, so every one of its seats went
	// to `working` on its first turn and stayed there for the life of the
	// process — mid-phase, in a phase that had ended.
	"agent_turn_completed": "idle",
	"reflection_completed": "idle",
	"llm_unavailable":      "afk",
	"turn.guard_breach":    "afk",
	"budget_exhausted":     "afk",
}

// afkEvents are the engine-detected failures that flip a seat to afk and carry
// a cause the dashboard renders as a status quip.
var afkEvents = map[string]struct{}{
	"llm_unavailable":   {},
	"turn.guard_breach": {},
	"budget_exhausted":  {},
}

// sandboxEvents feed the running-sandboxes panel: started → tracked;
// clarification → awaiting a person; completed or failed → dropped.
var sandboxEvents = map[string]struct{}{
	"sandbox_run_started":             {},
	"sandbox_clarification_requested": {},
	"sandbox_run_completed":           {},
	"sandbox_run_failed":              {},
}

// agentLive is the incrementally-maintained state of one seat.
type agentLive struct {
	role      string
	runtimeID string
	state     string

	currentPhase     string
	currentIteration int

	afkReason string
	lastError *ErrorInfo
	liveCall  *LiveCall
	budget    *BudgetMeter

	// stateTS is the instant of the last state-affecting event applied —
	// the reorder guard. Internal bookkeeping, never re-emitted.
	stateTS stamp

	// turn is the turn the seat is on, and turnAt the instant of the
	// event that last moved it — the turn's own reorder guard, apart from
	// stateTS because a turn's events are ordered against each other and
	// not against a meter report or an AFK hold. See turn.go.
	turn   *LiveTurn
	turnAt stamp

	// lastTurn is the newest turn the seat ended, and lastTurnAt when.
	lastTurn   *LastTurn
	lastTurnAt stamp

	// paused is the seat's pause, nil when it has none, and pausedAt the
	// instant of the event that last moved it — its own reorder guard. See
	// pause.go.
	paused   *Paused
	pausedAt stamp
}

func (a *agentLive) overlay() Overlay {
	return Overlay{
		State:            a.state,
		RuntimeID:        a.runtimeID,
		CurrentPhase:     optional(a.currentPhase),
		CurrentIteration: a.currentIteration,
		LiveCall:         a.liveCall.clone(),
		LastError:        a.lastError.clone(),
		Budget:           a.budget.clone(),
		AFKReason:        a.afkReason,
		Turn:             a.turn.clone(),
		LastTurn:         a.lastTurn.clone(),
		Paused:           a.paused.clone(),
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
	sandboxes map[string]*heldSandbox

	// endedRuns remembers how each run the events ended did, so a
	// reconcile that read the durable record before it caught up does not
	// put the run back. Pruned by every reconcile to the runs the record
	// still holds; bounded besides, for a process that never reconciles.
	endedRuns *boundedSet[runEnd]

	// endedTurns maps a turn that ENDED to the instant it did, so a
	// straggler from it — a phase or a progress round that lost a
	// cross-topic race to the completion — cannot put it back on its seat.
	// Bounded like finishedCalls, and for its reason: the window it covers
	// is seconds.
	endedTurns *boundedSet[stamp]

	// feed is a chronological ring of persisted-category events.
	feed      []FeedRow
	feedLimit int

	// feedIDs indexes the ids the ring holds, so an event is listed ONCE
	// however it arrived: off the stream, out of the store at startup, or
	// both, in either order. Exact rather than bounded like the sets below,
	// because it tracks the ring and shrinks with it: see trimFeed.
	feedIDs map[string]struct{}

	// spendIDs indexes the ids the live window holds, so a phase is counted
	// ONCE however it arrived: off the stream, out of the store at startup,
	// or both, in either order.
	//
	// EXACT rather than bounded like finishedCalls below, because it tracks
	// s.spend and shrinks with it — the same shape feedIDs has, and for the
	// same reason. A bounded set could not do it: its cap was the number of
	// records the seed reads, and the ids the live stream had already put
	// there sat at the FRONT of its eviction order, so a full seed evicted
	// them before its own loop reached the store's copies of those very
	// phases and counted each of them twice. See pruneSpend, which is where
	// this shrinks.
	spendIDs map[string]struct{}

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
	spend []spendEntry

	budget OrgBudget
	// budgetAt is when the held report was read, the guard against a
	// delayed report from another node. Internal, never re-emitted.
	budgetAt stamp

	// seededFrom is which nodes the startup seed read, nil until a seed
	// ran. See [LiveState.SeededFrom].
	seededFrom *eventfan.Coverage

	// now is injectable so a test can pin the clock the spend window and
	// the sandbox reconcile read. Nil takes the wall clock.
	now func() time.Time
}

// New builds an empty projection.
func New(opts ...Option) *LiveState {
	s := &LiveState{
		agents:        map[string]*agentLive{},
		sandboxes:     map[string]*heldSandbox{},
		endedRuns:     newBoundedSet[runEnd](dedupeLimit),
		endedTurns:    newBoundedSet[stamp](dedupeLimit),
		feedLimit:     EventFeedLimit,
		feedIDs:       map[string]struct{}{},
		spendIDs:      map[string]struct{}{},
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

// WithClock pins the clock the spend window and the sandbox reconcile read.
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

// OverlayRows renders the live overlays for the named roles as the wire rows
// the `agents` push carries: one object per seat, with its role INSIDE it.
//
// A LIST, and the role in the row, because that is what the client reads —
// store.js does `rows.map(r => [r.role, r])` behind an `Array.isArray` guard,
// so a map keyed by role is not merely a different spelling of the same thing:
// it fails the guard and the push is DISCARDED, silently, every time. Measured
// end to end (internal/e2e): a full turn ran, four agents pushes went out per
// phase, and the seat stayed idle on the dashboard from start to finish.
//
// The client is the compatibility reference and wins any disagreement about a
// frame's shape. This is that rule applied.
//
// A role with no live state is SKIPPED rather than sent as an empty overlay:
// the client merges these onto its existing rows, so a blank one would erase
// the state of a seat that simply had not changed.
func (s *LiveState) OverlayRows(roles []string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]map[string]any, 0, len(roles))
	for _, role := range roles {
		live := s.agents[role]
		if live == nil {
			continue
		}
		row := map[string]any{"role": role}
		mergeOverlay(row, live.overlay())
		out = append(out, row)
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
func (s *LiveState) Apply(env *Envelope) Change {
	s.mu.Lock()
	defer s.mu.Unlock()

	payload := env.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	var change Change

	// STAMPED HERE, so the frame the client is handed carries it and the
	// snapshot's own rows cannot disagree. It is the same derivation
	// recordEvent applies below, from the same function — the two used to be
	// one call and one omission, and the omission was the live half: a failed
	// turn arriving while somebody watched rendered exactly like a successful
	// one, then grew its failure mark on the next reload.
	//
	// A pointer receiver for the envelope, and that is the whole reason this
	// takes one: a value copy would be stamped and thrown away.
	env.Failed = types.Failed(env.Type, flag(payload, "failed"), false)

	// The live token meters. Stream-only: a report is a snapshot of a counter
	// that moves every round, so a copy replayed from history would show
	// figures the counter left behind long ago as the current ones.
	//
	// ONLY `budget_meters`. The older build's `budget_reported` carried one
	// figure per scope from the lifetime counters and is ignored rather than
	// translated: during the rollout that retired it those counters are not
	// the ones this build's gate charges, so folding it would draw a reading
	// of a counter that is being retired over the windows.
	if env.Type == "budget_meters" {
		return s.applyBudget(*env, payload)
	}

	// The in-flight call is stream-only: update it, but never let it into
	// the persisted-event buffer.
	if env.Type == "agent_turn_progress" {
		if role := s.applyProgress(*env, payload); role != "" {
			change.agentMoved(role)
		}
		return change
	}

	// Everything else carrying a category is a persisted event, mirrored into
	// the activity buffer once by id.
	//
	// The push is owed whether or not the row was new. A row the startup seed
	// already listed came out of the store without its payload, and the
	// envelope is the only frame that carries it: a client keeps a completed
	// phase's payload beside its feed, and dedupes the feed row itself by id.
	if env.Category != "" {
		s.recordEvent(env)
		change.Events = true
	}

	// Detached sandbox lifecycle, then stop: these do not drive the seat
	// state machine below. A LOST run is the one exception that reaches a
	// seat: it settles the parked turn that was waiting for it.
	if _, ok := sandboxEvents[env.Type]; ok {
		s.applySandbox(*env, payload)
		change.Sandboxes = true
		if env.Type == "sandbox_run_failed" && s.endLostRun(*env, payload) {
			change.agentMoved(str(payload, "role", "agent_role"))
		}
		return change
	}

	if env.Type == "agent_phase_completed" {
		change.Tokens = s.foldSpend(*env, payload)
	}

	role := str(payload, "role", "agent_role")
	if role == "" {
		return change
	}
	agent := s.ensureAgent(role)
	if id := str(payload, "agent_id"); id != "" {
		agent.runtimeID = id
	}

	// A PERSON'S PAUSE, on its own guard: it moves nothing else about the
	// seat, and nothing else moves it.
	if s.applyPause(agent, *env, payload) {
		change.agentMoved(role)
		return change
	}

	// THE TURN FIRST, and on its own guards: an event the state machine
	// below discards as older than the seat's newest (a completion that
	// lost a race to the next turn's first phase) still ended its turn.
	if s.applyTurnEvent(agent, *env, payload) {
		change.agentMoved(role)
	}
	if s.applyState(agent, *env, payload) {
		change.agentMoved(role)
	}
	return change
}

// applyTurnEvent moves the seat's turn for one event, reporting whether it did.
func (s *LiveState) applyTurnEvent(agent *agentLive, env Envelope, payload map[string]any) bool {
	if env.Type == "agent_turn_started" {
		return s.applyTurnStarted(agent, env, payload)
	}
	before := agent.overlay()
	turnID := str(payload, "turn_id")
	switch env.Type {
	case "agent_phase_started", "agent_phase_completed":
		s.touchTurn(agent, env, payload, StagePhase)
	case "agent_turn_completed":
		if flag(payload, "suspended") {
			// A SUSPENSION IS NOT AN END. The segment parked on a
			// detached coding run and the same turn completes again
			// when the run is collected.
			s.touchTurn(agent, env, payload, StageParked)
		} else {
			s.endTurnRecord(agent, env, turnID, env.Failed)
		}
	case "reflection_completed":
		s.extendLastTurn(agent, env, turnID)
	case "agent_spawned", "agent_terminated":
		// A NEW INSTANCE, or none: a turn the old one was running died
		// with it. A PARKED turn outlives both — it is a record in the
		// coordination store and a box, not a goroutine — and a spawn
		// older than the turn's newest event is the spawn that turn ran
		// under.
		at := newStamp(env.Timestamp)
		if agent.turn != nil && agent.turn.Stage != StageParked &&
			(env.Type == "agent_terminated" || agent.turnAt.empty() || at.empty() ||
				!at.before(agent.turnAt)) {
			agent.turn = nil
		}
	default:
		if env.Failed && agent.turn != nil && turnID != "" && agent.turn.TurnID == turnID {
			agent.turn.failed = true
		}
	}
	return !sameTurnOverlay(before, agent.overlay())
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
		// broken until it happens to do some work. A seat the projection
		// knew nothing about is idle from here too.
		if agent.state == "" || agent.state == "terminated" || agent.state == "afk" {
			agent.state = "idle"
			agent.afkReason = ""
			agent.lastError = nil
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
		// Only if a round of this same call has not already arrived.
		// phase_started and agent_turn_progress travel on DIFFERENT
		// subjects, so the opening round can land first; this assignment
		// used to be unconditional and replaced a call that already had a
		// model, a response and tool calls with an empty placeholder. The
		// reorder guard cannot catch it — applyProgress never advances
		// stateTS — so the check belongs here.
		if !agent.liveCall.sameCall(str(payload, "turn_id"),
			str(payload, "phase"), num(payload, "iteration")) {
			agent.liveCall = beginCall(env, payload)
		}

	case env.Type == "agent_phase_completed":
		agent.state = "working"
		agent.afkReason = ""
		if flag(payload, "failed") {
			s.recordPhaseFailure(agent, env, payload)
		}
		s.finishLiveCall(agent, env, payload)

	case env.Type == "agent_turn_completed" || env.Type == "reflection_completed":
		endTurn(agent, str(payload, "turn_id"))

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

// endTurn returns a seat to idle at the end of the turn `turnID`.
//
// SCOPED TO THAT TURN, and it has to be: both events that end one arrive
// asynchronously and neither is ordered against the next turn's work.
// reflection_completed is published by a separate consumer seconds after the
// turn returned, and the projection reads every type through one wildcard
// subscription where cross-topic order is not guaranteed — so by the time
// either lands the seat may already be several rounds into the NEXT turn. The
// clear used to be unconditional, which wiped the live row a reader was
// watching and reported a working seat as idle, until the following round
// happened to rebuild both.
//
// The timestamp reorder guard does not cover this on its own: applyProgress
// deliberately never advances stateTS, so a seat whose only events since the
// last phase boundary are progress rounds still carries the older stamp and
// lets a late completion through.
func endTurn(agent *agentLive, turnID string) {
	// A live call for ANOTHER turn is the seat having moved on. Neither the
	// row nor the state belongs to the turn ending here.
	if agent.liveCall != nil && turnID != "" && agent.liveCall.TurnID != turnID {
		return
	}
	agent.currentPhase = ""
	agent.currentIteration = 0
	// AN AFK SEAT STAYS AFK, and this is the one path that reaches it. An
	// engine-detected failure publishes its AFK event and then the turn's
	// own completion, microseconds apart and in that order — so forcing
	// idle here erases the cause the instant it was set, and an agent
	// whose provider died renders as a healthy idle seat on the screen and
	// on every reload. The hold used to be written on the task_completed
	// branch, which nothing ever published: the guard was real and
	// unreachable, and the reachable path had none.
	//
	// A seat leaves AFK only when it does real work again, which is
	// agent_phase_started's business.
	if agent.state == "afk" {
		return
	}
	agent.state = "idle"
	agent.liveCall = nil
}

// ensureAgent returns the live entry for a role, creating one that claims NO
// state.
//
// UNKNOWN, not offline, is what a new entry knows. Several things create one
// without saying anything about whether the seat is running: a meter report
// names every capped seat, and a spend record names the seat it billed. The
// overlay used to start at "offline", and a merged overlay OVERWRITES the
// roster's own state, so the first meter report after a boot turned every
// capped seat this node was serving from idle to offline on every open
// dashboard, and it stayed that way until the seat next took a turn.
func (s *LiveState) ensureAgent(role string) *agentLive {
	agent := s.agents[role]
	if agent == nil {
		agent = &agentLive{role: role}
		s.agents[role] = agent
	}
	return agent
}

// recordEvent lists one persisted event in the feed, unless the feed already
// lists it.
func (s *LiveState) recordEvent(env *Envelope) {
	if !s.admitFeedID(env.ID) {
		return
	}
	row := FeedRow{
		ID: env.ID, Type: env.Type, Timestamp: env.Timestamp,
		Source: env.Source, Actor: env.Actor, Summary: env.Summary,
		Category: env.Category, TraceID: env.TraceID, SpanID: env.SpanID,
		ParentSpanID: env.ParentSpanID, Topic: env.Topic,
		// Read off the envelope Apply just stamped, rather than derived a
		// second time: one derivation is what keeps the live row and the
		// seeded one agreeing about the same event.
		Failed: env.Failed,
	}
	s.feed = append(s.feed, row)
	s.trimFeed()
}

// admitFeedID records that the feed lists an id, reporting false when it
// already did.
//
// An EMPTY id is always admitted. Every stored row has one and so does every
// envelope the engine builds, so a blank is a producer's bug; it is still
// listed, rather than collapsed with some unrelated row that also lacked one.
func (s *LiveState) admitFeedID(id string) bool {
	if id == "" {
		return true
	}
	if _, listed := s.feedIDs[id]; listed {
		return false
	}
	s.feedIDs[id] = struct{}{}
	return true
}

// trimFeed drops the oldest rows past the ring's limit, and their ids with
// them.
//
// The ids go too because the index is the ring's own, not a history of every
// id ever seen: kept, it would grow for the life of a process the ring exists
// to keep bounded.
func (s *LiveState) trimFeed() {
	over := len(s.feed) - s.feedLimit
	if over <= 0 {
		return
	}
	for _, row := range s.feed[:over] {
		delete(s.feedIDs, row.ID)
	}
	// Re-sliced forward, which is enough for the same reason it is in
	// boundedSet: the remaining capacity shrinks with every drop, so the
	// next append past it reallocates and releases the evicted rows with
	// the old array.
	s.feed = s.feed[over:]
}

// callKey is the identity of one phase invocation, shared by both its events.
func callKey(turnID, phase string, iteration int) string {
	return turnID + "|" + phase + "|" + strconv.Itoa(iteration)
}
