package types

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// The agent turn: its phases, its live progress, and its two completion
// records. Every one of these carries `role` — the seat the work and its tokens
// belong to. Load-bearing, not decorative: the event store tags a row's
// agent_role from it and the live projection keys on it. Without one a turn's
// token totals were attributed to nobody, and every agent card read zero tokens
// no matter what the seat had spent.

func init() {
	events.Register[AgentTurnCompleted]()
	events.Register[TurnCompleted]()
	events.Register[AgentPhaseStarted]()
	events.Register[AgentPhaseCompleted]()
	events.Register[AgentTurnProgress]()
	events.Register[SubagentBatched]()
}

// Phase names one leg of the turn engine's loop.
//
// A plain string on the wire, and deliberately open: AgentTurnProgress carries
// the provider key here for callers outside the turn engine, and a value this
// build does not know must survive rather than fail.
type Phase string

// The phases a turn can report. The first three are the legs of the loop itself,
// in the order they run; PhaseSubagent, PhaseAuxiliary and PhaseJudge are nested
// calls made under one of those, and never appear without a host phase around
// them.
const (
	PhasePlan     Phase = "plan"
	PhaseExecute  Phase = "execute"
	PhaseReview   Phase = "review"
	PhaseSubagent Phase = "subagent"
	// PhaseAuxiliary is a learning worker's own LLM call, nested under a host
	// phase; PhaseJudge is the round-cap extension judge.
	PhaseAuxiliary Phase = "auxiliary"
	PhaseJudge     Phase = "judge"
)

// ExecuteBackend names where an Execute phase actually ran.
type ExecuteBackend string

// The two places an Execute phase can run. BackendNative is the engine's own
// tool loop; BackendSandbox is a detached coding run. Neither is Go's zero
// value, so a publisher states which one — see the Backend field on
// AgentPhaseCompleted.
const (
	BackendNative  ExecuteBackend = "native"
	BackendSandbox ExecuteBackend = "sandbox"
)

// PlanDecision is the planner's top-level verdict.
type PlanDecision string

// The three verdicts a planner can reach. PlanDecisionPlan produced a plan for
// Execute to work through; PlanDecisionDirect skips planning and acts;
// PlanDecisionSkip opts the turn out entirely — the planner decided the trigger
// was not for this agent, which is why learning short-circuits on it.
const (
	PlanDecisionPlan   PlanDecision = "plan"
	PlanDecisionDirect PlanDecision = "direct"
	PlanDecisionSkip   PlanDecision = "skip"
)

