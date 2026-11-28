package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/chatlet"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RULE: A WAKE JOINS A RUNNING RUN ONLY FROM THE RUN'S OWN REQUESTER, AND
// ONLY ABOUT ITS WORK. Its conversation or its work item, and nothing another
// writer wrote, nothing the ledger worked, no type whose claim is never read,
// no review's own channel and no schedule's next fire. An empty item never
// matches an empty item.
//
// Mutation: compare the requester alone without the login or whom it works for
// (requester-only), or match a nil item to a nil item, and a case goes red.
func TestOnlyTheRunsRequesterJoinsItAndOnlyAboutItsWork(t *testing.T) {
	t.Parallel()
	item := &types.WorkItem{Backend: types.WorkBackend("worklet"), ID: "task-1"}
	run := sandbox.PendingRun{Requester: "lead", ConversationKey: "chat:C1", WorkItem: item}
	lead := wakeOrigin{requester: "lead", conversation: "chat:C1"}
	wake := &events.Event{Type: types.ExternalNotification{}.EventType()}
	with := func(o wakeOrigin, edit func(*wakeOrigin)) wakeOrigin { edit(&o); return o }
	for _, tc := range []struct {
		name   string
		run    sandbox.PendingRun
		ev     *events.Event
		o      wakeOrigin
		worked bool
		rule   routeRule
		why    string
	}{
		{"the requester in the run's conversation", run, wake, lead, false, ruleConversation, ""},
		{"the requester about the run's item", run, wake, with(lead, func(o *wakeOrigin) {
			o.conversation, o.item = "chat:C2", &types.WorkItem{Backend: item.Backend, ID: item.ID}
		}), false, ruleWorkItem, ""},
		{"the requester about other work", run, wake, with(lead, func(o *wakeOrigin) {
			o.conversation, o.item = "chat:C2", &types.WorkItem{Backend: item.Backend, ID: "task-2"}
		}), false, ruleNone, stayOtherWork},
		{"another writer in the run's conversation", run, wake, with(lead, func(o *wakeOrigin) {
			o.requester = "ceo"
		}), false, ruleNone, stayWriter},
		{"the requester's credential for another", run, wake, with(lead, func(o *wakeOrigin) {
			o.login = "ops-token"
		}), false, ruleNone, stayWriter},
		{"the requester working for another", run, wake, with(lead, func(o *wakeOrigin) {
			o.workingFor = &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ceo"}
		}), false, ruleNone, stayWriter},
		{"a type the ledger does not key", run, &events.Event{Type: "agent_phase_started"}, lead, false,
			ruleNone, stayKind},
		{"a delegated review", run, &events.Event{Type: types.DelegatedReviewType}, lead, false, ruleNone,
			stayKind},
		{"a colleague's ask, answered on its own channel", run, &events.Event{Type: types.A2ARequestType},
			lead, false, ruleNone, stayKind},
		{"a schedule's fire", sandbox.PendingRun{Requester: "lead", ConversationKey: "schedule/nightly"},
			&events.Event{Type: types.TaskAssigned{}.EventType()},
			with(lead, func(o *wakeOrigin) { o.conversation = "schedule/nightly" }), false, ruleNone, stayScheduled},
		{"a wake the ledger worked", run, wake, lead, true, ruleNone, stayWorked},
		{"a run nobody asked for", sandbox.PendingRun{ConversationKey: "chat:C1"}, wake,
			with(lead, func(o *wakeOrigin) { o.requester = "" }), false, ruleNone, stayNoRequester},
		{"no item on either side", sandbox.PendingRun{Requester: "lead", ConversationKey: "chat:C1"}, wake,
			with(lead, func(o *wakeOrigin) { o.conversation = "chat:C2" }), false, ruleNone, stayOtherWork},
		{"an empty item on either side", sandbox.PendingRun{Requester: "lead", WorkItem: &types.WorkItem{}}, wake,
			with(lead, func(o *wakeOrigin) { o.conversation, o.item = "chat:C2", &types.WorkItem{} }), false,
			ruleNone, stayOtherWork},
	} {
		rule, why := routeOf(tc.run, tc.ev, tc.o, tc.worked, false)
		if rule != tc.rule || why != tc.why {
			t.Errorf("%s: routed (%q, %q), want (%q, %q)", tc.name, rule, why, tc.rule, tc.why)
		}
	}
}

