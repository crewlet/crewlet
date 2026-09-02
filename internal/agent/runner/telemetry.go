package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
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

	// mu guards the delegation counters on tally, which several workers
	// write concurrently. Nil on an emitter that publishes nothing.
	mu *sync.Mutex

	// hostIteration is the Execute round a NESTED phase belongs to, so a
	// dashboard groups a sub-agent under the round that spawned it rather
	// than beside the turn's own phases. Zero on the emitter every
	// ordinary phase uses, which never reads it.
	hostIteration int
}

// nestedAt is this emitter bound to the round a nested phase runs under.
func (e emitter) nestedAt(round int) emitter {
	e.hostIteration = round
	return e
}

func (r *Runner) emitter() emitter {
	return emitter{
		pub: r.cfg.Publisher, turn: r.cfg.Turn,
		role: r.cfg.Seat.Role.Name, tally: &r.spend, mu: &r.mu,
	}
}

// Spend is what one turn's phases cost, accumulated as they complete.
//
// It exists so the turn-level event reports the SAME numbers the phase events
// did. The alternative — the engine summing what it can see — is a second
// derivation of one fact, and the two drift the moment a phase is added, a
// rescue fires, or an extension runs the loop twice.
//
// One lock, and only the delegation counters need it. A turn's phases run in
// sequence on one goroutine and the engine reads this after turn.Run has
// returned, so the per-phase fields need nothing — but WORKERS RUN
// CONCURRENTLY, several of them reporting into the same tally from their own
// goroutines, and an unguarded += there is a data race the detector finds on
// the first fan-out.
type Spend struct {
	// The model that actually served each phase, which is not necessarily
	// the configured one: a fallback chain records who answered.
	ExecuteModel string
	ReviewModel  string

	InputTokens  int
	OutputTokens int

	// Response is the last phase's text — what the turn produced, for the
	// single-phase summary a dashboard shows before anyone expands it.
	Response string

	// ToolExecutions is every call the turn made, in order across phases.
	ToolExecutions []types.ToolExecution

	// ExecuteTools and AllTools are the tool NAMES, split the way the
	// learning workers reason about them and accumulated differently on
	// purpose.
	//
	// ExecuteTools keeps only the LAST round: the earlier rounds were
	// re-attempted work the agent itself judged incomplete, and a skill
	// drafted from their calls would be drafted from a sequence the agent
	// then chose not to stand behind. AllTools accumulates across every
	// round, because some calls are a fact about the WHOLE turn — the
	// reflect dispatcher reads it to see that the agent already wrote its
	// own memory, and a later round that did not call reflect_and_persist
	// again does not undo that.
	//
	// Two fields where the three-phase engine had two phases to split on:
	// the executor's rounds are now the only place either fact can come
	// from, so the split has to be made explicitly rather than fall out of
	// which phase ran.
	ExecuteTools []string
	AllTools     []string

	// Outcome is the executor's own last word on the turn — delivered,
	// no_action, blocked, or the engine-written `incomplete`. The LAST
	// one, because a turn that looped ends on the account it stood behind.
	Outcome string

	// Workers and WorkerTokens count what this turn DELEGATED: how many
	// tasks ran and what they cost between them.
	//
	// KEPT SEPARATE from InputTokens/OutputTokens above, and deliberately.
	// A worker's tokens are already charged through the shared meter, so
	// folding them into the turn's own totals would report them twice and
	// make the phase events stop summing to the turn's number. What they
	// answer instead is the question the phase numbers cannot: how much of
	// a turn's cost was fan-out, which is the first thing to look at when
	// a seat's spend jumps and its own rounds did not.
	Workers      int
	WorkerTokens int
}

// Total is the turn's own token count. It does NOT include what its workers
// spent — see the Workers fields.
func (s Spend) Total() int { return s.InputTokens + s.OutputTokens }

// recordWorker folds one finished delegated task into the tally.
//
// UNDER THE LOCK, unlike record: workers run concurrently.
func (s *Spend) recordWorker(mu *sync.Mutex, tokens int) {
	mu.Lock()
	defer mu.Unlock()
	s.Workers++
	s.WorkerTokens += tokens
}

// Spend reports what this turn has cost so far.
//
// Read under the lock the workers write through, because the engine reads it
// on the turn's own goroutine while a worker from a fan-out that outlived its
// tool call could still be reporting.
func (r *Runner) Spend() Spend {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spend
}

