package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// What the prefetch is told ABOUT a turn, derived from its trigger.
//
// These are the three questions a turn's context rests on — is the trigger a
// pointer, who spoke, and is any of them a colleague — and each has a wrong
// answer that is silent: an ungated search on a pointer returns noise, a
// forgotten speaker loses a profile, and a colleague keyed on a platform id
// splits one person's profile in two.

func notification(source, sender string, meta map[string]string, recon bool) *events.Event {
	return events.New(types.ExternalNotification{
		NotificationSource: source, SourceEventType: "message",
		Sender: sender, Subject: "a message", Body: "hello",
		Metadata: meta, ContextRequiresRecon: recon,
	}, events.NewTrace())
}

// A POINTER TRIGGER gates the three searches that judge relevance against
// the trigger text. ANY constituent, not all: a coalesced trigger carrying
// one webhook that only names a thing-that-changed is still a trigger the
// seat has to go and look behind.
func TestAnyPointerConstituentMakesTheWholeTriggerThin(t *testing.T) {
	t.Parallel()
	substantive := notification("chat", "U1", nil, false)
	pointer := notification("gitlab", "dev", nil, true)

	for _, tc := range []struct {
		name string
		evs  []*events.Event
		want bool
	}{
		{"a substantive trigger", []*events.Event{substantive}, false},
		{"a pointer", []*events.Event{pointer}, true},
		{"a merge with one pointer in it",
			[]*events.Event{substantive, pointer, substantive}, true},
		{"nothing at all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := requiresRecon(tc.evs); got != tc.want {
				t.Fatalf("requiresRecon = %v, want %v", got, tc.want)
			}
		})
	}
}

// A turn is woken by EVENTS, and only some are notifications: a scheduled
// fire, a sandbox completion and an agent-to-agent ask all reach a seat the
// same way and none has a sender or a recon flag.
func TestANonNotificationTriggerContributesNothing(t *testing.T) {
	t.Parallel()
	scheduled := events.New(types.TaskAssigned{TaskID: "t-1"}, events.NewTrace())
	if requiresRecon([]*events.Event{scheduled}) {
		t.Fatal("a scheduled fire was read as a pointer")
	}
	if got := sendersOf(nil, []*events.Event{scheduled}); len(got) != 0 {
		t.Fatalf("senders = %v, want none", got)
	}
}

// EVERY DISTINCT SPEAKER. A coalesced trigger is several people speaking,
// and the flat Sender field mirrors only the latest — so reading it alone
// profiles the last speaker and forgets the other three.
func TestEverySpeakerInACoalescedTriggerIsASender(t *testing.T) {
	t.Parallel()
	merged := events.New(types.ExternalNotification{
		NotificationSource: "chat", Sender: "Ana",
		Metadata: map[string]string{notify.ActorField: "U1"},
		Messages: []types.CoalescedMessage{
			{Sender: "Bo", Metadata: map[string]string{notify.ActorField: "U2"}},
			{Sender: "Cid", Metadata: map[string]string{notify.ActorField: "U3"}},
			// The latest again — one person, one profile.
			{Sender: "Ana", Metadata: map[string]string{notify.ActorField: "U1"}},
		},
	}, events.NewTrace())

	var ids []string
	for _, s := range sendersOf(nil, []*events.Event{merged}) {
		ids = append(ids, s.ExternalID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"U1", "U2", "U3"}) {
		t.Fatalf("senders = %v, want every distinct speaker once", ids)
	}
}

// A COLLEAGUE IS KEYED ON THEIR HANDLE, so what one seat learned about them
// on chat and what it learned on the tracker are ONE profile rather than two
// half-profiles under two platform ids.
func TestAColleagueIsKeyedOnTheirHandleAcrossBackends(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Engineering", Lead: "Tech Lead",
		Roles: []*org.Role{{Name: "Tech Lead", DeclaredHandle: "lead"}},
	}}}
	o.Normalize()
	reg := notify.NewRegistry(o)
	for _, id := range []struct{ ns, external string }{
		{"chat", "U-lead"}, {"gitlab", "lead-bot"},
	} {
		if err := reg.Register(id.ns, id.external, "lead"); err != nil {
			t.Fatalf("register %s: %v", id.external, err)
		}
	}

	onChat := notification("chat", "Tech Lead",
		map[string]string{notify.ActorField: "U-lead"}, false)
	onCodeHost := notification("gitlab", "lead-bot",
		map[string]string{notify.ActorField: "lead-bot"}, false)

	for _, ev := range []*events.Event{onChat, onCodeHost} {
		got := sendersOf(reg, []*events.Event{ev})
		if len(got) != 1 {
			t.Fatalf("senders = %v", got)
		}
		if got[0].Handle != "lead" {
			t.Fatalf("the colleague resolved to %+v, want handle lead", got[0])
		}
	}

	// A STRANGER keeps their platform identity, which is right: an
	// external counterparty has no handle here and their profile is
	// legitimately per-platform.
	stranger := sendersOf(reg, []*events.Event{notification("chat", "Someone",
		map[string]string{notify.ActorField: "U-outsider"}, false)})
	if len(stranger) != 1 || stranger[0].Handle != "" {
		t.Fatalf("a stranger resolved to %+v, want no handle", stranger)
	}
	if stranger[0].ExternalID != "U-outsider" || stranger[0].Platform != "chat" {
		t.Fatalf("a stranger lost their platform identity: %+v", stranger[0])
	}
}

