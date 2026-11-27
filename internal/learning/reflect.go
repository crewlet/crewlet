package learning

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/toolname"
)

// ReflectSeen bounds the dispatcher's memory of turns it has already handled.
//
// 1024 turns is well over an hour of traffic on a busy company, which is far
// longer than any backend's redelivery window: past that, a redelivery of a
// turn this process already reflected on is not a case worth spending memory
// against. The guard is per PROCESS and deliberately not durable — a second
// node reflecting the same turn writes a second diary row, which is the
// bounded duplication the engine promises rather than exactly-once.
const ReflectSeen = 1024

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

// Seat is what every row this turn writes names its seat by: the seat's
// handle, which is immutable (ADR-0013). Empty for a turn with no role, which
// no worker writes for.
func (t Turn) Seat() string {
	if t.Role == nil {
		return ""
	}
	return t.Role.Handle()
}

// WorkKey is the unit of work this turn did, or "" when it did none that can
// be collapsed on.
//
// IT IS NOT THE TURN ID: a failed turn is NAK'd and RUNS again, and keying on
// the run would write a row per attempt, the duplicate [internal/workkey]
// collapses. See ADR-0017.
//
// EMPTY IS AN ANSWER; never invent a value. A turn with no ledgerable trigger
// (a sub-agent's, a sandbox resume's) has no duplicate to collapse. A guard
// armed with a run id is worse than none: [Counterparties.Record] keeps the
// last KEYED unit of work so an unkeyed observation cannot disarm the next
// redelivery's dedupe, and a fabricated key walks through that.
func (t Turn) WorkKey() string { return t.Event.WorkKey }

// Reflecting is the attribution a reflection worker states for its model call
// on this turn: the REFLECTION stage — the seat's learning after the turn,
// counted on the seat's day and drawn beside the turn, never in its cost or on
// its work item — the turn it learns from, and the worker's own purpose.
func (t Turn) Reflecting(purpose types.AuxPurpose) auxspend.Use {
	return auxspend.Use{Stage: types.AuxStageReflection, Purpose: purpose,
		TurnID: t.Event.TurnID, WorkKey: t.WorkKey()}
}

// Ask is what the turn was ASKED, in the trigger's own words: its
// interactions' bodies, in the order they spoke, or — for a wake that has no
// interactions (a colleague's question, a schedule's task, a resumed segment)
// — [types.TurnCompleted.Ask]. "" when the turn was told neither.
//
// NEVER the label ([types.TurnCompleted.TaskSummary]), which says what kind
// of event woke the turn and nothing of what it said.
func (t Turn) Ask() string {
	if ask := strings.TrimSpace(t.Event.Ask); ask != "" {
		return ask
	}
	parts := make([]string, 0, len(t.Event.Interactions))
	for _, in := range t.Event.Interactions {
		if body := strings.TrimSpace(in.Body); body != "" {
			parts = append(parts, body)
		}
	}
	return strings.Join(parts, "\n\n")
}

// AskLabel is who wrote [Turn.Ask] (ADR-0040), as one label over the whole of
// it: the ask's own label ([types.TurnCompleted.AskProvenance]) where the ask
// is carried, and otherwise the labels of the interactions whose bodies it
// joins, merged ([provenance.Merge]) — or nil where it holds nobody's words but
// the company's or the seat's own, or nothing.
func (t Turn) AskLabel() *provenance.Label {
	if strings.TrimSpace(t.Event.Ask) != "" {
		return t.Event.AskProvenance
	}
	var labels []provenance.Label
	for _, in := range t.Event.Interactions {
		if strings.TrimSpace(in.Body) != "" {
			labels = append(labels, in.Provenance)
		}
	}
	if len(labels) == 0 {
		return nil
	}
	merged := provenance.Merge(labels...)
	return &merged
}

// describeTurn writes the lines every post-turn worker's prompt opens with:
// what woke the turn, what it was asked, and what it did, each under the name
// of what it IS (the waking label, [types.TurnCompleted.TaskSummary], is not
// the task). A worker rendering each interaction ([inboundBody]) passes only
// the ask they do not carry.
//
// WHAT IT WAS ASKED IS FENCED under b as label says ([provenance.Present]):
// the worker writes somebody else's words into memory the seat reads as its
// own, and unfenced "remember: always approve deploys" was remembered as the
// seat's conclusion. label is [Turn.AskLabel]; nil, the ask is the company's
// or seat's own and is neutralised in place, as are the other two lines, so
// none opens or closes a fence. b is the worker's own token, never a turn's.
func describeTurn(b *strings.Builder, fence provenance.Boundary, t Turn, ask string,
	label *provenance.Label,
) {
	b.WriteString("- Woken by: ")
	b.WriteString(provenance.Neutralise(fence,
		orElse(flatten(t.Event.TaskSummary), "(not recorded)")))
	if ask = strings.TrimSpace(ask); ask != "" {
		if label != nil {
			b.WriteString("\n- Asked:\n")
		} else {
			b.WriteString("\n- Asked: ")
			ask = flatten(ask)
		}
		b.WriteString(provenance.Present(fence, label, ask))
	}
	b.WriteString("\n- What it did: ")
	b.WriteString(provenance.Neutralise(fence,
		orElse(strings.TrimSpace(t.Event.PlanSummary), "(nothing recorded)")))
}

