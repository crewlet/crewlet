package topics_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// separators is what a seat token must never contribute to a subject: the
// token separator, and the two NATS wildcards. A token carrying any of them
// stops being a name and becomes syntax.
const separators = ".*>"

// THIS FILE WAS ABOUT THE HANDLE, and it is re-pointed rather than deleted.
//
// [topics.AgentInbox] used to interpolate a seat's HANDLE into a subject, and
// what made that safe lived entirely on the other side of the tree:
// org.ValidHandle enforces ^[a-z0-9][a-z0-9-]*$, which happens to exclude
// every character the subject grammar treats as syntax. The subject carries
// the seat's ID now (ADR-0019), so that particular dependency is gone — and
// the invariant it protected is not. It simply moved: what has to be a safe,
// single, canonical token is whatever [org.Organization.AgentIDFor] derives.
//
// The dependency is still load-bearing and still written down nowhere else.
// Neither package states it: org derives a uuid without knowing a subject is
// built from it, and topics interpolates and now PARSES a token without
// knowing where it came from.
//
// The consequences, unchanged in kind from the handle's:
//
//   - A dot splits the subject into extra tokens, so the seat's inbox no
//     longer matches crewlet.agent.*.inbox and stops being covered by
//     anything scoped to one seat.
//   - A `*` or a `>` turns the seat's OWN inbox into a wildcard PATTERN. A
//     node subscribing that seat would attach to a subject matching its
//     peers' inboxes — it receives their mail, and they stop receiving it.
//   - And one the handle never had: the token is PARSED back by
//     [topics.SeatFromInbox], which a diagnostic names a seat from and a
//     retirement sweep DELETES what it names. A derivation that produced a
//     non-canonical spelling would mint a subject its own inverse refuses,
//     so the sweep would see an unregistered mailbox for a seat that has one.

// TestEverySeatIDTheOrgDerivesYieldsASafeSubject walks the handles the org
// accepts and mints, derives each seat's id the way the engine does, and holds
// every name built from it against the subject grammar.
//
// Over the HANDLES rather than over arbitrary uuids, because the derivation is
// what is under test: a change that made AgentIDFor return something other
// than a uuid — a handle, a composite, a base64 digest — is exactly the change
// this has to catch, and a case that fed it a uuid to begin with could not.
func TestEverySeatIDTheOrgDerivesYieldsASafeSubject(t *testing.T) {
	t.Parallel()

	// Every handle the org ACCEPTS, exhaustive over characters rather than
	// a sample, in all three positions a character-class regex can
	// distinguish.
	accepted := 0
	for r := rune(0); r < 0x300; r++ {
		for _, candidate := range []string{
			string(r),             // the whole handle
			"a" + string(r),       // last position
			string(r) + "a",       // first position
			"a" + string(r) + "b", // interior
		} {
			if !org.ValidHandle(candidate) {
				continue
			}
			accepted++
			requireSafeSubject(t, "acme", candidate)
		}
	}
	if accepted == 0 {
		t.Fatal("org.ValidHandle accepted none of the candidates — either the pattern " +
			"changed shape or this loop stopped generating handles, and either way " +
			"the invariant went uncertified")
	}
	t.Logf("checked %d single-character handle placements org.ValidHandle accepts", accepted)

	// ...and the handles the org actually MINTS, which is the other way a
	// seat reaches this package: an operator who never wrote one gets
	// Slugify's output from the role name.
	minted := 0
	for _, name := range []string{
		"CEO", "QA Lead", "Head of Platform / Infra", "Émile Zola",
		"release.control", "a*b", "a>b", "ops — on call", "  padded  ",
		"7-Eleven Liaison", "Ünïcødé Person",
	} {
		handle := org.Slugify(name)
		if handle == "" {
			continue
		}
		if !org.ValidHandle(handle) {
			t.Errorf("org.Slugify(%q) = %q, which org.ValidHandle rejects — a seat "+
				"the org can name but cannot address", name, handle)
			continue
		}
		minted++
		requireSafeSubject(t, "acme", handle)
	}
	if minted == 0 {
		t.Fatal("org.Slugify produced no usable handles; this half certified nothing")
	}

	// AND THE COMPANY NAME IS AN INPUT TOO, so a company named in prose
	// full of subject syntax must not reach a subject either.
	for _, company := range []string{"Acme", "acme.corp", "a*b", "a>b", "Ünïcødé GmbH", "   "} {
		requireSafeSubject(t, company, "ceo")
	}
}

