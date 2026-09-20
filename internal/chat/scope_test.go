package chat_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/google/uuid"
)

// onePath is the single path a scope must resolve to, or the test fails
// naming what it got instead.
func onePath(t *testing.T, s statelog.ScopeSet) string {
	t.Helper()
	if len(s.Paths) != 1 {
		t.Fatalf("a scope resolved to %d paths, want exactly one: %v",
			len(s.Paths), s.Paths)
	}
	return s.Paths[0]
}

// A ROOM'S PATH COVERS EVERY MESSAGE WRITTEN IN IT, AND THERE IS NO PATH FOR A
// MESSAGE ALONE.
//
// This is the package's central correctness property expressed in the
// alphabet. The applier mints a per-channel sequence from LOG ORDER, so a node
// holding a record it cannot decode must stop writing rows for that ROOM — not
// for that message. Scoped to itself, a deferred post would let the next post
// in the room take the sequence the deferred one should have had, and that
// node's rows would disagree with every other node's for ever.
//
// The containment is the framework's own [statelog.Covers], because the
// framework is what actually runs the probe.
func TestAChannelScopeCoversEveryMessageWrittenInIt(t *testing.T) {
	t.Parallel()
	room := uuid.NewString()
	channel := onePath(t, chat.ScopeSet{Subject: true}.Resolve(chat.ChannelSubject(room)))
	message := onePath(t, chat.ScopeSet{Subject: true}.Resolve(chat.MessageSubject(room)))

	if !statelog.Covers(channel, message) {
		t.Fatalf("the room's path %q does not cover a message's %q — a "+
			"deferral on the room would let the next post in it take the "+
			"sequence the deferred one should have had", channel, message)
	}
	// AND IT IS THE SAME PATH, not a nested one: the message subject's id
	// IS the channel id, so there is nothing derived and nothing that can
	// drift between the two.
	if channel != message {
		t.Fatalf("a message resolved to %q and its room to %q — the two are "+
			"one object in this alphabet, and a message-shaped path is what "+
			"the domain deliberately does not have", message, channel)
	}
	// A SIBLING ROOM IS NOT COVERED, which is the other half of the
	// predicate: a deferral on one room must not stop the whole company
	// talking.
	elsewhere := onePath(t, chat.ScopeSet{Subject: true}.
		Resolve(chat.MessageSubject(uuid.NewString())))
	if statelog.Covers(channel, elsewhere) || statelog.Covers(elsewhere, channel) {
		t.Fatalf("two rooms cover each other, %q and %q — a record deferred in "+
			"one would block every write in the other", channel, elsewhere)
	}
}

// NO TERM IN THE ALPHABET NAMES A MESSAGE.
//
// The absence is the property, so it is asserted rather than assumed: every
// declared term kind resolves to the domain, a room or an address, and a
// message id handed to any of them either names a room (which is what a
// message subject's id IS) or names nothing.
func TestTheScopeAlphabetHasNoPerMessageTerm(t *testing.T) {
	t.Parallel()
	want := map[chat.TermKind]bool{
		chat.TermChannel: true, chat.TermName: true, chat.TermDomain: true,
	}
	for _, k := range chat.TermKinds {
		if !want[k] {
			t.Errorf("term kind %q is declared and this suite does not know it "+
				"— a term naming a message is the one shape this alphabet may "+
				"not grow, because it would let a deferred post be stepped over", k)
		}
		if !k.Valid() {
			t.Errorf("declared term kind %q reports itself invalid", k)
		}
	}
	if len(chat.TermKinds) != len(want) {
		t.Errorf("the domain declares %d term kinds and this suite classifies "+
			"%d", len(chat.TermKinds), len(want))
	}
}

// THE THREE PATHS NEST ON THE SEPARATOR, DOMAIN OVER EVERYTHING.
//
// The framework knows exactly one thing about a path — that it is a hierarchy
// written left to right with [statelog.ScopeSeparator] — so a term that did
// not nest would be a deferral nobody's probe reaches. The domain term is what
// a gate and an unreadable scope both resolve to, so it must cover both of the
// others.
func TestTheScopePathsNestUnderTheDomain(t *testing.T) {
	t.Parallel()
	domain := chat.ScopeTerm{Kind: chat.TermDomain}.Path()
	room := chat.ScopeTerm{Kind: chat.TermChannel, ID: uuid.NewString()}.Path()
	address := chat.ScopeTerm{Kind: chat.TermName, ID: chat.ChannelToken("launch")}.Path()

	for _, inner := range []string{room, address} {
		if !statelog.Covers(domain, inner) {
			t.Errorf("the domain term %q does not cover %q — a gate names the "+
				"domain, so anything it did not cover would go on being "+
				"applied", domain, inner)
		}
		if !strings.HasPrefix(inner, domain+statelog.ScopeSeparator) {
			t.Errorf("%q is not written under %q with the framework's own "+
				"separator", inner, domain)
		}
	}
	// AN ADDRESS IS NOT A ROOM. A create claims both, and a deferral on a
	// name must not block every write in the room that eventually holds
	// it — nor the reverse, since at the moment the claim is written the
	// room does not exist.
	if statelog.Covers(room, address) || statelog.Covers(address, room) {
		t.Fatalf("the address %q and the room %q cover each other", address, room)
	}
}

