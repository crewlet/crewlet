package notify_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
)

func inbound(eventType, actor string) notify.Inbound {
	return notify.Inbound{
		Source: "tracker", EventType: eventType, Subject: "ENG-42",
		Metadata: map[string]string{notify.ActorField: actor},
	}
}

// The loop this exists to stop: a seat assigned to its own issue receives a
// webhook for every comment it posts, and comments again.
func TestASeatIsNotWokenByItsOwnAction(t *testing.T) {
	r := registry(t)
	if err := r.Register("tracker", "acct-lead", "engineering-lead"); err != nil {
		t.Fatalf("register: %v", err)
	}
	lead, _ := r.ByHandle("engineering-lead")

	ok, reason := notify.Deliverable(prompts(), r, inbound("comment", "acct-lead"), lead)
	if ok {
		t.Fatal("a seat was woken by its own comment")
	}
	if reason == "" {
		t.Fatal("the skip carries no reason for an operator to read")
	}

	// Somebody else's comment on the same issue still lands.
	ok, _ = notify.Deliverable(prompts(), r, inbound("comment", "acct-someone"), lead)
	if !ok {
		t.Fatal("a colleague's comment was suppressed")
	}
}

// An agent posts under its BOT id while the same seat is addressed by member
// id. A direct string comparison against the transport namespace misses
// exactly the case that loops.
func TestTheGuardSeesBothSpellingsOfOneSeat(t *testing.T) {
	r := registry(t)
	if err := r.Register("tracker", "member-lead", "engineering-lead"); err != nil {
		t.Fatalf("register member: %v", err)
	}
	if err := r.Register(notify.BotNamespace("tracker"), "bot-lead", "engineering-lead"); err != nil {
		t.Fatalf("register bot: %v", err)
	}
	lead, _ := r.ByHandle("engineering-lead")

	for _, actor := range []string{"member-lead", "bot-lead"} {
		if ok, _ := notify.Deliverable(prompts(), r, inbound("comment", actor), lead); ok {
			t.Fatalf("the seat was woken by its own action as %q", actor)
		}
	}
}

// The exception: an event reporting the OUTCOME of the actor's own action is
// exactly what that actor needs to hear.
func TestAnOutcomeEventStillReachesItsActor(t *testing.T) {
	r := registry(t)
	if err := r.Register("tracker", "acct-lead", "engineering-lead"); err != nil {
		t.Fatalf("register: %v", err)
	}
	lead, _ := r.ByHandle("engineering-lead")

	ok, reason := notify.Deliverable(prompts(), r, inbound("pipeline_failed", "acct-lead"), lead)
	if !ok {
		t.Fatalf("the person who has to fix the build never heard: %s", reason)
	}
	// And the exception is per event type, not per source.
	if ok, _ := notify.Deliverable(prompts(), r, inbound("comment", "acct-lead"), lead); ok {
		t.Fatal("the exception leaked to every event from the source")
	}
}

// An unrecognised source gets the safe answer. A loop takes the company down
// with it; a missed notification costs one notification.
func TestAnUnclassifiedSourceNeverWakesItsActor(t *testing.T) {
	r := registry(t)
	if err := r.Register("mystery", "acct-lead", "engineering-lead"); err != nil {
		t.Fatalf("register: %v", err)
	}
	lead, _ := r.ByHandle("engineering-lead")

	n := inbound("pipeline_failed", "acct-lead")
	n.Source = "mystery"
	if ok, _ := notify.Deliverable(prompts(), r, n, lead); ok {
		t.Fatal("an unclassified source's event woke its own actor")
	}
}

// A human seat is addressable but never woken — a person reads the surface
// the event arrived on, and delivering means spawning a turn for somebody
// who is not an agent.
func TestAHumanSeatIsNeverDelivered(t *testing.T) {
	r := registry(t)
	dana, _ := r.ByHandle("dana-founder")

	ok, reason := notify.Deliverable(prompts(), r, inbound("comment", "somebody"), dana)
	if ok {
		t.Fatal("a turn was going to be spawned for a person")
	}
	if reason == "" {
		t.Fatal("the skip carries no reason")
	}
}

// A node with no company cannot tell whose action this was, and refusing to
// deliver on a guess would drop real work.
func TestWithoutACompanyNothingIsSuppressed(t *testing.T) {
	lead := notify.Party{Handle: "engineering-lead", Name: "Engineering Lead"}
	if notify.SelfAction(nil, "tracker", "acct-lead", lead) {
		t.Fatal("a nil registry claimed a self-action")
	}
	if ok, _ := notify.Deliverable(prompts(), nil, inbound("comment", "acct-lead"), lead); !ok {
		t.Fatal("a node with no company dropped a real notification")
	}
	// And an event that names no actor is nobody's own action.
	r := registry(t)
	if notify.SelfAction(r, "tracker", "", lead) {
		t.Fatal("an unattributed event was called a self-action")
	}
}

// The parse-time face of the same rule, where the identifiers are still the
// vendor's own usernames.
func TestSuppressSelfDropsTheActorAndKeepsTheOrder(t *testing.T) {
	targets := []string{"ana", "bo", "ana", "cy"}

	got := notify.SuppressSelf(targets, "ana", false)
	if !slices.Equal(got, []string{"bo", "cy"}) {
		t.Fatalf("SuppressSelf = %v", got)
	}
	// The same exception: a failed pipeline reaches the person who pushed.
	got = notify.SuppressSelf(targets, "ana", true)
	if !slices.Equal(got, targets) {
		t.Fatalf("the outcome exception dropped the actor anyway: %v", got)
	}
	if got = notify.SuppressSelf(targets, "", false); !slices.Equal(got, targets) {
		t.Fatalf("an unattributed event lost targets: %v", got)
	}
	// "Nobody to tell" is ONE value, so callers cannot disagree about it.
	if got = notify.SuppressSelf([]string{"ana"}, "ana", false); got != nil {
		t.Fatalf("an emptied target list is %#v, want nil", got)
	}
}
