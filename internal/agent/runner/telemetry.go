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

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The turn engine's telemetry. Nothing else publishes an agent_* event and the
// live projection keys on these three, so a silent phase leaves its seat idle
// on screen and no durable record:
//   - agent_phase_started: which phase, before the first provider call.
//   - agent_turn_progress: the in-flight view, NOT persisted (the completed
//     event supersedes it); published once the model has spoken, before each
//     call (`running_call`) and once the tools return.
//   - agent_phase_completed: the durable record.
//
// TELEMETRY NEVER FAILS THE WORK: every publish is fire-and-log, since the
// phase and its deliveries have already happened.

// Turn identifies the turn a runner is running: configuration, not state,
// since a runner is built per turn (Company.RunnerFor).
type Turn struct {
	// RunID names THIS EXECUTION of the turn ([turnctx.Turn.RunID],
	// ADR-0017): the `turn_id` on every event below, minted per run so a
	// redelivered trigger's retry writes its own phase records.
	RunID string

	// WorkKey is the unit of work, stable across re-runs, empty for a turn
	// with no ledgerable trigger; beside RunID so a reader can ask for
	// every attempt at one trigger.
	WorkKey string

	// AgentID is the seat's derived agent id, so a consumer can resolve the
	// seat without the org.
	AgentID string

	// Trigger is what woke this turn, on EVERY phase event so a live row
	// with no completed phase can still show its source.
	Trigger types.Trigger

	// ConversationKey is the durable CONVERSATION IDENTITY this turn served,
	// never the inbox partition key ([notify.Prompt.ConversationIdentity]),
	// so a direct message's phases are one thread; it is the only way to
	// ask the store for one thread's phases.
	ConversationKey string

	// Trace is the span context stamped on every event this turn emits.
	Trace events.TraceContext

	// Context is what the turn IS, the acting seat above all, passed to every
	// tool that asks (internal/agent/turnctx). Here rather than on Config so
	// a runner's turn identity lives in one place.
	Context *turnctx.Turn
}

// emitter publishes one turn's phase telemetry. A zero publisher publishes
// nothing (a test, or a sub-agent whose parent phase is the visible one), but
// the TALLY still runs: the engine publishes the turn-level event and must
// report the turn's spend even when its phases are silent.
type emitter struct {
	pub  queue.Publisher
	turn Turn
	role string
	// handle is the seat's handle, for the events that name the seat as
	// well as its role (a person's note to it among them).
	handle string
	tally  *Spend

	// onPhase is the working indicator's hook ([Config.OnPhase]), or nil,
	// called from [emitter.started].
	onPhase func(phase.Phase)

	// mu guards the delegation counters on tally, which several workers
	// write concurrently. Nil on an emitter that publishes nothing.
	mu *sync.Mutex

	// hostIteration is the Execute round a NESTED phase belongs to, so a
	// dashboard groups a sub-agent under it; zero, and unread, on an
	// ordinary phase's emitter.
	hostIteration int
}

// nestedAt is this emitter bound to the round a nested phase runs under.
func (e emitter) nestedAt(round int) emitter {
	e.hostIteration = round
	return e
}

// talliedOn is this emitter counting the workers it closes on spend, under mu,
// rather than on the runner's own tally — see [meter].
func (e emitter) talliedOn(spend *Spend, mu *sync.Mutex) emitter {
	e.tally, e.mu = spend, mu
	return e
}

func (r *Runner) emitter() emitter {
	return emitter{
		pub: r.cfg.Publisher, turn: r.cfg.Turn,
		role: r.cfg.Seat.Role.Name, handle: r.cfg.Seat.Role.Handle(),
		tally: &r.spend, mu: &r.mu,
		onPhase: r.cfg.OnPhase,
	}
}

// Spend is what one turn's phases cost, accumulated as they complete, so the
// turn-level event reports the SAME numbers the phase events did: an
// engine-side sum would be a second derivation that drifts.
//
// Only the delegation counters need the lock: phases run in sequence on one
// goroutine and the engine reads after turn.Run returns, but WORKERS RUN
// CONCURRENTLY into the same tally.
type Spend struct {
	// The model that actually served each phase, not necessarily the
	// configured one: whom a fallback chain's call landed on, or the chain
	// member an executor launch picked.
	ExecuteModel string
	ReviewModel  string

	InputTokens  int
	OutputTokens int

	// CacheRead and CacheWrite are the prompt cache's share of InputTokens
	// over the turn's own phases: a breakdown, never an addition.
	CacheRead  int
	CacheWrite int

	// Response is the last phase's text, for the summary a dashboard shows
	// before anyone expands the turn.
	Response string

	// ToolExecutions is the turn's calls as its phases' records list them,
	// in order, and ElidedCalls those they do not list, by tool: each
	// phase's held to its share as its own record holds it
	// ([keptExecutions]). [Spend.Calls] is what the turn's record carries.
	ToolExecutions []types.ToolExecution
	ElidedCalls    []types.ElidedCalls

	// ExecuteTools and AllTools are the tool NAMES the learning workers read.
	// ExecuteTools keeps only the LAST round: earlier rounds were work the
	// agent judged incomplete, and a skill must not be drafted from calls it
	// did not stand behind. AllTools accumulates every round, because some
	// calls are a fact about the whole turn (the reflect dispatcher sees the
	// agent already wrote its memory).
	ExecuteTools []string
	AllTools     []string

	// Outcome is the executor's last word: delivered, no_action, blocked, or
	// the engine-written `incomplete`. The LAST one, the account a looping
	// turn stood behind.
	Outcome string

	// Workers, WorkerInput and WorkerOutput count what this turn DELEGATED,
	// split like every token figure (cache share and price differ by
	// direction). KEPT OUT of InputTokens/OutputTokens: the shared meter
	// already charged them, so folding them in would count them twice and
	// break the phase events' sum. They answer how much of a turn was fan-out.
	Workers      int
	WorkerInput  int
	WorkerOutput int

	// Judged, JudgeInput and JudgeOutput count the round-cap extension judge's
	// calls and their cost, split because a task's charge adds them to its
	// own input and output (ADR-0022). Kept out of the turn's totals for
	// Workers' reason; they answer whether a seat's cost is its work or its
	// arguing for more rounds.
	Judged      int
	JudgeInput  int
	JudgeOutput int

	// Rounds is the provider rounds the turn's completed phases ran, as each
	// record states them (one per collected executor run, whose turns are its
	// harness's own).
	Rounds int

	// SentBack is how many reviews returned the work for another pass: the
	// review phases that decided self_iterate.
	SentBack int

	// Reviews is how many review phases completed, and Unaccepted how many
	// decided anything but done. With SentBack, this segment's share of what
	// the turn-ending record states ([types.TurnTally]), handed on by a
	// segment that parks. Counted here as records complete, the one place
	// every review passes: summed later from event logs, a turn whose records
	// two data nodes split is miscounted.
	Reviews    int
	Unaccepted int

	// Tools is this segment's calls to MCP servers' tools, per (server,
	// tool): every completed phase's and delegated worker's, a run's as its
	// bridge recorded them. Its share of [types.TurnTally.Tools], counted
	// here for Reviews' reason.
	Tools []types.ToolTally

	// Refusals is this segment's refused calls per (server, tool, class), the
	// engine's own tools included ([types.TurnTally.Refusals]).
	Refusals []types.RefusalTally

	// Review is the notes of the NEWEST review that sent the work back (a
	// turn sent back twice was answered on the second), empty if none did,
	// REDACTED as the phase's record holds them: a task's turn card quotes it
	// beside the send-back count, on a record of its own.
	Review string

	// Phases are the phases that completed, in the order each first ran.
	Phases []string

	// FailedIn is the newest phase whose record came back FAILED, empty when
	// none did (a stopped turn, or one that broke outside every phase).
	// Phases lists it too, so a task's turn card reads this to mark THAT chip.
	FailedIn string
}

