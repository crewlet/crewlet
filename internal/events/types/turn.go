package types

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
)

// Turn-engine telemetry: the guards firing, a tool call the required-skill gate
// refused, and the prompt-size meter. These are the events an operator reads
// when a turn behaved oddly but did not fail.
//
// There is no `execute.missing_tool` or `phase.tool_activated`: both detected
// PLAN INCOMPLETENESS, and there is no plan phase. Which tools a turn reached
// for is the `activate_tool` ledger on its phase card, verbatim, in the round
// that asked; a name that resolved to nothing is a failed row in that ledger.
// An event meaning "discovery worked normally" would be a row per activation,
// forever, for a question nobody can ask of it.

func init() {
	events.Register[ToolSkillGuardBlocked]()
	events.Register[PromptSize]()
	events.Register[TurnGuardBreach]()
}

// GuardKind names which runtime invariant a turn breached.
type GuardKind string

const (
	// GuardDepthCap means delegation depth reached its limit.
	GuardDepthCap GuardKind = "depth_cap"
	// GuardStall means two self-iterate rounds produced an unchanged artifact
	// hash.
	GuardStall GuardKind = "stall"
	// GuardMaxIter means the loop exhausted its iteration cap without finishing.
	GuardMaxIter GuardKind = "max_iter"
	// GuardUnhandledException means the turn panicked: in a phase, where the
	// turn loop recovers it, or around one, where the dispatcher or the
	// sandbox resume does. The trigger is recorded and acknowledged rather
	// than redelivered, because a redelivery runs the same defect on the same
	// input. Detail carries the panic's value and never its stack, which
	// stays in the log.
	GuardUnhandledException GuardKind = "unhandled_exception"
	// GuardScheduledTimeout means a scheduled turn exceeded its wall-clock cap.
	GuardScheduledTimeout GuardKind = "scheduled_timeout"
	// GuardRequestRefused means the model's provider refused the request the
	// turn's executor made as built, again in a session of its own: Detail
	// carries the provider's message. No round of the turn is judged on it.
	GuardRequestRefused GuardKind = "request_refused"
)

// ToolSkillGuardBlocked fires when the required-skill guard rejects a tool call
// made before the covering skill was loaded. The call never executes; the model
// gets an instructive error naming the keys to load and retries.
//
// Operators read this as "the agent tried to skip required practices".
// Occasional blocks are the guard working; chronic blocks on one skill say its
// catalogue summary is not landing, or the skill is over-scoped.
type ToolSkillGuardBlocked struct {
	Agent     string   `json:"agent_id"`
	RoleName  string   `json:"role"`
	Phase     Phase    `json:"phase"`
	ToolName  string   `json:"tool_name"`
	SkillKeys []string `json:"skill_keys,omitempty"`
	TurnID    string   `json:"turn_id"`
	// WorkKey is the unit of work this run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey   string `json:"work_key,omitempty"`
	Iteration int    `json:"iteration"`
}

// EventType is the "phase.tool_skill_blocked" wire type.
func (ToolSkillGuardBlocked) EventType() string { return "phase.tool_skill_blocked" }

// Role is the seat whose call was refused.
func (e ToolSkillGuardBlocked) Role() string { return e.RoleName }

// AgentID is the instance that made the refused call.
func (e ToolSkillGuardBlocked) AgentID() string { return e.Agent }

// SummaryFor lists the skill keys, which are the actionable half: they name
// what the agent has to load before the call will go through.
func (e ToolSkillGuardBlocked) SummaryFor(actor string) string {
	return lead(subject(actor, e.Phase),
		fmt.Sprintf("call to '%s' blocked pending required skill(s): %s",
			e.ToolName, strings.Join(e.SkillKeys, ", ")))
}