// PromptMessage is one message of the conversation a phase sent to the model.
type PromptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ToolExecution records one tool call a phase made: name, arguments (a JSON
// string), result and success.
//
// Deliberately an open map rather than a struct, and the one place in this
// catalogue that stays loose. Its consumers pass it through verbatim, precisely
// because a hand-maintained field list is how a failed tool call got deleted on
// its way to a screen: a producer that starts recording one more thing must not
// need every reader recompiled before that thing can be seen.
type ToolExecution = map[string]any

// RoundNarration records one round's model turn: `round`, `reasoning` and
// `content`.
//
// It exists because `response` is the JOIN of every round's turn, and a join
// cannot be undone: the parts are separated by a blank line and prose contains
// blank lines, so a consumer handed only the blob cannot say which round said
// what. A dashboard splitting it on the leading `<think>` tag showed the first
// round's thinking as "the reasoning" and every later round's thinking as
// "the answer", tags and all.
//
// Both are published rather than one replacing the other: `response` is what
// every existing consumer and every already-stored event reads, and this
// envelope evolves ADDITIVE-ONLY because a rolling upgrade puts two builds on
// one stream. The duplicated text is small beside the prompts these same
// events already carry.
//
// An open map for the same reason [ToolExecution] is one — a producer that
// starts recording one more thing must not need every reader recompiled.
type RoundNarration = map[string]any

// AgentTurnCompleted is the single-phase summary a dashboard reads at turn end.
type AgentTurnCompleted struct {
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	Model    string `json:"model"`
	// Trigger is what woke this turn — see DescribeTrigger.
	Trigger        Trigger         `json:"trigger"`
	Prompt         string          `json:"prompt"`
	PromptMessages []PromptMessage `json:"prompt_messages,omitempty"`
	Response       string          `json:"response"`
	InputTokens    int             `json:"input_tokens"`
	OutputTokens   int             `json:"output_tokens"`
	TotalTokens    int             `json:"total_tokens"`
	ToolExecutions []ToolExecution `json:"tool_executions,omitempty"`
	// A2AContext is set when the turn answered an agent-to-agent ask. Absent
	// and empty are the same fact — not an A2A turn — so no pointer.
	A2AContext map[string]any `json:"a2a_context,omitempty"`

	// The turn engine's own summary of the three-phase loop.
	TurnID         string `json:"turn_id"`
	PlanModel      string `json:"plan_model"`
	ExecuteModel   string `json:"execute_model"`
	ReviewModel    string `json:"review_model"`
	SubagentCount  int    `json:"subagent_count"`
	SubagentTokens int    `json:"subagent_tokens"`
	Iterations     int    `json:"iterations"`
	Decision       string `json:"decision"`
	// Failed is true when the turn ended on a failure path rather than
	// finishing. Decision already reads "failed" in that case, but the REASON
	// lived only on the separate LLMUnavailable / TurnGuardBreach events, which
	// the agent's LLM-history view does not read — so the turn a dashboard
	// showed had no way to say why it stopped.
	Failed bool `json:"failed"`
	// Error is the failure's message, truncated. Empty unless Failed.
	Error string `json:"error"`
	// ErrorKind is the machine-readable failure class: the classified provider
	// error, or the guard-breach kind.
	ErrorKind string `json:"error_kind"`
	// ConversationKey is which conversation the turn served, "{source}:{local}".
	// Stamped so the event store can answer "what has this seat done on this
	// thread" by tag, which no field on this event could answer before.
	ConversationKey string `json:"conversation_key"`
}

// EventType is the "agent_turn_completed" wire type. Not to be confused with
// TurnCompleted's "turn_completed": both describe the same turn, for two
// different consumers.
func (AgentTurnCompleted) EventType() string { return "agent_turn_completed" }

// Role is the seat the turn and its tokens belong to. Every token figure on
// this event is attributed through it.
func (e AgentTurnCompleted) Role() string { return e.RoleName }

// AgentID is the instance that ran the turn.
func (e AgentTurnCompleted) AgentID() string { return e.Agent }

// SummaryFor renders a failed turn as failed, with the error KIND rather than
// the message: the kind is short enough for a line and is what an operator
// scans a feed for. A2A turns keep their channel tag either way.
func (e AgentTurnCompleted) SummaryFor(actor string) string {
	tag := a2aTag(e.A2AContext)
	if e.Failed {
		reason := e.ErrorKind
		if reason == "" {
			reason = "error"
		}
		return lead(actor, "turn failed ("+reason+")"+tag)
	}
	if e.Model != "" {
		return lead(actor, fmt.Sprintf("completed LLM turn (%s, %d tokens)%s",
			e.Model, e.TotalTokens, tag))
	}
	return lead(actor, "completed a turn"+tag)
}

// TurnCompleted is the Plan/Execute/Review-shaped record the learning subsystem
// consumes to build an episode and update counterparty profiles. Distinct from
// AgentTurnCompleted, which is the dashboard's single-phase summary of the same
// turn.
type TurnCompleted struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	TaskID      string `json:"task_id"`
	// StartedAt / EndedAt bound the turn; DurationMS is the span the learning
	// workers actually reason about.
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	DurationMS  int       `json:"duration_ms"`
	TaskSummary string    `json:"task_summary"`
	PlanSummary string    `json:"plan_summary"`
	// ToolSequence is the tools called during the Execute phase of the FINAL
	// iteration. Execute-scoped by design: the reflect engine's no-action gate
	// and the single-turn skill-synthesis trigger both reason about Execute
	// work specifically.
	ToolSequence []string `json:"tool_sequence,omitempty"`
	// PlanToolSequence is the tools called during the Plan phase(s),
	// accumulated across self-iterate loops. Plan-only builtins never appear in
	// ToolSequence, and the reflect engine reads this to skip the post-turn
	// persist decision when the LLM already self-persisted in flight.
	PlanToolSequence []string `json:"plan_tool_sequence,omitempty"`
	SkillsUsed       []string `json:"skills_used,omitempty"`
	ReviewOutcome    string   `json:"review_outcome"`
	Iterations       int      `json:"iterations"`
	// PlanDecision is empty when the turn never produced a Plan artifact (an
	// infrastructure failure before Plan ran). Learning short-circuits on
	// PlanDecisionSkip: the planner explicitly opted out, so there is nothing
	// the agent engaged with to learn from, and persisting facts read off the
	// trigger would teach it things directed at someone else.
	PlanDecision PlanDecision `json:"plan_decision"`
	// Interactions carries each trigger message's sender and body when
	// identifiable. Usually one entry; a coalesced trigger carries one per
	// constituent, possibly from several senders. Empty for internal triggers.
	Interactions []InboundInteraction `json:"interactions,omitempty"`
	// ConversationKey is empty when the trigger has no conversation a later
	// message could reproduce.
	ConversationKey string `json:"conversation_key"`
}

