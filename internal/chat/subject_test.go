package chat_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// classification is what every closed set on [chat.ObjectKind] must answer for
// one kind.
type classification struct {
	arbitrated     bool
	installsGate   bool
	routable       bool
	recordsHistory bool
}

// theClassification is the expected answer for every kind, WRITTEN OUT rather
// than derived, because a table derived from the methods it checks agrees with
// itself whatever the methods say.
var theClassification = map[chat.ObjectKind]classification{
	chat.KindChannelName: {arbitrated: true, recordsHistory: true},
	chat.KindChannel:     {arbitrated: true, routable: true, recordsHistory: true},
	chat.KindMessage:     {routable: true, recordsHistory: true},
	chat.KindEviction:    {installsGate: true},
	chat.KindGeneration:  {},
	chat.KindBarrier:     {},
}

// EVERY KIND IS CLASSIFIED BY EVERY CLOSED SET, AND A KIND ADDED LATER CANNOT
// SLIP THROUGH UNCLASSIFIED.
//
// Four readers that cannot see each other compare against these sets: the
// stream declares which kinds carry an anchor, the applier turns an unknown
// version into a stop or a deferral, the wake feed decides whether anybody is
// told, and the history row's kind is what every filter selects on. A kind
// added to three of the four publishes, is delivered, wakes nobody and writes
// nothing — which looks exactly like a quiet company.
//
// THE WALK IS OVER [chat.ObjectKinds] AND OVER THE TABLE BOTH WAYS, so a
// seventh kind is a failure here before it is a silence in production.
func TestEveryObjectKindIsClassifiedByEveryClosedSet(t *testing.T) {
	t.Parallel()
	for _, k := range chat.ObjectKinds {
		want, named := theClassification[k]
		if !named {
			t.Fatalf("kind %q is declared and this suite does not classify it "+
				"— every closed set has an answer for every kind, and a kind "+
				"nobody classified is one that arbitrates nothing, gates "+
				"nothing, wakes nobody and files no history", k)
		}
		if got := k.Arbitrated(); got != want.arbitrated {
			t.Errorf("%s.Arbitrated() = %v, want %v", k, got, want.arbitrated)
		}
		if got := k.InstallsGate(); got != want.installsGate {
			t.Errorf("%s.InstallsGate() = %v, want %v", k, got, want.installsGate)
		}
		if got := k.Routable(); got != want.routable {
			t.Errorf("%s.Routable() = %v, want %v", k, got, want.routable)
		}
		if got := k.RecordsHistory(); got != want.recordsHistory {
			t.Errorf("%s.RecordsHistory() = %v, want %v",
				k, got, want.recordsHistory)
		}
		if !k.Valid() {
			t.Errorf("%s is declared and reports itself invalid", k)
		}
	}
	for k := range theClassification {
		if !slices.Contains(chat.ObjectKinds, k) {
			t.Errorf("this suite classifies %q and the domain does not declare "+
				"it — a kind nothing publishes is a case the applier can never "+
				"reach", k)
		}
	}
}

// AN UNKNOWN KIND IS A VALUE, NOT A PANIC, AND IT IS CLASSIFIED INTO NOTHING.
//
// A newer peer publishes a kind this build has never heard of, and the literal
// is RETAINED so the deferral can be filed under it. What it must never do is
// fall into a closed set by default: an unknown kind that reported itself
// routable would wake the company for a record nobody can render, and one that
// reported itself arbitrated would write an anchor for a subject nothing
// reads.
func TestAnUnknownObjectKindFallsIntoNoClosedSet(t *testing.T) {
	t.Parallel()
	future := chat.ObjectKind("huddle")
	if future.Valid() {
		t.Fatal("a kind this build never declared reports itself valid")
	}
	for _, c := range []struct {
		name string
		got  bool
	}{
		{"Arbitrated", future.Arbitrated()},
		{"InstallsGate", future.InstallsGate()},
		{"Routable", future.Routable()},
		{"RecordsHistory", future.RecordsHistory()},
	} {
		if c.got {
			t.Errorf("an unknown kind reports %s() true — every one of these is "+
				"a closed set precisely so a kind nobody classified is treated "+
				"as machinery rather than as whatever the default happened to "+
				"be", c.name)
		}
	}
}

