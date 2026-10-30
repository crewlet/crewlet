package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/cards"
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
	// finished counts the steps that have ENDED, which is what a status
	// frame's steps_done says. Not the newest step's index: the coding CLI
	// calls tools in parallel, so steps end out of order, and the index of
	// the one that ended last counted backwards (2, then 1) on screen.
	finished int
	// replies are this segment's answer messages, one per round that wrote
	// prose, in order; the last may be a round still being written.
	// narrated is how many of the loop's narrations they hold.
	replies  []*reply
	narrated int
	// anchor is the turn's newest assistant message, the one a card shown
	// now hangs on: uuid.Nil until the turn has one ([turn.emitCard]).
	anchor uuid.UUID
	// apps are the apps the turn has read through, across its segments
	// ([turn.used]).
	apps map[string]struct{}
	// freed says the person's running turn was given back ([turn.freeSlot]).
	freed bool
	// ended is the state the segment ended in, set once its end is told:
	// what a Stop that waited for it answers ([Service.stopRunner]).
	ended wire.TurnState
	audit []types.AssistantAuditStep
	shown bool
	// outside is the source of the last answer this turn read that somebody
	// the company cannot vouch for wrote, named on any proposal it makes
	// after (D25).
	outside  provenance.Source
	model    string
	provider string
	// deep is the turn's deep work (ADR-0054): what this segment hands a
	// run, and the run in flight.
	deep deepState
	// brief is a brief turn's own state ([wire.TurnBrief], brief.go).
	brief briefState
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

// tainted notes a read whose answer, at the trust it was fenced under,
// somebody outside the company wrote.
func (t *turn) tainted(tool catalog.Tool, trust provenance.Trust) {
	if !tool.Fence || !outsideTrust(trust) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.outside = tool.Source
}

// readAnswer notes an answer the turn's model is handed outside a step, a
// resumed segment's decision ([resultOf]): from src, at the trust it is fenced
// at, tainting the turn exactly as a step's read does.
func (t *turn) readAnswer(src provenance.Source, trust provenance.Trust) {
	t.tainted(catalog.Tool{Fence: true, Source: src}, trust)
}

// taintedBy carries the taint the turn's earlier segment parked with
// ([parked.Outside]) onto this one: the loop it continues still holds the
// answer that tainted it, so a proposal it makes names that answer's source as
// the earlier segment's would have.
func (t *turn) taintedBy(source provenance.Source) {
	if source == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.outside = source
}

// taint is the outside source a proposal made now names, "" for none.
func (t *turn) taint() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.outside)
}

// stepStart opens one tool step: numbered, labelled from the call's safe
// fields by its descriptor, the records it names said as o says them (a room
// the call names by its id, [catalog.Tool.Room]), and shown as running. req is
// the request the step shows (the path it was sent to,
// [catalog.Tool.StepRequest]) and pattern the route pattern it matched, which
// is all the turn's audit record keeps of it: the person reads their own
// request, and the company-wide trail never carries a path value (a task's
// key, a page's id).
func (t *turn) stepStart(tool catalog.Tool, args map[string]any, o cards.Options,
	req *wire.StepRequest, pattern string) stepClock {

	t.mu.Lock()
	t.steps++
	index := t.steps
	finished := t.finished
	parent := t.deep.parent
	t.mu.Unlock()
	label := oneLine(redact.Secrets(tool.StepLabel(args, o)))
	step := stepClock{started: t.svc.now(), pattern: pattern, StepEvent: wire.StepEvent{
		StepID: assistantdomain.StepID(t.id, index), TurnID: t.id, Index: index, Tool: tool.Name,
		Label: label, Request: req, State: wire.StepRunning, Parent: parent,
	}}
	step.StartedAt = step.started.UTC()
	t.firstOutput()
	t.emit(wire.Frame{Kind: wire.EventStatus, Status: &wire.StatusEvent{
		State: wire.StatusWorking, StepsDone: finished}})
	ev := step.StepEvent
	t.emit(wire.Frame{Kind: wire.EventStep, Step: &ev})
	return step
}

