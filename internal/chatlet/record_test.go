package chatlet_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// handlesOf is n DISTINCT handles, for the cases that are about a COUNT.
//
// Distinct matters: a generator that repeated itself would trip the duplicate
// guard in a membership case before the cap it is actually testing.
func handlesOf(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("person-%04d", i)
	}
	return out
}

// A RECORD A NEWER BUILD WROTE RE-PUBLISHES BYTE FOR BYTE.
//
// A rolling upgrade puts a record carrying fields this build has never heard
// of on the wire, and the reanchor path READS AND REPUBLISHES records. A node
// that dropped what it did not understand would silently strip a newer peer's
// field out of the log — permanently, because the log is the only copy.
//
// The mutation that turns this red is removing the unknown-field collection
// from the decode, or the merge from the encode: the second encoding then
// loses the carried key and the bytes differ.
func TestARecordFromANewerBuildRePublishesByteForByte(t *testing.T) {
	t.Parallel()
	room := uuid.NewString()
	post, err := json.Marshal(chatlet.MessagePost{
		V: 1, MessageID: uuid.NewString(), Body: "shipping the release now",
		Author: "sarah-chen", AuthorKind: chatlet.AuthorAgent,
	})
	if err != nil {
		t.Fatalf("encode a post: %v", err)
	}
	original, err := chatlet.Encode(chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, OpID: uuid.NewString(),
			Subject: chatlet.MessageSubject(room), Op: chatlet.OpPost,
			CreatedAt: time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC),
			Gen:       2, Writer: "node-a", Scope: chatlet.ScopeSet{Subject: true},
		},
		Mutation: post, Actor: "sarah-chen", ActorKind: chatlet.AuthorAgent,
		// The half this build does not know: a field a later version
		// added beside the eight reserved envelope keys.
		Extra: map[string]json.RawMessage{
			"huddle_id":   json.RawMessage(`"huddle-7"`),
			"reaction_of": json.RawMessage(`{"emoji":":wave:"}`),
		},
	})
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}

	back, err := chatlet.Decode(original)
	if err != nil {
		t.Fatalf("decode a record this build's own version wrote: %v", err)
	}
	if len(back.Extra) != 2 {
		t.Fatalf("the decode kept %d unknown field(s), want 2 — a field it did "+
			"not collect is a field the next encode drops", len(back.Extra))
	}
	if back.Actor != "sarah-chen" || back.Op != chatlet.OpPost {
		t.Fatalf("the known half did not survive: %#v", back.RecordEnvelope)
	}

	again, err := chatlet.Encode(back)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(original, again) {
		t.Fatalf("a record re-published as\n  %s\nand was published as\n  %s\n"+
			"— the log is the only copy, so a stripped field is gone for good",
			again, original)
	}
}

// A CARRIED FIELD LOSES TO A KNOWN ONE.
//
// The merge has exactly one rule and it has to go this way round: a stale
// value carried from a decode must never overwrite what the caller set on the
// struct beside it. The other direction is a write that silently does nothing.
func TestACarriedFieldLosesToAKnownOne(t *testing.T) {
	t.Parallel()
	data, err := chatlet.Encode(chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, Subject: chatlet.ChannelSubject(uuid.NewString()),
			Op: chatlet.OpPatch, Scope: chatlet.ScopeSet{Subject: true},
		},
		Actor: "sarah-chen",
		Extra: map[string]json.RawMessage{"actor": json.RawMessage(`"impostor"`)},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("read the encoding back: %v", err)
	}
	if got := string(fields["actor"]); got != `"sarah-chen"` {
		t.Fatalf("the encoded actor is %s, want the value the caller set — a "+
			"carried copy that won would undo the write that set it", got)
	}
}

// THE ENVELOPE NEVER FAILS ON VERSION, AND THE PAYLOAD IS WHERE IT DOES.
//
// Without an envelope there is no position, no kind, no subject and no scope,
// so a record that failed the first pass could only be DROPPED. With one, a
// node that cannot read a newer peer's payload can still file the deferral
// under the subject, probe for it, drop it through an eviction gate and
// reprocess it after an upgrade.
func TestTheEnvelopeDecodesAtEveryVersionAndThePayloadDoesNot(t *testing.T) {
	t.Parallel()
	room := uuid.NewString()
	future, err := chatlet.Encode(chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion + 3, OpID: uuid.NewString(),
			Subject: chatlet.MessageSubject(room), Op: "huddle",
			Scope: chatlet.ScopeSet{Subject: true}, Writer: "node-b",
		},
		Mutation: json.RawMessage(`{"shape":"nobody here knows"}`),
	})
	if err != nil {
		t.Fatalf("encode a newer build's record: %v", err)
	}

	env, err := chatlet.DecodeEnvelope(future)
	if err != nil {
		t.Fatalf("the envelope pass refused a newer build's record: %v — there "+
			"would then be no subject to file it under and nothing to do but "+
			"drop it", err)
	}
	if env.Subject != chatlet.MessageSubject(room) || env.Writer != "node-b" {
		t.Fatalf("the envelope lost what the deferral is filed under: %#v", env)
	}

	rec, err := chatlet.Decode(future)
	var version *chatlet.ErrFutureVersion
	if !errors.As(err, &version) {
		t.Fatalf("Decode returned %v, want an *ErrFutureVersion", err)
	}
	if version.Got != chatlet.RecordVersion+3 || version.Want != chatlet.RecordVersion {
		t.Errorf("the version error says got %d want %d", version.Got, version.Want)
	}
	if version.Subject != chatlet.MessageSubject(room) {
		t.Errorf("the version error names subject %s — a version error with no "+
			"subject is a record that can only be dropped", version.Subject)
	}
	if rec.Subject != chatlet.MessageSubject(room) {
		t.Errorf("Decode returned no envelope beside the error, and that is " +
			"precisely the case the applier retains")
	}
}

