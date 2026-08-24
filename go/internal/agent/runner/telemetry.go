package runner

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The turn engine's telemetry: what a phase tells the rest of the company
// while and after it runs.
//
// This is the ONLY source of everything a dashboard shows about a working
// seat. Nothing else publishes an agent_* event, and the live projection keys
// on exactly these three types — so a phase that runs silently is a phase that
// leaves the seat rendered as idle for its whole duration and leaves no
// durable record that it ever happened.
//
// Three events, and each answers a question the others cannot:
//
//   - agent_phase_started — WHICH phase, now. Published before the first
//     provider call, so a seat that is thinking says so; the completed event
//     is minutes away and carries nothing until it fires.
//   - agent_turn_progress — the round-by-round in-flight view. NOT persisted:
//     it is superseded by the completed event, and keeping every round would
//     make the event log mostly intermediate states of rows it also holds
//     finished. Published TWICE per round — once the model has spoken, before
//     its tools run, and again once they return — so the model's reasoning
//     reaches a screen without waiting on the slowest tool.
//   - agent_phase_completed — the durable record: the prompts verbatim, the
//     response, the tools, the tokens, the decision.
//
// TELEMETRY NEVER FAILS THE WORK. Every publish here is fire-and-log: the
// phase has already run, its deliveries have already fired, and a broker that
// refuses an event must not turn finished work into a failed turn. The same
// rule the sub-agent batch publisher states, for the same reason.

// Turn identifies the turn a runner is running.
//
// Per-turn, like the task and the conversation beside it: a runner is built
// per turn (see Company.RunnerFor), so these are configuration, not state.
type Turn struct {
	// ID is the work key — what identifies the unit of work this turn did.
	// NOT a turn id minted per node: two nodes completing one trigger mint
	// two, and a consumer correlating on it would draw one turn as two.
	ID string

	// AgentID is the seat's derived agent id, so a consumer can resolve the
	// seat without the org.
	AgentID string

	// Trigger is what woke this turn. It rides on EVERY phase event
	// because a live row that has no completed phase yet must still be
	// able to show its source.
	Trigger types.Trigger

	// ConversationKey is which conversation this turn served. It is the
	// only way to ask the store for one thread's phases: the reasoning is
	// durably kept as the <think> prefix of a phase response, and without
	// this it is addressable by agent and time alone.
	ConversationKey string

	// Trace is the span context stamped on every event this turn emits.
	Trace events.TraceContext

	// Context is what the turn IS — the acting seat above all — passed to
	// every tool that asks for it. See internal/agent/turnctx.
	//
	// Here rather than as a second field on Config because the two are one
	// fact: a runner built for a turn has an identity, and splitting it
	// across two config fields is two places for them to disagree about
	// which turn this is.
	Context *turnctx.Turn
}

// emitter publishes one turn's phase telemetry.
//
// A zero publisher is valid and publishes nothing, which is the embedded case
// a test drives directly and the sub-agent case where the parent phase is
// already the visible one. The TALLY still runs: the turn-level event is the
// engine's to publish, and it must be able to report what the turn spent even
// on a node whose phases are silent.
type emitter struct {
	pub   queue.Publisher
	turn  Turn
	role  string
	tally *Spend
}

func (r *Runner) emitter() emitter {
	return emitter{
		pub: r.cfg.Publisher, turn: r.cfg.Turn,
		role: r.cfg.Seat.Role.Name, tally: &r.spend,
	}
}

// Spend is what one turn's phases cost, accumulated as they complete.
//
// It exists so the turn-level event reports the SAME numbers the phase events
// did. The alternative — the engine summing what it can see — is a second
// derivation of one fact, and the two drift the moment a phase is added, a
// rescue fires, or an extension runs the loop twice.
//
// No lock. A turn's phases run in sequence on one goroutine and the engine
// reads this only after turn.Run has returned to it; a sub-agent runs on its
// own Runner and tallies into its own.
type Spend struct {
	// The model that actually served each phase, which is not necessarily
	// the configured one: a fallback chain records who answered.
	PlanModel    string
	ExecuteModel string
	ReviewModel  string

	InputTokens  int
	OutputTokens int

	// Response is the last phase's text — what the turn produced, for the
	// single-phase summary a dashboard shows before anyone expands it.
	Response string

	// ToolExecutions is every call the turn made, in order across phases.
	ToolExecutions []types.ToolExecution

	// PlanTools and ExecuteTools are the tool NAMES, split the way the
	// learning workers reason about them and accumulated differently on
	// purpose.
	//
	// Plan accumulates across self-iterate rounds, because a Plan-phase
	// builtin firing in round 1 is a fact about the whole turn — the
	// reflect dispatcher reads it to see that the agent already wrote its
	// own memory, and a later round that did not call it again does not
	// undo that. Execute keeps only the LAST round: the earlier rounds
	// were re-attempted work the agent itself judged incomplete, and a
	// skill drafted from their calls would be drafted from a sequence the
	// agent then chose not to stand behind.
	PlanTools    []string
	ExecuteTools []string

	// PlanDecision is the planner's verdict — the LAST one, for the same
	// reason: a turn that self-iterated ends on the decision it acted on.
	PlanDecision string
}

