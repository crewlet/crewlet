package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// recorder captures published events, keyed by topic.
type recorder struct {
	mu        sync.Mutex
	published []publication
	err       error

	// before, when set, runs at the top of every Publish, OUTSIDE this
	// recorder's own lock so the hook can read the rest of the world.
	//
	// It is how a case asserts an ORDER rather than an outcome: several
	// rules here are "this happens before the announcement", and an
	// announcement is a publish, so the only place from which the rule is
	// observable is inside one. Asserted after the fact instead, every one
	// of those rules passes whatever order the code took.
	before func()
}

type publication struct {
	topic string
	event *events.Event
}

func (r *recorder) Publish(_ context.Context, topic string, ev *events.Event) error {
	r.mu.Lock()
	before := r.before
	r.mu.Unlock()
	if before != nil {
		before()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.published = append(r.published, publication{topic: topic, event: ev})
	return nil
}

func (r *recorder) topics() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.published))
	for _, p := range r.published {
		out = append(out, p.topic)
	}
	return out
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.published)
}

// waiterRig is the whole detached-run world for one test.
type waiterRig struct {
	t        *testing.T
	queue    *recorder
	pending  *CoordStore
	provider *FakeProvider
	runner   *FakeRunner
	manager  *Manager
	waiter   *Waiter
	now      time.Time
}

func newWaiterRig(t *testing.T) *waiterRig {
	t.Helper()
	rig := &waiterRig{
		t:        t,
		queue:    &recorder{},
		provider: NewFakeProvider(),
		runner:   NewFakeRunner("claude-code"),
		now:      time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
	// ONE CLOCK FOR THE WHOLE WORLD, the store's included. The row's own
	// last write dates a pause nothing stamped ([PendingRun.HeldSince]), so
	// a store left on the wall clock would date it years after the instant
	// a test advances to and no age assertion below would mean anything.
	rig.pending = NewCoordStore(memory.NewFleet()).WithClock(func() time.Time { return rig.now })
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: rig.provider},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rig.manager = manager
	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: manager,
		Now: func() time.Time { return rig.now },
	})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	rig.waiter = waiter
	return rig
}

// launch seeds a running detached job with a real box behind it, in the order
// a real launch writes it: the row, the box, then the suspension that opens
// the run to the poll.
func (r *waiterRig) launch(turnID string) PendingRun {
	r.t.Helper()
	r.launching(turnID)
	r.suspend(turnID)
	return r.get(turnID)
}

// launching stops one step short of that: the job is started and its box
// attached, but the turn has not yet written the conversation a resume
// re-enters. Nothing may poll or claim a run here.
func (r *waiterRig) launching(turnID string) PendingRun {
	r.t.Helper()
	ctx := r.t.Context()
	box, err := r.provider.Create(ctx, Spec{})
	if err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	run := PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: sweID, Role: "SWE",
		// A DIRECT MESSAGE'S TWO VALUES: the conversation an answer is
		// matched on and the resume reports back to, and the partition
		// the kick-off arrived in. Different here on purpose — equal ones
		// would let a path that read the wrong field pass every case
		// below.
		CodingAgent: "claude-code", PartitionKey: "chat:D1:root-1",
		ConversationKey: "chat:D1",
		TraceID:         "tr-1", SpanID: "sp-1", CreatedAt: r.now,
	}
	if err := r.pending.BeginLaunch(ctx, run, Fence{}); err != nil {
		r.t.Fatalf("BeginLaunch: %v", err)
	}
	if err := r.pending.AttachSandbox(ctx, turnID, BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1",
		CodingAgent: "claude-code", PauseTTLSec: DefaultPauseTTL.Seconds(),
	}, Fence{}); err != nil {
		r.t.Fatalf("AttachSandbox: %v", err)
	}
	return r.get(turnID)
}

// suspend writes the execute_state a real suspending turn would have written,
// which is what opens the run to the completion poll.
func (r *waiterRig) suspend(turnID string) {
	r.t.Helper()
	suspended, err := r.pending.MarkSuspended(r.t.Context(), turnID, map[string]any{
		"version":              float64(1),
		"pending_tool_call_id": "call-1",
		"pending_tool_name":    "run_sandbox",
	})
	if err != nil {
		r.t.Fatalf("MarkSuspended: %v", err)
	}
	if !suspended {
		r.t.Fatalf("MarkSuspended %s: the launch did not open to the poll", turnID)
	}
}

