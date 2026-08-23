package topics_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// TestMatch pins the NATS wildcard grammar the whole engine's subject names
// are written in.
//
// This function is what an in-memory or future backend uses in place of a
// broker's native matcher, so the risk it carries is DISAGREEMENT: a
// subscriber that quietly receives too few events looks exactly like a quiet
// company, and one that receives too many turns a per-domain dashboard filter
// into a firehose. Both are silent.
//
// The table pins what the implementation actually does, which is not
// everywhere what a reader of the doc comment would assume — the cases that
// diverge are commented individually rather than left to be discovered.
func TestMatch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		pattern string
		subject string
		want    bool
	}{
		// A pattern with no wildcard is plain equality.
		{"identical subjects", "crewlet.events.task_created", "crewlet.events.task_created", true},
		{"a different leaf", "crewlet.events.task_created", "crewlet.events.task_failed", false},
		{"a different domain", "crewlet.events.task_created", "crewlet.config.task_created", false},
		{"a single token", "crewlet", "crewlet", true},

		// `*` is exactly one token. The separator is the point: without
		// the length check at the end, crewlet.events.* would also match
		// crewlet.events.task.created and a per-domain filter would
		// silently deepen into a firehose.
		{"`*` takes exactly one token", "crewlet.events.*", "crewlet.events.task_created", true},
		{"`*` does not cross a separator", "crewlet.events.*", "crewlet.events.task.created", false},
		{"`*` needs a token to take", "crewlet.events.*", "crewlet.events", false},
		{"`*` in the middle", "crewlet.agent.*.inbox", "crewlet.agent.alice.inbox", true},
		{"`*` in the middle, wrong tail", "crewlet.agent.*.inbox", "crewlet.agent.alice.control", false},
		{"`*` in the middle does not cross a separator", "crewlet.agent.*.inbox", "crewlet.agent.a.b.inbox", false},
		{"`*` alone takes one token", "*", "crewlet", true},
		{"`*` alone refuses two", "*", "crewlet.events", false},
		{"two `*`s", "*.*", "crewlet.events", true},

		// `>` is ONE OR MORE trailing tokens. Zero does not match: the
		// implementation requires len(sub) > i, so crewlet.events.> does
		// not cover crewlet.events itself. A backend provisioning a
		// stream from a `>` pattern therefore does not cover the bare
		// domain subject, which is a thing to know rather than assume.
		{"`>` takes one", "crewlet.events.>", "crewlet.events.task_created", true},
		{"`>` takes many", "crewlet.events.>", "crewlet.events.task.created.late", true},
		{"`>` does not take zero", "crewlet.events.>", "crewlet.events", false},
		{"`>` still needs the literal head to match", "crewlet.events.>", "crewlet.config.x", false},
		{"`>` alone takes one", ">", "crewlet", true},
		{"`>` alone takes many", ">", "crewlet.events.task_created", true},
		{"`>` after a `*`", "crewlet.*.>", "crewlet.events.task_created", true},

		// `>` is a wildcard only as the FINAL token. Elsewhere the
		// implementation refuses the whole pattern rather than treating
		// it as a literal token — so a malformed pattern matches
		// nothing, which is the safe direction for a subscription.
		{"`>` in the middle matches nothing", "crewlet.>.inbox", "crewlet.agent.inbox", false},
		{"`>` in the middle, however deep", "crewlet.>.inbox", "crewlet.a.b.inbox", false},

		// Token counts. A short pattern does not cover a deeper subject
		// and a long one does not cover a shallower one.
		{"pattern shorter than subject", "crewlet.events", "crewlet.events.task_created", false},
		{"pattern longer than subject", "crewlet.events.task_created", "crewlet.events", false},

		// Wildcards are interpreted on the PATTERN side only. A `*` in
		// the subject is an ordinary token — which is exactly why the
		// handle invariant in handle_invariant_test.go matters: a seat
		// whose handle were "*" would not merely fail to be found, its
		// own inbox subject would become a pattern covering its peers.
		{"a `*` in the subject is literal", "crewlet.agent.alice.inbox", "crewlet.agent.*.inbox", false},
		{"a `*` pattern does cover a `*` subject", "crewlet.agent.*.inbox", "crewlet.agent.*.inbox", true},
		{"a `>` in the subject is literal", "crewlet.events.task_created", "crewlet.events.>", false},

		// Empty segments. A subject with one is what an unroutable handle
		// produces; every backend rejects it before it gets here, so
		// these pin what Match does with an input it should never see
		// rather than an input it must handle.
		{"`*` matches an empty segment", "crewlet.agent.*.inbox", "crewlet.agent..inbox", true},
		{"`>` matches an empty trailing segment", "crewlet.events.>", "crewlet.events.", true},

		// The equality fast path runs BEFORE any parsing, so a pattern
		// always matches itself verbatim — including one the grammar
		// would otherwise refuse. Pinned because it is the one route by
		// which a malformed pattern matches anything at all.
		{"a malformed pattern matches itself", "crewlet.>.inbox", "crewlet.>.inbox", true},
		{"an empty pattern matches an empty subject", "", "", true},

		{"an empty pattern matches nothing else", "", "crewlet.events.x", false},
		{"nothing matches an empty subject", "crewlet.events.>", "", false},
		{"not even `>`", ">", "", false},
	} {
		if got := topics.Match(tc.pattern, tc.subject); got != tc.want {
			t.Errorf("%s: Match(%q, %q) = %v, want %v", tc.name, tc.pattern, tc.subject, got, tc.want)
		}
	}
}

// TestMatchIsSymmetricOnlyByAccident guards the reading error that a
// bidirectional-looking name invites. Match(a, b) is not Match(b, a): the
// first argument is the SUBSCRIPTION and the second is the traffic, and a
// backend that passed them the other way round would silently subscribe
// everything to nothing.
func TestMatchIsSymmetricOnlyByAccident(t *testing.T) {
	t.Parallel()

	pattern, subject := topics.EventsWildcard, topics.Event("task_created")
	if !topics.Match(pattern, subject) {
		t.Fatalf("Match(%q, %q) = false; the rest of this test assumes it holds", pattern, subject)
	}
	if topics.Match(subject, pattern) {
		t.Errorf("Match(%q, %q) = true — arguments are (pattern, subject) and the "+
			"order is load-bearing", subject, pattern)
	}
}