// Total is the turn's token count.
func (s Spend) Total() int { return s.InputTokens + s.OutputTokens }

// Spend reports what this turn has cost so far.
func (r *Runner) Spend() Spend { return r.spend }

// record folds one completed phase into the turn's tally.
func (s *Spend) record(rec phaseRecord) {
	switch rec.Phase {
	case phase.Plan:
		s.PlanModel = rec.Result.Model
	case phase.Execute:
		s.ExecuteModel = rec.Result.Model
	case phase.Review:
		s.ReviewModel = rec.Result.Model
	}
	s.InputTokens += rec.Result.InputTokens
	s.OutputTokens += rec.Result.OutputTokens
	if rec.Result.Text != "" {
		s.Response = rec.Result.Text
	}
	s.ToolExecutions = append(s.ToolExecutions, toolExecutions(rec.Result.Executions)...)
	switch rec.Phase {
	case phase.Plan:
		s.PlanTools = append(s.PlanTools, toolNames(rec.Result.Executions)...)
		if rec.Decision != "" {
			s.PlanDecision = rec.Decision
		}
	case phase.Execute:
		// REPLACED, not appended — see the field's own note.
		s.ExecuteTools = toolNames(rec.Result.Executions)
	}
}

// toolNames is the called tools in order, including repeats.
//
// REPEATS KEPT, because the sequence is what skill synthesis clusters on:
// "search, read, read, read, reply" and "search, read, reply" are different
// procedures, and a set would render them identical.
func toolNames(execs []toolloop.Execution) []string {
	if len(execs) == 0 {
		return nil
	}
	out := make([]string, 0, len(execs))
	for _, ex := range execs {
		out = append(out, ex.Name)
	}
	return out
}

// on reports whether anything is listening. Checked by the callers that would
// otherwise do real work — copying a message list, marshalling arguments — to
// build an event nobody receives.
func (e emitter) on() bool { return e.pub != nil }

// started opens a phase, and publishes the opening progress round with it.
//
// The two go together because they answer one question between them: started
// says WHICH phase, and the round with RoundNum -1 carries the prompt the
// phase is working, so the live view can show what the agent was asked while
// it is still answering. Consumers read RoundNum+1 as "rounds so far", which
// is why the sentinel is -1 and not 0 — a 0 would claim a round had finished.
func (e emitter) started(ctx context.Context, ph phase.Phase, iteration int, system, user string) {
	if !e.on() {
		return
	}
	e.publish(ctx, events.New(types.AgentPhaseStarted{
		Agent:     e.turn.AgentID,
		RoleName:  e.role,
		TurnID:    e.turn.ID,
		Iteration: iteration,
		Phase:     types.Phase(ph),
		Trigger:   e.turn.Trigger,
	}, e.turn.Trace))

	e.publish(ctx, events.New(types.AgentTurnProgress{
		Agent:     e.turn.AgentID,
		RoleName:  e.role,
		TurnID:    e.turn.ID,
		Phase:     types.Phase(ph),
		Iteration: iteration,
		Trigger:   e.turn.Trigger,
		Prompt:    user,
		PromptMessages: []types.PromptMessage{
			{Role: string(llm.RoleSystem), Content: system},
			{Role: string(llm.RoleUser), Content: user},
		},
		RoundNum: openingRound,
	}, e.turn.Trace))
}

// openingRound is the RoundNum of the update published before a phase's first
// provider call. See [emitter.started].
const openingRound = -1