func (r *waiterRig) get(turnID string) PendingRun {
	r.t.Helper()
	run, ok, err := r.pending.Get(r.t.Context(), turnID)
	if err != nil || !ok {
		r.t.Fatalf("Get %s = %v, %v", turnID, ok, err)
	}
	return run
}

// finished asserts that a run ended: its record is deleted, which is the
// only shape a settled run has.
func (r *waiterRig) finished(turnID string) {
	r.t.Helper()
	run, ok, err := r.pending.Get(r.t.Context(), turnID)
	if err != nil {
		r.t.Fatalf("Get %s: %v", turnID, err)
	}
	if ok {
		r.t.Fatalf("run %s still has a record in %q; a settled run has none", turnID, run.Status)
	}
}

func (r *waiterRig) tick() int {
	r.t.Helper()
	fired, err := r.waiter.Tick(r.t.Context())
	if err != nil {
		r.t.Fatalf("Tick: %v", err)
	}
	return fired
}

// ---------------------------------------------------------------------
// completion
// ---------------------------------------------------------------------

func TestARunningJobFiresNoCompletionUntilItFinishes(t *testing.T) {
	rig := newWaiterRig(t)
	rig.launch("t1")

	if fired := rig.tick(); fired != 0 {
		t.Fatalf("fired %d completions for a job still running", fired)
	}
	rig.runner.Finish(Result{Success: true})
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d, want 1", fired)
	}
}

// Two publishes, two purposes: the announcement feeds the dashboard's
// broadcast stream, the control copy reaches the one node that holds the
// suspended conversation.
func TestACompletionIsAnnouncedAndRoutedToTheSeatsOwner(t *testing.T) {
	rig := newWaiterRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true})
	rig.tick()

	got := rig.queue.topics()
	want := []string{
		topics.Event(types.SandboxRunCompleted{}.EventType()),
		sweControl(),
	}
	if len(got) != len(want) {
		t.Fatalf("published to %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("publish %d went to %q, want %q", i, got[i], want[i])
		}
	}
}

// The completion turn must nest under the turn that started the job rather
// than opening a trace root of its own.
func TestACompletionCarriesTheOriginalTrace(t *testing.T) {
	rig := newWaiterRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true})
	rig.tick()

	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	ev := rig.queue.published[0].event
	if ev.TraceID != "tr-1" {
		t.Fatalf("TraceID = %q, want the launching turn's", ev.TraceID)
	}
	if ev.ParentSpanID != "sp-1" {
		t.Fatalf("ParentSpanID = %q, want the launching span", ev.ParentSpanID)
	}
}

// A completion event is a pure control signal — the outcome is read at collect
// time, so nothing about the result may ride on the wire.
func TestACompletionCarriesTheIdentityAndNotTheOutcome(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "shipped it"})
	rig.tick()

	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	payload, ok := rig.queue.published[0].event.Data.(*types.SandboxRunCompleted)
	if !ok {
		t.Fatalf("payload is %T", rig.queue.published[0].event.Data)
	}
	if payload.TurnID != "t1" || payload.SandboxID != run.SandboxID {
		t.Fatalf("payload = %+v, want the run's identity", payload)
	}
	// And WHICH job finished, the only one the completion may claim.
	if run.LaunchID == "" || payload.LaunchID != run.LaunchID {
		t.Fatalf("launch = %q, want the job the tick saw finish, %q", payload.LaunchID, run.LaunchID)
	}
	if payload.AgentHandle != "swe" || payload.CodingAgent != "claude-code" {
		t.Fatalf("payload = %+v", payload)
	}
}

// A transient poll error is not a completion: firing on one would collect a
// result the job has not written.
func TestATransientPollErrorIsRetriedRatherThanFired(t *testing.T) {
	rig := newWaiterRig(t)
	rig.launch("t1")
	rig.runner.PollErr = errors.New("connection reset")

	for range 10 {
		if fired := rig.tick(); fired != 0 {
			t.Fatal("a poll error fired a completion")
		}
	}
	rig.runner.PollErr = nil
	rig.runner.Finish(Result{Success: true})
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d after the error cleared, want 1", fired)
	}
}

