package learning

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/workkey"
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
// It is an executor builtin, so a call lands in the executor-scoped
// [types.TurnCompleted.ToolSequence]. [Turn.SelfPersisted] reads
// [types.TurnCompleted.PlanToolSequence] too, which this build never writes: an
// older build recorded its planning phase's calls there, and a turn one of its
// nodes completed during a rolling upgrade must not be persisted twice.
const ReflectTool = "reflect_and_persist"

// ReflectSeen bounds the dispatcher's memory of turns it has already handled.
//
// 1024 turns is well over an hour of traffic on a busy company, which is far
// longer than any backend's redelivery window: past that, a redelivery of a
// turn this process already reflected on is not a case worth spending memory
// against. The guard is per PROCESS and deliberately not durable — a second
// node reflecting the same turn writes a second diary row, which is the
// bounded duplication the engine promises rather than exactly-once.
const ReflectSeen = 1024

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
	// handed down explicitly, because nothing in a turn travels ambiently.
	Trace events.TraceContext
}

// WorkKey is the unit of work this turn did, or "" when it did none that can
// be collapsed on.
//
// IT IS NOT THE TURN ID, and the readers below used to take it from there
// because one value carried both meanings. A turn that fails without acting is
// NAK'd and redelivered, so one unit of work legitimately RUNS several times;
// a turn id now names one of those runs, and keying the episode on it would
// write a row per attempt — which is precisely the duplicate
// [internal/workkey] exists to collapse. See ADR-0017.
//
// EMPTY IS AN ANSWER, and inventing a value for it is the failure this
// shape avoids. A turn with no ledgerable trigger — a scheduled fire — has no
// cross-run duplicate to collapse, and a guard armed with a run id in that
// slot is worse than one armed with nothing: [Counterparties.Record] keeps the
// last KEYED unit of work precisely so an unkeyed observation cannot disarm
// the next redelivery's dedupe, and a fabricated key walks straight through
// that.
//
// FALLING BACK ONLY ON SHAPE. A `turn_completed` from a build before the split
// carries no work key and its turn id IS one, and a rolling upgrade guarantees
// some of those — but so does a post-split turn with no trigger key, and the
// wire cannot tell the two apart because the field is `omitempty`. The GRAMMAR
// can: see [workkey.IsDerived].
func (t Turn) WorkKey() string {
	if t.Event.WorkKey != "" {
		return t.Event.WorkKey
	}
	if workkey.IsDerived(t.Event.TurnID) {
		return t.Event.TurnID
	}
	return ""
}

// DedupeKey is what the redelivery guard remembers, and it is a DIFFERENT
// question from [Turn.WorkKey].
//
// That one asks "which unit of work is this, if any", and answers "" honestly.
// This asks "have I already processed this delivery", which always has an
// answer — so it falls back to the run, and an unkeyed turn is deduped per
// execution rather than every unkeyed turn in the company collapsing onto one
// empty mark and reflecting exactly once.
func (t Turn) DedupeKey() string {
	if key := t.WorkKey(); key != "" {
		return key
	}
	return t.Event.TurnID
}

// SettledOutcomes are the review decisions that END a turn, and therefore the
// ones this package may learn from.
//
// self_iterate is not one: it is a mid-turn state the engine will REATTEMPT,
// so a fact persisted from it — or a skill drafted from it — is learned from
// work the agent itself judged incomplete, and the next round may contradict
// it. done and failed both mean the turn ran its course; failed is as much a
// lesson as done, which is why the settled set is not just "succeeded".
//
// ONE DEFINITION, THREE READERS, and it is a list because it had been three
// separate literals: [Settled], the lifecycle sweep's SQL tuple, and the
// clusterer's own pair. [terminalOutcomes]'s doc already warned that "two
// hand-written lists is how a row ends up being neither" — there were three,
// and it could not see the copy in cluster.go to say so.
//
// What that shape invites is silent in both directions. An outcome added here
// and to the sweep but not the clusterer is learned from and compacted while
// never drafting a skill; added to the clusterer alone it drafts skills from
// episodes the sweep then deletes as mid-state. Neither fails anything.
//
// Returned fresh, never a package-level slice: a shared backing array is one
// caller's append away from rewriting everybody else's list.
func SettledOutcomes() []string { return []string{"done", "failed"} }

