// Package topics_test certifies the subject grammar.
//
// The package under test is one string join per function, which is exactly
// why it needs pinning: nothing here can fail loudly. A producer and a
// consumer that disagree about a subject raise nothing anywhere — the
// publish succeeds into a topic no consumer reads, and the event is
// swallowed. Every assertion below is therefore about a NAME, and the cost
// of getting one wrong is silence.
//
// The suite is external (topics_test, not topics) on purpose. The package
// doc promises it "imports nothing else from the engine" so any layer may
// use it, and an internal test file importing internal/org would make that
// promise false for anyone reading the import graph. Being external also
// means these tests exercise only the exported surface, which is the whole
// surface anyone else can depend on.
package topics_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// alice and bob are two seats, as ids rather than handles: every per-seat name
// in this package is built from the seat's id, so a case naming one by handle
// would be testing a grammar nothing mints.
//
// FIXED LITERALS, so the expected subjects below can be written out. A derived
// value would make every assertion a restatement of the derivation.
const (
	aliceID = "0f1d4c07-6d2a-4e2b-9a3c-5b8e17d04c21"
	bobID   = "3a7b91e2-5c44-4f18-8d60-2e9a0c6fb735"
)

func seat(id string) uuid.UUID { return uuid.MustParse(id) }

var alice, bob = seat(aliceID), seat(bobID)

// TestAgentSubjectsAreTheDocumentedJoin pins the shape of every per-seat
// name, and pins it against the exported constants rather than against a
// second copy of the literal — a test that hard-codes "crewlet.agent.%s.inbox"
// keeps agreeing with itself after a grammar change while agreeing with
// nothing the engine does.
//
// The literal forms are still written out once each, because a constant-only
// assertion is a tautology: it would pass with every constant set to "x".
func TestAgentSubjectsAreTheDocumentedJoin(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"inbox subject", topics.AgentInbox(alice), "crewlet.agent." + aliceID + ".inbox"},
		{"inbox group", topics.AgentInboxGroup(alice), "agent-" + aliceID},
		{"control subject", topics.AgentControl(alice), "crewlet.agent." + aliceID + ".control"},
		{"control group", topics.AgentControlGroup(alice), "agent-" + aliceID + "-control"},
		// THE CANONICAL SPELLING, and only that one: uuid.Parse accepts
		// braced and urn-prefixed forms that String() never emits, and each
		// of them would be a different subject naming one seat.
		{"the canonical form", topics.AgentInbox(seat("{" + aliceID + "}")),
			"crewlet.agent." + aliceID + ".inbox"},
		{"an event subject", topics.Event("agent_phase_started"), "crewlet.events.agent_phase_started"},
		// A dead letter is not here: its tail is a digest of the pair
		// rather than a join, so its shape is pinned by
		// TestDeadLetterKeepsTheTopicGreppable instead.
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	// ...and the constants callers build wildcards and stream topologies
	// from must agree with those joins, or a backend provisioning a stream
	// from the prefix would provision one that does not cover the subject.
	if !strings.HasPrefix(topics.AgentInbox(alice), topics.AgentInboxPrefix) {
		t.Errorf("AgentInbox does not start with AgentInboxPrefix %q", topics.AgentInboxPrefix)
	}
	if !strings.HasSuffix(topics.AgentInbox(alice), topics.AgentInboxSuffix) {
		t.Errorf("AgentInbox does not end with AgentInboxSuffix %q", topics.AgentInboxSuffix)
	}
	if !strings.HasSuffix(topics.AgentControl(alice), topics.AgentControlSuffix) {
		t.Errorf("AgentControl does not end with AgentControlSuffix %q", topics.AgentControlSuffix)
	}
	if !strings.HasPrefix(topics.Event("agent_phase_started"), topics.EventsPrefix) {
		t.Errorf("Event does not start with EventsPrefix %q", topics.EventsPrefix)
	}
	if !strings.HasPrefix(topics.DeadLetter("a", "b"), topics.DeadLetterPrefix) {
		t.Errorf("DeadLetter does not start with DeadLetterPrefix %q", topics.DeadLetterPrefix)
	}
	for _, tc := range []struct{ prefix, subject string }{
		{topics.NotificationsPrefix, topics.NotificationsInbound},
		{topics.ConfigPrefix, topics.ConfigRevisionActivated},
		{topics.ConfigPrefix, topics.ConfigRevisionApplied},
	} {
		if !strings.HasPrefix(tc.subject, tc.prefix) {
			t.Errorf("%q is not under its domain prefix %q", tc.subject, tc.prefix)
		}
	}
}