// Every parser stamps the actor, but an engine-authored notification has
// none — the sender field is then all there is, and dropping it would lose
// the counterparty entirely.
func TestASenderWithNoStampedActorStillIdentifies(t *testing.T) {
	t.Parallel()
	got := sendersOf(nil, []*events.Event{notification("chat", "U0FOUNDER", nil, false)})
	if len(got) != 1 || got[0].ExternalID != "U0FOUNDER" {
		t.Fatalf("senders = %+v", got)
	}
}

// An anonymous notification names nobody, and an invalid subject must not
// reach the profile store as a lookup for "".
func TestAnAnonymousTriggerHasNoSenders(t *testing.T) {
	t.Parallel()
	got := sendersOf(nil, []*events.Event{notification("chat", "", nil, false)})
	for _, s := range got {
		if !s.Valid() {
			t.Fatalf("an invalid subject was kept: %+v", s)
		}
	}
	if len(got) != 0 {
		t.Fatalf("senders = %+v, want none", got)
	}
}

var _ = learning.Subject{}

// ── the prefetch's only signal ──

// EVERY BLOCK DEGRADES TO EMPTY BY DESIGN (internal/agent/prefetch), so a
// seat whose diary is unreachable, whose auxiliary model is misconfigured and
// whose knowledge base genuinely has nothing to say all build the same
// prompt. Without this event an operator has no way to tell them apart —
// which is what the type spent its whole life doing: it was registered,
// categorised and documented as "published once per turn", and nothing ever
// published it.
func TestThePrefetchReportsWhatEachBlockSurfaced(t *testing.T) {
	t.Parallel()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	got := make(chan types.PrefetchSummary, 1)
	if err := q.Subscribe(t.Context(), topics.Event("prefetch_summary"), "probe",
		func(_ context.Context, ev *events.Event) queue.Result {
			if p, ok := events.DataAs[*types.PrefetchSummary](ev); ok && p != nil {
				got <- *p
			}
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Through prefetchFor, not the publisher directly: the wire from the
	// turn to the event is the half that was missing, and a test that
	// called the publisher itself would have passed for the whole time
	// nothing did.
	e := &Engine{backends: &Backends{Queue: q}}
	company := &Company{
		Config: &config.Company{},
		Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			{Name: "Tech Lead", DeclaredHandle: "lead"}}},
	}
	e.prefetchFor(t.Context(), company, Request{
		Handle:  "lead",
		WorkKey: "work-1",
		Events:  []*events.Event{notification("gitlab", "dev", nil, true)},
	}, "a pull request got a comment")

	select {
	case summary := <-got:
		switch {
		case summary.RoleName != "Tech Lead" || summary.AgentHandle != "lead":
			t.Errorf("the summary is attributed to %+v", summary)
		case summary.TurnID != "work-1":
			t.Errorf("turn = %q", summary.TurnID)
		// A node with no store and no models renders nothing at all,
		// which is exactly the state this event exists to make visible:
		// a seat running with no memory must not look like a seat whose
		// stores had nothing to say.
		case summary.PersonalMemoryHit || summary.EpisodeRecallHit ||
			summary.CounterpartyHit || summary.SynthesizedSkillsHit:
			t.Errorf("a block reported a hit with no store behind it: %+v", summary)
		case summary.RelevantKnowledgeSelectionCount != 0:
			t.Error("pages were reported with no knowledge backend")
		// READ OFF THE TRIGGER, not off a model: this pointer webhook is
		// what gates three of the six searches, and without the flag its
		// zeroes read as empty stores.
		case !summary.TriggerRequiresRecon:
			t.Error("the gate that skipped three searches was not reported")
		}
	// BOUNDED, not t.Context(): a summary that is never published must
	// fail in a second rather than hanging until the package's own
	// timeout, where it reads as an unrelated suite-wide stall.
	case <-time.After(5 * time.Second):
		t.Fatal("no prefetch summary was published")
	}
}
