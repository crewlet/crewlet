package engine

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The turn-level events, published around turn.Run.
//
// The phases publish their own (internal/agent/runner/telemetry.go); these two
// close the turn, and they are separate because they have different readers:
//
//   - agent_turn_completed is the DASHBOARD's single-phase summary. It is what
//     ends a seat's live row, so a turn that failed to publish it leaves a
//     working indicator up until the next turn starts.
//   - turn_completed is the LEARNING subsystem's own record of the same turn,
//     described for a different consumer. One event
//     serving both would have to be the union of two schemas, and every reader
//     would then have to know which half applied to it.
//
// Published from the engine rather than from the loop because only this frame
// knows the things that bound a turn from outside it: what woke it, which
// conversation it served, how long it took, and that it ended at all — the
// loop returns identically whether its caller goes on to publish or not.

// turnTelemetry is one turn's publishable identity, assembled once at the top
// of the turn and used at both ends.
type turnTelemetry struct {
	handle string
	role   string
	// runID is THIS EXECUTION's identity — minted once at the top of the
	// turn and used at both ends, so a turn's opening phase event and its
	// completion name the same run. See ADR-0017.
	runID string
	// workKey is the unit of work, stable across a re-run, and workSince
	// when it began — the instant every operation id derived from the key
	// carries, and so reproduced by a re-run exactly as the key is.
	workKey   string
	workSince time.Time
	agentID   string
	trigger   types.Trigger

	// convKey is the CONVERSATION IDENTITY — what every event this turn
	// publishes is tagged with, what the episode row is filed under, and
	// what matches a person's answer back to a detached run — while
	// partKey is the inbox PARTITION the trigger arrived in, carried only
	// so a detached coding run's row can also state the batch it was
	// launched from, which is all a peer predating the identity can match
	// on.
	convKey   string
	partKey   string
	startedAt time.Time
	trace     events.TraceContext

	// interactions is who spoke to this seat and what they said, one per
	// constituent of the partition. It rides the completed-turn event
	// because the reflect dispatcher is a QUEUE CONSUMER: it can run on a
	// node that never saw the trigger, so an interaction it cannot read
	// off the payload is one it cannot reason about at all.
	interactions []types.InboundInteraction

	// skills is the synthesized-skill ids offered to this turn's prompt.
	// Set after the prefetch, which is the only thing that knows them.
	skills []string
}

// newRunID mints the identity of ONE EXECUTION of a turn.
//
// A uuid rather than anything derived, and that is the whole point: every
// derivable identity a turn has — the trigger's ids, the conversation, the
// seat, the clock rounded to anything useful — is something a redelivery
// reproduces, and reproducing it is exactly the bug ADR-0017 records. Two runs
// of one trigger must not be able to collide however hard they try.
//
// Not a package-level counter either: a run id is read by other processes (a
// detached sandbox row, a peer's dashboard), so it has to be unique across the
// fleet rather than within this process.
//
// TIME-ORDERED, and the instant it carries is read: a write a run with no unit
// of work makes is seeded from the RUN, and the operation id derived from it
// carries the run's start as its mint instant — which is what the state log
// compares with this node's latest adoption before it will decide an operation
// again (builtin.Actor.OperationSince). A resumed run keeps its id, so the
// instant rides the pending row with no field of its own. Minted by the state
// log's own grammar ([statelog.NewOpID]) so the one reader of that instant
// needs no second parser; its random tail keeps two runs from colliding
// exactly as a v4 did.
func newRunID() string { return statelog.NewOpID(time.Now(), "") }

