package jetstream

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A DERIVED NAME IS A DURABLE'S IDENTITY ON A BROKER TWO BUILDS SHARE, so the
// construction behind it may not move for any consumer that can already exist.
//
// Each of these is what the three derivations answered before they were built
// by one function, pinned as literals: a construction that renamed any of them
// would, under a rolling upgrade, have the new build create a second consumer
// beside the old one — two mailboxes for one seat, or a state-log reader that
// starts again from the beginning. The long cases are the ones a cut reaches,
// which is where a change to the escape, the cut or the marker would show.
func TestADerivedConsumerNameIsUnchangedForEveryConsumerThatCanAlreadyExist(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("h", 120)
	for _, tc := range []struct {
		name, got, want string
	}{
		{"a seat inbox", consumerName("crewlet.agent.alice.inbox", "agent-alice"),
			"agent-alice__crewlet_agent_alice_inbox__5d6381d3b0e8"},
		{"a seat inbox with a long handle, cut", consumerName("crewlet.agent."+long+".inbox", "agent-"+long),
			"agent-" + long + "__crewlet_agent_" + strings.Repeat("h", 24) + "__f9fb13110bfc"},
		{"a node's reader", domainConsumerName("CREWLET_TRACKER_LOG", "node-0"),
			"statelog__CREWLET_TRACKER_LOG__node-0__8a951e181bef"},
		{"a node's reader with the longest node id", domainConsumerName("CREWLET_TRACKER_LOG", strings.Repeat("n", 64)),
			"statelog__CREWLET_TRACKER_LOG__" + strings.Repeat("n", 64) + "__0dbfdc1e581e"},
		{"a group", domainGroupName("CREWLET_TRACKER_LOG", "tracker-wakes"),
			"tracker-wakes__CREWLET_TRACKER_LOG__d29fc121747d"},
		{"a group whose name is escaped", domainGroupName("CREWLET_TRACKER_LOG", "group."+long+" x*y>z"),
			"group_" + long + "_x_y_z__CREWLET_TRACKER_LOG__2f962fd32b8a"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: derived %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// A LABEL IS CUT ON A RUNE BOUNDARY, and the whole name stays inside its bound.
//
// A byte slice through a multi-byte character leaves invalid UTF-8 in a name
// the broker also uses for the consumer's directory on disk; the digest still
// separates two identities whose labels cut identically.
func TestADerivedLabelIsCutOnARuneBoundary(t *testing.T) {
	t.Parallel()
	// Two bytes a character, placed so that on each derivation the label's
	// last byte in the bound is the FIRST byte of one: a byte slice there
	// leaves half a character.
	wide := "x" + strings.Repeat("é", consumerNameMax)
	for name, derived := range map[string]string{
		"subscription":  consumerName("t.x", wide),
		"node's reader": domainConsumerName(wide, "node-0"),
		"group":         domainGroupName("CREWLET_X_LOG", wide),
	} {
		if !utf8.ValidString(derived) {
			t.Errorf("%s: the name %q is not valid UTF-8", name, derived)
		}
		if len(derived) > consumerNameMax {
			t.Errorf("%s: the name is %d bytes, past consumerNameMax (%d)", name, len(derived), consumerNameMax)
		}
		if want := 2 * consumerDigestBytes; !strings.HasSuffix(derived[:len(derived)-want], consumerNameSep) {
			t.Errorf("%s: the name %q does not end in a %d-character digest", name, derived, want)
		}
	}
	if consumerName("t.x", wide+"a") == consumerName("t.x", wide+"b") {
		t.Error("two groups whose labels cut identically share a consumer")
	}
}

// EVERY CHARACTER THE BROKER REFUSES IN A NAME IS ESCAPED, so a subscription or
// a group whose name holds one is created rather than refused by the broker
// with an error that names neither.
//
// Through the REAL broker, because the refused set is the server's: a list
// here that merely agreed with itself would prove nothing about what a create
// accepts.
func TestEveryDerivedNameIsOneTheBrokerAccepts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	awkward := "team/a\\b\tc\rd\ne\ff.g*h>i j"

	q := openForTest(t, Config{})
	if _, err := q.EnsureSubscription(ctx, "escapes.path/to\\thing", awkward); err != nil {
		t.Fatalf("EnsureSubscription with %q: %v", awkward, err)
	}

	logs := domainQueue(t, probeDomain())
	log, err := logs.DomainLog(ctx, probeDomain().Name)
	if err != nil {
		t.Fatalf("DomainLog: %v", err)
	}
	group, err := log.Group(ctx, awkward)
	if err != nil {
		t.Fatalf("Group %q: %v", awkward, err)
	}
	_ = group.Stop()
}