// Calls is the tool list a turn's record carries ([types.AgentTurnCompleted]):
// every call its phases' records list, held to the one share a record has
// (recordbytes.go), and what those records and this one do not list, by tool.
func (s Spend) Calls() RecordCalls {
	return keptExecutions(s.ToolExecutions, s.ElidedCalls)
}

// Total is the turn's own token count: what its PHASES spent, which the phase
// events sum to. Workers and extension judgements are reported in their own
// fields ([Spend.Workers]).
func (s Spend) Total() int { return s.InputTokens + s.OutputTokens }

// WorkerTokens is what the turn's delegated workers cost between them.
func (s Spend) WorkerTokens() int { return s.WorkerInput + s.WorkerOutput }

// JudgeTokens is what the turn's extension judgements cost between them.
func (s Spend) JudgeTokens() int { return s.JudgeInput + s.JudgeOutput }

// recordWorker folds one finished delegated task into the tally, UNDER THE
// LOCK: workers run concurrently.
func (s *Spend) recordWorker(mu *sync.Mutex, res subagent.Result) {
	mu.Lock()
	defer mu.Unlock()
	s.Workers++
	s.WorkerInput += res.InputTokens
	s.WorkerOutput += res.OutputTokens
	// ITS CALLS ARE THE TURN'S: a worker reaches the seat's MCP servers on
	// the seat's grant.
	s.Tools = types.SumToolTallies(s.Tools, toolTallies(res.Executions))
	s.Refusals = types.SumRefusals(s.Refusals, refusalTallies(res.Executions))
}

// recordJudge folds one extension judgement into the turn's tally. No lock:
// the judge runs on the phase's own goroutine, between tool-loop invocations.
func (s *Spend) recordJudge(d extension.Decision) {
	s.Judged++
	s.JudgeInput += d.InputTokens
	s.JudgeOutput += d.OutputTokens
}

// Spend reports what this turn has cost so far, under the workers' lock: a
// worker from a fan-out that outlived its tool call may still be reporting.
func (r *Runner) Spend() Spend {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spend
}

// record folds one completed phase into the turn's tally.
func (s *Spend) record(rec phaseRecord, calls RecordCalls) {
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
	if rec.Failed {
		s.FailedIn = rec.Phase.String()
	}
	if rec.Phase == phase.Review {
		s.Reviews++
		if rec.Decision != string(phase.Done) {
			s.Unaccepted++
		}
		if rec.Decision == string(phase.SelfIterate) {
			s.SentBack++
			s.Review = redact.Secrets(rec.Notes)
		}
	}
	if rec.Result.Text != "" {
		s.Response = rec.Result.Text
	}
	s.ToolExecutions = append(s.ToolExecutions, calls.Rows...)
	s.ElidedCalls = keptTally(append(s.ElidedCalls, calls.ByTool...))
	s.Tools = types.SumToolTallies(s.Tools, toolTallies(rec.Result.Executions))
	s.Refusals = types.SumRefusals(s.Refusals, refusalTallies(rec.Result.Executions))
	if rec.Phase == phase.Execute {
		names := toolNames(rec.Result.Executions)
		// REPLACED, not appended: see [Spend.ExecuteTools].
		s.ExecuteTools = names
		s.AllTools = append(s.AllTools, names...)
		if rec.Decision != "" {
			s.Outcome = rec.Decision
		}
	}
}

// toolTallies counts a phase's calls to MCP servers' tools, per (server,
// tool), each in exactly one outcome ([types.ToolTally]). Counted by whose
// tool the call NAMED ([toolloop.Execution.Source]), not who answered, so a
// call refused before any tool ran counts against its server, in the outcome
// [mcp.OutcomeOf] gives it. Builtins and unregistered names are not counted.
func toolTallies(execs []toolloop.Execution) []types.ToolTally {
	var out []types.ToolTally
	for _, ex := range execs {
		server, ok := mcp.OriginServer(ex.Source)
		if !ok {
			continue
		}
		t := types.ToolTally{Server: server, Tool: ex.Name, Calls: 1}
		switch mcp.OutcomeOf(ex.Origin, ex.Refusal, ex.Failed) {
		case mcp.CallRefused:
			t.Refused = 1
		case mcp.CallUnavailable:
			t.Unavailable = 1
		case mcp.CallFailed:
			t.Failed = 1
		case mcp.CallAnswered:
		}
		// ONLY WHAT WAS MEASURED ([toolExecutions]): an untimed call adds no
		// time and no instant.
		if !ex.StartedAt.IsZero() {
			t.TotalMS = ex.Duration.Milliseconds()
			t.LastAt = ex.StartedAt.UTC()
		}
		out = append(out, t)
	}
	return types.SumToolTallies(out)
}