// A box that can never be reached again can never produce a result, so the
// run has to be freed rather than polled forever.
func TestAVanishedBoxFiresCompletionOnceTheStreakIsLongEnough(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.provider.Vanished[run.SandboxID] = true

	// Two failures, but no time has passed: giving up is TERMINAL — the
	// completion fires, collect cannot reconnect either, and the run is
	// settled failed — so a burst of probes must not be able to spend the
	// window on its own.
	for i := range MinConnectFailures + 2 {
		if fired := rig.tick(); fired != 0 {
			t.Fatalf("gave up on probe %d with no time elapsed", i+1)
		}
	}
	rig.now = rig.now.Add(ConnectGiveUp)
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d after a full %s unreachable, want 1", fired, ConnectGiveUp)
	}
}

// THE WINDOW IS A DURATION, NOT A TICK COUNT. It was four consecutive
// failures, justified in the comment as "four ticks ≈ 1 minute" — true only at
// the default cadence. Driven faster, the same four failures are a fraction of
// a second, and a box that is briefly slow rather than gone gets its turn
// destroyed.
func TestTheGiveUpWindowDoesNotShrinkWithTheCadence(t *testing.T) {
	rig := newWaiterRig(t)
	fast, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
		Interval: 100 * time.Millisecond,
		Now:      func() time.Time { return rig.now },
	})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	run := rig.launch("t1")
	rig.provider.Vanished[run.SandboxID] = true

	// Far more probes than the old count, across the whole of a fast
	// waiter's tick budget for that many ticks.
	for range 20 {
		rig.now = rig.now.Add(100 * time.Millisecond)
		if fired, err := fast.Tick(t.Context()); err != nil || fired != 0 {
			t.Fatalf("gave up after 2s at a 100ms cadence: fired=%d err=%v", fired, err)
		}
	}
	rig.now = rig.now.Add(ConnectGiveUp)
	if fired, err := fast.Tick(t.Context()); err != nil || fired != 1 {
		t.Fatalf("fired %d after a full %s unreachable: %v", fired, ConnectGiveUp, err)
	}
}

// At a cadence slower than the window, the FIRST failure is already older than
// it. One probe is not evidence a box is gone.
func TestOneProbeNeverGivesUpHoweverOldTheRunIs(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.provider.Vanished[run.SandboxID] = true
	rig.now = rig.now.Add(100 * time.Hour)

	if fired := rig.tick(); fired != 0 {
		t.Fatal("a single failed probe declared the box gone and failed the run")
	}
	rig.now = rig.now.Add(ConnectGiveUp)
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d on the confirming probe, want 1", fired)
	}
}

// One reachable tick means the box is there; a later blip must start counting
// from zero rather than inheriting a streak from an hour ago.
func TestASuccessfulReconnectClearsTheFailureStreak(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")

	rig.provider.Vanished[run.SandboxID] = true
	rig.tick()
	rig.now = rig.now.Add(ConnectGiveUp)
	delete(rig.provider.Vanished, run.SandboxID)
	rig.tick() // reachable again — the streak, and its clock, start over

	rig.provider.Vanished[run.SandboxID] = true
	for i := range MinConnectFailures + 1 {
		if fired := rig.tick(); fired != 0 {
			t.Fatalf("the old streak carried over: gave up on probe %d", i+1)
		}
	}
}

// A runner nobody registered cannot be polled, and retrying forever would hold
// the seat busy for the life of the deployment.
func TestARunNamingAnUnknownRunnerIsFreedRatherThanPolledForever(t *testing.T) {
	rig := newWaiterRig(t)
	rig.launch("t1")
	if err := rig.pending.AttachSandbox(t.Context(), "t1", BoxRef{
		SandboxID: rig.get("t1").SandboxID, CodingAgent: "retired-agent",
	}, Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d, want the run freed", fired)
	}
}

// The engine imposes NO run-time limit on a coding job — the box is bounded
// only by how long the engine can go without a heartbeat.
func TestEveryTickKeepsTheRunningBoxAlive(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	box := rig.provider.Box(run.SandboxID)

	for range 5 {
		rig.tick()
	}
	if got := box.Keepalives(); got != 5 {
		t.Fatalf("box was heart-beaten %d times over 5 ticks — a long job would be reaped mid-run", got)
	}
}

