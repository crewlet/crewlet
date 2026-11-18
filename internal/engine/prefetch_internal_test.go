package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// What the prefetch is told ABOUT a turn, from its trigger: is it a pointer,
// who spoke, is any a colleague. Each has a silent wrong answer: an ungated
// search on a pointer returns noise, a forgotten speaker loses a profile, and
// a colleague keyed on a platform id splits one profile in two.

func notification(source, sender string, meta map[string]string, recon bool) *events.Event {
	return events.New(types.ExternalNotification{
		NotificationSource: source, SourceEventType: "message",
		Sender: sender, Subject: "a message", Body: "hello",
		Metadata: meta, ContextRequiresRecon: recon,
	}, events.NewTrace())
}

// A POINTER TRIGGER gates the three searches judged against the trigger text,
// and ANY pointer constituent makes the whole coalesced trigger one.
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

// A waking event that is no notification (a scheduled fire, an agent-to-agent
// ask) has no sender, recon flag or thread.
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
// one: a "thread" rooted at a top-level message is the trigger itself, which
// every such turn would spend a request to re-read.
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

// A COALESCED BURST IS ONE THREAD, the flat metadata's: a chat partition is
// thread-grained, so a burst never straddles two (see threadOf).
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

// EVERY DISTINCT SPEAKER of a coalesced trigger, not just the flat Sender,
// which mirrors only the latest.
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

// A COLLEAGUE IS KEYED ON THEIR HANDLE, so chat and the tracker build ONE
// profile, not two under two platform ids.
func TestAColleagueIsKeyedOnTheirHandleAcrossBackends(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Engineering", Lead: "Tech Lead",
		Roles: []*org.Role{{Name: "Tech Lead", DeclaredHandle: "lead"}},
	}}}
	o.Normalize()
	reg := notify.NewRegistry(o, nil)
	for _, id := range []struct{ ns, external string }{
		{"chat", "U-lead"}, {"gitlab", "lead-bot"},
	} {
		if err := reg.Register(id.ns, id.external, "lead"); err != nil {
			t.Fatalf("register %s: %v", id.external, err)
		}
	}

	onChat := notification("chat", "Tech Lead",
		map[string]string{notify.ActorField: "U-lead"}, false)
	onGitletHost := notification("gitlab", "lead-bot",
		map[string]string{notify.ActorField: "lead-bot"}, false)

	for _, ev := range []*events.Event{onChat, onGitletHost} {
		got := sendersOf(reg, []*events.Event{ev})
		if len(got) != 1 {
			t.Fatalf("senders = %v", got)
		}
		if got[0].Handle != "lead" {
			t.Fatalf("the colleague resolved to %+v, want handle lead", got[0])
		}
	}

	// A STRANGER keeps their platform identity: no handle here, so their
	// profile is legitimately per-platform.
	stranger := sendersOf(reg, []*events.Event{notification("chat", "Someone",
		map[string]string{notify.ActorField: "U-outsider"}, false)})
	if len(stranger) != 1 || stranger[0].Handle != "" {
		t.Fatalf("a stranger resolved to %+v, want no handle", stranger)
	}
	if stranger[0].ExternalID != "U-outsider" || stranger[0].Platform != "chat" {
		t.Fatalf("a stranger lost their platform identity: %+v", stranger[0])
	}
}

// An engine-authored notification stamps no actor, so the sender field is the
// counterparty's only identity.
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