// TestTheWildcardCoversTheDomainItNames pins that each exported wildcard and
// domain prefix actually covers the subjects of its domain and nothing else.
//
// A stream is provisioned from these. A wildcard that missed its own domain
// would create a stream nothing lands in, and a publish to an uncovered
// subject is dropped rather than refused — the same silence as a mistyped
// subject, one layer down.
func TestTheWildcardCoversTheDomainItNames(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		pattern string
		subject string
		want    bool
	}{
		{topics.EventsWildcard, topics.Event("agent_phase_started"), true},
		{topics.EventsWildcard, topics.NotificationsInbound, false},
		{topics.EventsWildcard, topics.ConfigRevisionApplied, false},
		{topics.EventsWildcard, topics.AgentInbox(alice), false},

		{topics.AgentInboxPrefix + ">", topics.AgentInbox(alice), true},
		{topics.AgentInboxPrefix + ">", topics.AgentControl(alice), true},
		{topics.AgentInboxPrefix + ">", topics.Event("agent_phase_started"), false},

		{topics.NotificationsPrefix + ">", topics.NotificationsInbound, true},
		{topics.NotificationsPrefix + ">", topics.Event("agent_phase_started"), false},

		{topics.ConfigPrefix + ">", topics.ConfigRevisionActivated, true},
		{topics.ConfigPrefix + ">", topics.ConfigRevisionApplied, true},
		{topics.ConfigPrefix + ">", topics.NotificationsInbound, false},

		{topics.DeadLetterPrefix + ">", topics.DeadLetter(topics.AgentInbox(alice),
			topics.AgentInboxGroup(alice)), true},

		// The reason dead letters live outside crewlet.*: the dashboard
		// streams crewlet.events.>, and a dead-lettered subject inside
		// that space would resurface poison as live traffic.
		{topics.EventsWildcard, topics.DeadLetter(topics.Event("agent_phase_started"), "grp"), false},
	} {
		if got := topics.Match(tc.pattern, tc.subject); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
	}
}

// TestANilSeatYieldsNoSubject pins the deliberate empty return, and pins WHY
// it is not "crewlet.agent..inbox".
//
// That string is a real, publishable subject with no subscriber. A caller
// that lost the seat's id would publish into it and the event would be
// swallowed: no error at the producer, no delivery at the consumer, and a
// seat that simply never wakes. Returning "" makes the caller's mistake
// something a caller can test for.
//
// The property is asserted for every id-derived name, not just the inbox,
// because a caller that guards the subject and not the group would attach a
// consumer named "agent-" to the right topic — one durable subscription
// shared by every seat that lost its id.
func TestANilSeatYieldsNoSubject(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		got  string
	}{
		{"AgentInbox", topics.AgentInbox(uuid.Nil)},
		{"AgentInboxGroup", topics.AgentInboxGroup(uuid.Nil)},
		{"AgentControl", topics.AgentControl(uuid.Nil)},
		{"AgentControlGroup", topics.AgentControlGroup(uuid.Nil)},
		{"Event", topics.Event("")},
	} {
		if tc.got != "" {
			t.Errorf("%s(\"\") = %q, want \"\" — a name built around nothing is a "+
				"real subject nobody reads, so the caller must be able to see the gap",
				tc.name, tc.got)
		}
	}

	// DeadLetter is deliberately NOT in that list, and the asymmetry is
	// worth stating rather than leaving as an oversight to be "fixed".
	//
	// The empty guard exists for a name derived from a SEAT ID, where the
	// caller holds a seat and has lost its identity, and where the safe
	// answer is to publish nothing. A dead letter is the opposite: it is
	// the last copy of a message that already failed, so returning "" would
	// make the caller drop the evidence. Degenerate halves therefore still
	// produce a legal, distinct subject — a placeholder segment for the
	// empty half, and the digest to keep it distinct from every other pair.
	empty := topics.DeadLetter("", "")
	requirePublishableSubject(t, empty)
	if empty == topics.DeadLetter("", "x") || empty == topics.DeadLetter("x", "") {
		t.Errorf("DeadLetter(\"\", \"\") = %q collides with a pair that has one half set",
			empty)
	}
}

