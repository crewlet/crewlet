package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/live"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The reasons a turn's context ends, read back with context.Cause.
var (
	errStopped     = errors.New("assistant: the person stopped the turn")
	errTurnTimeout = errors.New("assistant: the turn ran past its time")
	errDraining    = errors.New("assistant: this node is shutting down")
	errGrantEnding = errors.New("assistant: the turn's grant is about to end")
)

// turn is one segment of one turn, running on this node.
type turn struct {
	svc          *Service
	person       iam.Principal
	by           iam.Actor
	conversation uuid.UUID
	id           uuid.UUID
	// key is the operation every record of this segment is published under,
	// or derived, for a segment no request's key names (one an approval
	// resumed), the operation derived from what it is about.
	key      string
	derived  *Derivation
	log      *live.Log
	read     exec.Grant
	page     *wire.PageContext
	acts     []string
	boundary provenance.Boundary
	started  time.Time

	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}

	mu sync.Mutex
	// steps, cards and messages count what the turn has written across its
	// segments, so a resumed segment's ids continue its first one's.
	steps, cards, messages int
	// answer is this segment's answer message; streamed is its committed
	// rounds' prose, and drawn is the whole text its deltas have drawn on
	// the person's screen, a round being written included.
	answer uuid.UUID
	// anchored says a card of this segment hangs on its answer, which is
	// then recorded whether or not any prose was written, so every card's
	// message is in the transcript a reload reads.
	anchored bool
	streamed []string
	drawn    string
	audit    []types.AssistantAuditStep
	shown    bool
	// outside is the source of the last answer this turn read that somebody
	// the company cannot vouch for wrote, named on any proposal it makes
	// after (D25).
	outside  provenance.Source
	model    string
	provider string
}

// write is the record caller for this segment, with no step yet.
func (t *turn) write() Write {
	return Write{Owner: t.person.ID, By: t.by, Key: t.key, Derived: t.derived}
}

// emit appends one frame to the turn's log. A frame the log refuses is a
// fault in the code that built it, logged, and never sent half-formed.
func (t *turn) emit(f wire.Frame) {
	if _, err := t.log.Append(f); err != nil && !errors.Is(err, live.ErrEnded) {
		t.svc.log.Error("assistant_frame_refused", "turn", t.id, "kind", string(f.Kind),
			"error", err)
	}
}

// firstOutput records how long the person waited for the first thing the
// drawer shows: a step or a word.
func (t *turn) firstOutput() {
	t.mu.Lock()
	shown := t.shown
	t.shown = true
	t.mu.Unlock()
	if !shown {
		t.svc.metrics.Observe(metrics.AssistantFirstOutputLatency, t.svc.now().Sub(t.started), nil)
	}
}

// tainted notes a read whose answer somebody outside the company wrote.
func (t *turn) tainted(tool catalog.Tool) {
	if !tool.Fence || !outsideTrusts[tool.Trust] {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.outside = tool.Source
}

// taint is the outside source a proposal made now names, "" for none.
func (t *turn) taint() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.outside)
}

// stepStart opens one tool step: numbered, labelled from the call's safe
// fields by its descriptor, and shown as running.
func (t *turn) stepStart(tool catalog.Tool, args map[string]any, req *wire.StepRequest) stepClock {
	t.mu.Lock()
	t.steps++
	index := t.steps
	t.mu.Unlock()
	label := oneLine(redact.Secrets(tool.StepLabel(args)))
	step := stepClock{started: t.svc.now(), StepEvent: wire.StepEvent{
		StepID: assistantdomain.StepID(t.id, index), TurnID: t.id, Index: index, Tool: tool.Name,
		Label: label, Request: req, State: wire.StepRunning,
	}}
	step.StartedAt = step.started.UTC()
	t.firstOutput()
	t.emit(wire.Frame{Kind: wire.EventStatus, Status: &wire.StatusEvent{
		State: wire.StatusWorking, StepsDone: index - 1}})
	ev := step.StepEvent
	t.emit(wire.Frame{Kind: wire.EventStep, Step: &ev})
	return step
}

