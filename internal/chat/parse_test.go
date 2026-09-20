package chat_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// THE WAKE PATH, END TO END: a committed record becomes one notification per
// seat it concerned.
//
// The feed and the parser are two halves of one contract and neither can be
// checked alone — the feed decides WHAT to relay and the parser decides WHO it
// is for by reading that same relayed record. So every case below routes
// through BOTH, and the record they are given is one the shipped writer
// produced rather than one this file made up. A test that built the parser's
// input by hand would agree with itself while the two halves spoke different
// shapes, which is exactly what happened to the knowledge base's feed: every
// delivery decoded cleanly into a change with no page id, and every page
// notification in the company was dropped one line later.

// company is the roster as the notification spine sees it: every handle is a
// seat, and the ones named as people are the `kind: human` ones.
func company(t *testing.T, handles []string, people ...string) *notify.Registry {
	t.Helper()
	human := map[string]bool{}
	for _, h := range people {
		human[h] = true
	}
	o := &org.Organization{Name: "nimbus"}
	for _, handle := range handles {
		role := &org.Role{Name: strings.ToUpper(handle), DeclaredHandle: handle}
		if human[handle] {
			role.Kind = org.KindHuman
		}
		o.Roles = append(o.Roles, role)
	}
	o.Normalize()
	return notify.NewRegistry(o)
}

// wakes runs one committed record through the feed and then the parser, which
// is the whole inbound path a node actually executes.
func wakes(t *testing.T, rec chat.MutationRecord, reg *notify.Registry) []notify.Routed {
	t.Helper()
	delivery, wake := translate(t, wakeRecord(t, rec))
	if !wake {
		t.Fatal("the feed declined to relay a record that carries a routing " +
			"snapshot, so the parser was never asked who it was for")
	}
	routed, err := chat.NewParser().Parse(t.Context(),
		types.RawWebhook{Body: delivery.Body}, reg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return routed
}

// ONE MESSAGE MAKES ONE WAKE PER HANDLE, EACH WITH ITS OWN DERIVED ID.
//
// A message legitimately concerns several seats at once — one named, one
// following, one who started the thread — and each of them is a separate turn
// on a separate node. The id is DERIVED so that a redelivery is recognisable
// as one: the feed's claim is the first dedupe layer and it FAILS OPEN by
// design, because a coordination store that cannot be reached must not
// silently stop notifications, so something else has to catch what slips
// through.
//
// PER RECIPIENT is the whole of it. One id shared by all three would deliver
// the first wake and deduplicate the other two away — the seat that was named
// hears nothing, and nothing anywhere reports a missing notification.
func TestOneRecordMakesOneWakePerHandleWithIdsOfTheirOwn(t *testing.T) {
	t.Parallel()
	rec := aPost()
	rec.Notify.Recipients = []chat.Recipient{
		{Handle: "eng", Reason: chat.ReasonMention},
		{Handle: "ops", Reason: chat.ReasonFollowAll},
		{Handle: "qa", Reason: chat.ReasonReply},
	}
	rec.Notify.ThreadRoot = "msg-0"

	routed := wakes(t, rec, company(t, []string{"jane", "eng", "ops", "qa"}))
	if len(routed) != 3 {
		t.Fatalf("a message concerning three seats woke %d: %+v",
			len(routed), routed)
	}

	seen := map[string]bool{}
	ids := map[string]bool{}
	for _, r := range routed {
		if seen[r.To.Handle] {
			t.Errorf("%s was woken twice by one message — a handle appears "+
				"once, under the first reason that names it", r.To.Handle)
		}
		seen[r.To.Handle] = true
		if r.WakeID == uuid.Nil {
			t.Errorf("%s's wake carries no derived id, so nothing downstream "+
				"can recognise a redelivery of it", r.To.Handle)
		}
		if want := changefeed.WakeID(rec.OpID, r.To.Handle); r.WakeID != want {
			t.Errorf("%s's wake id is %s, want the one derived from the "+
				"record's own operation id and this handle — the inbox and "+
				"the completion ledger recompute it to recognise a duplicate",
				r.To.Handle, r.WakeID)
		}
		if ids[r.WakeID.String()] {
			t.Errorf("two recipients share wake id %s, so the second wake is "+
				"deduplicated away and that seat is never told", r.WakeID)
		}
		ids[r.WakeID.String()] = true
	}
	for _, handle := range []string{"eng", "ops", "qa"} {
		if !seen[handle] {
			t.Errorf("%s was named on the record and woken by nobody", handle)
		}
	}
}

// THE PARSER AND THE WRITE PATH ROUTE ONE RECORD THE SAME WAY.
//
// This is the reason the routing arithmetic is ONE function set rather than
// two implementations that agree today: the decide resolves the recipients
// inside its own snapshot and puts them on the record, and the parser resolves
// them again on another node, minutes later, from that snapshot. Both call
// [chat.Candidates] and [chat.Route]. If they could disagree, a seat would be
// listed on the record as concerned and woken by nobody — a message that
// reached the log and reached no one, with no error anywhere.
//
// The record here is written by the SHIPPED writer against a real broker and a
// real applier, so what the parser reads is what a company actually puts on
// its log.
func TestTheParserWakesExactlyTheSeatsTheWriterRouted(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "sarah"))
	room := r.room(person("jane"), "launch", "eng", "ops", "sarah")
	if _, err := r.store.Post(t.Context(), person("jane"), room.Channel.ID,
		chat.NewMessage{
			Body: "@eng @sarah where are we?", OperationID: "k-1",
			Mentions: []string{"eng", "sarah"},
		}); err != nil {
		t.Fatalf("post: %v", err)
	}
	r.drain()
	rec := r.lastRecord()
	if rec.Notify == nil {
		t.Fatal("a message anybody can answer carried no routing snapshot")
	}

	reg := company(t, []string{"jane", "eng", "ops", "sarah"}, "jane", "sarah")
	routed := wakes(t, rec, reg)

	// THE RECORD'S OWN SET, which the decide computed, against the
	// parser's. Same handles, same reasons, same order.
	want := rec.Notify.Recipients
	if len(want) == 0 {
		t.Fatal("control: the writer routed nobody, so the comparison below " +
			"would hold for a parser that woke nobody either")
	}
	if len(routed) != len(want) {
		t.Fatalf("the writer routed %+v and the parser woke %d seats: %+v",
			want, len(routed), routed)
	}
	for i, r := range routed {
		if r.To.Handle != want[i].Handle {
			t.Errorf("wake %d went to %q and the record says %q",
				i, r.To.Handle, want[i].Handle)
		}
		if got := r.Inbound.Metadata[notify.FollowReasonField]; got != string(want[i].Reason) {
			t.Errorf("%s was woken as %q and the record says %q — the reason "+
				"decides whether they owe an answer", r.To.Handle, got,
				want[i].Reason)
		}
	}
	for _, r := range routed {
		if r.To.Handle == "jane" {
			t.Error("jane was woken about her own message")
		}
		if r.To.Handle == "sarah" {
			t.Error("sarah is a person and was woken: a wake runs a TURN, so " +
				"routing one to a human spends a model call answering on " +
				"their behalf")
		}
	}
}