// TestSeatFromInboxIsTheInverseOfAgentInbox pins the round trip and, more
// importantly, pins what it must REFUSE.
//
// The caller is a log line, a diagnostic or a sweep naming the seat behind
// some traffic. A false identification there is worse than no identification:
// it puts an uninvolved seat in front of whoever is debugging, and the sweep
// deletes what it names.
func TestSeatFromInboxIsTheInverseOfAgentInbox(t *testing.T) {
	t.Parallel()

	for _, id := range []uuid.UUID{alice, bob, uuid.MustParse(
		"00000000-0000-4000-8000-000000000001")} {

		got, ok := topics.SeatFromInbox(topics.AgentInbox(id))
		if !ok || got != id {
			t.Errorf("round trip of %s: SeatFromInbox(%q) = (%s, %v), want (%s, true)",
				id, topics.AgentInbox(id), got, ok, id)
		}
	}

	for _, tc := range []struct{ name, subject string }{
		{"the empty string", ""},
		{"a control subject, not an inbox", topics.AgentControl(alice)},
		{"an event subject", topics.Event("agent_phase_started")},
		{"what an unroutable seat produces", "crewlet.agent..inbox"},
		{"a token with a dot would be two segments", "crewlet.agent.a.b.inbox"},
		{"the suffix alone", ".inbox"},
		{"the prefix alone", "crewlet.agent."},
		{"a longer subject that merely contains one", "x.crewlet.agent." + aliceID + ".inbox"},
		{"a dead letter carrying one", topics.DeadLetter(topics.AgentInbox(alice),
			topics.AgentInboxGroup(alice))},

		// THE TOKEN IS PARSED, not merely extracted. A consumer somebody
		// attached by hand under this prefix is not a mailbox, and the
		// sweep that reads this deletes what it is told is one.
		{"a handle where a seat id belongs", "crewlet.agent.alice.inbox"},
		{"a uuid in a spelling String() never emits", "crewlet.agent.{" + aliceID + "}.inbox"},
		{"the nil uuid", "crewlet.agent.00000000-0000-0000-0000-000000000000.inbox"},

		// The prefix ends in a dot and the suffix begins with one, so
		// they OVERLAP on a single character. Matching each against the
		// whole subject let this one satisfy both at once and report a
		// seat token of "inbox".
		{"prefix and suffix sharing one dot", "crewlet.agent.inbox"},
	} {
		if got, ok := topics.SeatFromInbox(tc.subject); ok {
			t.Errorf("%s: SeatFromInbox(%q) = (%s, true), want (nil, false)",
				tc.name, tc.subject, got)
		}
	}
}