// THE FIRST THREE KINDS ARE A REAL SEQUENCE, IN THAT ORDER.
//
// statelogtest publishes the first three a domain declares, twice each, in
// declaration order — so the order of [chat.ObjectKinds] decides what the
// framework's own certification suite exercises. A name claim, then the room
// it claimed for, then a message in that room is a sequence a strict,
// identity-claiming replay accepts. Putting the message first would publish a
// message into a room no create wrote, and the suite would certify a failure.
func TestTheFirstThreeKindsFormAValidPublishSequence(t *testing.T) {
	t.Parallel()
	want := []chat.ObjectKind{chat.KindChannelName, chat.KindChannel, chat.KindMessage}
	if got := chat.ObjectKinds[:3]; !slices.Equal(got, want) {
		t.Fatalf("the first three declared kinds are %v, want %v — the order is "+
			"what the framework's suite publishes, and these three have to form "+
			"a sequence a strict replay accepts", got, want)
	}
}

// A NAME AND ITS TOKEN AGREE, WHATEVER THE AUTHOR TYPED.
//
// The name is an ADDRESS, so `#Launch` and `#launch ` must arbitrate on ONE
// subject. Normalising at the caller instead is how two rooms end up with one
// name: the caller that forgets claims a second address nobody can resolve.
func TestAChannelNameAndItsTokenAgreeAcrossSpelling(t *testing.T) {
	t.Parallel()
	canonical := chat.ChannelToken("launch")
	for _, typed := range []string{"launch", "Launch", "  LAUNCH  ", "lAuNcH"} {
		if got := chat.ChannelToken(typed); got != canonical {
			t.Errorf("ChannelToken(%q) = %s, want %s — one address, one subject",
				typed, got, canonical)
		}
		if got := chat.ChannelNameSubject(typed).ID; got != canonical {
			t.Errorf("ChannelNameSubject(%q).ID = %s, want %s — the subject "+
				"normalises once so a caller cannot arbitrate on its own "+
				"capitalisation", typed, got, canonical)
		}
	}
	if other := chat.ChannelToken("launches"); other == canonical {
		t.Fatal("two different names share a token, so two rooms would contend " +
			"for one address")
	}
	if len(canonical) != 32 {
		t.Errorf("a channel token is %d hex characters, want 32 — the width is "+
			"what keeps the broker's per-subject index bounded", len(canonical))
	}
}

// PROSE IS NOT A BROKER PATH, AND THE SUBJECT REFUSES IT.
//
// A subject is a path the broker parses: a dot separates the kind from the id,
// and `*` and `>` are wildcards. A value carrying one of them is not a subject
// that addresses the wrong object — it is a subject PATTERN, which a publish
// cannot use and a consumer would match far more of the log than the writer
// meant.
func TestASubjectRefusesWhatABrokerPathCannotCarry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		subject chat.Subject
	}{
		{"no kind", chat.Subject{ID: "room-1"}},
		{"kind with a separator", chat.Subject{Kind: "chan.nel", ID: "room-1"}},
		{"kind with a wildcard", chat.Subject{Kind: "chan>", ID: "room-1"}},
		{"no id", chat.Subject{Kind: chat.KindChannel}},
		{"id with a space", chat.Subject{Kind: chat.KindChannel, ID: "room one"}},
		{"id with a separator", chat.Subject{Kind: chat.KindChannel, ID: "a.b"}},
		{"id with a wildcard", chat.Subject{Kind: chat.KindMessage, ID: "room*"}},
		{"id with a full wildcard", chat.Subject{Kind: chat.KindMessage, ID: "room>"}},
		{"id with a newline", chat.Subject{Kind: chat.KindMessage, ID: "room\n1"}},
	} {
		if err := tc.subject.Validate(); err == nil {
			t.Errorf("%s: %#v validated, and it is not a subject a broker can "+
				"carry", tc.name, tc.subject)
		}
	}
	// AND THE ONES THAT ARE LEGAL STAY LEGAL, so the guard above is not
	// simply refusing everything.
	for _, s := range []chat.Subject{
		chat.ChannelNameSubject("launch"),
		chat.ChannelSubject(uuid.NewString()),
		chat.MessageSubject(uuid.NewString()),
		chat.EvictionSubject("node-a"),
		chat.GenerationSubject(7),
		chat.BarrierSubject(),
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("%s: a subject this package mints was refused: %v", s, err)
		}
	}
}

