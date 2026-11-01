package types

import "github.com/crewlet/crewlet/internal/events"

func init() {
	events.Register[AssistantConversationRead]()
}

// AssistantConversationRead is one administrator's compliance read of somebody
// else's Dashboard Assistant conversation (decision D06): who read it, through
// which credential, whose conversation it was, and the reason they gave.
// Published with [OperatorSource] on its envelope by the node that opened the
// conversation, after the read was recorded in the conversation's own domain
// (which is what its owner is shown) and before the conversation is answered.
//
// THE REASON IS HERE, AND THE CONVERSATION IS NOT: an audited read is only
// worth the sentence the reader gave for it, and the audit log is read by the
// administrators the sentence is accountable to; what was read stays sealed in
// its own domain.
type AssistantConversationRead struct {
	// ActorName, ActorKind and OperatorID are the reader in the three halves
	// of [OperatorActed.ActorName].
	ActorName  string `json:"actor"`
	ActorKind  string `json:"actor_kind"`
	OperatorID string `json:"operator_id"`

	// ConversationID is the conversation read, and how the audit log lists
	// every read of it; OwnerID is the identity-directory id of the person
	// whose conversation it is.
	ConversationID string `json:"conversation_id"`
	OwnerID        string `json:"owner_id"`

	// FileID is the one file of the conversation read, for a compliance
	// download of a file it keeps; empty for a read of the transcript.
	FileID string `json:"file_id,omitempty"`

	// Reason is what the reader gave as the reason for the read.
	Reason string `json:"reason"`
}

// EventType is the "assistant_conversation_read" wire type.
func (AssistantConversationRead) EventType() string { return "assistant_conversation_read" }

// Actor is the administrator who read the conversation.
func (e AssistantConversationRead) Actor() string { return e.ActorName }

// SummaryFor names the read and its reason.
func (e AssistantConversationRead) SummaryFor(actor string) string {
	if e.FileID != "" {
		return lead(actor, "downloaded a file of an Ask Crewlet conversation for compliance: "+
			e.Reason)
	}
	return lead(actor, "read an Ask Crewlet conversation for compliance: "+e.Reason)
}