// refusalTallies counts a phase's refused calls per (server, tool, class): every
// call the engine classed a refusal, its own tools' included, under the server
// whose tool it named.
func refusalTallies(execs []toolloop.Execution) []types.RefusalTally {
	var out []types.RefusalTally
	for _, ex := range execs {
		if ex.Refusal == "" {
			continue
		}
		server, _ := mcp.OriginServer(ex.Source)
		out = append(out, types.RefusalTally{Server: server, Tool: ex.Name,
			Class: string(ex.Refusal), Calls: 1})
	}
	return types.SumRefusals(out)
}

// toolNames is the called tools in order, REPEATS KEPT: skill synthesis
// clusters on the sequence, and "search, read, read, reply" is a different
// procedure from "search, read, reply".
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

// on reports whether anything is listening, so callers skip the work of
// building an event nobody receives.
func (e emitter) on() bool { return e.pub != nil }

// started opens a phase and publishes its opening progress round: started says
// WHICH phase, and the round carries the prompt, so the live view shows what
// the agent was asked while it answers. That round's RoundNum is -1 because
// consumers read RoundNum+1 as "rounds so far"; 0 would claim a finished one.
func (e emitter) started(ctx context.Context, ph phase.Phase, iteration int,
	system, user prompts.Prompt, surface *tools.Surface, caps roundCaps,
) {
	// BEFORE THE PUBLISHER GATE, off the same `ph` as the event: the working
	// indicator is not telemetry, and a silent runner (a test, an embedded
	// runner, a broker refusing every publish) still has a person watching
	// its thread. See [Config.OnPhase].
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

	e.prompted(ctx, ph, iteration, system, user, caps)
	e.promptSize(ctx, ph, iteration, system.Text, user.Text, surface)
}

// prompted publishes a phase's opening progress round ([emitter.started]) on
// its own: an executor launch's, once it knows the run is handed a prompt
// other than its started record's (a session continuation or a relaunch's
// brief), so the live view, which reads that round as the phase's prompt,
// states what the run was sent.
func (e emitter) prompted(ctx context.Context, ph phase.Phase, iteration int,
	system, user prompts.Prompt, caps roundCaps,
) {
	if !e.on() {
		return
	}
	// REDACTED AND HELD TO A RECORD'S SHARE OF EACH PROMPT, part by part, as
	// the record holds them (recordedPrompt); the prompt the model was sent is
	// not changed.
	system, user = recordedPrompt(system), recordedPrompt(user)
	e.publish(ctx, events.New(types.AgentTurnProgress{
		Agent:     e.turn.AgentID,
		RoleName:  e.role,
		TurnID:    e.turn.RunID,
		WorkKey:   e.turn.WorkKey,
		Phase:     types.Phase(ph),
		Iteration: iteration,
		Trigger:   e.turn.Trigger,
		Prompt:    user.Text,
		// EACH MESSAGE WITH ITS OUTLINE, so the live view draws its parts
		// from the frame that built them, not its headings ([types.PromptSection]).
		PromptMessages: []types.PromptMessage{
			{Role: string(llm.RoleSystem), Content: system.Text, Sections: promptSections(ctx, ph, system)},
			{Role: string(llm.RoleUser), Content: user.Text, Sections: promptSections(ctx, ph, user)},
		},
		RoundNum: openingRound,
		// The cap from the first frame, so a live row can say "of 8"
		// before the model has answered once.
		MaxRounds:    caps.max,
		RoundCeiling: caps.ceiling,
		WorkItem:     e.workItem(),
	}, e.traceFor(ctx)))
}

// skillsInjected records what a prompt's tool-skill catalogue offered: one
// `knowledge_read` with `via: skill_injected` per rendered catalogue, naming
// the page behind each skill it listed. A READ, because a catalogue line is
// the page's own summary in front of the model; counting only loaded bodies
// would report the pages every phase sees as unread. ONE EVENT PER RENDER,
// never per skill, so the catalogue's size stays off the stream (why
// internal/learning keeps its per-offer stamp off it). Nothing is recorded for
// an empty render, a runner with no publisher, or one with no turn.
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

// capsOf states a phase's allowance for the round cap it is running under. The
// ceiling is the extension policy's when extensions are on, NEVER below the
// cap: a lower ceiling would tell a reader the phase overran a limit it never
// had.
func capsOf(policy extension.Policy, maxRounds int) roundCaps {
	ceiling := maxRounds
	if policy.Enabled {
		ceiling = max(ceiling, policy.Ceiling)
	}
	return roundCaps{max: maxRounds, ceiling: ceiling}
}

// workItem is the item the turn is charged to as the turn knows it now, or
// nil: a COPY, so an event never aliases the turn every publisher shares.
func (e emitter) workItem() *types.WorkItem {
	if e.turn.Context == nil || e.turn.Context.WorkItem == nil {
		return nil
	}
	item := *e.turn.Context.WorkItem
	return &item
}

// promptSize measures the prompt a phase is about to send, from
// [emitter.started]: the one frame holding the FINAL prompt.
//
// It takes the surface and renders [tools.Surface.ToolDefs] only past the
// [emitter.on] guard, since ToolDefs clones every schema and a silent runner
// (every sub-agent, every test) would pay for nothing. Safe here: ToolDefs
// releases the surface's lock before looking a tool up, and no caller of
// started holds it. The tool array is counted for EVERY phase, though none
// puts it in the prompt text: the harness's model is billed for it. The token
// figure is approximate (a real count needs each vendor's tokenizer, a
// provider call); the byte fields let a reader apply their own ratio. Bytes,
// not characters: see [types.PromptSize].
func (e emitter) promptSize(ctx context.Context, ph phase.Phase, iteration int,
	system, user string, surface *tools.Surface,
) {
	if !e.on() {
		return
	}
	// No phase reaches here with a nil surface, but a nil dereference on the
	// turn's goroutine would take the seat down rather than the measurement.
	var defs []llm.ToolDef
	if surface != nil {
		defs = surface.ToolDefs()
	}
	m, err := measurePrompt(system, user, defs)
	if err != nil {
		// Published anyway, short its tool term: what failed to encode here
		// the provider call is about to reject. Only this log names WHICH
		// tool; the row shows just tool_bytes 0 beside a non-zero tool_count.
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
		ToolBytes:         m.tools,
		ToolCount:         m.toolCount,
	}, e.traceFor(ctx)))
}