// requireSafeSubject derives the seat's id the way the engine does and asserts
// the properties every name built from it must have: the expected token count,
// no subject syntax smuggled in, and a token its own inverse reads back.
func requireSafeSubject(t *testing.T, company, handle string) {
	t.Helper()

	o := &org.Organization{Name: company, Roles: []*org.Role{
		{Name: "Seat", DeclaredHandle: handle}}}
	id, ok := o.AgentIDFor(o.Roles[0])
	if !ok {
		// A company or a handle too unnamed to derive from. Nothing is
		// built, which is the documented answer and not a failure.
		return
	}
	token := id.String()

	for _, tc := range []struct {
		what    string
		subject string
		want    []string
	}{
		{"AgentInbox", topics.AgentInbox(id), []string{"crewlet", "agent", token, "inbox"}},
		{"AgentControl", topics.AgentControl(id), []string{"crewlet", "agent", token, "control"}},
	} {
		got := strings.Split(tc.subject, ".")
		if len(got) != len(tc.want) {
			t.Errorf("%s for company %q seat %q = %q splits into %d tokens %q, want %d — "+
				"a seat id that carries a separator stops being one token, so the seat "+
				"is no longer covered by anything scoped to one seat",
				tc.what, company, handle, tc.subject, len(got), got, len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s for company %q seat %q = %q, token %d is %q, want %q",
					tc.what, company, handle, tc.subject, i, got[i], tc.want[i])
			}
		}
		if i := strings.IndexAny(tc.subject, "*>"); i >= 0 {
			t.Errorf("%s for company %q seat %q = %q contains the wildcard %q — this "+
				"seat's own inbox is now a PATTERN over its peers' inboxes, so it "+
				"receives their mail and they stop receiving it",
				tc.what, company, handle, tc.subject, tc.subject[i:i+1])
		}
	}

	// THE INVERSE READS IT BACK. A sweep deletes what it names, so a
	// derivation minting a token the inverse refuses would leave every
	// mailbox looking unregistered.
	if back, ok := topics.SeatFromInbox(topics.AgentInbox(id)); !ok || back != id {
		t.Errorf("SeatFromInbox(AgentInbox(%s)) = (%s, %v) for company %q seat %q, "+
			"want the same id — a subject its own inverse refuses is a mailbox "+
			"nothing can identify", id, back, ok, company, handle)
	}

	// Groups are not subjects, but they are wire names on the same
	// characters, and a wildcard in a durable subscription name is
	// rejected outright by JetStream — a seat that cannot be subscribed
	// at all.
	for _, tc := range []struct{ what, group string }{
		{"AgentInboxGroup", topics.AgentInboxGroup(id)},
		{"AgentControlGroup", topics.AgentControlGroup(id)},
	} {
		if i := strings.IndexAny(tc.group, separators); i >= 0 {
			t.Errorf("%s for company %q seat %q = %q contains %q",
				tc.what, company, handle, tc.group, tc.group[i:i+1])
		}
	}
}

