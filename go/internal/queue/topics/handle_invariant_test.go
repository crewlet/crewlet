package topics_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// separators is what a handle must never contribute to a subject: the token
// separator, and the two NATS wildcards. A handle carrying any of them stops
// being a name and becomes syntax.
const separators = ".*>"

// TestEveryHandleTheOrgAcceptsYieldsASafeSubject pins a cross-package
// invariant that neither package states and that both depend on.
//
// [topics.AgentInbox] interpolates a handle straight into a dot-separated
// subject. It escapes nothing, and it cannot: the subject IS the routing key,
// so an escape would have to be understood identically by every producer,
// every consumer and two brokers. What makes the raw interpolation safe is
// entirely on the other side of the tree — org.ValidHandle enforces
// ^[a-z0-9][a-z0-9-]*$, which happens to exclude every character the subject
// grammar treats as syntax.
//
// The dependency is load-bearing and, until this test, written down nowhere.
// Relaxing the handle pattern is an ordinary-looking change — allowing dots
// for namespacing ("platform.infra") is the obvious one someone will propose
// — and it would break routing here, in another package, with no compile
// error and no test failure anywhere near the edit.
//
// The consequences are not lookup misses, which is what makes them expensive:
//
//   - A dot splits the subject into extra tokens, so the seat's inbox no
//     longer matches crewlet.agent.*.inbox and stops being covered by
//     anything scoped to one seat.
//   - A `*` or a `>` turns the seat's OWN inbox into a wildcard PATTERN. A
//     node subscribing that seat would attach to a subject matching its
//     peers' inboxes — it receives their mail, and they stop receiving it.
//     That is a routing collision: mail delivered to the wrong seat, which
//     the receiving seat has no way to detect.
//
// The check is exhaustive over characters rather than a sample, in all three
// positions a character-class regex can distinguish, because that is what a
// relaxed pattern would change.
func TestEveryHandleTheOrgAcceptsYieldsASafeSubject(t *testing.T) {
	t.Parallel()

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
			requireSafeSubject(t, candidate)
		}
	}
	if accepted == 0 {
		t.Fatal("org.ValidHandle accepted none of the candidates — either the pattern " +
			"changed shape or this loop stopped generating handles, and either way " +
			"the invariant went uncertified")
	}
	t.Logf("checked %d single-character handle placements org.ValidHandle accepts", accepted)

	// ...and the handles the org actually MINTS, which is the other way a
	// handle reaches this package: an operator who never wrote one gets
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
		requireSafeSubject(t, handle)
	}
	if minted == 0 {
		t.Fatal("org.Slugify produced no usable handles; this half certified nothing")
	}
}

// requireSafeSubject asserts the properties every name derived from a handle
// must have: the expected token count, and no subject syntax smuggled in.
func requireSafeSubject(t *testing.T, handle string) {
	t.Helper()

	for _, tc := range []struct {
		what    string
		subject string
		want    []string
	}{
		{"AgentInbox", topics.AgentInbox(handle), []string{"crewlet", "agent", handle, "inbox"}},
		{"AgentControl", topics.AgentControl(handle), []string{"crewlet", "agent", handle, "control"}},
	} {
		got := strings.Split(tc.subject, ".")
		if len(got) != len(tc.want) {
			t.Errorf("%s(%q) = %q splits into %d tokens %q, want %d — a handle that "+
				"carries a separator stops being one token, so the seat is no longer "+
				"covered by anything scoped to one seat",
				tc.what, handle, tc.subject, len(got), got, len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s(%q) = %q, token %d is %q, want %q",
					tc.what, handle, tc.subject, i, got[i], tc.want[i])
			}
		}
		if i := strings.IndexAny(tc.subject, "*>"); i >= 0 {
			t.Errorf("%s(%q) = %q contains the wildcard %q — this seat's own inbox is "+
				"now a PATTERN over its peers' inboxes, so it receives their mail and "+
				"they stop receiving it",
				tc.what, handle, tc.subject, tc.subject[i:i+1])
		}
	}

	// Groups are not subjects, but they are wire names on the same
	// characters, and a wildcard in a durable subscription name is
	// rejected outright by JetStream — a seat that cannot be subscribed
	// at all.
	for _, tc := range []struct{ what, group string }{
		{"AgentInboxGroup", topics.AgentInboxGroup(handle)},
		{"AgentControlGroup", topics.AgentControlGroup(handle)},
	} {
		if i := strings.IndexAny(tc.group, separators); i >= 0 {
			t.Errorf("%s(%q) = %q contains %q", tc.what, handle, tc.group, tc.group[i:i+1])
		}
	}
}

