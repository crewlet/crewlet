package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
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
//   - turn_completed is the LEARNING subsystem's Plan/Execute/Review-shaped
//     record — the same turn, described for a different consumer. One event
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
	handle    string
	role      string
	agentID   string
	trigger   types.Trigger
	convKey   string
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

// describeTurn assembles the identity for one dispatch.
//
// The trigger is taken from the FIRST event of the partition. A coalesced
// partition has several, and the first is the one whose thread the turn is
// answering; branding the turn with the last would attribute it to whichever
// message happened to arrive while the seat was busy.
func (e *Engine) describeTurn(ctx context.Context, company *Company, req Request) turnTelemetry {
	t := turnTelemetry{
		handle:    req.Handle,
		convKey:   req.ConversationKey,
		startedAt: time.Now().UTC(),
	}
	if role := company.Org.AgentSeatByHandle(req.Handle); role != nil {
		t.role = role.Name
		if id, ok := company.Org.AgentIDFor(role); ok {
			t.agentID = id.String()
		}
	}
	t.interactions = e.interactionsOf(req.Events)
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
func (t turnTelemetry) runnerTurn(company *Company, workKey string, depth int, chain []string,
	task string, reply turn.Reply,
) runner.Turn {
	return runner.Turn{
		ID: workKey, AgentID: t.agentID, Trigger: t.trigger,
		ConversationKey: t.convKey, Trace: t.trace,
		Context: &turnctx.Turn{
			ID: workKey,
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
			// here, and the resumed turn reports back from the row.
			ConversationKey: t.convKey,
			// The brief and the delivery obligation, carried for the
			// same reason: a resumed turn sees neither its trigger nor
			// this frame, so both have to reach the row from here.
			Task:  task,
			Reply: string(reply),
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
	workKey string, spend runner.Spend, res turn.Result, err error,
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
		// legitimately have run three phases on three models.
		Model:          lastModel(spend),
		Trigger:        t.trigger,
		Prompt:         t.trigger.Summary,
		Response:       res.Artifact,
		InputTokens:    spend.InputTokens,
		OutputTokens:   spend.OutputTokens,
		TotalTokens:    spend.Total(),
		ToolExecutions: spend.ToolExecutions,
		TurnID:         workKey,
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
	switch {
	case err != nil:
		summary.Error = truncate(err.Error(), maxTurnErrorLength)
		summary.ErrorKind = "error"
	case res.Breach != nil:
		// A guard breach is not an error — the turn ran and was stopped by
		// a rule. Naming the RULE is the whole value: "depth" and "stall"
		// send an operator to different places, and a bare "failed" sends
		// them to neither.
		summary.Error = truncate(res.Breach.Detail, maxTurnErrorLength)
		summary.ErrorKind = string(res.Breach.Kind)
	}
	e.publishEvent(ctx, events.New(summary, t.trace), t.role)

	e.publishEvent(ctx, events.New(types.TurnCompleted{
		Agent:       t.agentID,
		AgentHandle: t.handle,
		RoleName:    t.role,
		TurnID:      workKey,
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

// maxTurnErrorLength caps the failure text on a turn event, for the same
// reason the phase event caps its own: a wrapped provider failure can carry
// every attempt's body, and this event fans out to every open dashboard.
const maxTurnErrorLength = 2000

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

// truncate caps a string at n bytes, on a rune boundary. Cutting a multi-byte
// rune in half produces invalid UTF-8, which JSON encoders replace with U+FFFD
// — turning one over-long error into a garbled one.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
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
		handle:    in.Run.AgentHandle,
		convKey:   in.Run.ConversationKey,
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
	if role := company.Org.AgentSeatByHandle(in.Run.AgentHandle); role != nil {
		t.role = role.Name
		if id, ok := company.Org.AgentIDFor(role); ok {
			t.agentID = id.String()
		}
	}
	return t
}

// skipDecision maps the turn's decision onto the one plan_decision value
// anything still reads.
//
// A turn that decided nobody was asking is [types.PlanDecisionSkip]; every
// other turn writes the empty string, which is what the field already meant
// for a turn that produced no plan artifact. The learning gate reads exactly
// one value, so writing a richer vocabulary here would be inventing consumers.
func skipDecision(decision string) types.PlanDecision {
	if decision == string(phase.Skipped) {
		return types.PlanDecisionSkip
	}
	return ""
}