// stepEnd closes a step in its final state: shown, recorded in the
// transcript, counted and kept for the turn's audit record. status and bytes
// are the route's answer, zero for a step that sent nothing.
func (t *turn) stepEnd(ctx context.Context, step stepClock, state wire.StepState, summary string,
	refusal *wire.Refusal, status, bytes int) {

	ev := step.StepEvent
	ev.State, ev.Summary, ev.Refusal = state, summary, refusal
	ev.DurationMS = t.svc.now().Sub(step.started).Milliseconds()
	t.emit(wire.Frame{Kind: wire.EventStep, Step: &ev})
	t.emit(wire.Frame{Kind: wire.EventStatus, Status: &wire.StatusEvent{
		State: wire.StatusThinking, StepsDone: ev.Index}})
	t.record(ctx, "step", func(ctx context.Context, w Write) error {
		return t.svc.records.RecordStep(ctx, w.Step("step", strconv.Itoa(ev.Index)), t.id, ev)
	})
	t.svc.metrics.Add(metrics.AssistantSteps, 1, metrics.Attrs{"state": string(state)})
	audit := types.AssistantAuditStep{Tool: ev.Tool, Outcome: string(state), Status: status,
		Bytes: bytes}
	if ev.Request != nil {
		audit.Method, audit.Route = ev.Request.Method, ev.Request.Route
	}
	if refusal != nil {
		audit.Refusal = refusal.Code
	}
	t.mu.Lock()
	t.audit = append(t.audit, audit)
	t.mu.Unlock()
}

// emitCard shows one card with the segment's answer and records it with the
// apps a reader must hold to be shown it again.
func (t *turn) emitCard(ctx context.Context, card wire.Card, apps []string) {
	if err := card.Validate(); err != nil {
		t.svc.log.ErrorContext(ctx, "assistant_card_refused", "turn", t.id,
			"type", string(card.Type), "error", err)
		return
	}
	t.mu.Lock()
	t.cards++
	n := t.cards
	message := t.answer
	t.anchored = true
	t.mu.Unlock()
	id := assistantdomain.CardID(t.id, n)
	if card.Type == wire.CardTable && card.Table != nil {
		// EVERY TABLE IS DOWNLOADABLE, from here alone: the card's id is
		// minted here, and the export routes read this stored card again as
		// its owner, with every column the drawer leaves out.
		table := *card.Table
		table.Exports = wire.ExportsOf(id)
		card.Table = &table
	}
	ev := wire.CardEvent{CardID: id, MessageID: message, Card: card}
	t.emit(wire.Frame{Kind: wire.EventCard, Card: &ev})
	t.record(ctx, "card", func(ctx context.Context, w Write) error {
		return t.svc.records.RecordCard(ctx, w.Step("card", strconv.Itoa(n)), t.id, ev, apps)
	})
}

// record writes one transcript record on a context that outlives the turn's
// own, since what it records already happened. A record that fails is logged
// and never fails the turn: the person saw it live, and only a reload will
// miss it.
func (t *turn) record(ctx context.Context, what string, write func(context.Context, Write) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := write(ctx, t.write()); err != nil {
		t.svc.log.WarnContext(ctx, "assistant_record_failed", "turn", t.id, "record", what,
			"error", redact.Secrets(err.Error()),
			"detail", "the person saw it live; the transcript a reload reads is missing it")
	}
}

// recordTimeout bounds one transcript record: the state log's own resolve
// budget for a write, and well inside a turn.
const recordTimeout = 10 * time.Second

// onProgress draws the segment's answer as the loop writes it: every committed
// round's prose, and while a round is being written, as much of it as is
// settled ([settledView]).
//
// ONE RULE FOR EVERY CHANGE ([llm.Reconcile]): the text the screen should hold
// is computed whole from the snapshot, and the frame sent is the tail that
// takes the drawn text there, or a restart carrying the whole of it where the
// drawn text is no prefix of it. A round's partial grows by tails; a provider
// failover or a backend's retraction empties it, and the screen is redrawn
// rather than left holding text the answer does not contain; a committed round
// replaces its partial, whose settled view is a prefix of it, by its tail.
func (t *turn) onProgress(res toolloop.Result) {
	t.mu.Lock()
	for i := len(t.streamed); i < len(res.Narration); i++ {
		t.streamed = append(t.streamed, strings.TrimSpace(redact.Secrets(res.Narration[i].Content)))
	}
	want := joinAnswer(t.streamed, settledView(res.Partial))
	tail, restart := llm.Reconcile(t.drawn, want)
	t.drawn = want
	answer := t.answer
	if res.Model != "" {
		t.model = res.Model
	}
	if res.ProviderKey != "" {
		t.provider = res.ProviderKey
	}
	t.mu.Unlock()
	if tail == "" && !restart {
		return
	}
	t.firstOutput()
	t.emit(wire.Frame{Kind: wire.EventDelta, Delta: &wire.DeltaEvent{
		MessageID: answer, Text: tail, Restart: restart}})
}