// EventType is the "turn_completed" wire type, the learning subsystem's
// Plan/Execute/Review-shaped record.
func (TurnCompleted) EventType() string { return "turn_completed" }

// Role is the seat whose episode this becomes.
func (e TurnCompleted) Role() string { return e.RoleName }

// AgentID is the instance whose diary and profiles the learning workers write
// against — it keys every per-agent row.
func (e TurnCompleted) AgentID() string { return e.Agent }

// SummaryFor names the review outcome (done, self_iterate), which is the one
// thing that distinguishes two turns of the same seat in a feed.
func (e TurnCompleted) SummaryFor(actor string) string {
	if e.ReviewOutcome != "" {
		return lead(actor, "completed turn ("+e.ReviewOutcome+")")
	}
	return lead(actor, "completed turn")
}

// AgentPhaseStarted opens each Plan / Execute / Review phase.
//
// A lightweight live signal: the matching AgentPhaseCompleted carries the full
// picture, but until it fires nothing tells a dashboard WHICH phase a working
// agent is in. Pair them — started sets the current phase, completed leaves it,
// the next started overwrites it, and task completion clears it.
//
// Not emitted for subagent, judge or auxiliary phases: those nest under a host
// phase that is already showing.
type AgentPhaseStarted struct {
	Agent     string `json:"agent_id"`
	RoleName  string `json:"role"`
	TurnID    string `json:"turn_id"`
	Iteration int    `json:"iteration"`
	Phase     Phase  `json:"phase"`
	// Trigger rides on every phase event so a live row that has no completed
	// phase yet can still show the turn's source.
	Trigger Trigger `json:"trigger"`
}

// EventType is the "agent_phase_started" wire type.
func (AgentPhaseStarted) EventType() string { return "agent_phase_started" }

// Role is the seat entering the phase; it is what a live view keys the
// current-phase marker on.
func (e AgentPhaseStarted) Role() string { return e.RoleName }

// AgentID is the instance entering the phase.
func (e AgentPhaseStarted) AgentID() string { return e.Agent }

// SummaryFor names the iteration as well as the phase, because a self-iterate
// turn emits the same phase several times and the number is what tells them
// apart.
func (e AgentPhaseStarted) SummaryFor(actor string) string {
	return lead(actor, fmt.Sprintf("started %s (iter %d)", e.Phase, e.Iteration))
}

