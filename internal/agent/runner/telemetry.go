package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tools"
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
//     finished. Published TWICE per round and ONCE PER CALL — once the model
//     has spoken, before its tools run; before each call, naming it as
//     `running_call`; and once they return — so the model's reasoning reaches
//     a screen without waiting on the slowest tool, and the slowest tool is
//     on the screen while it is slow.
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
	// RunID names THIS EXECUTION of the turn — see [turnctx.Turn.RunID]
	// and ADR-0017. It is the `turn_id` on every event published below,
	// and it is minted fresh per run, so a redelivered trigger's second
	// attempt writes its own phase records rather than landing on top of
	// the first attempt's.
	RunID string

	// WorkKey is the unit of work this run is doing — stable across a
	// re-run, empty for a turn with no ledgerable trigger. Carried on
	// every event beside RunID so a reader can ask "every attempt at this
	// trigger" without the two identities having to be one value.
	WorkKey string

	// AgentID is the seat's derived agent id, so a consumer can resolve the
	// seat without the org.
	AgentID string

	// Trigger is what woke this turn. It rides on EVERY phase event
	// because a live row that has no completed phase yet must still be
	// able to show its source.
	Trigger types.Trigger

	// ConversationKey is which conversation this turn served — the durable
	// CONVERSATION IDENTITY, never the inbox partition key beside it
	// ([notify.Prompt.ConversationIdentity]), so a direct message's phases
	// are one thread however the messages in it were threaded. It is the
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

	// onPhase is the working indicator's hook, or nil. See
	// [Config.OnPhase]; it is called from [emitter.started].
	onPhase func(phase.Phase)

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
		onPhase: r.cfg.OnPhase,
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

	// CacheRead and CacheWrite are the prompt cache's share of InputTokens
	// across the turn's own phases — a breakdown of it, never an addition.
	// Summed from the same phase records the token totals are, for the
	// same reason: the turn's number and its phases' numbers are one fact.
	CacheRead  int
	CacheWrite int

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

	// Workers, WorkerInput and WorkerOutput count what this turn
	// DELEGATED: how many tasks ran and what they cost between them, split
	// the way every other token figure is — because a cache's share and a
	// provider's price both differ between input and output, and a sum
	// cannot be split back.
	//
	// KEPT SEPARATE from InputTokens/OutputTokens above, and deliberately.
	// A worker's tokens are already charged through the shared meter, so
	// folding them into the turn's own totals would report them twice and
	// make the phase events stop summing to the turn's number. What they
	// answer instead is the question the phase numbers cannot: how much of
	// a turn's cost was fan-out, which is the first thing to look at when
	// a seat's spend jumps and its own rounds did not.
	Workers      int
	WorkerInput  int
	WorkerOutput int

	// Judged, JudgeInput and JudgeOutput count the round-cap extension
	// judge: how many times a phase ran out of rounds and asked for more,
	// and what those calls cost between them — split the way every other
	// token figure is, because a task's charge adds them to its own input
	// and output (ADR-0022).
	//
	// Kept out of the turn's own totals for the same reason a worker's are —
	// the judge's spend goes through the shared meter, so folding it in
	// would report it twice and stop the phase events summing to the turn's
	// number. What it answers instead is a question the phase numbers
	// cannot: whether a seat's cost is its work or its arguing about
	// whether to keep working.
	Judged      int
	JudgeInput  int
	JudgeOutput int

	// Rounds is the provider rounds the turn's completed phases ran, as
	// each phase record states them — so a resumed phase counts the rounds
	// before its suspend as well as after, exactly as its tokens do.
	Rounds int

	// SentBack is how many reviews returned the work for another pass: the
	// review phases that decided self_iterate. The same count the usage
	// domain's per-seat `sent_back` is, taken from the same records.
	SentBack int

	// Phases are the phases that completed, in the order each first ran.
	Phases []string
}

// Total is the turn's own token count — what its PHASES spent, and what the
// phase events sum to. It does not include what its workers or its extension
// judgements cost; both are metered separately and reported in their own
// fields, for the reason those fields state.
func (s Spend) Total() int { return s.InputTokens + s.OutputTokens }

// WorkerTokens is what the turn's delegated workers cost between them.
func (s Spend) WorkerTokens() int { return s.WorkerInput + s.WorkerOutput }

// JudgeTokens is what the turn's extension judgements cost between them.
func (s Spend) JudgeTokens() int { return s.JudgeInput + s.JudgeOutput }

// recordWorker folds one finished delegated task into the tally.
//
// UNDER THE LOCK, unlike record: workers run concurrently.
func (s *Spend) recordWorker(mu *sync.Mutex, res subagent.Result) {
	mu.Lock()
	defer mu.Unlock()
	s.Workers++
	s.WorkerInput += res.InputTokens
	s.WorkerOutput += res.OutputTokens
}