// bytesPerToken is the approximate count's ratio: both built-in providers'
// docs give four characters per token for English, and a prompt's prose and
// JSON are overwhelmingly ASCII, so four bytes. It makes prompt growth
// comparable across builds; the billed count is the provider's, on the
// completed phase.
const bytesPerToken = 4

// promptMeasure is one prompt's size, by component.
type promptMeasure struct {
	system    int
	user      int
	tools     int
	toolCount int
}

// approximateTokens is the whole prompt over one ratio.
func (m promptMeasure) approximateTokens() int {
	return (m.system + m.user + m.tools) / bytesPerToken
}

// measurePrompt sizes what a phase is about to send, PURE OVER VALUES so it is
// tested without a live runner. The text terms are whole whatever the tools
// do: an unencodable tool term comes back as the error beside them, since the
// caller publishes the row either way.
func measurePrompt(system, user string, defs []llm.ToolDef) (promptMeasure, error) {
	m := promptMeasure{system: len(system), user: len(user), toolCount: len(defs)}
	// Not named `tools`: that would shadow the imported package.
	size, err := toolDefBytes(defs)
	m.tools = size
	return m, err
}

// toolDefBytes is the compact JSON size of a tool-definition array in ONE
// CANONICAL SHAPE ([toolDefWire]), not [llm.ToolDef]'s Go field names nor a
// vendor's real array: a fallback moving a seat between providers would
// otherwise change the figure, and "is the prompt getting smaller" is what it
// answers. It is a FLOOR ([types.PromptSize.ToolBytes]): the harness reads the
// definitions over MCP, annotations and all, and wraps them its own way.
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
		// NAME THE TOOL to fix: json's error names only a Go type. One pass
		// per tool, on a path already failing.
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

// fallback records one hand-off inside a phase's provider chain — a call's,
// or the executor launch's pick of the member that takes its run. Wired where
// a chain is walked (a phase's calls, a worker's, the launch via
// [AgentRunRequest.OnFallback]) through [chain.Options.OnFallback], because
// the chain is shared with sub-agents and every phase and must not hold a
// turn id. Unpublished, every screen shows providers that never fail.
func (e emitter) fallback(ctx context.Context, ph phase.Phase, iteration int, f chain.Fallback) {
	if !e.on() {
		return
	}
	if f.FromFleet != nil {
		// AN LLM FLEET'S HAND-OFF IS A MOVE: provider_fallback stays for
		// pinned lists.
		e.fleetHandOff(ctx, ph, f)
		return
	}
	// WHEN THE HEAD IS BACK, where its vendor said: what a screen showing
	// the seat on its fallback names.
	var resets *time.Time
	var fromCLI string
	var fromSubscription bool
	if _, at, limited := llm.LimitedUntil(f.Err); limited {
		if !at.IsZero() {
			utc := at.UTC()
			resets = &utc
		}
		var failure *llm.Error
		if errors.As(f.Err, &failure) {
			fromCLI, fromSubscription = failure.Harness, failure.Subscription
		}
	}
	e.publish(ctx, events.New(types.ProviderFallback{
		Agent:     e.turn.AgentID,
		RoleName:  e.role,
		TurnID:    e.turn.RunID,
		WorkKey:   e.turn.WorkKey,
		Iteration: iteration,
		Phase:     types.Phase(ph),
		// Verbatim, EMPTY To included: the chain writes "" for its last
		// member, and a placeholder would name a provider that does not exist.
		FromProviderKey: f.From,
		ToProviderKey:   f.To,
		ErrorKind:       f.Kind.String(),
		ResetsAt:        resets,

		FromHarness:      fromCLI,
		FromSubscription: fromSubscription,
	}, e.traceFor(ctx)))
}

// progress publishes one in-flight round, its words redacted (liveredact.go);
// words is what the phase's earlier frames redacted of its calls and prose.
func (e emitter) progress(ctx context.Context, ph phase.Phase, iteration int, words *liveWords,
	res toolloop.Result, caps roundCaps,
) {
	if !e.on() {
		return
	}
	// WHAT GROWS ON THE FRAME, EACH HELD TO ITS LIVE SHARE (recordbytes.go,
	// "The live frame"): the frame is republished whole five times a second,
	// so past a share it keeps the newest and says what it left out.
	rounds := keptRounds(recordRounds{
		Rounds:      phaseRounds(res.Rounds),
		Invocations: phaseInvocations(res.Invocations),
		Steers:      phaseSteers(res.Steers),
		Narration:   words.narration(res.Narration),
		Calls:       words.executions(res.Executions),
	})
	calls := keptList(rounds.Calls, liveList, rounds.Dropped)
	running, runningElided := keptRunning(words.runningCalls(res.Running))
	e.publish(ctx, events.New(types.AgentTurnProgress{
		Agent:     e.turn.AgentID,
		RoleName:  e.role,
		TurnID:    e.turn.RunID,
		WorkKey:   e.turn.WorkKey,
		Phase:     types.Phase(ph),
		Iteration: iteration,
		Model:     res.Model,
		Trigger:   e.turn.Trigger,
		// NO PROMPT: sent once on the opening frame, and the live projection
		// carries it forward as it does the trigger. Republished five times a
		// second it is the largest thing here, and past
		// [queue.MaxPayloadBytes] the publish is refused and the live row
		// silently stops for the rest of the phase.
		Response:         words.response(res.Text),
		InputTokens:      res.InputTokens,
		OutputTokens:     res.OutputTokens,
		TotalTokens:      res.InputTokens + res.OutputTokens,
		CacheReadTokens:  res.CacheRead,
		CacheWriteTokens: res.CacheWrite,
		Rounds:           rounds.Rounds,
		Steers:           rounds.Steers,
		RoundsElided:     rounds.Elided,
		MaxRounds:        caps.max,
		RoundCeiling:     caps.ceiling,
		RoundStartedAt:   res.RoundStartedAt,
		RunStartedAt:     utcOrZero(res.RunStartedAt),
		RunningCalls:     running,
		RunningElided:    runningElided,
		Invocations:      rounds.Invocations,
		WorkItem:         e.workItem(),
		// RoundsUsed is 1-based, RoundNum 0-based ([openingRound]); derived,
		// so the two never disagree.
		RoundNum:       res.RoundsUsed - 1,
		ToolExecutions: calls.Rows,
		CallsElided:    calls.Elided,
		ElidedCalls:    calls.ByTool,
		RoundNarration: keptNarration(rounds.Narration, liveNarrationBytes),
		PartialRound:   words.partialRound(res.Partial),
	}, e.traceFor(ctx)))
}