// describeTurn assembles the identity for one dispatch.
//
// The trigger is taken from the FIRST event of the partition. A coalesced
// partition has several, and the first is the one whose thread the turn is
// answering; branding the turn with the last would attribute it to whichever
// message happened to arrive while the seat was busy.
func (e *Engine) describeTurn(ctx context.Context, company *Company, req Request) turnTelemetry {
	t := turnTelemetry{
		handle:    req.Handle,
		runID:     req.RunID,
		workKey:   req.WorkKey,
		workSince: req.WorkSince,
		convKey:   req.ConversationKey,
		// READ OFF THE PARTITION'S OWN EVENTS rather than carried on the
		// Request beside the identity, because every constituent already
		// holds it and a field would be one more thing a second Request
		// construction site could leave empty — which is exactly how the
		// conversation reached the sandbox row as "" for the whole life of
		// that feature. The identity has no such source: a resumed turn
		// has no events at all, so it has to travel.
		partKey:   partitionKeyOf(req.Events),
		startedAt: time.Now().UTC(),
	}
	t.role, t.agentID = seatIdentity(company, req.Handle)
	// OFF THE ASK: a coalesced conversation's interactions come from the
	// merged digest's own constituent list, which is the same set the
	// partition held and the one place a merge combined them.
	t.interactions = e.interactionsOf(req.Ask())
	// The turn's own span, not the trigger's ids copied forward.
	//
	// This used to read `TraceID: ev.TraceID, SpanID: ev.SpanID` straight off
	// the trigger, which made every event the turn published claim the
	// TRIGGER's span as its own — so `span_id` named a span that had already
	// ended and the dashboard's tree collapsed the whole turn onto the wake
	// that started it. The trace is still inherited, which was the right
	// half: the dispatcher restored it onto ctx before runTurn opened the
	// turn span beneath it, so the trace id is the trigger's and the span id
	// is this turn's, with the trigger as its parent.
	t.trace = tracing.TraceOf(ctx)
	for _, ev := range req.Events {
		if ev == nil {
			continue
		}
		t.trigger = types.DescribeTrigger(ev)
		break
	}
	return t
}

// runnerTurn is the identity handed to the phase runner.
//
// TWO IDENTITIES, NEVER ONE. runID names this execution and is what every
// event's turn_id carries; workKey names the unit of work and is what a write
// has to be idempotent against. See ADR-0017 for what folding them into one
// value cost.
// Both identities come off the telemetry rather than off arguments, so the
// events published at the two ends of a turn and the context its tools run
// under cannot name different runs.
func (t turnTelemetry) runnerTurn(company *Company,
	depth int, chain []string, task string, reply turn.Reply,
) runner.Turn {
	return runner.Turn{
		RunID: t.runID, WorkKey: t.workKey,
		AgentID:         t.agentID,
		Trigger:         t.trigger,
		ConversationKey: t.convKey, Trace: t.trace,
		Context: &turnctx.Turn{
			RunID:     t.runID,
			WorkKey:   t.workKey,
			WorkSince: t.workSince,
			// The seat and the ORG both come off the pinned epoch, so a
			// colleague lookup mid-turn resolves against the roster this
			// turn started under rather than one that changed underneath
			// it.
			Seat:  company.Org.AgentSeatByHandle(t.handle),
			Org:   company.Org,
			Depth: depth,
			// The path that got here, so an ask this turn makes carries
			// the whole provenance rather than only its immediate asker.
			// It was set on the sandbox-resume path alone, so every
			// ordinary turn's ask reported a one-element chain.
			Chain: chain,
			// The conversation this turn owes an answer to, so work it
			// detaches carries it: a coding run's row is written from
			// here, the resumed turn reports back through it, and a
			// person's answer is MATCHED on it — because a person
			// answers on the conversation rather than into the batch
			// their reply lands in.
			//
			// The partition rides along so the row can state the batch
			// the run was launched from: it tells two runs parked on
			// one direct message apart, and it is all a peer predating
			// the conversation field has to match on.
			ConversationKey: t.convKey,
			PartitionKey:    t.partKey,
			// The brief and the delivery obligation, carried for the
			// same reason: a resumed turn sees neither its trigger nor
			// this frame, so both have to reach the row from here.
			Task:  task,
			Reply: reply.String(),
		},
	}
}

