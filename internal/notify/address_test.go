package notify_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/notify"
)

// nativeRule is a backend whose channel types are ITS OWN — the case the
// package-level list could not express, and the one every future backend is.
var nativeRule = notify.AddressRule{
	DirectKinds: []string{"direct", "huddle"},
	Follows:     notify.AddressingFollows(),
}

// nativeMeta is one delivery from that backend.
func nativeMeta(mutate func(map[string]string)) map[string]string {
	m := map[string]string{
		notify.TransportField:   "native",
		notify.ChannelField:     "c-42",
		notify.ChannelTypeField: "room",
		notify.MessageIDField:   "m-7",
	}
	if mutate != nil {
		mutate(m)
	}
	return m
}

// ONE RULE DECIDES WHO IS ADDRESSED, and it is asserted BY CONSTRUCTION:
// the prompt, the working indicator and the delivery gate are handed the
// same value and there is no second place for one of them to ask.
//
// They disagreed before this: the indicator and the gate read a
// package-level list of channel types while the prompt read the backend's
// own, so a backend whose vocabulary was not in that list — the engine's
// native chat, on day one — was in a DIRECT conversation for the
// conversation key and NOT ADDRESSED for the reply obligation, for the same
// message.
func TestOneRuleDecidesWhoIsAddressed(t *testing.T) {
	t.Parallel()
	prompt := notify.ChatPrompt{Backend: "native", Label: "Native", Address: nativeRule}
	indicator := notify.NewStatusDriver(notify.StatusOptions{
		Poster: &poster{backend: "native", text: true, refresh: time.Minute, rule: nativeRule},
		Mode:   notify.StatusAddressed,
	})
	t.Cleanup(func() { indicator.Stop(context.Background()) })

	cases := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"a direct conversation", nativeMeta(func(m map[string]string) {
			m[notify.ChannelTypeField] = "direct"
		}), true},
		{"this backend's second private kind", nativeMeta(func(m map[string]string) {
			m[notify.ChannelTypeField] = "huddle"
		}), true},
		{"a message naming the seat", nativeMeta(func(m map[string]string) {
			m[notify.FollowReasonField] = string(notify.FollowMention)
		}), true},
		{"a thread it is in because it was named", nativeMeta(func(m map[string]string) {
			m[notify.ThreadField] = "m-1"
			m[notify.FollowingField] = string(notify.FollowMention)
		}), true},
		{"a room message addressed to nobody", nativeMeta(nil), false},
		{"a broadcast", nativeMeta(func(m map[string]string) {
			m[notify.FollowReasonField] = string(notify.FollowCollective)
		}), false},
	}
	for _, c := range cases {
		gate := prompt.Addressed(notify.Inbound{Source: "native", Metadata: c.meta})
		raised := indicator.Begin(t.Context(), "swe", "turn-1", "plan", c.meta) != nil
		if gate != c.want || raised != c.want {
			t.Errorf("%s: gate = %v, indicator = %v, want %v", c.name, gate, raised, c.want)
		}
		if got := prompt.Address.Addressed(c.meta); got != c.want {
			t.Errorf("%s: the rule itself says %v, want %v", c.name, got, c.want)
		}
	}
}

// A BACKEND'S DIRECTNESS IS ITS OWN, and both readers of it must agree:
// "direct" is not in any shipped backend's vocabulary, and the conversation
// key collapses to the channel for exactly the messages the rule calls
// direct — which is what makes a person's typing burst one turn instead of
// four.
func TestABackendsOwnChannelTypeIsDirectToEveryReader(t *testing.T) {
	t.Parallel()
	prompt := notify.ChatPrompt{Backend: "native", Address: nativeRule}
	direct := nativeMeta(func(m map[string]string) { m[notify.ChannelTypeField] = "direct" })

	if !prompt.Address.IsDirect(direct) {
		t.Fatal("the backend's own direct kind is not read as direct")
	}
	if !prompt.Addressed(notify.Inbound{Source: "native", Metadata: direct}) {
		t.Fatal("a direct conversation does not oblige an answer")
	}
	if got := prompt.PartitionKey(direct, ""); got != "c-42" {
		t.Fatalf("a direct conversation keys on %q, want the channel alone", got)
	}
	// And a room on that same backend is neither.
	room := nativeMeta(nil)
	if prompt.Address.IsDirect(room) {
		t.Fatal("a room is read as direct")
	}
	if got := prompt.PartitionKey(room, ""); got != "c-42:m-7" {
		t.Fatalf("a room message keys on %q, want channel:anchor", got)
	}
}