// record folds one completed phase into the turn's tally.
func (s *Spend) record(rec phaseRecord) {
	switch rec.Phase {
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
	if rec.Phase == phase.Execute {
		names := toolNames(rec.Result.Executions)
		// REPLACED, not appended — see the field's own note.
		s.ExecuteTools = names
		s.AllTools = append(s.AllTools, names...)
		if rec.Decision != "" {
			s.Outcome = rec.Decision
		}
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
	}, e.traceFor(ctx)))

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
	}, e.traceFor(ctx)))
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
		RoundNarration: roundNarration(res.Narration),
		PartialRound:   partialRound(res.Partial),
	}, e.traceFor(ctx)))
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

	// Decision is the phase's structured verdict: the executor's outcome,
	// the reviewer's decision, "done" on a marked onboarding pass.
	Decision string

	// Rescued marks a phase whose submit tool never fired, so its payload
	// was synthesised. The executor and the reviewer both can; a sub-agent
	// answers in prose and has nothing to rescue.
	Rescued bool

	// Notes is short free text: the reviewer's notes, the executor's
	// missing tools.
	Notes string

	// Available is the tools whose schemas were actually passed in the
	// call — what the model could invoke. Catalogue is the prose list of
	// names the executor was shown, with no schemas: sending every MCP
	// server's tool definitions is what made a turn expensive, and this is
	// what replaced it.
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

// subagentCompleted closes ONE sub-agent, as a phase nested under the Execute
// round that spawned it.
//
// The subagent package produces a Result on every path a child can end on
// precisely so the caller's phase event cannot be missing — and the spawn tool
// is that caller. Without this a fan-out is invisible: its tokens are charged,
// its model calls happened, and nothing in the event store or the dashboard
// says a sub-agent ran at all. That is exactly how a subsystem stays broken
// unnoticed.
//
// Its tokens do NOT join the parent's own phase totals: they are already
// charged through the shared meter, and adding them there would report them
// twice and make the turn's phase numbers stop summing to its total. They are
// counted SEPARATELY — see Spend.Workers — which is what answers "how much of
// this turn was fan-out", a question the phase numbers cannot.
func (e emitter) subagentCompleted(ctx context.Context, res subagent.Result) {
	// COUNTED FIRST, and on every path — including the ones that never
	// reached a model and the runs with no publisher at all. A task that
	// timed out still ran, and a fan-out reported as three workers when
	// four were started hides exactly the one worth looking at.
	if e.tally != nil && e.mu != nil {
		e.tally.recordWorker(e.mu, res.Tokens())
	}
	if !e.on() {
		return
	}
	ev := types.AgentPhaseCompleted{
		Agent:    e.turn.AgentID,
		RoleName: e.role,
		TurnID:   e.turn.ID,
		Phase:    types.PhaseSubagent,
		// NESTED under the phase that spawned it, so a dashboard groups
		// it beneath that Execute round rather than rendering it as a
		// standalone sibling of the turn's own three phases.
		HostPhase:     types.PhaseExecute,
		HostIteration: e.hostIteration,
		// WHICH task and WHICH template. A call of eight otherwise
		// produces eight records distinguishable only by their prompts,
		// and the one an operator is looking for is the one that failed.
		Worker:         res.Worker,
		TaskID:         res.ID,
		Model:          res.Model,
		ProviderKey:    res.ProviderKey,
		Trigger:        e.turn.Trigger,
		SystemPrompt:   res.SystemPrompt,
		UserPrompt:     res.UserPrompt,
		Response:       res.Text,
		ToolExecutions: toolExecutions(res.Executions),
		InputTokens:    res.InputTokens,
		OutputTokens:   res.OutputTokens,
		TotalTokens:    res.Tokens(),
		RoundsUsed:     res.Rounds,
		ToolsAvailable: res.ToolsAvailable,
		// The grant's refusals, which is what Notes is documented to
		// carry for this phase. A child that asked for a tool it could
		// not have is the first thing to look at when its answer is thin.
		Notes:   rejectedNote(res.Rejected),
		Backend: types.BackendNative,
		// The task's own status — ok / no_result / timed_out / skipped —
		// which is the one field that says what became of it. It is a
		// phase's structured verdict, so it rides the same field the
		// executor's outcome and the reviewer's decision do.
		Decision:        string(res.Status),
		ConversationKey: e.turn.ConversationKey,
		Failed:          res.Failed(),
		Error:           truncate(res.Error, maxErrorLength),
		ErrorKind:       string(res.Status),
	}
	e.publish(ctx, events.New(ev, e.traceFor(ctx)))
}

