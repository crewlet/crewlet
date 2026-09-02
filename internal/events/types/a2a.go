package types

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
)

// Agent-to-agent channels: one ask, one answer, then closed. These events are
// the audit trail of that exchange — the delivery itself rides the target
// seat's durable inbox, never a second in-process path.

// The two WAKE types an A2A exchange puts on a seat's inbox.
//
// They are named here, beside the audit events, rather than spelled inline
// where they are minted, because the completion ledger keys on them: a
// producer and a consumer that disagree about a type name never raise — the
// ledger simply records nothing under a name nobody looks up, and every
// redelivery runs the turn again.
const (
	// A2ARequestType wakes the TARGET of an ask.
	A2ARequestType = "a2a_request"

	// A2AMessageType wakes the REQUESTER with the answer. This hop lands
	// on a channel that is already closed by design, so the completion
	// ledger is the only thing standing between a redelivery and a second
	// turn spent acting on the same answer.
	A2AMessageType = "a2a_message"
)

func init() {
	events.Register[A2ARequest]()
	events.Register[A2AMessage]()
	events.Register[A2AChannelOpened]()
	events.Register[A2AMessageSent]()
	events.Register[A2AMessageDelivered]()
	events.Register[A2AChannelClosed]()
}

// A2ARequest is the wake a colleague's ask puts on the TARGET's inbox.
//
// TYPED, and that is a correction rather than a preference. Both wakes used to
// travel as a free-form Payload bag holding the brief under a "content" key,
// and nothing in the engine read that key: the turn's ask was assembled by
// [Briefer]-less fallback and the target seat was woken with the literal
// string "(a2a_request)" instead of the question. A registered payload is what
// makes the ask reachable — and what makes it survive a round-trip through a
// build that predates it.
type A2ARequest struct {
	ChannelID string `json:"channel_id"`
	Requester string `json:"requester"`
	// SenderRole is the asking seat's role name, so the answer's author is
	// identifiable without a second lookup.
	SenderRole string `json:"sender_role"`
	// Content is the brief — the question itself, verbatim.
	Content string `json:"content"`
}

// EventType is the "a2a_request" wire type.
func (A2ARequest) EventType() string { return A2ARequestType }

// Brief is the colleague's question, which IS the woken seat's whole task.
func (e A2ARequest) Brief() string {
	who := e.SenderRole
	if who == "" {
		who = e.Requester
	}
	if who == "" {
		return e.Content
	}
	return "A colleague (" + who + ") asks:\n\n" + e.Content
}

// Actor is the colleague who asked, not the bus that relayed it.
func (e A2ARequest) Actor() string { return e.Requester }

// Summary is the dashboard's one-liner for the ask.
func (e A2ARequest) Summary() string {
	return lead(e.Requester, "asked a colleague on "+e.ChannelID)
}

// A2AMessage is the wake an answer puts on the REQUESTER's inbox.
type A2AMessage struct {
	ChannelID string `json:"channel_id"`
	Sender    string `json:"sender"`
	// SenderRole is the answering seat's role name.
	SenderRole string `json:"sender_role"`
	// Question is the ask this answers, carried so the requester's next
	// turn reads the exchange rather than a reply with no antecedent. The
	// channel is closed by the time this lands, so there is nowhere else
	// left to fetch it from.
	Question string `json:"question,omitempty"`
	// Content is the answer, verbatim.
	Content string `json:"content"`
}

// EventType is the "a2a_message" wire type.
func (A2AMessage) EventType() string { return A2AMessageType }

// Brief is the answer, under the question it answers.
func (e A2AMessage) Brief() string {
	who := e.SenderRole
	if who == "" {
		who = e.Sender
	}
	var b strings.Builder
	if e.Question != "" {
		b.WriteString("You asked a colleague:\n\n" + e.Question + "\n\n")
	}
	if who != "" {
		b.WriteString("Their answer (" + who + "):\n\n")
	}
	b.WriteString(e.Content)
	return b.String()
}