// settledView is the part of a round being written that may be shown now: the
// prefix [redact.Settled] says no later text can change the redaction of,
// redacted and trimmed as the round's narration will be. So a credential is
// never on screen before the end that would redact it arrives, and the view is
// always a prefix of the prose the round commits.
func settledView(p *toolloop.Partial) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(redact.Secrets(p.Content[:redact.Settled(p.Content)]))
}

// joinAnswer is a segment's answer: its rounds' prose, a paragraph apart,
// with live (the round being written) last.
func joinAnswer(rounds []string, live string) string {
	parts := make([]string, 0, len(rounds)+1)
	for _, p := range rounds {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if live != "" {
		parts = append(parts, live)
	}
	return strings.Join(parts, "\n\n")
}

// answerText is the segment's answer as the person was shown it once its
// rounds committed.
func (t *turn) answerText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return joinAnswer(t.streamed, "")
}

// fence is checked by the loop before every round and every call: a stopped
// turn, a draining node and a grant about to end each stop it before anything
// more is sent.
func (t *turn) fence() error {
	if cause := context.Cause(t.ctx); cause != nil {
		return cause
	}
	if t.read != nil && t.read.Expires().Sub(t.svc.now()) < GrantMargin {
		return errGrantEnding
	}
	return nil
}

// stop ends the turn as the person asked.
func (t *turn) stop() { t.cancel(errStopped) }

// loopRun is what a segment runs: its starting conversation and what it runs
// on.
type loopRun struct {
	messages []llm.Message
	provider llm.Provider
	key      string
	meter    toolloop.BudgetMeter
	tools    []catalog.Tool
	steps    int
	timeout  time.Duration
	// fresh is a first segment, whose conversation may be titled after it.
	fresh    bool
	question string
}

// run drives one segment to its end, on its own goroutine.
func (t *turn) run(r loopRun) {
	defer close(t.done)
	defer t.svc.release(t)
	ctx, cancel := context.WithTimeoutCause(t.ctx, r.timeout, errTurnTimeout)
	defer cancel()
	ctx = llm.WithSeat(ctx, assistantHome(t.person.ID))
	ctx, span := tracing.Start(ctx, "assistant", "assistant.turn",
		attribute.String("conversation_id", t.conversation.String()),
		attribute.String("turn_id", t.id.String()),
		attribute.String("prompt_version", PromptVersion))
	defer span.End()

	surface := newSurface(t, r.tools)
	progress := &toolloop.Progress{}
	res, err := toolloop.Run(ctx, toolloop.Config{
		Provider: r.provider, ProviderKey: r.key, Surface: surface, Messages: r.messages,
		MaxRounds: r.steps + 1, AllowSuspend: true, Budget: r.meter,
		Fence: t.fence, OnProgress: t.onProgress, Progress: progress,
		// STREAMED, so a round's prose reaches the drawer while it is
		// written, through the CLI's token stream where it has one. A CLI
		// that cannot stream never sends a fragment, and its round arrives
		// whole when it commits, as before.
		StreamPartials: true, StreamTokens: true,
		// The assistant's prompt is its whole instruction: it replaces a
		// CLI's own coding-agent prompt rather than running under it.
		OwnPrompt: true,
	})
	if res == nil {
		snap := progress.Snapshot()
		res = &snap
	}
	t.onProgress(*res)
	end := t.conclude(ctx, res, err)
	span.SetAttributes(attribute.String("outcome", string(end.State)),
		attribute.Int("steps", end.Steps))
	if end.State == wire.TurnFailed {
		tracing.Fail(span, err)
	}
	if r.fresh && end.State == wire.TurnCompleted {
		t.svc.title(ctx, t, r.question, t.answerText(), r.meter)
	}
}