// THE ENVELOPE STILL REFUSES WHAT LEAVES NOTHING TO FILE.
//
// "Never fails on version" is not "never fails": a record with no version at
// all cannot be told apart from a newer build's, and one whose subject is not
// a broker path has nowhere to be filed. Both are refusals about the record's
// own legibility rather than about its age.
func TestTheEnvelopeRefusesWhatLeavesNothingToFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, payload string }{
		{"not json at all", `{`},
		{"no version", `{"subject":{"k":"message","i":"room-1"},"op":"post"}`},
		{"a zero version", `{"v":0,"subject":{"k":"message","i":"room-1"}}`},
		{"a negative version", `{"v":-1,"subject":{"k":"message","i":"room-1"}}`},
		{"no subject kind", `{"v":1,"subject":{"i":"room-1"},"op":"post"}`},
		{"a subject id a broker reads as a pattern",
			`{"v":1,"subject":{"k":"message","i":"room>"},"op":"post"}`},
	} {
		if _, err := chatlet.DecodeEnvelope([]byte(tc.payload)); err == nil {
			t.Errorf("%s: the envelope pass accepted %s", tc.name, tc.payload)
		}
	}
}

// A GATE IS ANSWERED FROM THE ENVELOPE ALONE, AND DESTRUCTION IS A GATE.
//
// The question has to be answerable by a node that cannot decode the payload,
// because it is what turns an unknown version into a STOP rather than a
// deferral. An eviction is a gate by its KIND. An erase and a prune are gates
// by their OP, on an ordinary channel subject, and they must be: both DELETE
// rows, and a deferred deletion is a node still serving what every other node
// removed, with no inverse that repairs it.
func TestEveryRecordThatDeletesOrFencesInstallsAGate(t *testing.T) {
	t.Parallel()
	room := uuid.NewString()
	for _, tc := range []struct {
		name string
		env  chatlet.RecordEnvelope
		want bool
	}{
		{"an eviction", chatlet.RecordEnvelope{
			Subject: chatlet.EvictionSubject("node-a"), Op: chatlet.OpEviction}, true},
		{"an erase", chatlet.RecordEnvelope{
			Subject: chatlet.ChannelSubject(room), Op: chatlet.OpErase}, true},
		{"a prune", chatlet.RecordEnvelope{
			Subject: chatlet.ChannelSubject(room), Op: chatlet.OpPrune}, true},
		{"a post", chatlet.RecordEnvelope{
			Subject: chatlet.MessageSubject(room), Op: chatlet.OpPost}, false},
		{"a tombstoning delete", chatlet.RecordEnvelope{
			Subject: chatlet.MessageSubject(room), Op: chatlet.OpDelete}, false},
		{"a settings patch", chatlet.RecordEnvelope{
			Subject: chatlet.ChannelSubject(room), Op: chatlet.OpPatch}, false},
		{"a barrier", chatlet.RecordEnvelope{
			Subject: chatlet.BarrierSubject(), Op: chatlet.OpBarrier}, false},
	} {
		if got := tc.env.InstallsGate(); got != tc.want {
			t.Errorf("%s: InstallsGate() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// THE WAKE VOCABULARY PUTS EVERY ADDRESSED REASON BEFORE EVERY OTHER.
//
// [chatlet.Reasons] is precedence order — a recipient appears once, under the
// first reason that applies — and the ordering rests on one rule a reader can
// check: an obligation to answer outranks an FYI. A vocabulary where a
// `follow_all` sat above a `mention` would file somebody's name in a room's
// noise, and nothing downstream could tell.
func TestThePrecedenceOrderPutsEveryAddressedReasonFirst(t *testing.T) {
	t.Parallel()
	if len(chatlet.Reasons) != 8 {
		t.Fatalf("the vocabulary has %d reasons, want 8", len(chatlet.Reasons))
	}
	seenUnaddressed := false
	for _, r := range chatlet.Reasons {
		if !r.Valid() {
			t.Errorf("declared reason %q reports itself invalid", r)
		}
		if r.Addressed() {
			if seenUnaddressed {
				t.Errorf("%q is addressed and sits below an unaddressed reason "+
					"— an obligation to answer outranks an FYI, and the order "+
					"is what the routing walks", r)
			}
			continue
		}
		seenUnaddressed = true
	}
	// THE ADDRESSED SET IS EXACTLY FOUR, written out rather than counted
	// off the method that is under test.
	want := []chatlet.Reason{
		chatlet.ReasonDM, chatlet.ReasonMention, chatlet.ReasonReplyToOwnRoot,
		chatlet.ReasonLeadFallback,
	}
	var got []chatlet.Reason
	for _, r := range chatlet.Reasons {
		if r.Addressed() {
			got = append(got, r)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the addressed reasons are %v, want %v — a seat that treats a "+
			"collective address or a standing follow as addressed answers every "+
			"remark in every room it follows", got, want)
	}
	// A REPLY AS A PARTICIPANT IS NOT ADDRESSED AND A REPLY TO YOUR OWN
	// ROOT IS: that split is the whole reason the vocabulary has eight
	// values rather than seven.
	if chatlet.ReasonReply.Addressed() {
		t.Error("a plain reply obliges an answer, which would make every busy " +
			"thread oblige every past speaker to answer again")
	}
	if !chatlet.ReasonReplyToOwnRoot.Addressed() {
		t.Error("a reply to the thread you started does not oblige an answer")
	}
	if chatlet.Reason("huddle").Valid() || chatlet.Reason("huddle").Addressed() {
		t.Error("a reason this build never declared is valid or addressed")
	}
	// AND NO REASON IS LISTED TWICE. A precedence walk over a slice with a
	// repeat would assign one recipient two reasons, and the second would
	// be the weaker one.
	seen := map[chatlet.Reason]bool{}
	for _, r := range chatlet.Reasons {
		if seen[r] {
			t.Errorf("the vocabulary lists %q twice", r)
		}
		seen[r] = true
	}
}

// aNotify is a valid routing snapshot the cap cases each break one field of.
func aNotify() *chatlet.Notify {
	return &chatlet.Notify{
		MessageID: uuid.NewString(), ChannelID: uuid.NewString(),
		ChannelName: "launch", ChannelKind: chatlet.KindPublic,
		Author: "sarah-chen", AuthorKind: chatlet.AuthorAgent,
		Excerpt: "shipping the release now",
	}
}

// EVERY COLLECTION A ROUTING SNAPSHOT CARRIES IS BOUNDED AT THE PUBLISH
// BOUNDARY.
//
// A snapshot is copied onto the log, replicated to every node, held for the
// stream's whole retention window and read back by every applier — so an
// unbounded collection is not one screen rendering badly, it is bytes every
// member of the fleet stores for a year. THE WALK IS PER FIELD, because a
// check that bounded three of four would pass any case written about the
// interesting one.
func TestEveryNotifyCollectionIsBoundedAndNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field string
		over  func(*chatlet.Notify)
	}{
		{"excerpt", func(n *chatlet.Notify) {
			n.Excerpt = strings.Repeat("x", chatlet.MaxExcerpt+1)
		}},
		{"mentions", func(n *chatlet.Notify) {
			n.Mentions = handlesOf(chatlet.MaxMentions + 1)
		}},
		{"recipients", func(n *chatlet.Notify) {
			for _, h := range handlesOf(chatlet.MaxRecipients + 1) {
				n.Recipients = append(n.Recipients,
					chatlet.Recipient{Handle: h, Reason: chatlet.ReasonFollowAll})
			}
		}},
	} {
		n := aNotify()
		tc.over(n)
		err := n.Validate()
		if err == nil {
			t.Errorf("%s: a snapshot over the cap was published", tc.field)
			continue
		}
		if !errors.Is(err, chatlet.ErrInvalid) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: the refusal %q does not name the field", tc.field, err)
		}
	}

	for _, tc := range []struct {
		name   string
		field  string
		mangle func(*chatlet.Notify)
	}{
		{"no message", "message_id", func(n *chatlet.Notify) { n.MessageID = "" }},
		{"no room", "channel_id", func(n *chatlet.Notify) { n.ChannelID = "" }},
		{"an unknown channel kind", "channel_kind", func(n *chatlet.Notify) {
			n.ChannelKind = "huddle"
		}},
		{"an unknown author kind", "author_kind", func(n *chatlet.Notify) {
			n.AuthorKind = "daemon"
		}},
		{"a recipient with no handle", "recipients", func(n *chatlet.Notify) {
			n.Recipients = []chatlet.Recipient{{Reason: chatlet.ReasonMention}}
		}},
		{"a reason from a newer build", "recipients", func(n *chatlet.Notify) {
			n.Recipients = []chatlet.Recipient{{Handle: "amir-haddad", Reason: "huddle"}}
		}},
	} {
		n := aNotify()
		tc.mangle(n)
		err := n.Validate()
		if err == nil {
			t.Errorf("%s: validated", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: the refusal %q does not name %s", tc.name, err, tc.field)
		}
	}

	// AND A NIL SNAPSHOT IS LEGAL, because nil is how a record says it
	// wakes nobody — which is what a system line is.
	var quiet *chatlet.Notify
	if err := quiet.Validate(); err != nil {
		t.Errorf("a record that wakes nobody was refused: %v", err)
	}
	if err := aNotify().Validate(); err != nil {
		t.Errorf("an ordinary snapshot was refused: %v", err)
	}
}

// AN EXCERPT IS CUT INSIDE ITS OWN BUDGET, ON A RUNE BOUNDARY.
//
// [chatlet.MaxExcerpt] is a CEILING [chatlet.Notify.Validate] enforces, so a marker
// appended outside the budget would turn every long message into a refused
// write rather than a marked one. And a plain byte cut through a multi-byte
// rune is invalid UTF-8, which a JSON encoder substitutes and a model reads as
// a replacement character.
func TestAnExcerptIsCutInsideItsOwnBudget(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", chatlet.MaxExcerpt)
	cut := chatlet.Excerpt(long)
	if len(cut) > chatlet.MaxExcerpt {
		t.Fatalf("an excerpt is %d bytes against a %d ceiling the write itself "+
			"refuses", len(cut), chatlet.MaxExcerpt)
	}
	if !strings.Contains(cut, "…") {
		t.Error("a cut excerpt carries no marker, so a reader cannot tell the " +
			"remainder from the whole")
	}
	if strings.ContainsRune(cut, '�') {
		t.Fatal("the cut split a multi-byte rune, which a JSON encoder " +
			"substitutes and a model reads as a replacement character")
	}
	n := aNotify()
	n.Excerpt = cut
	if err := n.Validate(); err != nil {
		t.Fatalf("an excerpt this package cut was refused by the cap it was "+
			"cut against: %v", err)
	}
	if short := "all fine"; chatlet.Excerpt(short) != short {
		t.Error("a short body was cut")
	}
}

// EVERY RECORD AT EVERY CAP AT ONCE STILL FITS THE BOUND THE LOG DECLARES.
//
// [chatlet.MaxRecordBytes] is what the framework refuses a larger record at,
// `record_too_large`, before anything is sent — so a set of caps that each
// looked reasonable alone, and together let a gesture form a larger record,
// would be a message somebody typed and lost, or a room's membership the org
// chart could never write. Every op is formed here at every cap at once, with
// the text that ENCODES largest: the record is JSON, and a `<`, a `>`, a `&`,
// a control character or an invalid byte each becomes a six-byte escape, so a
// body of [chatlet.MaxBody] bytes is up to six times that on the wire. The
// handles are the widest a seat may have (`iam.MaxLogin`), every member
// follows the room, and a unit is keyed as long as an `id` may be.
func TestEveryRecordAtEveryCapFitsTheDeclaredBound(t *testing.T) {
	t.Parallel()
	bound := chatlet.Domain{}.Stream().MaxRecordBytes
	if bound != chatlet.MaxRecordBytes {
		t.Fatalf("the stream declares a largest record of %d and the package %d",
			bound, chatlet.MaxRecordBytes)
	}
	worst := func(n int) string { return strings.Repeat("<", n) }
	wide := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			head := fmt.Sprintf("seat-%05d-", i)
			out[i] = head + strings.Repeat("w", 64-len(head))
		}
		return out
	}
	links := make([]string, chatlet.MaxLinks)
	for i := range links {
		links[i] = fmt.Sprintf("https://example.com/%d?", i) +
			strings.Repeat("&", chatlet.MaxLinkBytes-24)
	}
	members := make([]chatlet.Member, 0, chatlet.MaxMembers)
	for _, h := range wide(chatlet.MaxMembers) {
		members = append(members, chatlet.Member{Handle: h, FollowAll: true})
	}
	name := strings.Repeat("n", chatlet.MaxChannelName)
	unit := strings.Repeat("u", 64)
	notify := aNotify()
	notify.Mentions = wide(chatlet.MaxMentions)
	notify.ThreadRoot = uuid.NewString()
	notify.ChannelName = name
	notify.Excerpt = worst(chatlet.MaxExcerpt)
	notify.Author = wide(1)[0]
	notify.Lead = wide(1)[0]
	notify.WakesTruncated = true
	for _, h := range wide(chatlet.MaxRecipients) {
		notify.Recipients = append(notify.Recipients,
			chatlet.Recipient{Handle: h, Reason: chatlet.ReasonReplyToOwnRoot})
	}
	days := chatlet.RetentionForever
	archived := true
	erased := make([]string, chatlet.MaxEraseMessages)
	for i := range erased {
		erased[i] = uuid.NewString()
	}
	cases := []struct {
		op      chatlet.OpKind
		subject chatlet.Subject
		payload chatlet.Payload
		notify  *chatlet.Notify
	}{
		{chatlet.OpPost, chatlet.MessageSubject(uuid.NewString()), chatlet.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: worst(chatlet.MaxBody),
			ThreadRoot: uuid.NewString(), Mentions: wide(chatlet.MaxMentions),
			Collective: true, Links: links, Author: wide(1)[0],
			AuthorKind: chatlet.AuthorHuman,
		}, notify},
		{chatlet.OpEdit, chatlet.MessageSubject(uuid.NewString()), chatlet.MessageEdit{
			V: 1, MessageID: uuid.NewString(), Body: worst(chatlet.MaxBody),
			Mentions: wide(chatlet.MaxMentions), Links: links,
			EditedBy: wide(1)[0], EditedByKind: chatlet.AuthorHuman,
		}, notify},
		{chatlet.OpCreate, chatlet.ChannelNameSubject(name), chatlet.ChannelCreate{
			V: 1, ChannelID: uuid.NewString(), Kind: chatlet.KindUnit, Name: name,
			Unit: unit, Topic: worst(chatlet.MaxTopic), Purpose: worst(chatlet.MaxPurpose),
			Members: members, CreatedBy: wide(1)[0], CreatedByKind: chatlet.AuthorSystem,
			ChartEpoch: time.Now().UnixNano(),
		}, nil},
		{chatlet.OpMembers, chatlet.ChannelSubject(uuid.NewString()), chatlet.MemberSet{
			V: 1, Members: members, ChartEpoch: time.Now().UnixNano(), Unit: unit,
		}, nil},
		{chatlet.OpPatch, chatlet.ChannelSubject(uuid.NewString()), chatlet.ChannelPatch{
			V: 1, Topic: ptr(worst(chatlet.MaxTopic)),
			Purpose: ptr(worst(chatlet.MaxPurpose)), Archived: &archived,
		}, nil},
		{chatlet.OpRetention, chatlet.ChannelSubject(uuid.NewString()), chatlet.RetentionSet{
			V: 1, RetentionDays: &days,
		}, nil},
		{chatlet.OpErase, chatlet.ChannelSubject(uuid.NewString()), chatlet.MessageErase{
			V: 1, MessageIDs: erased, Reason: worst(chatlet.MaxEraseReason),
			By: wide(1)[0], ByKind: chatlet.AuthorOperator,
		}, nil},
		{chatlet.OpReact, chatlet.MessageSubject(uuid.NewString()), chatlet.Reaction{
			V: 1, MessageID: uuid.NewString(), Emoji: worst(chatlet.MaxEmoji),
			By: wide(1)[0],
		}, nil},
		{chatlet.OpDelete, chatlet.MessageSubject(uuid.NewString()), chatlet.MessageDelete{
			V: 1, MessageID: uuid.NewString(), DeletedBy: wide(1)[0],
			DeletedByKind: chatlet.AuthorHuman,
		}, nil},
		{chatlet.OpPrune, chatlet.ChannelSubject(uuid.NewString()), chatlet.Prune{
			V: 1, Cutoff: time.Now().UTC(),
		}, nil},
	}
	largest := 0
	for _, c := range cases {
		if err := c.payload.Validate(); err != nil {
			t.Fatalf("a %s at every cap was refused by its own caps: %v", c.op, err)
		}
		if err := c.notify.Validate(); err != nil {
			t.Fatalf("a %s's snapshot at every cap was refused: %v", c.op, err)
		}
		payload, err := json.Marshal(c.payload)
		if err != nil {
			t.Fatalf("encode the %s: %v", c.op, err)
		}
		record, err := chatlet.Encode(chatlet.MutationRecord{
			RecordEnvelope: chatlet.RecordEnvelope{
				OpID: uuid.NewString(), Subject: c.subject, Op: c.op,
				CreatedAt: time.Now().UTC(), Gen: 9, Writer: wide(1)[0],
				Scope: chatlet.ScopeSet{Subject: true},
			},
			Mutation: payload, Actor: wide(1)[0], ActorKind: chatlet.AuthorHuman,
			OperatorID: wide(1)[0], TurnID: uuid.NewString(),
			Chain: wide(4), Notify: c.notify,
		})
		if err != nil {
			t.Fatalf("encode the %s record: %v", c.op, err)
		}
		largest = max(largest, len(record))
		if int64(len(record)) > bound {
			t.Errorf("a %s at every cap encodes to %d bytes against the %d this "+
				"log declares — the caps have to be held together, because "+
				"they all ride one record", c.op, len(record), bound)
		}
	}
	t.Logf("the largest record at every cap is %d bytes of %d", largest, bound)
}