// publishTurnCompleted closes the turn for both readers.
//
// TELEMETRY NEVER FAILS THE WORK, so this returns nothing: the turn's
// deliveries have already fired and its result is already the caller's answer.
// A broker that refuses these events must not turn finished work into a failed
// turn — the same rule the phase publisher states.
func (e *Engine) publishTurnCompleted(ctx context.Context, t turnTelemetry,
	spend runner.Spend, res turn.Result, err error,
) {
	ended := time.Now().UTC()
	failed := err != nil || res.Decision == phase.Failed
	decision := string(res.Decision)

	summary := types.AgentTurnCompleted{
		Agent:    t.agentID,
		RoleName: t.role,
		// The model that answered the LAST phase to run, which is what a
		// one-line row names. The per-phase models are carried beside it
		// rather than collapsed, because a seat with a fallback chain can
		// legitimately have run the executor and the reviewer on two
		// different models.
		Model:          lastModel(spend),
		Trigger:        t.trigger,
		Prompt:         t.trigger.Summary,
		Response:       res.Artifact,
		InputTokens:    spend.InputTokens,
		OutputTokens:   spend.OutputTokens,
		TotalTokens:    spend.Total(),
		ToolExecutions: spend.ToolExecutions,
		TurnID:         t.runID,
		WorkKey:        t.workKey,
		ExecuteModel:   spend.ExecuteModel,
		ReviewModel:    spend.ReviewModel,
		// What this turn DELEGATED, beside what it spent itself. Kept
		// apart from TotalTokens on purpose: a worker's tokens are
		// already charged through the shared meter, so folding them in
		// would double-count them — and the split is the only thing that
		// answers "how much of this turn was fan-out" when a seat's spend
		// jumps and its own rounds did not.
		SubagentCount:   spend.Workers,
		SubagentTokens:  spend.WorkerTokens,
		Iterations:      res.Rounds,
		Decision:        decision,
		Failed:          failed,
		ConversationKey: t.convKey,
	}
	if err != nil {
		// Bounded only so the event is publishable at all; see
		// events.MaxDiagnosticBytes. An event refused by the queue is
		// logged and dropped, so an unbounded failure text costs the
		// operator the whole record rather than its tail.
		summary.Error = events.ClipDiagnostic(err.Error())
		summary.ErrorKind = "error"
	}
	if res.Breach != nil {
		// A guard breach is not an error: the turn ran and was stopped by
		// a rule. Naming the RULE is the whole value: "depth_cap" and
		// "stall" send an operator to different places, and a bare "failed"
		// sends them to neither.
		//
		// THE RULE WINS WHEN BOTH ARE SET, and a panic is the case that
		// sets both: its error says what broke and its breach names the
		// guard. Read the other way round, the one kind that says the
		// engine itself is at fault reached the Turn screen as the generic
		// "error". The error's own text is kept, since it names the phase
		// and round the breach detail does not.
		if summary.Error == "" {
			summary.Error = events.ClipDiagnostic(res.Breach.Detail)
		}
		summary.ErrorKind = string(res.Breach.Kind)
	}
	e.publishEvent(ctx, events.New(summary, t.trace), t.role)
	e.publishFailure(ctx, t, res, err)

	e.publishEvent(ctx, events.New(types.TurnCompleted{
		Agent:       t.agentID,
		AgentHandle: t.handle,
		RoleName:    t.role,
		TurnID:      t.runID,
		WorkKey:     t.workKey,
		StartedAt:   t.startedAt,
		EndedAt:     ended,
		DurationMS:  int(ended.Sub(t.startedAt) / time.Millisecond),
		TaskSummary: t.trigger.Summary,
		PlanSummary: planSummary(res),
		// ReviewOutcome is the reviewer's decision, which is the turn's
		// decision except where a guard ended it first — so it is read off
		// the result rather than off the last review, and the two differ
		// exactly when the engine overrode the reviewer.
		ReviewOutcome: decision,
		Iterations:    res.Rounds,
		// EVERYTHING THE REFLECT DISPATCHER GATES ON. Its workers run on
		// whichever node wins the delivery, which is rarely this one, so
		// a fact left off this payload is a fact no worker can consult —
		// and the gates fail OPEN-LOOKING: an absent tool sequence reads
		// as "the agent engaged with nothing", which silently skips every
		// worker on exactly the successful turns worth learning from.
		ToolSequence: spend.ExecuteTools,
		AllToolNames: spend.AllTools,
		Outcome:      spend.Outcome,
		// SKIP OR NOTHING. The field's only surviving reader gates on
		// PlanDecisionSkip, and a turn that skipped is exactly the one
		// the loop reports as phase.Skipped — so it is derived from the
		// turn's own decision rather than from anything a model wrote.
		PlanDecision:    skipDecision(decision),
		SkillsUsed:      t.skills,
		Interactions:    t.interactions,
		ConversationKey: t.convKey,
	}, t.trace), t.role)
}

