package types

import "github.com/crewlet/crewlet/internal/events"

// Detached sandbox coding runs. The kick-off turn ends as soon as the job is
// launched and the agent stays busy until the completion signal arrives, so
// these three events are the only trace of work that outlives its turn.

func init() {
	events.Register(SandboxRunStarted{})
	events.Register(SandboxRunCompleted{})
	events.Register(SandboxClarificationRequested{})
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

func (SandboxRunStarted) EventType() string { return "sandbox_run_started" }

func (e SandboxRunStarted) Role() string    { return e.RoleName }
func (e SandboxRunStarted) AgentID() string { return e.Agent }

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

func (SandboxRunCompleted) EventType() string { return "sandbox_run_completed" }

func (e SandboxRunCompleted) Role() string    { return e.RoleName }
func (e SandboxRunCompleted) AgentID() string { return e.Agent }

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

func (SandboxClarificationRequested) EventType() string {
	return "sandbox_clarification_requested"
}

func (e SandboxClarificationRequested) Role() string    { return e.RoleName }
func (e SandboxClarificationRequested) AgentID() string { return e.Agent }

func (e SandboxClarificationRequested) SummaryFor(actor string) string {
	audience := e.Audience
	if audience == "" {
		audience = "someone"
	}
	return lead(actor, "asked "+audience+" a question")
}
