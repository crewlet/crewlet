package chat_test

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

	"github.com/crewlet/crewlet/internal/chat"
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
	post, err := json.Marshal(chat.MessagePost{
		V: 1, MessageID: uuid.NewString(), Body: "shipping the release now",
		Author: "sarah-chen", AuthorKind: chat.AuthorAgent,
	})
	if err != nil {
		t.Fatalf("encode a post: %v", err)
	}
	original, err := chat.Encode(chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.RecordVersion, OpID: uuid.NewString(),
			Subject: chat.MessageSubject(room), Op: chat.OpPost,
			CreatedAt: time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC),
			Gen:       2, Writer: "node-a", Scope: chat.ScopeSet{Subject: true},
		},
		Mutation: post, Actor: "sarah-chen", ActorKind: chat.AuthorAgent,
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

	back, err := chat.Decode(original)
	if err != nil {
		t.Fatalf("decode a record this build's own version wrote: %v", err)
	}
	if len(back.Extra) != 2 {
		t.Fatalf("the decode kept %d unknown field(s), want 2 — a field it did "+
			"not collect is a field the next encode drops", len(back.Extra))
	}
	if back.Actor != "sarah-chen" || back.Op != chat.OpPost {
		t.Fatalf("the known half did not survive: %#v", back.RecordEnvelope)
	}

	again, err := chat.Encode(back)
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
	data, err := chat.Encode(chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.RecordVersion, Subject: chat.ChannelSubject(uuid.NewString()),
			Op: chat.OpPatch, Scope: chat.ScopeSet{Subject: true},
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
	future, err := chat.Encode(chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.RecordVersion + 3, OpID: uuid.NewString(),
			Subject: chat.MessageSubject(room), Op: "huddle",
			Scope: chat.ScopeSet{Subject: true}, Writer: "node-b",
		},
		Mutation: json.RawMessage(`{"shape":"nobody here knows"}`),
	})
	if err != nil {
		t.Fatalf("encode a newer build's record: %v", err)
	}

	env, err := chat.DecodeEnvelope(future)
	if err != nil {
		t.Fatalf("the envelope pass refused a newer build's record: %v — there "+
			"would then be no subject to file it under and nothing to do but "+
			"drop it", err)
	}
	if env.Subject != chat.MessageSubject(room) || env.Writer != "node-b" {
		t.Fatalf("the envelope lost what the deferral is filed under: %#v", env)
	}

	rec, err := chat.Decode(future)
	var version *chat.ErrFutureVersion
	if !errors.As(err, &version) {
		t.Fatalf("Decode returned %v, want an *ErrFutureVersion", err)
	}
	if version.Got != chat.RecordVersion+3 || version.Want != chat.RecordVersion {
		t.Errorf("the version error says got %d want %d", version.Got, version.Want)
	}
	if version.Subject != chat.MessageSubject(room) {
		t.Errorf("the version error names subject %s — a version error with no "+
			"subject is a record that can only be dropped", version.Subject)
	}
	if rec.Subject != chat.MessageSubject(room) {
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
		if _, err := chat.DecodeEnvelope([]byte(tc.payload)); err == nil {
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
		env  chat.RecordEnvelope
		want bool
	}{
		{"an eviction", chat.RecordEnvelope{
			Subject: chat.EvictionSubject("node-a"), Op: chat.OpEviction}, true},
		{"an erase", chat.RecordEnvelope{
			Subject: chat.ChannelSubject(room), Op: chat.OpErase}, true},
		{"a prune", chat.RecordEnvelope{
			Subject: chat.ChannelSubject(room), Op: chat.OpPrune}, true},
		{"a post", chat.RecordEnvelope{
			Subject: chat.MessageSubject(room), Op: chat.OpPost}, false},
		{"a tombstoning delete", chat.RecordEnvelope{
			Subject: chat.MessageSubject(room), Op: chat.OpDelete}, false},
		{"a settings patch", chat.RecordEnvelope{
			Subject: chat.ChannelSubject(room), Op: chat.OpPatch}, false},
		{"a barrier", chat.RecordEnvelope{
			Subject: chat.BarrierSubject(), Op: chat.OpBarrier}, false},
	} {
		if got := tc.env.InstallsGate(); got != tc.want {
			t.Errorf("%s: InstallsGate() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// THE WAKE VOCABULARY PUTS EVERY ADDRESSED REASON BEFORE EVERY OTHER.
//
// [chat.Reasons] is precedence order — a recipient appears once, under the
// first reason that applies — and the ordering rests on one rule a reader can
// check: an obligation to answer outranks an FYI. A vocabulary where a
// `follow_all` sat above a `mention` would file somebody's name in a room's
// noise, and nothing downstream could tell.
func TestThePrecedenceOrderPutsEveryAddressedReasonFirst(t *testing.T) {
	t.Parallel()
	if len(chat.Reasons) != 8 {
		t.Fatalf("the vocabulary has %d reasons, want 8", len(chat.Reasons))
	}
	seenUnaddressed := false
	for _, r := range chat.Reasons {
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
	want := []chat.Reason{
		chat.ReasonDM, chat.ReasonMention, chat.ReasonReplyToOwnRoot,
		chat.ReasonLeadFallback,
	}
	var got []chat.Reason
	for _, r := range chat.Reasons {
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
	if chat.ReasonReply.Addressed() {
		t.Error("a plain reply obliges an answer, which would make every busy " +
			"thread oblige every past speaker to answer again")
	}
	if !chat.ReasonReplyToOwnRoot.Addressed() {
		t.Error("a reply to the thread you started does not oblige an answer")
	}
	if chat.Reason("huddle").Valid() || chat.Reason("huddle").Addressed() {
		t.Error("a reason this build never declared is valid or addressed")
	}
	// AND NO REASON IS LISTED TWICE. A precedence walk over a slice with a
	// repeat would assign one recipient two reasons, and the second would
	// be the weaker one.
	seen := map[chat.Reason]bool{}
	for _, r := range chat.Reasons {
		if seen[r] {
			t.Errorf("the vocabulary lists %q twice", r)
		}
		seen[r] = true
	}
}

// aNotify is a valid routing snapshot the cap cases each break one field of.
func aNotify() *chat.Notify {
	return &chat.Notify{
		MessageID: uuid.NewString(), ChannelID: uuid.NewString(),
		ChannelName: "launch", ChannelKind: chat.KindPublic,
		Author: "sarah-chen", AuthorKind: chat.AuthorAgent,
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
		over  func(*chat.Notify)
	}{
		{"excerpt", func(n *chat.Notify) {
			n.Excerpt = strings.Repeat("x", chat.MaxExcerpt+1)
		}},
		{"mentions", func(n *chat.Notify) {
			n.Mentions = handlesOf(chat.MaxMentions + 1)
		}},
		{"recipients", func(n *chat.Notify) {
			for _, h := range handlesOf(chat.MaxRecipients + 1) {
				n.Recipients = append(n.Recipients,
					chat.Recipient{Handle: h, Reason: chat.ReasonFollowAll})
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
		if !errors.Is(err, chat.ErrInvalid) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: the refusal %q does not name the field", tc.field, err)
		}
	}

	for _, tc := range []struct {
		name   string
		field  string
		mangle func(*chat.Notify)
	}{
		{"no message", "message_id", func(n *chat.Notify) { n.MessageID = "" }},
		{"no room", "channel_id", func(n *chat.Notify) { n.ChannelID = "" }},
		{"an unknown channel kind", "channel_kind", func(n *chat.Notify) {
			n.ChannelKind = "huddle"
		}},
		{"an unknown author kind", "author_kind", func(n *chat.Notify) {
			n.AuthorKind = "daemon"
		}},
		{"a recipient with no handle", "recipients", func(n *chat.Notify) {
			n.Recipients = []chat.Recipient{{Reason: chat.ReasonMention}}
		}},
		{"a reason from a newer build", "recipients", func(n *chat.Notify) {
			n.Recipients = []chat.Recipient{{Handle: "amir-haddad", Reason: "huddle"}}
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
	// wakes nobody — which is what an import is.
	var quiet *chat.Notify
	if err := quiet.Validate(); err != nil {
		t.Errorf("a record that wakes nobody was refused: %v", err)
	}
	if err := aNotify().Validate(); err != nil {
		t.Errorf("an ordinary snapshot was refused: %v", err)
	}
}

// AN EXCERPT IS CUT INSIDE ITS OWN BUDGET, ON A RUNE BOUNDARY.
//
// [chat.MaxExcerpt] is a CEILING [chat.Notify.Validate] enforces, so a marker
// appended outside the budget would turn every long message into a refused
// write rather than a marked one. And a plain byte cut through a multi-byte
// rune is invalid UTF-8, which a JSON encoder substitutes and a model reads as
// a replacement character.
func TestAnExcerptIsCutInsideItsOwnBudget(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", chat.MaxExcerpt)
	cut := chat.Excerpt(long)
	if len(cut) > chat.MaxExcerpt {
		t.Fatalf("an excerpt is %d bytes against a %d ceiling the write itself "+
			"refuses", len(cut), chat.MaxExcerpt)
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
	if short := "all fine"; chat.Excerpt(short) != short {
		t.Error("a short body was cut")
	}
}

// A POST AT EVERY CAP AT ONCE STILL FITS INSIDE ONE RECORD.
//
// [chat.MaxRecordBytes] is sixteen times inside an external NATS cluster's
// 1 MiB default, and it is the number every content cap is checked against
// TOGETHER rather than one at a time: a body, eight links and a full routing
// snapshot all ride the same record, and a set of caps that each looked
// reasonable alone could still publish a record the broker refuses — at which
// point the message is one somebody typed and lost.
func TestAPostAtEveryCapFitsInsideOneRecord(t *testing.T) {
	t.Parallel()
	links := make([]string, chat.MaxLinks)
	for i := range links {
		links[i] = "https://example.com/" + strings.Repeat("u", chat.MaxLinkBytes-20)
	}
	post := chat.MessagePost{
		V: 1, MessageID: uuid.NewString(),
		Body:       strings.Repeat("x", chat.MaxBody),
		ThreadRoot: uuid.NewString(),
		Mentions:   handlesOf(chat.MaxMentions),
		Collective: true,
		Links:      links,
		Author:     "sarah-chen", AuthorKind: chat.AuthorAgent,
	}
	if err := post.Validate(); err != nil {
		t.Fatalf("a post at every cap was refused by its own caps: %v", err)
	}
	payload, err := json.Marshal(post)
	if err != nil {
		t.Fatalf("encode the post: %v", err)
	}

	notify := aNotify()
	notify.Mentions = handlesOf(chat.MaxMentions)
	notify.ThreadRoot = post.ThreadRoot
	notify.ChannelName = strings.Repeat("c", chat.MaxChannelName)
	notify.Excerpt = strings.Repeat("x", chat.MaxExcerpt)
	notify.Lead = "amir-haddad"
	notify.WakesTruncated = true
	for _, h := range handlesOf(chat.MaxRecipients) {
		notify.Recipients = append(notify.Recipients,
			chat.Recipient{Handle: h, Reason: chat.ReasonReplyToOwnRoot})
	}
	if err := notify.Validate(); err != nil {
		t.Fatalf("a snapshot at every cap was refused by its own caps: %v", err)
	}

	record, err := chat.Encode(chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.RecordVersion, OpID: uuid.NewString(),
			Subject: chat.MessageSubject(notify.ChannelID), Op: chat.OpPost,
			CreatedAt: time.Now().UTC(), Gen: 9, Writer: "node-a",
			Scope: chat.ScopeSet{Subject: true},
		},
		Mutation: payload, Actor: "sarah-chen", ActorKind: chat.AuthorAgent,
		TurnID: uuid.NewString(), Chain: handlesOf(4), Notify: notify,
	})
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	if len(record) > chat.MaxRecordBytes {
		t.Fatalf("a post at every cap encodes to %d bytes against a %d design "+
			"maximum — the caps have to be checked together, because they all "+
			"ride one record", len(record), chat.MaxRecordBytes)
	}
}

// THE BARRIER IS THE ONE RECORD THAT MAY NOT CARRY AN OP ID.
//
// An op id becomes the Nats-Msg-Id, and a repeat inside the duplicate window
// is answered from the dedupe cache with NO quorum round trip at all — so the
// sequence it returns is a position nothing confirmed, which is exactly the
// claim a barrier exists to make and the one it must never fake.
func TestABarrierRefusesAnOpID(t *testing.T) {
	t.Parallel()
	if _, err := chat.EncodeBarrier(statelogBarrier("op-1")); err == nil {
		t.Fatal("a barrier carrying an op id was encoded")
	}
	data, err := chat.EncodeBarrier(statelogBarrier(""))
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	rec, err := chat.Decode(data)
	if err != nil {
		t.Fatalf("decode a barrier: %v", err)
	}
	if rec.Subject != chat.BarrierSubject() || rec.Op != chat.OpBarrier {
		t.Fatalf("a barrier decoded as %#v", rec.RecordEnvelope)
	}
	payload, err := chat.DecodeMutation(rec)
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
	want := map[chat.OpKind]chat.ObjectKind{
		chat.OpPatch:      chat.KindChannel,
		chat.OpMembers:    chat.KindChannel,
		chat.OpErase:      chat.KindChannel,
		chat.OpPrune:      chat.KindChannel,
		chat.OpPost:       chat.KindMessage,
		chat.OpEdit:       chat.KindMessage,
		chat.OpDelete:     chat.KindMessage,
		chat.OpReact:      chat.KindMessage,
		chat.OpEviction:   chat.KindEviction,
		chat.OpGeneration: chat.KindGeneration,
		chat.OpBarrier:    chat.KindBarrier,
	}
	for _, op := range chat.OpKinds {
		if !op.Valid() {
			t.Errorf("declared op %q reports itself invalid", op)
		}
		if op == chat.OpCreate {
			continue
		}
		got, ok := chat.SubjectKind(op, chat.KindPublic)
		if !ok {
			t.Errorf("op %q rides no subject, so nothing can publish it", op)
			continue
		}
		if got != want[op] {
			t.Errorf("op %q rides %q, want %q", op, got, want[op])
		}
	}
	if len(want)+1 != len(chat.OpKinds) {
		t.Errorf("the domain declares %d ops and this suite places %d plus the "+
			"create", len(chat.OpKinds), len(want))
	}

	// A NAMED ROOM'S CREATE ARBITRATES ON ITS NAME, so two people typing
	// `#launch` contend at the broker. A DIRECT one has no name to contend
	// for, so it arbitrates on its own derived id, create-only.
	for _, k := range []chat.Kind{chat.KindPublic, chat.KindPrivate, chat.KindUnit} {
		if got, _ := chat.SubjectKind(chat.OpCreate, k); got != chat.KindChannelName {
			t.Errorf("a %s room's create rides %q, want %q — two rooms with one "+
				"name is not a state this alphabet can express",
				k, got, chat.KindChannelName)
		}
	}
	for _, k := range []chat.Kind{chat.KindDM, chat.KindGroup} {
		if got, _ := chat.SubjectKind(chat.OpCreate, k); got != chat.KindChannel {
			t.Errorf("a %s conversation's create rides %q, want %q",
				k, got, chat.KindChannel)
		}
	}
	if _, ok := chat.SubjectKind(chat.OpCreate, "huddle"); ok {
		t.Error("a create for an unknown channel kind was given a subject — the " +
			"two answers are different arbitration disciplines, and guessing " +
			"the wrong one for a named room is two rooms with one name")
	}
	if _, ok := chat.SubjectKind("huddle", chat.KindPublic); ok {
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
		subject chat.Subject
		op      chat.OpKind
		payload any
	}{
		{chat.ChannelNameSubject("launch"), chat.OpCreate, aChannelCreate()},
		{chat.ChannelSubject(room), chat.OpCreate, aChannelCreate()},
		{chat.ChannelSubject(room), chat.OpPatch, chat.ChannelPatch{V: 1}},
		{chat.ChannelSubject(room), chat.OpMembers, chat.MemberSet{V: 1}},
		{chat.ChannelSubject(room), chat.OpErase, chat.MessageErase{V: 1}},
		{chat.ChannelSubject(room), chat.OpPrune, chat.Prune{V: 1}},
		{chat.MessageSubject(room), chat.OpPost, chat.MessagePost{V: 1}},
		{chat.MessageSubject(room), chat.OpEdit, chat.MessageEdit{V: 1}},
		{chat.MessageSubject(room), chat.OpDelete, chat.MessageDelete{V: 1}},
		{chat.MessageSubject(room), chat.OpReact, chat.Reaction{V: 1}},
		{chat.EvictionSubject("node-a"), chat.OpEviction, chat.Eviction{V: 1}},
		{chat.GenerationSubject(3), chat.OpGeneration, chat.Generation{V: 1}},
	} {
		body, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatalf("encode a %s payload: %v", tc.op, err)
		}
		got, err := chat.DecodeMutation(chat.MutationRecord{
			RecordEnvelope: chat.RecordEnvelope{
				V: chat.RecordVersion, Subject: tc.subject, Op: tc.op,
				Scope: chat.ScopeSet{Subject: true},
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
		subject chat.Subject
		op      chat.OpKind
	}{
		{chat.MessageSubject(room), chat.OpPatch},
		{chat.ChannelSubject(room), chat.OpPost},
		{chat.ChannelNameSubject("launch"), chat.OpPatch},
		{chat.Subject{Kind: "huddle", ID: room}, chat.OpPost},
		{chat.MessageSubject(room), "huddle"},
	} {
		_, err := chat.DecodeMutation(chat.MutationRecord{
			RecordEnvelope: chat.RecordEnvelope{
				V: chat.RecordVersion, Subject: tc.subject, Op: tc.op,
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
	if _, err := chat.DecodeMutation(chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.RecordVersion, Subject: chat.MessageSubject(room),
			Op: chat.OpPost,
		},
	}); err == nil {
		t.Error("a post with no payload decoded to an empty message")
	}
}

// aChannelCreate is a valid named-room create the cases below break one field
// of at a time.
func aChannelCreate() chat.ChannelCreate {
	return chat.ChannelCreate{
		V: 1, ChannelID: uuid.NewString(), Kind: chat.KindPublic,
		Name: "launch", Topic: "the 2.0 release",
		Members:   []chat.Member{{Handle: "sarah-chen"}, {Handle: "amir-haddad", FollowAll: true}},
		CreatedBy: "sarah-chen", CreatedByKind: chat.AuthorHuman,
	}
}

// aDirectCreate is a valid direct conversation, id and all.
func aDirectCreate() chat.ChannelCreate {
	members := []chat.Member{{Handle: "sarah-chen"}, {Handle: "amir-haddad"}}
	return chat.ChannelCreate{
		V: 1, Kind: chat.KindDM, Members: members,
		ChannelID: chat.DirectChannelID(
			[]string{"sarah-chen", "amir-haddad"}).String(),
		CreatedBy: "sarah-chen", CreatedByKind: chat.AuthorHuman,
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
	crowd := handlesOf(chat.MaxMembers + 1)
	tooMany := make([]chat.Member, len(crowd))
	for i, h := range crowd {
		tooMany[i] = chat.Member{Handle: h}
	}
	erased := make([]string, chat.MaxEraseMessages+1)
	for i := range erased {
		erased[i] = uuid.NewString()
	}

	for _, tc := range []struct {
		name    string
		field   string
		payload chat.Payload
	}{
		{"a create with no id", "channel_id", func() chat.Payload {
			c := aChannelCreate()
			c.ChannelID = ""
			return c
		}()},
		{"a create with a kind from a newer build", "kind", func() chat.Payload {
			c := aChannelCreate()
			c.Kind = "huddle"
			return c
		}()},
		{"a named room with an illegal address", "name", func() chat.Payload {
			c := aChannelCreate()
			c.Name = "The Launch Room"
			return c
		}()},
		{"a unit room naming no unit", "unit", func() chat.Payload {
			c := aChannelCreate()
			c.Kind = chat.KindUnit
			return c
		}()},
		{"a public room naming a unit", "unit", func() chat.Payload {
			c := aChannelCreate()
			c.Unit = "engineering"
			return c
		}()},
		{"a direct conversation with a name", "name", func() chat.Payload {
			c := aDirectCreate()
			c.Name = "sarah-and-amir"
			return c
		}()},
		{"a direct conversation claiming an id it did not derive", "channel_id",
			func() chat.Payload {
				c := aDirectCreate()
				c.ChannelID = uuid.NewString()
				return c
			}()},
		{"a direct conversation of one", "members", func() chat.Payload {
			c := aDirectCreate()
			c.Members = []chat.Member{{Handle: "sarah-chen"}}
			c.ChannelID = chat.DirectChannelID([]string{"sarah-chen"}).String()
			return c
		}()},
		{"a direct conversation over the participant cap", "members",
			func() chat.Payload {
				handles := handlesOf(chat.MaxDMParticipants + 1)
				members := make([]chat.Member, len(handles))
				for i, h := range handles {
					members[i] = chat.Member{Handle: h}
				}
				c := aDirectCreate()
				c.Members = members
				c.ChannelID = chat.DirectChannelID(handles).String()
				return c
			}()},
		{"a create over the topic cap", "topic", func() chat.Payload {
			c := aChannelCreate()
			c.Topic = strings.Repeat("t", chat.MaxTopic+1)
			return c
		}()},
		{"a create over the purpose cap", "purpose", func() chat.Payload {
			c := aChannelCreate()
			c.Purpose = strings.Repeat("p", chat.MaxPurpose+1)
			return c
		}()},
		{"a create with negative retention", "retention_days", func() chat.Payload {
			c := aChannelCreate()
			days := -1
			c.RetentionDays = &days
			return c
		}()},
		{"a create with no author", "created_by", func() chat.Payload {
			c := aChannelCreate()
			c.CreatedBy = ""
			return c
		}()},
		{"a patch that sets nothing", "patch", chat.ChannelPatch{V: 1}},
		{"a membership over the cap", "members", chat.MemberSet{V: 1, Members: tooMany}},
		{"a membership naming somebody twice", "members", chat.MemberSet{
			V: 1, Members: []chat.Member{{Handle: "sarah-chen"}, {Handle: "sarah-chen"}},
		}},
		{"a post with neither text nor a link", "body", chat.MessagePost{
			V: 1, MessageID: uuid.NewString(), Author: "sarah-chen",
			AuthorKind: chat.AuthorAgent,
		}},
		{"a post over the body cap", "body", chat.MessagePost{
			V: 1, MessageID: uuid.NewString(),
			Body:   strings.Repeat("x", chat.MaxBody+1),
			Author: "sarah-chen", AuthorKind: chat.AuthorAgent,
		}},
		{"a post over the mention cap", "mentions", chat.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: "hi",
			Mentions: handlesOf(chat.MaxMentions + 1),
			Author:   "sarah-chen", AuthorKind: chat.AuthorAgent,
		}},
		{"a post over the link cap", "links", chat.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: "hi",
			Links:  make([]string, chat.MaxLinks+1),
			Author: "sarah-chen", AuthorKind: chat.AuthorAgent,
		}},
		{"a post with a link over its own cap", "links", chat.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: "hi",
			Links:  []string{strings.Repeat("u", chat.MaxLinkBytes+1)},
			Author: "sarah-chen", AuthorKind: chat.AuthorAgent,
		}},
		{"an import with no provenance", "imported.vendor_id", chat.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: "hi",
			Author: "sarah-chen", AuthorKind: chat.AuthorAgent,
			Imported: &chat.Imported{
				Source: "slack", Author: "sarah-chen",
				AuthorKind: chat.AuthorHuman, AuthoredAt: time.Now().UTC(),
			},
		}},
		{"an import with no authored instant", "imported.authored_at",
			chat.MessagePost{
				V: 1, MessageID: uuid.NewString(), Body: "hi",
				Author: "sarah-chen", AuthorKind: chat.AuthorAgent,
				Imported: &chat.Imported{
					Source: "slack", VendorID: "1699999999.000100",
					Author: "sarah-chen", AuthorKind: chat.AuthorHuman,
				},
			}},
		{"an edit that empties the message", "body", chat.MessageEdit{
			V: 1, MessageID: uuid.NewString(),
			EditedBy: "sarah-chen", EditedByKind: chat.AuthorHuman,
		}},
		{"a deletion naming nobody", "deleted_by", chat.MessageDelete{
			V: 1, MessageID: uuid.NewString(), DeletedByKind: chat.AuthorHuman,
		}},
		{"an erase over the cap", "message_ids", chat.MessageErase{
			V: 1, ChannelID: uuid.NewString(), MessageIDs: erased,
			Count: len(erased), By: "ops", ByKind: chat.AuthorOperator,
		}},
		{"an erase whose count disagrees with its list", "count", chat.MessageErase{
			V: 1, ChannelID: uuid.NewString(), MessageIDs: erased[:3], Count: 9,
			By: "ops", ByKind: chat.AuthorOperator,
		}},
		{"an erase written by an agent", "by_kind", chat.MessageErase{
			V: 1, ChannelID: uuid.NewString(), MessageIDs: erased[:2], Count: 2,
			By: "sarah-chen", ByKind: chat.AuthorAgent,
		}},
		{"a reaction with no emoji", "emoji", chat.Reaction{
			V: 1, MessageID: uuid.NewString(), By: "sarah-chen",
		}},
		{"a reaction over the emoji cap", "emoji", chat.Reaction{
			V: 1, MessageID: uuid.NewString(), By: "sarah-chen",
			Emoji: strings.Repeat("e", chat.MaxEmoji+1),
		}},
		{"a prune with no cutoff", "cutoff", chat.Prune{V: 1}},
		{"an eviction naming no node", "node_id", chat.Eviction{
			V: chat.GateRecordVersion, EvictedAt: time.Now().UTC(),
		}},
		{"an eviction at a version a gate may never take", "v", chat.Eviction{
			V: chat.GateRecordVersion + 1, NodeID: "node-a",
			EvictedAt: time.Now().UTC(),
		}},
		{"a reanchor claiming generation zero", "generation", chat.Generation{
			V: 1, StreamCreatedAt: time.Now().UTC(),
		}},
	} {
		err := tc.payload.Validate()
		if err == nil {
			t.Errorf("%s: validated", tc.name)
			continue
		}
		if !errors.Is(err, chat.ErrInvalid) {
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
// retention of [chat.RetentionForever].
func TestTheShapesARealCompanyWritesAreAccepted(t *testing.T) {
	t.Parallel()
	forever := chat.RetentionForever
	topic := "the 2.0 release"
	for _, p := range []chat.Payload{
		aChannelCreate(),
		aDirectCreate(),
		func() chat.Payload {
			c := aChannelCreate()
			c.Kind = chat.KindUnit
			c.Unit = "engineering"
			c.RetentionDays = &forever
			return c
		}(),
		chat.ChannelPatch{V: 1, Topic: &topic},
		chat.ChannelPatch{V: 1, RetentionDays: &forever},
		chat.MemberSet{V: 1},
		chat.MemberSet{V: 1, Members: []chat.Member{{Handle: "sarah-chen", FollowAll: true}}},
		chat.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: "shipping now",
			Mentions: []string{"amir-haddad"}, Author: "sarah-chen",
			AuthorKind: chat.AuthorAgent,
		},
		chat.MessagePost{
			V: 1, MessageID: uuid.NewString(),
			Links:  []string{"https://example.com/build/42"},
			Author: "ops", AuthorKind: chat.AuthorSystem,
		},
		chat.MessagePost{
			V: 1, MessageID: uuid.NewString(), Body: "we shipped it last year",
			Author: "sarah-chen", AuthorKind: chat.AuthorHuman,
			Imported: &chat.Imported{
				Source: "slack", VendorID: "1699999999.000100",
				Author: "sarah-chen", AuthorKind: chat.AuthorHuman,
				AuthoredAt: time.Date(2030, 11, 14, 9, 30, 0, 0, time.UTC),
			},
		},
		chat.MessageEdit{
			V: 1, MessageID: uuid.NewString(), Body: "shipping tomorrow",
			EditedBy: "sarah-chen", EditedByKind: chat.AuthorHuman,
		},
		chat.MessageDelete{
			V: 1, MessageID: uuid.NewString(),
			DeletedBy: "sarah-chen", DeletedByKind: chat.AuthorHuman,
		},
		chat.MessageErase{
			V: 1, ChannelID: uuid.NewString(),
			MessageIDs: []string{uuid.NewString(), uuid.NewString()}, Count: 2,
			Reason: "a credential was pasted into the room",
			By:     "ops-token", ByKind: chat.AuthorOperator,
		},
		chat.Reaction{
			V: 1, MessageID: uuid.NewString(), Emoji: ":shipit:", By: "amir-haddad",
		},
		chat.Prune{V: 1, Cutoff: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
		chat.Eviction{
			V: chat.GateRecordVersion, NodeID: "node-b", EvictedBy: "node-a",
			EvictedAt: time.Now().UTC(),
		},
		chat.Generation{V: 1, Generation: 3, StreamCreatedAt: time.Now().UTC()},
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%T: a payload this package writes was refused: %v", p, err)
		}
	}
}

// AN IMPORT WAKES NOBODY, AND THE SHAPE IS WHAT SAYS SO.
//
// A year of somebody's Slack replayed onto the log is a year of posts that
// already happened. A wake per imported message would page the whole company
// about conversations it has already had, at once — so an import carries its
// provenance and NO routing snapshot, and the wake filter asks about the
// record's own shape rather than about a flag a writer sets and a version-gated
// decode would have to read.
func TestAnImportedMessageCarriesNoRoutingSnapshot(t *testing.T) {
	t.Parallel()
	post := chat.MessagePost{
		V: 1, MessageID: uuid.NewString(), Body: "we shipped it last year",
		Author: "sarah-chen", AuthorKind: chat.AuthorHuman,
		Imported: &chat.Imported{
			Source: "slack", VendorID: "1699999999.000100",
			Author: "sarah-chen", AuthorKind: chat.AuthorHuman,
			AuthoredAt: time.Date(2030, 11, 14, 9, 30, 0, 0, time.UTC),
		},
	}
	if err := post.Validate(); err != nil {
		t.Fatalf("an imported post was refused: %v", err)
	}
	body, err := json.Marshal(post)
	if err != nil {
		t.Fatalf("encode the post: %v", err)
	}
	data, err := chat.Encode(chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.RecordVersion, OpID: uuid.NewString(),
			Subject: chat.MessageSubject(uuid.NewString()), Op: chat.OpPost,
			Scope: chat.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "import", ActorKind: chat.AuthorOperator,
	})
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	rec, err := chat.Decode(data)
	if err != nil {
		t.Fatalf("decode the record: %v", err)
	}
	if rec.Notify != nil {
		t.Fatal("an imported record carries a routing snapshot, so a migration " +
			"would page the whole company about a year of old conversations")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("read the encoding back: %v", err)
	}
	if _, present := fields["notify"]; present {
		t.Error("a quiet record still writes a notify key, so the wake filter " +
			"would have to look inside it rather than at the record's shape")
	}
}
