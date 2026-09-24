package engine

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

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
	// workKey is the unit of work, stable across a re-run.
	workKey   string
	agentID   string
	trigger   types.Trigger
	convKey   string
	startedAt time.Time
	trace     events.TraceContext

	// triggeredAt is [turnctx.Turn.TriggeredAt] for this turn: the earliest
	// instant an operation id its writes derive can have been minted.
	triggeredAt time.Time

	// interactions is who spoke to this seat and what they said, one per
	// constituent of the partition. It rides the completed-turn event
	// because the reflect dispatcher is a QUEUE CONSUMER: it can run on a
	// node that never saw the trigger, so an interaction it cannot read
	// off the payload is one it cannot reason about at all.
	interactions []types.InboundInteraction

	// skills is the synthesized-skill ids offered to this turn's prompt.
	// Set after the prefetch, which is the only thing that knows them.
	skills []string

	// log is where this turn's telemetry logs what its events could not
	// carry (see [turnTelemetry.failureTexts]). Nil is the engine's own
	// component logger, which is what every turn runs with. A field rather
	// than the package logger alone for the reason [ReconcilerOptions.Log]
	// gives: a test asserting the line through the process-wide logger would
	// be racing every parallel test that logs.
	log *slog.Logger
}

// logger is where this turn's telemetry logs; see the field.
func (t turnTelemetry) logger() *slog.Logger {
	if t.log != nil {
		return t.log
	}
	return log
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
func newRunID() string { return uuid.NewString() }

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
		convKey:   req.ConversationKey,
		startedAt: time.Now().UTC(),
	}
	// OFF THE PARTITION'S OWN EVENTS, the ones the work key is derived
	// from — never the merged digest a coalesced turn is handed, which is
	// minted fresh on every merge.
	t.triggeredAt = turnctx.TriggerInstant(req.WorkKey, req.Events, t.startedAt)
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
			RunID:   t.runID,
			WorkKey: t.workKey,
			// When the ids those two seed can first have been minted,
			// which is what every write this turn makes is stamped with.
			TriggeredAt: t.triggeredAt,
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
	// Cut ONCE, for every event below that carries one of them, so a text
	// two events share is logged whole once rather than once per event.
	texts := t.failureTexts(ctx, res, err)

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
		// logged and dropped, so an unbounded failure text would cost the
		// operator the whole record rather than its tail. The event
		// carries the head, and this node's log the whole: see
		// [turnTelemetry.failureTexts].
		summary.Error = texts.err
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
		// and round the breach detail does not. Either is the head of what
		// this node's log carries whole ([turnTelemetry.failureTexts]).
		if summary.Error == "" {
			summary.Error = texts.breach
		}
		summary.ErrorKind = string(res.Breach.Kind)
	}
	e.publishEvent(ctx, events.New(summary, t.trace), t.role)
	e.publishFailure(ctx, t, res, err, texts)

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
//
// texts is [turnTelemetry.failureTexts] for this res and err: the texts these
// events carry, cut to what an event can hold, with their whole in this
// node's log.
func (e *Engine) publishFailure(ctx context.Context, t turnTelemetry,
	res turn.Result, err error, texts failureTexts,
) {
	// A breach and an error are not exclusive: an unhandled exception is
	// both, and reporting only one would drop the guard that named it.
	if res.Breach != nil {
		e.publishEvent(ctx, events.New(types.TurnGuardBreach{
			Agent:    t.agentID,
			RoleName: t.role,
			Kind:     res.Breach.Kind,
			// Past the bound, its head, and the whole in this node's log
			// (texts).
			Detail:  texts.breach,
			TurnID:  t.runID,
			WorkKey: t.workKey,
		}, t.trace), t.role)
	}
	if err == nil {
		return
	}

	// BUDGET BEFORE PROVIDER. A budget refusal — a room read that stopped a
	// call before it was sent, or a charge that refused a round after its
	// provider had answered — is a [toolloop.BudgetError] built from the
	// meter's answer alone, so it never wraps a provider's error and the two
	// are disjoint in practice. Ordering them makes that a property of this
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
			// Past the bound, its head, and the whole in this node's log
			// (texts).
			LastError: texts.chain,
			TurnID:    t.runID,
			WorkKey:   t.workKey,
		}, t.trace), t.role)
	}
}