// THE BARRIER IS THE ONE RECORD THAT MAY NOT CARRY AN OP ID.
//
// An op id becomes the Nats-Msg-Id, and a repeat inside the duplicate window
// is answered from the dedupe cache with NO quorum round trip at all — so the
// sequence it returns is a position nothing confirmed, which is exactly the
// claim a barrier exists to make and the one it must never fake.
func TestABarrierRefusesAnOpID(t *testing.T) {
	t.Parallel()
	if _, err := chatlet.EncodeBarrier(statelogBarrier("op-1")); err == nil {
		t.Fatal("a barrier carrying an op id was encoded")
	}
	data, err := chatlet.EncodeBarrier(statelogBarrier(""))
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	rec, err := chatlet.Decode(data)
	if err != nil {
		t.Fatalf("decode a barrier: %v", err)
	}
	if rec.Subject != chatlet.BarrierSubject() || rec.Op != chatlet.OpBarrier {
		t.Fatalf("a barrier decoded as %#v", rec.RecordEnvelope)
	}
	payload, err := chatlet.DecodeMutation(rec)
	if err != nil || payload != nil {
		t.Fatalf("DecodeMutation on a barrier = (%v, %v), want (nil, nil) — the "+
			"one payload-free record, and nil is its value rather than an "+
			"absence", payload, err)
	}
}

