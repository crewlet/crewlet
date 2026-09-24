package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
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
	if got := threadOf([]*events.Event{scheduled}); got.Root != "" {
		t.Fatalf("a scheduled fire named thread %+v", got)
	}
	a2a := events.New(types.ExternalNotification{
		NotificationSource: "a2a", Sender: "lead", Body: "can you check staging",
	}, events.NewTrace())
	if got := threadOf([]*events.Event{a2a}); got.Root != "" {
		t.Fatalf("an agent-to-agent ask named thread %+v", got)
	}
}

// THE THREAD COMES OFF THE TRIGGER'S OWN METADATA, and only where there is
// one to come off.
//
// A top-level chat message has no earlier conversation: reading a "thread"
// rooted at it returns the triggering message, which the turn was already
// handed — so every top-level message in the company would spend an HTTP
// request at turn start to re-read its own trigger.
func TestTheThreadComesOffTheTriggersOwnMetadata(t *testing.T) {
	t.Parallel()
	chat := func(over map[string]string) map[string]string {
		m := map[string]string{
			"transport": "slack", "channel": "C0ENG",
			"ts": "1700000002.000100", "thread_ts": "1700000001.000100",
		}
		for k, v := range over {
			if v == "" {
				delete(m, k)
				continue
			}
			m[k] = v
		}
		return m
	}
	for name, tc := range map[string]struct {
		metadata map[string]string
		want     notify.Thread
	}{
		"a thread reply": {chat(nil), notify.Thread{
			Backend: "slack", Channel: "C0ENG", Root: "1700000001.000100"}},
		"a top-level message": {chat(map[string]string{"thread_ts": ""}), notify.Thread{}},
		"a webhook":           {map[string]string{"issue_key": "ENG-42"}, notify.Thread{}},
		"nothing at all":      {nil, notify.Thread{}},
	} {
		got := threadOf([]*events.Event{notification("chat", "U1", tc.metadata, false)})
		if got != tc.want {
			t.Errorf("%s resolved to %+v, want %+v", name, got, tc.want)
		}
	}
}