// AgentPhaseCompleted closes each phase with the full context a per-phase
// timeline needs: what was sent, what came back, which tools ran, which
// structured decision it produced.
//
// One event per phase invocation. A self-iterate turn fires the trio several
// times and Iteration tags which; sub-agent spawns emit their own as siblings
// under the parent Execute phase.
type AgentPhaseCompleted struct {
	Agent     string `json:"agent_id"`
	RoleName  string `json:"role"`
	TurnID    string `json:"turn_id"`
	Iteration int    `json:"iteration"`
	Phase     Phase  `json:"phase"`
	// HostPhase / HostIteration are set only on a judge event: the phase that
	// triggered the judge, so dashboards group it under that phase instead of
	// rendering a standalone sibling.
	HostPhase     Phase `json:"host_phase"`
	HostIteration int   `json:"host_iteration"`
	// Worker names the auxiliary learning worker that made the call, and is set
	// only when Phase is PhaseAuxiliary.
	Worker      string  `json:"worker"`
	Model       string  `json:"model"`
	ProviderKey string  `json:"provider_key"`
	Trigger     Trigger `json:"trigger"`
	// The prompt and response are VERBATIM, not truncated: this telemetry is
	// what shows the operator what the model actually saw. Only Error is capped.
	SystemPrompt   string          `json:"system_prompt"`
	UserPrompt     string          `json:"user_prompt"`
	Response       string          `json:"response"`
	ToolExecutions []ToolExecution `json:"tool_executions,omitempty"`
	// RoundNarration is Response split back into the rounds that produced
	// it, so a reader can put a round's thinking beside the calls it asked
	// for. See [RoundNarration].
	RoundNarration  []RoundNarration `json:"round_narration,omitempty"`
	InputTokens     int              `json:"input_tokens"`
	OutputTokens    int              `json:"output_tokens"`
	TotalTokens     int              `json:"total_tokens"`
	RoundsUsed      int              `json:"rounds_used"`
	ExhaustedRounds bool             `json:"exhausted_rounds"`
	Decision        string           `json:"decision"`
	// RescueFired is true when the phase's submit tool was not called on the
	// first run of the loop, prompting a constrained rescue call. Plan and
	// Review can rescue; Execute and sub-agent phases never set this.
	RescueFired bool `json:"rescue_fired"`
	// Notes is free text kept short: review's notes, rejected sub-agent tools,
	// missing tool names from Execute.
	Notes string `json:"notes"`
	// ToolsAvailable is the tools whose JSON schemas were actually passed in
	// the call — what the model could invoke this round. Plan starts with only
	// its meta-tools here, because sending full schemas for the whole catalogue
	// is what made planning expensive; a tool the planner activates joins the
	// list from that round on.
	ToolsAvailable []string `json:"tools_available,omitempty"`
	// ToolCatalogue is the tools offered as prose in the Plan prompt, with no
	// schema. Empty for every phase other than Plan.
	ToolCatalogue []string `json:"tool_catalogue,omitempty"`
	// Backend is BackendSandbox only on an Execute phase that ran in one, so
	// the dashboard renders the sandbox badge precisely where it applies. Note
	// BackendNative is the wire default and NOT the Go zero value —
	// publishers set it explicitly.
	Backend       ExecuteBackend `json:"backend"`
	CodingAgent   string         `json:"coding_agent"`
	SandboxID     string         `json:"sandbox_id"`
	CostUSD       float64        `json:"cost_usd"`
	DeliveredRefs []string       `json:"delivered_refs,omitempty"`
	// Failed is true when the phase died instead of finishing.
	//
	// A phase that raises used to publish NOTHING: the only durable record was
	// the started event, leaving a dashboard showing an in-flight call with no
	// response and no reason. The runners now emit on the failure path too, so
	// Response, ToolExecutions and the token counts are PARTIAL on a failed
	// event rather than absent.
	Failed bool `json:"failed"`
	// Error is the failure's message, truncated. Empty unless Failed.
	Error string `json:"error"`
	// ErrorKind is the classified LLM error for an exhausted provider chain,
	// otherwise the exception's type name.
	ErrorKind string `json:"error_kind"`
	// ConversationKey is which conversation this phase's turn served.
	//
	// This event is where the model's reasoning is durably kept, as the <think>
	// prefix of Response, and until this field existed it was addressable only
	// by agent id and time: the store tags a channel id for A2A events alone,
	// so no query could ask for one thread's phases.
	ConversationKey string `json:"conversation_key"`
}

// EventType is the "agent_phase_completed" wire type.
func (AgentPhaseCompleted) EventType() string { return "agent_phase_completed" }

// Role is the seat the phase ran for — including for a judge or auxiliary
// phase, which is nested work done on that seat's behalf.
func (e AgentPhaseCompleted) Role() string { return e.RoleName }

// AgentID is the instance that ran the phase.
func (e AgentPhaseCompleted) AgentID() string { return e.Agent }

// SummaryFor assembles the line from the parts that are present rather than
// from a fixed template: a phase can have failed, decided, run in a sandbox or
// none of the three, and a template would leave gaps for the absent ones.
func (e AgentPhaseCompleted) SummaryFor(actor string) string {
	var parts []string
	if head := upperFirst(subject(actor, e.Phase)); head != "" {
		parts = append(parts, head)
	}
	if e.Backend == BackendSandbox {
		agent := e.CodingAgent
		if agent == "" {
			agent = "?"
		}
		parts = append(parts, "[sandbox:"+agent+"]")
	}
	switch {
	case e.Failed:
		kind := e.ErrorKind
		if kind == "" {
			kind = "error"
		}
		parts = append(parts, "✗ failed ("+kind+")")
	case e.Decision != "":
		parts = append(parts, "→ "+e.Decision)
	}
	if e.Model != "" {
		parts = append(parts, fmt.Sprintf("(%s, %d tokens)", e.Model, e.TotalTokens))
	}
	return strings.Join(parts, " ")
}