// statelogBarrier is the framework's own envelope for a read barrier.
func statelogBarrier(opID string) statelog.Envelope {
	return statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind},
		OpID:    opID, Gen: 2,
	}
}

// EVERY OP RIDES A SUBJECT, AND THE CREATE IS THE ONE WITH TWO ANSWERS.
//
// The table that says where a record is PUBLISHED and the switch that says
// what it MEANS are two halves of one dispatch. Written apart and left
// unchecked, the first op to move publishes to a subject whose kind the
// applier has no case for — the record is accepted, delivered and faults on a
// pair nobody wrote.
func TestEveryOpRidesASubjectAndTheCreateHasTwoAnswers(t *testing.T) {
	t.Parallel()
	want := map[chatlet.OpKind]chatlet.ObjectKind{
		chatlet.OpPatch:      chatlet.KindChannel,
		chatlet.OpMembers:    chatlet.KindChannel,
		chatlet.OpErase:      chatlet.KindChannel,
		chatlet.OpPrune:      chatlet.KindChannel,
		chatlet.OpRetention:  chatlet.KindChannel,
		chatlet.OpPost:       chatlet.KindMessage,
		chatlet.OpEdit:       chatlet.KindMessage,
		chatlet.OpDelete:     chatlet.KindMessage,
		chatlet.OpReact:      chatlet.KindMessage,
		chatlet.OpEviction:   chatlet.KindEviction,
		chatlet.OpGeneration: chatlet.KindGeneration,
		chatlet.OpBarrier:    chatlet.KindBarrier,
	}
	for _, op := range chatlet.OpKinds {
		if !op.Valid() {
			t.Errorf("declared op %q reports itself invalid", op)
		}
		if op == chatlet.OpCreate {
			continue
		}
		got, ok := chatlet.SubjectKind(op, chatlet.KindPublic)
		if !ok {
			t.Errorf("op %q rides no subject, so nothing can publish it", op)
			continue
		}
		if got != want[op] {
			t.Errorf("op %q rides %q, want %q", op, got, want[op])
		}
	}
	if len(want)+1 != len(chatlet.OpKinds) {
		t.Errorf("the domain declares %d ops and this suite places %d plus the "+
			"create", len(chatlet.OpKinds), len(want))
	}

	// A NAMED ROOM'S CREATE ARBITRATES ON ITS NAME, so two people typing
	// `#launch` contend at the broker. A DIRECT one has no name to contend
	// for, so it arbitrates on its own derived id, create-only.
	for _, k := range []chatlet.Kind{chatlet.KindPublic, chatlet.KindPrivate, chatlet.KindUnit} {
		if got, _ := chatlet.SubjectKind(chatlet.OpCreate, k); got != chatlet.KindChannelName {
			t.Errorf("a %s room's create rides %q, want %q — two rooms with one "+
				"name is not a state this alphabet can express",
				k, got, chatlet.KindChannelName)
		}
	}
	for _, k := range []chatlet.Kind{chatlet.KindDM, chatlet.KindGroup} {
		if got, _ := chatlet.SubjectKind(chatlet.OpCreate, k); got != chatlet.KindChannel {
			t.Errorf("a %s conversation's create rides %q, want %q",
				k, got, chatlet.KindChannel)
		}
	}
	if _, ok := chatlet.SubjectKind(chatlet.OpCreate, "huddle"); ok {
		t.Error("a create for an unknown channel kind was given a subject — the " +
			"two answers are different arbitration disciplines, and guessing " +
			"the wrong one for a named room is two rooms with one name")
	}
	if _, ok := chatlet.SubjectKind("huddle", chatlet.KindPublic); ok {
		t.Error("an op this build never declared was given a subject")
	}
}