// A NAME IS A SHAPE, AND THE SHAPE IS BOUNDED WHERE IT IS CHECKED.
//
// [chat.ValidName] and [chat.MaxChannelName] are one rule: the pattern's own
// bound is what makes the constant true, so a name that passes the pattern can
// never exceed the cap and a cap that moved without the pattern would be a
// number nothing enforces.
func TestAChannelNameIsBoundedByTheShapeThatChecksIt(t *testing.T) {
	t.Parallel()
	atCap := strings.Repeat("a", chat.MaxChannelName)
	if !chat.ValidName(atCap) {
		t.Fatalf("a %d-byte name was refused, and %d is the cap",
			len(atCap), chat.MaxChannelName)
	}
	if chat.ValidName(atCap + "a") {
		t.Fatalf("a %d-byte name was accepted against a %d cap — the pattern's "+
			"own bound is what enforces the constant", len(atCap)+1,
			chat.MaxChannelName)
	}
	for _, bad := range []string{
		"", " ", "Launch", "launch room", "launch.room", "launch/room",
		"-launch", "launch_room", "launch>", "läunch",
	} {
		if chat.ValidName(bad) {
			t.Errorf("ValidName(%q) is true — a name is a claim key, half of a "+
				"scope path and the input to a broker subject token, so a "+
				"character legal in one of those and not the others is exactly "+
				"what this refuses", bad)
		}
	}
	for _, good := range []string{"launch", "launch-room", "a", "0", "eng-2026"} {
		if !chat.ValidName(good) {
			t.Errorf("ValidName(%q) is false, and it is a legal address", good)
		}
	}
	if got := chat.NormalizeName("  LAUNCH-Room "); got != "launch-room" {
		t.Errorf("NormalizeName = %q, want %q", got, "launch-room")
	}
}