// failureTexts is what a failed turn's events say about why it failed, each
// text as those events carry it: bounded at [events.MaxDiagnosticBytes], so
// that the event carrying it can be published at all.
type failureTexts struct {
	// err is the turn's error, as agent_turn_completed carries it.
	err string
	// breach is the guard breach's detail, as turn.guard_breach carries it
	// and as the summary does for a turn that broke a guard with no error.
	breach string
	// chain is the exhausted provider chain's error, as llm_unavailable
	// carries it.
	chain string
}

// failureTexts cuts the texts a failed turn's events carry, and logs what
// they cannot.
//
// THE EVENTS CARRY THE HEAD, AND THIS NODE'S LOG THE WHOLE. When the bound cuts
// any of them, one line — turn_failure_cut, at WARN — carries the whole of
// every text it cut, beside the run's identity, which is what joins the line
// to the events and is the place the cut's own marker sends a reader. The
// frames that settle a failed turn's delivery log its error in their own ways
// — at INFO for a seat that moved, and from the queue, which does not know the
// run, for a delivery handed back — so the line that is certain to hold a cut
// text's whole, and to name its run, is written here, where the texts are cut.
//
// ONCE PER TEXT: a cut text already inside one the line carries whole is not
// repeated — a panic's breach detail inside a turn error whose text quotes the
// panic, say, or an exhausted chain's error inside one that quotes the chain.
func (t turnTelemetry) failureTexts(ctx context.Context, res turn.Result, err error) failureTexts {
	var (
		out    failureTexts
		logged []string
		attrs  []any
	)
	cut := func(field, text string) string {
		head := events.ClipDiagnostic(text)
		if head == text {
			return text
		}
		for _, whole := range logged {
			if strings.Contains(whole, text) {
				return head
			}
		}
		logged = append(logged, text)
		attrs = append(attrs, field, text, field+"_bytes", len(text))
		return head
	}
	if err != nil {
		out.err = cut("error", err.Error())
	}
	if res.Breach != nil {
		out.breach = cut("breach_detail", res.Breach.Detail)
	}
	var exhausted *chain.Error
	if errors.As(err, &exhausted) {
		out.chain = cut("last_error", exhausted.Error())
	}
	if len(attrs) > 0 {
		t.logger().WarnContext(ctx, "turn_failure_cut", append([]any{
			"turn_id", t.runID, "work_key", t.workKey, "seat", t.handle, "role", t.role,
			"detail", "this turn's failure events carry these texts cut to what one event can " +
				"hold; this line carries them whole",
		}, attrs...)...)
	}
	return out
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
//
// WITHOUT ctx's CANCELLATION, as the runner publishes a phase record
// (runner.emitter.publishPhase): each event here reports work that already
// happened — a turn that ran, what it spent, why it stopped — and the turns
// most in need of one are those whose context ended under them. Releasing a
// seat detaches its mailbox, which cancels the context its running turn was
// handed, and a broker client refuses a publish under a context that is
// already done. A turn's phase records already go out this way; closing
// events that did not would leave a released seat's live row working, with
// nothing saying the turn ended. The values ctx carries, the trace among
// them, are kept.
func (e *Engine) publishEvent(ctx context.Context, ev *events.Event, role string) {
	ctx = context.WithoutCancel(ctx)
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
		// the resume sees no trigger and could not re-derive it.
		runID:     in.Run.TurnID,
		workKey:   in.Run.UnitOfWork(),
		convKey:   in.Run.ConversationKey,
		startedAt: time.Now().UTC(),
		// THE RUN'S FIRST LAUNCH, the earliest instant its row records. It
		// is later than every operation the turn named before it suspended,
		// so it bounds only the ones the resume names afresh; the original
		// turn's own instant, which the row does not carry, bounds all.
		triggeredAt: in.Run.CreatedAt,
		role:        in.Run.Role,
		agentID:     in.Run.AgentID,
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