// A run parked on a question is not running: the seat is free and the box is
// deliberately NOT heart-beaten, because the pause TTL is what bounds it.
func TestAParkedRunIsNeitherPolledNorHeartBeaten(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	box := rig.provider.Box(run.SandboxID)
	if err := rig.pending.MarkAwaiting(t.Context(), "t1", Clarification{
		Question: "which branch?", Audience: "requester",
	}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	rig.runner.Finish(Result{Success: true})

	if fired := rig.tick(); fired != 0 {
		t.Fatal("a parked run fired a completion")
	}
	if got := box.Keepalives(); got != 0 {
		t.Fatalf("a parked box was heart-beaten %d times — the pause TTL would never expire it", got)
	}
}

// THE LAUNCH WINDOW, from the poll's side. The job is running and can finish
// at any moment, but the turn that started it has not yet written the
// conversation a resume re-enters. Firing here hands the coordinator a claim
// it cannot resume — and the run's turn is destroyed by a job that was too
// quick. The tick that finds it suspended is the one that may fire.
func TestARunThatHasNotSuspendedYetIsNotPolled(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launching("t1")
	box := rig.provider.Box(run.SandboxID)
	rig.runner.Finish(Result{Success: true, Text: "done before the turn unwound"})

	if fired := rig.tick(); fired != 0 {
		t.Fatal("a completion fired for a run with no conversation to resume")
	}
	if got := rig.queue.count(); got != 0 {
		t.Fatalf("published %d events for a launching run", got)
	}
	if got := box.Keepalives(); got != 0 {
		t.Fatalf("a launching box was heart-beaten %d times before it was pollable", got)
	}

	rig.suspend("t1")
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d completions once the suspension landed, want 1", fired)
	}
}

// A publish failure must not be counted as a fired completion: the coordinator
// never heard, so the next tick has to try again.
func TestAFailedPublishIsRetriedOnTheNextTick(t *testing.T) {
	rig := newWaiterRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true})
	rig.queue.err = errors.New("broker unreachable")

	if fired := rig.tick(); fired != 0 {
		t.Fatalf("fired %d despite the publish failing", fired)
	}
	rig.queue.err = nil
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d after the broker came back, want 1", fired)
	}
}

// ---------------------------------------------------------------------
// the pause reaper
// ---------------------------------------------------------------------

// park moves a launched run onto a question with its box snapshotted.
func (r *waiterRig) park(turnID string) {
	r.t.Helper()
	ctx := r.t.Context()
	if err := r.pending.MarkAwaiting(ctx, turnID, Clarification{
		Question: "which branch?", Audience: "requester", Branch: "wip/t1",
	}); err != nil {
		r.t.Fatalf("MarkAwaiting: %v", err)
	}
	if err := r.pending.MarkBoxPaused(ctx, turnID, r.now); err != nil {
		r.t.Fatalf("MarkBoxPaused: %v", err)
	}
}

func TestAPauseInsideItsTtlIsLeftAlone(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.park("t1")

	rig.now = rig.now.Add(DefaultPauseTTL - time.Second)
	rig.tick()

	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want the run still parked", got.Status)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v before the TTL elapsed", killed)
	}
	if rig.provider.Box(run.SandboxID) == nil {
		t.Fatal("the box is gone")
	}
}

// The run is NOT over when the pause expires: the answer can still arrive, and
// the work re-seeds from the pushed branch, which was always the durable half.
func TestAnExpiredPauseIsReclaimedAndTheRunReseeds(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.park("t1")

	rig.now = rig.now.Add(DefaultPauseTTL + time.Second)
	rig.tick()

	got := rig.get("t1")
	if got.Status != StatusReseed {
		t.Fatalf("status = %q, want %q", got.Status, StatusReseed)
	}
	if got.SandboxID != "" {
		t.Fatalf("sandbox_id = %q, want it cleared so the answer provisions a fresh box", got.SandboxID)
	}
	if !got.PausedAt.IsZero() {
		t.Fatal("paused_at survived the reap — the row still claims a snapshot exists")
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want [%s]", killed, run.SandboxID)
	}
	if got.Branch != "wip/t1" {
		t.Fatalf("branch = %q — the durable half of the work was lost", got.Branch)
	}
}