// stepEnd closes a step in its final state: shown, recorded in the
// transcript, counted and kept for the turn's audit record. status and bytes
// are the route's answer, zero for a step that sent nothing.
//
// A STEP'S REFUSAL IS SHOWN IN THE PERSON'S WORDS ([cards.PersonRefusal]):
// whatever the route or the assistant's own check said is the model's, and
// the caller hands it to the model on its own channel.
func (t *turn) stepEnd(ctx context.Context, step stepClock, state wire.StepState, summary string,
	refusal *wire.Refusal, status, bytes int) {

	ev := step.StepEvent
	ev.State, ev.Summary = state, summary
	if refusal != nil {
		shown := cards.PersonRefusal(*refusal, status)
		ev.Refusal = &shown
	}
	ev.DurationMS = t.svc.now().Sub(step.started).Milliseconds()
	t.emit(wire.Frame{Kind: wire.EventStep, Step: &ev})
	t.mu.Lock()
	t.finished++
	finished := t.finished
	t.mu.Unlock()
	t.emit(wire.Frame{Kind: wire.EventStatus, Status: &wire.StatusEvent{
		State: wire.StatusThinking, StepsDone: finished}})
	t.record(ctx, "step", func(ctx context.Context, w Write) error {
		return t.svc.records.RecordStep(ctx, w.Step("step", strconv.Itoa(ev.Index)), t.id, ev)
	})
	t.svc.metrics.Add(metrics.AssistantSteps, 1, metrics.Attrs{"state": string(state)})
	audit := types.AssistantAuditStep{Tool: ev.Tool, Outcome: string(state), Status: status,
		Bytes: bytes}
	if ev.Request != nil {
		audit.Method, audit.Route = ev.Request.Method, step.pattern
	}
	if refusal != nil {
		audit.Refusal = refusal.Code
	}
	t.mu.Lock()
	t.audit = append(t.audit, audit)
	t.mu.Unlock()
}