// phaseRecord is what a completed phase reports, assembled by each phase
// rather than runPhase: its decision and whether its submit tool was rescued
// are known only once its payload is decoded, after runPhase returns.
type phaseRecord struct {
	Phase     phase.Phase
	Iteration int
	// Picks are the LLM fleet's picks of the chain the phase ran on, nil on
	// a pinned one: what its decision record is read from.
	Picks []*fleet.Pick
	// System and User are the phase's opening prompts with their
	// outlines; both zero on the collection of an executor's run, which
	// opens no conversation of its own.
	System    prompts.Prompt
	User      prompts.Prompt
	Result    toolloop.Result
	Exhausted bool

	// Elapsed is the phase's wall clock as [Runner.runPhase] measured it,
	// carried rather than read here: a clock read after the payload decode
	// (the executor's is the largest) would fold the caller's work in.
	Elapsed time.Duration

	// StartedAt is when THIS SEGMENT of the phase began, UTC — see
	// [types.AgentPhaseCompleted.StartedAt] for why it is not published
	// minus Elapsed on a resumed phase.
	StartedAt time.Time

	// Caps is the allowance the phase ended under.
	Caps roundCaps

	// Decision is the phase's structured verdict: the executor's outcome,
	// the reviewer's decision, "done" on a marked onboarding pass. A STRING
	// because the phases put different enums in it ([turn.Outcome],
	// [phase.Decision]); callers render theirs through String().
	Decision string

	// Rescued marks a phase whose submit tool never fired, so its payload
	// was synthesised. The executor and the reviewer both can; a sub-agent
	// answers in prose and has nothing to rescue.
	Rescued bool

	// Notes is short free text: the reviewer's notes, the executor's
	// missing tools.
	Notes string

	// Available is the tools whose schemas were passed in the call;
	// Catalogue the rest it could reach by discovery, without schemas
	// (every server's definitions would make a turn expensive). Disjoint.
	Available []string
	Catalogue []string

	// Run says which run and box this phase was, where that is not this
	// process. The zero value is a phase that ran on this host, which is
	// what every phase but the executor is.
	Run RunRecord

	// RunSteers is what a collected executor read of the turn's notes —
	// see [phaseResult.RunSteers].
	RunSteers []types.PhaseSteer

	// RunElided is the calls a collected executor's record did not keep —
	// see [phaseResult.RunElided].
	RunElided []ledger.Elided

	// Failed and Err describe a phase that died instead of finishing; the
	// rest of the record is then PARTIAL rather than absent, so a dashboard
	// shows why the call it drew in flight never answered.
	Failed bool
	Err    error
}

// judged reports one round-cap extension judgement, as a phase nested under
// the phase that ran out of rounds. THE JUDGE IS A MODEL CALL outside the
// tool loop's metering, so without this record it has no card, no row in the
// token breakdown and no charge, and a misconfigured judge rescuing every
// phase looks like phases that deserved no extension.
func (e emitter) judged(ctx context.Context, host phase.Phase, iteration, hostRound int,
	granted int, d extension.Decision, began time.Time, took time.Duration,
) {
	// Tallied first, as everywhere here: the turn's own accounting must not
	// depend on whether anyone is listening.
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
		// NESTED under the phase that asked, as the dashboard's grouping
		// expects of every non-turn phase.
		HostPhase:     types.Phase(host),
		HostIteration: iteration,
		// The round the host had reached when it ran out: the judge sits
		// AFTER it on a timeline.
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
		// `Notes` is the judge's reason: what makes a rescue readable, and on
		// failure the only thing naming what went wrong.
		Decision: verdict,
		Notes:    recordedText(d.Reason, notesBytes),
		// ITS OWN DURATION: a slow judge is a stall its host phase's duration
		// absorbs unnamed, and a nested phase publishes no
		// agent_phase_started to subtract from.
		DurationMS:      int(took / time.Millisecond),
		ConversationKey: e.turn.ConversationKey,
	}, e.traceFor(ctx)))
}