// A ROSTER THE PARSER CANNOT SEE ROUTES NOBODY.
//
// Without it the parser cannot tell a seat the company still has from one it
// does not, nor a person from an agent — and both answers are required before
// anybody is woken. Waking everybody the record named would run a turn on a
// human's behalf and on behalf of seats that no longer exist.
//
// The pair is the point: the same record with a roster wakes somebody, so this
// is a statement about the missing roster rather than about the record.
func TestAParserWithNoRosterWakesNobody(t *testing.T) {
	t.Parallel()
	rec := aPost()
	if got := wakes(t, rec, company(t, []string{"jane", "eng"})); len(got) != 1 {
		t.Fatalf("control: the record woke %+v, want eng alone", got)
	}
	delivery, _ := translate(t, wakeRecord(t, rec))
	routed, err := chat.NewParser().Parse(t.Context(),
		types.RawWebhook{Body: delivery.Body}, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(routed) != 0 {
		t.Errorf("a parser with no roster woke %+v", routed)
	}
}

// EVERY KEY A READER SOMEWHERE ELSE DEPENDS ON IS STAMPED.
//
// A chat notification travels as a map, and each key in it is a contract
// between this parser and a reader in another package — the coalescer, the
// working indicator, the reply obligation, the prompt, the self-action guard.
// Spelled at one end only, that contract is invisible: the reader gets "" and
// answers as though the message had nothing to say, and nothing fails, logs or
// tells the two halves apart.
//
// PRESENCE, NOT CONTENT, for the required set: the thread key is empty on a
// top-level message and stamped anyway, because an absent key and an empty one
// are the same to every reader and the difference is exactly what a check like
// this cannot see afterwards.
func TestTheParserStampsEveryKeyItsReadersDependOn(t *testing.T) {
	t.Parallel()
	rec := aPost()
	routed := wakes(t, rec, company(t, []string{"jane", "eng"}))
	if len(routed) != 1 {
		t.Fatalf("the record woke %+v, want eng alone", routed)
	}
	meta := routed[0].Inbound.Metadata

	for _, key := range notify.RequiredChatKeys() {
		if _, ok := meta[key]; !ok {
			t.Errorf("the wake does not stamp %q, so whoever reads it gets "+
				"an empty string and cannot tell that from a message that "+
				"genuinely said nothing", key)
		}
	}
	for key, want := range map[string]string{
		notify.TransportField:    chat.Source,
		notify.ChannelField:      "room-1",
		notify.ChannelTypeField:  string(chat.KindPublic),
		notify.ChannelKindField:  string(types.ChannelPublic),
		notify.ChannelNameField:  "launch",
		notify.MessageIDField:    "msg-1",
		notify.ThreadField:       "",
		notify.ThreadAnchorField: "msg-1",
		notify.UserField:         "jane",
		notify.ActorField:        "jane",
		notify.FollowReasonField: string(chat.ReasonMention),
		chat.MetaAuthorKind:      string(chat.AuthorHuman),
		chat.MetaAddressed:       "true",
	} {
		if got := meta[key]; got != want {
			t.Errorf("%s is %q, want %q", key, got, want)
		}
	}
	// A TOP-LEVEL MESSAGE RODE NO FOLLOW, so the key that says why the
	// seat is in the thread must be absent rather than empty: its presence
	// used to be read as an obligation, and a thread a seat had merely
	// spoken in once then obliged it to answer every later message for
	// ever.
	if _, ok := meta[notify.FollowingField]; ok {
		t.Errorf("a top-level mention was stamped as riding a thread follow")
	}
}

// A REPLY THAT RODE A STANDING ARRANGEMENT SAYS WHICH ONE.
//
// [notify.FollowingField] is why the seat is in this THREAD at all, which is
// the only answer a later reply has once the message that named it has
// scrolled away. It carries the REASON and never a bare "true": a follow is
// not an ask, and which follow decides whether the reply obliges an answer.
func TestAReplyStampsWhyTheSeatIsInTheThread(t *testing.T) {
	t.Parallel()
	rec := aPost()
	rec.Notify.ThreadRoot = "msg-0"
	rec.Notify.Recipients = []chat.Recipient{
		{Handle: "eng", Reason: chat.ReasonFollow},
		{Handle: "ops", Reason: chat.ReasonMention},
	}
	routed := wakes(t, rec, company(t, []string{"jane", "eng", "ops"}))

	for _, r := range routed {
		meta := r.Inbound.Metadata
		if meta[notify.ThreadAnchorField] != "msg-0" {
			t.Errorf("%s's reply anchors on %q, want the thread's own root — "+
				"the anchor is where a reply GOES, and the working indicator "+
				"raises on the same value", r.To.Handle,
				meta[notify.ThreadAnchorField])
		}
		switch r.To.Handle {
		case "eng":
			if meta[notify.FollowingField] != string(chat.ReasonFollow) {
				t.Errorf("a follower's reply says it is in the thread for "+
					"%q", meta[notify.FollowingField])
			}
		case "ops":
			if _, ok := meta[notify.FollowingField]; ok {
				t.Errorf("being named in THIS message was stamped as a " +
					"standing follow, which would address every later reply " +
					"in the thread for ever")
			}
		}
	}
}

// THE ROOM'S OWN KIND AND THE CANONICAL ONE ARE BOTH STAMPED, AND THEY ARE
// DIFFERENT FACTS.
//
// The raw kind is this domain's word, read only against the rule this domain
// declared. The canonical one is the cross-backend shape, read by consumers
// that must compare a native room with a Slack channel — and the mapping
// belongs in the only code that knows what a unit room is.
//
// AN UNKNOWN KIND IS UNKNOWN rather than being guessed into one of the four:
// the canonical value is a closed set a consumer switches on, and a kind a
// newer peer wrote has no place in it.
func TestAWakeCarriesBothTheRoomsOwnKindAndTheCanonicalOne(t *testing.T) {
	t.Parallel()
	for kind, want := range map[chat.Kind]types.ChannelKind{
		chat.KindDM:                  types.ChannelDM,
		chat.KindGroup:               types.ChannelGroup,
		chat.KindPrivate:             types.ChannelGroup,
		chat.KindPublic:              types.ChannelPublic,
		chat.KindUnit:                types.ChannelPublic,
		chat.Kind("something-newer"): types.ChannelUnknown,
	} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			rec := aPost()
			rec.Notify.ChannelKind = kind
			routed := wakes(t, rec, company(t, []string{"jane", "eng"}))
			if len(routed) != 1 {
				t.Fatalf("the record woke %+v", routed)
			}
			meta := routed[0].Inbound.Metadata
			if meta[notify.ChannelTypeField] != string(kind) {
				t.Errorf("the room's own kind is stamped as %q",
					meta[notify.ChannelTypeField])
			}
			if got := meta[notify.ChannelKindField]; got != string(want) {
				t.Errorf("the canonical kind is %q, want %q", got, want)
			}
		})
	}
}