// A ROUTER'S OUTCOMES AND RULES ARE CLOSED SETS, the counter's attributes.
func TestTheRoutersOutcomesAndRulesAreClosed(t *testing.T) {
	t.Parallel()
	for _, o := range routeOutcomes {
		if !o.Valid() {
			t.Errorf("outcome %q is not valid", o)
		}
	}
	for _, r := range routeRules {
		if !r.Valid() {
			t.Errorf("rule %q is not valid", r)
		}
	}
	if routeOutcome("routed").Valid() || routeRule("anything").Valid() {
		t.Error("a value outside the set is valid")
	}
	if !types.DispositionJoined.Valid() {
		t.Error("the joined disposition is not valid")
	}
}

// A WAKE IS CLAIMED BY EXACTLY THE KEYS ITS DELIVERY IS DROPPED BY. Its own, and
// a chat message's in a thread: a claim missing one leaves the delivery to run
// again after the run read it.
//
// Mutation: drop the chat key from the router's wake, or derive it apart from
// dropWorked's, and this goes red.
func TestAWakeIsClaimedByTheKeysItsDeliveryIsDroppedBy(t *testing.T) {
	t.Parallel()
	inThread := events.New(types.ExternalNotification{NotificationSource: "mattermost",
		SourceEventType: "message", Sender: "lead", Body: "use staging",
		Metadata: map[string]string{notify.TransportField: "mattermost", "channel": "C9",
			"thread_ts": "root-post", "ts": "post-2"}}, events.TraceContext{})
	plain := events.New(types.A2ARequest{Requester: "lead", Content: "and the old flag"}, events.TraceContext{})
	for name, ev := range map[string]*events.Event{"chat in a thread": inThread, "a colleague's ask": plain} {
		asked := &askedLedger{MemoryCompletions: ledgerstore.NewMemoryCompletions()}
		d := &Dispatcher{Completions: asked, Ledgered: inbox.Ledgered}
		d.dropWorked(t.Context(), "swe", []*events.Event{ev})
		got := deliveryKeys(ev)
		if !slices.Equal(got, asked.first()) {
			t.Errorf("%s: the router claims %v, the drop reads %v", name, got, asked.first())
		}
	}
	if len(deliveryKeys(inThread)) != 2 {
		t.Error("a chat message in a thread is claimed without its message key")
	}
}

// askedLedger records the keys each Worked read asked.
type askedLedger struct {
	*ledgerstore.MemoryCompletions
	mu    sync.Mutex
	asked [][]string
}

func (a *askedLedger) Worked(ctx context.Context, handle string, keys []string) map[string]bool {
	a.mu.Lock()
	a.asked = append(a.asked, slices.Clone(keys))
	a.mu.Unlock()
	return a.MemoryCompletions.Worked(ctx, handle, keys)
}

func (a *askedLedger) first() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.asked) == 0 {
		return nil
	}
	return a.asked[0]
}

// routerRig is an engine holding a company of the swe seat and its lead, a
// running run of swe's that lead asked for in chat:C1, and swe's inbox on an
// in-memory broker with no consumer: the held inbox a router reads.
type routerRig struct {
	e              *Engine
	q              *memory.Queue
	store          *sandbox.CoordStore
	runs           *runSteers
	ledger         *ledgerstore.MemoryCompletions
	seen           *ledgerstore.SeenThrough
	job            string
	subject, group string
	handed         chan tookWakes
	model          *llmtest.Script
}

func newRouterRig(t *testing.T) *routerRig {
	t.Helper()
	return newRouterRigFor(t, "lead", "chat:C1")
}

