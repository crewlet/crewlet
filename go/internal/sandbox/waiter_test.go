package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// recorder captures published events, keyed by topic.
type recorder struct {
	mu        sync.Mutex
	published []publication
	err       error
}

type publication struct {
	topic string
	event *events.Event
}

func (r *recorder) Publish(_ context.Context, topic string, ev *events.Event) error {
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
	pending  *MemoryStore
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
		pending:  NewMemoryStore(),
		provider: NewFakeProvider(),
		runner:   NewFakeRunner("claude-code"),
		now:      time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
	manager, err := NewManager(ManagerOptions{
		Provider: rig.provider,
		Runners:  map[string]Runner{"claude-code": rig.runner},
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

// launch seeds a running detached job with a real box behind it.
func (r *waiterRig) launch(turnID string) PendingRun {
	r.t.Helper()
	ctx := r.t.Context()
	box, err := r.provider.Create(ctx, Spec{})
	if err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	run := PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: "a-1", Role: "SWE",
		CodingAgent: "claude-code", ConversationKey: "chat:C1",
		TraceID: "tr-1", SpanID: "sp-1", CreatedAt: r.now,
	}
	if err := r.pending.Create(ctx, run); err != nil {
		r.t.Fatalf("Create row: %v", err)
	}
	if err := r.pending.AttachSandbox(ctx, turnID, BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1",
		CodingAgent: "claude-code", PauseTTLSec: DefaultPauseTTL.Seconds(),
	}, Fence{}); err != nil {
		r.t.Fatalf("AttachSandbox: %v", err)
	}
	return r.get(turnID)
}

func (r *waiterRig) get(turnID string) PendingRun {
	r.t.Helper()
	run, ok, err := r.pending.Get(r.t.Context(), turnID)
	if err != nil || !ok {
		r.t.Fatalf("Get %s = %v, %v", turnID, ok, err)
	}
	return run
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
		topics.AgentControl("swe"),
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

	for i := 1; i < MaxConnectFailures; i++ {
		if fired := rig.tick(); fired != 0 {
			t.Fatalf("gave up after %d failures, want %d", i, MaxConnectFailures)
		}
	}
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d on the %dth failure, want 1", fired, MaxConnectFailures)
	}
}

// One reachable tick means the box is there; a later blip must start counting
// from zero rather than inheriting a streak from an hour ago.
func TestASuccessfulReconnectClearsTheFailureStreak(t *testing.T) {
	rig := newWaiterRig(t)
	run := rig.launch("t1")

	rig.provider.Vanished[run.SandboxID] = true
	for range MaxConnectFailures - 1 {
		rig.tick()
	}
	delete(rig.provider.Vanished, run.SandboxID)
	rig.tick() // reachable again

	rig.provider.Vanished[run.SandboxID] = true
	for i := 1; i < MaxConnectFailures; i++ {
		if fired := rig.tick(); fired != 0 {
			t.Fatalf("the old streak carried over: gave up on failure %d", i)
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
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1"); err != nil || !won {
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
