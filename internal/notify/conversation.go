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
//
// A zero [Standing] is chart-only, which no running node builds from: a node
// holding the estate reads its own identity rows from boot, one without data
// reads them through the estate router. A turn pins the registry's withholding
// (turnctx.Turn.Withheld), so a lead's roster and lookup_colleague omit
// exactly the people an inbound message would not be attributed to, without
// saying why.
//
// # Engine-hosted senders are handles
//
// A sender id from a source the engine hosts (tracker, knowledge base, chat,
// Gitlet) is the seat's handle, so [NewRegistry] is told those sources as
// handle namespaces (the engine's one firstPartySources list, held to its
// native parsers by a test). Read through a registry that did not know, a
// colleague on a native surface was an outside party: filed under a platform
// id, fenced as external, refused artifacts their ceiling allows.
//
// # Both directions, one seam each
//
// Writes (the working indicator, a [StatusPost]) and reads ([ThreadReader],
// [ThreadReaders]) are per-backend seams. The fan-out routes on the trigger's
// own transport, because a company runs two chat surfaces and a caller holding
// one backend's reader silently reads none. [ThreadOf] is not [ConversationOf]:
// one answers where a reply lands, the other what was already said.
//
// # The spine fences every wake's body
//
// [Render] is the one caller of [Prompt.Build] and hands a prompt a body
// already fenced (ADR-0040), so no prompt can render it bare. A source states
// who wrote the body only where the payload says one person wrote every word
// ([Stated]); otherwise nobody, which reads external on a vendor's surface,
// because who caused an event is not who wrote its body. A surface the engine
// hosts reads as a colleague's ([StatedHosted]).
//
// The delivery mints the wake's fence token ([Inbound].Fence), as no turn
// exists yet; a coalesced digest moves earlier fences onto the latest's token
// and the turn onto its own (provenance.Rekey). engine's
// TestEveryWakeSurfaceFencesWhatSomebodyWrote and
// TestEveryPromptBuilderIsAWakeSurface hold every surface to this.
//
// # Partitions, passes and replay
//
// [KeyOf] splits the stamped key by writer and by whom an agent wrote for, so
// one turn works for one writer (ADR-0109). A relayed record a newer build
// wrote is passed, never acked ([Defer]): an ack loses the wake for the fleet.
// Nothing steers a pass to a node that reads it, so the dead letter delivers
// it (replay.go), paced by the rate valve in a bucket of its own.
package notify