// A COALESCED BURST IS ONE THREAD, and it is the flat metadata's.
//
// A chat partition is thread-grained wherever a thread exists — a direct
// conversation partitions on the bare channel, but only for messages with no
// thread at all — so a coalesced burst can never straddle two threads, and
// the flat fields (which mirror the latest constituent) are the whole answer.
func TestACoalescedChatBurstResolvesToOneThread(t *testing.T) {
	t.Parallel()
	meta := map[string]string{
		"transport": "slack", "channel": "C0ENG",
		"ts": "1700000009.000100", "thread_ts": "1700000001.000100",
	}
	merged := events.New(types.ExternalNotification{
		NotificationSource: "chat", Sender: "Ana", Metadata: meta,
		Messages: []types.CoalescedMessage{
			{Sender: "Bo", Metadata: meta}, {Sender: "Ana", Metadata: meta},
		},
	}, events.NewTrace())

	got := threadOf([]*events.Event{merged})
	if got.Root != "1700000001.000100" || got.Channel != "C0ENG" || got.Backend != "slack" {
		t.Fatalf("a coalesced burst resolved to %+v", got)
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
		RunID:   "run-1",
		WorkKey: "work-1",
		Events:  []*events.Event{notification("gitlab", "dev", nil, true)},
	}, "a pull request got a comment")

	select {
	case summary := <-got:
		switch {
		case summary.RoleName != "Tech Lead" || summary.AgentHandle != "lead":
			t.Errorf("the summary is attributed to %+v", summary)
		// THE RUN, so the summary sits under the same id every phase
		// record of this turn does. Under the work key it landed on an
		// id no phase shares — invisible to the turn view. See ADR-0017.
		case summary.TurnID != "run-1":
			t.Errorf("turn = %q, want the run this prefetch was assembled for",
				summary.TurnID)
		// A node with no store and no models renders nothing at all,
		// which is exactly the state this event exists to make visible:
		// a seat running with no memory must not look like a seat whose
		// stores had nothing to say.
		case summary.PersonalMemoryHit || summary.EpisodeRecallHit ||
			summary.CounterpartyHit || summary.SynthesizedSkillsHit:
			t.Errorf("a block reported a hit with no store behind it: %+v", summary)
		case summary.RelevantKnowledgeSelectionCount != 0:
			t.Error("pages were reported with no knowledge backend")
		// A WEBHOOK NAMES NO THREAD, so the seventh block reports nothing
		// at all — not a thread it could not read, and not a read that
		// found nothing. Those three are what tell "this seat was handed
		// the conversation" from "it was told to go and find it" from
		// "there was no conversation".
		case summary.ThreadContextHit || summary.ThreadContextBytes != 0 ||
			summary.ThreadContextPosts != 0 || summary.ThreadContextRead ||
			summary.ThreadContextStoppedShort:
			t.Errorf("a non-chat trigger reported a thread: %+v", summary)
		// READ OFF THE TRIGGER, not off a model: this pointer webhook is
		// what gates three of the seven searches, and without the flag its
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

// AND A CHAT THREAD REPORTS WHAT IT WAS HANDED.
//
// Hit, bytes, the message count and whether a backend ANSWERED are four
// different facts, and no three of them imply the fourth: both of the block's
// zero-message paths render a non-empty hint, so hit and bytes look identical
// on a thread that was read and empty and on one that could not be read at
// all. Reported as a count alone, the first was published to the dashboard as
// the second — a healthy node claiming it could not reach its own chat
// surface.
func TestTheThreadBlockReportsWhatItWasHanded(t *testing.T) {
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

	e := &Engine{backends: &Backends{Queue: q}}
	company := &Company{
		Config: &config.Company{},
		Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			{Name: "Tech Lead", DeclaredHandle: "lead"}}},
	}
	// A node with no chat transport at all: ChatThreads is empty, the read
	// reports false, and the block renders the unreadable hint. That is the
	// maintenance-mode and unreachable-instance case, and it has to be
	// VISIBLE rather than looking like a thread nobody wrote in.
	e.prefetchFor(t.Context(), company, Request{
		Handle:  "lead",
		WorkKey: "work-2",
		Events: []*events.Event{notification("chat", "U1", map[string]string{
			"transport": "slack", "channel": "C0ENG",
			"ts": "1700000002.000100", "thread_ts": "1700000001.000100",
		}, true)},
	}, "+1")

	select {
	case summary := <-got:
		if !summary.ThreadContextHit || summary.ThreadContextBytes == 0 {
			t.Errorf("a thread reply reported no block at all: %+v", summary)
		}
		if summary.ThreadContextPosts != 0 {
			t.Errorf("a node with no chat reader reported %d messages",
				summary.ThreadContextPosts)
		}
		if summary.ThreadContextRead {
			t.Errorf("a node with no chat reader reported the thread as read: %+v",
				summary)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no prefetch summary was published")
	}
}

// AND A THREAD THAT WAS READ REACHES THE EVENT AS ONE.
//
// The block's own answer and the event's fields are two structs copied across
// by hand, with nothing holding them together: a field left out here is a
// block reporting correctly into a summary that does not carry it, and every
// path above reads false — a healthy read published as a node that could not
// reach its chat surface, and a thread truncated at the newest end published
// as a whole one. Exercised against the mapping directly, because a node's
// chat readers are its running transports and a fake cannot be one.
func TestTheSummaryCarriesEveryThreadFact(t *testing.T) {
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

	e := &Engine{backends: &Backends{Queue: q}}
	seat := &org.Role{Name: "Tech Lead", DeclaredHandle: "lead"}
	began := time.Date(2026, 9, 24, 9, 30, 0, 0, time.UTC)
	e.publishPrefetchSummary(t.Context(), seat, "agent-1", "run-3", "work-3",
		prefetch.Request{}, prefetch.Blocks{
			ThreadContext:             "- **Ana Ruiz (ana)**: staging redirects in a loop",
			ThreadContextPosts:        12,
			ThreadContextRead:         true,
			ThreadContextStoppedShort: true,
		}, began, 1850*time.Millisecond)

	select {
	case summary := <-got:
		if !summary.ThreadContextHit || summary.ThreadContextBytes == 0 {
			t.Errorf("a rendered thread reported no block: %+v", summary)
		}
		if summary.ThreadContextPosts != 12 {
			t.Errorf("the message count arrived as %d", summary.ThreadContextPosts)
		}
		if !summary.ThreadContextRead {
			t.Error("a thread that was read arrived as one that could not be")
		}
		if !summary.ThreadContextStoppedShort {
			t.Error("a read that stopped short arrived as a complete one")
		}
		if !summary.StartedAt.Equal(began) || summary.DurationMS != 1850 {
			t.Errorf("the assembly's timing arrived as %v / %dms, want %v / 1850ms",
				summary.StartedAt, summary.DurationMS, began)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no prefetch summary was published")
	}
}

// THE CONTEXT ASSEMBLY IS TIMED WHERE IT RUNS.
//
// It is the stretch between a turn announcing itself and its first phase
// opening — a diary, a thread, a knowledge base, an auxiliary model for two of
// them — and without its own measurement a turn's timeline had a gap there that
// nothing on the record could explain.
func TestPrefetchSummaryIsTimed(t *testing.T) {
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

	e := &Engine{backends: &Backends{Queue: q}}
	company := &Company{
		Config: &config.Company{},
		Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			{Name: "Tech Lead", DeclaredHandle: "lead"}}},
	}
	before := time.Now().UTC()
	e.prefetchFor(t.Context(), company, Request{
		Handle: "lead", RunID: "run-4", WorkKey: "work-4",
		Events: []*events.Event{notification("gitlab", "dev", nil, true)},
	}, "a pull request got a comment")
	after := time.Now().UTC()

	select {
	case summary := <-got:
		if summary.StartedAt.IsZero() || summary.StartedAt.Location() != time.UTC ||
			summary.StartedAt.Before(before) || summary.StartedAt.After(after) {
			t.Errorf("started_at = %v, want a UTC instant inside the call [%v, %v]",
				summary.StartedAt, before, after)
		}
		if limit := int(after.Sub(before) / time.Millisecond); summary.DurationMS < 0 || summary.DurationMS > limit {
			t.Errorf("duration_ms = %d, want a measurement inside the call's own %dms",
				summary.DurationMS, limit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no prefetch summary was published")
	}
}
