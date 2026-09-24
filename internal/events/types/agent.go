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
	events.Register[AgentTurnStarted]()
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

// The phases a turn can report. PhaseOnboarding, PhaseExecute and PhaseReview
// are the legs of the turn itself, in the order they run; PhaseSubagent,
// PhaseAuxiliary and PhaseJudge are nested calls made under one of those, and
// never appear without a host phase around them.
//
// The retired `plan` value has NO CONSTANT here, and that is not an oversight:
// Phase is a plain string precisely so a value this build does not produce
// still decodes, round-trips and renders. Every event a pre-redesign node
// wrote carries it, and nothing in this build switches on it.
const (
	// PhaseOnboarding is the dedicated first-turn pass, at iteration 0,
	// before the executor's first round.
	PhaseOnboarding Phase = "onboarding"
	PhaseExecute    Phase = "execute"
	PhaseReview     Phase = "review"
	PhaseSubagent   Phase = "subagent"
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

// PlanDecision is the turn-level opt-out, on the wire under `plan_decision`.
//
// ONE VALUE SURVIVES. It used to carry a planner's three-way verdict; with no
// planner the only fact a reader still gates on is whether the turn opted out
// entirely, and that is derived from the turn's own decision rather than from
// anything a model wrote. The type and the wire name are kept because the
// field is a column in the episode store, and renaming it would migrate a
// value to buy a better word.
type PlanDecision string

// PlanDecisionSkip opts the turn out entirely — nobody was asking this seat to
// do anything, which is why learning short-circuits on it. A pre-redesign node
// also wrote `plan` and `direct` here; both decode as an unrecognised value,
// which is exactly how every reader already treats anything that is not skip.
const PlanDecisionSkip PlanDecision = "skip"

// PromptMessage is one message of the conversation a phase sent to the model.
type PromptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ToolExecution records one tool call a phase made: name, arguments (a JSON
// string), result and success, the round that asked for it, and — from a build
// that timed it — `started_at` (RFC 3339, UTC), `duration_ms`, and `origin`
// (`builtin` or `mcp:<server>`) with `server` (the bare MCP server name) for
// the tool that answered. The last four are ABSENT on a row nothing timed —
// an older peer's, an agent-mode run's bridged call — and `origin`/`server`
// are absent on a call no tool answered (an unknown name, one not offered, one
// a guard refused): absent means "not recorded", never "instant" or "the
// engine's own".
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

// PhaseRound is one provider call of a phase's tool loop — the MODEL's half of
// a round: when it was asked, how long it took to answer, who answered, and
// what it cost. The round's tool calls are timed on their own
// [ToolExecution] rows, which carry the same `round`, so a slow model and a
// slow tool are never one number.
//
// Typed rather than an open map like its two neighbours, because nothing about
// it is a producer's to extend per call: it is the loop's own measurement, and
// every field is one the loop always has.
type PhaseRound struct {
	// Round is one-based, on the scale `tool_executions[].round` and
	// `round_narration[].round` share — the key the three lists join on.
	Round int `json:"round"`
	// StartedAt is when the provider call was made, UTC.
	StartedAt time.Time `json:"started_at"`
	// DurationMS is how long the model took to answer, on the publishing
	// node's monotonic clock.
	DurationMS int `json:"duration_ms"`
	// Model is the model that served THIS round, which a fallback chain
	// can make differ from the phase's.
	Model        string `json:"model,omitempty"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	// CacheReadTokens and CacheWriteTokens are the share of InputTokens a
	// provider's prompt cache served and stored — a breakdown of it, never
	// an addition to it.
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	// ToolCalls is how many calls the model asked for this round.
	ToolCalls int `json:"tool_calls"`
}

// RunningCall is the tool call a phase is running RIGHT NOW, on the live
// progress frame only: published immediately before the call is handed to the
// tool and cleared by the frame after it returns.
//
// Its own field rather than a row in `tool_executions`, for the reason
// PartialRound is not merged into `round_narration`: a row there is a call that
// has answered, and a reader must be able to tell a call in flight from one
// that returned nothing.
type RunningCall struct {
	// Round is the round the call belongs to, on the `tool_executions`
	// scale.
	Round int    `json:"round"`
	Name  string `json:"name"`
	// Arguments is the call's arguments as a JSON string — the same
	// encoding `tool_executions[].arguments` uses, for the same reason.
	Arguments string `json:"arguments"`
	// StartedAt is when the call was handed to the tool, UTC.
	StartedAt time.Time `json:"started_at"`
}

// AgentTurnStarted opens a turn — or one SEGMENT of it, when a parked coding
// run is resumed — before its context is assembled and before its first phase.
//
// THE ONE EVENT THAT SAYS A TURN EXISTS WHILE IT IS STILL GATHERING CONTEXT.
// Every other turn event is published from inside a phase or after the last
// one, so until a phase opened nothing said a turn was running — a seat whose
// prefetch was reading a long thread showed as idle for as long as that took —
// and a turn's start was only ever inferred from the first event it happened
// to publish. It carries the work item the turn is charged to, resolved at
// dispatch, so a live screen can say "working on ENG-412" from the first
// instant rather than from the first completed phase.
//
// Its delegation depth is the ENVELOPE's `delegation_depth`, not a field here:
// the envelope owns that key, and a payload field under it would be dropped
// on the way out.
type AgentTurnStarted struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	// TurnID names this run — see ADR-0017.
	TurnID string `json:"turn_id"`
	// WorkKey is the unit of work this run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey].
	WorkKey string `json:"work_key,omitempty"`
	// WorkItem is the one item this turn is charged to, when a rule at
	// dispatch named one, and absent otherwise. ABSENT, never null: an
	// unattributed turn is the ordinary case for a chat wake, and a reader
	// tells it apart by the missing key. A sole write names an item only at
	// completion, so a turn that ends up charged by one starts without it.
	WorkItem *WorkItem `json:"work_item,omitempty"`
	// WorkItemBasis is the rule that named WorkItem, and empty with it.
	WorkItemBasis WorkItemBasis `json:"work_item_basis"`
	// Trigger is what woke the turn — see DescribeTrigger. A resumed
	// segment carries the event that resumed it.
	Trigger         Trigger `json:"trigger"`
	ConversationKey string  `json:"conversation_key"`
	// StartedAt is when this run — or this segment of it — began, on the
	// publishing node's clock.
	StartedAt time.Time `json:"started_at"`
	// Resumed marks a segment that re-entered a parked run rather than a
	// fresh dispatch. One turn id then has several starts, and only the
	// first is the turn beginning.
	Resumed bool `json:"resumed"`
}

// EventType is the "agent_turn_started" wire type.
func (AgentTurnStarted) EventType() string { return "agent_turn_started" }

// Role is the seat the turn runs as.
func (e AgentTurnStarted) Role() string { return e.RoleName }

// AgentID is the instance running the turn.
func (e AgentTurnStarted) AgentID() string { return e.Agent }

// SummaryFor names the item when the turn has one, because that is the
// question a feed line about a turn beginning is read to answer.
func (e AgentTurnStarted) SummaryFor(actor string) string {
	verb := "started a turn"
	if e.Resumed {
		verb = "resumed a turn"
	}
	if e.WorkItem != nil {
		if label := e.WorkItem.Key; label != "" {
			return lead(actor, verb+" on "+label)
		}
		return lead(actor, verb+" on "+e.WorkItem.Ref())
	}
	return lead(actor, verb)
}

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

	// The turn engine's own summary of the loop.
	TurnID string `json:"turn_id"`
	// WorkKey is the unit of work this run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey string `json:"work_key,omitempty"`
	// PlanModel is NO LONGER WRITTEN: there is no plan phase. It stays on
	// the type because the event store holds rows an earlier build wrote,
	// and a reader that dropped the field would render those turns as
	// though the model that served their planning had never been recorded.
	PlanModel      string `json:"plan_model,omitempty"`
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

// TurnCompleted is the turn-shaped record the learning subsystem consumes to
// build an episode and update counterparty profiles. Distinct from
// AgentTurnCompleted, which is the dashboard's single-phase summary of the same
// turn.
type TurnCompleted struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work this run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey string `json:"work_key,omitempty"`
	TaskID  string `json:"task_id"`
	// StartedAt / EndedAt bound the turn; DurationMS is the span the learning
	// workers actually reason about.
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	DurationMS  int       `json:"duration_ms"`
	TaskSummary string    `json:"task_summary"`
	// PlanSummary is what the turn set out to do, in the agent's own
	// words — the executor's submitted summary, or the reviewer's account
	// of what landed. It keeps its wire name: it is a column in the
	// episode store and the heading every learning worker renders, and
	// renaming it would migrate a value to buy a better word.
	PlanSummary string `json:"plan_summary"`
	// ToolSequence is the tools called during the FINAL executor round.
	// Last-round-scoped by design: the reflect engine's no-action gate and
	// the single-turn skill-synthesis trigger both reason about the work
	// the agent stood behind, not the rounds it judged incomplete.
	ToolSequence []string `json:"tool_sequence,omitempty"`
	// AllToolNames is every tool called in every round of the turn.
	//
	// The whole-turn view ToolSequence deliberately is not: a builtin
	// fired in round 1 is a fact about the turn, and the reflect engine
	// reads this to skip the post-turn persist decision when the agent
	// already self-persisted in flight.
	AllToolNames []string `json:"all_tool_names,omitempty"`
	// PlanToolSequence is NO LONGER WRITTEN — it was the Plan phase's own
	// calls, and there is no Plan phase. AllToolNames replaces it. It stays
	// on the type, and the reflect engine keeps reading it, because the
	// event store holds rows an earlier build wrote and a mixed fleet is
	// still writing them: dropping it would make a turn that self-persisted
	// look like one that did not, and run the persist decision twice.
	PlanToolSequence []string `json:"plan_tool_sequence,omitempty"`
	SkillsUsed       []string `json:"skills_used,omitempty"`
	ReviewOutcome    string   `json:"review_outcome"`
	Iterations       int      `json:"iterations"`
	// Outcome is the executor's own last word on the turn — `delivered`,
	// `no_action`, `blocked`, or the engine-written `incomplete`. Empty on
	// a turn that never reached an executor at all.
	Outcome string `json:"outcome,omitempty"`
	// PlanDecision now carries only PlanDecisionSkip, and only for a turn
	// that ended having decided nobody was asking it to do anything —
	// which is the one thing every reader of this field gates on.
	//
	// Kept rather than replaced by Outcome because a mixed fleet writes
	// both: an older node still publishes plan/direct/skip here, and a
	// reader switched to Outcome alone would treat those turns as having
	// no outcome. Learning short-circuits on PlanDecisionSkip: nothing the
	// agent engaged with, so persisting facts read off the trigger would
	// teach it things directed at someone else.
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
// record of one turn.
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

// AgentPhaseStarted opens each onboarding / execute / review phase.
//
// A lightweight live signal: the matching AgentPhaseCompleted carries the full
// picture, but until it fires nothing tells a dashboard WHICH phase a working
// agent is in. Pair them — started sets the current phase, completed leaves it,
// the next started overwrites it, and task completion clears it.
//
// Not emitted for subagent, judge or auxiliary phases: those nest under a host
// phase that is already showing.
type AgentPhaseStarted struct {
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	TurnID   string `json:"turn_id"`
	// WorkKey is the unit of work this run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey   string `json:"work_key,omitempty"`
	Iteration int    `json:"iteration"`
	Phase     Phase  `json:"phase"`
	// Trigger rides on every phase event so a live row that has no completed
	// phase yet can still show the turn's source.
	Trigger Trigger `json:"trigger"`
	// WorkItem is the item the turn is charged to, as the turn knew it when
	// the phase opened — see [AgentTurnStarted.WorkItem]. Absent, never
	// null, on an unattributed turn.
	WorkItem *WorkItem `json:"work_item,omitempty"`
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
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	// TurnID names ONE RUN of a turn. (TurnID, Phase, Iteration) is the
	// identity every reader keys a phase row on — the live projection, the
	// dashboard's merge, the store's fold — so it has to be unique per
	// execution, which is exactly what the work key is not. See ADR-0017.
	TurnID string `json:"turn_id"`
	// WorkKey is the unit of work this run was dispatched for — the set
	// of trigger events the dispatch derived it from, stable across a
	// re-run and across nodes, and empty for a trigger with none.
	//
	// BESIDE TurnID RATHER THAN INSTEAD OF IT. TurnID names ONE RUN, so a
	// redelivered trigger's second attempt writes its own phase records
	// instead of landing on top of the first attempt's — and this is what
	// still groups the attempts, which is the question an operator asks
	// when a turn failed and came back. The two were one value once; see
	// ADR-0017 for what that cost.
	WorkKey   string `json:"work_key,omitempty"`
	Iteration int    `json:"iteration"`
	Phase     Phase  `json:"phase"`
	// HostPhase / HostIteration are set only on a NESTED phase — a judge or
	// a delegated worker: the phase that fired it, so dashboards group it
	// under that phase instead of rendering a standalone sibling.
	HostPhase     Phase `json:"host_phase"`
	HostIteration int   `json:"host_iteration"`
	// HostRound is the ROUND of the host phase a nested phase belongs to,
	// on the host's `tool_executions[].round` scale: for a worker, the
	// round whose `delegate` call spawned it; for a judge, the last round
	// the host ran before it asked for more. HostIteration names the turn
	// iteration, which a phase of forty rounds spans whole, so it could
	// place a worker under its phase but not under the call that made it.
	// Absent on every other phase, and on a nested phase an older peer
	// published.
	HostRound int `json:"host_round,omitempty"`
	// Worker names the worker behind this call: the learning worker on a
	// PhaseAuxiliary event, the delegate template on a PhaseSubagent one.
	// Empty on every other phase, and on an ad-hoc delegation that named
	// no template.
	Worker string `json:"worker"`
	// TaskID is a delegated task's own id, as the parent wrote it. Set
	// only on PhaseSubagent, and it is what pairs this phase record with
	// a node of the call's graph on the matching SubagentBatched event —
	// without it a call of eight is eight indistinguishable records.
	TaskID      string  `json:"task_id,omitempty"`
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
	RoundNarration []RoundNarration `json:"round_narration,omitempty"`
	// Rounds is one entry per provider call the phase made, keyed on the
	// round number the two lists above share — see [PhaseRound]. Absent on
	// a phase that ran no loop in this process, and on an older peer's.
	Rounds       []PhaseRound `json:"rounds,omitempty"`
	InputTokens  int          `json:"input_tokens"`
	OutputTokens int          `json:"output_tokens"`
	TotalTokens  int          `json:"total_tokens"`
	// CacheReadTokens and CacheWriteTokens are the share of InputTokens the
	// provider's prompt cache served and stored, summed over Rounds. A
	// BREAKDOWN of InputTokens, never an addition to it: TotalTokens is
	// still input plus output, and the cache's share of the input is
	// cache_read_tokens / input_tokens.
	CacheReadTokens  int  `json:"cache_read_tokens"`
	CacheWriteTokens int  `json:"cache_write_tokens"`
	RoundsUsed       int  `json:"rounds_used"`
	ExhaustedRounds  bool `json:"exhausted_rounds"`
	// MaxRounds is the round cap the phase ENDED under — its base budget
	// plus every extension the judge granted — and RoundCeiling the most
	// it could ever have been granted. RoundsUsed against MaxRounds is
	// "how far into its allowance", and MaxRounds against RoundCeiling is
	// "how much more it could have asked for". Zero on a phase that runs
	// no loop of its own (a judge) and on an older peer's.
	MaxRounds    int `json:"max_rounds,omitempty"`
	RoundCeiling int `json:"round_ceiling,omitempty"`
	// StartedAt is when THIS SEGMENT of the phase began, on the publishing
	// node's clock. A resumed executor is one phase in two segments, and
	// DurationMS below covers both — so StartedAt is deliberately NOT
	// "published minus duration": that instant is when the first segment
	// began, possibly days earlier and on another node, and the gap
	// between the two segments was a coding run rather than this phase.
	// Absent on an older peer's record.
	StartedAt time.Time `json:"started_at,omitzero"`
	// WorkItem is the item the turn is charged to, as the turn knew it
	// when this record was published — see [AgentTurnStarted.WorkItem].
	WorkItem *WorkItem `json:"work_item,omitempty"`
	// LaunchID names the detached coding run a resumed phase collected —
	// the job [SandboxRunCompleted.LaunchID] names, which the pending-run
	// row held when the resume claimed it. A turn can launch more than
	// once, so one segment of a phase is (turn_id, phase, iteration,
	// launch_id). Empty on every phase that collected no run.
	LaunchID string `json:"launch_id,omitempty"`
	// DurationMS is how long the work this record reports actually took,
	// measured by the process that published it.
	//
	// ON THE RECORD rather than reconstructed from the matching
	// AgentPhaseStarted, which is what every consumer used to do. That
	// reconstruction needs BOTH events in one reader's hands, and three
	// readers never have both: a dashboard deep-linked into a turn while it
	// runs asked its query before the phase started and only buffers
	// completed envelopes afterwards; a nested phase — a worker, a judge —
	// publishes no start at all, so no worker of a delegate fan-out had a
	// duration anywhere; and a phase started on one node and completed on
	// another subtracts two clocks nothing reconciles. One record that
	// carries its own measurement answers all three, and the arithmetic is
	// done where the clock is.
	//
	// Zero means this record measures no loop of its own rather than a
	// phase that took no time: an agent-mode executor's rounds happen
	// inside the CLI's own loop, in another process, and the engine's
	// re-entry replays what that run already did. The detached run's own
	// wall clock is on sandbox_run_started / sandbox_run_completed, which
	// is where a reader asking how long the CODING took should look.
	DurationMS int `json:"duration_ms"`
	// EmptyAnswerRounds counts the rounds in which the model produced
	// neither prose nor a tool call — it spent its output budget on hidden
	// reasoning and stopped.
	//
	// Here rather than as an event of its own because this is the record
	// that already knows the seat, the phase and the model. The cli-agent
	// backend used to raise a transport error on that round, which walked
	// the fallback chain and could end the turn as llm_unavailable; it now
	// hands back an empty completion and the tool loop corrects it, so a
	// model that habitually answers nothing would otherwise be visible only
	// as unexplained rescues. A non-zero count on a phase is the
	// explanation, and the fix is the entry's `model`.
	EmptyAnswerRounds int    `json:"empty_answer_rounds,omitempty"`
	Decision          string `json:"decision"`
	// RescueFired is true when the phase's submit tool was not called on the
	// first run of the loop, prompting a constrained rescue call. The
	// executor and the reviewer both can; sub-agent phases never set this.
	RescueFired bool `json:"rescue_fired"`
	// Notes is free text kept short: review's notes, rejected sub-agent tools,
	// missing tool names from Execute.
	Notes string `json:"notes"`
	// ToolsAvailable is the tools whose JSON schemas were actually passed in
	// the call — what the model could invoke this round. The executor starts
	// with every first-party tool and NO MCP tool, because sending full
	// schemas for every server's catalogue is what made a turn expensive; a
	// tool it activates through discovery joins the list from that round on.
	ToolsAvailable []string `json:"tools_available,omitempty"`
	// ToolCatalogue is the tools offered as prose in the executor's prompt,
	// with no schema — builtin names and MCP server names. Empty for every
	// phase other than execute.
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
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	TurnID   string `json:"turn_id"`
	// WorkKey is the unit of work this run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey   string `json:"work_key,omitempty"`
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
	// CacheReadTokens and CacheWriteTokens are the prompt cache's share of
	// InputTokens so far — see [AgentPhaseCompleted.CacheReadTokens].
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	// Rounds is every provider call so far — see [PhaseRound].
	Rounds []PhaseRound `json:"rounds,omitempty"`
	// MaxRounds is the cap the phase is running under NOW, which an
	// extension raises mid-phase, and RoundCeiling the most it can be
	// raised to. What "round 3 of 8" is rendered from, off the loop's own
	// cap rather than a copy of the config.
	MaxRounds    int `json:"max_rounds,omitempty"`
	RoundCeiling int `json:"round_ceiling,omitempty"`
	// RoundStartedAt is when the latest round's provider call was made:
	// the round being written while the model answers, and the round whose
	// tools are running after it has. Absent on the opening frame.
	RoundStartedAt time.Time `json:"round_started_at,omitzero"`
	// RunningCall is the tool call in flight — see [RunningCall]. Absent
	// whenever no call is running.
	RunningCall *RunningCall `json:"running_call,omitempty"`
	// WorkItem is the item the turn is charged to, as the turn knows it
	// now — see [AgentTurnStarted.WorkItem].
	WorkItem *WorkItem `json:"work_item,omitempty"`
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
	// PartialRound is the round being written RIGHT NOW: `round`,
	// `reasoning`, `content`, and `abandoned` for attempts a provider gave
	// up on partway through.
	//
	// SEPARATE from RoundNarration on purpose — a reader must be able to
	// tell text that is still arriving from text the model has committed
	// to, and merging them would make an in-flight fragment
	// indistinguishable from a finished round. It appears only while a
	// round is open, and only on this live-only event: nothing persists a
	// half-written sentence.
	PartialRound map[string]any `json:"partial_round,omitempty"`
	A2AContext   map[string]any `json:"a2a_context,omitempty"`
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

// SubagentBatched fires once per delegate call, so a dashboard can count
// fan-out work and spot a pathological one.
type SubagentBatched struct {
	ParentHandle string `json:"parent_handle"`
	// TurnID is the RUN that made the call, and WorkKey the unit of work
	// behind it. Neither was here, so the one summary a fan-out emits
	// could be joined to nothing: not to the turn that ran it, not to the
	// per-worker `agent_phase_completed` rows it summarises, and not to
	// `/events?turn_id=`, which its empty column never answered. See
	// ADR-0017.
	TurnID      string `json:"turn_id,omitempty"`
	WorkKey     string `json:"work_key,omitempty"`
	TaskCount   int    `json:"task_count"`
	Successes   int    `json:"successes"`
	Failures    int    `json:"failures"`
	TotalTokens int    `json:"total_tokens"`

	// StartedAt is when the delegate call began, UTC, and Round the round
	// of the parent's phase that made it, on that phase's
	// `tool_executions[].round` scale. Together with each worker record's
	// `host_round` they place a fan-out under the call that spawned it,
	// which the turn iteration alone cannot: one Execute phase spans every
	// round of the iteration. Absent on an older peer's event, and Round
	// on a call made outside a tool loop.
	StartedAt time.Time `json:"started_at,omitzero"`
	Round     int       `json:"round,omitempty"`

	// Graph is the shape the call ran: every task, the worker it used, its
	// topological wave and what it waited for.
	//
	// ON THE EVENT rather than reconstructed from the parent's tool
	// arguments, because those are not on any event a dashboard can read —
	// the phase record carries the CALL, not the arguments. Without this a
	// two-wave call and two independent tasks are indistinguishable
	// afterwards, which is exactly the distinction an operator wondering
	// why a turn took four minutes is trying to make.
	Graph []SubagentNode `json:"graph,omitempty"`

	// Statuses is each task's own outcome, keyed by task id — `ok`,
	// `no_result`, `skipped_dependency_failed`, `timed_out`, and the rest.
	// The counts above cannot say WHICH failed, and in a graph that is the
	// only thing that explains the shape of what came back.
	Statuses map[string]string `json:"statuses,omitempty"`
}

// SubagentNode is one task in a delegate call's graph.
type SubagentNode struct {
	ID string `json:"id"`
	// Worker is the template this task ran, empty for an ad-hoc one.
	Worker string `json:"worker,omitempty"`
	// Wave is the topological depth: 0 for a task that waited on nothing.
	Wave int `json:"wave"`
	// After is what this task waited for, as the parent wrote it.
	After []string `json:"after,omitempty"`
}

// EventType is the "subagent_batched" wire type.
func (SubagentBatched) EventType() string { return "subagent_batched" }

// SummaryFor gives the whole batch in one line — count, successes, failures,
// tokens — because a pathological batch is only recognisable from the ratio,
// and this event carries no role for a dashboard to group the parts under.
func (e SubagentBatched) SummaryFor(actor string) string {
	waves := 1
	for _, n := range e.Graph {
		waves = max(waves, n.Wave+1)
	}
	shape := ""
	if waves > 1 {
		// The one fact the counts cannot carry: a two-wave call took as
		// long as its slowest chain, not its slowest task.
		shape = fmt.Sprintf(", %d waves", waves)
	}
	return lead(actor, fmt.Sprintf("delegated %d tasks (%d ok, %d failed, %d tokens%s)",
		e.TaskCount, e.Successes, e.Failures, e.TotalTokens, shape))
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