// Settled reports whether a review outcome ended its turn.
//
// Takes the outcome rather than a [Turn], because two of the three readers
// hold an [Episode] row instead — the turn they describe finished long ago.
func Settled(outcome string) bool {
	return slices.Contains(SettledOutcomes(), outcome)
}

// Settled reports whether the turn reached a terminal outcome.
func (t Turn) Settled() bool { return Settled(t.Event.ReviewOutcome) }

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
//   - the turn opted out (plan_decision skip) — nobody was asking this seat
//     to do anything, and the engine ended it silently;
//   - the turn engaged with nothing: it finished `done` having called no
//     tool at all, which is what a seat that read the trigger, recognised it
//     as somebody else's and then said so only to itself looks like.
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

	// Reflect runs one pass and returns the lifecycle events to publish,
	// empty for none.
	//
	// A SLICE because a pass is not always about one thing: the
	// counterparty profiler observes every distinct sender of a coalesced
	// trigger, and its event is per-subject. Collapsing that to one would
	// either announce the first party and silently drop the rest, or need
	// a second publishing path inside the worker — which is where the
	// trace context and the source stamping would start to disagree with
	// the dispatcher's.
	//
	// It may return BOTH payloads and an error: the payloads are what the
	// worker managed to conclude, the error is what went wrong reaching
	// them. A classifier whose LLM call failed still concluded "nothing
	// was persisted", and dropping that event because the call failed
	// would hide the failure from every surface that counts outcomes.
	Reflect(ctx context.Context, t Turn) ([]events.Payload, error)
}

// Subscriber is the half of the queue the dispatcher attaches through.
type Subscriber interface {
	Subscribe(ctx context.Context, topic, group string, h queue.Handler) error
}

// Reflector dispatches learning workers over completed turns.
//
// ONE PER PROCESS, not one per config epoch, even though its org and its
// workers both belong to an epoch: the redelivery ring below is the reason.
// It is what stops a turn being reflected on twice, and reflection is not
// idempotent — each pass is a fresh auxiliary call that writes a second,
// differently-worded row for the same fact. A reflector rebuilt on every
// apply would empty that ring, so a redelivery landing either side of a
// config change would be classified twice, which is the one failure the ring
// exists to prevent. So an apply calls [Reflector.Reconfigure] and the
// subscription, and the ring, stay put.
type Reflector struct {
	pub queue.Publisher

	// live is the epoch-scoped half, swapped whole by Reconfigure and
	// never mutated — the same rule the engine's own epoch follows.
	live atomic.Pointer[reflectorEpoch]

	mu   sync.Mutex
	seen *recentTurns
}

// reflectorEpoch is what an apply replaces.
type reflectorEpoch struct {
	org     *org.Organization
	workers []Worker

	// budget is the pre-flight gate: may this seat spend on reflection at
	// all? Nil always admits, which is the single-node case and the case
	// where no ceiling is configured.
	//
	// On the EPOCH rather than the Reflector because the answer depends on
	// the company's ceilings, and those move with a revision.
	budget BudgetGate
}

// BudgetGate reports whether a seat may spend auxiliary tokens right now.
//
// Three-valued in the way this codebase insists on: true/false is the
// answer, and an error is "the counter could not be reached", which is NOT a
// refusal. A blip must not silently stop a company learning — the charge on
// the way out is what keeps an unreachable counter from also being free.
type BudgetGate func(ctx context.Context, seat *org.Role) (bool, error)

// NewReflector builds a dispatcher over an org and a publisher.
//
// An EMPTY worker list is allowed: a company may wire the dispatcher before
// wiring any worker, and every pass then short-circuits on the no-workers
// fast path. A NIL worker in the list is not — it is a caller that appended
// an optional worker without checking it, and the alternative to refusing it
// here is a nil dereference on the first completed turn, which is a stack
// trace naming this package for a mistake made in the engine's wiring.
func NewReflector(o *org.Organization, pub queue.Publisher, workers []Worker, budget BudgetGate) (*Reflector, error) {
	if o == nil {
		return nil, fmt.Errorf("learning: reflection needs an organization to resolve seats against")
	}
	if pub == nil {
		return nil, fmt.Errorf("learning: reflection needs a publisher for its lifecycle events")
	}
	if err := validateWorkers(workers); err != nil {
		return nil, err
	}
	r := &Reflector{pub: pub, seen: newRecentTurns(ReflectSeen)}
	r.live.Store(&reflectorEpoch{org: o, workers: slices.Clone(workers), budget: budget})
	return r, nil
}