// publishFailure publishes the DEDICATED record of why a turn stopped.
//
// [turn.Breach]'s own doc says it is "returned on the result rather than
// published from inside the loop … carried on the breach the caller
// publishes", and [toolloop.BudgetError]'s says "a phase that stopped because
// the company ran out of tokens is a different event from one whose provider
// failed, and they are reported differently". This frame is that caller, and
// for a long time it published neither: the reason was folded into
// agent_turn_completed's error/error_kind and the three dedicated types were
// registered, categorised and documented with no producer.
//
// What that cost is a whole dashboard state. `afk` is derived from exactly
// these three types (internal/api/livestate: llm_unavailable,
// turn.guard_breach, budget_exhausted) and from nothing else, so no seat could
// ever reach it — the attention queue's "the engine stopped it" row, the seat
// screen's AFK banner and the `broken` rail were all unreachable branches.
//
// The summary event still carries error/error_kind, and that is not a second
// copy to keep in step: it is the ONE-LINE reason on a row about the turn,
// where these are the failure itself, with the chain that was tried, the
// ceiling that refused and the guard that fired.
func (e *Engine) publishFailure(ctx context.Context, t turnTelemetry,
	res turn.Result, err error,
) {
	// A breach and an error are not exclusive: an unhandled exception is
	// both, and reporting only one would drop the guard that named it.
	if res.Breach != nil {
		e.publishEvent(ctx, events.New(types.TurnGuardBreach{
			Agent:    t.agentID,
			RoleName: t.role,
			Kind:     res.Breach.Kind,
			Detail:   events.ClipDiagnostic(res.Breach.Detail),
			TurnID:   t.runID,
			WorkKey:  t.workKey,
		}, t.trace), t.role)
	}
	if err == nil {
		return
	}

	// BUDGET BEFORE PROVIDER. A refused charge is reported by the phase as
	// its own error and never reaches a provider at all, so the two are
	// disjoint in practice — but ordering them makes that a property of this
	// function rather than of whichever wrapper happened to be outermost.
	var budget *toolloop.BudgetError
	if errors.As(err, &budget) {
		e.publishEvent(ctx, events.New(types.BudgetExhausted{
			Agent:      t.agentID,
			RoleName:   t.role,
			TurnID:     t.runID,
			WorkKey:    t.workKey,
			BudgetType: types.BudgetScope(budget.Scope),
			UsedTokens: budget.Used,
			MaxTokens:  budget.Limit,
		}, t.trace), t.role)
		return
	}

	// EVERY MEMBER FAILED RETRYABLY — the seat has no model left and is
	// effectively AFK. A non-retryable failure from one member is not this:
	// it comes back as that backend's own error, the chain never wrapped it,
	// and calling it "unavailable" would blame a chain that was never walked.
	var exhausted *chain.Error
	if errors.As(err, &exhausted) {
		e.publishEvent(ctx, events.New(types.LLMUnavailable{
			Agent:         t.agentID,
			RoleName:      t.role,
			ProviderChain: exhausted.Attempted,
			AttemptCount:  len(exhausted.Attempted),
			LastErrorKind: llm.KindOf(exhausted.Err).String(),
			LastError:     events.ClipDiagnostic(exhausted.Error()),
			TurnID:        t.runID,
			WorkKey:       t.workKey,
		}, t.trace), t.role)
	}
}

// lastModel names the model that served the last phase to run.
//
// Read backwards through the loop's order rather than tracked separately: a
// turn stopped by a guard mid-executor has no review model, and one that never
// reached a provider has neither. Tracking "the last one" as its own field
// would be a third copy of a fact these two already carry.
func lastModel(s runner.Spend) string {
	for _, m := range []string{s.ReviewModel, s.ExecuteModel} {
		if m != "" {
			return m
		}
	}
	return ""
}

// planSummary is the reviewer's account of the turn, or the artifact.
//
// The learning subsystem reads this to build an episode. The LAST review's
// completed-work prose is the better source where it exists — it is the
// semantic layer over the engine-built call ledger — and the artifact is what
// a turn that never reached Review has instead.
func planSummary(res turn.Result) string {
	if res.LastReview != nil && res.LastReview.CompletedWork != "" {
		return res.LastReview.CompletedWork
	}
	return res.Artifact
}

// publishEvent sends one turn-level event, or logs why it could not.
func (e *Engine) publishEvent(ctx context.Context, ev *events.Event, role string) {
	// The envelope's source is the seat, for the same reason the phase
	// events set it: a consumer with no other attribution renders an
	// unsourced event as "system".
	ev.Source = role
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "turn_telemetry_publish_failed", "type", ev.Type,
			"role", role, "error", err)
	}
}

