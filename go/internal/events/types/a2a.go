package types

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/events"
)

// Agent-to-agent channels: one ask, one answer, then closed. These events are
// the audit trail of that exchange — the delivery itself rides the target
// seat's durable inbox, never a second in-process path.

func init() {
	events.Register(A2AChannelOpened{})
	events.Register(A2AMessageSent{})
	events.Register(A2AMessageDelivered{})
	events.Register(A2AChannelClosed{})
}

// A2AChannelOpened marks a channel being created between two agents.
type A2AChannelOpened struct {
	ChannelID    string   `json:"channel_id"`
	Requester    string   `json:"requester"`
	Target       string   `json:"target"`
	Participants []string `json:"participants,omitempty"`
}

func (A2AChannelOpened) EventType() string { return "a2a_channel_opened" }

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

func (A2AMessageDelivered) EventType() string { return "a2a_message_delivered" }

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

func (A2AChannelClosed) EventType() string { return "a2a_channel_closed" }

func (e A2AChannelClosed) Summary() string {
	who := e.ClosedBy
	if who == "" {
		// An idle-close is the maintenance sweep's doing, not a participant's.
		who = "system"
	}
	return fmt.Sprintf("%s closed A2A channel %s (%d messages)",
		who, e.ChannelID, e.MessageCount)
}
