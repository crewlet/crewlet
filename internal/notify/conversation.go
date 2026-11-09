// Package notify is the backend-neutral notification spine.
//
// Everything an inbound message needs on its way from a third-party app's
// webhook to a seat's turn, with no third-party app in it: which conversation
// an event belongs to, how several events in one conversation merge into a
// single trigger, who the parties are, which events a seat must not be woken
// by, and how often it may be woken at all.
//
// The third-party apps sit on top and contribute only what is genuinely
// theirs (a parser, a transport, a prompt, a mention grammar). That split is
// load-bearing rather than tidy: a spine built after its first integration is
// a spine with that integration's assumptions welded into it, and the second
// integration then arrives to find its own shape unrepresentable.
//
// # Who the parties are has TWO inputs
//
// The party [Registry] is built from the org view and from one reading of the
// identity [Directory]: the org says which seats exist and which contact
// identities a human seat declares, and the directory says whether the person
// holding that seat may still be reached through them. A suspended or removed
// holder's identities are withheld, with no configuration change at all — see
// directory.go. Both inputs are fixed for a registry's life, and either moving
// is a new registry built whole and swapped by the engine.
package notify

import (
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE TWO KEYS, AND WHICH QUESTION EACH ANSWERS.
//
// An event carries two identities because SIX subsystems read one, and they
// do not all ask the same thing:
//
//   - THE PARTITION KEY — "which other events is this one handled WITH?" The
//     broker partitions a seat's inbox by it (internal/node) and the
//     coalescer merges a partition into one trigger.
//   - THE CONVERSATION IDENTITY — "which ongoing conversation is this event
//     part of?" The executor log keys a seat's sessions on it — so the
//     conversation's next turn reads its head and how its earlier turns
//     ended under it — the turn telemetry carries it into the event store's
//     conversation_key tag and the episodes column, the API's
//     answerable-in-chat predicate reads it, and a parked executor matches a
//     person's answer back to the question that asked it on it — because a
//     person answers on the conversation, which for a direct message is not
//     the batch their reply lands in.
//
// They were ONE value, and the one case where the two questions have
// different answers is what that cost: a direct message's top-level burst
// partitions on the bare channel so a person typing four times costs one turn,
// while a reply in the thread that burst started partitions on the thread —
// so a seat's own prior turn on the DM was filed under a key its next turn
// never looked up, and read as a first turn every time.
//
// THE PARTITION REFINES THE IDENTITY. Every event in one partition carries
// the SAME identity, and a partition key is either that identity or a finer
// cut of it — on every source, not just chat. That is the property that makes
// a turn's conversation well-defined: a turn is a partition and its rows on
// the executor log are keyed once, so if two constituents of one coalesced
// trigger disagreed about the identity the key would depend on which event
// sorted first. It is stated and enforced where a source decides both — see
// [Prompt.ConversationIdentity].
//
// Both are STAMPED BY THE PRODUCER and read by everyone else. The
// notification layer knows the third-party app and can derive them; the
// broker's partition function is then a field read that cannot disagree with
// what the producer meant. Deriving either again at partition time would put
// integration knowledge in the queue layer and give two places a chance to
// answer differently.
//
// A SCHEDULE IS A CONVERSATION TOO, of each seat that runs it: the scheduler
// stamps every fire with the schedule's conversation ([ScheduleConversation])
// as both keys, so the fires that wait in a seat's inbox together run as one
// turn and each fire's turn continues the session the schedule's last fire
// left, as a conversation's next turn continues its last turn's. It is the one
// conversation nobody can reply into ([Derived]): every fire of a schedule
// carries it, so a fire read as a reply would be taken for the answer to the
// question the last fire's run parked on.
//
// THE PARTITION FUNCTION SPLITS THE STAMPED KEY BY WRITER ([KeyOf]), in memory,
// where a seat's drain is grouped into turns. A turn works for the one
// principal who woke it, held to the apps they use (ADR-0109), so two people
// in one thread, or a person and a stranger, are two partitions and two turns,
// each bounded by its own writer, never one turn working for both. The writer
// is read from fields every producer already sends (a first-party notice's
// writer, the actor every parser stamps, the sender), so the queue learns no
// integration and an older producer's events split too. The STAMPED key stays
// thread-grained ([KeyOfAll]): it is the conversation a merged event carries
// and a parked run's batch, which a later writer's reply in the same thread
// must still find.

// PartitionField is the payload field a producer stamps the partition key
// into.
//
// Spelled "partition_key", as the coalescing record's own field is, because
// everywhere else in the engine "conversation_key" is the conversation
// IDENTITY ([ConversationField]) — and a partition is a batch inside it.
const PartitionField = "partition_key"

// ConversationField is the payload field the durable conversation identity is
// stamped into.
const ConversationField = "conversation_identity"

// TriggerField carries where on its log the record a first-party notification
// was derived from was committed — the position token
// ([types.RawWebhook.Trigger]) — stamped on the wake's envelope bag beside the
// partition key, and on its metadata copy for a person reading it. The woken
// turn hands it to its node's read-your-writes floors before its first read
// ([TriggerOf]). Absent on every vendor's wake, whose turn raises no read
// floor of its own.
const TriggerField = "trigger_position"

// TriggerOf is the trigger position an event carries ([TriggerField]), or
// empty.
func TriggerOf(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	token, _ := ev.Payload[TriggerField].(string)
	return token
}

// RecipientField carries the handle a notification was resolved to.
//
// Stamped by the inbound service after the recipient cascade, because it is
// the one fact about a notification a parser genuinely cannot know: which
// seat a third-party app's account id or email belongs to is answered by the
// org, not by the payload.
const RecipientField = "recipient_handle"

// TransportField carries the name of the chat backend a message arrived on.
//
// THE DISCRIMINATOR FOR EVERY CHAT-ONLY DECISION, and the reason it is a
// constant rather than a literal: a company can run two chat surfaces, and
// four places have to agree on this key for a turn to reach the right one —
// each chat parser stamps it, [ConversationOf] matches a working indicator's
// driver on it, [ThreadOf] matches the thread reader on it, and the engine
// reads it to tell a chat-triggered turn from a schedule tick. Written as a
// literal in each, a rename in one of them makes a backend silently deaf on
// the surfaces that still spell it the old way: every driver refuses the
// trigger, and refusing is indistinguishable from "nobody is waiting". See
// ADR-0008.
//
// A source with no chat concept stamps nothing, which is what makes "was this
// turn woken by a chat message" answerable without asking each backend.
const TransportField = "transport"

// ChannelKindField carries the CANONICAL shape of the surface a message
// arrived on — one of [types.ChannelKind].
//
// Stamped by the parser and never derived downstream, for the reason
// [PartitionField] gives: the raw value is integration-specific (Mattermost
// says "D", Slack says the id starts with "D", a tracker has no channel at
// all), and mapping it anywhere but in the third-party app's own parser puts
// integration knowledge in a layer that must not have it, where it would
// quietly mark arbitrary surfaces as direct messages the first time a
// third-party app changed its encoding.
//
// A source with no channel concept stamps nothing, which reads back as
// [types.ChannelUnknown]. That is a real answer for a tracker or a code
// host — "this did not arrive on a channel" — rather than a gap.
const ChannelKindField = "channel_kind"

// EventPrefix namespaces the fallback key.
//
// Its own namespace so it can never collide with a derived key: a
// third-party app's local key is namespaced by source, and no source is
// called "event".
const EventPrefix = "event:"

// Fallback is the key for an event with no derivable conversation.
//
// UNIQUE PER EVENT, which is what makes it correct rather than a placeholder:
// an event that cannot name its conversation must never be merged with
// another, and a shared fallback would merge every one of them. A task
// assignment and an A2A wake each become a partition of one, which is exactly
// the pre-coalescing dispatch path. A schedule's fire is not among them: it
// names its schedule's conversation ([ScheduleConversation]), which every fire
// of the schedule shares.
//
// IT SERVES BOTH KEYS, and must: [Derived] refuses it under either reading,
// so a key minted for one event — by the broker's partition function, or by a
// peer that wrote one onto a run — is never offered a person's reply as a
// conversation they could be answering on, nor shown as a thread a reply in
// chat could reach.
func Fallback(eventID string) string { return EventPrefix + eventID }

// SchedulePrefix namespaces a schedule's conversation ([ScheduleConversation]).
//
// A SLASH, NEVER A COLON, so no key a message derives can start with it: a
// source's key is its source, a colon and the source's own local key
// ([Namespaced]), and the per-event fallback is [EventPrefix]'s — which is what
// lets a schedule's conversation be known by its prefix alone ([Scheduled]).
const SchedulePrefix = "schedule/"

// ScheduleConversation is the conversation every fire of one schedule is, of
// each seat that runs it: the identity the scheduler stamps on a fire and the
// inbox partition the fire shares with the schedule's other fires
// (internal/schedule's FirePartition). The executor log keys a seat's sessions
// on (seat, conversation), so each runner seat's fires are one conversation of
// that seat — the next fire continues the session the last one left — and a
// unit schedule fanned out to two seats is two conversations, one per seat.
//
// THE SCHEDULE'S WHOLE IDENTITY AND NOTHING ELSE: its scope (a role or a unit),
// the scope's id (the seat's agent id, the unit's key) and its name. So a
// schedule renamed, or moved between a role and a unit, starts a conversation
// of its own, as does a role schedule whose company was renamed (the seat's
// agent id derives from the company's name); a `lead` schedule whose unit
// changes hands is the new lead's to start, on that seat's own log; and an edit
// to its task, its cron, its zone or its timeout keeps it.
//
// ESCAPED, each part, so a separator in one part cannot merge two schedules: a
// unit `Q3/Launch` with a schedule `review` is not a unit `Q3` with a schedule
// `Launch/review`. The name is bounded as it is escaped here
// (org.ScheduleNameMaxBytes), so every key fits the executor log's
// (internal/executordomain's ConversationKeyMaxBytes), which refuses a longer
// key rather than cut one.
func ScheduleConversation(scope types.ScheduleScope, scopeID, name string) string {
	return SchedulePrefix + url.PathEscape(string(scope)) + "/" + url.PathEscape(scopeID) + "/" +
		url.PathEscape(name)
}

// Scheduled reports whether a conversation key is a schedule's
// ([ScheduleConversation]): every turn of it is a fire, told in the schedule's
// words — the fire that begins, the schedule's earlier fires and how they ended
// — and nobody can reply into it ([Derived]).
//
// THE ONE PLACE A SCHEDULE'S KEY IS RECOGNISED, by its prefix. A package that
// cannot import this one — internal/sandbox, which this package reaches through
// internal/tracing — is told the answer by its caller.
func Scheduled(key string) bool { return strings.HasPrefix(key, SchedulePrefix) }

// Namespaced turns a third-party app's SOURCE-LOCAL key into a global one.
//
// Two third-party apps can and do mint the same local key (a Jira issue and
// a GitLab issue are both plausibly "42"), and an un-namespaced key would
// merge their events into one trigger. The prompt returns the local half
// precisely so it never has to know this rule.
func Namespaced(source, local string) string {
	if source == "" || local == "" {
		return ""
	}
	return source + ":" + local
}

// KeyOf is the partition function: the partition key an event carries, split
// by the event's writer ([WriterSeparator], writerOf), or its fallback.
//
// ONE TURN WORKS FOR ONE WRITER (see "THE PARTITION FUNCTION SPLITS THE STAMPED
// KEY BY WRITER" above): a person's four messages in a thread are one turn, and
// a second person's message in the same thread is a turn of its own. Only an
// external notification names a writer; an A2A ask, a task assignment and a
// schedule's fire partition by their stamped key as before. A per-event
// fallback is a partition of one already, and gets no writer.
func KeyOf(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	key, _ := ev.Payload[PartitionField].(string)
	if key == "" {
		return Fallback(ev.ID.String())
	}
	if writer := writerOf(ev); writer != "" {
		return key + WriterSeparator + writer
	}
	return key
}

// WriterSeparator joins a stamped partition key to its writer in [KeyOf]. A
// writer is escaped (url.PathEscape), so it can hold no space and the
// separator is never part of one; a stamped key holds no writer at all.
const WriterSeparator = " by "

// writerOf is who wrote ev, as the partition function splits on it: a
// first-party notice's writer by the kind its record names (a seat by its
// handle, a credential acting as no seat by its login, the engine as itself),
// and anything else by the actor every parser stamps ([ActorField]), else its
// sender. Empty for an event that is no external notification.
//
// THE SAME IDENTITY THE TURN IS THEN WORKED FOR: the requester a turn's reads
// are decided for is composed from these fields (the engine's turnRequesters,
// writerSubject), so a partition never holds two principals the turn would
// have to work for at once.
func writerOf(ev *events.Event) string {
	n, ok := events.DataAs[*types.ExternalNotification](ev)
	if !ok || n == nil {
		return ""
	}
	if w := n.Writer; w != nil {
		switch w.Kind {
		case iam.ActorAgent, iam.ActorHuman:
			if seat := strings.TrimSpace(w.Seat); seat != "" {
				return "seat:" + url.PathEscape(seat)
			}
		case iam.ActorOperator:
			if login := strings.TrimSpace(w.Login); login != "" {
				return "login:" + url.PathEscape(login)
			}
		case iam.ActorSystem:
			return "system"
		}
	}
	if actor := strings.TrimSpace(n.Metadata[ActorField]); actor != "" {
		return "actor:" + url.PathEscape(actor)
	}
	if sender := strings.TrimSpace(n.Sender); sender != "" {
		return "sender:" + url.PathEscape(sender)
	}
	return ""
}

// KeyOfAll is the partition key for a whole partition: the key its events
// were STAMPED with, thread-grained, never [KeyOf]'s writer split.
//
// Every event in a partition shares a stamped key by construction — the
// partition function only ever refines it — so a later event naming a
// different one is a routing bug. Taking the FIRST keeps the answer stable
// rather than letting it depend on which event happened to sort last.
//
// THE STAMPED KEY, because it is what outlives the drain: the coalescer stamps
// it on the merged event (so KeyOf reads the merged event back into the same
// partition, by the same writer), a parked run's row carries it as the batch a
// reply is matched against, and the turn's telemetry names it. A writer in it
// would hide a parked run from every reply but its own writer's, which is the
// answer route's decision to make, not the key's.
func KeyOfAll(evs []*events.Event) string {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if key, _ := ev.Payload[PartitionField].(string); key != "" {
			return key
		}
	}
	return ""
}

