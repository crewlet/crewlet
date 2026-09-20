package types

import "github.com/crewlet/crewlet/internal/events"

// Detached sandbox coding runs. The kick-off turn ends as soon as the job is
// launched and the agent stays busy until the completion signal arrives, so
// these events are the only trace of work that outlives its turn: a run's own
// record is deleted the moment it settles.

func init() {
	events.Register[SandboxRunStarted]()
	events.Register[SandboxRunCompleted]()
	events.Register[SandboxClarificationRequested]()
	events.Register[SandboxRunFailed]()
}

// SandboxRunStarted marks a detached coding job being kicked off, after the
// pending-run row is persisted.
type SandboxRunStarted struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the run this belongs to was dispatched
	// for — see [AgentPhaseCompleted.WorkKey] and ADR-0017. Carried so the
	// work-key filter answers with a run's WHOLE record rather than only
	// its phases.
	WorkKey         string `json:"work_key,omitempty"`
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
//
// LaunchID names WHICH job finished. The row is the turn's and a turn can run
// more than one job, so a duplicate of this signal can arrive after the job it
// was raised for has parked on a question or been replaced by the next one;
// the claim takes the row only while it still holds this launch, running.
type SandboxRunCompleted struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	LaunchID    string `json:"launch_id,omitempty"`
	// WorkKey is the unit of work the run this belongs to was dispatched
	// for — see [AgentPhaseCompleted.WorkKey] and ADR-0017. Carried so the
	// work-key filter answers with a run's WHOLE record rather than only
	// its phases.
	WorkKey     string `json:"work_key,omitempty"`
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
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the run this belongs to was dispatched
	// for — see [AgentPhaseCompleted.WorkKey] and ADR-0017. Carried so the
	// work-key filter answers with a run's WHOLE record rather than only
	// its phases.
	WorkKey         string `json:"work_key,omitempty"`
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
// THE SILENT ENDING, given a voice. `settleFailed` kills the box, deletes the
// run's record and frees the seat, and it published nothing, so a turn that
// had been destroyed presented to the seat, the dashboard and the requester as
// an identical silence. Several distinct conditions funnel into it, and the
// first symptom of any of them was a wait that never ended: the run vanished
// from the active board and the completion event that would have explained it
// had already been acked. A failure has to be at least as loud as the question
// [SandboxClarificationRequested] already announces, and since a settled run
// has no record, this event is the only account of how it ended.
//
// Reason is a closed set — see the SandboxFailure constants — because it is
// the one field a reader acts on differently.
type SandboxRunFailed struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the run this belongs to was dispatched
	// for — see [AgentPhaseCompleted.WorkKey] and ADR-0017. Carried so the
	// work-key filter answers with a run's WHOLE record rather than only
	// its phases.
	WorkKey     string `json:"work_key,omitempty"`
	SandboxID   string `json:"sandbox_id"`
	CodingAgent string `json:"coding_agent"`
	Reason      string `json:"reason"`
	// Detail is a human sentence naming what to do about it, never a stack
	// or a raw provider error: it reaches an operator's board.
	Detail string `json:"detail"`
}

// The reasons a detached run is settled without resuming its turn.
//
// A NAMED SET, because these are not variations of one failure: an
// unreachable box is infrastructure, a missing conversation is a bug in this
// engine, a suspension that could not be recorded is the coordination store or
// this engine, an abandoned tail is a node that died, and a removed seat is an
// operator's own change. An operator seeing them merged into "the sandbox
// failed" would chase the wrong one.
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

	// SandboxFailureSuspensionUnrecorded is a run whose turn suspended but
	// whose conversation never reached the run's record: the runner
	// recorded none, it would not serialize, or the record could not be
	// written and the run is still launching. The job was already
	// executing, so its box is reclaimed rather than left to its TTL. A run
	// that is no longer launching is not ended under this reason: its
	// conversation landed after all, or somebody else already ended it.
	SandboxFailureSuspensionUnrecorded = "suspension_unrecorded"

	// SandboxFailureClaimStranded is a run whose tail this node claimed and
	// then could not give back: the park or the resume the claim was taken
	// for did not land, and the row could not be reverted to the status it
	// was claimed from either. A row left in the at-most-once claim is read
	// by no completion poll, re-claimed by no redelivery and matched by no
	// answer, so the run is ended here rather than left to strand its box
	// until the seat changes hands. It names the coordination store: two
	// writes on one row failed in a row.
	SandboxFailureClaimStranded = "claim_unreverted"

	// SandboxFailureSeatRemoved is a run of a seat that was removed from the
	// company and not restored within the mailbox retirement grace. It is
	// ended when the seat's mailbox is retired, because no resume, answer or
	// completion can reach a seat that is gone.
	SandboxFailureSeatRemoved = "seat_removed"
)

// EventType is the "sandbox_run_failed" wire type.
func (SandboxRunFailed) EventType() string { return "sandbox_run_failed" }

// Role is the seat whose turn was lost.
func (e SandboxRunFailed) Role() string { return e.RoleName }

// AgentID is the instance whose run failed.
func (e SandboxRunFailed) AgentID() string { return e.Agent }

// SummaryFor names the reason, because the whole point of this event is that
// the reasons are told apart.
func (e SandboxRunFailed) SummaryFor(actor string) string {
	reason := e.Reason
	if reason == "" {
		reason = "an unrecorded reason"
	}
	return lead(actor, "lost a sandbox run to "+reason)
}