// Reconfigure swaps the org and the worker set an apply produced.
//
// Validated the same way the constructor validates, and REFUSED the same
// way: a bad worker set leaves the previous one serving rather than taking
// the dispatcher down, because the alternative to reflecting with the old
// config is not reflecting at all.
//
// An in-flight pass finishes on the epoch it started with — it read the
// pointer once — which is the same guarantee a turn gets about its own
// config pin.
func (r *Reflector) Reconfigure(o *org.Organization, workers []Worker, budget BudgetGate) error {
	if o == nil {
		return fmt.Errorf("learning: reflection needs an organization to resolve seats against")
	}
	if err := validateWorkers(workers); err != nil {
		return err
	}
	r.live.Store(&reflectorEpoch{org: o, workers: slices.Clone(workers), budget: budget})
	log.Info("reflect_engine_reconfigured", "workers", r.names())
	return nil
}

// epoch is the currently-serving org and worker set.
func (r *Reflector) epoch() *reflectorEpoch {
	if live := r.live.Load(); live != nil {
		return live
	}
	return &reflectorEpoch{}
}

// validateWorkers refuses a set the dispatcher cannot report on honestly.
func validateWorkers(workers []Worker) error {
	names := make(map[string]bool, len(workers))
	for i, w := range workers {
		if w == nil {
			return fmt.Errorf("learning: reflection worker %d is nil", i)
		}
		// A pass reports its skips and its swallowed failures BY NAME, so
		// two workers sharing one erase each other in both maps and an
		// operator reads one worker's failure as the other's.
		if names[w.Name()] {
			return fmt.Errorf("learning: two reflection workers named %q", w.Name())
		}
		names[w.Name()] = true
	}
	return nil
}

// Start attaches the dispatcher to the completed-turn subject.
func (r *Reflector) Start(ctx context.Context, sub Subscriber) error {
	if err := sub.Subscribe(ctx, turnCompletedTopic, ReflectGroup, r.Handle); err != nil {
		return fmt.Errorf("learning: subscribe %s: %w", turnCompletedTopic, err)
	}
	log.InfoContext(ctx, "reflect_engine_started", "workers", r.names())
	return nil
}

func (r *Reflector) names() []string {
	workers := r.epoch().workers
	out := make([]string, 0, len(workers))
	for _, w := range workers {
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
			log.ErrorContext(ctx, "reflection_panicked", "error", rec, "stack", string(debug.Stack()))
		}
	}()
	tc, ok := events.DataAs[*types.TurnCompleted](ev)
	if !ok {
		// The subject carries one type, so this is a build that does not
		// know it (a rolling upgrade) or a mis-routed publish. Acked
		// either way: redelivering it produces the same non-answer.
		log.DebugContext(ctx, "reflection_skipped_unreadable_event", "type", eventTypeOf(ev))
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

	// SkipNoBudget is the pass declining to START because the company or
	// the seat is already at its ceiling.
	//
	// Reflection is best effort, so this is a skip rather than a failure —
	// but it is the skip that costs nothing, which is the entire point: a
	// pass that runs and then fails has already made its auxiliary calls
	// and spent the tokens it was supposed to be saving.
	SkipNoBudget = "no_budget"
)