// Connect auto-resumes, so reclaiming through it would boot the work back up
// purely to shut it down.
func TestTheReaperKillsByIdRatherThanConnecting(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	box := rig.provider.Box(run.SandboxID)
	rig.park("t1")
	if err := box.Pause(t.Context()); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	rig.now = rig.now.Add(DefaultPauseTTL + time.Second)
	rig.tick()

	if !box.Paused() {
		t.Fatal("the reaper resumed the box before reclaiming it")
	}
}

// A zero TTL means "never hold a blocked box": the coordinator already tore
// this one down when the run blocked, so there is no snapshot left to expire.
func TestAZeroPauseTtlIsNotADeadlineForTheReaper(t *testing.T) {
	rig := newWaiterRig(t)
	rig.launch("t1")
	if err := rig.pending.AttachSandbox(t.Context(), "t1", BoxRef{
		SandboxID: rig.get("t1").SandboxID, CodingAgent: "claude-code", PauseTTLSec: 0,
	}, Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	rig.park("t1")

	rig.now = rig.now.Add(100 * time.Hour)
	rig.tick()

	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want the run left where it was", got.Status)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v for a run that holds no snapshot", killed)
	}
}

// Every other paused box belongs to a tail being actively driven; expiring one
// from here would kill it out from under live work.
func TestTheReaperTouchesOnlyRunsParkedOnAQuestion(t *testing.T) {
	rig := newWaiterRig(t)
	rig.launch("t1")
	if err := rig.pending.MarkBoxPaused(t.Context(), "t1", rig.now); err != nil {
		t.Fatalf("MarkBoxPaused: %v", err)
	}
	if err := rig.pending.SetStatus(t.Context(), "t1", StatusResumed, Fence{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	rig.now = rig.now.Add(100 * time.Hour)
	rig.tick()

	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("the reaper killed %v out from under a live tail", killed)
	}
}

// The answer that un-parks a run and the reaper that expires it race. The flip
// is the authority for the WHOLE reap, not just for the status write — killing
// before it lands destroys the box the resume is reconnecting to.
func TestAnAnsweredRunIsNotReclaimedUnderTheResume(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.park("t1")

	// The answer arrives between the reaper's snapshot and its flip.
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1", AnswerTail(rig.get("t1").LaunchID)); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	rig.now = rig.now.Add(DefaultPauseTTL + time.Second)
	rig.waiter.reapExpiredPauses(t.Context(), []PendingRun{withPaused(run, rig.now.Add(-DefaultPauseTTL-time.Second))})

	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("the reaper destroyed %v underneath a resume that had already claimed the run", killed)
	}
	if got := rig.get("t1"); got.Status != StatusResumed {
		t.Fatalf("status = %q, want the resume's claim to stand", got.Status)
	}
}

// The pause instant is a SECOND, warn-only write after the box is already
// paused. A run whose park then lands holds a snapshot nothing dates — and a
// reaper that skipped it left that box paused and billed for ever, which on a
// remote provider is the most expensive way this subsystem fails.
func TestAParkedBoxWhosePauseWasNeverRecordedIsStillReaped(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	if err := rig.provider.Box(run.SandboxID).Pause(t.Context()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// The park lands; the stamp that would have dated it does not.
	if err := rig.pending.MarkAwaiting(t.Context(), "t1", Clarification{
		Question: "which branch?", Audience: "requester", Branch: "wip/t1",
	}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if rig.get("t1").Paused() {
		t.Fatal("the row carries a pause instant, so this case is not the one it is named for")
	}

	// Dated from the park, which is the row's own last write.
	rig.now = rig.now.Add(DefaultPauseTTL - time.Second)
	rig.tick()
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v inside the TTL, measured from the park", killed)
	}

	rig.now = rig.now.Add(2 * time.Second)
	rig.tick()

	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the undated snapshot %q reclaimed", killed, run.SandboxID)
	}
	if got := rig.get("t1"); got.Status != StatusReseed {
		t.Fatalf("status = %q, want %q so the answer still resumes the turn", got.Status, StatusReseed)
	}
}