// PromptSize reports one phase's final prompt size, so prompt-slimming progress
// is measurable. A SEPARATE ROW rather than a derivation: counting
// AgentPhaseCompleted's verbatim prompts means hauling them across the driver,
// the cost schema/0015 took the spend columns out of the payload to avoid.
//
// IT MEASURES ONE PHASE'S OPENING FRAME, not the billed input (round N also
// carries every prior tool result), and not every phase: a delegated worker
// and the round-cap judge publish none.
//
// BYTES, NOT CHARACTERS: len() of a Go string. The units diverge on exactly the
// prompts worth measuring (non-Latin names, emoji, CJK), and a byte is what a
// vendor bounds a request in and we can measure without a tokenizer.
type PromptSize struct {
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	TurnID   string `json:"turn_id"`
	// WorkKey is the unit of work this run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey   string `json:"work_key,omitempty"`
	Iteration int    `json:"iteration"`
	Phase     Phase  `json:"phase"`

	// ApproximateTokens covers every term below it — the system and user
	// text and the tool-definition array — over one chars-per-token ratio.
	// The byte counts ride along so a reader comparing builds can apply
	// their own.
	ApproximateTokens int `json:"approximate_tokens"`

	// SystemBytes and UserBytes are the two messages a phase opens its
	// conversation with — for the executor, the system prompt and the
	// brief its run is launched with.
	SystemBytes int `json:"system_bytes"`
	UserBytes   int `json:"user_bytes"`

	// ToolBytes is the COMPACT JSON size of the tool-definition array offered
	// with the prompt, in ONE CANONICAL SHAPE (`{name, description,
	// parameters}` per tool), so one number means the same whichever harness
	// served the phase. It is a FLOOR: each harness wraps the definitions in its
	// vendor's own shape. And it is the OPENING array: a mid-phase activate_tool
	// adds tools the harness lists from then on.
	ToolBytes int `json:"tool_bytes"`

	// ToolCount is how many definitions those bytes are, because a
	// forty-tool surface and a four-tool one at the same byte count are
	// different problems.
	ToolCount int `json:"tool_count"`
}

// EventType is the "prompt.size" wire type.
func (PromptSize) EventType() string { return "prompt.size" }

// Role is the seat whose prompt was measured.
func (e PromptSize) Role() string { return e.RoleName }

// AgentID is the instance the measurement belongs to.
func (e PromptSize) AgentID() string { return e.Agent }

// SummaryFor reports the approximate token count and the tool term, and leaves
// the byte splits on the payload — the split is for someone comparing builds,
// not for a feed.
//
// The tool term is on the line because it is the one component an operator can
// act on from the feed alone, by trimming what a seat is granted; and because
// a bare "prompt ~N tokens" is what made this meter read as authoritative for
// as long as it was blind to the array. It is rendered whatever it holds: the
// publisher always measures the array, so a zero is a phase that was offered
// no tools, which is worth saying rather than hiding.
func (e PromptSize) SummaryFor(actor string) string {
	return lead(subject(actor, e.Phase), fmt.Sprintf(
		"prompt ~%d tokens (%d tool definitions, %d bytes)",
		e.ApproximateTokens, e.ToolCount, e.ToolBytes))
}

// TurnGuardBreach fires whenever a runtime-invariant guard trips during a turn,
// so every breach reaches the events table and not just the log. Its type is in
// a failure BY TYPE: a breached guard is a failed turn however the payload
// reads.
type TurnGuardBreach struct {
	Agent    string    `json:"agent_id"`
	RoleName string    `json:"role"`
	Kind     GuardKind `json:"kind"`
	Detail   string    `json:"detail"`
	TurnID   string    `json:"turn_id"`
	// WorkKey is the unit of work this run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey string `json:"work_key,omitempty"`
}

// EventType is the "turn.guard_breach" wire type, and one of the four
// [FailureEventNames].
func (TurnGuardBreach) EventType() string { return "turn.guard_breach" }

// Role is the seat whose turn breached the invariant.
func (e TurnGuardBreach) Role() string { return e.RoleName }

// AgentID is the instance running that turn.
func (e TurnGuardBreach) AgentID() string { return e.Agent }

// SummaryFor carries both the guard kind and its detail: the kind says which
// invariant, and only the detail says what tripped it.
func (e TurnGuardBreach) SummaryFor(actor string) string {
	return lead(actor, fmt.Sprintf("guard %s: %s", e.Kind, e.Detail))
}