// THE PAYLOAD DISPATCH IS ON (KIND, OP) AND NOTHING ELSE.
//
// A pair this build does not know is a NEWER PEER'S record, not an empty one
// — so it is an error naming both rather than a nil payload the applier would
// apply as a patch that changes nothing.
func TestThePayloadDispatchIsOnTheKindAndOpTogether(t *testing.T) {
	t.Parallel()
	room := uuid.NewString()
	for _, tc := range []struct {
		subject chatlet.Subject
		op      chatlet.OpKind
		payload any
	}{
		{chatlet.ChannelNameSubject("launch"), chatlet.OpCreate, aChannelCreate()},
		{chatlet.ChannelSubject(room), chatlet.OpCreate, aChannelCreate()},
		{chatlet.ChannelSubject(room), chatlet.OpPatch, chatlet.ChannelPatch{V: 1}},
		{chatlet.ChannelSubject(room), chatlet.OpMembers, chatlet.MemberSet{V: 1}},
		{chatlet.ChannelSubject(room), chatlet.OpErase, chatlet.MessageErase{V: 1}},
		{chatlet.ChannelSubject(room), chatlet.OpPrune, chatlet.Prune{V: 1}},
		{chatlet.MessageSubject(room), chatlet.OpPost, chatlet.MessagePost{V: 1}},
		{chatlet.MessageSubject(room), chatlet.OpEdit, chatlet.MessageEdit{V: 1}},
		{chatlet.MessageSubject(room), chatlet.OpDelete, chatlet.MessageDelete{V: 1}},
		{chatlet.MessageSubject(room), chatlet.OpReact, chatlet.Reaction{V: 1}},
		{chatlet.EvictionSubject("node-a"), chatlet.OpEviction, chatlet.Eviction{V: 1}},
		{chatlet.GenerationSubject(3), chatlet.OpGeneration, chatlet.Generation{V: 1}},
	} {
		body, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatalf("encode a %s payload: %v", tc.op, err)
		}
		got, err := chatlet.DecodeMutation(chatlet.MutationRecord{
			RecordEnvelope: chatlet.RecordEnvelope{
				V: chatlet.RecordVersion, Subject: tc.subject, Op: tc.op,
				Scope: chatlet.ScopeSet{Subject: true},
			},
			Mutation: body,
		})
		if err != nil {
			t.Errorf("(%s, %s) has no payload shape: %v", tc.subject.Kind, tc.op, err)
			continue
		}
		if got == nil {
			t.Errorf("(%s, %s) decoded to nothing", tc.subject.Kind, tc.op)
		}
	}

	// A PAIR NOBODY WROTE NAMES BOTH HALVES IN THE REFUSAL, so an operator
	// reading the log can tell a newer peer's op from a newer peer's kind.
	for _, tc := range []struct {
		subject chatlet.Subject
		op      chatlet.OpKind
	}{
		{chatlet.MessageSubject(room), chatlet.OpPatch},
		{chatlet.ChannelSubject(room), chatlet.OpPost},
		{chatlet.ChannelNameSubject("launch"), chatlet.OpPatch},
		{chatlet.Subject{Kind: "huddle", ID: room}, chatlet.OpPost},
		{chatlet.MessageSubject(room), "huddle"},
	} {
		_, err := chatlet.DecodeMutation(chatlet.MutationRecord{
			RecordEnvelope: chatlet.RecordEnvelope{
				V: chatlet.RecordVersion, Subject: tc.subject, Op: tc.op,
			},
			Mutation: json.RawMessage(`{"v":1}`),
		})
		if err == nil {
			t.Errorf("(%s, %s) decoded to a payload this build never writes",
				tc.subject.Kind, tc.op)
			continue
		}
		if !strings.Contains(err.Error(), string(tc.subject.Kind)) ||
			!strings.Contains(err.Error(), string(tc.op)) {
			t.Errorf("the refusal %q names neither the kind nor the op", err)
		}
	}

	// AND A PAIR THAT NEEDS A PAYLOAD AND HAS NONE IS REFUSED, rather than
	// decoded into a zero value the applier would write as a real change.
	if _, err := chatlet.DecodeMutation(chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, Subject: chatlet.MessageSubject(room),
			Op: chatlet.OpPost,
		},
	}); err == nil {
		t.Error("a post with no payload decoded to an empty message")
	}
}

