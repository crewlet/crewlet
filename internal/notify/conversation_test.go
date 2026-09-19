package notify_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
)

func evOf(t *testing.T, payload map[string]any) *events.Event {
	t.Helper()
	ev := events.New(types.ExternalNotification{NotificationSource: "chat"}, events.TraceContext{})
	for k, v := range payload {
		if ev.Payload == nil {
			ev.Payload = map[string]any{}
		}
		ev.Payload[k] = v
	}
	return ev
}

// An event that cannot name its conversation must never merge with another,
// and a SHARED fallback would merge every one of them into a single turn.
func TestTheFallbackIsUniquePerEvent(t *testing.T) {
	a, b := evOf(t, nil), evOf(t, nil)
	ka, kb := notify.KeyOf(a), notify.KeyOf(b)
	if ka == kb {
		t.Fatalf("two unrelated events share the key %q — they would coalesce into one turn", ka)
	}
	for _, k := range []string{ka, kb} {
		if !strings.HasPrefix(k, notify.EventPrefix) {
			t.Fatalf("fallback %q is not namespaced", k)
		}
	}
}

// A stamped key is what the producer meant; the broker must not second-guess.
func TestAStampedKeyWins(t *testing.T) {
	ev := evOf(t, nil)
	notify.Stamp(ev, "chat:C1:1718.003", "chat:C1:1718.003")
	if got := notify.KeyOf(ev); got != "chat:C1:1718.003" {
		t.Fatalf("KeyOf = %q, want the stamped key", got)
	}
}

// An absent field and an empty one would read the same to every reader, so an
// empty key is not stamped at all and the fallback stays in one place.
func TestAnEmptyKeyIsNotStamped(t *testing.T) {
	ev := evOf(t, nil)
	notify.Stamp(ev, "", "")
	if _, present := ev.Payload[notify.PartitionField]; present {
		t.Fatal("an empty key was stamped, so the fallback is now bypassed by an empty string")
	}
	if _, present := ev.Payload[notify.ConversationField]; present {
		t.Fatal("an empty identity was stamped, so the fallback is now bypassed by an empty string")
	}
	if !strings.HasPrefix(notify.KeyOf(ev), notify.EventPrefix) {
		t.Fatal("the fallback did not apply")
	}
	// BOTH READS FALL BACK TO THE SAME VALUE, which is what keeps
	// [notify.Derived] gating recording and the answerable-in-chat column
	// identically: an event that can be "derived" under one key and not the
	// other writes a row no later message can read back.
	if notify.ConversationIdentityOf(ev) != notify.KeyOf(ev) {
		t.Fatalf("an event naming neither key fell back to %q and %q",
			notify.KeyOf(ev), notify.ConversationIdentityOf(ev))
	}
}

func TestStampingAnEventWithNoPayloadWorks(t *testing.T) {
	ev := events.New(types.ExternalNotification{}, events.TraceContext{})
	ev.Payload = nil
	notify.Stamp(ev, "chat:C1", "chat:C1")
	if got := notify.KeyOf(ev); got != "chat:C1" {
		t.Fatalf("KeyOf = %q", got)
	}
}

// Two third-party apps mint the same local key routinely (a work item and an
// issue are both plausibly "42"), and an un-namespaced key merges their
// events.
func TestALocalKeyIsNamespacedByItsSource(t *testing.T) {
	if a, b := notify.Namespaced("jira", "42"), notify.Namespaced("gitlab", "42"); a == b {
		t.Fatalf("two sources produced the same key %q", a)
	}
	if got := notify.Namespaced("jira", "42"); got != "jira:42" {
		t.Fatalf("Namespaced = %q", got)
	}
	// Half a key is no key: namespacing an empty local half would produce
	// "jira:", which every event from that source would share.
	if got := notify.Namespaced("jira", ""); got != "" {
		t.Fatalf("Namespaced with no local key = %q, want empty", got)
	}
	if got := notify.Namespaced("", "42"); got != "" {
		t.Fatalf("Namespaced with no source = %q, want empty", got)
	}
}

// The question a parked sandbox run asks before telling somebody to reply in
// a thread that may not exist.
func TestDerivedTellsAReplyableConversationFromAnEventFallback(t *testing.T) {
	cases := map[string]bool{
		"chat:C1:1718.003": true,
		"jira:ENG-42":      true,
		"event:018f-abc":   false,
		"":                 false,
	}
	for key, want := range cases {
		if got := notify.Derived(key); got != want {
			t.Fatalf("Derived(%q) = %v, want %v", key, got, want)
		}
	}
}

// Every event in a partition shares a key by construction, so a later one
// naming a different key is a routing bug — and taking the first keeps the
// answer stable rather than depending on which event sorted last.
func TestAPartitionsKeyIsItsFirstStampedOne(t *testing.T) {
	first, second := evOf(t, nil), evOf(t, nil)
	notify.Stamp(first, "chat:C1", "chat:C1")
	notify.Stamp(second, "chat:C2", "chat:C2")
	if got := notify.KeyOfAll([]*events.Event{first, second}); got != "chat:C1" {
		t.Fatalf("KeyOfAll = %q, want the first", got)
	}
	// A partition that stamped nothing has no key — the caller decides what
	// that means, rather than being handed one event's fallback as if it
	// described the whole partition.
	if got := notify.KeyOfAll([]*events.Event{evOf(t, nil)}); got != "" {
		t.Fatalf("KeyOfAll with nothing stamped = %q, want empty", got)
	}
}