// TWO SEATS OPENING ONE CONVERSATION FROM EITHER SIDE DERIVE ONE ROOM.
//
// This is the whole reason a direct conversation's id is derived rather than
// minted. Nobody creates a DM: it comes into existence because somebody spoke,
// and the other side may speak at the same moment on another node. Two minted
// ids are two rooms with the same two people in them, each holding half the
// conversation, and no arbitration afterwards can merge them — neither writer
// was deciding against anything the other wrote.
func TestTwoSidesOfOneDirectConversationDeriveOneID(t *testing.T) {
	t.Parallel()
	sarah, amir := "sarah-chen", "amir-haddad"
	from := chat.DirectChannelID([]string{sarah, amir})
	to := chat.DirectChannelID([]string{amir, sarah})
	if from != to {
		t.Fatalf("one conversation derived two ids, %s and %s — the order the "+
			"two sides hold their participants in is not a fact about the room",
			from, to)
	}
	if from == uuid.Nil {
		t.Fatal("a conversation between two named people derived the nil id")
	}
	// NORMALISED AND DEDUPLICATED, because the two sides do not hold their
	// handles in one spelling either, and a caller that lists itself twice
	// must not derive a third room.
	for _, same := range [][]string{
		{"  Sarah-Chen ", "AMIR-HADDAD"},
		{amir, sarah, amir},
		{sarah, "", amir},
	} {
		if got := chat.DirectChannelID(same); got != from {
			t.Errorf("DirectChannelID(%q) = %s, want %s", same, got, from)
		}
	}
	// A DIFFERENT SET IS A DIFFERENT ROOM, including one that merely adds
	// somebody: a group conversation is not the pair's conversation with a
	// third person watching.
	if third := chat.DirectChannelID([]string{sarah, amir, "dana-ruiz"}); third == from {
		t.Fatal("adding a participant derived the same room, so a group " +
			"conversation would land in the pair's own history")
	}
	// AND THE SEPARATOR CANNOT BE FORGED. A handle may not contain NUL, so
	// there is no pair of distinct participant sets that join to one
	// string — the test that a comma or a space could not pass.
	ab := chat.DirectChannelID([]string{"ab", "c"})
	a := chat.DirectChannelID([]string{"a", "bc"})
	if ab == a {
		t.Fatal("two different participant sets derived one room, which is a " +
			"collision between two conversations")
	}
	// AN EMPTY SET IS THE NIL ID rather than the digest of nothing, so an
	// unnameable conversation cannot silently acquire the id every other
	// unnameable conversation would also derive.
	for _, empty := range [][]string{nil, {}, {""}, {"  "}} {
		if got := chat.DirectChannelID(empty); got != uuid.Nil {
			t.Errorf("DirectChannelID(%q) = %s, want the nil id", empty, got)
		}
	}
}

// A SUBJECT ROUND-TRIPS THROUGH THE WIRE GRAMMAR IT IS PUBLISHED ON.
//
// The publisher builds the subject here, the feed's filter matches the string,
// and the applier parses it back — three readers that cannot see each other.
// A composition that did not invert is a record delivered to a consumer whose
// kind switch has no case for what it recovers.
func TestASubjectRoundTripsThroughTheWireGrammar(t *testing.T) {
	t.Parallel()
	room := uuid.NewString()
	for _, want := range []chat.Subject{
		chat.ChannelNameSubject("launch"),
		chat.ChannelSubject(room),
		chat.MessageSubject(room),
		chat.EvictionSubject("node-a"),
		chat.GenerationSubject(3),
		chat.BarrierSubject(),
	} {
		wire := want.Wire()
		if !strings.HasPrefix(wire, topics.ChatLogPrefix+".") {
			t.Errorf("%s publishes to %q, which is outside the log's own "+
				"wildcard", want, wire)
		}
		got, ok := chat.ParseSubject(wire)
		if !ok {
			t.Fatalf("%q is not recoverable as a subject on this log", wire)
		}
		if got != want {
			t.Errorf("%q parsed back as %#v, want %#v", wire, got, want)
		}
	}
	if _, ok := chat.ParseSubject("crewlet.pages.log.page.7"); ok {
		t.Error("a subject on another domain's log parsed as a chat subject")
	}
}

// A MESSAGE SUBJECT IS THE ROOM'S, NOT THE MESSAGE'S.
//
// Every post, edit, deletion and reaction in one room lands on ONE subject,
// which is what gives the log a total order per room for the applier to mint a
// per-channel sequence from, and what keeps the broker's per-subject index
// bounded by the number of rooms rather than by the number of things anybody
// ever said.
func TestEveryMessageInARoomSharesOneSubject(t *testing.T) {
	t.Parallel()
	room := uuid.NewString()
	first := chat.MessageSubject(room)
	second := chat.MessageSubject(room)
	if first != second {
		t.Fatalf("two messages in one room took two subjects, %s and %s — the "+
			"applier mints its per-channel sequence from the order of one",
			first, second)
	}
	if first.ID != room {
		t.Errorf("a message subject's id is %q, want the room's id %q",
			first.ID, room)
	}
	if elsewhere := chat.MessageSubject(uuid.NewString()); elsewhere == first {
		t.Fatal("two rooms shared a message subject, so talk in one would " +
			"contend with talk in the other")
	}
}