// aChannelCreate is a valid named-room create the cases below break one field
// of at a time.
func aChannelCreate() chatlet.ChannelCreate {
	return chatlet.ChannelCreate{
		V: 1, ChannelID: uuid.NewString(), Kind: chatlet.KindPublic,
		Name: "launch", Topic: "the 2.0 release",
		Members:   []chatlet.Member{{Handle: "sarah-chen"}, {Handle: "amir-haddad", FollowAll: true}},
		CreatedBy: "sarah-chen", CreatedByKind: chatlet.AuthorHuman,
	}
}

// aDirectCreate is a valid direct conversation, id and all.
func aDirectCreate() chatlet.ChannelCreate {
	members := []chatlet.Member{{Handle: "sarah-chen"}, {Handle: "amir-haddad"}}
	return chatlet.ChannelCreate{
		V: 1, Kind: chatlet.KindDM, Members: members,
		ChannelID: chatlet.DirectChannelID(
			[]string{"sarah-chen", "amir-haddad"}).String(),
		CreatedBy: "sarah-chen", CreatedByKind: chatlet.AuthorHuman,
	}
}

// EVERY PAYLOAD REFUSES WHAT IT WILL NOT WRITE, NAMING THE FIELD.
//
// A cap enforced at each caller is a cap missing from whichever caller is
// written next, so each payload states its own — and the refusal names the
// field a person has to change, because an error that says only "invalid" is
// one somebody retries unchanged.
func TestEveryPayloadRefusesWhatItWillNotWriteNamingTheField(t *testing.T) {
	t.Parallel()
	crowd := handlesOf(chatlet.MaxMembers + 1)
	tooMany := make([]chatlet.Member, len(crowd))
	for i, h := range crowd {
		tooMany[i] = chatlet.Member{Handle: h}
	}
	erased := make([]string, chatlet.MaxEraseMessages+1)
	for i := range erased {
		erased[i] = uuid.NewString()
	}

	for _, tc := range []struct {
		name    string
		field   string
		payload chatlet.Payload
	}{
		{"a create with no id", "channel_id", func() chatlet.Payload {
			c := aChannelCreate()
			c.ChannelID = ""
			return c
		}()},
		{"a create with a kind from a newer build", "kind", func() chatlet.Payload {
			c := aChannelCreate()
			c.Kind = "huddle"
			return c
		}()},
		{"a named room with an illegal address", "name", func() chatlet.Payload {
			c := aChannelCreate()
			c.Name = "The Launch Room"
			return c
		}()},
		{"a unit room naming no unit", "unit", func() chatlet.Payload {
			c := aChannelCreate()
			c.Kind = chatlet.KindUnit
			return c
		}()},
		{"a public room naming a unit", "unit", func() chatlet.Payload {
			c := aChannelCreate()
			c.Unit = "engineering"
			return c
		}()},
		// THE CHART'S ROOM CARRIES THE CHART'S ACTIVATION, and only it:
		// without one, an older chart applied late cannot be told apart
		// from the current one.
		{"a unit room naming no activation", "chart_epoch", func() chatlet.Payload {
			c := aChannelCreate()
			c.Kind, c.Unit = chatlet.KindUnit, "engineering"
			return c
		}()},
		{"a public room naming an activation", "chart_epoch", func() chatlet.Payload {
			c := aChannelCreate()
			c.ChartEpoch = 1_772_445_600_000
			return c
		}()},
		{"a direct conversation with a name", "name", func() chatlet.Payload {
			c := aDirectCreate()
			c.Name = "sarah-and-amir"
			return c
		}()},
		{"a direct conversation claiming an id it did not derive", "channel_id",
			func() chatlet.Payload {
				c := aDirectCreate()
				c.ChannelID = uuid.NewString()
				return c
			}()},
		{"a direct conversation of one", "members", func() chatlet.Payload {
			c := aDirectCreate()
			c.Members = []chatlet.Member{{Handle: "sarah-chen"}}
			c.ChannelID = chatlet.DirectChannelID([]string{"sarah-chen"}).String()
			return c
		}()},
		{"a direct conversation over the participant cap", "members",
			func() chatlet.Payload {
				handles := handlesOf(chatlet.MaxDMParticipants + 1)
				members := make([]chatlet.Member, len(handles))
				for i, h := range handles {
					members[i] = chatlet.Member{Handle: h}
				}
				c := aDirectCreate()
				c.Members = members
				c.ChannelID = chatlet.DirectChannelID(handles).String()
				return c
			}()},
		{"a create over the topic cap", "topic", func() chatlet.Payload {
			c := aChannelCreate()
			c.Topic = strings.Repeat("t", chatlet.MaxTopic+1)
			return c
		}()},
		{"a create over the purpose cap", "purpose", func() chatlet.Payload {
			c := aChannelCreate()
			c.Purpose = strings.Repeat("p", chatlet.MaxPurpose+1)
			return c
		}()},
		{"a retention that is negative", "retention_days", func() chatlet.Payload {
			days := -1
			return chatlet.RetentionSet{V: 1, RetentionDays: &days}
		}()},
		{"an erase whose reason is over its cap", "reason", chatlet.MessageErase{
			V: 1, MessageIDs: erased[:2], By: "ops", ByKind: chatlet.AuthorHuman,
			Reason: strings.Repeat("r", chatlet.MaxEraseReason+1),
		}},
		{"a create with no author", "created_by", func() chatlet.Payload {
			c := aChannelCreate()
			c.CreatedBy = ""
			return c
		}()},
		{"a patch that sets nothing", "patch", chatlet.ChannelPatch{V: 1}},
		{"a membership over the cap", "members", chatlet.MemberSet{V: 1, Members: tooMany}},
		{"a membership naming a negative activation", "chart_epoch",
			chatlet.MemberSet{V: 1, ChartEpoch: -1}},
		{"a re-home naming no activation", "unit",
			chatlet.MemberSet{V: 1, Unit: "platform"}},
		{"a re-home to a padded unit", "unit",
			chatlet.MemberSet{V: 1, ChartEpoch: 1_772_445_600_000, Unit: " platform"}},
		{"a membership naming somebody twice", "members", chatlet.MemberSet{
			V: 1, Members: []chatlet.Member{{Handle: "sarah-chen"}, {Handle: "sarah-chen"}},
		}},
		{"a post with neither text nor a link", "body", chatlet.MessagePost{
			V: 1, MessageID: uuid.NewString(), Author: "sarah-chen",
			AuthorKind: chatlet.AuthorAgent,
		}},
		{"a post over the body cap", "body", chatlet.MessagePost{
			V: 1, MessageID: uuid.NewString(),
			Body:   strings.Repeat("x", chatlet.MaxBody+1),
			Author: "sarah-chen", AuthorKind: chatlet.AuthorAgent,
		}},
		{"a post over the mention cap", "mentions", chatlet.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: "hi",
			Mentions: handlesOf(chatlet.MaxMentions + 1),
			Author:   "sarah-chen", AuthorKind: chatlet.AuthorAgent,
		}},
		{"a post over the link cap", "links", chatlet.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: "hi",
			Links:  make([]string, chatlet.MaxLinks+1),
			Author: "sarah-chen", AuthorKind: chatlet.AuthorAgent,
		}},
		{"a post with a link over its own cap", "links[0]", chatlet.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: "hi",
			Links:  []string{strings.Repeat("u", chatlet.MaxLinkBytes+1)},
			Author: "sarah-chen", AuthorKind: chatlet.AuthorAgent,
		}},
		{"a post linking to no address", "links[1]", chatlet.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: "hi",
			Links:  []string{"https://example.com/plan", "/worklet/ENG-1"},
			Author: "sarah-chen", AuthorKind: chatlet.AuthorAgent,
		}},
		{"an edit that empties the message", "body", chatlet.MessageEdit{
			V: 1, MessageID: uuid.NewString(),
			EditedBy: "sarah-chen", EditedByKind: chatlet.AuthorHuman,
		}},
		{"an edit linking in a scheme no reader opens", "links[0]", chatlet.MessageEdit{
			V: 1, MessageID: uuid.NewString(), Body: "hi",
			Links:    []string{"javascript:void(0)"},
			EditedBy: "sarah-chen", EditedByKind: chatlet.AuthorHuman,
		}},
		{"a deletion naming nobody", "deleted_by", chatlet.MessageDelete{
			V: 1, MessageID: uuid.NewString(), DeletedByKind: chatlet.AuthorHuman,
		}},
		{"an erase over the cap", "message_ids", chatlet.MessageErase{
			V: 1, MessageIDs: erased, By: "ops", ByKind: chatlet.AuthorHuman,
		}},
		{"an erase written by an agent", "by_kind", chatlet.MessageErase{
			V: 1, MessageIDs: erased[:2], By: "sarah-chen", ByKind: chatlet.AuthorAgent,
		}},
		{"a reaction with no emoji", "emoji", chatlet.Reaction{
			V: 1, MessageID: uuid.NewString(), By: "sarah-chen",
		}},
		{"a reaction over the emoji cap", "emoji", chatlet.Reaction{
			V: 1, MessageID: uuid.NewString(), By: "sarah-chen",
			Emoji: strings.Repeat("e", chatlet.MaxEmoji+1),
		}},
		{"a prune with no cutoff", "cutoff", chatlet.Prune{V: 1}},
		{"an eviction naming no node", "node_id", chatlet.Eviction{
			V: chatlet.GateRecordVersion, EvictedAt: time.Now().UTC(),
		}},
		{"an eviction at a version a gate may never take", "v", chatlet.Eviction{
			V: chatlet.GateRecordVersion + 1, NodeID: "node-a",
			EvictedAt: time.Now().UTC(),
		}},
		{"a reanchor claiming generation zero", "generation", chatlet.Generation{
			V: 1, StreamCreatedAt: time.Now().UTC(),
		}},
	} {
		err := tc.payload.Validate()
		if err == nil {
			t.Errorf("%s: validated", tc.name)
			continue
		}
		if !errors.Is(err, chatlet.ErrInvalid) {
			t.Errorf("%s: the refusal %q is not comparable with errors.Is",
				tc.name, err)
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: the refusal %q does not name %q, and an error that "+
				"says only \"invalid\" is one somebody retries unchanged",
				tc.name, err, tc.field)
		}
	}
}