// subagentCompleted closes ONE sub-agent, as a phase nested under the Execute
// round that spawned it. subagent produces a Result on every path a child can
// end on so this record cannot be missing; without it a fan-out's tokens are
// charged and nothing says it ran. Its tokens stay OUT of the parent's phase
// totals and are counted in [Spend.Workers], for that field's reason.
func (e emitter) subagentCompleted(ctx context.Context, res subagent.Result) {
	// COUNTED FIRST, on every path, publisher or not: a task that timed out
	// still ran, and a short worker count hides the one worth looking at.
	if e.tally != nil && e.mu != nil {
		e.tally.recordWorker(e.mu, res)
	}
	if !e.on() {
		return
	}
	// HELD TO THEIR SHARES, as the turn's own phases' records are
	// (recordbytes.go): a worker's loop answers its MCP tools whole, and runs
	// as many rounds as its cap lets it.
	rounds := keptRounds(recordRounds{
		Rounds:    phaseRounds(res.RoundRecords),
		Narration: recordedNarration(res.Narration),
		Calls:     recordedExecutions(res.Executions),
	})
	calls := keptExecutions(rounds.Calls, rounds.Dropped)
	system, user := recordedPrompt(res.SystemPrompt), recordedPrompt(res.UserPrompt)
	available, availableElided := keptNames(res.ToolsAvailable)
	ev := types.AgentPhaseCompleted{
		Agent:    e.turn.AgentID,
		RoleName: e.role,
		TurnID:   e.turn.RunID,
		WorkKey:  e.turn.WorkKey,
		Phase:    types.PhaseSubagent,
		// THE ROUND THIS RAN IN: identity is (turn, phase, iteration) plus a
		// task id unique only WITHIN one delegate call (`plan` refuses a
		// repeat there), so without it a task id reused in a later round
		// collides and the dashboard's merge keeps only one record.
		Iteration: e.hostIteration,
		// NESTED under the Execute round that spawned it, not a sibling of
		// the turn's own phases.
		HostPhase:     types.PhaseExecute,
		HostIteration: e.hostIteration,
		// And the ROUND whose delegate call spawned it, placing the worker
		// under its call: only the executor's run delegates, and all its
		// calls are on the run's one round ([runRound],
		// [subagent.Config.Round]).
		HostRound: runRound,
		// WHICH task and template: a call of eight is otherwise told apart
		// only by prompts.
		Worker:         res.Worker,
		TaskID:         res.ID,
		Model:          res.Model,
		ProviderKey:    res.ProviderKey,
		FleetDecision:  FleetDecision(res.Picks, res.ProviderKey),
		Trigger:        e.turn.Trigger,
		SystemPrompt:   system.Text,
		UserPrompt:     user.Text,
		SystemSections: promptSections(ctx, phase.Subagent, system),
		UserSections:   promptSections(ctx, phase.Subagent, user),
		Response:       recordedText(res.Text, responseBytes),
		ToolExecutions: calls.Rows,
		CallsElided:    calls.Elided,
		ElidedCalls:    calls.ByTool,
		// Beside the executions, on the round number they share: a worker's
		// card reads the same round ledger as the turn's phases, and without
		// narration shows bare tool rows.
		RoundNarration:   keptNarration(rounds.Narration, narrationBytes),
		Rounds:           rounds.Rounds,
		RoundsElided:     rounds.Elided,
		InputTokens:      res.InputTokens,
		OutputTokens:     res.OutputTokens,
		TotalTokens:      res.Tokens(),
		CacheReadTokens:  res.CacheRead,
		CacheWriteTokens: res.CacheWrite,
		RoundsUsed:       res.Rounds,
		ExhaustedRounds:  res.Exhausted,
		// A worker has no extension: its cap is its ceiling.
		MaxRounds:    res.MaxRounds,
		RoundCeiling: res.MaxRounds,
		StartedAt:    utcOrZero(res.StartedAt),
		WorkItem:     e.workItem(),
		// THE WORKER'S OWN WALL CLOCK: a fan-out runs in parallel under one
		// cap, so which worker was slow is the question, and a nested phase
		// publishes no start to subtract.
		DurationMS:           int(res.Elapsed / time.Millisecond),
		ToolsAvailable:       available,
		ToolsAvailableElided: availableElided,
		// The grant's refusals, Notes' documented content for this phase:
		// the first thing to check when a child's answer is thin.
		Notes: recordedText(rejectedNote(res.Rejected), notesBytes),
		// The task's status (ok / no_result / timed_out / skipped) is its
		// structured verdict, so it rides Decision like the executor's outcome.
		Decision:        string(res.Status),
		ConversationKey: e.turn.ConversationKey,
		Failed:          res.Failed(),
		// A child's failure text, sized by nothing the parent controls:
		// redacted, and clipped so the event stays publishable, since one
		// over the queue's ceiling is refused and the whole record lost
		// (events.MaxDiagnosticBytes).
		Error:     events.Diagnostic(res.Error),
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
	// WHAT GROWS ON THE RECORD, EACH HELD TO ITS SHARE OF THE ONE MESSAGE
	// (recordbytes.go), with what it does not keep said beside it.
	k := rec.kept()
	// Tallied BEFORE the publisher check: see [emitter].
	e.tally.record(rec, k.calls)
	if !e.on() {
		return
	}
	system, user := recordedPrompt(rec.System), recordedPrompt(rec.User)
	available, availableElided := keptNames(rec.Available)
	catalogue, catalogueElided := keptNames(rec.Catalogue)
	ev := types.AgentPhaseCompleted{
		Agent:            e.turn.AgentID,
		RoleName:         e.role,
		TurnID:           e.turn.RunID,
		WorkKey:          e.turn.WorkKey,
		Iteration:        rec.Iteration,
		Phase:            types.Phase(rec.Phase),
		Model:            rec.Result.Model,
		ProviderKey:      rec.Result.ProviderKey,
		FleetDecision:    FleetDecision(rec.Picks, rec.Result.ProviderKey),
		Trigger:          e.turn.Trigger,
		SystemPrompt:     system.Text,
		UserPrompt:       user.Text,
		SystemSections:   promptSections(ctx, rec.Phase, system),
		UserSections:     promptSections(ctx, rec.Phase, user),
		Response:         recordedText(rec.Result.Text, responseBytes),
		ToolExecutions:   k.calls.Rows,
		CallsElided:      k.calls.Elided,
		ElidedCalls:      k.calls.ByTool,
		RoundNarration:   keptNarration(k.rounds.Narration, narrationBytes),
		Rounds:           k.rounds.Rounds,
		Steers:           k.rounds.Steers,
		Invocations:      k.rounds.Invocations,
		RoundsElided:     k.rounds.Elided,
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
		// WHICH ROUND WAS EXPENSIVE, the question the token total raises.
		// Zero where the phase ran no loop here ([types.AgentPhaseCompleted]).
		DurationMS: int(rec.Elapsed / time.Millisecond),
		// Off the loop's own count: a round that answered nothing records no
		// narration to count it from.
		EmptyAnswerRounds:    rec.Result.EmptyAnswers,
		Decision:             rec.Decision,
		RescueFired:          rec.Rescued,
		Notes:                recordedText(rec.Notes, notesBytes),
		ToolsAvailable:       available,
		ToolsAvailableElided: availableElided,
		ToolCatalogue:        catalogue,
		ToolCatalogueElided:  catalogueElided,
		ConversationKey:      e.turn.ConversationKey,
		Failed:               rec.Failed,
	}
	// THE RUN AN EXECUTOR WAS, on the record that collected it: harness, box,
	// deliveries, launch. Only the phase says where it ran
	// ([types.AgentPhaseCompleted.Harness]).
	if rec.Run.Sandboxed() {
		ev.Harness = rec.Run.Harness
		ev.SandboxID = rec.Run.SandboxID
		ev.DeliveredRefs = rec.Run.DeliveredRefs
		ev.DeliveredRefsElided = rec.Run.RefsElided
		ev.PushedRefs = rec.Run.Pushed
		ev.PushedRefsElided = rec.Run.PushedElided
		ev.PushedRefsUnread = rec.Run.PushedUnread
		ev.LaunchID = rec.Run.LaunchID
	}
	if rec.Err != nil {
		// Redacted, and clipped as a DELIVERY GUARANTEE, not a content
		// budget: an event over the queue's ceiling is refused and the error
		// ABSENT; a tighter cut would cost the errors worth reading (a
		// wrapped chain's cause is at its end). See events.MaxDiagnosticBytes.
		ev.Error = events.Diagnostic(rec.Err.Error())
		ev.ErrorKind = ErrorKind(rec.Err)
	}
	e.publish(ctx, events.New(ev, e.traceFor(ctx)))
}

// keptPhase is what a phase's record keeps of what grows on it with its rounds
// (recordbytes.go): its rounds within their window ([keptRounds]), and its
// calls held to their share ([keptExecutions]).
type keptPhase struct {
	rounds recordRounds
	calls  RecordCalls
}

// kept is what the phase's record keeps of its rounds and its calls: its
// loop's calls, or its run's as the bridge recorded them, tallying a round's
// past the window with those the list drops and those a collected run's record
// dropped first ([phaseResult.RunElided]). Kept once, for its record and the
// turn's, which lists what this one does.
func (rec phaseRecord) kept() keptPhase {
	rounds := keptRounds(recordRounds{
		Rounds:      phaseRounds(rec.Result.Rounds),
		Invocations: withDecisions(phaseInvocations(rec.Result.Invocations), rec.Picks),
		Steers:      append(phaseSteers(rec.Result.Steers), rec.RunSteers...),
		Narration:   recordedNarration(rec.Result.Narration),
		Calls:       recordedExecutions(rec.Result.Executions),
	})
	return keptPhase{rounds: rounds,
		calls: keptExecutions(rounds.Calls, slices.Concat(runTally(rec.RunElided), rounds.Dropped))}
}

// runTally is a collected run's calls its record did not keep, as a phase's
// record tallies them ([types.ElidedCalls]): by tool, how many and how many
// failed, a call whose tool its record did not keep under no name. Folded
// and held to its census where the record keeps it ([keptTally]). Nil for a
// run that dropped none, and for every phase that is no run.
func runTally(elided []ledger.Elided) []types.ElidedCalls {
	if len(elided) == 0 {
		return nil
	}
	out := make([]types.ElidedCalls, 0, len(elided))
	for _, e := range elided {
		if e.Calls > 0 {
			out = append(out, types.ElidedCalls{Name: e.Name, Calls: e.Calls, Failed: min(e.Failed, e.Calls)})
		}
	}
	return out
}

// StoppedKind is the error kind a phase and a turn a person stopped carry: not
// a failure class, but the one word that says why the record has an error and
// no failure.
const StoppedKind = "stopped"

// ErrorKind names a failure's CLASS, the one word a dashboard prints beside a
// failed phase or turn: ONE classifier for both, so they say the same word.
// Classified kinds are ones an operator can act on (rotate a key, raise a cap,
// wait out a provider, mend the host); the rest are
// [types.ErrorKindUnclassified], never the error's Go type, which for a
// wrapped error is *fmt.wrapError whatever happened. A CONDITION OF THE
// ENVIRONMENT (store unreachable, no data node, queue down, disk, sandbox
// class, deadline) is its own kind via [builtin.ConditionKind], never
// `error`, the word for the engine's own defect.
func ErrorKind(err error) string {
	var provider *llm.Error
	var contained *panics.Failure
	switch {
	case errors.As(err, &contained):
		// A PANIC IS ITS OWN REPORT whatever wraps it, so nothing counting
		// unclassified failures counts it twice.
		return types.ErrorKindPanicked
	case errors.As(err, &provider):
		return provider.Kind.String()
	case errors.Is(err, toolloop.ErrBudgetExhausted):
		return "budget_exhausted"
	case turn.Stopped(err):
		// Not a failure at all, and named so: a person paused the seat
		// and asked for its running turn to stop.
		return StoppedKind
	}
	if kind, ok := builtin.ConditionKind(err); ok {
		return kind
	}
	return types.ErrorKindUnclassified
}

// publish sends one event, or logs why it could not. Its SOURCE is the seat's
// role name: an actor-less consumer attributes by the envelope, and would
// otherwise render every phase as "system".
func (e emitter) publish(ctx context.Context, ev *events.Event) {
	ev.Source = e.role
	if err := e.pub.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "phase_telemetry_publish_failed", "type", ev.Type,
			"role", e.role, "turn_id", e.turn.RunID, "error", err)
	}
}

