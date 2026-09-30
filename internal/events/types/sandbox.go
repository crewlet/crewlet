package types

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/events"
)

// Detached sandbox coding runs. The kick-off turn ends as soon as the job is
// launched and the agent stays busy until the completion signal arrives, so
// these events are the only trace of work that outlives its turn: a run's own
// record is deleted the moment it settles.

func init() {
	events.Register[SandboxRunStarted]()
	events.Register[SandboxRunCompleted]()
	events.Register[SandboxClarificationRequested]()
	events.Register[SandboxRunFailed]()
	events.Register[SandboxRunUsage]()
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

// SandboxRunUsage is what one collected coding run reported spending in its
// box: its tokens, and its price where the coding agent quotes one.
//
// ITS OWN RECORD, PUBLISHED AT THE COLLECT, because the collect is where a
// run's spend is known and the only moment every run reaches: the budgets are
// charged there, and so is the tracker task the turn was spent on. Carried on
// the phase the run's turn resumed into, it reached no token view for a run
// that never resumed — a question nobody answered, a resume abandoned, a node
// that died holding the claim — although the budgets had counted it.
//
// ONE RECORD PER LAUNCH. Its id is derived from the turn and the launch
// ([PendingRun.LaunchID] in internal/sandbox) and its timestamp is the FIRST
// collect of that launch, which the run's own record keeps across a retry —
// so a completion collected again after a failed resume publishes this same
// event, identical in the two fields the event store keys a row on and the
// live projection dedupes by. A phase carrying the run could be published
// again at another instant, and a run retried across the edge of a queried
// window was counted in both.
//
// A SPEND RECORD, like [AgentPhaseCompleted]: the Tokens view, the turns list
// and the Turn screen count it, under the execute phase that launched the run
// and under its CodingAgent where a phase's figures stand under its model —
// the box reports no model, and the coding agent is what spent them.
type SandboxRunUsage struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	LaunchID    string `json:"launch_id"`
	// WorkKey is the unit of work the run this belongs to was dispatched
	// for — see [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey     string `json:"work_key,omitempty"`
	SandboxID   string `json:"sandbox_id"`
	CodingAgent string `json:"coding_agent"`
	// InputTokens, OutputTokens and TotalTokens are what the box reported,
	// never negative: a negative count off a coding agent's output is a bad
	// payload, and counted it would refund spend that happened.
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// CostUSD is the price the coding agent quoted, zero where it quotes
	// none — a subscription CLI's marginal cost, or OpenCode, which reports
	// no usage at all.
	CostUSD float64 `json:"cost_usd"`
}

// EventType is the "sandbox_run_usage" wire type.
func (SandboxRunUsage) EventType() string { return "sandbox_run_usage" }

// Role is the seat whose run spent it — the seat its budget is charged to.
func (e SandboxRunUsage) Role() string { return e.RoleName }

// AgentID is the instance that launched the run.
func (e SandboxRunUsage) AgentID() string { return e.Agent }

// SummaryFor is possessive, as [SandboxRunCompleted]'s is — the box spent it,
// not the seat — and names the price only where one was quoted: "$0.00" beside
// a run nobody priced would be a price nobody gave.
func (e SandboxRunUsage) SummaryFor(actor string) string {
	agent := e.CodingAgent
	if agent == "" {
		agent = "coding"
	}
	spent := fmt.Sprintf("%s run spent %d tokens", agent, e.TotalTokens)
	if e.CostUSD > 0 {
		spent += fmt.Sprintf(" ($%.2f)", e.CostUSD)
	}
	if actor == "" {
		return "A " + spent
	}
	return actor + "'s " + spent
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
	// for did not land, or the collect was held because what the run spent
	// was not recorded yet, and the row could not be reverted to the status
	// it was claimed from either. A row left in the at-most-once claim is
	// read by no completion poll, re-claimed by no redelivery and matched by
	// no answer, so the run is ended here rather than left to strand its box
	// until the seat changes hands. The failed hand-back is the coordination
	// store's; the failure before it is the store's after a park or a
	// resume, and after a held collect whichever of the broker, the tracker
	// or the token counter did not record the run's spend.
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