// newRouterRigFor is [newRouterRig] whose run requester asked for in
// conversation.
func newRouterRigFor(t *testing.T, requester, conversation string) *routerRig {
	t.Helper()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	runs, _ := steersOver(t, steerRing, store)
	ledger := ledgerstore.NewMemoryCompletions()
	handed := make(chan tookWakes, 8)
	runs.claimingIn(ledger).telling(func(_ context.Context, _ string, took tookWakes) { handed <- took })
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	seen := ledgerstore.NewMemorySeenThrough()
	runs.seeingThrough(seen)
	e := &Engine{backends: &Backends{Queue: q, Fleet: fleet}, runSteers: runs}
	chart := &org.Organization{Name: "Acme", Roles: []*org.Role{{Name: "SWE"}, {Name: "Lead", Kind: org.KindHuman}}}
	e.epoch.current.Store(&Company{Org: chart})
	e.notify.registry = notify.NewRegistry(chart, nil)
	subject, group, err := seatHold{engine: e}.inboxOf("swe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.EnsureSubscription(ctx, subject, group); err != nil {
		t.Fatal(err)
	}
	opened, err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: "run-1", AgentID: sweAgent, AgentHandle: "swe", Role: "SWE", Harness: "claude-code",
		TraceID: "tr-1", CreatedAt: time.Now().UTC(), Requester: requester, ConversationKey: conversation,
		Steer: sandbox.RunSteer{Seat: runs.anchor("run-1", sweAgent, "swe")},
	}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if ok, err := store.MarkSuspended(ctx, "run-1",
		sandbox.Suspension{State: json.RawMessage(`{"version":4}`), Iteration: 1}); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	return &routerRig{e: e, q: q, store: store, runs: runs, ledger: ledger, seen: seen, job: opened.LaunchID,
		subject: subject, group: group, handed: handed}
}

// post publishes to swe's inbox a Chatlet message seat wrote as a person in
// conversation, addressed to swe, from the record at position seq on the chat
// log, and answers it.
func (r *routerRig) post(t *testing.T, seat, conversation, words string, seq uint64) *events.Event {
	t.Helper()
	return r.publish(t, seat, conversation, words, seq, true)
}

// publish is [routerRig.post], addressed to swe or not.
func (r *routerRig) publish(t *testing.T, seat, conversation, words string, seq uint64,
	addressed bool) *events.Event {
	t.Helper()
	ev := events.New(types.ExternalNotification{NotificationSource: "chatlet", SourceEventType: "post",
		Sender: seat, Body: words, Addressed: addressed,
		Writer: &types.NotificationWriter{Kind: iam.ActorHuman, Seat: seat}}, events.TraceContext{})
	ev.Payload = map[string]any{notify.ConversationField: conversation, notify.PartitionField: conversation,
		notify.TriggerField: statelog.Position{Stream: "chat", Generation: 1, Seq: seq}.String()}
	if err := r.q.Publish(t.Context(), r.subject, ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// filed is the id of every wake the run's record holds, in its order.
func (r *routerRig) filed(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, n := range wakeItems(mustRow(t, r.store, "run-1").Steer.Notes) {
		for _, w := range n.Wake.Wakes {
			out = append(out, w.ID)
		}
	}
	return out
}

// A REQUESTER'S FOLLOW-UP JOINS THE RUNNING RUN ONCE, AND ITS DELIVERY IS SKIPPED
// AS JOINED, NAMING THE TURN. Read off the held inbox, filed once however often
// it is read again (a nudge, a backstop, a new router after a restart), read by
// a hook, claimed: its delivery is then dropped `joined` naming the run's turn,
// and a person's queued line for it is taken down. Another writer's message in
// the same conversation stays, and so does a filed wake the run never read: both
// run as turns of their own.
//
// Mutation: file without the store's dedupe and a fresh router's read (decided
// lost) files it twice; drop the claim, and the delivery runs again.
func TestARequestersFollowUpJoinsTheRunOnceAndItsDeliveryIsSkippedNamingTheTurn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRouterRig(t)
	followUp := r.post(t, "lead", "chat:C1", "use staging, not production", 7)
	other := r.post(t, "ceo", "chat:C1", "and fix the logo", 8)
	memo := newRouteMemo()
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	r.e.routeSeat(ctx, "swe", r.subject, r.group, newRouteMemo())
	if got := r.filed(t); !slices.Equal(got, []string{followUp.ID.String()}) {
		t.Fatalf("the run's record holds wakes %v, want the requester's follow-up once", got)
	}
	if mail, err := r.q.Backlog(ctx, r.subject, r.group, 8); err != nil || len(mail) != 2 {
		t.Fatalf("the held inbox holds %d messages (%v), want both still there", len(mail), err)
	}

	box := boundInbox(t, r.runs, r.store, "run-1", r.job)
	viewHolds(t, box, 1)
	read := box.Take(ctx, mcpbridge.SteerEventToolBatch, hookCall())
	if !strings.Contains(read, "use staging, not production") {
		t.Fatalf("the hook was not handed the follow-up: %s", read)
	}
	settled, err := r.store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: r.job, Held: []string{takeOf(t, read)}})
	if err != nil {
		t.Fatal(err)
	}
	r.runs.confirmed(ctx, mustRow(t, r.store, "run-1"), settled)
	select {
	case got := <-r.handed:
		if len(got.wakes) != 1 || !slices.Equal(got.wakes[0].Triggers, []string{followUp.ID.String()}) ||
			got.as != chatlet.TakenJoined || got.turn != "run-1" {
			t.Errorf("the queued line handed over is %+v, want the follow-up's, joined to run-1", got)
		}
	case <-ctx.Done():
		t.Fatal("no queued line was taken down for the claimed wake")
	}

	var observed []*events.Event
	d := &Dispatcher{Completions: r.ledger, Ledgered: inbox.Ledgered,
		Observe: func(_ context.Context, ev *events.Event) { observed = append(observed, ev) }}
	all := []*events.Event{followUp, other}
	surviving, skipped := d.dropWorked(ctx, "swe", all)
	if len(surviving) != 1 || surviving[0].ID != other.ID {
		t.Fatalf("after the run, %d deliveries run, want the other writer's alone", len(surviving))
	}
	d.noteSkipped(ctx, "swe", all, surviving, skipped)
	if len(observed) != 1 {
		t.Fatalf("recorded %d skips, want the follow-up's", len(observed))
	}
	skip, ok := events.DataAs[*types.TurnTriggerSkipped](observed[0])
	if !ok || skip.Disposition != types.DispositionJoined || !strings.Contains(skip.Reason, "run-1") {
		t.Errorf("the follow-up's skip is %+v, want disposition joined naming turn run-1", skip)
	}
}