// TestASubjectIsBuiltFromTheIDAndNotTheHandle shows the invariant from the
// other side: the exact strings that would break routing if a seat's handle
// ever reached a subject again, and the fact that nothing but a canonical
// uuid is read back out of one.
//
// Without this half, the test above could pass because the derivation had
// been narrowed to accept nothing at all.
func TestASubjectIsBuiltFromTheIDAndNotTheHandle(t *testing.T) {
	t.Parallel()

	// The collision, demonstrated rather than asserted in the abstract.
	// These calls are legal — AgentInbox has no opinion about its argument
	// beyond its type — and the results are why the type is a uuid.
	victim := topics.AgentInbox(alice)
	for _, tc := range []struct{ name, token string }{
		{"a dot, the namespacing someone will eventually propose", "platform.infra"},
		{"the one-token wildcard", "*"},
		{"the tail wildcard", ">"},
		{"a space", "qa lead"},
		{"uppercase, which two producers would case-fold differently", "Alice"},
	} {
		hand := topics.AgentInboxPrefix + tc.token + topics.AgentInboxSuffix
		if _, ok := topics.SeatFromInbox(hand); ok {
			t.Errorf("%s: SeatFromInbox(%q) named a seat. The inverse is what a "+
				"diagnostic reads and what a retirement sweep DELETES, so a "+
				"hand-built subject under this prefix must never resolve to one",
				tc.name, hand)
		}
	}
	if wildcard := topics.AgentInboxPrefix + "*" + topics.AgentInboxSuffix; !topics.Match(wildcard, victim) {
		t.Errorf("expected %q to match %q; the demonstration this test rests on no "+
			"longer holds, so re-derive it before trusting the rule", wildcard, victim)
	}
	if tail := topics.AgentInboxPrefix + ">" + topics.AgentInboxSuffix; topics.Match(tail, victim) {
		// `>` is only a wildcard as the FINAL token, so a token of ">"
		// does not swallow a peer's inbox — it produces a subject that
		// matches nothing but itself. Recorded so the next reader does
		// not assume both wildcards fail the same way.
		t.Errorf("%q matched %q; `>` is documented as final-token-only", tail, victim)
	}
}

// TestDistinctSeatsNeverShareAName is the collision property stated
// positively: distinct seats get distinct subjects and distinct groups, and
// no seat's subject matches another's.
//
// Over seats the ORG derives, including the near misses a shared company name
// or a shared handle makes possible, since those are where an accidental
// sharing would actually come from.
func TestDistinctSeatsNeverShareAName(t *testing.T) {
	t.Parallel()

	type seat struct{ company, handle string }
	seats := []seat{
		{"acme", "alice"}, {"acme", "alice-2"}, {"acme", "a"}, {"acme", "ab"},
		{"acme", "a-b"}, {"acme", "0"}, {"acme", "release"},
		{"acme", "release-control"}, {"acme", "control"}, {"acme", "inbox"},
		// Two companies sharing a handle: the derivation is namespaced by
		// the company name precisely so these cannot collide.
		{"acme-two", "alice"}, {"acme", "alice"},
	}
	ids := map[uuid.UUID]seat{}
	for _, s := range seats {
		id, ok := org.DeriveAgentID(s.company, s.handle)
		if !ok {
			t.Fatalf("%q/%q derives no id; fix the corpus", s.company, s.handle)
		}
		if prev, dup := ids[id]; dup && prev != s {
			t.Errorf("%q/%q and %q/%q derive one id %s — one seat would take the "+
				"other's mail", prev.company, prev.handle, s.company, s.handle, id)
		}
		ids[id] = s
	}

	seen := map[string]string{}
	claim := func(t *testing.T, name, owner string) {
		t.Helper()
		if prev, dup := seen[name]; dup {
			t.Errorf("%q is minted for both %s and %s", name, prev, owner)
			return
		}
		seen[name] = owner
	}
	for id, s := range ids {
		claim(t, topics.AgentInbox(id), fmt.Sprintf("inbox of %q/%q", s.company, s.handle))
		claim(t, topics.AgentControl(id), fmt.Sprintf("control of %q/%q", s.company, s.handle))
	}

	for a := range ids {
		for b := range ids {
			if a == b {
				continue
			}
			if topics.Match(topics.AgentInbox(a), topics.AgentInbox(b)) {
				t.Errorf("seat %s's inbox subject matches seat %s's — one would take "+
					"the other's mail", a, b)
			}
			if topics.Match(topics.AgentInbox(a), topics.AgentControl(b)) {
				t.Errorf("seat %s's inbox subject matches seat %s's control subject; a "+
					"sandbox completion would be requeued behind the wait it exists to end",
					a, b)
			}
		}
	}
	if len(seen) != 2*len(ids) {
		t.Fatalf("minted %d distinct names for %d seats; the corpus or the walk is wrong",
			len(seen), 2*len(ids))
	}
}