// THE INDICATOR AND THE DELIVERY GATE CANNOT DISAGREE.
//
// Three subsystems ask "is somebody waiting on this seat?" and none of them
// may answer differently: the working indicator raises "is thinking…" while
// the agent works, the delivery check refuses to let an addressed turn end in
// silence, and the prompt tells the agent which of the two it is in. A spinner
// over a turn that is allowed to end in silence, and a silence after a
// spinner, are the same bug — one question answered twice.
//
// Here they are one evaluation read twice: the parser evaluates
// [chat.AddressRule] once and stamps the verdict, the prompt reads the stamp,
// and the indicator evaluates the same rule over the same metadata. The case
// that proves it is a reason notify has never heard of — `reply_to_own_root`
// and `lead_fallback` are not follows on any vendor — which a rule that fell
// back to the vendor vocabulary would read as "does not address".
func TestTheStampedVerdictIsTheRuleTheIndicatorEvaluates(t *testing.T) {
	t.Parallel()
	prompt := chat.NewPrompt()
	rule := chat.AddressRule()
	for _, reason := range append(slices.Clone(chat.Reasons),
		chat.Reason("something-newer")) {

		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			rec := aPost()
			rec.Notify.ChannelKind = chat.KindPublic
			rec.Notify.ThreadRoot = "msg-0"
			rec.Notify.Lead = "eng"
			rec.Notify.AuthorKind = chat.AuthorHuman
			rec.Notify.Recipients = []chat.Recipient{{Handle: "eng", Reason: reason}}
			routed := wakes(t, rec, company(t, []string{"jane", "eng"}))
			if len(routed) != 1 {
				t.Fatalf("a record naming one seat woke %+v", routed)
			}
			in := routed[0].Inbound

			gate := prompt.Addressed(in)
			indicator := rule.Addressed(in.Metadata)
			if gate != indicator {
				t.Fatalf("the reply obligation says %v and the working "+
					"indicator says %v for %q", gate, indicator, reason)
			}
			if want := reason.Addressed(); gate != want {
				t.Errorf("%q addresses=%v, and the vocabulary says %v — the "+
					"rule's follow set is derived from that vocabulary, so "+
					"the two cannot be different answers", reason, gate, want)
			}
		})
	}
}