// AND EVERY PAYLOAD THIS PACKAGE MEANS TO WRITE IS ACCEPTED.
//
// The guard above is worth nothing if it simply refuses everything, so the
// shapes a real company produces are walked here — including the two whose
// zero value is a real gesture: an empty membership (emptying a room) and a
// retention of [chatlet.RetentionForever].
func TestTheShapesARealCompanyWritesAreAccepted(t *testing.T) {
	t.Parallel()
	forever := chatlet.RetentionForever
	topic := "the 2.0 release"
	for _, p := range []chatlet.Payload{
		aChannelCreate(),
		aDirectCreate(),
		func() chatlet.Payload {
			c := aChannelCreate()
			c.Kind = chatlet.KindUnit
			c.Unit = "engineering"
			c.ChartEpoch = 1_772_445_600_000
			return c
		}(),
		chatlet.ChannelPatch{V: 1, Topic: &topic},
		chatlet.RetentionSet{V: 1, RetentionDays: &forever},
		chatlet.RetentionSet{V: 1},
		chatlet.MemberSet{V: 1},
		chatlet.MemberSet{V: 1, Members: []chatlet.Member{{Handle: "sarah-chen", FollowAll: true}}},
		chatlet.MemberSet{V: 1, Members: []chatlet.Member{{Handle: "sarah-chen"}},
			ChartEpoch: 1_772_445_600_000},
		chatlet.MemberSet{V: 1, Members: []chatlet.Member{{Handle: "sarah-chen"}},
			ChartEpoch: 1_772_445_600_000, Unit: "platform"},
		chatlet.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: "shipping now",
			Mentions: []string{"amir-haddad"}, Author: "sarah-chen",
			AuthorKind: chatlet.AuthorAgent,
		},
		chatlet.MessagePost{
			V: 1, MessageID: uuid.NewString(),
			Links:  []string{"https://example.com/build/42"},
			Author: "ops", AuthorKind: chatlet.AuthorSystem,
		},
		chatlet.MessageEdit{
			V: 1, MessageID: uuid.NewString(), Body: "shipping tomorrow",
			EditedBy: "sarah-chen", EditedByKind: chatlet.AuthorHuman,
		},
		chatlet.MessageDelete{
			V: 1, MessageID: uuid.NewString(),
			DeletedBy: "sarah-chen", DeletedByKind: chatlet.AuthorHuman,
		},
		chatlet.MessageErase{
			V: 1, MessageIDs: []string{uuid.NewString(), uuid.NewString()},
			Reason: "a credential was pasted into the room",
			By:     "ops-token", ByKind: chatlet.AuthorHuman,
		},
		chatlet.Reaction{
			V: 1, MessageID: uuid.NewString(), Emoji: ":shipit:", By: "amir-haddad",
		},
		chatlet.Prune{V: 1, Cutoff: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
		chatlet.Eviction{
			V: chatlet.GateRecordVersion, NodeID: "node-b", EvictedBy: "node-a",
			EvictedAt: time.Now().UTC(),
		},
		chatlet.Generation{V: 1, Generation: 3, StreamCreatedAt: time.Now().UTC()},
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%T: a payload this package writes was refused: %v", p, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }
