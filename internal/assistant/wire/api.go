package wire

import (
	"time"

	"github.com/google/uuid"
)

// The request and answer bodies of the `/assistant/` routes. Every POST and
// PUT body names the [ProtocolVersion] it was written against, and a version
// the server does not speak is `409 assistant_protocol_mismatch`.

// Versioned is a body that carries nothing but the version: creating a
// conversation, stopping a turn.
type Versioned struct {
	ProtocolVersion int `json:"protocol_version"`
}

// TurnRequest asks a question: `POST /assistant/conversations/{id}/turns`.
type TurnRequest struct {
	ProtocolVersion int       `json:"protocol_version"`
	Input           TurnInput `json:"input"`
	// PageContext is null when the person removed the page chip.
	PageContext *PageContext `json:"page_context"`
	// Mode is what the question asks for; absent is [TurnAsk]. A
	// [TurnBrief] needs the page context, which says what it covers.
	Mode TurnMode `json:"mode,omitempty"`
	// --- deep work (im-deep) ---
	// Attachments are files the person uploaded to the conversation for
	// this question, at most [AttachmentsPerTurn].
	Attachments []uuid.UUID `json:"attachments,omitempty"`
}

// TurnInput is the person's question.
type TurnInput struct {
	Text string `json:"text"`
}

// TurnAccepted answers a question with the turn it started and where to read
// it. A retry under the same Idempotency-Key answers the same turn.
type TurnAccepted struct {
	TurnID    uuid.UUID `json:"turn_id"`
	StreamURL string    `json:"stream_url"`
	AfterSeq  uint64    `json:"after_seq"`
}

// ProposalDecision approves or declines a proposal, echoing the digest the
// card showed: `POST /assistant/proposals/{id}/approve` and `/decline`.
type ProposalDecision struct {
	ProtocolVersion int    `json:"protocol_version"`
	Digest          string `json:"digest"`
	// PageContext is the screen the person decided from, as it is now: the
	// turn the decision resumes runs on it, so it reads the draft an
	// applied change left and offers the actions the screen accepts. Null
	// when the person removed the page chip.
	PageContext *PageContext `json:"page_context,omitempty"`
}

// ProposalAnswer is what a decision answers: the proposal's state now and,
// for an approval, where the resumed turn streams from.
type ProposalAnswer struct {
	Proposal  ProposalRef `json:"proposal"`
	TurnID    uuid.UUID   `json:"turn_id"`
	StreamURL string      `json:"stream_url,omitempty"`
	AfterSeq  uint64      `json:"after_seq,omitempty"`
}

// ProposalRef is a proposal by id and state.
type ProposalRef struct {
	ID    uuid.UUID     `json:"id"`
	State ProposalState `json:"state"`
}

// ActionReport is the page's claim on, then its outcome of, a page action it
// was sent: `PUT /assistant/actions/{id}`. A claim ([ActionApplying]) another
// tab already made is `409 action_already_claimed`.
type ActionReport struct {
	ProtocolVersion int         `json:"protocol_version"`
	State           ActionState `json:"state"`
	// Revision is the page's revision when it reported.
	Revision string `json:"revision"`
	Reason   string `json:"reason,omitempty"`
	Receipt  string `json:"receipt,omitempty"`
	// PageContext is the screen as it is after the action, on the report
	// that settles it ([ProposalDecision.PageContext]).
	PageContext *PageContext `json:"page_context,omitempty"`
}

// ActionAnswer is the action's state after a report.
type ActionAnswer struct {
	ActionID uuid.UUID   `json:"action_id"`
	State    ActionState `json:"state"`
}

// StopAnswer is a stopped turn's state.
type StopAnswer struct {
	State TurnState `json:"state"`
}

// PurgeAnswer is a conversation's purge: the state log's own outcome and the
// position the purge record landed at.
type PurgeAnswer struct {
	Outcome  string  `json:"outcome"`
	Position *string `json:"position"`
}

// AuditReadRequest is an administrator's compliance read of somebody else's
// conversation (D06): `POST /assistant/audit/conversations/{id}/read`, with the
// reason they give, which is recorded beside their name and shown to the
// owner.
type AuditReadRequest struct {
	ProtocolVersion int    `json:"protocol_version"`
	Reason          string `json:"reason"`
}