// A TRIGGER IS NOT A STANDING FOLLOW. Being in a thread is not being asked
// something in it: a `thread_following` marker read as an obligation made
// every later reply in every thread a seat had ever spoken in a turn that
// may not end in silence.
func TestAStandingFollowIsNotAnAsk(t *testing.T) {
	t.Parallel()
	for _, reason := range []notify.FollowReason{notify.FollowMention, notify.FollowExplicit} {
		if !reason.Addresses() {
			t.Errorf("%q does not address the seat", reason)
		}
	}
	for _, reason := range []notify.FollowReason{notify.FollowCollective, notify.FollowParticipated} {
		if reason.Addresses() {
			t.Errorf("%q addresses the seat", reason)
		}
		meta := nativeMeta(func(m map[string]string) {
			m[notify.ThreadField] = "m-1"
			m[notify.FollowReasonField] = string(reason)
			m[notify.FollowingField] = string(reason)
		})
		if nativeRule.Addressed(meta) {
			t.Errorf("a reply riding a %q follow obliges an answer", reason)
		}
	}
	// The set a rule is built from is the same answer, derived rather than
	// written out twice.
	for _, reason := range notify.AddressingFollows() {
		if !reason.Addresses() {
			t.Errorf("AddressingFollows offers %q, which does not address", reason)
		}
	}
	if got := len(notify.AddressingFollows()); got != 2 {
		t.Fatalf("AddressingFollows has %d reasons, want mention and explicit", got)
	}
}

// A REASON OFF THE WIRE IS A VALUE, not a panic: a follow row or a
// notification may carry a reason written by a build that knew one this one
// does not, which a rolling upgrade guarantees.
func TestAFollowReasonReportsWhetherTheEngineKnowsIt(t *testing.T) {
	t.Parallel()
	for _, reason := range notify.AddressingFollows() {
		if !reason.Valid() {
			t.Errorf("%q is not a reason the engine knows", reason)
		}
	}
	for _, reason := range []notify.FollowReason{"", "mentioned", "follow_all"} {
		if reason.Valid() {
			t.Errorf("%q reads as a known reason", reason)
		}
		if reason.Addresses() {
			t.Errorf("%q addresses the seat", reason)
		}
	}
}

// THE ZERO RULE ADDRESSES NOTHING, which is the conservative half of the
// contract: a seat wrongly told nobody is waiting keeps the freedom to stay
// silent, while one wrongly told somebody is must post on every broadcast it
// observes.
func TestTheZeroRuleAddressesNothing(t *testing.T) {
	t.Parallel()
	var rule notify.AddressRule
	for _, meta := range []map[string]string{
		nativeMeta(nil),
		nativeMeta(func(m map[string]string) { m[notify.ChannelTypeField] = "direct" }),
		nativeMeta(func(m map[string]string) {
			m[notify.FollowReasonField] = string(notify.FollowMention)
		}),
		{},
	} {
		if rule.Addressed(meta) {
			t.Errorf("a rule that declares nothing addressed %v", meta)
		}
	}
	// And an EMPTY channel type is "the backend did not say", never a
	// kind — a rule that listed the empty string would otherwise read
	// every room as a direct message.
	empty := notify.AddressRule{DirectKinds: []string{""}}
	if empty.IsDirect(nativeMeta(func(m map[string]string) {
		delete(m, notify.ChannelTypeField)
	})) {
		t.Fatal("a message with no channel type was read as direct")
	}
}

// THE DM PREFIX IS OPT-IN, because a backend whose ids are opaque would
// otherwise mark arbitrary public channels as direct messages — and then
// every seat in them owes an answer to traffic nobody addressed to any of
// them.
func TestTheChannelIDPrefixIsOptIn(t *testing.T) {
	t.Parallel()
	opaque := nativeRule
	prefixed := notify.AddressRule{
		DirectKinds: slices.Clone(nativeRule.DirectKinds),
		DMPrefix:    "D",
		Follows:     notify.AddressingFollows(),
	}
	meta := nativeMeta(func(m map[string]string) { m[notify.ChannelField] = "D0ANA" })

	if !prefixed.IsDirect(meta) {
		t.Fatal("a declared prefix did not answer")
	}
	if opaque.IsDirect(meta) {
		t.Fatal("a backend that declared no prefix used one anyway")
	}
}
