package types

import (
	"strconv"

	"github.com/crewlet/crewlet/internal/events"
)

// The Dashboard Assistant's runtime audit: what a person's assistant did for
// them, beside what they did themselves ([OperatorActed]).
//
// # Every call, never its content
//
// A turn's reads are the person's own requests, each decided by the route it
// asked exactly as their click would be, so none of them writes a record of
// its own; what the audit log needs is that the assistant asked them, and in
// which order. So a turn's record names every step's tool, the route pattern
// it sent, what became of it and how many bytes came back, and NEVER an
// argument, a query value, an answer or the person's question: those are the
// company's content, already behind the routes that serve it, and a person's
// question is theirs (ADR-0053). A write the assistant proposed is recorded
// where it executes, by the act route's own [OperatorActed] under the
// assistant transport, once the person approved it.

func init() {
	events.Register[AssistantTurnCompleted]()
}

// AssistantTurnCompleted is one segment of a person's assistant turn ending:
// answered, stopped, failed, parked on a proposal, or out of budget. Published
// by the node that ran it, with [OperatorSource] on its envelope, so the Audit
// log reads it beside every other call a person made.
type AssistantTurnCompleted struct {
	// ActorName, ActorKind and OperatorID are the person in the three
	// halves of [OperatorActed.ActorName]: the seat they hold, `human`, and
	// the browser session the turn was asked through.
	ActorName  string `json:"actor"`
	ActorKind  string `json:"actor_kind"`
	OperatorID string `json:"operator_id"`

	// ConversationID and TurnID name the turn; the conversation id is also
	// how the audit log lists every turn of one conversation.
	//
	// `assistant_turn_id` AND NEVER `turn_id`: that key names a SEAT's turn
	// on every event that carries it, and the Turn screen's query collects
	// every stored event carrying it, so an assistant turn spelled so would
	// be drawn inside some seat's turn with which it shares nothing.
	ConversationID string `json:"conversation_id"`
	TurnID         string `json:"assistant_turn_id"`

	// State is how the segment ended, in the assistant protocol's own
	// vocabulary (internal/assistant/wire's TurnState), and ErrorCode the
	// turn-level failure, if any (its ErrorCode).
	State     string `json:"state"`
	ErrorCode string `json:"error_code,omitempty"`

	// PromptVersion is the system prompt's version the turn ran under, so
	// a change in behaviour can be read against a change in the prompt.
	PromptVersion string `json:"prompt_version"`
	// Model and ProviderKey are what served the turn's last round.
	Model       string `json:"model,omitempty"`
	ProviderKey string `json:"provider_key,omitempty"`

	// DurationMS is the segment's own clock, and Steps every tool step it
	// took, in order.
	DurationMS int64                `json:"duration_ms"`
	Steps      []AssistantAuditStep `json:"steps"`

	// Failed is stamped for a segment that ended failed, interrupted or
	// out of budget, so the event store tags the row and the log's failure
	// filter finds it.
	Failed bool `json:"failed,omitempty"`
}

// AssistantAuditStep is one tool step as the audit records it: no arguments
// and no answer.
type AssistantAuditStep struct {
	Tool string `json:"tool"`
	// Method and Route are the route pattern the step sent, empty for a
	// step that sent nothing (a write that became a proposal, a call
	// refused before it was sent).
	Method string `json:"method,omitempty"`
	Route  string `json:"route,omitempty"`
	// Outcome is the step's state (internal/assistant/wire's StepState).
	Outcome string `json:"outcome"`
	// Status is the HTTP status the route answered, zero when nothing was
	// sent.
	Status int `json:"status,omitempty"`
	// Bytes is how much of the answer was read.
	Bytes int `json:"bytes,omitempty"`
	// Refusal is the API's own code for a refused step.
	Refusal string `json:"refusal,omitempty"`
}

// EventType is the "assistant_turn_completed" wire type.
func (AssistantTurnCompleted) EventType() string { return "assistant_turn_completed" }

// Actor is the person whose assistant it was.
func (e AssistantTurnCompleted) Actor() string { return e.ActorName }

// SummaryFor says how the segment ended and how many steps it took.
func (e AssistantTurnCompleted) SummaryFor(actor string) string {
	summary := "ran an Ask Crewlet turn: " + e.State + ", " + strconv.Itoa(len(e.Steps)) + " steps"
	if e.ErrorCode != "" {
		summary += " (" + e.ErrorCode + ")"
	}
	return lead(actor, summary)
}

// assistantFailedStates are the segment endings [AssistantTurnCompleted.Failed]
// is stamped for, spelled as the protocol spells them.
var assistantFailedStates = map[string]bool{
	"failed": true, "interrupted": true, "budget_exhausted": true, "expired": true,
}

// NewAssistantTurnCompleted builds the audit record for one segment, with
// [AssistantTurnCompleted.Failed] derived from its state rather than set beside
// it.
func NewAssistantTurnCompleted(e AssistantTurnCompleted) AssistantTurnCompleted {
	e.Failed = assistantFailedStates[e.State]
	if e.Steps == nil {
		e.Steps = []AssistantAuditStep{}
	}
	return e
}