// recordJudge folds one extension judgement into the turn's tally.
//
// No lock, like record and unlike recordWorker: the judge is called from the
// phase's own goroutine, between tool-loop invocations.
func (s *Spend) recordJudge(d extension.Decision) {
	s.Judged++
	s.JudgeInput += d.InputTokens
	s.JudgeOutput += d.OutputTokens
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
	s.CacheRead += rec.Result.CacheRead
	s.CacheWrite += rec.Result.CacheWrite
	s.Rounds += rec.Result.RoundsUsed
	if name := rec.Phase.String(); !slices.Contains(s.Phases, name) {
		s.Phases = append(s.Phases, name)
	}
	if rec.Phase == phase.Review && rec.Decision == string(phase.SelfIterate) {
		s.SentBack++
	}
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
func (e emitter) started(ctx context.Context, ph phase.Phase, iteration int,
	system, user string, seed []llm.Message, surface *tools.Surface, caps roundCaps,
) {
	// BEFORE THE PUBLISHER GATE, and off the same `ph` the event below
	// carries. The working indicator is not telemetry: a runner whose phases
	// are silent — a test, an embedded runner, a node whose broker refused
	// every publish — still has a person watching a chat thread, so gating
	// this on whether anything is listening to the stream would be gating a
	// reader's own view on the engine's observability. See [Config.OnPhase].
	if e.onPhase != nil {
		e.onPhase(ph)
	}
	if !e.on() {
		return
	}
	e.publish(ctx, events.New(types.AgentPhaseStarted{
		Agent:     e.turn.AgentID,
		RoleName:  e.role,
		TurnID:    e.turn.RunID,
		WorkKey:   e.turn.WorkKey,
		Iteration: iteration,
		Phase:     types.Phase(ph),
		Trigger:   e.turn.Trigger,
		WorkItem:  e.workItem(),
	}, e.traceFor(ctx)))

	e.publish(ctx, events.New(types.AgentTurnProgress{
		Agent:     e.turn.AgentID,
		RoleName:  e.role,
		TurnID:    e.turn.RunID,
		WorkKey:   e.turn.WorkKey,
		Phase:     types.Phase(ph),
		Iteration: iteration,
		Trigger:   e.turn.Trigger,
		Prompt:    user,
		PromptMessages: []types.PromptMessage{
			{Role: string(llm.RoleSystem), Content: system},
			{Role: string(llm.RoleUser), Content: user},
		},
		RoundNum: openingRound,
		// The cap from the first frame, so a live row can say "of 8"
		// before the model has answered once.
		MaxRounds:    caps.max,
		RoundCeiling: caps.ceiling,
		WorkItem:     e.workItem(),
	}, e.traceFor(ctx)))

	e.promptSize(ctx, ph, iteration, system, user, seed, surface)
}

// skillsInjected records what a prompt's tool-skill catalogue offered: one
// `knowledge_read` with `via: skill_injected` per rendered catalogue, naming
// the page behind each skill it listed.
//
// A READ, because the catalogue line IS page content — the summary an author
// wrote on the skill's page, in front of the model — and a knowledge base that
// counted only the bodies loaded would report the pages every phase is shown
// as the ones nobody reads.
//
// ONE EVENT PER RENDER, listing its pages, and never one per skill: the
// catalogue's size belongs in the event's page list rather than in the event
// stream, which is the reason internal/learning keeps its own per-offer stamp
// off the stream entirely. Rendered nothing, recorded nothing. Nothing is
// recorded for a runner with no publisher — a sub-agent's own runner, a test —
// or with no turn to name a seat by.
func (e emitter) skillsInjected(ctx context.Context, ph phase.Phase, offerings [][]skills.Skill) {
	if !e.on() || e.turn.Context == nil {
		return
	}
	for _, offered := range offerings {
		pages := make([]types.KnowledgeReadPage, 0, len(offered))
		for _, s := range offered {
			if s.SourcePageID == "" {
				continue
			}
			pages = append(pages, types.KnowledgeReadPage{
				ID: s.SourcePageID, Container: s.SourceContainer, Title: s.SourceTitle,
			})
		}
		if len(pages) == 0 {
			continue
		}
		e.publish(ctx, events.New(types.KnowledgeRead{
			Agent: e.turn.AgentID, AgentHandle: e.turn.Context.Handle(), RoleName: e.role,
			TurnID: e.turn.RunID, WorkKey: e.turn.WorkKey, Phase: types.Phase(ph),
			Via:     types.ReadViaSkillInjected,
			Backend: offered[0].SourceBackend,
			Pages:   pages,
		}, e.traceFor(ctx)))
	}
}

// openingRound is the RoundNum of the update published before a phase's first
// provider call. See [emitter.started].
const openingRound = -1

// roundCaps is a phase's round allowance as a live row and a record state it:
// the cap it is running under now, and the most that cap can be raised to.
type roundCaps struct {
	max     int
	ceiling int
}

// capsOf states a phase's allowance for the round cap it is running under.
//
// The ceiling is the extension policy's where extensions are on, and NEVER
// below the cap itself: with extensions off the phase cannot be granted a
// round beyond the cap, and a resumed executor re-enters with a fresh budget on
// top of its pre-suspend rounds, which can lift its cap past a ceiling
// configured for a phase that never parked. A ceiling below the cap would tell
// a reader the phase had overrun a limit it never had.
func capsOf(policy extension.Policy, maxRounds int) roundCaps {
	ceiling := maxRounds
	if policy.Enabled {
		ceiling = max(ceiling, policy.Ceiling)
	}
	return roundCaps{max: maxRounds, ceiling: ceiling}
}

// workItem is the item the turn is charged to as the turn knows it now, or nil.
//
// A COPY, so an event built from it never aliases the turn's own value: the
// turn is the one thing a phase's publishers share.
func (e emitter) workItem() *types.WorkItem {
	if e.turn.Context == nil || e.turn.Context.WorkItem == nil {
		return nil
	}
	item := *e.turn.Context.WorkItem
	return &item
}

// promptSize measures the prompt a phase is about to send.
//
// Published from [emitter.started] because that is the one frame holding the
// FINAL prompt — after every section builder, every prefetch and every ledger
// have had their say. Anywhere earlier measures a draft.
//
// THE SURFACE IS TAKEN, NOT ITS DEFINITIONS, and rendered inside the [emitter.on]
// guard above: [tools.Surface.ToolDefs] clones every active tool's schema, and
// a runner with no publisher — every sub-agent, and every test driving one
// directly — would otherwise pay that render to build an event nobody
// receives. The call is safe from here: ToolDefs takes the surface's lock only
// to clone the active name list and has released it before it looks a tool up,
// and neither caller of started holds that lock.
//
// AGENT MODE COUNTS THE ARRAY TOO, although the launcher's Brief is the system
// and user text alone. Those definitions reach the coding CLI over the MCP
// bridge and that CLI's own model is billed for every one of them, so leaving
// them out would report the engine's cheapest-looking phase as its slimmest.
//
// The token figure is approximate by construction and says so in its field
// name: a real count needs the vendor's own tokenizer, which differs per model
// and would make this event a provider call. The size fields ride along so
// anyone comparing builds can apply their own ratio rather than inheriting
// this one.
//
// MEASURED IN BYTES, which is what len() of a Go string is and what the Go
// fields are named for. The WIRE KEYS still say chars and deliberately do not
// move — see [types.PromptSize], which carries the whole reason.
func (e emitter) promptSize(ctx context.Context, ph phase.Phase, iteration int,
	system, user string, seed []llm.Message, surface *tools.Surface,
) {
	if !e.on() {
		return
	}
	// Every phase in this package builds its surface before it opens, so a
	// nil one is not a state the engine reaches — but this runs on the
	// turn's own goroutine, where a nil dereference takes the seat down
	// rather than the measurement, and a telemetry frame is the last place
	// worth discovering that from.
	var defs []llm.ToolDef
	if surface != nil {
		defs = surface.ToolDefs()
	}
	m, err := measurePrompt(system, user, seed, defs)
	if err != nil {
		// The row still goes out, short one term, and it is worth more
		// than a phase with no measurement at all — whatever could not be
		// encoded here is what the provider call after it is about to
		// reject for the same reason. The log line is the only place that
		// says WHICH term came up short, because the row itself cannot:
		// a tool_chars of 0 beside a non-zero tool_count is visible, but
		// a conversation measured short of its own tool-call arguments
		// reads as a perfectly ordinary figure.
		log.WarnContext(ctx, "prompt_size_measure_failed", "phase", ph,
			"turn_id", e.turn.RunID, "error", err)
	}
	e.publish(ctx, events.New(types.PromptSize{
		Agent:             e.turn.AgentID,
		RoleName:          e.role,
		TurnID:            e.turn.RunID,
		WorkKey:           e.turn.WorkKey,
		Iteration:         iteration,
		Phase:             types.Phase(ph),
		ApproximateTokens: m.approximateTokens(),
		SystemBytes:       m.system,
		UserBytes:         m.user,
		MessageBytes:      m.messages,
		ToolBytes:         m.tools,
		ToolCount:         m.toolCount,
	}, e.traceFor(ctx)))
}

// bytesPerToken is the ratio the approximate count uses. Four is the figure
// both built-in providers' own documentation gives for English text — stated
// there as characters per token, which is the same number here because the
// prose and JSON a prompt is made of is overwhelmingly ASCII, where one
// character is one byte. This number exists to make prompt growth comparable
// across builds rather than to bill anybody — the real count is on the
// completed phase, from the provider.
const bytesPerToken = 4

// promptMeasure is one prompt's size, by component.
type promptMeasure struct {
	system    int
	user      int
	messages  int
	tools     int
	toolCount int
}

// approximateTokens is the whole prompt over one ratio.
func (m promptMeasure) approximateTokens() int {
	return (m.system + m.user + m.messages + m.tools) / bytesPerToken
}

// measurePrompt sizes what a phase is about to send.
//
// PURE OVER VALUES, in the shape internal/textindex and internal/search use
// for the same reason: arithmetic that can only be exercised through a live
// runner is arithmetic nobody re-measures.
//
// A SEEDED phase and a fresh one are measured as the loop sends them, which is
// exclusively one or the other: a resumed loop re-enters its saved messages
// and the system and user strings are ignored (see [phaseRun], which switches
// on the same nil), so counting them here would report bytes no provider
// receives — and counting only them is what reported every resumed executor at
// 0/0.
//
// THE TWO TERMS ARE MEASURED INDEPENDENTLY and the first failure is returned
// with both of them filled as far as they got. The caller publishes the row
// either way, and a term zeroed because a different term could not be encoded
// is a number nobody can interpret.
func measurePrompt(system, user string, seed []llm.Message, defs []llm.ToolDef) (promptMeasure, error) {
	m := promptMeasure{toolCount: len(defs)}
	var failure error
	if seed == nil {
		m.system, m.user = len(system), len(user)
	} else {
		m.messages, failure = seedBytes(seed)
	}
	// Not named `tools`: this file imports the package of that name, and a
	// local that shadows it is a compile error waiting for the next line
	// added here.
	size, err := toolDefBytes(defs)
	m.tools = size
	if failure == nil {
		failure = err
	}
	return m, failure
}

// seedBytes is what a parked conversation weighs: every message's text, the
// reasoning each assistant round carries, and the arguments of the tool calls
// in it. It returns what it managed to count alongside any failure, because
// the row goes out regardless.
//
// THE THINKING TERM IS COUNTED ONCE PER MESSAGE, and that is the whole of the
// arithmetic here. [llm.Message] carries a model's reasoning in two shapes and
// a backend sets either or both: the Anthropic backend fills ThinkingBlocks —
// which it hands straight back into the next call's content blocks and is
// billed for — and ALSO renders that same thinking text into ReasoningContent
// as prose, while the OpenAI backend fills ReasoningContent alone and the
// cli-agent text backend writes exactly that prose into the prompt it builds.
// So summing both would double the largest term a parked Anthropic turn
// carries, on the one backend that actually pays for it, and dropping either
// would report a resumed phase as having thought nothing on the other. The
// blocks win wherever there are blocks; the prose stands in where there are
// none.
//
// Signature is deliberately out of the sum: it is a fixed-size opaque token
// the provider mints per block rather than anything a model wrote, so counting
// it would make this figure move with a vendor's token format instead of with
// the prompt. A tool call's id and name are out for the same reason — bounded
// identifiers beside arguments that run to kilobytes.
func seedBytes(seed []llm.Message) (int, error) {
	total := 0
	for _, msg := range seed {
		total += len(msg.Content)
		if len(msg.ThinkingBlocks) > 0 {
			for _, tb := range msg.ThinkingBlocks {
				// Data is the redacted-thinking payload, which is the
				// whole of such a block: it has no readable Thinking,
				// and it is still handed back and still billed.
				total += len(tb.Thinking) + len(tb.Data)
			}
		} else {
			total += len(msg.ReasoningContent)
		}
		for _, tc := range msg.ToolCalls {
			if len(tc.Arguments) == 0 {
				total += emptyArgsBytes
				continue
			}
			encoded, err := json.Marshal(tc.Arguments)
			if err != nil {
				// NAME THE CALL, as toolDefBytes names the tool: json
				// reports the offending Go type and a parked
				// conversation holds one per round.
				return total, fmt.Errorf("measuring the parked conversation: the "+
					"arguments of tool call %q cannot be encoded as JSON: %w",
					tc.Name, err)
			}
			total += len(encoded)
		}
	}
	return total, nil
}

// emptyArgsBytes is what an argument-less tool call weighs on the wire: the two
// bytes of `{}` both HTTP backends send for one, rather than the four
// json.Marshal answers for a nil map. A `null` there is a shape no provider
// ever receives.
const emptyArgsBytes = len(`{}`)

// toolDefBytes is the compact JSON size of a tool-definition array.
//
// COMPACT, and ONE CANONICAL SHAPE rather than any vendor's own: this object
// per tool, whoever serves the phase. Marshalling [llm.ToolDef] itself would
// measure its Go field names, and measuring each backend's real array would
// make "is the prompt getting smaller" unanswerable the moment a fallback
// chain moved a seat between providers — which is the question this figure
// exists for.
//
// WHICH MAKES IT A FLOOR, stated on [types.PromptSize.ToolBytes] where a
// reader of the row will find it: every backend sends MORE than this. OpenAI
// wraps each entry as {"type":"function","function":{…}}, Anthropic spells
// the schema `input_schema` and puts a cache breakpoint on the last entry,
// both write an absent schema out as {"type":"object","properties":{}} where
// the omitempty below drops it, and the cli-agent text backend renders the
// same definitions indented inside a fenced catalogue. An empty DESCRIPTION
// is the one thing genuinely dropped on both — neither vendor sends a field
// for it.
func toolDefBytes(defs []llm.ToolDef) (int, error) {
	if len(defs) == 0 {
		return 0, nil
	}
	wire := make([]toolDefWire, 0, len(defs))
	for _, d := range defs {
		wire = append(wire, toolDefWire{
			Name: d.Name, Description: d.Description, Parameters: d.Parameters,
		})
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		// NAME THE TOOL a person has to fix, which the array's own error
		// cannot: json reports the offending Go type and there are as
		// many of those as there are schemas on the surface. One pass per
		// tool on a path that is already failing.
		for _, d := range defs {
			if _, each := json.Marshal(d.Parameters); each != nil {
				return 0, fmt.Errorf("measuring the tool definitions: the parameters "+
					"of tool %q cannot be encoded as JSON: %w", d.Name, each)
			}
		}
		return 0, fmt.Errorf("measuring the tool definitions: %w", err)
	}
	return len(encoded), nil
}

// toolDefWire is the canonical entry a tool array is measured as. Deliberately
// not any one vendor's: see [toolDefBytes].
type toolDefWire struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// fallback records one hand-off inside a phase's provider chain.
//
// Wired at the two places a chain is built, because the chain itself must not
// publish: it is handed to a sub-agent and to every phase alike, and a
// provider that knew about the event stream would have to be given a turn id
// it has no business holding. [chain.Options.OnFallback] exists for exactly
// this, and for a while nothing wired it — so the type was registered,
// categorised, documented and asserted in tests while no code path could
// produce one. An event nobody publishes reads, from every screen, as a
// company whose providers never fail.
//
// Fire-and-log like every publish here: a hand-off the operator cannot see is
// worse than one they cannot see published, and neither is worth failing a
// turn that is still running on the next provider.
func (e emitter) fallback(ctx context.Context, ph phase.Phase, iteration int, f chain.Fallback) {
	if !e.on() {
		return
	}
	e.publish(ctx, events.New(types.ProviderFallback{
		Agent:     e.turn.AgentID,
		RoleName:  e.role,
		TurnID:    e.turn.RunID,
		WorkKey:   e.turn.WorkKey,
		Iteration: iteration,
		Phase:     types.Phase(ph),
		// Carried through verbatim, EMPTY To included: the chain writes
		// "" for its last member on purpose, and substituting a
		// placeholder would name a provider that does not exist.
		FromProviderKey: f.From,
		ToProviderKey:   f.To,
		ErrorKind:       f.Kind.String(),
	}, e.traceFor(ctx)))
}

// progress publishes one in-flight round.
func (e emitter) progress(ctx context.Context, ph phase.Phase, iteration int, res toolloop.Result, caps roundCaps) {
	if !e.on() {
		return
	}
	e.publish(ctx, events.New(types.AgentTurnProgress{
		Agent:     e.turn.AgentID,
		RoleName:  e.role,
		TurnID:    e.turn.RunID,
		WorkKey:   e.turn.WorkKey,
		Phase:     types.Phase(ph),
		Iteration: iteration,
		Model:     res.Model,
		Trigger:   e.turn.Trigger,
		// NO PROMPT. It is sent once, on the opening frame, and the live
		// projection carries it forward from there — exactly as it already
		// carries the trigger.
		//
		// It was the largest thing on this event by a wide margin and the
		// one thing on it that never changes: a seat with a 30 KB system
		// prompt republished the whole of it five times a second, for the
		// length of every phase, to every open dashboard. Past
		// [queue.MaxPayloadBytes] the publish is refused outright, this
		// publisher logs and moves on, and the live row simply stops for
		// the rest of the phase with nothing on screen to say why — which
		// is likeliest at the tail of exactly the long phases somebody is
		// watching.
		Response:         tail(res.Text),
		InputTokens:      res.InputTokens,
		OutputTokens:     res.OutputTokens,
		TotalTokens:      res.InputTokens + res.OutputTokens,
		CacheReadTokens:  res.CacheRead,
		CacheWriteTokens: res.CacheWrite,
		Rounds:           phaseRounds(res.Rounds),
		MaxRounds:        caps.max,
		RoundCeiling:     caps.ceiling,
		RoundStartedAt:   res.RoundStartedAt,
		RunningCall:      runningCall(res.Running),
		WorkItem:         e.workItem(),
		// RoundsUsed is 1-based and RoundNum is 0-based; see the sentinel
		// above. Subtracting rather than counting separately keeps the two
		// from ever disagreeing about which round this is.
		RoundNum:       res.RoundsUsed - 1,
		ToolExecutions: liveExecutions(res.Executions),
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

	// Elapsed is the phase's wall clock, as [Runner.runPhase] measured it.
	//
	// Carried through rather than read here: this record is assembled after
	// the phase's payload is decoded, so a clock read at this point would
	// fold a different amount of the caller's own work into the number on
	// every path — and the executor's decode is the largest of them.
	Elapsed time.Duration

	// StartedAt is when THIS SEGMENT of the phase began, UTC — see
	// [types.AgentPhaseCompleted.StartedAt] for why it is not published
	// minus Elapsed on a resumed phase.
	StartedAt time.Time

	// Caps is the allowance the phase ended under.
	Caps roundCaps

	// Decision is the phase's structured verdict: the executor's outcome,
	// the reviewer's decision, "done" on a marked onboarding pass.
	//
	// A STRING rather than either enum, deliberately: this is the wire
	// shape of a telemetry record, and the phases put genuinely different
	// sets in it — [turn.Outcome] and [phase.Decision]. Callers render
	// their own through String().
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

	// Run says which box this phase ran in, where that is not this process.
	// The zero value is the native tool loop, which is what all but the
	// resumed Execute phases are.
	Run RunRecord

	// Failed and Err describe a phase that died instead of finishing. The
	// rest of the record is then PARTIAL rather than absent: a phase that
	// raises used to publish nothing at all, leaving a dashboard showing an
	// in-flight call with no response and no reason.
	Failed bool
	Err    error
}

// judged reports one round-cap extension judgement, as a phase nested under
// the phase that ran out of rounds.
//
// THE JUDGE IS A MODEL CALL, and it was the only one in the engine that
// nothing recorded: no phase event, so no card under the Execute round that
// fired it and no row in the token breakdown; no span, though the turn-engine
// doc promised `agent.turn.judge`; and no charge, because it runs outside the
// tool loop where every other call is metered. `types.PhaseJudge` was declared,
// read by the dashboard's nested-call grouping, and produced by nobody.
//
// What that cost is the question an operator actually asks: a company whose
// judge model is misconfigured and rescues every phase looked exactly like one
// whose phases genuinely deserved no extension. The only trace was a log line.
func (e emitter) judged(ctx context.Context, host phase.Phase, iteration, hostRound int,
	granted int, d extension.Decision, began time.Time, took time.Duration,
) {
	// Tallied first, as everywhere here: the tally is the turn's own
	// accounting and must not depend on whether anyone is listening.
	e.tally.recordJudge(d)
	if !e.on() {
		return
	}
	verdict := "rescue"
	if granted > 0 {
		verdict = "extend"
	}
	e.publish(ctx, events.New(types.AgentPhaseCompleted{
		Agent:    e.turn.AgentID,
		RoleName: e.role,
		TurnID:   e.turn.RunID,
		WorkKey:  e.turn.WorkKey,
		Phase:    types.PhaseJudge,
		// NESTED under the phase that asked, which is what the dashboard's
		// grouping already expects of every non-turn phase.
		HostPhase:     types.Phase(host),
		HostIteration: iteration,
		// The round the host phase had reached when it ran out: the judge
		// sits AFTER it on a timeline, not beside the phase's first round.
		HostRound:        hostRound,
		Iteration:        iteration,
		Model:            d.Model,
		ProviderKey:      d.ProviderKey,
		Trigger:          e.turn.Trigger,
		InputTokens:      d.InputTokens,
		OutputTokens:     d.OutputTokens,
		TotalTokens:      d.Tokens(),
		CacheReadTokens:  d.CacheRead,
		CacheWriteTokens: d.CacheWrite,
		StartedAt:        began.UTC(),
		WorkItem:         e.workItem(),
		// The verdict and the judge's own wording for it. `Notes` is the
		// reason: it is what makes a rescue readable, and on the failure
		// paths it is the only thing that names what went wrong.
		Decision: verdict,
		Notes:    d.Reason,
		// HOW LONG THE JUDGEMENT COST. A judge runs in the middle of a
		// phase that has already been running for minutes, so a slow cheap
		// model here is a stall the phase's own duration absorbs without
		// naming. Until this event carried its own measurement there was
		// nothing to name it with: a nested phase publishes no
		// agent_phase_started, so the two-event reconstruction every
		// consumer did could never produce a duration for one.
		DurationMS:      int(took / time.Millisecond),
		Backend:         types.BackendNative,
		ConversationKey: e.turn.ConversationKey,
	}, e.traceFor(ctx)))
}

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
		e.tally.recordWorker(e.mu, res)
	}
	if !e.on() {
		return
	}
	hostRound, _ := toolloop.CallRound(ctx)
	ev := types.AgentPhaseCompleted{
		Agent:    e.turn.AgentID,
		RoleName: e.role,
		TurnID:   e.turn.RunID,
		WorkKey:  e.turn.WorkKey,
		Phase:    types.PhaseSubagent,
		// THE ROUND THIS RAN IN, and it was left at zero.
		//
		// A phase's identity is (turn, phase, iteration) plus the task id
		// that distinguishes one worker of a fan-out from the next, and
		// task ids are only unique WITHIN one delegate call — `plan`
		// refuses a repeat there and nothing constrains the next round.
		// So a self-iterating turn that delegated a task named the same
		// thing twice produced two records under one identity, and the
		// dashboard's merge kept whichever arrived last: a worker, its
		// prompt, its tools and its failure were simply not on the page.
		// Iteration is what tags which round fired the trio, and a nested
		// phase belongs to the round that spawned it.
		Iteration: e.hostIteration,
		// NESTED under the phase that spawned it, so a dashboard groups
		// it beneath that Execute round rather than rendering it as a
		// standalone sibling of the turn's own phases.
		HostPhase:     types.PhaseExecute,
		HostIteration: e.hostIteration,
		// And the ROUND whose delegate call spawned it, off the context
		// the call ran under — which is how a worker is placed under its
		// call rather than anywhere in a phase of forty rounds.
		HostRound: hostRound,
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
		// Published beside the executions, on the round number they share.
		// A worker's card is the same round ledger as the turn's own phases
		// and reads it the same way; without this half of the pair, every
		// delegated worker rendered as bare tool rows with nothing that
		// asked for them.
		RoundNarration:   roundNarration(res.Narration),
		Rounds:           phaseRounds(res.RoundRecords),
		InputTokens:      res.InputTokens,
		OutputTokens:     res.OutputTokens,
		TotalTokens:      res.Tokens(),
		CacheReadTokens:  res.CacheRead,
		CacheWriteTokens: res.CacheWrite,
		RoundsUsed:       res.Rounds,
		// A worker has no extension: its cap is its ceiling.
		MaxRounds:    res.MaxRounds,
		RoundCeiling: res.MaxRounds,
		StartedAt:    utcOrZero(res.StartedAt),
		WorkItem:     e.workItem(),
		// THE WORKER'S OWN WALL CLOCK, off the result. A fan-out of eight
		// runs its tasks in parallel under one wall-clock cap, so "which
		// worker was slow" is the question a delegate call raises and the
		// only one its records could not answer — a nested phase publishes
		// no start, so there was never a second instant to subtract.
		DurationMS:     int(res.Elapsed / time.Millisecond),
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
		// A CHILD'S failure text, which is the one field on this event
		// whose length is set by something the parent does not control.
		// Bounded only so the event can be published — one over the
		// queue's ceiling is refused and this publisher logs and moves
		// on, so an unbounded child error costs the operator the whole
		// record rather than its tail. The parent phase's own error is
		// carried the same way; see events.MaxDiagnosticBytes.
		Error:     events.ClipDiagnostic(res.Error),
		ErrorKind: string(res.Status),
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
		Agent:            e.turn.AgentID,
		RoleName:         e.role,
		TurnID:           e.turn.RunID,
		WorkKey:          e.turn.WorkKey,
		Iteration:        rec.Iteration,
		Phase:            types.Phase(rec.Phase),
		Model:            rec.Result.Model,
		ProviderKey:      rec.Result.ProviderKey,
		Trigger:          e.turn.Trigger,
		SystemPrompt:     rec.System,
		UserPrompt:       rec.User,
		Response:         rec.Result.Text,
		ToolExecutions:   toolExecutions(rec.Result.Executions),
		RoundNarration:   roundNarration(rec.Result.Narration),
		Rounds:           phaseRounds(rec.Result.Rounds),
		InputTokens:      rec.Result.InputTokens,
		OutputTokens:     rec.Result.OutputTokens,
		TotalTokens:      rec.Result.InputTokens + rec.Result.OutputTokens,
		CacheReadTokens:  rec.Result.CacheRead,
		CacheWriteTokens: rec.Result.CacheWrite,
		RoundsUsed:       rec.Result.RoundsUsed,
		ExhaustedRounds:  rec.Exhausted,
		MaxRounds:        rec.Caps.max,
		RoundCeiling:     rec.Caps.ceiling,
		StartedAt:        utcOrZero(rec.StartedAt),
		WorkItem:         e.workItem(),
		// WHICH ROUND WAS EXPENSIVE, which is the question the token total
		// makes a reader ask and could not answer. Zero where the phase ran
		// no loop in this process — see [types.AgentPhaseCompleted].
		DurationMS: int(rec.Elapsed / time.Millisecond),
		// Off the loop's own count rather than a re-derivation from the
		// narration: a round that answered nothing records no narration
		// at all, so there is nothing downstream to count it from.
		EmptyAnswerRounds: rec.Result.EmptyAnswers,
		Decision:          rec.Decision,
		RescueFired:       rec.Rescued,
		Notes:             rec.Notes,
		ToolsAvailable:    rec.Available,
		ToolCatalogue:     rec.Catalogue,
		// Set explicitly. BackendNative is the value every consumer reads
		// as "ran here", and it is NOT the zero value — an empty string
		// renders as an unknown backend rather than as the normal one.
		//
		// It was a CONSTANT here, on every phase, which is why nothing in
		// the tree ever produced BackendSandbox: a detached coding run and
		// three rounds in this process reported the same backend, and the
		// two most expensive things a seat does were indistinguishable in
		// the event log.
		Backend:         types.BackendNative,
		ConversationKey: e.turn.ConversationKey,
		Failed:          rec.Failed,
	}
	if rec.Run.Sandboxed() {
		ev.Backend = types.BackendSandbox
		ev.CodingAgent = rec.Run.CodingAgent
		ev.SandboxID = rec.Run.SandboxID
		ev.DeliveredRefs = rec.Run.DeliveredRefs
		ev.LaunchID = rec.Run.LaunchID
	}
	if rec.Err != nil {
		// The 2000-character cut this used to carry landed on exactly the
		// errors worth reading: an exhausted provider chain naming what
		// each attempt refused, a wrapped chain whose cause is at its end.
		// What is left is a DELIVERY GUARANTEE, not a content budget —
		// an event over the queue's ceiling is refused and this publisher
		// logs and moves on, so an unbounded error would reach the
		// operator not shortened but ABSENT. See events.MaxDiagnosticBytes.
		ev.Error = events.ClipDiagnostic(rec.Err.Error())
		ev.ErrorKind = classifyError(rec.Err)
	}
	e.publish(ctx, events.New(ev, e.traceFor(ctx)))
}

// StoppedKind is the error kind a phase and a turn a person stopped carry: not
// a failure class, but the one word that says why the record has an error and
// no failure.
const StoppedKind = "stopped"

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
	case turn.Stopped(err):
		// Not a failure at all, and named so: a person paused the seat
		// and asked for its running turn to stop.
		return StoppedKind
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
			"role", e.role, "turn_id", e.turn.RunID, "error", err)
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
// Arguments go out as a JSON STRING, and it is the string rather than the map
// because the payload rides a store and a socket that keep it as text: a map
// decoded and re-encoded there loses the one thing a transcript cannot make
// up, which is a number too wide for a float64. The provider layer decodes a
// model's arguments through json.Number precisely so that an id survives, and
// a second pass through the default decoder is where it stops surviving.
//
// It is NOT about key order, which two earlier readings of this comment took
// it for: `encodeArgs` marshals a Go map, and encoding/json sorts those, so
// the order the model emitted is already gone before the string is formed.
// The ledger's elision does not depend on it either — `ledger.fitArguments`
// admits by serialised cost and breaks ties by name, and says so.
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
		// ONLY WHAT WAS MEASURED. An execution nobody timed — a
		// pre-suspend row an older build wrote, an agent-mode run's
		// bridged call — has no start, and writing `duration_ms: 0` for
		// it would state an instant call. Absent is the honest spelling of
		// "not recorded", and it is the one every reader already treats
		// that way.
		if !ex.StartedAt.IsZero() {
			row["started_at"] = ex.StartedAt.UTC().Format(time.RFC3339Nano)
			row["duration_ms"] = int(ex.Duration / time.Millisecond)
		}
		if ex.Origin != "" {
			row["origin"] = ex.Origin
		}
		if ex.Server != "" {
			row["server"] = ex.Server
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

// phaseRounds renders the loop's per-round provider calls in their wire shape.
func phaseRounds(rounds []toolloop.Round) []types.PhaseRound {
	if len(rounds) == 0 {
		return nil
	}
	out := make([]types.PhaseRound, 0, len(rounds))
	for _, r := range rounds {
		out = append(out, types.PhaseRound{
			Round:            r.Round,
			StartedAt:        r.StartedAt.UTC(),
			DurationMS:       int(r.Duration / time.Millisecond),
			Model:            r.Model,
			InputTokens:      r.InputTokens,
			OutputTokens:     r.OutputTokens,
			CacheReadTokens:  r.CacheRead,
			CacheWriteTokens: r.CacheWrite,
			ToolCalls:        r.ToolCalls,
		})
	}
	return out
}

// runningCall renders the call in flight, or nil when none is — so the key is
// absent on every frame but the one published immediately before a call.
func runningCall(c *toolloop.RunningCall) *types.RunningCall {
	if c == nil {
		return nil
	}
	return &types.RunningCall{
		Round: c.Round, Name: c.Name, Arguments: encodeArgs(c.Args),
		StartedAt: c.StartedAt.UTC(),
	}
}

// utcOrZero is t in UTC, or the zero time left zero — which `omitzero` drops,
// so a record that measured no start states none rather than the year 1.
func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
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

// liveExecutions is the round's tool calls with their OUTPUT bounded.
//
// Only on the live event: the durable record keeps every result verbatim, and
// this is the copy that is republished five times a second for the length of
// the phase. A tool result is routinely the largest thing on the frame — a
// knowledge search, a file read — and it is already final: the reader opens it
// on the completed record, where it is whole.
//
// The arguments are NOT bounded. They are what a reader scans a running phase
// for ("which file is it reading now?"), they are small, and cutting JSON in
// the middle produces something no consumer can parse.
func liveExecutions(execs []toolloop.Execution) []types.ToolExecution {
	out := toolExecutions(execs)
	for _, row := range out {
		if result, ok := row["result"].(string); ok {
			row["result"] = tail(result)
		}
		if failure, ok := row["error"].(string); ok {
			row["error"] = tail(failure)
		}
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
//
// The encoding itself is [tools.RecordArgs], shared with the bridged-run log
// because the two are one rule; what stays here is the EMPTY spelling. `{}`
// rather than "" because a phase event records a call that took no arguments,
// which is a real call, and the transcript shows the document it was made
// with.
func encodeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	raw, err := tools.RecordArgs(args)
	if err != nil {
		return "{}"
	}
	return raw
}

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