// ConversationIdentityOf is the durable conversation an event belongs to, or
// its fallback.
//
// Named for what it answers rather than as a sibling of [KeyOf], and NOT
// "ConversationOf" — that name is taken by the chat thread resolver the
// working indicator raises its spinner on ([ConversationOf] in status.go),
// which answers a different question from different metadata.
//
// NEVER THE PARTITION. Every producer that stamps a partition stamps the
// identity it refines beside it ([Prompt.ConversationIdentity], and a
// schedule's fire its [ScheduleConversation] as both), so an event naming no
// identity names no conversation at all — an A2A wake, a task assignment — and
// gets its own fallback.
func ConversationIdentityOf(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	if id, _ := ev.Payload[ConversationField].(string); id != "" {
		return id
	}
	return Fallback(ev.ID.String())
}

// ConversationIdentityOfAll is the conversation identity for a whole
// partition.
//
// The FIRST event that names one, and here that is a lookup rather than a
// choice: every event in a partition carries the same identity by
// construction, because a source's partition key refines its identity. See
// [Prompt.ConversationIdentity].
//
// A partition that names none yields "", so the caller decides what that
// means rather than being handed one event's fallback as if it described the
// whole partition.
func ConversationIdentityOfAll(evs []*events.Event) string {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if id, _ := ev.Payload[ConversationField].(string); id != "" {
			return id
		}
	}
	return ""
}