// A WAKE THE RUN READ BINDS ITS TURN AS ITS OWN TRIGGER WOULD, AND READS ITS
// CONVERSATION THROUGH IT. The resumed turn owes the answer the read wake
// owes (its delivery check and review hold it to it), and the seat's
// seen-through mark for the wake's conversation moves to the wake's record at
// the claim, so a later message there at or below it that asks nothing is
// absorbed (ADR-0120). A wake the run never read owes the turn nothing and
// moves no mark.
//
// Mutation: drop the read wakes' obligations from turnReply (what the resume
// asks), or the claim's seeThrough, and this goes red.
func TestAWakeTheRunReadBindsItsTurnAndReadsItsConversationThrough(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRouterRig(t)
	followUp := r.post(t, "lead", "chat:C1", "use staging, not production", 7)
	r.e.routeSeat(ctx, "swe", r.subject, r.group, newRouteMemo())
	if got := parkedReply(t, r.runs, mustRow(t, r.store, "run-1")); got != turn.NoReply() {
		t.Fatalf("a wake no step read binds the turn to %v, want nothing", got)
	}
	if mark := r.seen.Mark(ctx, "swe", "chat:C1", "chat"); !mark.IsZero() {
		t.Fatalf("a wake no step read moved the conversation's mark to %v", mark)
	}
	box := boundInbox(t, r.runs, r.store, "run-1", r.job)
	viewHolds(t, box, 1)
	read := box.Take(ctx, mcpbridge.SteerEventToolBatch, hookCall())
	settled, err := r.store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: r.job, Held: []string{takeOf(t, read)}})
	if err != nil {
		t.Fatal(err)
	}
	r.runs.confirmed(ctx, mustRow(t, r.store, "run-1"), settled)
	if got, want := parkedReply(t, r.runs, mustRow(t, r.store, "run-1")),
		ReplyFor([]*events.Event{followUp}); got != want || !got.Awaited() {
		t.Errorf("the turn whose run read the follow-up owes %v, want the follow-up's own %v", got, want)
	}
	if mark := r.seen.Mark(ctx, "swe", "chat:C1", "chat"); mark.Seq != 7 {
		t.Fatalf("the conversation's mark is %v after the run read the record at 7", mark)
	}
	older := r.publish(t, "lead", "chat:C1", "fyi, the build is green", 6, false)
	d := &Dispatcher{Completions: r.ledger, SeenThrough: r.seen, Ledgered: inbox.Ledgered}
	if surviving, skipped := d.dropWorked(ctx, "swe", []*events.Event{older}); len(surviving) != 0 ||
		skipped[older.ID] != skipAbsorbed {
		t.Errorf("a message asking nothing below the read wake's record runs (%d left, cause %v)",
			len(surviving), skipped[older.ID])
	}
}