// rejectedNote renders a grant's refusals for the event's Notes field.
func rejectedNote(rejected []string) string {
	if len(rejected) == 0 {
		return ""
	}
	return "rejected tools: " + strings.Join(rejected, ", ")
}

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
		RoundNarration:  roundNarration(rec.Result.Narration),
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
	e.publish(ctx, events.New(ev, e.traceFor(ctx)))
}

// classifyError names a failure's CLASS, for the one-word reason a dashboard
// prints beside a failed phase.
//
// The classified kinds are the ones an operator can act on: rotate a key,
// raise a cap, wait out a provider. Everything else is "error", deliberately.
// The tempting alternative is to name the failure's own type, and in Go that
// is a lie: a wrapped error's type is *fmt.wrapError whatever went wrong
// underneath, so the field would carry the same meaningless token for every
// unclassified failure while looking specific. One honest generic beats a
// specific-looking constant.
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
		log.WarnContext(ctx, "phase_telemetry_publish_failed", "type", ev.Type,
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

// roundNarration renders the loop's per-round model turns in the wire shape
// consumers read: round, reasoning, content.
//
// The round number matches the one on that round's tool executions, which is
// the whole contract — it is what lets a reader interleave the two lists into
// one chronological ledger without a second ordering rule.
func roundNarration(narr []toolloop.Narration) []types.RoundNarration {
	if len(narr) == 0 {
		return nil
	}
	out := make([]types.RoundNarration, 0, len(narr))
	for _, n := range narr {
		out = append(out, types.RoundNarration{
			"round":     n.Round,
			"reasoning": n.Reasoning,
			"content":   n.Content,
		})
	}
	return out
}

// partialRound renders the round currently being written, or nil.
//
// Nil rather than an empty object when there is nothing in flight, so
// `omitempty` drops the key entirely: a consumer reads "absent" as "no round
// is open", and an empty object would read as "a round is open and has said
// nothing", which is a different fact.
func partialRound(p *toolloop.Partial) map[string]any {
	if p == nil {
		return nil
	}
	out := map[string]any{
		"round":     p.Round,
		"reasoning": tail(p.Reasoning),
		"content":   tail(p.Content),
	}
	if len(p.Abandoned) > 0 {
		out["abandoned"] = roundNarration(p.Abandoned)
	}
	return out
}

// partialTail bounds how much of a round in flight goes on the wire.
//
// The whole accumulated text is republished five times a second — deltas
// cannot be sent instead, because the socket hub drops the OLDEST frame when a
// client falls behind and a consumer that had missed one would splice the
// remaining fragments into nonsense. Republishing the accumulation is
// therefore the correct shape for a lossy channel, and it is also quadratic in
// the length of the round: a thirteen-thousand-character reasoning block costs
// a seat about four megabytes over its life, times every open dashboard.
//
// The tail is what a reader is actually watching — text appears at the END —
// and the full text arrives moments later on the round's own narration, which
// is authoritative anyway. Four thousand characters is roughly two screens at
// this type size, so nothing a reader could have been mid-way through is cut.
const partialTail = 4000

// tail is the last partialTail characters, marked when it elides.
func tail(text string) string {
	if len(text) <= partialTail {
		return text
	}
	// Cut on a RUNE boundary: slicing a UTF-8 string by bytes can split a
	// multi-byte character, and the replacement glyph would be the last
	// thing on screen every time the cut landed mid-character.
	cut := text[len(text)-partialTail:]
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	return "…" + cut
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
		out = append(out, types.PromptMessage{Role: m.Role, Content: m.Content})
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

// traceFor is the trace an event this emitter publishes belongs to.
//
// The ACTIVE span when there is one — which inside runPhase is the phase's own
// span, so each phase becomes its own node in the trace tree rather than every
// event in a turn sharing one id and collapsing onto it.
//
// The TURN's trace otherwise, and that fallback is the whole reason this is a
// function. tracing.TraceOf mints a fresh root when no span is open, which is
// correct for a publisher that would otherwise have no trace at all and wrong
// here: an event published from a detached goroutine would leave its turn's
// trace and start a second one nobody looks at.
func (e emitter) traceFor(ctx context.Context) events.TraceContext {
	if tracing.Active(ctx) {
		return tracing.TraceOf(ctx)
	}
	return e.turn.Trace
}