// AgentTurnProgress reports one tool-call round while the phase is still
// running.
//
// Not persisted — the matching AgentPhaseCompleted is the durable record. Live
// consumers correlate these with the turn-grouped history through TurnID,
// Phase and Iteration, which mirror the same fields on the phase events.
type AgentTurnProgress struct {
	Agent     string `json:"agent_id"`
	RoleName  string `json:"role"`
	TurnID    string `json:"turn_id"`
	Phase     Phase  `json:"phase"`
	Iteration int    `json:"iteration"`
	Model     string `json:"model"`
	// Trigger is repeated on every round so a live row keeps its source across
	// the round-by-round overwrites.
	Trigger        Trigger         `json:"trigger"`
	Prompt         string          `json:"prompt"`
	PromptMessages []PromptMessage `json:"prompt_messages,omitempty"`
	Response       string          `json:"response"`
	InputTokens    int             `json:"input_tokens"`
	OutputTokens   int             `json:"output_tokens"`
	TotalTokens    int             `json:"total_tokens"`
	// RoundNum is zero-based, or -1 for the opening update a phase publishes
	// before its first provider call — the one carrying PromptMessages so the
	// live view can show what the agent was asked while it is still answering.
	// Consumers read RoundNum+1 as "rounds so far", which is why the sentinel
	// is -1 rather than 0.
	RoundNum       int             `json:"round_num"`
	ToolExecutions []ToolExecution `json:"tool_executions,omitempty"`
	// RoundNarration is what the model said in each round so far. Free on
	// this event in storage terms — nothing persists it — and it is what
	// lets the live view append a round rather than redraw one blob.
	RoundNarration []RoundNarration `json:"round_narration,omitempty"`
	A2AContext     map[string]any   `json:"a2a_context,omitempty"`
}

// EventType is the "agent_turn_progress" wire type. Live only — nothing
// persists it, so it never appears in a history query.
func (AgentTurnProgress) EventType() string { return "agent_turn_progress" }

// Role is the seat currently working; the live projection keys its row on it.
func (e AgentTurnProgress) Role() string { return e.RoleName }

// AgentID is the instance mid-phase.
func (e AgentTurnProgress) AgentID() string { return e.Agent }

// SummaryFor degrades to a bare "working" when neither phase nor round is set:
// the round-by-round bits are context for a line whose real content is that the
// seat is still alive.
func (e AgentTurnProgress) SummaryFor(actor string) string {
	tag := a2aTag(e.A2AContext)
	var bits []string
	if e.Phase != "" {
		bits = append(bits, string(e.Phase))
	}
	// RoundNum is 0-based and -1 before the first provider call, so the line
	// renders RoundNum+1 — "rounds so far", the reading every other consumer
	// takes. Testing it against 0 rendered the sentinel verbatim ("round -1")
	// on the opening update every phase publishes, and suppressed the clause on
	// round 0, which is the first real round: the two cases that matter were
	// exactly the two it got wrong.
	if e.RoundNum >= 0 {
		bits = append(bits, fmt.Sprintf("round %d", e.RoundNum+1))
	}
	if len(bits) > 0 {
		return lead(actor, "working ("+strings.Join(bits, ", ")+")"+tag)
	}
	return lead(actor, "working"+tag)
}

// SubagentBatched fires once per batched sub-agent spawn, so a dashboard can
// count fan-out work and spot a pathological batch.
type SubagentBatched struct {
	ParentHandle string `json:"parent_handle"`
	TaskCount    int    `json:"task_count"`
	Successes    int    `json:"successes"`
	Failures     int    `json:"failures"`
	TotalTokens  int    `json:"total_tokens"`
}

// EventType is the "subagent_batched" wire type.
func (SubagentBatched) EventType() string { return "subagent_batched" }

// SummaryFor gives the whole batch in one line — count, successes, failures,
// tokens — because a pathological batch is only recognisable from the ratio,
// and this event carries no role for a dashboard to group the parts under.
func (e SubagentBatched) SummaryFor(actor string) string {
	return lead(actor, fmt.Sprintf("batched %d sub-agents (%d ok, %d failed, %d tokens)",
		e.TaskCount, e.Successes, e.Failures, e.TotalTokens))
}

// FormatReasoningAndContent renders one assistant turn's reasoning and visible
// content as ONE string: the model's extended thinking wrapped in <think> tags,
// immediately before the content it produced.
//
// This is the wire format of the response field on AgentPhaseCompleted and
// AgentTurnProgress, and it lives beside the events whose field it defines
// because several call sites build that field and they must not drift. When the
// live event omitted reasoning and the completed one included it, a thinking
// model's thoughts simply did not exist on the dashboard until its phase ended
// — the live view streamed tool calls against an empty response.
//
// Empty reasoning yields the content unchanged, so a non-thinking model's
// response keeps its plain shape. Reasoning with no content — a thinking model
// that hit its output cap — still renders, because the thinking is then the
// only signal there is.
func FormatReasoningAndContent(reasoning, content string) string {
	reasoning = strings.TrimSpace(reasoning)
	content = strings.TrimSpace(content)
	if reasoning == "" {
		return content
	}
	if content == "" {
		return "<think>" + reasoning + "</think>"
	}
	return "<think>" + reasoning + "</think>\n\n" + content
}