func TestNilEventsAreSurvivable(t *testing.T) {
	if got := notify.KeyOf(nil); got != "" {
		t.Fatalf("KeyOf(nil) = %q", got)
	}
	if got := notify.KeyOfAll([]*events.Event{nil, nil}); got != "" {
		t.Fatalf("KeyOfAll(nils) = %q", got)
	}
	if got := notify.ConversationIdentityOf(nil); got != "" {
		t.Fatalf("ConversationIdentityOf(nil) = %q", got)
	}
	if got := notify.ConversationIdentityOfAll([]*events.Event{nil, nil}); got != "" {
		t.Fatalf("ConversationIdentityOfAll(nils) = %q", got)
	}
	notify.Stamp(nil, "chat:C1", "chat:C1") // must not panic
}

// THE TWO FIELDS ARE INDEPENDENT ON THE WIRE, because the one case they exist
// for is the one where they differ: a direct message's thread reply partitions
// on the thread and belongs to the whole channel.
func TestBothKeysRoundTripIndependently(t *testing.T) {
	ev := evOf(t, nil)
	notify.Stamp(ev, "chat:D1:root-1", "chat:D1")
	if got := notify.KeyOf(ev); got != "chat:D1:root-1" {
		t.Errorf("the broker would partition this on %q", got)
	}
	if got := notify.ConversationIdentityOf(ev); got != "chat:D1" {
		t.Errorf("the ledger would key this on %q", got)
	}
	if got := notify.KeyOfAll([]*events.Event{ev}); got != "chat:D1:root-1" {
		t.Errorf("KeyOfAll = %q", got)
	}
	if got := notify.ConversationIdentityOfAll([]*events.Event{ev}); got != "chat:D1" {
		t.Errorf("ConversationIdentityOfAll = %q", got)
	}
}

// AN OLDER PEER'S EVENT CARRIES ONE FIELD, and the identity read has to fall
// back to it. CREWLET_AGENT is interest retention with no maxAge and a parked
// seat republishes for hours, so an event stamped by a build from before the
// split reaches this one whenever — and reading the absence as "no
// conversation" would refuse to record the turn at all, losing history a
// person can see, where the fallback merely reproduces that build's own
// answer.
func TestAnEventFromBeforeTheSplitStillNamesAConversation(t *testing.T) {
	ev := evOf(t, map[string]any{notify.PartitionField: "chat:D1:root-1"})
	if got := notify.ConversationIdentityOf(ev); got != "chat:D1:root-1" {
		t.Errorf("a single-field event resolved to %q, want the field it carries", got)
	}
	if got := notify.ConversationIdentityOfAll([]*events.Event{ev}); got != "chat:D1:root-1" {
		t.Errorf("the partition resolved to %q", got)
	}
	// And the PARTITION read is untouched by the split, which is the half a
	// mixed fleet cannot afford to get wrong: an unkeyed wake is a partition
	// of one, so ten comments on one thread wake a seat ten times.
	if got := notify.KeyOf(ev); got != "chat:D1:root-1" {
		t.Errorf("the partition function reads %q", got)
	}
}

// THE NEW FIELD WINS WHERE BOTH ARE PRESENT, or the fallback above would make
// the split unobservable on every event that carries it.
func TestTheStampedIdentityBeatsTheFallback(t *testing.T) {
	ev := evOf(t, map[string]any{
		notify.PartitionField:    "chat:D1:root-1",
		notify.ConversationField: "chat:D1",
	})
	if got := notify.ConversationIdentityOf(ev); got != "chat:D1" {
		t.Fatalf("identity = %q, want the stamped one rather than the partition", got)
	}
}

// THE QUEUE MINTS THE SAME FALLBACK NAMESPACE THIS PACKAGE DOES, and nothing
// else pins the two spellings together: internal/queue holds its own hardcoded
// copy of "event:" for a key function that answered empty, and it cannot
// import this package (notify imports queue). Rename [notify.EventPrefix]
// alone and the broker goes on minting the old namespace — which [Derived]
// then reads as a real conversation, so a parked run is offered as answerable
// in a thread that does not exist.
func TestTheQueuesOwnFallbackSharesThisNamespace(t *testing.T) {
	ev := evOf(t, nil)
	parts := queue.PartitionByKey([]*events.Event{ev},
		func(*events.Event) string { return "" },
		func(e *events.Event) *events.Event { return e })
	if len(parts) != 1 {
		t.Fatalf("one event made %d partitions", len(parts))
	}
	if got := parts[0].Key; got != notify.Fallback(ev.ID.String()) {
		t.Fatalf("the queue minted %q and this package mints %q", got,
			notify.Fallback(ev.ID.String()))
	}
	if notify.Derived(parts[0].Key) {
		t.Fatal("the queue's fallback reads as a real conversation, so a parked run " +
			"would be offered as answerable in a thread nobody can reach")
	}
}