// AN UNREADABLE SCOPE IS THE WHOLE DOMAIN, AND IT IS NEVER AN ERROR.
//
// This runs on a node decoding a record a NEWER build wrote. The only honest
// reading of a blast radius this build cannot parse is "everything", and
// returning an error would take the envelope pass down with it — leaving a
// record with no subject to file it under, which can only be dropped.
//
// AN EMPTY SET IS THE SAME CASE. "This record makes nothing stale" is the one
// claim a record a build cannot read may not make.
func TestAnUnreadableScopeIsTheWholeDomain(t *testing.T) {
	t.Parallel()
	domain := chat.ScopeTerm{Kind: chat.TermDomain}.Path()
	room := chat.ScopeTerm{Kind: chat.TermChannel, ID: uuid.NewString()}.Path()

	for _, raw := range []string{
		`"x"`,                           // a sentinel from a grammar this build has not got
		`"s/ENG"`,                       // the wiki's sentinel, which is not this one
		`42`,                            // not a scope at all
		`{"kind":"channel"}`,            // an object where an array or a string belongs
		`[]`,                            // an enumeration that names nothing
		`[{"k":"huddle","i":"room-1"}]`, // a term kind from a newer build
		`null`,
	} {
		var s chat.ScopeSet
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatalf("decoding %s failed with %v — widest-on-unreadable is the "+
				"contract, and an error here fails the envelope pass that "+
				"exists so such a record can be filed at all", raw, err)
		}
		got := onePath(t, s.Resolve(chat.ChannelSubject(uuid.NewString())))
		if got != domain {
			t.Errorf("%s resolved to %q, want the whole domain %q", raw, got, domain)
		}
		if !statelog.Covers(got, room) {
			t.Errorf("%s resolved to %q, which does not cover a room", raw, got)
		}
	}
}

// THE COMMON CASE IS ONE BYTE ON THE WIRE, AND IT SURVIVES THE ROUND TRIP.
//
// Every message ever posted carries the exactly-the-subject sentinel, so its
// encoding is the one that has to be cheap — and it has to decode back to the
// sentinel rather than to an enumeration, or the record would claim a blast
// radius the writer never stated.
func TestTheSubjectSentinelIsOneByteAndRoundTrips(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(chat.ScopeSet{Subject: true})
	if err != nil {
		t.Fatalf("encode the sentinel: %v", err)
	}
	if string(encoded) != `"`+chat.ScopeSentinel+`"` {
		t.Fatalf("the sentinel encoded as %s, want %q", encoded, chat.ScopeSentinel)
	}
	var back chat.ScopeSet
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("decode the sentinel: %v", err)
	}
	if !back.Subject || len(back.Terms) != 0 {
		t.Fatalf("the sentinel decoded as %#v, want exactly-the-subject", back)
	}

	// AND AN ENUMERATION SURVIVES AS AN ENUMERATION.
	room := uuid.NewString()
	create := chat.ScopeSet{Terms: []chat.ScopeTerm{
		{Kind: chat.TermName, ID: chat.ChannelToken("launch")},
		{Kind: chat.TermChannel, ID: room},
	}}
	data, err := json.Marshal(create)
	if err != nil {
		t.Fatalf("encode a create's scope: %v", err)
	}
	var terms chat.ScopeSet
	if err := json.Unmarshal(data, &terms); err != nil {
		t.Fatalf("decode a create's scope: %v", err)
	}
	if len(terms.Terms) != 2 || terms.Subject {
		t.Fatalf("a create's scope decoded as %#v, want its two terms", terms)
	}
	resolved := terms.Resolve(chat.ChannelNameSubject("launch"))
	if len(resolved.Paths) != 2 {
		t.Fatalf("a create resolved to %v, want the address it claimed and the "+
			"room it made", resolved.Paths)
	}
}