// toolExecutions renders the loop's executions in the wire shape consumers
// read: name, arguments, result, success. An open map, like
// types.ToolExecution, so a producer recording one more thing needs no reader
// recompiled. Arguments go out as a JSON STRING because a store and a socket
// keep the payload as text, and a map decoded and re-encoded there loses a
// number too wide for a float64 (the provider layer decodes through
// json.Number to keep it). Not for key order: `encodeArgs` marshals a Go map,
// which encoding/json sorts, and `ledger.fitArguments` does not depend on it.
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
		// ONLY WHAT WAS MEASURED: an untimed execution (arguments that did
		// not parse, answered rather than run) has no start, and
		// `duration_ms: 0` would state an instant call. Absent means "not
		// recorded" to every reader.
		if !ex.StartedAt.IsZero() {
			row["started_at"] = ex.StartedAt.UTC().Format(time.RFC3339Nano)
			row["duration_ms"] = int(ex.Duration / time.Millisecond)
		}
		// The call's own id tells two calls of one response apart (a harness
		// may run them side by side) and matches a live row's running call.
		if ex.CallID != "" {
			row["call_id"] = ex.CallID
		}
		if ex.Origin != "" {
			row["origin"] = ex.Origin
		}
		if ex.Server != "" {
			row["server"] = ex.Server
		}
		// WHOSE TOOL IT WAS, answered or refused, and the class the engine
		// gave a failure it classified: the two facts a server's refused
		// and unavailable calls are read by (types.ToolExecution). Absent
		// rather than empty where there is neither.
		if ex.Source != "" {
			row["source"] = ex.Source
		}
		if ex.Refusal != "" {
			row["refusal"] = string(ex.Refusal)
		}
		// A CALL WHOSE ARGUMENTS ARE SEALED says which it is: its `arguments`
		// are `{}` only because no record holds them in the clear, and a held
		// call's `success: false` is its question for a person, never a tool
		// that broke (types.ToolExecution).
		if ex.Held {
			row["held"] = true
		}
		if ex.Approved {
			row["approved"] = true
		}
		// AND A CALL WHOSE ARGUMENTS ARE NOT KNOWN says why: its `arguments`
		// are not what it was made with, and drawn as they stand a reader
		// takes it for the tool called with none (types.ToolExecution).
		if ex.ArgsUnknown != "" {
			row["arguments_unknown"] = string(ex.ArgsUnknown)
		}
		// WHAT ITS IMAGES WERE, by type and size: the record keeps no picture,
		// and an answer that carried none has no key (types.ToolExecution).
		if len(ex.Images) > 0 {
			row["images"] = ex.Images
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
			StopReason:       string(r.StopReason),
		})
	}
	return out
}