// Derived reports whether a key names a real conversation somebody could reply
// into, as opposed to the per-event fallback or a schedule's
// ([ScheduleConversation]).
//
// THE QUESTION BOTH OF A PARKED EXECUTOR'S ANSWER ROUTES ASK: whether a
// delivery may be offered to a parked run as the reply to its question
// (internal/engine's Dispatcher.answered), and whether a person reading the
// runs board can answer a parked run in chat (internal/api/queries'
// answerable_in_chat). A run an A2A wake or a task assignment started stored no
// conversation, and a fallback key is one no inbound message can ever
// reproduce. A schedule's is reproduced by every fire of the schedule and by
// nothing anybody writes: offered as a reply, the next fire would be taken for
// the answer to the question the last fire's run parked on, and telling
// somebody to "reply in the thread" would send them to a thread that does not
// exist.
func Derived(key string) bool {
	return key != "" && !strings.HasPrefix(key, EventPrefix) && !Scheduled(key)
}

// Stamp writes both keys onto an event's payload.
//
// THE ONE WRITER FOR BOTH, so a producer cannot spell either field
// differently from its readers, and so the two can never be stamped by
// different code paths that disagree about which value went where. A key that
// is empty is not stamped at all: an absent field and an empty one would be
// the same to every reader, and leaving it absent keeps the fallback in one
// place.
func Stamp(ev *events.Event, partition, conversation string) {
	if ev == nil || (partition == "" && conversation == "") {
		return
	}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	if partition != "" {
		ev.Payload[PartitionField] = partition
	}
	if conversation != "" {
		ev.Payload[ConversationField] = conversation
	}
}