// A retirement sweep deletes what MailboxSeat names, so a false positive is a
// consumer destroyed that belonged to nobody's mailbox.
func TestMailboxSeatNamesOnlyAPairTheMailboxGrammarProduces(t *testing.T) {
	t.Parallel()

	for _, id := range []uuid.UUID{alice, bob} {
		for _, pair := range [][2]string{
			{topics.AgentInbox(id), topics.AgentInboxGroup(id)},
			{topics.AgentControl(id), topics.AgentControlGroup(id)},
		} {
			got, ok := topics.MailboxSeat(pair[0], pair[1])
			if !ok || got != id {
				t.Errorf("MailboxSeat(%q, %q) = (%s, %v), want (%s, true)",
					pair[0], pair[1], got, ok, id)
			}
		}
	}

	for _, tc := range []struct{ name, topic, group string }{
		{"an inbox under another seat's group", topics.AgentInbox(alice), topics.AgentInboxGroup(bob)},
		{"an inbox under the control group", topics.AgentInbox(alice), topics.AgentControlGroup(alice)},
		{"a control subject under the inbox group", topics.AgentControl(alice), topics.AgentInboxGroup(alice)},
		// The collision TestGroupNamesAreNotUniqueOnTheirOwn describes,
		// from the side that must NOT match: `agent-` + X + `-control`
		// and `agent-` + Y coincide when Y is X + "-control", so that
		// group on X's INBOX topic is neither seat's mailbox.
		{"a control group spelled onto an inbox topic", topics.AgentInbox(alice),
			topics.AgentInboxGroup(alice) + "-control"},
		{"a control subject with a dotted token", "crewlet.agent.a.b.control", "agent-a.b-control"},
		{"an empty control token", "crewlet.agent..control", "agent--control"},
		{"a handle where a seat id belongs", "crewlet.agent.alice.control", "agent-alice-control"},
		{"a service subscription", topics.NotificationsInbound, "notify-inbound"},
		{"an event subscription", topics.Event("task_created"), topics.AgentInboxGroup(alice)},
	} {
		if got, ok := topics.MailboxSeat(tc.topic, tc.group); ok {
			t.Errorf("%s: MailboxSeat(%q, %q) = (%s, true), want (nil, false)",
				tc.name, tc.topic, tc.group, got)
		}
	}
}

// TestGroupNamesAreNotUniqueOnTheirOwn states an invariant of the GRAMMAR
// that every backend already depends on.
//
// `agent-` + X and `agent-` + Y + `-control` are the same string whenever X is
// Y + "-control". No pair of seat IDS reaches that — a uuid never ends in
// "-control" — but the grammar is what a backend keys on, and the property it
// must not assume is that a group name alone identifies a subscription.
//
// What makes it harmless is that a subscription is identified by the (topic,
// group) PAIR, and the two pairs differ in their topic. JetStream's
// consumerName hashes the pair for exactly this reason. A backend that ever
// keyed a subscription on the group ALONE would collapse two subscriptions
// onto one mailbox, which is the failure this note exists to prevent.
//
// Asserted over the GRAMMAR rather than over two seats, because the seat ids
// that used to demonstrate it were handles. Do not read the absence of a
// colliding pair of ids as the collision being gone: it is a property of the
// token space, and the token space is not this package's to promise.
func TestGroupNamesAreNotUniqueOnTheirOwn(t *testing.T) {
	t.Parallel()

	// X is alice's group name; Y is a token that, run through the inbox
	// grammar, mints the same string alice's CONTROL group does.
	control := topics.AgentControlGroup(alice)
	collides := topics.AgentGroupPrefix + aliceID + topics.AgentControlGroupSuffix
	if control != collides {
		t.Fatalf("the group grammar no longer composes as this test assumes: "+
			"AgentControlGroup = %q, prefix+id+suffix = %q", control, collides)
	}

	// What makes it harmless: the PAIRS are distinct, because the subjects
	// are. Assert that, since it is the property a backend relies on.
	if a, b := topics.AgentInbox(alice), topics.AgentControl(alice); a == b {
		t.Errorf("a seat's inbox and control subjects are one string: %q", a)
	}
}