// phaseSteers renders the notes a phase's loop read in their wire shape — each
// at the round whose provider call first saw it, at a round's opening — or nil.
func phaseSteers(marks []toolloop.SteerMark) []types.PhaseSteer {
	if len(marks) == 0 {
		return nil
	}
	out := make([]types.PhaseSteer, 0, len(marks))
	for _, m := range marks {
		out = append(out, types.PhaseSteer{Round: m.Round, NoteID: m.ID,
			Boundary: types.SteerBoundaryRound})
	}
	return out
}

// steered records that the turn read a person's note, where read says: at the
// opening of a round of the engine's loop, or in an executor's brief.
func (e emitter) steered(ctx context.Context, read steer.Read) {
	if !e.on() {
		return
	}
	e.publish(ctx, events.New(steer.Record(e.steerTurn(), read.Note, steer.Delivered(read.Reading)),
		e.traceFor(ctx)))
}

// steerExpired records a note the turn took and never read.
func (e emitter) steerExpired(ctx context.Context, n steer.Note) {
	if !e.on() {
		return
	}
	e.publish(ctx, events.New(steer.Record(e.steerTurn(), n, steer.Expired()), e.traceFor(ctx)))
}

// steerTurn is the turn a note this runner records was sent to.
func (e emitter) steerTurn() steer.Turn {
	return steer.Turn{Agent: e.turn.AgentID, AgentHandle: e.handle, Role: e.role,
		TurnID: e.turn.RunID, WorkKey: e.turn.WorkKey}
}

// phaseInvocations renders a phase's harness runs in their wire shape.
func phaseInvocations(runs []toolloop.Invocation) []types.PhaseInvocation {
	if len(runs) == 0 {
		return nil
	}
	out := make([]types.PhaseInvocation, len(runs))
	for i, r := range runs {
		out[i] = types.PhaseInvocation{
			StartedAt: utcOrZero(r.StartedAt), DurationMS: int(r.Duration.Milliseconds()),
			ProviderKey: r.ProviderKey, Model: r.Model, Outcome: string(r.Outcome),
			FirstRound: r.FirstRound, LastRound: r.LastRound,
			RequestsObserved: r.RequestsObserved,
			InputTokens:      r.InputTokens, OutputTokens: r.OutputTokens,
			CacheReadTokens: r.CacheRead, CacheWriteTokens: r.CacheWrite,
			ToolCalls: r.Calls,
		}
	}
	return out
}

// utcOrZero is t in UTC, or the zero time left zero — which `omitzero` drops,
// so a record that measured no start states none rather than the year 1.
func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

// promptSections renders a prompt's outline in its wire shape, or nil.
//
// NIL FOR A MAP THAT DOES NOT TILE ITS TEXT: every map a reader receives can be
// sliced by, and a reader finding none falls back to the prompt's headings.
// Logged at warn: no builder here produces such a map (non-UTF-8 vendor text is
// the one way), so a builder regression would otherwise degrade every screen
// silently.
func promptSections(ctx context.Context, ph phase.Phase, p prompts.Prompt) []types.PromptSection {
	if len(p.Sections) == 0 {
		return nil
	}
	if !p.Valid() {
		log.WarnContext(ctx, "prompt_outline_withheld", "phase", ph,
			"sections", len(p.Sections), "bytes", len(p.Text),
			"valid_utf8", utf8.ValidString(p.Text))
		return nil
	}
	out := make([]types.PromptSection, 0, len(p.Sections))
	for _, s := range p.Sections {
		out = append(out, types.PromptSection{
			Key: s.Key, Title: s.Title, Bytes: s.Bytes, Headed: s.Headed,
		})
	}
	return out
}

// roundNarration renders the loop's per-round model turns in the wire shape
// consumers read: round, content, and `declined` (only when true) on a round
// that answered in prose where the phase had to end in a call. The round
// number matches its tool executions', which lets a reader interleave the two
// lists without a second ordering rule.
func roundNarration(narr []toolloop.Narration) []types.RoundNarration {
	if len(narr) == 0 {
		return nil
	}
	out := make([]types.RoundNarration, 0, len(narr))
	for _, n := range narr {
		row := types.RoundNarration{
			"round":   n.Round,
			"content": n.Content,
		}
		if n.Declined {
			row["declined"] = true
		}
		out = append(out, row)
	}
	return out
}

// partialTail bounds, in BYTES (the wire's unit), how much of a round in
// flight goes on the wire. The whole accumulation is republished five times a
// second, not deltas, because the socket hub drops the OLDEST frame and spliced
// fragments would be nonsense; that is quadratic in the round's length. The
// tail is what a reader watches, and the round's narration brings the full
// text moments later. 4000 bytes is about two screens of English, so nothing
// a reader is mid-way through is cut.
const partialTail = 4000

// encodeArgs renders a call's arguments as JSON text ([tools.RecordArgs], one
// rule with the bridged-run log), `{}` when empty or unmarshalable: a call
// with no arguments is real and shown with its document, and a "%v" dump could
// put an unquoted credential-shaped value on a screen expecting JSON.
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

// traceFor is the trace an event belongs to: the ACTIVE span (inside runPhase
// the phase's own, so each phase is its own trace node), else the TURN's.
// tracing.TraceOf alone would mint a fresh root with no span open, moving an
// event from a detached goroutine to a trace nobody looks at.
func (e emitter) traceFor(ctx context.Context) events.TraceContext {
	if tracing.Active(ctx) {
		return tracing.TraceOf(ctx)
	}
	return e.turn.Trace
}