// TestTheHandlePatternIsWhatMakesTheSubjectSafe shows the invariant from the
// other side: the exact strings that would break routing, and the fact that
// org.ValidHandle is the only thing standing between them and a subject.
//
// Without this half, the test above could pass because the handle pattern had
// been relaxed to accept nothing at all.
func TestTheHandlePatternIsWhatMakesTheSubjectSafe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, handle string }{
		{"a dot, the namespacing someone will eventually propose", "platform.infra"},
		{"a leading dot", ".infra"},
		{"a trailing dot", "infra."},
		{"the one-token wildcard", "*"},
		{"the tail wildcard", ">"},
		{"a wildcard among ordinary characters", "qa-*"},
		{"uppercase, which two producers would case-fold differently", "Alice"},
		{"a space", "qa lead"},
		{"the empty handle", ""},
	} {
		if org.ValidHandle(tc.handle) {
			t.Errorf("%s: org.ValidHandle(%q) = true. The subject grammar interpolates "+
				"a handle raw, so this now reaches a subject; see the collision below "+
				"for what that costs",
				tc.name, tc.handle)
		}
	}

	// The collision itself, demonstrated rather than asserted in the
	// abstract. These calls are legal — AgentInbox has no opinion about its
	// argument — and the results are why the caller must not have one either.
	victim := topics.AgentInbox("alice")

	if wildcard := topics.AgentInbox("*"); !topics.Match(wildcard, victim) {
		t.Errorf("expected %q to match %q; the demonstration this test rests on no "+
			"longer holds, so re-derive it before trusting the rule", wildcard, victim)
	}
	if tail := topics.AgentInbox(">"); topics.Match(tail, victim) {
		// `>` is only a wildcard as the FINAL token, so a handle of ">"
		// does not swallow a peer's inbox — it produces a subject that
		// matches nothing but itself. Recorded so the next reader does
		// not assume both wildcards fail the same way.
		t.Errorf("%q matched %q; `>` is documented as final-token-only", tail, victim)
	}
	if dotted, _ := topics.HandleFromInbox(topics.AgentInbox("platform.infra")); dotted != "" {
		t.Errorf("HandleFromInbox recovered %q from a dotted handle's subject; a dot "+
			"makes it two tokens and the subject is no longer a seat's inbox", dotted)
	}
}

// TestDistinctHandlesNeverShareAName is the collision property stated
// positively: distinct seats get distinct subjects and distinct groups, and
// no seat's subject matches another's.
//
// The handles include the near misses a hyphen makes possible, since those
// are where an accidental sharing would actually come from.
func TestDistinctHandlesNeverShareAName(t *testing.T) {
	t.Parallel()

	handles := []string{
		"alice", "alice-2", "a", "ab", "a-b", "0",
		"release", "release-control", "control", "inbox",
		"qa", "qa-lead", "qa-lead-2",
	}
	for _, h := range handles {
		if !org.ValidHandle(h) {
			t.Fatalf("%q is not a handle the org would accept; fix the corpus", h)
		}
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
	for _, h := range handles {
		claim(t, topics.AgentInbox(h), fmt.Sprintf("inbox of %q", h))
		claim(t, topics.AgentControl(h), fmt.Sprintf("control of %q", h))
	}

	for _, a := range handles {
		for _, b := range handles {
			if a == b {
				continue
			}
			if topics.Match(topics.AgentInbox(a), topics.AgentInbox(b)) {
				t.Errorf("seat %q's inbox subject matches seat %q's — %q would take "+
					"%q's mail", a, b, a, b)
			}
			if topics.Match(topics.AgentInbox(a), topics.AgentControl(b)) {
				t.Errorf("seat %q's inbox subject matches seat %q's control subject; a "+
					"sandbox completion would queue behind the pause it exists to lift",
					a, b)
			}
		}
	}
	if len(seen) != 2*len(handles) {
		t.Fatalf("minted %d distinct names for %d handles; the corpus or the walk is wrong",
			len(seen), 2*len(handles))
	}
}
