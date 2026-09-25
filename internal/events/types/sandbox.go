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
	events.Register[SandboxAnswerGiven]()
	events.Register[SandboxRunAnswered]()
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
	// WorkItem is the item the launching turn is charged to, absent when
	// it is on nothing. The run is part of that turn, so a running-runs
	// panel can say which item a box is working on without joining back to
	// a turn that may have parked days ago.
	WorkItem *WorkItem `json:"work_item,omitempty"`
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
	// WorkItem is the item the parked turn is charged to — the one its row
	// recorded at launch — so a question can be shown against the work it
	// is about. Absent when the turn is on nothing.
	WorkItem *WorkItem `json:"work_item,omitempty"`
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

// SandboxAnswerGiven is the wake an answer BY TURN puts on the seat's own
// inbox: a person answering a parked coding run's question by naming the run,
// rather than by replying on the conversation it was asked in.
//
// # Why a second way in
//
// The chat route matches a reply to a run by conversation, and a run launched
// by anything that is not a conversation — a schedule, a task assignment, a
// colleague's ask — stored none. Such a run parked on a question nobody could
// answer: there was no thread to reply in, and the question waited out its
// pause TTL while the person who knew the answer had nowhere to put it. An
// answer by turn names the run instead, and it can be given from any node.
//
// # Why on the seat's INBOX
//
// Because only the node holding the seat can resume the turn, and the seat's
// inbox is how anything reaches that node — a durable subscription that the
// holder, and only the holder, consumes. So an answer accepted on one node is
// carried out on another, it survives a restart in between, and it WAITS
// BEHIND A PAUSE exactly as a person's chat reply would: a paused seat takes
// nothing off its inbox, this included.
//
// NEVER A TURN. The dispatcher routes it to the coding run it names before
// the inbox screening runs, and whatever that answers — resumed, not
// awaiting, gone — the delivery is spent there: an answer that fell through
// to the ordinary route would wake the seat on a message addressed to a run.
//
// KEPT OUT OF THE EVENT STORE, like the other inbox wakes: what happened to
// the answer is [SandboxRunAnswered], and that the person gave it is their
// `operator_acted` row. See internal/events/category.go.
type SandboxAnswerGiven struct {
	// TurnID is the run the answer is for — the parked run's own key.
	TurnID string `json:"turn_id"`

	// AgentHandle is the seat that run belongs to, which is the inbox this
	// travels on; carried in the payload as well so the dispatcher that
	// routes it can refuse one that reached the wrong seat.
	AgentHandle string `json:"agent_handle"`

	// Answer is the person's reply, verbatim. Redacted where it is spliced
	// into the suspended conversation, like a chat reply.
	Answer string `json:"answer"`

	// AnsweredBy is the credential the answer was given under — the
	// operator token's own id — and AnsweredBySeat the person that token
	// is bound to, empty for a token nobody bound. Two facts for the
	// reason `operator_acted` records two: an audit asks what a credential
	// did, a person reads who answered.
	AnsweredBy     string `json:"answered_by"`
	AnsweredBySeat string `json:"answered_by_seat,omitempty"`
}

// EventType is the "sandbox_answer_given" wire type.
func (SandboxAnswerGiven) EventType() string { return "sandbox_answer_given" }

// SummaryFor names who answered, for a log line: the event itself is never on
// a feed.
func (e SandboxAnswerGiven) SummaryFor(actor string) string {
	who := e.AnsweredBySeat
	if who == "" {
		who = e.AnsweredBy
	}
	if who == "" {
		who = actor
	}
	return lead(who, "answered a parked coding run")
}

// AnswerVia is the route an answer reached a parked coding run by.
type AnswerVia string

const (
	// AnswerViaChat — a reply on the conversation the question was asked
	// in, matched to the run by that conversation.
	AnswerViaChat AnswerVia = "chat"

	// AnswerViaOperator — an answer that named the run by its turn, through
	// the operator surface ([SandboxAnswerGiven]).
	AnswerViaOperator AnswerVia = "operator"
)

// Valid reports whether v is one of the two routes.
func (v AnswerVia) Valid() bool { return v == AnswerViaChat || v == AnswerViaOperator }

// AnswerOutcome is what an answer that reached a parked coding run became.
type AnswerOutcome string

const (
	// AnswerResumed — the answer resumed the suspended turn.
	AnswerResumed AnswerOutcome = "resumed"

	// AnswerNotAwaiting — the run exists and was not waiting for an answer
	// any more: another answer claimed it first, or it is running a job.
	AnswerNotAwaiting AnswerOutcome = "not_awaiting"

	// AnswerGone — the run is over: its record is gone, or it could not be
	// resumed at all and was ended.
	AnswerGone AnswerOutcome = "gone"
)

// Valid reports whether o is one of the three outcomes.
func (o AnswerOutcome) Valid() bool {
	switch o {
	case AnswerResumed, AnswerNotAwaiting, AnswerGone:
		return true
	}
	return false
}

// SandboxRunAnswered records what an answer to a parked coding run's question
// became, on either route.
//
// THE CHAT ROUTE LEFT NO RECORD. A person's reply that resumed a run was an
// ordinary chat message the dispatcher handed to the coordinator, and nothing
// said afterwards that it had been taken as an answer — nor, when the run had
// already been answered or had ended, that the reply had reached nothing. Now
// both routes publish this, once per answer that REACHED a run, with the route
// it came by and who gave it. An answer that is still owed a retry has not
// become anything yet and publishes nothing; a chat message that matched no
// run was never an answer at all.
type SandboxRunAnswered struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey string `json:"work_key,omitempty"`
	// WorkItem is the item the parked turn is charged to, absent when it is
	// on nothing, so an answer is shown against the work it moved.
	WorkItem *WorkItem `json:"work_item,omitempty"`

	Via     AnswerVia     `json:"via"`
	Outcome AnswerOutcome `json:"outcome"`

	// AnsweredBy is who gave the answer: the chat sender's id on that
	// transport, or the operator credential. AnsweredBySeat is the person
	// in the chart where that is known — the seat an operator token is
	// bound to — and empty otherwise.
	AnsweredBy     string `json:"answered_by,omitempty"`
	AnsweredBySeat string `json:"answered_by_seat,omitempty"`
}

// EventType is the "sandbox_run_answered" wire type.
func (SandboxRunAnswered) EventType() string { return "sandbox_run_answered" }

// Role is the seat whose run was answered.
func (e SandboxRunAnswered) Role() string { return e.RoleName }

// AgentID is the instance whose run was answered.
func (e SandboxRunAnswered) AgentID() string { return e.Agent }

// SummaryFor says what the answer became, which is the one thing a reader of
// the feed wants from it.
func (e SandboxRunAnswered) SummaryFor(actor string) string {
	switch e.Outcome {
	case AnswerResumed:
		return lead(actor, "resumed a sandbox run with an answer")
	case AnswerNotAwaiting:
		return lead(actor, "was answered, but its sandbox run was not waiting")
	case AnswerGone:
		return lead(actor, "was answered after its sandbox run had ended")
	}
	return lead(actor, "was answered")
}