// EVERY BLOCK BUT KNOWLEDGE AND CHAT-THREAD DEGRADES SILENTLY BY DESIGN
// (internal/agent/prefetch), so an unreachable diary, a misconfigured
// auxiliary model and a seat that never stored a memory all start with none:
// the summary published once per turn is the only way to tell them apart.
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

	// Through prefetchFor, not the publisher: the wire from the turn to the
	// event is under test, and calling the publisher would skip it.
	e := &Engine{backends: &Backends{Queue: q}}
	company := &Company{
		Config: &config.Company{},
		Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			{Name: "Tech Lead", DeclaredHandle: "lead"}}},
	}
	req := Request{
		Handle:  "lead",
		RunID:   "run-1",
		WorkKey: "work-1",
		Events:  []*events.Event{notification("gitlab", "dev", nil, true)},
	}
	e.prefetchFor(t.Context(), company, req, "a pull request got a comment", e.askOf(req), auxspend.Use{Stage: types.AuxStageTurn, TurnID: "run-1"}, provenance.NewBoundary(), nil, nil, false, nil)

	select {
	case summary := <-got:
		switch {
		case summary.RoleName != "Tech Lead" || summary.AgentHandle != "lead":
			t.Errorf("the summary is attributed to %+v", summary)
		// THE RUN, the id every phase record of this turn sits under; the
		// work key is shared by no phase, so invisible to the turn view
		// (ADR-0017).
		case summary.TurnID != "run-1":
			t.Errorf("turn = %q, want the run this prefetch was assembled for",
				summary.TurnID)
		// No store and no models render nothing: a seat with no memory
		// must not look like one whose stores had nothing to say.
		case summary.PersonalMemoryHit || summary.EpisodeRecallHit ||
			summary.CounterpartyHit || summary.SynthesizedSkillsHit:
			t.Errorf("a block reported a hit with no store behind it: %+v", summary)
		case summary.RelevantKnowledgeSelectionCount != 0:
			t.Error("pages were reported with no knowledge backend")
		// A WEBHOOK NAMES NO THREAD, so the seventh block reports nothing:
		// not an unread thread, not an empty read. Those tell "handed the
		// conversation", "told to find it" and "there was none" apart.
		case summary.ThreadContextHit || summary.ThreadContextBytes != 0 ||
			summary.ThreadContextPosts != 0 || summary.ThreadContextRead ||
			summary.ThreadContextStoppedShort:
			t.Errorf("a non-chat trigger reported a thread: %+v", summary)
		// READ OFF THE TRIGGER: this pointer gates three of the seven
		// searches, and without the flag their zeroes read as empty stores.
		case !summary.TriggerRequiresRecon:
			t.Error("the gate that skipped three searches was not reported")
		}
	// BOUNDED, not t.Context(): a summary never published fails here rather
	// than at the package timeout, where it reads as a suite-wide stall.
	case <-time.After(5 * time.Second):
		t.Fatal("no prefetch summary was published")
	}
}

// AND A CHAT THREAD REPORTS WHAT IT WAS HANDED. Hit, bytes, count and whether
// a backend ANSWERED are four facts, none implied by the rest: both
// zero-message paths render a non-empty hint, so a thread read empty and one
// not read at all differ only in the read flag.
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
	// No chat transport (maintenance mode, an unreachable instance): the
	// read reports false and the block renders the unreadable hint, VISIBLE
	// rather than looking like a thread nobody wrote in.
	req := Request{
		Handle:  "lead",
		WorkKey: "work-2",
		Events: []*events.Event{notification("chat", "U1", map[string]string{
			"transport": "slack", "channel": "C0ENG",
			"ts": "1700000002.000100", "thread_ts": "1700000001.000100",
		}, true)},
	}
	e.prefetchFor(t.Context(), company, req, "+1", e.askOf(req), auxspend.Use{Stage: types.AuxStageTurn}, provenance.NewBoundary(), nil, nil, false, nil)

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

// AND A THREAD THAT WAS READ REACHES THE EVENT AS ONE. The block's answer and
// the event's fields are two structs copied by hand, so a field left out
// publishes a healthy read as unreachable and a truncated thread as whole.
// Against the mapping directly: a node's chat readers are its running
// transports, which a fake cannot be.
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
			TurnEmbedding:             types.EmbedFailed,
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
		// An empty episode block after a failed embed is not "nothing
		// similar", and only the summary says so.
		if summary.TurnEmbedding != types.EmbedFailed {
			t.Errorf("the turn's vector arrived as %q, want the failure", summary.TurnEmbedding)
		}
		if !summary.StartedAt.Equal(began) || summary.DurationMS != 1850 {
			t.Errorf("the assembly's timing arrived as %v / %dms, want %v / 1850ms",
				summary.StartedAt, summary.DurationMS, began)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no prefetch summary was published")
	}
}

// THE CONTEXT ASSEMBLY IS TIMED WHERE IT RUNS: the stretch between a turn
// announcing itself and its first phase, which nothing else on the record
// measures.
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
	req := Request{
		Handle: "lead", RunID: "run-4", WorkKey: "work-4",
		Events: []*events.Event{notification("gitlab", "dev", nil, true)},
	}
	e.prefetchFor(t.Context(), company, req, "a pull request got a comment", e.askOf(req), auxspend.Use{Stage: types.AuxStageTurn, TurnID: "run-4"}, provenance.NewBoundary(), nil, nil, false, nil)
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