// conclude ends the segment: it records the answer, parks or closes the turn,
// tells the stream how it ended, and leaves its audit record.
func (t *turn) conclude(ctx context.Context, res *toolloop.Result, err error) assistantdomain.TurnEnd {
	text := t.answerText()
	if len(text) > MessageMaxBytes {
		text = textcut.Within(text, MessageMaxBytes)
	}
	state, failure := t.outcome(ctx, res, err)
	end := assistantdomain.TurnEnd{State: state, Steps: t.stepCount(),
		InputTokens: int64(res.InputTokens), OutputTokens: int64(res.OutputTokens),
		CacheRead: int64(res.CacheRead)}
	if failure != nil {
		end.ErrorCode = string(failure.Code)
	}

	var answer *uuid.UUID
	t.mu.Lock()
	id, anchored := t.answer, t.anchored
	t.mu.Unlock()
	if text != "" || anchored {
		answer = &id
		t.record(ctx, "answer", func(ctx context.Context, w Write) error {
			return t.svc.records.RecordMessage(ctx, w.Step("message", "answer"), t.id,
				assistantdomain.NewMessage{ID: id, Role: wire.RoleAssistant, Text: text})
		})
	}

	if state == wire.TurnWaitingForApproval {
		if parkErr := t.park(ctx, res); parkErr != nil {
			t.svc.log.ErrorContext(ctx, "assistant_park_failed", "turn", t.id,
				"error", redact.Secrets(parkErr.Error()))
			state, failure = wire.TurnFailed, &wire.ErrorEvent{Code: wire.ErrorInternal,
				Message: "The change could not be held for your approval. Ask again."}
			end.State, end.ErrorCode = state, string(failure.Code)
		}
	}
	settled := true
	if state != wire.TurnWaitingForApproval {
		settled = t.closeOnce(ctx, end)
	} else {
		t.emit(wire.Frame{Kind: wire.EventStatus, Status: &wire.StatusEvent{
			State: wire.StatusWaitingForApproval, StepsDone: end.Steps}})
	}
	if t.read != nil {
		t.svc.revoker.Revoke(t.read.ID())
	}
	if failure != nil {
		t.emit(wire.Frame{Kind: wire.EventError, Error: failure})
	}
	duration := t.svc.now().Sub(t.started)
	t.emit(wire.Frame{Kind: wire.EventDone, Done: &wire.DoneEvent{State: state,
		DurationMS: duration.Milliseconds(), Steps: end.Steps, MessageID: answer,
		TraceID: tracing.TraceOf(ctx).TraceID}})
	t.log.End()
	if !settled {
		// AFTER THE STREAM HAS ITS END: the person is told at once, and
		// the record is retried behind it ([turn.settle]).
		t.settle(ctx, end)
	}

	attrs := metrics.Attrs{"outcome": string(state)}
	t.svc.metrics.Add(metrics.AssistantTurns, 1, attrs)
	t.svc.metrics.Observe(metrics.AssistantTurnDuration, duration, attrs)
	t.publishAudit(ctx, state, end.ErrorCode, duration)
	t.svc.log.InfoContext(ctx, "assistant_turn_ended", "turn", t.id,
		"conversation", t.conversation, "state", string(state), "error_code", end.ErrorCode,
		"steps", end.Steps, "duration_ms", duration.Milliseconds(),
		"input_tokens", end.InputTokens, "output_tokens", end.OutputTokens)
	return end
}

// outcome is how a segment ended, and the turn-level failure the person reads,
// if any.
func (t *turn) outcome(ctx context.Context, res *toolloop.Result, err error) (wire.TurnState, *wire.ErrorEvent) {
	cause := context.Cause(ctx)
	switch {
	case err == nil && res.Suspended:
		return wire.TurnWaitingForApproval, nil
	case err == nil && res.ExhaustedRounds:
		return wire.TurnFailed, &wire.ErrorEvent{Code: wire.ErrorStepLimit,
			Message:   "This question needed more steps than a turn may take. Ask a narrower one.",
			Retryable: true}
	case err == nil:
		return wire.TurnCompleted, nil
	case errors.Is(err, errStopped) || errors.Is(cause, errStopped):
		return wire.TurnStopped, nil
	case errors.Is(err, toolloop.ErrBudgetExhausted):
		failure := &wire.ErrorEvent{Code: wire.ErrorBudgetExhausted,
			Message: "The company's token budget for this window is spent."}
		var budget *toolloop.BudgetError
		if errors.As(err, &budget) {
			if budget.Scope == PersonBudgetScope {
				failure.Message = "Your own assistant token budget for this window is spent."
			}
			if !budget.ResetsAt.IsZero() {
				failure.Hint = "It resets " + budget.ResetsAt.UTC().Format(time.RFC3339) + "."
			}
		}
		return wire.TurnBudgetExhausted, failure
	case errors.Is(err, errGrantEnded) || errors.Is(err, errGrantEnding):
		return wire.TurnFailed, &wire.ErrorEvent{Code: wire.ErrorGrantEnded,
			Message: "Your session ended, so the assistant stopped acting for you.",
			Hint:    "Sign in again, then ask again.", Retryable: true}
	case errors.Is(err, errDraining) || errors.Is(cause, errDraining):
		return wire.TurnInterrupted, &wire.ErrorEvent{Code: wire.ErrorTurnInterrupted,
			Message:   "The node answering you restarted before the turn finished.",
			Retryable: true}
	case errors.Is(cause, errTurnTimeout):
		return wire.TurnFailed, &wire.ErrorEvent{Code: wire.ErrorTurnTimeout,
			Message:   "The turn took longer than it may. Ask again, or ask a narrower question.",
			Retryable: true}
	case isModelFailure(err):
		return wire.TurnFailed, &wire.ErrorEvent{Code: wire.ErrorModelUnavailable,
			Message: "The assistant's model did not answer.", Retryable: true,
			RetryAfterSeconds: modelRetrySeconds}
	}
	t.svc.log.ErrorContext(ctx, "assistant_turn_failed", "turn", t.id,
		"error", redact.Secrets(err.Error()))
	return wire.TurnFailed, &wire.ErrorEvent{Code: wire.ErrorInternal,
		Message: "Something went wrong inside the engine. The reason is in this node's log."}
}

