package learning

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// ReflectGroup is the dispatcher's consumer group.
//
// Its own group, shared with nothing: reflection is the one subsystem an
// operator turns off on its own, and a group shared with another consumer
// would take that consumer's traffic down with it.
const ReflectGroup = "reflect-engine"

// ReflectTool is the in-flight builtin an agent calls to write its own memory.
//
// The dispatcher reads it as "the LLM already handled persistence this turn".
// It is a Plan-phase-only builtin, so a call lands in
// [types.TurnCompleted.PlanToolSequence] and never in the Execute-scoped
// ToolSequence — [Turn.SelfPersisted] reads both anyway, so moving the tool to
// another phase cannot quietly turn the check into a no-op that
// double-persists every turn.
const ReflectTool = "reflect_and_persist"

// reflectSeen bounds the dispatcher's memory of turns it has already handled.
//
// 1024 turns is well over an hour of traffic on a busy company, which is far
// longer than any backend's redelivery window: past that, a redelivery of a
// turn this process already reflected on is not a case worth spending memory
// against. The guard is per PROCESS and deliberately not durable — a second
// node reflecting the same turn writes a second diary row, which is the
// bounded duplication the engine promises rather than exactly-once (REWRITE
// PLAN §16).
const reflectSeen = 1024

// turnCompletedTopic is the subject completed turns arrive on.
var turnCompletedTopic = topics.Event(types.TurnCompleted{}.EventType())

// Turn is one completed turn, with the seat's role already resolved.
//
// The role is resolved ONCE by the dispatcher and passed down rather than
// looked up per worker: every worker needs it (for its model chain, if
// nothing else) and a worker that resolved its own would be reading the org
// at a different instant from the gate that decided the turn was worth
// reflecting on.
type Turn struct {
	Role  *org.Role
	Event types.TurnCompleted

	// Trace is the completed turn's trace context, carried so a worker
	// publishing its own events links them to the turn that caused them.
	// It lives on the envelope, never on the payload, so it has to be
	// handed down explicitly — an ambient one is what the Go rewrite
	// deleted (rewrite/decisions/000).
	Trace events.TraceContext
}

// Settled reports whether the turn reached a terminal outcome.
//
// self_iterate is not one: it is a mid-turn state the engine will REATTEMPT,
// so a fact persisted from it — or a skill drafted from it — is learned from
// work the agent itself judged incomplete, and the next round may contradict
// it. done and failed both mean the turn ran its course; failed is as much a
// lesson as done, which is why the settled set is not just "succeeded".
func (t Turn) Settled() bool {
	return t.Event.ReviewOutcome == "done" || t.Event.ReviewOutcome == "failed"
}

// SelfPersisted reports whether the turn already wrote its own memory.
func (t Turn) SelfPersisted() bool {
	return slices.Contains(t.Event.PlanToolSequence, ReflectTool) ||
		slices.Contains(t.Event.ToolSequence, ReflectTool)
}

// Engaged reports whether the agent actually acted on the trigger.
//
// Two routes to "it did not", and both produce phantom-directive learning if
// they are not blocked:
//
//   - the planner explicitly opted out (plan_decision skip) — it recognised
//     the trigger was for somebody else;
//   - the planner INTENDED to opt out but never called submit_plan, so the
//     engine coerced the decision to direct and Execute ran with the full
//     registry and called nothing.
//
// Either way the agent processed nothing externally observable, and a fact
// read off the trigger body would teach it a directive it never received:
// "User Sam prefers replies opened with 'hey sam'" landing in agent-pm's
// memory because Sam was @-mentioning agent-ceo in a shared channel and
// agent-pm merely observed the trigger.
//
// The empty-tool-sequence half is qualified by the outcome on purpose. A
// FAILED turn that called nothing failed at something, and that is worth
// reflecting on; only a turn that finished done having called nothing claims
// to have engaged with a trigger it did not touch.
func (t Turn) Engaged() bool {
	if t.Event.PlanDecision == types.PlanDecisionSkip {
		return false
	}
	return len(t.Event.ToolSequence) > 0 || t.Event.ReviewOutcome != "done"
}