// progress publishes one in-flight round.
func (e emitter) progress(ctx context.Context, ph phase.Phase, iteration int, res toolloop.Result) {
	if !e.on() {
		return
	}
	e.publish(ctx, events.New(types.AgentTurnProgress{
		Agent:          e.turn.AgentID,
		RoleName:       e.role,
		TurnID:         e.turn.ID,
		Phase:          types.Phase(ph),
		Iteration:      iteration,
		Model:          res.Model,
		Trigger:        e.turn.Trigger,
		Prompt:         userPrompt(res.Messages),
		PromptMessages: promptMessages(res.Messages),
		Response:       res.Text,
		InputTokens:    res.InputTokens,
		OutputTokens:   res.OutputTokens,
		TotalTokens:    res.InputTokens + res.OutputTokens,
		// RoundsUsed is 1-based and RoundNum is 0-based; see the sentinel
		// above. Subtracting rather than counting separately keeps the two
		// from ever disagreeing about which round this is.
		RoundNum:       res.RoundsUsed - 1,
		ToolExecutions: toolExecutions(res.Executions),
	}, e.turn.Trace))
}

// phaseRecord is what a completed phase reports.
//
// Assembled by each phase rather than by runPhase, because the fields that
// distinguish a phase from a bare loop run — the decision it reached, whether
// its submit tool had to be rescued — are known only after its payload is
// decoded, and runPhase returns before that happens.
type phaseRecord struct {
	Phase     phase.Phase
	Iteration int
	System    string
	User      string
	Result    toolloop.Result
	Exhausted bool

	// Decision is the phase's structured verdict: the plan's decision, the
	// review's. Empty for Execute, which reaches none.
	Decision string

	// Rescued marks a phase whose submit tool never fired, so its payload
	// was synthesised. Plan and Review can rescue; Execute never does.
	Rescued bool

	// Notes is short free text: review's notes, Execute's missing tools.
	Notes string

	// Available is the tools whose schemas were actually passed in the
	// call — what the model could invoke. Catalogue is the prose list
	// offered in the Plan prompt, with no schema, and is Plan's alone.
	Available []string
	Catalogue []string

	// Failed and Err describe a phase that died instead of finishing. The
	// rest of the record is then PARTIAL rather than absent: a phase that
	// raises used to publish nothing at all, leaving a dashboard showing an
	// in-flight call with no response and no reason.
	Failed bool
	Err    error
}

// maxErrorLength caps the error text on a failed phase.
//
// The prompts and the response are deliberately verbatim — this telemetry is
// what shows an operator what the model actually saw — but an error is not
// that. A provider chain that exhausted can carry every attempt's body, and a
// wrapped decode failure can carry the whole undecodable document; both are
// megabytes of one string on an event that fans out to every open dashboard.
// Two thousand characters holds any message written to be read.
const maxErrorLength = 2000

// completed closes a phase.
func (e emitter) completed(ctx context.Context, rec phaseRecord) {
	// Tallied BEFORE the publisher check: see [emitter].
	e.tally.record(rec)
	if !e.on() {
		return
	}
	ev := types.AgentPhaseCompleted{
		Agent:           e.turn.AgentID,
		RoleName:        e.role,
		TurnID:          e.turn.ID,
		Iteration:       rec.Iteration,
		Phase:           types.Phase(rec.Phase),
		Model:           rec.Result.Model,
		Trigger:         e.turn.Trigger,
		SystemPrompt:    rec.System,
		UserPrompt:      rec.User,
		Response:        rec.Result.Text,
		ToolExecutions:  toolExecutions(rec.Result.Executions),
		InputTokens:     rec.Result.InputTokens,
		OutputTokens:    rec.Result.OutputTokens,
		TotalTokens:     rec.Result.InputTokens + rec.Result.OutputTokens,
		RoundsUsed:      rec.Result.RoundsUsed,
		ExhaustedRounds: rec.Exhausted,
		Decision:        rec.Decision,
		RescueFired:     rec.Rescued,
		Notes:           rec.Notes,
		ToolsAvailable:  rec.Available,
		ToolCatalogue:   rec.Catalogue,
		// Set explicitly. BackendNative is the value every consumer reads
		// as "ran here", and it is NOT the zero value — an empty string
		// renders as an unknown backend rather than as the normal one.
		Backend:         types.BackendNative,
		ConversationKey: e.turn.ConversationKey,
		Failed:          rec.Failed,
	}
	if rec.Err != nil {
		ev.Error = truncate(rec.Err.Error(), maxErrorLength)
		ev.ErrorKind = classifyError(rec.Err)
	}
	e.publish(ctx, events.New(ev, e.turn.Trace))
}