// ConversationList is the person's conversations, newest first.
type ConversationList struct {
	Conversations []ConversationSummary `json:"conversations"`
	NextCursor    string                `json:"next_cursor,omitempty"`
}

// ConversationCreated answers a create.
type ConversationCreated struct {
	Conversation ConversationSummary `json:"conversation"`
}

// ConversationSummary is one conversation in a list.
type ConversationSummary struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// LastActivityAt is when the conversation was last used (a turn opened,
	// a message, a step, a card or a proposal recorded), the moment a listing
	// is ordered by and so the one it shows. UpdatedAt also moves on a change
	// that is not activity (a new title), so a list showing it disagrees with
	// its own order.
	LastActivityAt time.Time `json:"last_activity_at"`
	// LastTurnState is the newest turn's state, absent before the first.
	LastTurnState TurnState `json:"last_turn_state,omitempty"`
}

// Transcript is a conversation as its owner reads it after a reload, and as
// an administrator's audited read returns it.
//
// THE CARDS ARE RE-CHECKED ON EVERY READ: a card whose app the reader no
// longer holds is replaced by a notice, and a proposal card carries its state
// now rather than when it was drawn.
type Transcript struct {
	Conversation ConversationSummary `json:"conversation"`
	Messages     []Message           `json:"messages"`
	Steps        []StepEvent         `json:"steps"`
	Cards        []CardEvent         `json:"cards"`
	// OpenTurn is the turn still running or parked, and where to read it.
	OpenTurn *OpenTurn `json:"open_turn"`
	// AuditReads are the compliance reads of this conversation, so its
	// owner sees who read it and why (D06).
	AuditReads []AuditRead `json:"audit_reads"`
	// Turns are how each of its turns stands, oldest first: what a reload
	// draws each turn's outcome line from ("Stopped. Nothing changed.", a
	// failure's sentence), as the live stream drew it from the done and
	// error frames. Without them a reloaded turn that was stopped or failed
	// before it wrote a word read as completed and drew nothing at all.
	Turns []TurnOutcome `json:"turns"`
}

// TurnOutcome is how one turn of a transcript stands.
type TurnOutcome struct {
	TurnID uuid.UUID `json:"turn_id"`
	State  TurnState `json:"state"`
	// Error is the turn-level failure it ended with, in its code's sentence
	// ([ErrorCode.Sentence]); absent for a turn that did not fail.
	Error *ErrorEvent `json:"error,omitempty"`
}

// Message is one message in a transcript.
type Message struct {
	ID        uuid.UUID   `json:"id"`
	TurnID    uuid.UUID   `json:"turn_id"`
	Role      MessageRole `json:"role"`
	Text      string      `json:"text"`
	CreatedAt time.Time   `json:"created_at"`
	// Page is the chip a user message was sent with, absent when the
	// person sent it without the page.
	Page *PageChip `json:"page,omitempty"`
	// --- deep work (im-deep) ---
	// Attachments are the files a user message was sent with.
	Attachments []FileRef `json:"attachments,omitempty"`
}

// OpenTurn is a turn a client re-attaches to after a reload.
type OpenTurn struct {
	TurnID    uuid.UUID `json:"turn_id"`
	State     TurnState `json:"state"`
	StreamURL string    `json:"stream_url"`
	AfterSeq  uint64    `json:"after_seq"`
}

// AuditRead is one administrator's compliance read of a conversation.
type AuditRead struct {
	// Reader is the administrator as the audit trail names an author.
	Reader string    `json:"reader"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// ViewerAssistant is the `assistant` member of the `/viewer` answer, so the
// Ask pill decides without a second request.
type ViewerAssistant struct {
	Available bool         `json:"available"`
	Reason    ViewerReason `json:"reason,omitempty"`
	// Model is what a [ViewerModelNotReady] reason means for this person,
	// absent for every other reason.
	Model *ModelNotice `json:"model,omitempty"`
}

// ModelNotice is the model problem a person is told before they ask: the
// error code a turn would fail with, and the same sentence and hint.
type ModelNotice struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Hint    string    `json:"hint,omitempty"`
	// RetryAt is when the model is tried again, for a spent plan whose
	// reset is known.
	RetryAt *time.Time `json:"retry_at,omitempty"`
}