// Worker is one post-turn learning pass.
//
// Three methods because the dispatcher asks three different questions, and
// the middle one is what keeps each worker's applicability with the worker:
// the persist decider must not run on an unsettled turn, the counterparty
// profiler must (observing who you talked to does not depend on what you
// decided to do next), and a dispatcher holding that table would have to be
// edited every time a worker is added.
type Worker interface {
	// Name identifies the worker in logs and in a pass result.
	Name() string

	// Skip returns why this worker has nothing to do for the turn, as a
	// short snake_case reason, or "" when it should run. A reason rather
	// than a bool because "did not run" is not a diagnosis: an operator
	// looking at a company that never learns anything needs to know
	// WHICH gate is closing.
	Skip(t Turn) string

	// Reflect runs one pass and returns the lifecycle event to publish,
	// nil for none.
	//
	// It may return BOTH a payload and an error: the payload is what the
	// worker managed to conclude, the error is what went wrong reaching
	// it. A classifier whose LLM call failed still concluded "nothing was
	// persisted", and dropping that event because the call failed would
	// hide the failure from every surface that counts outcomes.
	Reflect(ctx context.Context, t Turn) (events.Payload, error)
}

// Subscriber is the half of the queue the dispatcher attaches through.
type Subscriber interface {
	Subscribe(ctx context.Context, topic, group string, h queue.Handler) error
}

// Reflector dispatches learning workers over completed turns.
type Reflector struct {
	org     *org.Organization
	pub     queue.Publisher
	workers []Worker

	mu   sync.Mutex
	seen *recentTurns
}

// NewReflector builds a dispatcher over an org and a publisher.
//
// An EMPTY worker list is allowed: a company may wire the dispatcher before
// wiring any worker, and every pass then short-circuits on the no-workers
// fast path. A NIL worker in the list is not — it is a caller that appended
// an optional worker without checking it, and the alternative to refusing it
// here is a nil dereference on the first completed turn, which is a stack
// trace naming this package for a mistake made in the engine's wiring.
func NewReflector(o *org.Organization, pub queue.Publisher, workers []Worker) (*Reflector, error) {
	if o == nil {
		return nil, fmt.Errorf("learning: reflection needs an organization to resolve seats against")
	}
	if pub == nil {
		return nil, fmt.Errorf("learning: reflection needs a publisher for its lifecycle events")
	}
	names := make(map[string]bool, len(workers))
	for i, w := range workers {
		if w == nil {
			return nil, fmt.Errorf("learning: reflection worker %d is nil", i)
		}
		// A pass reports its skips and its swallowed failures BY NAME, so
		// two workers sharing one erase each other in both maps and an
		// operator reads one worker's failure as the other's.
		if names[w.Name()] {
			return nil, fmt.Errorf("learning: two reflection workers named %q", w.Name())
		}
		names[w.Name()] = true
	}
	return &Reflector{
		org:     o,
		pub:     pub,
		workers: slices.Clone(workers),
		seen:    newRecentTurns(reflectSeen),
	}, nil
}

// Start attaches the dispatcher to the completed-turn subject.
func (r *Reflector) Start(ctx context.Context, sub Subscriber) error {
	if err := sub.Subscribe(ctx, turnCompletedTopic, ReflectGroup, r.Handle); err != nil {
		return fmt.Errorf("learning: subscribe %s: %w", turnCompletedTopic, err)
	}
	log.Info("reflect_engine_started", "workers", r.names())
	return nil
}

func (r *Reflector) names() []string {
	out := make([]string, 0, len(r.workers))
	for _, w := range r.workers {
		out = append(out, w.Name())
	}
	return out
}

// Handle is the queue handler for one completed turn.
//
// It ALWAYS acks, whatever happened. Reflection is work about a turn that is
// already over: a nak would redeliver the turn to spend another round of
// auxiliary-LLM tokens reaching the same conclusion, and a turn whose
// reflection reliably fails would consume its redelivery budget and land in
// the dead-letter queue, where a poison message about a turn nobody is
// waiting on is pure noise. The failure belongs in the log, not in the
// broker.
func (r *Reflector) Handle(ctx context.Context, ev *events.Event) queue.Result {
	// The outer recover is a BACKSTOP, not control flow: each worker
	// already runs under its own (see dispatch), so what reaches here is a
	// panic from the dispatcher's own code or from the publisher — a
	// backend that panics on a closed connection, say. Letting it out
	// would take down the queue's consumer goroutine, which is the one
	// failure mode "reflection must never break the engine" exists to
	// prevent, and it would take the seat's inbox consumer with it on any
	// backend that shares a goroutine across subscriptions.
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("reflection_panicked", "error", rec, "stack", string(debug.Stack()))
		}
	}()
	tc, ok := events.DataAs[*types.TurnCompleted](ev)
	if !ok {
		// The subject carries one type, so this is a build that does not
		// know it (a rolling upgrade) or a mis-routed publish. Acked
		// either way: redelivering it produces the same non-answer.
		log.Debug("reflection_skipped_unreadable_event", "type", eventTypeOf(ev))
		return queue.Ack()
	}
	r.Reflect(ctx, *tc, events.TraceContext{
		TraceID: ev.TraceID, SpanID: ev.SpanID, ParentSpanID: ev.ParentSpanID,
	})
	return queue.Ack()
}