// A FILED WAKE THE RUN NEVER READS, AND ONE THE RECORD REFUSES, RUN AS TURNS OF
// THEIR OWN. Neither is claimed; a refusal is decided for its job and never
// filed again within it.
//
// Mutation: claim a wake at its filing, or retry a refused filing on every read,
// and this goes red.
func TestAnUnreadOrRefusedWakeRunsAsATurnOfItsOwn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRouterRig(t)
	unread := r.post(t, "lead", "chat:C1", "and ping me after", 9)
	r.e.routeSeat(ctx, "swe", r.subject, r.group, newRouteMemo())
	if got := r.filed(t); !slices.Equal(got, []string{unread.ID.String()}) {
		t.Fatalf("the record holds wakes %v, want the follow-up", got)
	}
	refused := r.post(t, "lead", "chat:C1", "one more thing", 10)
	gone := mustRow(t, r.store, "run-1")
	gone.LaunchID = "job-replaced"
	memo := newRouteMemo()
	r.e.routeWake(ctx, "swe", []sandbox.PendingRun{gone}, refused, nil, memo, &routeRead{})
	if !memo.done("job-replaced", refused.ID.String()) {
		t.Error("a refused filing is not decided for its job, so every read files it again")
	}
	if got := r.filed(t); slices.Contains(got, refused.ID.String()) {
		t.Errorf("a wake for a job the record no longer names was filed: %v", got)
	}
	d := &Dispatcher{Completions: r.ledger, Ledgered: inbox.Ledgered}
	surviving, _ := d.dropWorked(ctx, "swe", []*events.Event{unread, refused})
	if len(surviving) != 2 {
		t.Fatalf("%d of the unread and the refused wake run, want both", len(surviving))
	}
}

// A ROUTER LIVES WHILE ITS SEAT IS BUSY. It stops when the run's hold lifts or
// the seat is let go, and every router ends with the engine's stop, after which
// none starts: each a goroutine with a way to stop, waited on its own signal.
//
// Mutation: drop the cancel on a free seat, or stopRouters' wait, and this goes
// red (or hangs to the test's deadline).
func TestARouterLivesWhileItsSeatIsBusyAndEndsWithTheEngine(t *testing.T) {
	t.Parallel()
	r := newRouterRig(t)
	e := r.e
	running := func(handle string) *seatRouter {
		e.wakeRouters.mu.Lock()
		defer e.wakeRouters.mu.Unlock()
		return e.wakeRouters.running[handle]
	}
	ended := func(t *testing.T, router *seatRouter, what string) {
		t.Helper()
		select {
		case <-router.done:
		case <-t.Context().Done():
			t.Fatalf("the router did not end %s", what)
		}
	}
	e.followSeat("swe", true)
	first := running("swe")
	if first == nil {
		t.Fatal("a busy seat has no router")
	}
	e.followSeat("swe", true)
	if running("swe") != first {
		t.Fatal("a second busy signal started a second router")
	}
	e.followSeat("swe", false)
	ended(t, first, "when its seat's run let the seat go")
	if running("swe") != nil {
		t.Fatal("a free seat keeps its router")
	}
	e.followSeat("swe", true)
	e.followSeat("lead", true)
	held := []*seatRouter{running("swe"), running("lead")}
	e.stopRouters()
	for _, router := range held {
		ended(t, router, "at the engine's stop")
	}
	e.followSeat("swe", true)
	if running("swe") != nil {
		t.Fatal("a router started after the engine stopped")
	}
}

// parkedReply is what run's turn answers when it resumes ([runSteers.turnReply]),
// had it parked owing nothing of its own.
func parkedReply(t *testing.T, runs *runSteers, run sandbox.PendingRun) turn.Reply {
	t.Helper()
	run.Reply = turn.NoReply().String()
	reply, err := runs.turnReply(run)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}