// TestDeadLetterIsInjectiveOverArbitraryPairs pins that no two distinct
// subscriptions share a dead-letter subject — over the pairs the CONTRACT
// accepts, not merely over the names this grammar happens to mint.
//
// The narrower property was the trap. EnsureSubscription takes whatever
// (topic, group) a caller passes, and the conformance suite now subscribes
// with the dotted group "h.i" on purpose, because a backend was aliasing
// distinct pairs one layer up. A dot-joined "dlq." + topic + "." + group
// aliases the moment either half contains a dot: ("a.b", "c") and
// ("a", "b.c") were one subject. Two subscriptions' poison in one place, with
// nothing to separate it — a dead letter carries the original event, not the
// subscription that gave up on it.
//
// BOUNDARY: injectivity here rests on a 48-bit digest, so it is
// probabilistic rather than structural. That is the same bound the durable
// consumer name in internal/queue/jetstream runs on, sized against a real
// ceiling of hundreds of subscriptions. The READABLE head is lossy by design
// and proves nothing on its own — two pairs may well share it.
func TestDeadLetterIsInjectiveOverArbitraryPairs(t *testing.T) {
	t.Parallel()

	// The names the grammar mints...
	seats := []uuid.UUID{alice, bob,
		uuid.MustParse("00000000-0000-4000-8000-000000000001")}
	var subjects, groups []string
	for _, id := range seats {
		subjects = append(subjects, topics.AgentInbox(id), topics.AgentControl(id))
		groups = append(groups, topics.AgentInboxGroup(id), topics.AgentControlGroup(id))
	}
	subjects = append(subjects,
		topics.Event("agent_phase_started"), topics.Event("agent_turn_completed"),
		topics.NotificationsInbound,
		topics.ConfigRevisionActivated, topics.ConfigRevisionApplied,
	)

	// ...and the names a caller may pass that the grammar never mints. The
	// pairs whose halves differ only in where the boundary falls are the
	// ones the old join collapsed.
	//
	// "a"/"b" and "a"/"ab" are there for a sharper reason than variety:
	// they are the pairs that collide when the two halves are digested
	// without a separator between them ("a"+"b" and "ab"+""), which is the
	// mistake a digest invites once it has replaced a join.
	subjects = append(subjects, "a", "b", "a.b", "a.b.c", "a_b", "coll.shared", "coll.a.b", "", "a..b", "a b", "a/b", "a*b")
	groups = append(groups, "g", "a", "ab", "b.c", "c", "h.i", "h_i", "", "x.y.z", "g/h")

	// Deduped, because neither list is guaranteed distinct: the group
	// grammar is not injective (see TestGroupNamesAreNotUniqueOnTheirOwn),
	// so "release-control"'s inbox group and "release"'s control group are
	// one string. Comparing a pair against ITSELF would report a collision
	// that is only this list counting twice.
	subjects, groups = distinct(subjects), distinct(groups)

	seen := map[string][2]string{}
	// The DIGEST is asserted separately from the whole subject, and the
	// separate assertion is the load-bearing one. The readable head is a
	// second thing that varies with the pair, so whole-subject injectivity
	// can hold while the digest itself aliases — measured: a digest taken
	// over group+topic with no separator between them ("a"+"b" and
	// "ab"+"") is invisible through the subject, because the heads "b.a"
	// and "_.ab" still differ. That leaves the doc comment's claim that no
	// two pairs share a digest untested, and the head is documented as
	// lossy and truncatable, so it is exactly the half that may stop
	// separating them.
	digests := map[string][2]string{}
	pairs := 0
	for _, subject := range subjects {
		for _, group := range groups {
			pairs++
			dlq := topics.DeadLetter(subject, group)
			if prev, dup := seen[dlq]; dup {
				t.Errorf("dead letter %q is shared by (%q, %q) and (%q, %q)",
					dlq, prev[0], prev[1], subject, group)
				continue
			}
			seen[dlq] = [2]string{subject, group}

			id := dlq[strings.LastIndex(dlq, ".")+1:]
			if prev, dup := digests[id]; dup {
				t.Errorf("digest %q is shared by (%q, %q) and (%q, %q); the readable "+
					"head is what is telling these apart, and it is lossy by design",
					id, prev[0], prev[1], subject, group)
				continue
			}
			digests[id] = [2]string{subject, group}
		}
	}
	if pairs == 0 {
		t.Fatal("compared no pairs — this test certified nothing")
	}
	t.Logf("compared %d distinct (topic, group) pairs", pairs)

	// The exact pair the dot join collapsed, called out so a reviewer can
	// see the regression this test exists to stop rather than infer it from
	// a cross product.
	if topics.DeadLetter("a.b", "c") == topics.DeadLetter("a", "b.c") {
		t.Error(`DeadLetter("a.b", "c") == DeadLetter("a", "b.c"): the subject is a ` +
			"plain join again, and two subscriptions' poison shares one topic")
	}

	// The result must still be a publishable subject in the ordinary
	// grammar, which is what lets a backend carry poison with no special
	// case — and the special case in the poison path is the code nobody
	// exercises. Checked against the very inputs that could break it.
	for _, subject := range subjects {
		for _, group := range groups {
			requirePublishableSubject(t, topics.DeadLetter(subject, group))
		}
	}

	// Same pair, same subject, every time: the digest is over the inputs
	// and nothing else. A dead-letter subject a reader could not re-derive
	// would make DeadLetters() unable to find what deadLetter() wrote.
	if a, b := topics.DeadLetter("coll.shared", "h.i"), topics.DeadLetter("coll.shared", "h.i"); a != b {
		t.Errorf("DeadLetter is not deterministic: %q then %q", a, b)
	}
}