// modelRetrySeconds is how long a person is told to wait after the model chain
// failed: long enough for a rate limit's bench or a CLI's restart.
const modelRetrySeconds = 10

// isModelFailure reports whether err is the provider chain's: every model in
// it failed or sat out.
func isModelFailure(err error) bool {
	var failure *llm.Error
	return errors.As(err, &failure)
}

// stepCount is how many steps the turn has taken.
func (t *turn) stepCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.steps
}

// parked is what a segment that suspended on a proposal keeps of its loop, so
// the approval that resumes it, on any node, continues the same conversation
// with the call answered.
type parked struct {
	V        int           `json:"v"`
	Messages []llm.Message `json:"messages"`
	// PendingCall and PendingTool are the call the loop suspended on,
	// unanswered.
	PendingCall string `json:"pending_call"`
	PendingTool string `json:"pending_tool"`
	// Steps, Cards and Messages are what the turn has written so far, so a
	// resumed segment's ids continue its first one's.
	Steps    int    `json:"steps"`
	Cards    int    `json:"cards"`
	Count    int    `json:"message_count"`
	Boundary string `json:"boundary"`
	// Seq is the newest seq the parking segment told, its closing frames
	// counted ([parkedTail]): what the segment an approval opens continues
	// after where no node of the fleet remembers more.
	Seq uint64 `json:"seq"`
}

// parkedTail is how many frames a segment tells after its park record has
// landed: the waiting status and the done frame ([turn.conclude]).
const parkedTail = 2

// parkedVersion is the parked loop's format. A reshape takes a new number.
const parkedVersion = 1

// park records the suspended loop beside the turn.
func (t *turn) park(ctx context.Context, res *toolloop.Result) error {
	t.mu.Lock()
	keep := parked{V: parkedVersion, Messages: res.Messages, PendingCall: res.PendingToolCallID,
		PendingTool: res.PendingToolName, Steps: t.steps, Cards: t.cards, Count: t.messages,
		Boundary: string(t.boundary), Seq: t.log.Last() + parkedTail}
	t.mu.Unlock()
	raw, err := json.Marshal(keep)
	if err != nil {
		return fmt.Errorf("encode the parked loop: %w", err)
	}
	if len(raw) > ParkedMaxBytes {
		return fmt.Errorf("the parked loop is %d bytes, over the %d a turn may park",
			len(raw), ParkedMaxBytes)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	return t.svc.records.ParkTurn(ctx, t.write().Step("park"), t.id, raw)
}

// publishAudit leaves the segment's runtime audit record: who, which steps on
// which routes and how each ended, never an argument or an answer.
func (t *turn) publishAudit(ctx context.Context, state wire.TurnState, code string,
	duration time.Duration) {

	t.mu.Lock()
	steps := append([]types.AssistantAuditStep(nil), t.audit...)
	model, provider := t.model, t.provider
	t.mu.Unlock()
	data := types.NewAssistantTurnCompleted(types.AssistantTurnCompleted{
		ActorName: t.by.Name, ActorKind: string(t.by.Kind), OperatorID: t.by.OperatorID,
		ConversationID: t.conversation.String(), TurnID: t.id.String(), State: string(state),
		ErrorCode: code, PromptVersion: PromptVersion, Model: model, ProviderKey: provider,
		DurationMS: duration.Milliseconds(), Steps: steps,
	})
	ev := events.NewFrom(data, tracing.TraceOf(ctx))
	ev.Source = types.OperatorSource
	publish, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := t.svc.audit.Publish(publish, topics.Event(ev.Type), ev); err != nil {
		t.svc.log.ErrorContext(ctx, "assistant_audit_not_published", "turn", t.id,
			"error", err, "detail", "the turn ran and its answer was shown; only its "+
				"runtime audit record is missing from this node's event log")
	}
}