import (
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE TWO KEYS. THE PARTITION KEY asks "which events is this handled WITH?"
// (the broker partitions a seat's inbox by it, the coalescer merges one).
// THE CONVERSATION IDENTITY asks "which conversation is this part of?" (the
// executor log keys sessions on it, and a parked executor matches an answer
// to its question). They differ for a direct message: a top-level burst
// partitions on the channel, a thread reply on the thread, so one value filed
// a seat's DM turn under a key its next turn never looked up. THE PARTITION
// REFINES THE IDENTITY on every source ([Prompt.ConversationIdentity]).
// Both are STAMPED BY THE PRODUCER, never derived in the queue layer, which
// must hold no integration knowledge. A SCHEDULE IS A CONVERSATION TOO
// ([ScheduleConversation], both keys), the one nobody can reply into
// ([Derived]).

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
// THE DISCRIMINATOR FOR EVERY CHAT-ONLY DECISION, a constant so the four
// readers cannot drift: each chat parser stamps it, [ConversationOf] and
// [ThreadOf] match a driver and the thread reader on it, and the engine tells
// a chat-triggered turn from a schedule tick by it. A rename missed in one
// spelling leaves that backend silently deaf: every driver refuses the
// trigger, indistinguishable from "nobody is waiting" (ADR-0008).
//
// A source with no chat concept stamps nothing.
const TransportField = "transport"

// ChannelKindField carries the CANONICAL shape of the surface a message
// arrived on — one of [types.ChannelKind].
//
// Stamped by the parser, never derived downstream: the raw value is
// integration-specific (Mattermost says "D", Slack an id starting with "D"),
// and mapping it outside the app's own parser would quietly mark arbitrary
// surfaces as direct messages when an app changed its encoding.
//
// A source with no channel concept stamps nothing, which reads back as
// [types.ChannelUnknown]: a real answer ("not on a channel"), not a gap.
const ChannelKindField = "channel_kind"

// EventPrefix namespaces the fallback key.
//
// Its own namespace so it can never collide with a derived key: a
// third-party app's local key is namespaced by source, and no source is
// called "event".
const EventPrefix = "event:"

// Fallback is the key for an event with no derivable conversation.
//
// UNIQUE PER EVENT: an event that cannot name its conversation must never
// merge with another, which a shared fallback would do. A task assignment and
// an A2A wake each become a partition of one. A schedule's fire is not among
// them: it names its schedule's conversation ([ScheduleConversation]).
//
// IT SERVES BOTH KEYS: [Derived] refuses it under either reading, so a key
// minted for one event is never offered a person's reply as a conversation
// they could answer on, nor shown as a thread a chat reply could reach.
func Fallback(eventID string) string { return EventPrefix + eventID }

// SchedulePrefix namespaces a schedule's conversation ([ScheduleConversation]).
//
// A SLASH, NEVER A COLON, so no key a message derives can start with it: a
// source's key is its source, a colon and the source's own local key
// ([Namespaced]), and the per-event fallback is [EventPrefix]'s — which is what
// lets a schedule's conversation be known by its prefix alone ([Scheduled]).
const SchedulePrefix = "schedule/"

// ScheduleConversation is the conversation every fire of one schedule is, of
// each seat that runs it: the identity the scheduler stamps on a fire and its
// partition (internal/schedule's FirePartition). The executor log keys on
// (seat, conversation), so a unit schedule fanned out to two seats is two.
//
// ITS WHOLE IDENTITY: scope (role or unit), scope id and name; a rename or a
// role/unit move starts a new conversation, an edit to task, cron, zone or
// timeout keeps it. ESCAPED, each part, so `Q3/Launch` + `review` never
// merges with `Q3` + `Launch/review`; the name is bounded
// (org.ScheduleNameMaxBytes) to fit the executor log's ConversationKeyMaxBytes,
// which refuses rather than cuts.
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
// ONE TURN WORKS FOR ONE WRITER, held to the apps they use (ADR-0109): two
// people in a thread, or a person and a stranger, are two turns. The writer is
// read from fields every producer sends (a first-party notice's writer, the
// actor, the sender), so the queue learns no integration. ONE WHOM TOO
// ([ForSeparator], [FirstPartyWriter]): an agent's writes for two requesters
// are two turns. The STAMPED key stays thread-grained ([KeyOfAll]) so a later
// writer's reply still finds a parked run. EXCEPTION: ADR-0120's seen-through
// mark keys on the thread, so a wake that obliges the seat to nothing is
// absorbed whatever its writer. A fallback gets no writer.
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
// writer's parts are escaped (url.PathEscape), so none holds a space, and the
// only spaces in a writer are [ForSeparator]'s, so this separator is never
// part of one; a stamped key holds no writer at all.
const WriterSeparator = " by "

// writerOf is who wrote ev, as the partition function splits on it: a
// first-party notice's writer by the kind its record names (a seat by its
// handle, a credential acting as no seat by its login, the engine as itself;
// an agent's or the engine's writer also names whom it wrote for), and anything
// else by the actor every parser stamps ([ActorField]), else its sender. Empty
// for an event that is no external notification.
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
				return "seat:" + url.PathEscape(seat) + forOf(w)
			}
		case iam.ActorOperator:
			if login := strings.TrimSpace(w.Login); login != "" {
				return "login:" + url.PathEscape(login)
			}
		case iam.ActorSystem:
			return "system" + forOf(w)
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