// Actor is the colleague who answered.
func (e A2AMessage) Actor() string { return e.Sender }

// Summary is the dashboard's one-liner for the answer.
func (e A2AMessage) Summary() string {
	return lead(e.Sender, "answered a colleague on "+e.ChannelID)
}

// A2AChannelOpened marks a channel being created between two agents.
type A2AChannelOpened struct {
	ChannelID    string   `json:"channel_id"`
	Requester    string   `json:"requester"`
	Target       string   `json:"target"`
	Participants []string `json:"participants,omitempty"`
}

// EventType is the "a2a_channel_opened" wire type.
func (A2AChannelOpened) EventType() string { return "a2a_channel_opened" }

// Summary leads with the requester the payload names, not the resolved actor:
// the publisher is the A2A service, and attributing the ask to it would hide
// which colleague actually asked.
func (e A2AChannelOpened) Summary() string {
	return lead(e.Requester, "opened A2A channel with "+e.Target)
}

// A2AMessageSent marks a message put on an A2A channel. It is also a trigger
// the learning subsystem normalizes into an inbound interaction, which is why
// Sender and Content are the fields that matter downstream.
type A2AMessageSent struct {
	ChannelID  string `json:"channel_id"`
	Sender     string `json:"sender"`
	SenderRole string `json:"sender_role"`
	Recipient  string `json:"recipient"`
	MessageID  string `json:"message_id"`
	Content    string `json:"content"`
}

// EventType is the "a2a_message_sent" wire type. Distinct from A2AMessageType,
// which is the inbox WAKE — this is the audit record of the same message.
func (A2AMessageSent) EventType() string { return "a2a_message_sent" }

// SummaryFor prefers the message's own sender, for the same reason MessageSent
// does: the publisher is the bus, not the colleague who asked.
func (e A2AMessageSent) SummaryFor(actor string) string {
	who := e.Sender
	if who == "" {
		who = actor
	}
	phrase := "sent A2A message on " + e.ChannelID
	if e.Recipient != "" {
		phrase += " → " + e.Recipient
	}
	return lead(who, phrase)
}

// A2AMessageDelivered marks messages being read by the target agent.
type A2AMessageDelivered struct {
	ChannelID          string `json:"channel_id"`
	Recipient          string `json:"recipient"`
	Sender             string `json:"sender"`
	MessageCount       int    `json:"message_count"`
	TotalContentLength int    `json:"total_content_length"`
}

// EventType is the "a2a_message_delivered" wire type.
func (A2AMessageDelivered) EventType() string { return "a2a_message_delivered" }

// Summary leads with the recipient, because delivery is something that happened
// TO a seat: it is the read, not the send.
func (e A2AMessageDelivered) Summary() string {
	return lead(e.Recipient, fmt.Sprintf("received %d A2A message(s) from %s on %s",
		e.MessageCount, e.Sender, e.ChannelID))
}

// A2AChannelClosed marks a channel closing — the exchange is over, and a
// re-open is a new ask rather than a continued volley.
type A2AChannelClosed struct {
	ChannelID    string   `json:"channel_id"`
	ClosedBy     string   `json:"closed_by"`
	Participants []string `json:"participants,omitempty"`
	MessageCount int      `json:"message_count"`
	DurationMS   float64  `json:"duration_ms"`
}

// EventType is the "a2a_channel_closed" wire type.
func (A2AChannelClosed) EventType() string { return "a2a_channel_closed" }

// Summary names who closed the channel, falling back to the engine itself for
// the idle sweep — see the branch below.
func (e A2AChannelClosed) Summary() string {
	who := e.ClosedBy
	if who == "" {
		// An idle-close is the maintenance sweep's doing, not a participant's.
		who = "system"
	}
	return fmt.Sprintf("%s closed A2A channel %s (%d messages)",
		who, e.ChannelID, e.MessageCount)
}
