package notify

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A PERSON WAITING ON A SEAT IS TAKEN BEFORE THE SEAT'S BACKGROUND WORK.
//
// A wake rides its seat's priority lane (queue.Prioritize) exactly when two
// things hold, and both are answers this package already gives elsewhere:
//
//   - THE SEAT OWES AN ANSWER, by the backend's own [AddressRule]: a direct
//     message, a mention, a question it was asked, a thread it is in for one
//     of those reasons. The same value the working indicator and the delivery
//     check read ([Prompt.Addressed]), so the three cannot disagree about
//     whether somebody is waiting.
//   - A PERSON WROTE IT ([WrittenByPerson]). An agent's mention of a colleague
//     is work between seats and waits its turn with the rest of it; a
//     person's is somebody watching a screen for the reply.
//
// Everything else (a tracker notice, a room's news, a colleague's ask, a
// schedule's fire) is background, and the order among background work is
// what it always was. [Service] applies the rule as it publishes each wake.

// WrittenByPerson reports whether the text a notification carries was written
// by a person rather than by an agent or by the engine itself.
//
// THE KIND THE RECORD STATED FIRST, because a source the engine hosts names
// its writer's kind on every record, a person whose seat has since gone
// included: the writer a first-party notice states ([Inbound.Writer]), else
// the kind a parser put in the metadata ([BodyAuthorKindField]). Writer comes
// first because it is the one statement every first-party producer makes —
// Gitlet's wakes state their writer there alone, and read past it a
// colleague's review comment on a change was a sender the registry could not
// place, which this rule calls a person, so an agent's remark jumped the
// lane. Then the colleague the sender resolves to, in the source's own
// namespace ([Parties.ByExternalID]): a seat is a person or an agent. A
// sender nobody in the company holds is somebody outside it, which on every
// surface a seat is reached on is a person; no sender at all is nobody.
func WrittenByPerson(reg Parties, n Inbound) bool {
	kind := iam.ActorKind(n.Metadata[BodyAuthorKindField])
	if n.Writer != nil {
		kind = n.Writer.Kind
	}
	switch kind {
	case iam.ActorHuman, iam.ActorOperator:
		return true
	case iam.ActorAgent, iam.ActorSystem:
		return false
	}
	sender := strings.TrimSpace(n.Sender)
	if sender == "" {
		return false
	}
	if reg != nil {
		if party, ok := reg.ByExternalID(n.Source, sender); ok {
			return party.Human
		}
	}
	return true
}

// WaitingNotice is what a seat's waiting channel carries
// ([topics.SeatWaiting]): a person's message reached the seat's priority lane,
// and where it was written, so the node running the seat can show the person
// it is queued if the seat is busy ([Statuses.Queue]).
//
// THE CONVERSATION'S KEYS AND NOTHING ELSE. Not the message, not its author's
// words, not who wrote it: the node that hears this needs to know which
// thread to draw a line in, and the message itself reaches it on the seat's
// durable inbox. See [waitingKeys].
type WaitingNotice struct {
	// Wake is the id of the wake the message rides, which the turn that
	// picks it up names among its constituents ([Statuses.Dequeue]).
	Wake string `json:"wake"`

	// Metadata is the wake's own conversation keys ([waitingKeys]).
	Metadata map[string]string `json:"metadata"`
}

// waitingKeys are the metadata keys a queued indicator is placed and decided
// by: which backend, which room and thread ([ConversationOf]), and whether the
// seat owes an answer there ([AddressRule.Addressed]).
var waitingKeys = []string{
	TransportField, ChannelField, ChannelTypeField, MessageIDField,
	ThreadField, ThreadAnchorField, FollowReasonField, FollowingField,
}

// waitingMetadata is the part of a wake's metadata a [WaitingNotice] carries.
func waitingMetadata(meta map[string]string) map[string]string {
	out := make(map[string]string, len(waitingKeys))
	for _, key := range waitingKeys {
		if v := meta[key]; v != "" {
			out[key] = v
		}
	}
	return out
}

// announceWaiting tells the node running a seat that a person's message
// reached its priority lane.
//
// BEST EFFORT, and only a log line when it fails: the message is already on
// the seat's durable inbox, and a node that never hears this shows the person
// no queued line, which is all that is lost.
//
// UNSEALED, unlike a presence frame, because it carries no person and no text
// (see [WaitingNotice]) and reaches no browser: a node that hears it decides
// for itself whether the seat is its own and busy, and the most a forged one
// can do is draw a queued line, on a broker where the same access could
// already publish the wake itself.
func (s *Service) announceWaiting(ctx context.Context, seat uuid.UUID, wake uuid.UUID, meta map[string]string) {
	subject := topics.SeatWaiting(seat)
	if subject == "" {
		return
	}
	payload, err := json.Marshal(WaitingNotice{Wake: wake.String(), Metadata: waitingMetadata(meta)})
	if err != nil {
		log.WarnContext(ctx, "waiting_notice_unencoded", "seat", seat.String(), "error", err.Error())
		return
	}
	if err := s.queue.Broadcast(ctx, subject, payload); err != nil {
		log.DebugContext(ctx, "waiting_notice_not_sent", "seat", seat.String(), "error", err.Error(),
			"detail", "the message is on the seat's inbox; only its queued line is not shown")
	}
}

// DecodeWaitingNotice reads a waiting channel's payload.
func DecodeWaitingNotice(payload []byte) (WaitingNotice, error) {
	var n WaitingNotice
	err := json.Unmarshal(payload, &n)
	return n, err
}

// PersonWrote reports whether a person wrote anything n delivers: n itself, or
// any message coalesced into it, which share n's writer.
func PersonWrote(reg Parties, n types.ExternalNotification) bool {
	copies := []Inbound{{Source: n.NotificationSource, EventType: n.SourceEventType,
		Sender: n.Sender, Metadata: n.Metadata, Writer: n.Writer}}
	for _, m := range n.Messages {
		copies = append(copies, Inbound{Source: n.NotificationSource, EventType: m.SourceEventType,
			Sender: m.Sender, Metadata: m.Metadata, Writer: n.Writer})
	}
	return slices.ContainsFunc(copies, func(in Inbound) bool { return WrittenByPerson(reg, in) })
}