// Reflection is what one pass did, returned rather than only logged so a
// caller — and the suite — can assert on the reason a pass did nothing
// instead of matching log lines.
type Reflection struct {
	// Skip names why the whole pass short-circuited; "" when it ran.
	Skip string

	// Ran names the workers that produced an outcome, in dispatch order.
	Ran []string

	// Skipped maps a worker to why it had nothing to do this turn.
	Skipped map[string]string

	// Failed maps a worker to the failure that was swallowed on its
	// behalf. Present so "the pass ran and everything failed" is
	// distinguishable from "the pass ran"; a caller that only counted Ran
	// would read them as the same thing.
	Failed map[string]error
}

// Skip reasons. Exported because they are what an operator reads back out of
// a log line or a test asserts on, and a caller comparing against its own
// copy of the string is how the two drift.
const (
	SkipNoWorkers    = "no_workers"
	SkipNoRole       = "no_role"
	SkipRoleDisabled = "role_disabled"
	SkipDuplicate    = "duplicate"
	SkipNoEngagement = "no_engagement"
)

// Reflect runs one pass over a completed turn.
//
// Exported so an embedder can drive reflection without a queue — and so the
// gates below are reachable in a test without staging a delivery.
func (r *Reflector) Reflect(ctx context.Context, tc types.TurnCompleted, tr events.TraceContext) Reflection {
	// Fast path before any other work: with nothing wired there is no
	// question to ask about this turn, and asking it means an org lookup
	// and a dedup insert per completed turn for nothing.
	if len(r.workers) == 0 {
		return Reflection{Skip: SkipNoWorkers}
	}

	role := r.org.Role(tc.RoleName)
	if role == nil {
		// A turn from a seat this epoch no longer has. Learning about a
		// role that has been renamed or removed would write memory under
		// an identity nothing can read back.
		log.Debug("reflection_skipped_no_role", "turn_id", tc.TurnID, "role", tc.RoleName)
		return Reflection{Skip: SkipNoRole}
	}

	// Per-role opt-out. Unset inherits the company-wide setting, which is
	// the wiring's decision — a company with learning off does not build a
	// Reflector at all — so only an explicit false is a skip here.
	if !role.LearningEnabled.Or(true) {
		log.Debug("reflection_skipped_role_disabled", "turn_id", tc.TurnID, "role", role.Name)
		return Reflection{Skip: SkipRoleDisabled}
	}

	// Redelivery guard. Every backend may redeliver, and reflection is not
	// idempotent: each pass is a fresh auxiliary-LLM call that can write a
	// second, differently-worded row for the same fact.
	//
	// Marked BEFORE the remaining gates and never released. Python
	// released the mark on its transient early returns (budget exhausted,
	// concurrency slot unavailable) so a later redelivery would get a real
	// pass; neither gate is ported here, so there is no path left that
	// wants a retry — and a mark released on a path that cannot happen is
	// a mark that does nothing.
	if !r.mark(tc.TurnID) {
		log.Debug("reflection_skipped_duplicate", "turn_id", tc.TurnID)
		return Reflection{Skip: SkipDuplicate}
	}

	turn := Turn{Role: role, Event: tc, Trace: tr}
	if !turn.Engaged() {
		log.Info("reflection_skipped_no_engagement", "turn_id", tc.TurnID,
			"agent_handle", tc.AgentHandle, "plan_decision", string(tc.PlanDecision),
			"tool_count", len(tc.ToolSequence), "review_outcome", tc.ReviewOutcome)
		// The sentinel still fires. A turn the dispatcher DECIDED not to
		// learn from and a turn reflection never reached look identical
		// on every surface otherwise, and the second is a bug while the
		// first is the gate working.
		r.publish(ctx, turn, types.ReflectionCompleted{
			Agent: tc.Agent, AgentHandle: tc.AgentHandle, RoleName: tc.RoleName,
			TurnID: tc.TurnID, WorkersRun: 0, ReviewOutcome: tc.ReviewOutcome,
		})
		return Reflection{Skip: SkipNoEngagement}
	}

	out := Reflection{}
	for _, w := range r.workers {
		if reason := w.Skip(turn); reason != "" {
			if out.Skipped == nil {
				out.Skipped = map[string]string{}
			}
			out.Skipped[w.Name()] = reason
			log.Debug("reflection_worker_skipped", "turn_id", tc.TurnID,
				"worker", w.Name(), "reason", reason)
			continue
		}
		payload, err := r.dispatch(ctx, w, turn)
		if err != nil {
			if out.Failed == nil {
				out.Failed = map[string]error{}
			}
			out.Failed[w.Name()] = err
			log.Error("reflection_worker_failed", "turn_id", tc.TurnID,
				"worker", w.Name(), "error", err)
		}
		// Counted as RUN whether or not it failed, and whether or not it
		// had anything to publish: it spent the turn's budget and the
		// operator's wall clock. Python counted a worker that its own
		// gate had just skipped, which made a pass that did nothing at
		// all report workers_run=1.
		out.Ran = append(out.Ran, w.Name())
		if payload != nil {
			r.publish(ctx, turn, payload)
		}
	}

	// The trailing sentinel. The auxiliary phase events workers emit keep
	// the seat rendering as WORKING for as long as they are the newest
	// event for that role; this is what flips it back to idle when the
	// pass is over.
	r.publish(ctx, turn, types.ReflectionCompleted{
		Agent: tc.Agent, AgentHandle: tc.AgentHandle, RoleName: tc.RoleName,
		TurnID: tc.TurnID, WorkersRun: len(out.Ran), ReviewOutcome: tc.ReviewOutcome,
	})
	return out
}

