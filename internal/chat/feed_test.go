package chat_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chat"
)

// THE FEED'S ONE JOB: deciding whether a committed record can wake anybody at
// all, and HANDLING the ones that cannot.
//
// Every case below is about the second half. A record that wakes nobody is not
// an error and must not be naked: a record naked for having nothing to say is
// redelivered for ever, and a busy company's log is overwhelmingly made of
// them — reactions, prunes, membership sets, the fleet's own bookkeeping. The
// cost of getting this wrong is not a missed notification; it is one consumer
// spinning on a record nobody will ever accept.

// wakeRecord is a valid committed record, as the broker hands one to the feed.
//
// BUILT AND ENCODED rather than written through the store, because these cases
// are about records this build would never WRITE — a newer build's version, a
// kind that carries no payload — and a harness that could only produce what
// the writer produces could not state them.
func wakeRecord(t *testing.T, rec chat.MutationRecord) changefeed.Record {
	t.Helper()
	if rec.V == 0 {
		rec.V = chat.RecordVersion
	}
	payload, err := chat.Encode(rec)
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	return changefeed.Record{Payload: payload, Key: rec.Subject.Wire()}
}

// aPost is an ordinary message in a room, routed to one seat.
func aPost() chat.MutationRecord {
	return chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.RecordVersion, OpID: "op-1",
			Subject: chat.MessageSubject("room-1"),
			Op:      chat.OpPost, Writer: "node-a",
		},
		Actor: "jane", ActorKind: chat.AuthorHuman,
		Notify: &chat.Notify{
			MessageID: "msg-1", ChannelID: "room-1", ChannelName: "launch",
			ChannelKind: chat.KindPublic, Author: "jane",
			AuthorKind: chat.AuthorHuman, Excerpt: "where are we?",
			Recipients: []chat.Recipient{{Handle: "eng", Reason: chat.ReasonMention}},
		},
	}
}

// quiet is the record with its routing snapshot removed, which is how a record
// says "wake nobody" whatever else it carries.
func quiet(rec chat.MutationRecord) chat.MutationRecord {
	rec.Notify = nil
	return rec
}

// on moves a record onto another subject and op.
func on(rec chat.MutationRecord, subject chat.Subject, op chat.OpKind) chat.MutationRecord {
	rec.Subject, rec.Op = subject, op
	return rec
}

// translate runs one record through the shipped translator.
func translate(t *testing.T, rec changefeed.Record) (changefeed.Delivery, bool) {
	t.Helper()
	delivery, wake, err := chat.NewTranslator().Translate(t.Context(), rec)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	return delivery, wake
}

// A MESSAGE ANYBODY CAN ANSWER IS RELAYED WHOLE.
//
// The control for every quiet case below: without it each of them passes
// identically if the feed simply stopped relaying anything at all.
//
// THE WHOLE RECORD TRAVELS, including a field this build does not understand.
// The node that wins a feed message is rarely the node running the seat that
// gets woken and is usually behind on the row, so the parser routes from what
// the record carries rather than from what this node has applied — and a
// relay that dropped the half it could not read would make a rolling upgrade a
// period in which some people are simply not told.
func TestTheFeedRelaysAMessageWithEverythingTheRecordCarried(t *testing.T) {
	t.Parallel()
	rec := aPost()
	rec.Extra = map[string]json.RawMessage{"something_newer": json.RawMessage(`"x"`)}

	delivery, wake := translate(t, wakeRecord(t, rec))
	if !wake {
		t.Fatal("a message routed to a seat woke nobody — the feed is the " +
			"only thing that derives a wake from a committed record, so a " +
			"decline here is a company that is never told what is said to it")
	}
	if delivery.ID != "op-1" {
		t.Errorf("the delivery is identified as %q, want the record's own "+
			"operation id: it seeds every recipient's wake id, so a different "+
			"value makes a redelivery unrecognisable", delivery.ID)
	}
	if delivery.Actor != "jane" {
		t.Errorf("the delivery names actor %q, want jane — a parser that "+
			"cannot see who wrote the record wakes them about their own "+
			"message", delivery.Actor)
	}
	if delivery.Body["something_newer"] == nil {
		t.Errorf("the relayed body dropped a field a newer build wrote: %v",
			delivery.Body)
	}
	round, err := json.Marshal(delivery.Body)
	if err != nil {
		t.Fatalf("re-encode the body: %v", err)
	}
	back, err := chat.Decode(round)
	if err != nil {
		t.Fatalf("the relayed body is not a record any more: %v", err)
	}
	if back.Notify == nil || back.Notify.MessageID != "msg-1" {
		t.Errorf("the relayed record's routing snapshot is %+v, want the one "+
			"the writer put on it", back.Notify)
	}
}

