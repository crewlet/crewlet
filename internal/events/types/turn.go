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
// Two types are GONE rather than merely unpublished, and the difference
// matters: `execute.missing_tool` and `phase.tool_activated` were both defined
// to detect PLAN INCOMPLETENESS — "the executor found a tool the plan did not
// list" — and there is no plan phase. `ExecuteMissingTool` carried the plan's
// own tool list as `plan_tools`. Neither ever had a producer, so no store holds
// a row of either, and nothing had to be migrated.
//
// The question each was asking is either dead or already answered better. Which
// tools a turn reached for is the `activate_tool` ledger on its own phase card,
// verbatim, in the round that asked; a name that resolved to nothing is a
// failed row in that same ledger. An event that fires on every non-trivial turn
// and means "discovery worked normally" is a row per activation, forever, for a
// question nobody can now ask of it — the docs had already downgraded it to
// "Routine", which is the shape of a signal that has stopped being one.

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
// is measurable over time rather than argued about.
//
// A SEPARATE ROW rather than a derivation, and that is the whole reason it
// exists: AgentPhaseCompleted carries both prompts verbatim, so the size is
// technically already stored — and reading it back means hauling every phase's
// whole prompt and response across the driver to count them in Go, which is
// exactly the cost schema/0015 promoted the spend columns out of the payload
// to avoid. The measured integers — the text a phase opens with, or the parked
// conversation a resumed one re-enters instead, the tool definitions offered
// alongside either, how many there were, and one approximation over the lot —
// answer the question at a scan.
//
// WHAT IT DOES NOT MEASURE, because a meter read as more than it is, is worse
// than no meter. This row is one phase's OPENING frame: not the whole phase's
// billed input, which on round N also carries every prior round's tool results,
// and not every phase in the engine — a delegated worker and the round-cap
// judge publish none of these at all.
//
// BYTES, NOT CHARACTERS, UNDER KEYS THAT SAY CHARS — and the split between
// those two halves is the whole of it. The measurement is len() of a Go
// string, which is bytes, and the wire keys have said `chars` since the event
// was written. Nothing rounded that back: the dashboard renders both with a
// byte formatter, so a panel headed "System" printed "24 KB" under a tooltip
// asserting it had counted characters. The units coincide on ASCII and
// diverge on exactly the prompts worth measuring — a roster of non-Latin
// names, a Slack thread with emoji in it, a CJK knowledge block — where a
// UTF-8 rune is two to four bytes and "the count" silently became a different
// quantity per company. The byte is the honest one here anyway: it is what a
// vendor bounds a request body in and what this engine can measure without a
// tokenizer.
//
// So the Go names, the doc, the dashboard's labels and the docs say bytes,
// and THE KEYS DO NOT MOVE. A key is an identifier rather than an assertion —
// nothing decodes `system_chars` and divides by a rune width — so renaming it
// corrects no arithmetic and costs every row already written. ADR-0006 is
// additive-only and marked `Tag-status: unreleased`, which is to say it binds
// before any release precisely because a rolling upgrade is a contract
// between PEERS: a renamed tag is a dropped field on whichever half has not
// upgraded yet. Here the drop would not even read as absence — the payload is
// stored verbatim and read back raw by the Turn screen over a 30-day window,
// where a missing key coerces to 0 and renders "0 B" beside a correct
// `approximate_tokens`. A confidently wrong row, on every turn taken before
// the upgrade, from one restart of one node. `plan_summary` and the `execute`
// phase are frozen for the same reason, and say so where they are defined.
//
// Adding a second key instead is the other half of the trap: one integer with
// two identities, undeletable under ADR-0006, and — since scalars here carry
// no omitempty — a build relaying an older event would emit
// `"system_bytes":0` beside a truthful `"system_chars":24000`, asserting zero
// rather than saying nothing.
//
// Addressed like every other phase event, so the size a turn actually paid is
// readable on that turn rather than only in aggregate.
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
	// text, a resumed phase's seeded conversation, and the tool-definition
	// array — over one chars-per-token ratio. The byte counts ride along so
	// a reader comparing builds can apply their own.
	ApproximateTokens int `json:"approximate_tokens"`

	// SystemBytes and UserBytes are the two messages a phase OPENS a
	// conversation with. Both are zero on a resumed phase, which prepends
	// neither: what it sends is MessageBytes.
	//
	// A BYTE COUNT under a key that says chars. The Go name is what is
	// honest; the key is frozen by ADR-0006 — see the note above, and grep
	// for either spelling to land on it. The three counts below carry the
	// same split for the same reason.
	SystemBytes int `json:"system_chars"`
	UserBytes   int `json:"user_chars"`

	// MessageBytes is the conversation a RESUMED phase re-enters — its
	// original system and user messages, the assistant rounds since, and
	// the tool results they collected — and zero for a phase that opens one
	// of its own. A detached coding run stops the executor mid-loop and the
	// loop is re-entered later, so its prompt is a message list rather than
	// a pair of strings; measuring only the pair reported every resumed
	// executor as a phase with no prompt at all.
	//
	// WHAT IS COUNTED, per message: every message's text, each assistant
	// round's reasoning, and the compact JSON of its tool calls' arguments.
	// The reasoning is counted ONCE — the structured thinking blocks where a
	// round has them, the reasoning prose where it does not, never both,
	// because on Anthropic the prose is a rendering of the same thinking and
	// summing the two would double the biggest term a parked turn carries.
	// Reasoning is in the figure at all because it is billed: Anthropic puts
	// every thinking block back into the request and charges for it, and the
	// `cli-agent` text backend writes the prose into the prompt literally.
	//
	// WHAT IS STILL OUT, in the same spirit as ToolBytes naming the
	// cli-agent fence as the larger rendering: a thinking block's signature
	// — a fixed-size opaque token the provider mints per block, so counting
	// it would move this figure with a vendor's token format rather than
	// with the prompt — and each tool call's id and name, bounded
	// identifiers beside arguments that run to kilobytes. Out too is every
	// per-message envelope a backend adds around all of it (role labels,
	// content-block framing, the cli-agent transcript's own `## assistant`
	// headings), so on any backend the real figure is larger than this one.
	MessageBytes int `json:"message_chars"`

	// ToolBytes is the COMPACT JSON size of the tool-definition array
	// offered with the prompt — the shape both HTTP vendors put on the
	// wire, so the figure is comparable across providers. The `cli-agent`
	// text backend renders those same definitions INDENTED, inside a fenced
	// catalogue and followed by a response contract, so on that backend the
	// real figure is larger than this one; a number whose rendering is not
	// recorded gets compared against a different number.
	//
	// ROUND ONE'S ARRAY. The tool loop re-reads the surface at the top of
	// every round precisely so a mid-phase activate_tool is offered on the
	// next call, so a phase that promotes three MCP tools sends more than
	// this says.
	ToolBytes int `json:"tool_chars"`

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
// the character splits on the payload — the split is for someone comparing
// builds, not for a feed.
//
// The tool term is on the line because it is the one component an operator can
// act on from the feed alone, by trimming what a seat is granted; and because
// a bare "prompt ~N tokens" is what made this meter read as authoritative for
// as long as it was blind to the array.
func (e PromptSize) SummaryFor(actor string) string {
	return lead(subject(actor, e.Phase),
		fmt.Sprintf("prompt ~%d tokens (%d tool definitions, %d chars)",
			e.ApproximateTokens, e.ToolCount, e.ToolBytes))
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
	// WorkKey is the unit of work this run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey string `json:"work_key,omitempty"`
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