// classifyError names a failure's CLASS, for the one-word reason a dashboard
// prints beside a failed phase.
//
// The classified kinds are the ones an operator can act on: rotate a key,
// raise a cap, wait out a provider. Everything else is "error" — deliberately,
// where the Python this replaces used the exception's type name. Go's answer
// to that question is a lie: a wrapped error's type is *fmt.wrapError whatever
// went wrong underneath, so the field would carry the same meaningless token
// for every unclassified failure while looking specific. One honest generic
// beats a specific-looking constant.
func classifyError(err error) string {
	var provider *llm.Error
	switch {
	case errors.As(err, &provider):
		return provider.Kind.String()
	case errors.Is(err, toolloop.ErrBudgetExhausted):
		return "budget_exhausted"
	case errors.Is(err, context.DeadlineExceeded):
		return llm.KindTimeout.String()
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "error"
}

// publish sends one event, or logs why it could not.
//
// The event's SOURCE is the seat's role name. The payloads carry a role of
// their own, but the envelope's source is what an actor-less consumer
// attributes by — without it every phase in the company renders as "system".
func (e emitter) publish(ctx context.Context, ev *events.Event) {
	ev.Source = e.role
	if err := e.pub.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.Warn("phase_telemetry_publish_failed", "type", ev.Type,
			"role", e.role, "turn_id", e.turn.ID, "error", err)
	}
}

// toolExecutions renders the loop's executions in the wire shape consumers
// read: name, arguments, result, success.
//
// An open map rather than a struct, matching types.ToolExecution, and the one
// place in this catalogue that stays loose on purpose — a producer that starts
// recording one more thing must not need every reader recompiled before that
// thing can be seen.
//
// Arguments go out as a JSON STRING. The dashboard accepts either and
// stringifies an object itself, but the string is what survives a round trip
// through a store that keeps the payload as text: a map re-decoded there comes
// back with its key order gone, and the ledger's own elision depends on that
// order (the discriminator is usually the shortest value).
func toolExecutions(execs []toolloop.Execution) []types.ToolExecution {
	if len(execs) == 0 {
		return nil
	}
	out := make([]types.ToolExecution, 0, len(execs))
	for _, ex := range execs {
		row := types.ToolExecution{
			"name":      ex.Name,
			"arguments": encodeArgs(ex.Args),
			"result":    ex.Output,
			"success":   !ex.Failed,
			"round":     ex.Round,
		}
		if ex.Failed {
			// Rendered in place of the result when the call failed, so a
			// consumer showing `result ?? error` has something to show.
			row["error"] = ex.Output
		}
		out = append(out, row)
	}
	return out
}

// encodeArgs renders a call's arguments as JSON text, falling back to nothing
// rather than to a Go-syntax dump: an argument map that will not marshal is
// one no consumer could have parsed either way, and "%v" of it would put an
// unquoted credential-shaped value on a screen that expects JSON.
func encodeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// promptMessages renders the conversation for a live consumer.
//
// The system and user messages ONLY. The assistant turns and the tool results
// are the response, which the same event carries separately, and repeating
// them here would double the size of every round's envelope to say what the
// row already shows.
func promptMessages(msgs []llm.Message) []types.PromptMessage {
	out := make([]types.PromptMessage, 0, 2)
	for _, m := range msgs {
		if m.Role != llm.RoleSystem && m.Role != llm.RoleUser {
			continue
		}
		out = append(out, types.PromptMessage{Role: string(m.Role), Content: m.Content})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// userPrompt is the ask, for the consumers that show one line rather than the
// whole conversation. The FIRST user message: the loop appends corrective and
// extension nudges as later user turns, and the last one is a nudge rather
// than the ask.
func userPrompt(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role == llm.RoleUser {
			return m.Content
		}
	}
	return ""
}

// truncate caps a string at n bytes, on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8StartsRune(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// utf8StartsRune reports whether b can begin a UTF-8 encoding. Cutting a
// multi-byte rune in half produces invalid UTF-8, which JSON encoders replace
// with U+FFFD — turning one over-long error into a garbled one.
func utf8StartsRune(b byte) bool { return b&0xC0 != 0x80 }