// The fallback is scoped to the wait it exists for. Every other pause lasts one
// dispatch and is settled by the tail that made it, so an unstamped row there
// is a LIVE box — dating it from the last write would reclaim a checkout out
// from under the turn using it.
func TestAnUnstampedBoxTheEngineIsDrivingIsNotTreatedAsHeld(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")

	for _, status := range []string{StatusRunning, StatusResumed} {
		if err := rig.pending.SetStatus(t.Context(), "t1", status, Fence{}); err != nil {
			t.Fatalf("SetStatus %s: %v", status, err)
		}
		if _, held := rig.get("t1").HeldSince(); held {
			t.Fatalf("a %s run with no pause stamp reads as a held snapshot", status)
		}
	}
	rig.now = rig.now.Add(100 * time.Hour)
	rig.tick()
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("the reaper killed %v out from under a live tail", killed)
	}
	if rig.provider.Box(run.SandboxID) == nil {
		t.Fatal("the box is gone")
	}
}

// withPaused is the stale snapshot a reaper decides from.
func withPaused(run PendingRun, at time.Time) PendingRun {
	run.Status = StatusAwaiting
	run.PausedAt = at
	run.PauseTTLSeconds = DefaultPauseTTL.Seconds()
	return run
}

// ---------------------------------------------------------------------
// the duty gate
// ---------------------------------------------------------------------

// N nodes polling means N reconnects per box per tick and N racing reapers.
func TestANodeWithoutTheDutyDoesNothing(t *testing.T) {
	rig := newWaiterRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true})

	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
		ClaimDuty: func(context.Context) (bool, error) { return false, nil },
	})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	fired, err := waiter.Tick(t.Context())
	if err != nil || fired != 0 {
		t.Fatalf("Tick = %d, %v; want a node without the duty to stand down", fired, err)
	}
	if rig.queue.count() != 0 {
		t.Fatal("a node without the duty published a completion")
	}
}

// FAIL CLOSED: not knowing whether this node holds the duty and polling anyway
// is the multi-poller case the duty exists to prevent, and a skipped tick
// costs one interval.
func TestAnUnreadableDutyStandsDownRatherThanPollingAnyway(t *testing.T) {
	rig := newWaiterRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true})

	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
		ClaimDuty: func(context.Context) (bool, error) {
			return false, errors.New("coordination store unreachable")
		},
	})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	if fired, err := waiter.Tick(t.Context()); err != nil || fired != 0 {
		t.Fatalf("Tick = %d, %v; want a stand-down", fired, err)
	}
}

// ---------------------------------------------------------------------
// the loop
// ---------------------------------------------------------------------

func TestTheLoopTicksAndStopsCleanly(t *testing.T) {
	rig := newWaiterRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true})

	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
		Interval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	waiter.Start(t.Context())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && rig.queue.count() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	waiter.Stop()
	if rig.queue.count() == 0 {
		t.Fatal("the loop never fired a completion")
	}
	// Stop is idempotent and returns only once the in-flight tick is done.
	waiter.Stop()
}

func TestAWaiterNeedsItsCollaborators(t *testing.T) {
	if _, err := NewWaiter(WaiterOptions{}); err == nil {
		t.Fatal("a waiter with no queue, store or manager was accepted")
	}
}

// A COMPLETION CARRIES THE UNIT OF WORK TOO, by the same rule and for the same
// reason: see TestAnAnnouncementCarriesTheUnitOfWorkOfAPreSplitRun. This is
// the announcement the dashboard's board reads and the one that routes the
// resume, so a blank key here is a resumed turn whose writes dedupe against
// nothing.
func TestACompletionCarriesTheUnitOfWorkOfAPreSplitRun(t *testing.T) {
	const preSplit = "0123456789abcdef0123456789abcdef"

	rig := newWaiterRig(t)
	rig.launch(preSplit)
	rig.runner.Finish(Result{Success: true})
	rig.tick()

	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	payload, ok := rig.queue.published[0].event.Data.(*types.SandboxRunCompleted)
	if !ok {
		t.Fatalf("payload is %T", rig.queue.published[0].event.Data)
	}
	if payload.WorkKey != preSplit {
		t.Errorf("WorkKey = %q, want the pre-split run's unit of work %q",
			payload.WorkKey, preSplit)
	}
}