// dispatch runs one worker, converting a panic into an error.
//
// Per worker rather than once around the loop, so one worker taking a nil map
// or an out-of-range index does not cost the pass its remaining workers or
// its sentinel — the seat would then render as working for ever, which is
// the visible half of a bug whose invisible half is simply no learning.
func (r *Reflector) dispatch(ctx context.Context, w Worker, t Turn) (payload events.Payload, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			payload = nil
			err = fmt.Errorf("learning: worker %s panicked: %v", w.Name(), rec)
			log.Error("reflection_worker_panicked", "worker", w.Name(),
				"turn_id", t.Event.TurnID, "error", rec, "stack", string(debug.Stack()))
		}
	}()
	return w.Reflect(ctx, t)
}

// publish emits one lifecycle event, best effort.
//
// The trace context is copied VERBATIM from the turn that caused it, so a
// trace-grouped view nests reflection under the turn's own card rather than
// showing it as free-floating background work with no cause.
func (r *Reflector) publish(ctx context.Context, t Turn, payload events.Payload) {
	ev := events.NewFrom(payload, t.Trace)
	if ev == nil {
		return
	}
	ev.Source = t.Event.RoleName
	if err := r.pub.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		// Swallowed on purpose: the worker's WRITE already landed. Losing
		// the announcement of it costs a dashboard row, where propagating
		// the failure would cost the learning it is announcing.
		log.Warn("learning_event_publish_failed", "type", ev.Type,
			"turn_id", t.Event.TurnID, "error", err)
	}
}

// mark records a turn id, reporting whether it is the first sighting.
func (r *Reflector) mark(turnID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen.mark(turnID)
}

// recentTurns is a bounded set of recently-seen ids with FIFO eviction.
//
// A ring rather than a slice that is appended to and resliced: the resliced
// form keeps the whole backing array alive between reallocations, so the
// bound holds on the map and not on the memory, which is the half that
// matters for a process that runs for months.
type recentTurns struct {
	ring []string
	at   int
	in   map[string]struct{}
}

func newRecentTurns(size int) *recentTurns {
	return &recentTurns{ring: make([]string, size), in: make(map[string]struct{}, size)}
}

func (s *recentTurns) mark(id string) bool {
	if _, dup := s.in[id]; dup {
		return false
	}
	if evicted := s.ring[s.at]; evicted != "" {
		delete(s.in, evicted)
	}
	s.ring[s.at] = id
	s.at = (s.at + 1) % len(s.ring)
	s.in[id] = struct{}{}
	return true
}

// eventTypeOf names an event for a log line without dereferencing a nil one.
func eventTypeOf(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	return ev.Type
}