// EVERY CHANGE THAT WAKES NOBODY IS HANDLED, NOT REFUSED.
//
// Each row states a different reason a record has nothing to say, because each
// would have to be found separately if it were missing: the three machinery
// kinds are the fleet talking to itself, the quiet ops are gestures nobody is
// told about, a newer build's payload is rules this one cannot read, and an
// absent routing snapshot is a record that says so itself.
//
// A row that ACKS is the assertion. Naking any of them puts one durable
// consumer into a redelivery loop over a record no build will ever accept.
func TestTheFeedAcksEveryChangeThatWakesNobody(t *testing.T) {
	t.Parallel()
	room := chat.ChannelSubject("room-1")

	cases := map[string]chat.MutationRecord{
		"a read barrier": on(quiet(aPost()), chat.BarrierSubject(), chat.OpBarrier),
		"a node's eviction": on(quiet(aPost()),
			chat.EvictionSubject("node-b"), chat.OpEviction),
		"a reanchor's generation": on(quiet(aPost()),
			chat.GenerationSubject(2), chat.OpGeneration),
		"a name claim": on(quiet(aPost()),
			chat.ChannelNameSubject("launch"), chat.OpCreate),

		// The quiet OPS. Each rides a routable subject — a room or its
		// message stream — so only the op itself keeps them quiet, which
		// is precisely what makes them worth a row.
		"a retention prune":      on(aPost(), room, chat.OpPrune),
		"an operator's erase":    on(aPost(), room, chat.OpErase),
		"a membership set":       on(aPost(), room, chat.OpMembers),
		"a topic change":         on(aPost(), room, chat.OpPatch),
		"a room being created":   on(aPost(), room, chat.OpCreate),
		"a reaction, or its un-": on(aPost(), chat.MessageSubject("room-1"), chat.OpReact),
		"a message being deleted": on(aPost(), chat.MessageSubject("room-1"),
			chat.OpDelete),

		// And the two that are quiet whatever the op and the subject.
		"an import, which wakes nobody": quiet(aPost()),
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, wake := translate(t, wakeRecord(t, rec)); wake {
				t.Fatalf("%s produced a wake — there is nobody to deliver it "+
					"to and nothing for them to open", name)
			}
		})
	}
}

// A PAYLOAD FROM A NEWER BUILD IS DROPPED WITHOUT WAKING, and that is neither
// a loss nor an error.
//
// The envelope decodes at every version, so this node knows what the record is
// about and knows it cannot read the rules it was written under. Waking
// somebody from a payload it cannot decode would be inventing a notification.
// A peer on that build wins the next redelivery of the same record, which is
// what makes a rolling upgrade a period of reduced coverage rather than an
// outage — so this one is HANDLED.
func TestTheFeedDropsANewerBuildsRecordWithoutWakingAnybody(t *testing.T) {
	t.Parallel()
	payload, err := chat.Encode(aPost())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("read the record back: %v", err)
	}
	raw["v"] = chat.RecordVersion + 1
	future, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}

	delivery, wake, err := chat.NewTranslator().Translate(t.Context(),
		changefeed.Record{Payload: future, Key: "chat.message.room-1"})
	if err != nil {
		t.Fatalf("a record from a newer build failed the feed: %v — it is "+
			"handled, because naking it would spin this consumer for ever "+
			"while every node on the older build does the same", err)
	}
	if wake {
		t.Fatalf("a record this build cannot decode produced a wake: %+v",
			delivery)
	}
}

// A RECORD THAT IS NOT A RECORD IS AN ERROR, and it is the one thing here
// that is.
//
// Every other outcome above is a decision about a record the feed understood.
// Bytes that are not a chat record at all are a broken producer or the wrong
// stream, and acking them would make the corruption silent.
func TestTheFeedRefusesBytesThatAreNotARecord(t *testing.T) {
	t.Parallel()
	_, _, err := chat.NewTranslator().Translate(t.Context(),
		changefeed.Record{Payload: []byte(`{"v":0}`), Key: "chat.message.room-1"})
	if err == nil {
		t.Fatal("a payload with no version was accepted — a record that does " +
			"not state its version cannot be told apart from a newer build's")
	}
}

// THE SOURCE AND THE GROUP ARE THE NAMES THE FLEET IS POSITIONED ON.
//
// Both are addresses rather than labels, and both fail silently when changed.
//
// THE SOURCE is what the parser publishes wakes under AND what this domain's
// posting tools declare as their delivery surface. Spelled differently at
// either end, the turn's delivery gate finds no delivery for the source and
// falls back to "any delivery counts" — so every addressed chat turn is then
// allowed to end in silence, which is the failure the source-scoped gate was
// introduced to fix.
//
// THE GROUP is where the fleet's durable position IS. A rename does not move
// the consumer; it starts a second one at the head of the log and abandons
// everything the first had not handled — a company that silently stops being
// told about its own conversation, with nothing to replay from.
func TestTheSourceAndTheFeedGroupAreTheNamesTheFleetIsPositionedOn(t *testing.T) {
	t.Parallel()
	if chat.Source != "chat" {
		t.Errorf("the notification source is %q: it is also the surface the "+
			"posting tools declare, and the two must be one string",
			chat.Source)
	}
	if chat.FeedGroup != "crewlet-chat-feed" {
		t.Errorf("the durable consumer is named %q — renaming it abandons "+
			"every wake the fleet had not handled", chat.FeedGroup)
	}
	got := chat.NewTranslator().Source()
	if got.Name != chat.Source || got.Group != chat.FeedGroup {
		t.Errorf("the translator serves %+v, want the two constants above: "+
			"the feed resolves its consumer's name from this value, so a "+
			"third spelling here is a consumer nobody else can find", got)
	}
}

// A FEED WITH NO LOG SAYS WHAT IS MISSING.
//
// The seam is the engine's to fill, and an unfilled one is not a degraded
// feed: it is a company where no seat is ever woken by anything said to it.
// The refusal names the thing to give it, because the symptom — silence —
// points at nothing.
func TestAFeedWithNoLogRefusesAndNamesWhatToGiveIt(t *testing.T) {
	t.Parallel()
	_, err := chat.FeedSource{}.Open(t.Context(), chat.FeedGroup)
	if err == nil {
		t.Fatal("a feed source with no log opened a consumer over nothing")
	}
	if !strings.Contains(err.Error(), "chat.FeedSource") {
		t.Errorf("the refusal reads %q and does not name what a person has "+
			"to give it", err)
	}
}