// ForSeparator joins a writer to whom it wrote for in [KeyOf]'s writer: an
// agent writer (`seat:lead for seat:ada`), and the engine as the writer of
// its record about an agent's earlier act made for somebody
// (`system for seat:ada`). Every part is escaped (url.PathEscape), so none
// holds a space and the separator is never part of one.
const ForSeparator = " for "

// forOf is whom an agent's write was made for, as the partition splits on it
// ([types.NotificationWriter.WorkingFor]), and whom the engine's record about
// an agent's earlier act carries ([FirstPartyWriter]): empty for every other
// writer, and otherwise [WorkingForKey] of its carrier, so every constituent of
// one partition carries the same carrier and the turn's requester, read off the
// first of them, is every one's. The engine's wakes for two requesters are two
// turns as an agent's are: keyed as one, the second requester's would be worked
// inside the first's turn, held to the first's apps.
func forOf(w *types.NotificationWriter) string {
	if w.Kind != iam.ActorAgent && w.Kind != iam.ActorSystem {
		return ""
	}
	return WorkingForKey(w.WorkingFor)
}

// WorkingForKey is whom a write was made for as the partition keys it: empty
// for nobody, and otherwise [ForSeparator] and the WHOLE carrier, its kind, the
// seat or the login, and a credential's apps.
//
// THE ONE SPELLING: readers that compare carriers (an edit's worked-through
// mark, the drop of an edit's wake) must agree with the partition. A pickup's
// carrier keeps apps in grant order, so they are held to the writer's order
// ([knownApps]: known apps once each, in iam.Apps order), never arrival order.
//
// ONLY NIL IS "": an empty-kind carrier is read as a stranger's, and keying it
// as nobody would match a turn that works for nobody. An unknown kind keys by
// the kind alone, since strangers hold the same nothing.
func WorkingForKey(c *types.WorkingFor) string {
	if c == nil {
		return ""
	}
	switch c.Kind {
	case types.WorkingForSeat:
		return ForSeparator + "seat:" + url.PathEscape(strings.TrimSpace(c.Seat))
	case types.WorkingForCredential:
		known := knownApps(c.Apps)
		apps := make([]string, 0, len(known))
		for _, app := range known {
			apps = append(apps, url.PathEscape(string(app)))
		}
		return ForSeparator + "login:" + url.PathEscape(strings.TrimSpace(c.Login)) + ":" +
			strings.Join(apps, "+")
	}
	return ForSeparator + url.PathEscape(string(c.Kind))
}

// KeyOfAll is the partition key for a whole partition: the key its events
// were STAMPED with, thread-grained, never [KeyOf]'s writer split.
//
// Every event in a partition shares a stamped key by construction, so a later
// one naming a different key is a routing bug; taking the FIRST keeps the
// answer independent of sort order.
//
// THE STAMPED KEY, because it outlives the drain: the coalescer stamps it on
// the merged event (so KeyOf reads it back into the same partition and
// writer), a parked run's row carries it as the batch a reply is matched
// against, and telemetry names it. A writer in it would hide a parked run from
// every reply but its own writer's, which is the answer route's decision.
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
// NOT "ConversationOf": that is the chat thread resolver the working
// indicator raises its spinner on ([ConversationOf] in status.go), a
// different question from different metadata.
//
// NEVER THE PARTITION. Every producer that stamps a partition stamps the
// identity it refines beside it ([Prompt.ConversationIdentity], a schedule's
// [ScheduleConversation] as both), so an event naming none (an A2A wake, a
// task assignment) names no conversation and gets its own fallback.
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
// (internal/engine's Dispatcher.answered) and whether a person can answer it
// in chat (internal/api/queries' answerable_in_chat). A fallback key is one no
// inbound message can reproduce. A schedule's is reproduced by every fire and
// by nothing anybody writes: offered as a reply, the next fire would be taken
// for the answer to the last fire's question, and "reply in the thread" would
// point at a thread that does not exist.
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