// emitCard shows one card under the turn's newest assistant message (the
// round that asked for it, or the last one that wrote prose before it) and
// records it with the apps a reader must hold to be shown it again.
//
// EVERY CARD'S MESSAGE IS IN THE TRANSCRIPT: a turn whose rounds have written
// no prose yet opens an empty answer for its first card to hang on, drawn and
// recorded, so a reload places the card where it was drawn live rather than
// dropping it.
func (t *turn) emitCard(ctx context.Context, card wire.Card, apps []string) {
	if err := card.Validate(); err != nil {
		t.svc.log.ErrorContext(ctx, "assistant_card_refused", "turn", t.id,
			"type", string(card.Type), "error", err)
		return
	}
	t.mu.Lock()
	t.cards++
	n := t.cards
	message, opened := t.anchor, false
	if message == uuid.Nil {
		t.messages++
		message, opened = assistantdomain.MessageID(t.id, t.messages), true
		t.anchor = message
	}
	t.mu.Unlock()
	if opened {
		t.emit(wire.Frame{Kind: wire.EventDelta, Delta: &wire.DeltaEvent{MessageID: message}})
		t.recordReply(ctx, message, "")
	}
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

// reply is one round's answer message as the person was drawn it.
type reply struct {
	round int
	id    uuid.UUID
	// drawn is the text its deltas have drawn on the person's screen, and
	// committed says its round has ended and drawn is its prose.
	drawn     string
	committed bool
}

// onProgress draws the segment's answer as the loop writes it, ONE MESSAGE PER
// ROUND, in order, so each round's prose sits above the cards its calls showed
// and the final round's below them, live and on a reload alike. A committed
// round's prose is recorded as its own message the moment it commits.
//
// ONE RULE FOR EVERY CHANGE ([llm.Reconcile]), per message: the text a round's
// message should hold is computed whole from the snapshot (its committed
// prose, or while it is written as much as is settled, [settledView]), and the
// frame sent is the tail that takes the drawn text there, or a restart
// carrying the whole of it where the drawn text is no prefix of it. A round's
// partial grows by tails; a provider failover or a backend's retraction
// empties it, and the screen is redrawn rather than left holding text the
// answer does not contain; a round that ends with no prose withdraws what it
// drew.
func (t *turn) onProgress(res toolloop.Result) {
	t.mu.Lock()
	if t.brief.on {
		// A BRIEF'S PROSE IS NEVER ITS ANSWER: the brief card is, and
		// whatever the model writes around its calls is neither drawn nor
		// recorded.
		if res.Model != "" {
			t.model = res.Model
		}
		if res.ProviderKey != "" {
			t.provider = res.ProviderKey
		}
		t.mu.Unlock()
		return
	}
	var deltas []wire.DeltaEvent
	var commits []reply
	for ; t.narrated < len(res.Narration); t.narrated++ {
		n := res.Narration[t.narrated]
		r := t.replyLocked(n.Round)
		deltas = r.draw(deltas, strings.TrimSpace(redact.Secrets(n.Content)))
		r.committed = true
		if r.drawn != "" {
			t.anchor = r.id
			commits = append(commits, *r)
		}
	}
	if live := t.liveLocked(); live != nil && (res.Partial == nil || res.Partial.Round != live.round) {
		// THE ROUND ENDED WITH NO PROSE, or the loop moved past it: what it
		// drew is withdrawn, and its message is never recorded.
		deltas = live.draw(deltas, "")
		t.replies = t.replies[:len(t.replies)-1]
	}
	if res.Partial != nil {
		if view := settledView(res.Partial); view != "" || t.liveLocked() != nil {
			deltas = t.replyLocked(res.Partial.Round).draw(deltas, view)
		}
	}
	if res.Model != "" {
		t.model = res.Model
	}
	if res.ProviderKey != "" {
		t.provider = res.ProviderKey
	}
	t.mu.Unlock()
	for _, d := range deltas {
		t.firstOutput()
		t.emit(wire.Frame{Kind: wire.EventDelta, Delta: &d})
	}
	for _, r := range commits {
		t.recordReply(t.ctx, r.id, r.drawn)
	}
}

// replyLocked is the message of round: the round being written, or a new
// message after every one the turn has, under t.mu.
func (t *turn) replyLocked(round int) *reply {
	if live := t.liveLocked(); live != nil && live.round == round {
		return live
	}
	t.messages++
	r := &reply{round: round, id: assistantdomain.MessageID(t.id, t.messages)}
	t.replies = append(t.replies, r)
	return r
}

// liveLocked is the reply of the round being written, or nil, under t.mu.
func (t *turn) liveLocked() *reply {
	if n := len(t.replies); n > 0 && !t.replies[n-1].committed {
		return t.replies[n-1]
	}
	return nil
}

// draw appends the frame that takes r's drawn text to want, if any.
func (r *reply) draw(deltas []wire.DeltaEvent, want string) []wire.DeltaEvent {
	tail, restart := llm.Reconcile(r.drawn, want)
	r.drawn = want
	if tail == "" && !restart {
		return deltas
	}
	return append(deltas, wire.DeltaEvent{MessageID: r.id, Text: tail, Restart: restart})
}

// recordReply records one of the turn's answer messages, cut to
// [MessageMaxBytes].
func (t *turn) recordReply(ctx context.Context, id uuid.UUID, text string) {
	if len(text) > MessageMaxBytes {
		text = textcut.Within(text, MessageMaxBytes)
	}
	apps := t.appsNow()
	t.record(ctx, "answer", func(ctx context.Context, w Write) error {
		return t.svc.records.RecordMessage(ctx, w.Step("message", id.String()), t.id,
			assistantdomain.NewMessage{ID: id, Role: wire.RoleAssistant, Text: text, Apps: apps})
	})
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

// answerText is the segment's committed prose, its rounds a paragraph apart:
// what a new conversation is titled after.
func (t *turn) answerText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	parts := make([]string, 0, len(t.replies))
	for _, r := range t.replies {
		if r.committed && r.drawn != "" {
			parts = append(parts, r.drawn)
		}
	}
	return strings.Join(parts, "\n\n")
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

// freeSlot gives the person's running turn back, once.
func (t *turn) freeSlot() {
	t.mu.Lock()
	freed := t.freed
	t.freed = true
	t.mu.Unlock()
	if !freed {
		t.svc.unadmit(t.person.ID)
	}
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
	// rounds is the loop's whole round budget where the segment sets its
	// own (a brief: its reads, then its answer and retry), and steps and
	// the answer's round where it is zero.
	rounds  int
	timeout time.Duration
	// fresh is a first segment, whose conversation may be titled after it.
	fresh bool
	// terminate are the tools whose success ends the loop: a brief's
	// answer ([toolloop.Config.TerminateAfter]).
	terminate []string
	question  string
	// deep is the deep-work run a resumed segment continues before its
	// loop, the answer to the call it parked on being that run's end.
	deep *parkedDeep
	// decision is the engine's words for the decision a resumed segment
	// answers its parked call with ([resultOf]), and screen the screen the
	// decision was made from, empty when the decision carried none: kept
	// apart, so a deep-work task records the decision alone.
	decision, screen string
}

// maxRounds is the segment's round budget: its own where it sets one, else a
// round for each step and one for the answer.
func (r loopRun) maxRounds() int {
	if r.rounds > 0 {
		return r.rounds
	}
	return r.steps + 1
}

// run drives one segment to its end, on its own goroutine.
func (t *turn) run(r loopRun) {
	defer close(t.done)
	defer t.svc.release(t)
	// THE SEGMENT'S CLOCK, which a deep-work run moves later to cover itself
	// ([segmentClock]); its expiry ends the segment as timed out.
	ctx, cancel := context.WithCancelCause(t.ctx)
	defer cancel(nil)
	clock := startClock(t.svc.now(), r.timeout, func() { cancel(errTurnTimeout) })
	defer clock.stop()
	t.mu.Lock()
	t.deep.clock, t.deep.meter, t.deep.tools = clock, r.meter, r.tools
	t.mu.Unlock()
	ctx = llm.WithSeat(ctx, assistantHome(t.person.ID))
	ctx, span := tracing.Start(ctx, "assistant", "assistant.turn",
		attribute.String("conversation_id", t.conversation.String()),
		attribute.String("turn_id", t.id.String()),
		attribute.String("prompt_version", t.promptVersion()))
	defer span.End()

	if r.deep != nil {
		// A PARKED RUN IS CONTINUED FIRST: its end, not the decision, is
		// the answer to the call the turn parked on.
		answered, parkAgain, err := t.answerParkedDeep(ctx, &r)
		if err != nil || parkAgain {
			end := t.conclude(ctx, answered, err)
			span.SetAttributes(attribute.String("outcome", string(end.State)),
				attribute.Int("steps", end.Steps))
			return
		}
	}
	surface := newSurface(t, r.tools)
	progress := &toolloop.Progress{}
	res, err := toolloop.Run(ctx, toolloop.Config{
		Provider: r.provider, ProviderKey: r.key, Surface: surface, Messages: r.messages,
		Host: t.svc.sessions, ServerTitle: "Ask Crewlet",
		MaxRounds: r.maxRounds(), AllowSuspend: true, Budget: r.meter,
		TerminateAfter: r.terminate,
		Fence:          t.fence, OnProgress: t.onProgress, Progress: progress,
		// STREAMED, so a round's prose reaches the drawer while it is
		// written, through the CLI's token stream where it has one. A CLI
		// that cannot stream never sends a fragment, and its round arrives
		// whole when it commits, as before.
		StreamPartials: true,
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
	if r.fresh && end.State == wire.TurnCompleted && !t.briefing() {
		t.svc.title(ctx, t, r.question, t.answerText(), r.meter)
	}
}

// conclude ends the segment: it records the answer, parks or closes the turn,
// tells the stream how it ended, and leaves its audit record.
func (t *turn) conclude(ctx context.Context, res *toolloop.Result, err error) assistantdomain.TurnEnd {
	state, failure := t.outcome(ctx, res, err)
	end := assistantdomain.TurnEnd{State: state, Steps: t.stepCount(),
		InputTokens: int64(res.InputTokens), OutputTokens: int64(res.OutputTokens),
		CacheRead: int64(res.CacheRead)}
	if failure != nil {
		end.ErrorCode = string(failure.Code)
	}

	// The answer the done frame names is the turn's newest message, every
	// round's prose already recorded as it committed.
	var answer *uuid.UUID
	t.mu.Lock()
	if t.anchor != uuid.Nil {
		id := t.anchor
		answer = &id
	}
	t.mu.Unlock()

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
	// THE PERSON'S RUNNING TURN IS GIVEN BACK BEFORE THE STREAM HEARS THE
	// END: a client that reads done and asks at once is never refused
	// assistant_busy by a runner still tearing down (its audit record, a
	// close it retries).
	t.freeSlot()
	duration := t.svc.now().Sub(t.started)
	t.emit(wire.Frame{Kind: wire.EventDone, Done: &wire.DoneEvent{State: state,
		DurationMS: duration.Milliseconds(), Steps: end.Steps, MessageID: answer,
		TraceID: tracing.TraceOf(ctx).TraceID}})
	t.log.End()
	t.mu.Lock()
	t.ended = state
	t.mu.Unlock()
	if !settled {
		// AFTER THE STREAM HAS ITS END: the person is told at once, and
		// the record is retried behind it ([turn.settle]).
		t.settle(ctx, end)
	}

	attrs := metrics.Attrs{"outcome": string(state)}
	t.svc.metrics.Add(metrics.AssistantTurns, 1, attrs)
	t.svc.metrics.Observe(metrics.AssistantTurnDuration, duration, attrs)
	t.publishAudit(ctx, state, end.ErrorCode, duration)
	ended := []any{"turn", t.id, "conversation", t.conversation, "state", string(state),
		"error_code", end.ErrorCode, "steps", end.Steps, "duration_ms", duration.Milliseconds(),
		"input_tokens", end.InputTokens, "output_tokens", end.OutputTokens}
	if end.ErrorCode == string(wire.ErrorBriefInvalid) && err != nil {
		// A BRIEF THAT COULD NOT BE WRITTEN SAYS WHY where the turn's end is
		// read: the last refusal's reason, or the brief's own cap.
		ended = append(ended, "reason", redact.Secrets(err.Error()))
	}
	t.svc.log.InfoContext(ctx, "assistant_turn_ended", ended...)
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
			Message:   wire.ErrorStepLimit.Sentence(),
			Retryable: true}
	case err == nil:
		return wire.TurnCompleted, nil
	case errors.Is(err, errStopped) || errors.Is(cause, errStopped):
		return wire.TurnStopped, nil
	case errors.Is(err, errBriefInvalid):
		return wire.TurnFailed, &wire.ErrorEvent{Code: wire.ErrorBriefInvalid,
			Message: wire.ErrorBriefInvalid.Sentence(), Retryable: true}
	case errors.Is(err, toolloop.ErrBudgetExhausted):
		failure := &wire.ErrorEvent{Code: wire.ErrorBudgetExhausted,
			Message: wire.ErrorBudgetExhausted.Sentence()}
		var budget *toolloop.BudgetError
		if errors.As(err, &budget) && budget.Scope == BriefBudgetScope {
			// THE BRIEF'S OWN CAP, not a window: nothing the company or the
			// person set is spent, and a narrower question answers.
			return wire.TurnFailed, &wire.ErrorEvent{Code: wire.ErrorBriefInvalid,
				Message: "The brief read more than a brief may. Ask about one thing on " +
					"this page instead.", Retryable: true}
		}
		if errors.As(err, &budget) {
			if budget.Scope == PersonBudgetScope {
				failure.Message = "Your own Ask Crewlet token budget for this window is spent."
			}
			// BOTH WAYS BACK, in a person's words: the window turning over,
			// and the ceiling raised, which the remedy beside it opens
			// (ADR-0060). The reset's instant rides as the wait, never as a
			// timestamp in the sentence a person reads.
			failure.Hint = "It comes back when the window resets, or as soon as the " +
				"ceiling is raised."
			if !budget.ResetsAt.IsZero() {
				failure.RetryAfterSeconds = max(int((budget.ResetsAt.Sub(t.svc.now())+
					time.Second-1)/time.Second), 1)
			}
			if remedy := budgetRemedy(budget.Scope); !remedy.IsZero() {
				failure.Remedy = &remedy
			}
		}
		return wire.TurnBudgetExhausted, failure
	case errors.Is(err, errGrantEnded) || errors.Is(err, errGrantEnding):
		return wire.TurnFailed, &wire.ErrorEvent{Code: wire.ErrorGrantEnded,
			Message: wire.ErrorGrantEnded.Sentence(),
			Hint:    "Sign in again, then ask again.", Retryable: true}
	case errors.Is(err, errDraining) || errors.Is(cause, errDraining):
		return wire.TurnInterrupted, &wire.ErrorEvent{Code: wire.ErrorTurnInterrupted,
			Message:   wire.ErrorTurnInterrupted.Sentence(),
			Retryable: true}
	case errors.Is(cause, errTurnTimeout):
		return wire.TurnFailed, &wire.ErrorEvent{Code: wire.ErrorTurnTimeout,
			Message:   wire.ErrorTurnTimeout.Sentence(),
			Retryable: true}
	}
	if problem, ok := modelProblemOf(err, t.person.Can(iam.GrantAdmin)); ok {
		return wire.TurnFailed, problem.event(t.svc.now())
	}
	t.svc.log.ErrorContext(ctx, "assistant_turn_failed", "turn", t.id,
		"error", redact.Secrets(err.Error()))
	return wire.TurnFailed, &wire.ErrorEvent{Code: wire.ErrorInternal,
		Message: wire.ErrorInternal.Sentence()}
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
	// Anchor is the turn's newest assistant message, which a resumed
	// segment's first cards hang on; empty for none.
	Anchor string `json:"anchor,omitempty"`
	// Apps are the apps the loop read through: a person who no longer
	// holds one is never handed its answers back ([Service.Carries]).
	Apps []string `json:"apps,omitempty"`
	// Seq is the newest seq the parking segment told, its closing frames
	// counted ([parkedTail]): what the segment an approval opens continues
	// after where no node of the fleet remembers more.
	Seq uint64 `json:"seq"`
	// Deep is the deep-work run the turn parked on, continued by the
	// approval that resumes it; absent for a quick-mode proposal.
	Deep *parkedDeep `json:"deep,omitempty"`
	// Page is the page context the segment ran on: what a resumed segment
	// runs on when its decision carried none. A decision that carries one
	// replaces it ([Service.Resume]).
	Page *wire.PageContext `json:"page,omitempty"`
	// Outside is the source of the last answer the segment read that
	// somebody the company cannot vouch for wrote ([turn.taint]), empty for
	// none: the loop a resumed segment continues still holds that answer
	// ([turn.taintedBy]).
	Outside string `json:"outside,omitempty"`
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
		Boundary: string(t.boundary), Seq: t.log.Last() + parkedTail, Deep: t.deep.parked,
		Page: t.page, Outside: string(t.outside)}
	if t.anchor != uuid.Nil {
		keep.Anchor = t.anchor.String()
	}
	for app := range t.apps {
		keep.Apps = append(keep.Apps, app)
	}
	slices.Sort(keep.Apps)
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
	return t.svc.records.ParkTurn(ctx, t.write().Step("park"), t.id, raw, keep.Steps)
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
		ErrorCode: code, PromptVersion: t.promptVersion(), Model: model, ProviderKey: provider,
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