// A DIRECT CONVERSATION ADDRESSES ITS PARTICIPANTS WHATEVER THE REASON SAYS.
//
// The rule's two halves are both load-bearing and they answer different
// questions. A reason a newer build routed under is one this build cannot
// read — but a message in a conversation that exists only between these
// parties was addressed to them by the room itself, and nothing addresses a
// seat more directly than that.
func TestADirectConversationIsAddressedByTheRoomItself(t *testing.T) {
	t.Parallel()
	rec := aPost()
	rec.Notify.ChannelKind = chat.KindDM
	rec.Notify.ChannelName = ""
	rec.Notify.Recipients = []chat.Recipient{
		{Handle: "eng", Reason: chat.Reason("something-newer")},
	}
	routed := wakes(t, rec, company(t, []string{"jane", "eng"}))
	if len(routed) != 1 {
		t.Fatalf("a direct message woke %+v", routed)
	}
	if !chat.NewPrompt().Addressed(routed[0].Inbound) {
		t.Error("a direct message left its recipient owing nobody an answer, " +
			"so the seat may end the turn in silence — and to the person who " +
			"asked, silence is indistinguishable from a message that was lost")
	}
}

// AN IMPORT WAKES NOBODY, AT THE PARSER TOO.
//
// The feed drops it first, so this is the second of two guards on one rule —
// deliberately, because the two layers are independent: the feed decides what
// to relay and the parser decides who a relayed record is for. A replayed body,
// a test or a second producer reaching the parser directly must not turn a
// year of somebody's Slack into a year of turns.
func TestARecordThatAnnouncedNothingWakesNobodyAtTheParser(t *testing.T) {
	t.Parallel()
	payload, err := chat.Encode(quiet(aPost()))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	body := map[string]any{}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("read the record back: %v", err)
	}
	routed, err := chat.NewParser().Parse(t.Context(),
		types.RawWebhook{Body: body}, company(t, []string{"jane", "eng"}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(routed) != 0 {
		t.Errorf("a record that announced nothing woke %+v", routed)
	}
}

// A DELIVERY WITH NO RECORD IN IT IS AN ERROR THAT SAYS SO.
//
// The parser's whole input is the record the feed relayed. An empty body is a
// producer that relayed something else, and answering "nobody" would make that
// indistinguishable from a company in which nothing is being said.
func TestAnEmptyDeliveryIsRefusedRatherThanRoutedToNobody(t *testing.T) {
	t.Parallel()
	_, err := chat.NewParser().Parse(t.Context(), types.RawWebhook{}, nil)
	if err == nil {
		t.Fatal("a delivery carrying no record parsed as a message nobody " +
			"was concerned by")
	}
	if !strings.Contains(err.Error(), "record") {
		t.Errorf("the refusal reads %q and does not say what is missing", err)
	}
}
