package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
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
}

// describeTurn assembles the identity for one dispatch.
//
// The trigger is taken from the FIRST event of the partition. A coalesced
// partition has several, and the first is the one whose thread the turn is
// answering; branding the turn with the last would attribute it to whichever
// message happened to arrive while the seat was busy.
func (e *Engine) describeTurn(company *Company, req Request) turnTelemetry {
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
	for _, ev := range req.Events {
		if ev == nil {
			continue
		}
		t.trigger = types.DescribeTrigger(ev)
		// The trace context is INHERITED from the trigger, so every event
		// this turn emits joins the span the wake started. Minting a fresh
		// one here would break the chain at exactly the join a reader
		// follows to get from "a message arrived" to "here is what it did".
		t.trace = events.TraceContext{
			TraceID: ev.TraceID, SpanID: ev.SpanID, ParentSpanID: ev.ParentSpanID,
		}
		break
	}
	return t
}

// runnerTurn is the identity handed to the phase runner.
func (t turnTelemetry) runnerTurn(workKey string) runner.Turn {
	return runner.Turn{
		ID: workKey, AgentID: t.agentID, Trigger: t.trigger,
		ConversationKey: t.convKey, Trace: t.trace,
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
		Model:           lastModel(spend),
		Trigger:         t.trigger,
		Prompt:          t.trigger.Summary,
		Response:        res.Artifact,
		InputTokens:     spend.InputTokens,
		OutputTokens:    spend.OutputTokens,
		TotalTokens:     spend.Total(),
		ToolExecutions:  spend.ToolExecutions,
		TurnID:          workKey,
		PlanModel:       spend.PlanModel,
		ExecuteModel:    spend.ExecuteModel,
		ReviewModel:     spend.ReviewModel,
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
		ReviewOutcome:   decision,
		Iterations:      res.Rounds,
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
// turn that ended in Plan (a skip) has no Execute or Review model, and one
// stopped by a guard mid-Execute has no Review model. Tracking "the last one"
// as its own field would be a fourth copy of a fact these three already carry.
func lastModel(s runner.Spend) string {
	for _, m := range []string{s.ReviewModel, s.ExecuteModel, s.PlanModel} {
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
		log.Warn("turn_telemetry_publish_failed", "type", ev.Type,
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