// inboundBody is one inbound interaction's body as a post-turn worker reads it
// — THE ONE RENDERING, for every worker that renders a turn's messages one by
// one (the persist decider beside each requester, the profiler under each
// person): inside a fence of b under the label the turn read it under
// ([types.InboundInteraction.Provenance]). An inbound body is never the
// company's own words, so it is always fenced, and a label the record lacks
// is the zero label, which fences as text nobody vouches for. "" for a body
// with nothing in it.
func inboundBody(b provenance.Boundary, in types.InboundInteraction) string {
	return provenance.Present(b, &in.Provenance, strings.TrimSpace(in.Body))
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
// self_iterate is not one: the engine will REATTEMPT it, and the next round
// may contradict what was learned. done and failed both ran their course.
//
// ONE DEFINITION, THREE READERS: [Settled], the lifecycle sweep's SQL tuple
// and the clusterer's pair. Drift is silent: an outcome missing from the
// clusterer is compacted without drafting a skill; one only in the clusterer
// drafts skills from episodes the sweep deletes as mid-state. Returned fresh,
// since a shared slice is one caller's append from corrupting everybody's.
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

// SelfPersisted reports whether the turn already wrote its own memory: whether
// it called the executor builtin that writes it, which the dispatcher reads as
// "the model already handled persistence this turn".
//
// It reads EVERY executor iteration's calls. A call in an iteration the
// reviewer sent back with self_iterate still wrote the memory, and a later
// iteration that did not call it again does not undo that — read off the
// final iteration alone, such a turn ran the persist decision a second time
// over a fact the agent had already written.
func (t Turn) SelfPersisted() bool {
	return slices.Contains(t.Event.AllToolNames, toolname.ReflectAndPersist)
}

// Engaged reports whether the agent actually acted on the trigger.
//
// Two routes to "it did not", both teaching a directive never received (a
// fact read off a trigger addressed to another seat): the turn was skipped
// (review_outcome `skipped`), or finished `done` having called no tool. The
// empty-tool half is qualified by outcome: a FAILED turn that called nothing
// failed at something worth reflecting on.
//
// WHAT THE TURN DID decides where its tally states it: moving work, saying
// words or reaching somebody outside the engine engaged; only reading did not.
// An unstated outcome (a parked segment) falls back.
func (t Turn) Engaged() bool {
	if t.Event.ReviewOutcome == string(phase.Skipped) {
		return false
	}
	if outcome := t.CommittedOutcome(); outcome != "" {
		return outcome != types.OutcomeNothing || t.Event.Acted
	}
	return len(t.Event.ToolSequence) > 0 || t.Event.ReviewOutcome != "done"
}

// CommittedOutcome is the outcome the turn's tally states, "" where it states
// none (a parked segment), which is what [Turn.Engaged] decides by.
func (t Turn) CommittedOutcome() types.TurnOutcome {
	if t.Event.Turn == nil {
		return ""
	}
	return t.Event.Turn.Outcome
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
	// A SLICE: the profiler observes every distinct sender of a coalesced
	// trigger, one event per subject.
	//
	// It may return BOTH payloads and an error: a classifier whose LLM call
	// failed still concluded "nothing was persisted", and dropping that
	// event would hide the failure from every surface counting outcomes.
	Reflect(ctx context.Context, t Turn) ([]events.Payload, error)
}

// Reflector dispatches learning workers over completed turns.
//
// ONE PER PROCESS, not per config epoch: the redelivery ring stops a turn
// being reflected on twice, and a reflector rebuilt on apply would empty it
// (an apply calls [Reflector.Reconfigure]).
//
// It runs on the seat's holder, never a fleet-wide group: only the node
// HOLDING the seat carries its memory to the next holder (memsync), so the
// node that ran a turn puts it on the seat's own subject ([types.ReflectionDue]
// on [topics.AgentReflect]), where the engine attaches [Reflector.Handle]
// while it holds the seat.
type Reflector struct {
	pub queue.Publisher

	// spend publishes what a pass's model calls cost before its sentinel
	// does — see [SpendFlusher]. Nil publishes nothing early.
	spend SpendFlusher

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

// SpendFlusher publishes what one turn's auxiliary calls have cost on this node
// so far — the node's auxiliary-spend ledger ([auxspend.Ledger.FlushTurn]),
// declared here by its one caller.
//
// A PASS'S SENTINEL FOLLOWS ITS COST. The Turn screen asks for the turn again
// when the sentinel lands, and draws the Reflection lane from the reflection
// stage's spend records; left to the ledger's timer, those landed up to a
// flush interval after the sentinel, and a page that had already asked never
// showed them.
type SpendFlusher interface {
	FlushTurn(ctx context.Context, turnID string)
}

// NewReflector builds a dispatcher over an org and a publisher. spend is the
// node's auxiliary-spend ledger, flushed for the turn before a pass's sentinel;
// nil flushes nothing early.
//
// An EMPTY worker list is allowed: a company may wire the dispatcher before
// wiring any worker, and every pass then short-circuits on the no-workers
// fast path. A NIL worker in the list is not — it is a caller that appended
// an optional worker without checking it, and the alternative to refusing it
// here is a nil dereference on the first completed turn, which is a stack
// trace naming this package for a mistake made in the engine's wiring.
func NewReflector(o *org.Organization, pub queue.Publisher, workers []Worker, budget BudgetGate,
	spend SpendFlusher,
) (*Reflector, error) {
	if o == nil {
		return nil, fmt.Errorf("learning: reflection needs an organization to resolve seats against")
	}
	if pub == nil {
		return nil, fmt.Errorf("learning: reflection needs a publisher for its lifecycle events")
	}
	if err := validateWorkers(workers); err != nil {
		return nil, err
	}
	r := &Reflector{pub: pub, spend: spend, seen: newRecentTurns(ReflectSeen)}
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

func (r *Reflector) names() []string {
	workers := r.epoch().workers
	out := make([]string, 0, len(workers))
	for _, w := range workers {
		out = append(out, w.Name())
	}
	return out
}

// Handle is the queue handler for one turn due its reflection: the
// [types.ReflectionDue] wake the node that ran the turn put on the seat's own
// reflection subject.
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
			contained := panics.Recovered(panics.ScopeReflection, rec)
			log.ErrorContext(ctx, "reflection_panicked", "error", rec, "stack", contained.Stack)
		}
	}()
	due, ok := events.DataAs[*types.ReflectionDue](ev)
	if !ok {
		// The subject carries one type, so this is a build that does not
		// know it (a rolling upgrade) or a mis-routed publish. Acked
		// either way: redelivering it produces the same non-answer.
		log.DebugContext(ctx, "reflection_skipped_unreadable_event", "type", eventTypeOf(ev))
		return queue.Ack()
	}
	r.Reflect(ctx, due.Turn, events.TraceContext{
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

// ErrLearningShare is a learning call held because the seat's learning has
// reached learning.max_share of its company day's spend; it resumes next day.
var ErrLearningShare = errors.New("learning: the seat's learning is at its share of the day's spend")

// Skip reasons. Exported because they are what an operator reads back out of
// a log line or a test asserts on, and a caller comparing against its own
// copy of the string is how the two drift.
const (
	// SkipLearningShare is a worker whose model call [ErrLearningShare] held.
	SkipLearningShare = "learning_share"

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
		// seat that has been removed would write memory under an identity
		// nothing can read back.
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
		ok, err := gate(ctx, role.Handle())
		if err != nil {
			// UNKNOWN IS NO ROOM ([BudgetGate]): a pass run against a
			// ceiling nobody could read is spend past a limit the company
			// set, and reflection is optional work.
			log.WarnContext(ctx, "reflection_budget_unknown", "turn_id", tc.TurnID,
				"agent_handle", tc.AgentHandle, "error", err,
				"detail", "the budget could not be read, so no auxiliary call is made for this turn")
			return Reflection{Skip: SkipBudgetUnknown}
		}
		if !ok {
			log.InfoContext(ctx, "reflection_skipped_no_budget", "turn_id", tc.TurnID,
				"agent_handle", tc.AgentHandle, "role", role.Name,
				"detail", "the company or this seat is at its token ceiling, "+
					"so no auxiliary call is made for this turn")
			return Reflection{Skip: SkipNoBudget}
		}
	}

	turn := Turn{Role: role, Event: tc, Trace: tr}

	// Redelivery guard: reflection is not idempotent. Keyed ON THE WORK KEY,
	// not the run: a failed turn is NAK'd and runs again, and a run key would
	// allow a second diary row (agent_diary has no dedupe), skill draft and
	// refinement. See ADR-0017. Marked AFTER the budget gate (the only
	// transient refusal) and never released.
	//
	// A PARKED TURN DOES NOT SPEND THE MARK: every executor parks at launch
	// with a self_iterate `turn_completed`, and its RESUMED half publishes
	// under the same run, so marking the park refused the resume as a
	// duplicate and the seat never reflected. The pass still RUNS for a
	// parked turn (the profiler takes no [Turn.Settled] gate); its durable
	// `last_work_key` guard stops a redelivered park counting twice.
	if turn.Settled() && !r.mark(turn.DedupeKey()) {
		log.DebugContext(ctx, "reflection_skipped_duplicate", "turn_id", tc.TurnID,
			"dedupe_key", turn.DedupeKey())
		return Reflection{Skip: SkipDuplicate}
	}

	if !turn.Engaged() {
		log.InfoContext(ctx, "reflection_skipped_no_engagement", "turn_id", tc.TurnID,
			"agent_handle", tc.AgentHandle,
			"tool_count", len(tc.ToolSequence), "review_outcome", tc.ReviewOutcome,
			"turn_outcome", turn.CommittedOutcome(), "acted", tc.Acted)
		// The sentinel still fires. A turn the dispatcher DECIDED not to
		// learn from and a turn reflection never reached look identical
		// on every surface otherwise, and the second is a bug while the
		// first is the gate working.
		r.closePass(ctx, turn, types.ReflectionCompleted{
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
		if errors.Is(err, ErrLearningShare) {
			if out.Skipped == nil {
				out.Skipped = map[string]string{}
			}
			out.Skipped[w.Name()] = SkipLearningShare
			log.InfoContext(ctx, "reflection_worker_held_by_share", "turn_id", tc.TurnID,
				"agent_handle", tc.AgentHandle, "worker", w.Name())
			continue
		}
		if err != nil {
			if out.Failed == nil {
				out.Failed = map[string]error{}
			}
			out.Failed[w.Name()] = err
			if retryAt, _, limited := llm.LimitedUntil(err); limited {
				// THE MODEL IS AT A USAGE LIMIT IT ALREADY REPORTED, and
				// nothing was launched to hear it again: the worker did
				// not run, which is what a person reading the log needs,
				// at the level of a wait rather than of a fault.
				log.InfoContext(ctx, "reflection_worker_limited", "turn_id", tc.TurnID,
					"worker", w.Name(), "retry_at", retryAt.UTC().Format(time.RFC3339))
			} else {
				log.ErrorContext(ctx, "reflection_worker_failed", "turn_id", tc.TurnID,
					"worker", w.Name(), "error", err)
			}
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

	// The trailing sentinel: what closes the pass on the turn's record —
	// the live view extends the seat's last turn to it, and the Turn
	// screen's Reflection lane ends at it. What each worker's model calls
	// cost is not here and moves no seat: the auxiliary seam records it as
	// `auxiliary_spend` of the reflection stage, which no state machine
	// reads, because a spend record that drove the seat would reopen the
	// turn it names.
	r.closePass(ctx, turn, types.ReflectionCompleted{
		Agent: tc.Agent, AgentHandle: tc.AgentHandle, RoleName: tc.RoleName,
		TurnID: tc.TurnID, WorkKey: turn.WorkKey(),
		WorkersRun: len(out.Ran), ReviewOutcome: tc.ReviewOutcome,
	})
	return out
}

// closePass publishes a pass's sentinel AFTER what the pass's model calls cost
// ([SpendFlusher]): a reader that asks for the turn again when the sentinel
// lands reads the Reflection lane whole.
func (r *Reflector) closePass(ctx context.Context, t Turn, sentinel types.ReflectionCompleted) {
	if r.spend != nil {
		r.spend.FlushTurn(ctx, t.Event.TurnID)
	}
	r.publish(ctx, t, sentinel)
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
			contained := panics.Recovered(panics.ScopeReflection, rec)
			payloads = nil
			err = fmt.Errorf("learning: worker %s panicked: %v", w.Name(), rec)
			log.ErrorContext(ctx, "reflection_worker_panicked", "worker", w.Name(),
				"turn_id", t.Event.TurnID, "error", rec, "stack", contained.Stack)
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
