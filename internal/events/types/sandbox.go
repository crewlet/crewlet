package types

import "github.com/crewlet/crewlet/internal/events"

// Detached sandbox coding runs. The kick-off turn ends as soon as the job is
// launched and the agent stays busy until the completion signal arrives, so
// these three events are the only trace of work that outlives its turn.

func init() {
	events.Register[SandboxRunStarted]()
	events.Register[SandboxRunCompleted]()
	events.Register[SandboxClarificationRequested]()
	events.Register[SandboxRunFailed]()
}

// SandboxRunStarted marks a detached coding job being kicked off, after the
// pending-run row is persisted.
type SandboxRunStarted struct {
	Agent           string `json:"agent_id"`
	AgentHandle     string `json:"agent_handle"`
	RoleName        string `json:"role"`
	TurnID          string `json:"turn_id"`
	SandboxID       string `json:"sandbox_id"`
	CodingAgent     string `json:"coding_agent"`
	ConversationKey string `json:"conversation_key"`
	TaskID          string `json:"task_id"`
	// Task is a short human-readable summary for the running-sandboxes panel;
	// the full brief lives on the pending run, not on the wire.
	Task string `json:"task"`
}

// EventType is the "sandbox_run_started" wire type.
func (SandboxRunStarted) EventType() string { return "sandbox_run_started" }

// Role is the seat the detached job belongs to; it stays busy until the run
// reports back.
func (e SandboxRunStarted) Role() string { return e.RoleName }

// AgentID is the instance that launched the run.
func (e SandboxRunStarted) AgentID() string { return e.Agent }

// SummaryFor names the coding agent, which is the one thing distinguishing two
// otherwise identical sandbox jobs on the same seat.
func (e SandboxRunStarted) SummaryFor(actor string) string {
	return lead(actor, "started a sandbox job ("+e.CodingAgent+")")
}

// SandboxRunCompleted signals that a detached coding job finished.
//
// A pure control signal. It carries the original TurnID so the engine can load
// the pending row, reconnect and collect the actual result from the sandbox —
// the outcome (success, delivered refs, tokens) is read at collect time and
// never rides on the event. The engine flips the row running→resumed atomically
// so the resume happens at most once even when this is delivered more than
// once, which it will be: successive poll ticks can both fire before the first
// claim lands, and queue delivery is at-least-once.
type SandboxRunCompleted struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	SandboxID   string `json:"sandbox_id"`
	CodingAgent string `json:"coding_agent"`
}

// EventType is the "sandbox_run_completed" wire type.
func (SandboxRunCompleted) EventType() string { return "sandbox_run_completed" }

// Role is the seat waiting on the run — the seat this signal releases.
func (e SandboxRunCompleted) Role() string { return e.RoleName }

// AgentID is the instance whose suspended turn resumes.
func (e SandboxRunCompleted) AgentID() string { return e.Agent }

// SummaryFor is possessive ("Engineer's sandbox job completed"), so it spells
// the actor into the line rather than going through lead: the job finished, and
// the seat did not do the finishing.
func (e SandboxRunCompleted) SummaryFor(actor string) string {
	if actor == "" {
		return "Sandbox job completed"
	}
	return actor + "'s sandbox job completed"
}

// SandboxClarificationRequested records the in-sandbox coding agent asking a
// person a question.
//
// The sandbox never posts anything itself: it signals through its ask tool, the
// runner surfaces the question and audience, and the engine posts it on the
// audited per-role surface. This event records that routing; the agent goes
// free while it waits, so nothing else marks the pause.
//
// Audience is "requester", "team", "manager" or a handle — an open set, since a
// named colleague is a legitimate audience.
type SandboxClarificationRequested struct {
	Agent           string `json:"agent_id"`
	AgentHandle     string `json:"agent_handle"`
	RoleName        string `json:"role"`
	TurnID          string `json:"turn_id"`
	SandboxID       string `json:"sandbox_id"`
	Question        string `json:"question"`
	Audience        string `json:"audience"`
	ConversationKey string `json:"conversation_key"`
}

// EventType is the "sandbox_clarification_requested" wire type.
func (SandboxClarificationRequested) EventType() string {
	return "sandbox_clarification_requested"
}

// Role is the seat the question is posted as — the sandbox never speaks in its
// own name.
func (e SandboxClarificationRequested) Role() string { return e.RoleName }

// AgentID is the instance whose run asked.
func (e SandboxClarificationRequested) AgentID() string { return e.Agent }

// SummaryFor names the audience, falling back to "someone": an unrouted
// question is still a question, and a line reading "asked  a question" would
// hide that the audience was missing.
func (e SandboxClarificationRequested) SummaryFor(actor string) string {
	audience := e.Audience
	if audience == "" {
		audience = "someone"
	}
	return lead(actor, "asked "+audience+" a question")
}

// SandboxRunFailed records a detached coding run being settled without its
// turn ever resuming.
//
// THE SILENT ENDING, given a voice. `settleFailed` marks the row, kills the
// box and frees the seat — and published nothing, so a turn that had been
// destroyed presented to the seat, the dashboard and the requester as an
// identical silence. Three distinct conditions funnel into it, and the first
// symptom of any of them was a wait that never ended: the run vanished from
// the active board and the completion event that would have explained it had
// already been acked. A failure has to be at least as loud as the question
// [SandboxClarificationRequested] already announces.
//
// Reason is a closed set — see the SandboxFailure constants — because it is
// the one field a reader acts on differently.
type SandboxRunFailed struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	SandboxID   string `json:"sandbox_id"`
	CodingAgent string `json:"coding_agent"`
	Reason      string `json:"reason"`
	// Detail is a human sentence naming what to do about it, never a stack
	// or a raw provider error: it reaches an operator's board.
	Detail string `json:"detail"`
}

// The reasons a detached run is settled without resuming its turn.
//
// A NAMED SET, because the three are not variations of one failure: an
// unreachable box is infrastructure, a missing conversation is a bug in this
// engine, and an abandoned tail is a node that died. An operator seeing them
// merged into "the sandbox failed" would chase the wrong one.
const (
	// SandboxFailureCollect — the job finished but its box could not be
	// read back, so there is no result to splice in.
	SandboxFailureCollect = "collect_unreachable"

	// SandboxFailureNoConversation — the row carried no suspended Execute
	// conversation, so there is nothing to resume into.
	SandboxFailureNoConversation = "no_execute_state"

	// SandboxFailureAbandoned — a tail the previous owner of this seat left
	// mid-flight, found by the recovery pass. Nothing will ever pick it up.
	SandboxFailureAbandoned = "abandoned_tail"
)

// EventType is the "sandbox_run_failed" wire type.
func (SandboxRunFailed) EventType() string { return "sandbox_run_failed" }

// Role is the seat whose turn was lost.
func (e SandboxRunFailed) Role() string { return e.RoleName }

// AgentID is the instance whose run failed.
func (e SandboxRunFailed) AgentID() string { return e.Agent }

// SummaryFor names the reason, because the whole point of this event is that
// the three are told apart.
func (e SandboxRunFailed) SummaryFor(actor string) string {
	reason := e.Reason
	if reason == "" {
		reason = "an unrecorded reason"
	}
	return lead(actor, "lost a sandbox run to "+reason)
}