// A SCOPE A WRITER COULD NOT HAVE MEANT IS REFUSED AT THE WRITE, NAMING WHY.
//
// Validate runs where a record is WRITTEN and never where one is read: a scope
// off the wire has already been widened to the domain, and refusing it on the
// read side would turn a newer peer's record into a decode failure.
func TestAScopeAWriterCouldNotHaveMeantIsRefused(t *testing.T) {
	t.Parallel()
	tooMany := make([]chat.ScopeTerm, chat.MaxScopeTerms+1)
	for i := range tooMany {
		tooMany[i] = chat.ScopeTerm{Kind: chat.TermChannel, ID: uuid.NewString()}
	}
	for _, tc := range []struct {
		name string
		set  chat.ScopeSet
	}{
		{"the sentinel and an enumeration at once", chat.ScopeSet{
			Subject: true,
			Terms:   []chat.ScopeTerm{{Kind: chat.TermDomain}},
		}},
		{"names nothing at all", chat.ScopeSet{}},
		{"over the term cap", chat.ScopeSet{Terms: tooMany}},
		{"a term kind this build does not write", chat.ScopeSet{
			Terms: []chat.ScopeTerm{{Kind: "huddle", ID: "room-1"}},
		}},
		{"a term with no id", chat.ScopeSet{
			Terms: []chat.ScopeTerm{{Kind: chat.TermChannel}},
		}},
		{"a term id carrying the path separator", chat.ScopeSet{
			Terms: []chat.ScopeTerm{{Kind: chat.TermChannel, ID: "a/b"}},
		}},
	} {
		err := tc.set.Validate()
		if err == nil {
			t.Errorf("%s: validated", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "scope") {
			t.Errorf("%s: the refusal %q does not name the field", tc.name, err)
		}
	}
	// AND THE TWO SHAPES A WRITER ACTUALLY WRITES ARE ACCEPTED.
	for _, ok := range []chat.ScopeSet{
		{Subject: true},
		{Terms: []chat.ScopeTerm{
			{Kind: chat.TermName, ID: chat.ChannelToken("launch")},
			{Kind: chat.TermChannel, ID: uuid.NewString()},
		}},
		{Terms: []chat.ScopeTerm{{Kind: chat.TermDomain}}},
	} {
		if err := ok.Validate(); err != nil {
			t.Errorf("a scope this package writes was refused: %v", err)
		}
	}
}

// A BARRIER MAKES NOTHING STALE, SO ITS SCOPE INTERSECTS NOTHING.
//
// A barrier writes no row on any node. A scope that overlapped a real object
// would make every linearizable read look as though it had been invalidated by
// the very append that establishes it, and would serialise each read behind
// every other.
func TestABarrierScopeIntersectsNothing(t *testing.T) {
	t.Parallel()
	barrier := chat.ScopeSet{Subject: true}.Resolve(chat.BarrierSubject())
	if got := onePath(t, barrier); got != statelog.BarrierScope {
		t.Fatalf("a barrier resolved to %q, want the framework's own %q",
			got, statelog.BarrierScope)
	}
	for _, other := range []chat.Subject{
		chat.ChannelSubject(uuid.NewString()),
		chat.MessageSubject(uuid.NewString()),
		chat.ChannelNameSubject("launch"),
	} {
		object := chat.ScopeSet{Subject: true}.Resolve(other)
		if barrier.Intersects(object) {
			t.Errorf("a barrier intersects %s, so every linearizable read would "+
				"wait behind every other", other)
		}
	}
}

// A GATE IS ABOUT THE WHOLE DOMAIN, AND SO IS A KIND THIS BUILD CANNOT PLACE.
//
// An eviction licenses or drops records on every subject, so anything narrower
// would be a claim the record does not make. An unknown kind from a newer peer
// lands in the same place for the mirror reason: narrowing it to the room its
// id happens to look like would be a containment claim nobody wrote.
func TestAGateAndAnUnknownKindBothResolveToTheDomain(t *testing.T) {
	t.Parallel()
	domain := chat.ScopeTerm{Kind: chat.TermDomain}.Path()
	for _, s := range []chat.Subject{
		chat.EvictionSubject("node-a"),
		chat.GenerationSubject(4),
		{Kind: "huddle", ID: uuid.NewString()},
	} {
		got := onePath(t, chat.ScopeSet{Subject: true}.Resolve(s))
		if got != domain {
			t.Errorf("%s resolved to %q, want the whole domain %q", s, got, domain)
		}
	}
}