// describeResume is describeTurn for a re-entry.
//
// The trace comes from the SUSPENDED TURN's row rather than from the event
// that woke this one, so the resumed phases join the span the original wake
// started. The completion event that triggered the resume already carries that
// trace, but a clarification ANSWER does not — it is an ordinary inbound on
// the conversation, and taking its trace would file the second half of a turn
// under a different root from its first half.
func (e *Engine) describeResume(ctx context.Context, company *Company, in resumeInput) turnTelemetry {
	t := turnTelemetry{
		handle: in.Run.AgentHandle,
		// THE SAME RUN, RESUMED — never a fresh one. A suspended executor
		// is re-entered mid-round with the conversation it parked, so its
		// remaining phases belong to the run that started them; minting a
		// second id here would split one turn across two on every screen.
		// The work key rides the row for the same reason its reply does:
		// the resume sees no trigger and could not re-derive it — and so
		// does the instant it began, without which the resumed half of the
		// turn would derive different operation ids from the first half.
		runID:     in.Run.TurnID,
		workKey:   in.Run.UnitOfWork(),
		workSince: in.Run.WorkSince,
		// AND EACH CONVERSATION VALUE FROM ITS OWN FIELD: the resumed
		// turn's events are tagged with the conversation it reports back
		// to and is answered on, while the partition it was launched from
		// is carried forward so a run that suspends AGAIN writes the same
		// pair a first launch would.
		//
		// PartitionKey rather than ConversationKey for the second one,
		// which is the whole of it: the row's ConversationKey is the
		// IDENTITY — that is the field the split moved the name onto —
		// so reading it here collapsed the pair the moment a run parked
		// twice. A re-parked row then held the bare DM channel where its
		// first launch held the thread, and [sandbox.ConversationRef.Best]
		// lost the one fact that tells two questions on one direct
		// message apart: with no partition to agree with, both rows fall
		// through to recency and the reply to the question in one thread
		// resumes the run waiting in the other.
		convKey:   in.Run.Conversation(),
		partKey:   in.Run.PartitionKey,
		startedAt: time.Now().UTC(),
		role:      in.Run.Role,
		agentID:   in.Run.AgentID,
		// The resumed turn's OWN span, opened by resumeTurn under the
		// reconstructed suspended one. This used to be built by hand as
		// `{TraceID: run.TraceID, ParentSpanID: run.SpanID}` with SpanID
		// left EMPTY, so every event the second half of a turn published
		// carried span_id="" — unplaceable in the dashboard's tree and
		// indistinguishable from every other resumed turn.
		trace: tracing.TraceOf(ctx),
	}
	if in.Trigger != nil {
		t.trigger = types.DescribeTrigger(in.Trigger)
	}
	// Re-derived from the org when the row predates a rename, so a resumed
	// turn is still attributed to a seat that exists.
	if role, agentID := seatIdentity(company, in.Run.AgentHandle); role != "" {
		t.role = role
		if agentID != "" {
			t.agentID = agentID
		}
	}
	return t
}

// seatIdentity is the role name and agent id a seat's events are addressed
// to, or two empty strings for a handle this company does not name.
//
// ONE DERIVATION for every frame that addresses a seat-level event: the
// turn's own telemetry, the resumed turn's, and the guard breach a panic
// outside either publishes. Written out at each, the three would have to
// agree about a human seat (no agent id) and an unknown handle (no role)
// without anything checking that they do.
//
// Nil-safe on the company, because the panic path can run on a node that has
// no epoch yet.
func seatIdentity(company *Company, handle string) (role, agentID string) {
	if company == nil || company.Org == nil {
		return "", ""
	}
	seat := company.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return "", ""
	}
	if id, ok := company.Org.AgentIDFor(seat); ok {
		agentID = id.String()
	}
	return seat.Name, agentID
}

// skipDecision maps the turn's decision onto the one plan_decision value
// anything still reads.
//
// A turn that decided nobody was asking is [types.PlanDecisionSkip]; every
// other turn writes the empty string, which is what the field already meant
// for a turn that produced no artifact. The learning gate reads exactly
// one value, so writing a richer vocabulary here would be inventing consumers.
func skipDecision(decision string) types.PlanDecision {
	if decision == string(phase.Skipped) {
		return types.PlanDecisionSkip
	}
	return ""
}
