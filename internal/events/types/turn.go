package types

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
)

// Turn-engine telemetry: the guards firing, the tool surface changing under a
// phase, and the prompt-size meter. These are the events an operator reads when
// a turn behaved oddly but did not fail.

func init() {
	events.Register[ExecuteMissingTool]()
	events.Register[PhaseToolActivated]()
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
	// GuardUnhandledException means an uncaught failure escaped the turn body.
	// The breach is published before that failure propagates, so the record
	// survives even when the turn does not.
	GuardUnhandledException GuardKind = "unhandled_exception"
	// GuardScheduledTimeout means a scheduled turn exceeded its wall-clock cap.
	GuardScheduledTimeout GuardKind = "scheduled_timeout"
)

// ExecuteMissingTool fires when Execute's model calls a tool name that is not
// in its surface — a signal that the plan was incomplete.
type ExecuteMissingTool struct {
	Agent     string   `json:"agent_id"`
	RoleName  string   `json:"role"`
	ToolName  string   `json:"tool_name"`
	PlanTools []string `json:"plan_tools,omitempty"`
}

// EventType is the "execute.missing_tool" wire type. The dotted spelling is
// this file's convention for turn-engine telemetry, and it is what the store
// and the dashboard filter on — it is not interchangeable with an underscore.
func (ExecuteMissingTool) EventType() string { return "execute.missing_tool" }

// Role is the seat whose Execute phase asked for the tool.
func (e ExecuteMissingTool) Role() string { return e.RoleName }

// AgentID is the instance running that phase.
func (e ExecuteMissingTool) AgentID() string { return e.Agent }

// SummaryFor names the tool that was missing, not the plan's tool list: the
// list is on the payload for whoever is diagnosing, and it is far too long for
// a feed line.
func (e ExecuteMissingTool) SummaryFor(actor string) string {
	return lead(actor, "asked for unknown tool '"+e.ToolName+"'")
}

// PhaseToolActivated fires when a phase promotes a catalogue tool into its
// active surface.
//
// Read it per phase: on Plan it is the planner pulling a tool in for recon,
// expected on most non-trivial turns. On Execute it means the executor found a
// tool the plan did not list — plan incompleteness, and chronic occurrences say
// the planner needs more guidance.
type PhaseToolActivated struct {
	Agent     string `json:"agent_id"`
	RoleName  string `json:"role"`
	Phase     Phase  `json:"phase"`
	ToolName  string `json:"tool_name"`
	TurnID    string `json:"turn_id"`
	Iteration int    `json:"iteration"`
}

// EventType is the "phase.tool_activated" wire type.
func (PhaseToolActivated) EventType() string { return "phase.tool_activated" }

// Role is the seat whose phase promoted the tool.
func (e PhaseToolActivated) Role() string { return e.RoleName }

// AgentID is the instance running that phase.
func (e PhaseToolActivated) AgentID() string { return e.Agent }

// SummaryFor leads with actor AND phase, because which phase activated the tool
// is the whole signal: on Plan it is routine recon, on Execute it means the
// plan was incomplete.
func (e PhaseToolActivated) SummaryFor(actor string) string {
	return lead(subject(actor, e.Phase), "activated '"+e.ToolName+"'")
}

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
	Iteration int      `json:"iteration"`
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
// is measurable over time rather than argued about.
type PromptSize struct {
	Agent             string `json:"agent_id"`
	RoleName          string `json:"role"`
	Phase             Phase  `json:"phase"`
	ApproximateTokens int    `json:"approximate_tokens"`
	SystemChars       int    `json:"system_chars"`
	UserChars         int    `json:"user_chars"`
}

// EventType is the "prompt.size" wire type.
func (PromptSize) EventType() string { return "prompt.size" }

// Role is the seat whose prompt was measured.
func (e PromptSize) Role() string { return e.RoleName }

// AgentID is the instance the measurement belongs to.
func (e PromptSize) AgentID() string { return e.Agent }

// SummaryFor reports the approximate token count and leaves the character
// splits on the payload — the split is for someone comparing builds, not for a
// feed.
func (e PromptSize) SummaryFor(actor string) string {
	return lead(subject(actor, e.Phase),
		fmt.Sprintf("prompt ~%d tokens", e.ApproximateTokens))
}

// TurnGuardBreach fires whenever a runtime-invariant guard trips during a turn,
// so every breach reaches the events table and not just the log. Its type is in
// FailureEventTypes: a breached guard is a failed turn however the payload
// reads.
type TurnGuardBreach struct {
	Agent    string    `json:"agent_id"`
	RoleName string    `json:"role"`
	Kind     GuardKind `json:"kind"`
	Detail   string    `json:"detail"`
	TurnID   string    `json:"turn_id"`
}

// EventType is the "turn.guard_breach" wire type, and one of the four names in
// FailureEventTypes.
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