// Reflect runs one pass over a completed turn.
//
// Exported so an embedder can drive reflection without a queue — and so the
// gates below are reachable in a test without staging a delivery.
func (r *Reflector) Reflect(ctx context.Context, tc types.TurnCompleted, tr events.TraceContext) Reflection {
	// Fast path before any other work: with nothing wired there is no
	// question to ask about this turn, and asking it means an org lookup
	// and a dedup insert per completed turn for nothing.
	live := r.epoch()
	if len(live.workers) == 0 {
		return Reflection{Skip: SkipNoWorkers}
	}

	// live.org is never nil past that check: both the constructor and
	// Reconfigure refuse a nil org, and the only epoch without one is the
	// zero value epoch() falls back to — which has no workers either, so
	// it has already returned above.
	// THE HANDLE, which is what addresses a seat. The turn's `role` is the
	// seat's DISPLAY NAME and nothing resolves a seat by it — so looking a
	// seat up by it found nobody, every completed turn was skipped as
	// SkipNoRole, and the company silently stopped learning: no episode,
	// no diary row, no counterparty profile, on a path whose every failure
	// is best effort and therefore says nothing.
	role := live.org.Role(tc.AgentHandle)
	if role == nil {
		// A turn from a seat this epoch no longer has. Learning about a
		// seat that has been renamed or removed would write memory under
		// an identity nothing can read back.
		log.DebugContext(ctx, "reflection_skipped_no_role",
			"turn_id", tc.TurnID, "agent_handle", tc.AgentHandle, "role", tc.RoleName)
		return Reflection{Skip: SkipNoRole}
	}

	// Per-role opt-out. Unset inherits the company-wide setting, which is
	// the wiring's decision — a company with learning off does not build a
	// Reflector at all — so only an explicit false is a skip here.
	if !role.LearningEnabled.Or(true) {
		log.DebugContext(ctx, "reflection_skipped_role_disabled", "turn_id", tc.TurnID, "role", role.Name)
		return Reflection{Skip: SkipRoleDisabled}
	}

	// THE BUDGET PRE-FLIGHT, and it sits here — BEFORE the redelivery mark —
	// for a reason worth stating.
	//
	// An exhausted budget is the one TRANSIENT refusal in this sequence:
	// every other gate is a property of the turn that will still hold on a
	// redelivery, while this one flips the moment an operator raises the
	// ceiling or the window rolls. Marking first and skipping second would
	// burn the turn's one chance to be learned from on a condition that had
	// nothing to do with the turn. Checking first means a redelivery after
	// the cap moves gets a real pass, and nothing has to release a mark.
	if gate := live.budget; gate != nil {
		ok, err := gate(ctx, role)
		if err != nil {
			// UNKNOWN, not refused. A counter that cannot be reached must
			// not silently stop a company learning; what keeps that from
			// also being free is that the completion charges on the way
			// out either way.
			log.WarnContext(ctx, "reflection_budget_unknown", "turn_id", tc.TurnID,
				"agent_handle", tc.AgentHandle, "error", err,
				"detail", "reflecting anyway; the spend is still charged")
		} else if !ok {
			log.InfoContext(ctx, "reflection_skipped_no_budget", "turn_id", tc.TurnID,
				"agent_handle", tc.AgentHandle, "role", role.Name,
				"detail", "the company or this seat is at its token ceiling, "+
					"so no auxiliary call is made for this turn")
			return Reflection{Skip: SkipNoBudget}
		}
	}

	turn := Turn{Role: role, Event: tc, Trace: tr}

	// Redelivery guard. Every backend may redeliver, and reflection is not
	// idempotent: each pass is a fresh auxiliary-LLM call that can write a
	// second, differently-worded row for the same fact.
	//
	// ON THE WORK KEY, not the run. "The same fact" is a property of the
	// unit of work, and a turn that fails without acting is NAK'd and runs
	// again — so a mark keyed on the run would let every redelivery take a
	// full second pass: a second diary row (agent_diary has no dedupe of
	// its own), a second skill draft, a second refinement. The episode row
	// and the interaction count would still collapse on their own indexes,
	// which is what would make the duplication invisible in the two places
	// anybody looks. See ADR-0017.
	//
	// Marked AFTER the budget gate and never released. The budget is the
	// only transient refusal, and taking the mark after it is what lets a
	// redelivery get a real pass once the ceiling moves — so no path left
	// here wants a mark released, and a release on a path that cannot
	// happen is a release that does nothing.
	// A PARKED TURN DOES NOT SPEND IT, and that is the one condition on
	// this guard. A suspended executor publishes `turn_completed` carrying
	// the suspend's own self_iterate, and its RESUMED half publishes under
	// the same run — so marking the park refused the resume as a duplicate,
	// and a seat that does its work through `run_sandbox` reflected on
	// nothing, ever: no episode, no diary row, no counterparty profile, no
	// skill, with an empty memory tab as the only symptom.
	//
	// The pass still RUNS for a parked turn, because one worker legitimately
	// wants it: observing who you talked to does not depend on what the
	// agent decided to do next, so the counterparty profiler takes no
	// [Turn.Settled] gate. What a redelivered park can now repeat is that
	// one worker's auxiliary call — and its own durable guard
	// (`last_work_key`) still stops the interaction being counted twice,
	// which is the half that would have been wrong rather than merely
	// expensive.
	if turn.Settled() && !r.mark(turn.DedupeKey()) {
		log.DebugContext(ctx, "reflection_skipped_duplicate", "turn_id", tc.TurnID,
			"dedupe_key", turn.DedupeKey())
		return Reflection{Skip: SkipDuplicate}
	}

	if !turn.Engaged() {
		log.InfoContext(ctx, "reflection_skipped_no_engagement", "turn_id", tc.TurnID,
			"agent_handle", tc.AgentHandle, "plan_decision", string(tc.PlanDecision),
			"tool_count", len(tc.ToolSequence), "review_outcome", tc.ReviewOutcome)
		// The sentinel still fires. A turn the dispatcher DECIDED not to
		// learn from and a turn reflection never reached look identical
		// on every surface otherwise, and the second is a bug while the
		// first is the gate working.
		r.publish(ctx, turn, types.ReflectionCompleted{
			Agent: tc.Agent, AgentHandle: tc.AgentHandle, RoleName: tc.RoleName,
			TurnID: tc.TurnID, WorkKey: turn.WorkKey(),
			WorkersRun: 0, ReviewOutcome: tc.ReviewOutcome,
		})
		return Reflection{Skip: SkipNoEngagement}
	}

	out := Reflection{}
	for _, w := range live.workers {
		if reason := w.Skip(turn); reason != "" {
			if out.Skipped == nil {
				out.Skipped = map[string]string{}
			}
			out.Skipped[w.Name()] = reason
			log.DebugContext(ctx, "reflection_worker_skipped", "turn_id", tc.TurnID,
				"worker", w.Name(), "reason", reason)
			continue
		}
		payloads, err := r.dispatch(ctx, w, turn)
		if err != nil {
			if out.Failed == nil {
				out.Failed = map[string]error{}
			}
			out.Failed[w.Name()] = err
			log.ErrorContext(ctx, "reflection_worker_failed", "turn_id", tc.TurnID,
				"worker", w.Name(), "error", err)
		}
		// Counted as RUN whether or not it failed, and whether or not it
		// had anything to publish: it spent the turn's budget and the
		// operator's wall clock. Counting a worker the gate has just
		// skipped makes a pass that did nothing at all report
		// workers_run=1.
		out.Ran = append(out.Ran, w.Name())
		for _, payload := range payloads {
			if payload != nil {
				r.publish(ctx, turn, payload)
			}
		}
	}

	// The trailing sentinel. The auxiliary phase events workers emit keep
	// the seat rendering as WORKING for as long as they are the newest
	// event for that role; this is what flips it back to idle when the
	// pass is over.
	r.publish(ctx, turn, types.ReflectionCompleted{
		Agent: tc.Agent, AgentHandle: tc.AgentHandle, RoleName: tc.RoleName,
		TurnID: tc.TurnID, WorkKey: turn.WorkKey(),
		WorkersRun: len(out.Ran), ReviewOutcome: tc.ReviewOutcome,
	})
	return out
}

// dispatch runs one worker, converting a panic into an error.
//
// Per worker rather than once around the loop, so one worker taking a nil map
// or an out-of-range index does not cost the pass its remaining workers or
// its sentinel — the seat would then render as working for ever, which is
// the visible half of a bug whose invisible half is simply no learning.
func (r *Reflector) dispatch(ctx context.Context, w Worker, t Turn) (payloads []events.Payload, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			payloads = nil
			err = fmt.Errorf("learning: worker %s panicked: %v", w.Name(), rec)
			log.ErrorContext(ctx, "reflection_worker_panicked", "worker", w.Name(),
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
		log.WarnContext(ctx, "learning_event_publish_failed", "type", ev.Type,
			"turn_id", t.Event.TurnID, "error", err)
	}
}

// mark records a turn's [Turn.DedupeKey], reporting whether it is the first
// sighting.
//
// The unit of work where there is one, not the run: a run id would let every
// redelivery of one trigger take a full second pass.
func (r *Reflector) mark(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen.mark(key)
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