// TestDeadLetterKeepsTheTopicGreppable pins the half of the subject that is
// for people. The digest is the identity, but an operator hunting poison
// starts from the topic they know, so the head has to still say it.
func TestDeadLetterKeepsTheTopicGreppable(t *testing.T) {
	t.Parallel()

	inbox := topics.AgentInbox(alice)
	dlq := topics.DeadLetter(inbox, topics.AgentInboxGroup(alice))

	head := topics.DeadLetterPrefix + inbox + "." + topics.AgentInboxGroup(alice) + "."
	if !strings.HasPrefix(dlq, head) {
		t.Errorf("DeadLetter = %q, want it to start with %q", dlq, head)
	}
	id := strings.TrimPrefix(dlq, head)
	if len(id) != 12 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("DeadLetter = %q ends in %q, want 12 lowercase hex characters of digest",
			dlq, id)
	}

	// It stays outside crewlet.*, and inside its own namespace, which is
	// what keeps poison off the dashboard's feed and inside the DLQ stream.
	if topics.Match(topics.EventsWildcard, dlq) {
		t.Errorf("%q is matched by %q — poison would resurface as live traffic",
			dlq, topics.EventsWildcard)
	}
	if !topics.Match(topics.DeadLetterPrefix+">", dlq) {
		t.Errorf("%q is not matched by %q, so the DLQ stream would not carry it",
			dlq, topics.DeadLetterPrefix+">")
	}
}

// requirePublishableSubject asserts the checks every backend applies to a
// subject on the way in. A dead letter that trips one cannot be published at
// all, and it is the one message whose loss costs the only evidence.
func requirePublishableSubject(t *testing.T, subject string) {
	t.Helper()
	switch {
	case subject == "":
		t.Errorf("empty subject")
	case strings.ContainsAny(subject, " \t\r\n"):
		t.Errorf("%q contains whitespace", subject)
	case strings.ContainsAny(subject, "*>"):
		t.Errorf("%q contains a wildcard, so it is a pattern rather than a subject", subject)
	case strings.HasPrefix(subject, "."), strings.HasSuffix(subject, "."), strings.Contains(subject, ".."):
		t.Errorf("%q has an empty segment", subject)
	}
}

func distinct(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
