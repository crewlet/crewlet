package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

	// current is what the waiter's manager source answers: the rig's own
	// manager unless a case swaps it, the way an apply does.
	current atomic.Pointer[Manager]
}

// managers is the rig's manager source, as the coordinator's is the engine's.
func (r *waiterRig) managers() *Manager { return r.current.Load() }

func newWaiterRig(t *testing.T) *waiterRig {
	t.Helper()
	return newWaiterRigIn(t, Direct)
}

// newWaiterRigIn is a rig whose one backend, and so every run it launches, is
// in cell. Its runs name no launch node, and neither does its waiter: a test's
// node, which drives them.
func newWaiterRigIn(t *testing.T, cell Placement) *waiterRig {
	t.Helper()
	rig := &waiterRig{
		t:        t,
		queue:    &recorder{},
		provider: NewFakeProvider(),
		runner:   NewFakeRunner("claude-code"),
		now:      time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
	rig.runner.Thread = testThreadTraits["claude-code"]
	// ONE CLOCK FOR THE WHOLE WORLD, the store's included. The row's own
	// last write dates a pause nothing stamped ([PendingRun.HeldSince]), so
	// a store left on the wall clock would date it years after the instant
	// a test advances to and no age assertion below would mean anything.
	rig.pending = NewCoordStore(memory.NewFleet()).WithClock(func() time.Time { return rig.now })
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{cell: rig.provider},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rig.manager = manager
	rig.current.Store(manager)
	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
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
	return r.launchingUnder(turnID, rigLease)
}

// launchingUnder is [waiterRig.launching] under a lease of the case's choosing —
// the zero one for a run a node that held no lease launched, which a row stays
// at until something re-stamps it.
func (r *waiterRig) launchingUnder(turnID string, lease Fence) PendingRun {
	r.t.Helper()
	return r.launchingAs(turnID, lease, func(*PendingRun) {})
}

// launchWith is [waiterRig.launch] of a run the case edits before it is
// written — given repositories on Gitlet, say, as a launch writes them.
func (r *waiterRig) launchWith(turnID string, edit func(*PendingRun)) PendingRun {
	r.t.Helper()
	r.launchingAs(turnID, rigLease, edit)
	r.suspend(turnID)
	return r.get(turnID)
}

// launchingAs is [waiterRig.launchingUnder] for a row the case shapes before
// the write that opens it — a turn a colleague's ask woke, say, whose row the
// launch writes the ask onto, or one given repositories on Gitlet, as
// a launch writes them.
func (r *waiterRig) launchingAs(turnID string, lease Fence, shape func(*PendingRun)) PendingRun {
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
		Harness: "claude-code", PartitionKey: "chat:D1:root-1",
		ConversationKey: "chat:D1",
		TraceID:         "tr-1", SpanID: "sp-1", CreatedAt: r.now,
		// The item the launching turn was on, so every case runs over a
		// row that carries one — and a write that dropped it would show.
		// The unit of work likewise.
		WorkItem: &rigItem, WorkKey: rigWorkKey,
		// The model the launch pointed its agent at, which the launch's
		// own record is filed under.
		Launch: LaunchRecord{Model: "claude-sonnet-5"},
	}
	shape(&run)
	if _, err := r.pending.BeginLaunch(ctx, run, lease); err != nil {
		r.t.Fatalf("BeginLaunch: %v", err)
	}
	// THE SESSION THE LAUNCH NAMED, as a runner names it before its harness
	// starts: what a run carried on in its box continues
	// ([PendingRun.SessionRan]).
	// A minting harness names at its launch only the session it continues
	// (minted.go), which a case shapes onto the run.
	session := "sess-" + turnID
	if r.runner.MintsSession() {
		session = run.SessionID
	}
	if err := r.pending.AttachSandbox(ctx, turnID, BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", SessionID: session,
		Harness: "claude-code", PauseTTLSec: DefaultPauseTTL.Seconds(),
	}, lease); err != nil {
		r.t.Fatalf("AttachSandbox: %v", err)
	}
	return r.get(turnID)
}

// rigIteration is the executor iteration every rig launch suspends in. Not 1,
// so a record that fell back to a default would show.
const rigIteration = 2

// rigLease is the seat lease every rig launch is made under, which the row is
// stamped with as a real launch's is ([PendingStore.BeginLaunch]): its node's
// writes carry it, and a seat's next holder — every successor a case hands the
// seat to recovers it at a higher epoch — fences them out.
var rigLease = Fence{Owner: "node-a:1", Epoch: 1}

// leased is a coordinator's lease seam for a node that holds every seat under
// fence ([CoordinatorOptions.Lease]).
func leased(fence Fence) func(string) (Fence, bool) {
	return func(string) (Fence, bool) { return fence, true }
}

// notLeased is the lease seam of a node that has a seat host and holds none of
// its seats: it has noticed it lost them.
func notLeased(string) (Fence, bool) { return Fence{}, false }

// rigItem is the work item every rig launch is charged to.
var rigItem = types.WorkItem{Backend: types.WorkWorklet, ID: "task-1", Key: "ENG-1", Project: "ENG"}

// rigWorkKey is the unit of work every rig launch was dispatched for —
// distinct from every turn id a case launches under, so an announcement that
// carried one in place of the other shows.
const rigWorkKey = "0123456789abcdef0123456789abcdef"

// suspend writes the suspended conversation a real suspending turn would have
// written, which is what opens the run to the completion poll.
func (r *waiterRig) suspend(turnID string) {
	r.t.Helper()
	r.suspendIn(r.t.Context(), turnID)
}

// suspendIn is suspend from inside a call the coordinator made — a resumed
// turn's own work — under that call's context.
func (r *waiterRig) suspendIn(ctx context.Context, turnID string) {
	r.t.Helper()
	suspended, err := r.pending.MarkSuspended(ctx, turnID, Suspension{State: json.RawMessage(
		`{"version":4,"iteration":1}`),
		Iteration: rigIteration})
	if err != nil {
		r.t.Fatalf("MarkSuspended: %v", err)
	}
	if !suspended {
		r.t.Fatalf("MarkSuspended %s: the launch did not open to the poll", turnID)
	}
}

// ask records ask as the question the job the row holds now asked, the way the
// run's ask tool does over its bridge ([PendingStore.RecordAsk]), failing the
// case unless it was recorded. A collection parks a run on what its row holds,
// so a case whose run asks records the ask while the job is running and then
// finishes the runner with the rest of what it reports.
func (r *waiterRig) ask(turnID string, ask Ask) {
	r.t.Helper()
	run := r.get(turnID)
	got, err := r.pending.RecordAsk(r.t.Context(), turnID, run.LaunchID, ask)
	if err != nil || got != AskRecorded {
		r.t.Fatalf("RecordAsk on %s (%s) = %q, %v; want it recorded", turnID, run.Status, got, err)
	}
}

// whichBranch is the question a rig's run asks where the case is about the
// park rather than what was asked: put to the requester, as most are.
func whichBranch() Ask {
	return Ask{Question: "which branch?", To: AskToRequester, Decision: choosing("which branch?")}
}

func (r *waiterRig) get(turnID string) PendingRun {
	r.t.Helper()
	return r.getIn(r.t.Context(), turnID)
}

// getIn is get from inside a call the coordinator made, under that call's
// context.
func (r *waiterRig) getIn(ctx context.Context, turnID string) PendingRun {
	r.t.Helper()
	run, ok, err := r.pending.Get(ctx, turnID)
	if err != nil || !ok {
		r.t.Fatalf("Get %s = %v, %v", turnID, ok, err)
	}
	return run
}

// callsIn is the bridged tool log of turnID's run, read from inside a call the
// coordinator made, under that call's context.
func (r *waiterRig) callsIn(ctx context.Context, turnID string) BridgeLog {
	r.t.Helper()
	log, err := r.pending.BridgeLog(ctx, r.getIn(ctx, turnID))
	if err != nil {
		r.t.Fatalf("BridgeLog %s = %v", turnID, err)
	}
	return log
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
	t.Parallel()
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

// A FINISHED RUN NOBODY CLAIMED IS ANNOUNCED AGAIN, ONE POLL INTERVAL LATER.
//
// The announcement is a signal, not the work: the run's row is. So a completion
// whose delivery was acked without a claim (a node that may not start the turn
// yet, on a condition that clears by waiting) loses nothing, because the run is
// still running and a later poll fires it again. That is what lets the control
// topic hand such a completion back without spending a delivery.
//
// Mutation: fire a run's completion once and remember it for good, and the poll
// an interval later fires nothing for a run that is still waiting for its claim.
func TestAnUnclaimedCompletionIsAnnouncedAgain(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true})
	for tick := 1; tick <= 2; tick++ {
		if fired := rig.tick(); fired != 1 {
			t.Fatalf("tick %d fired %d completions for a finished run nobody claimed, want 1",
				tick, fired)
		}
		rig.now = rig.now.Add(DefaultPollInterval)
	}
}

// A JOB NOBODY RESUMES IS RECORDED AS FINISHED ONCE, AND ANNOUNCED TO ITS SEAT
// ON A DOUBLING SCHEDULE. Announced on every poll, a finished health check whose
// review met a model's weekly limit put 6280 completions on the event stream in
// 27 hours. The event stream's copy is the record that the job finished, and
// it goes out once per job; the seat's copy is the backstop for a condition
// that clears with nothing to say so, and it waits twice as long each time, up
// to the ceiling. A new job of the same turn starts again.
//
// Mutation: announce on every poll, or publish the event stream's copy with
// every announcement, and the counts below are the poll's.
func TestAJobNobodyResumesIsAnnouncedOnASchedule(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true})
	public := func() int {
		n := 0
		for _, topic := range rig.queue.topics() {
			if topic == topics.Event(types.ExecutorFinished{}.EventType()) {
				n++
			}
		}
		return n
	}

	var fired []time.Duration
	start := rig.now
	for elapsed := time.Duration(0); elapsed <= 27*time.Hour; elapsed += DefaultPollInterval {
		rig.now = start.Add(elapsed)
		if rig.tick() == 1 {
			fired = append(fired, elapsed)
		}
	}
	if public() != 1 {
		t.Fatalf("the event stream recorded the job's completion %d times, want once", public())
	}
	want := []time.Duration{0, DefaultPollInterval, 3 * DefaultPollInterval, 7 * DefaultPollInterval}
	if len(fired) < len(want) || !slices.Equal(fired[:len(want)], want) {
		t.Fatalf("announced at %v, want the schedule to begin %v", fired, want)
	}
	if limit := int(27*time.Hour/(reannounceCeilingPolls*DefaultPollInterval)) + 10; len(fired) > limit {
		t.Fatalf("announced %d times in 27 hours, want at most %d", len(fired), limit)
	}
	for i := 1; i < len(fired); i++ {
		if gap := fired[i] - fired[i-1]; gap > reannounceCeilingPolls*DefaultPollInterval+DefaultPollInterval {
			t.Fatalf("announcement %d came %v after the one before, past the ceiling", i, gap)
		}
	}

	// A NEW JOB OF THE SAME TURN IS NEWS AGAIN.
	relaunch := rig.launch("t1")
	rig.runner.Finish(Result{Success: true})
	if fired := rig.tick(); fired != 1 || public() != 2 {
		t.Fatalf("the turn's next job %s fired %d and recorded %d completions in all, want "+
			"it announced and recorded at once", relaunch.LaunchID, fired, public())
	}
}

// Two publishes, two purposes: the announcement feeds the dashboard's
// broadcast stream, the control copy reaches the one node that holds the
// suspended conversation.
func TestACompletionIsAnnouncedAndRoutedToTheSeatsOwner(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true})
	rig.tick()

	got := rig.queue.topics()
	want := []string{
		topics.Event(types.ExecutorFinished{}.EventType()),
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
	t.Parallel()
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

// AN ANNOUNCEMENT SAYS WHEN ITS WAITER ASKED THE STORE FOR THE ROW IT WAS MADE
// FROM, NEVER WHEN IT WAS MADE. The seat's holder reads that instant as the
// store of the box's host answering since it found that store silent, which
// ends its renewal of the job's completion ([announcedAfter]), and the box is
// polled between the read and the announcement: a poll that takes a minute
// leaves the announcement a minute after the read, and a silence that began in
// between, read off the announcement's own instant, was read as over while it
// lasted.
//
// Mutation: say the instant the announcement is made, or none, and the read it
// says is a minute late, or missing.
func TestAnAnnouncementSaysWhenItsRowWasRead(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t1")
	clock := &stepClock{at: rig.now}
	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
		Now: clock.now,
	})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	// THE POLL TAKES A MINUTE, and finds the job over.
	rig.runner.PollFunc = func(context.Context, Sandbox) (JobState, error) {
		clock.advance(time.Minute)
		return JobEnded, nil
	}
	read := clock.now()
	if fired, err := waiter.Tick(t.Context()); err != nil || fired != 1 {
		t.Fatalf("the pass announced %d completions, %v; want the finished job announced", fired, err)
	}
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	announced := 0
	for _, p := range rig.queue.published {
		if _, ok := p.event.Data.(*types.ExecutorFinished); !ok {
			continue
		}
		announced++
		if asked := rowAsked(p.event); !asked.Equal(read) {
			t.Fatalf("the announcement to %s says its row was read at %v; want %v, when the pass asked the "+
				"store for its listing, a minute before the poll ended", p.topic, asked, read)
		}
	}
	if announced == 0 {
		t.Fatal("the pass announced nothing")
	}
}

// steppingRows is a run store whose every read of a run's row, and every
// record of a job's loss, takes step on clock before it answers: the time the
// store takes, on the case's clock, so an instant taken after the store was
// asked is not the instant it was asked.
type steppingRows struct {
	PendingStore
	clock *stepClock
	step  time.Duration
}

func (s *steppingRows) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	s.clock.advance(s.step)
	return s.PendingStore.Get(ctx, turnID)
}

func (s *steppingRows) MarkLost(ctx context.Context, turnID, launch string, loss JobLoss) (bool, error) {
	s.clock.advance(s.step)
	return s.PendingStore.MarkLost(ctx, turnID, launch, loss)
}

// rowAskedOfEvery holds every completion rig's queue carried to the instant
// want, the one its waiter asked its store at, and fails the case where none
// was announced.
func rowAskedOfEvery(t *testing.T, rig *waiterRig, want time.Time, why string) {
	t.Helper()
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	announced := 0
	for _, p := range rig.queue.published {
		if _, ok := p.event.Data.(*types.ExecutorFinished); !ok {
			continue
		}
		announced++
		if asked := rowAsked(p.event); !asked.Equal(want) {
			t.Errorf("the announcement to %s says its row was read at %v; want %v, %s", p.topic, asked, want, why)
		}
	}
	if announced == 0 {
		t.Fatal("nothing was announced")
	}
}

// A NUDGE'S ANNOUNCEMENT AND A GONE NODE'S SAY WHEN THEIR STORE WAS ASKED,
// NEVER WHEN THEY WERE MADE: the read's or the record's instant, as a pass's
// says its listing's (TestAnAnnouncementSaysWhenItsRowWasRead), since a later
// instant reads a silence that began after the ask as over ([announcedAfter]).
//
// Mutation: stamp the instant the announcement is made, or one taken after the
// store was asked, on either road, and its announcement says a later read.
func TestEveryRoadsAnnouncementSaysWhenItsStoreWasAsked(t *testing.T) {
	t.Parallel()
	// waiterOn builds rig's waiter on clock, its store taking a second on
	// the clock for each read of a run's row and each record of a loss, and
	// every poll of a box a minute, which finds the job over.
	waiterOn := func(t *testing.T, rig *waiterRig, clock *stepClock) *Waiter {
		t.Helper()
		rig.runner.PollFunc = func(context.Context, Sandbox) (JobState, error) {
			clock.advance(time.Minute)
			return JobEnded, nil
		}
		waiter, err := NewWaiter(WaiterOptions{
			Queue: rig.queue, Pending: &steppingRows{PendingStore: rig.pending, clock: clock, step: time.Second},
			Manager: rig.managers, Now: clock.now,
		})
		if err != nil {
			t.Fatalf("NewWaiter: %v", err)
		}
		return waiter
	}

	t.Run("a nudge's", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			rig := newWaiterRig(t)
			run := rig.launch("t1")
			clock := &stepClock{at: rig.now}
			waiter := waiterOn(t, rig, clock)
			asked := clock.now()
			waiter.follow(t.Context(), Nudge{TurnID: "t1", LaunchID: run.LaunchID, Why: NudgeSubmitted})
			rowAskedOfEvery(t, rig, asked, "when the nudge asked the store for the run's row, a second "+
				"before it answered and a minute before the poll ended")
		})
	})

	t.Run("a gone launch node's", func(t *testing.T) {
		t.Parallel()
		rig := newWaiterRig(t)
		run := rig.launch("t1")
		clock := &stepClock{at: rig.now}
		waiter := waiterOn(t, rig, clock)
		asked := clock.now()
		waiter.endGone(t.Context(), run, rig.now.Add(-time.Hour), rig.now.Add(-time.Minute))
		rowAskedOfEvery(t, rig, asked, "when the waiter asked the store to record the job's loss, a second "+
			"before it answered")
	})
}

// A completion event is a pure control signal — the outcome is read at collect
// time, so nothing about the result may ride on the wire.
func TestACompletionCarriesTheIdentityAndNotTheOutcome(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "shipped it"})
	rig.tick()

	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	payload, ok := rig.queue.published[0].event.Data.(*types.ExecutorFinished)
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
	if payload.AgentHandle != "swe" || payload.Harness != "claude-code" {
		t.Fatalf("payload = %+v", payload)
	}
}

// A transient poll error is not a completion: firing on one would collect a
// result the job has not written.
func TestATransientPollErrorIsRetriedRatherThanFired(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.provider.Vanished[run.SandboxID] = true

	// Two failures, but no time has passed: giving up is TERMINAL for the
	// job — the completion fires, and a box that is gone has its job lost
	// and its round started again — so a burst of probes must not be able to
	// spend the window on its own.
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
	t.Parallel()
	rig := newWaiterRig(t)
	fast, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t1")
	if err := rig.pending.AttachSandbox(t.Context(), "t1", BoxRef{
		SandboxID: rig.get("t1").SandboxID, Harness: "retired-agent",
	}, Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d, want the run freed", fired)
	}
}

// The engine imposes NO run-time limit on a launched job — the box is bounded
// only by how long the engine can go without a heartbeat.
func TestEveryTickKeepsTheRunningBoxAlive(t *testing.T) {
	t.Parallel()
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

// THE POLL NEVER WAKES A BOX ITS RUN HAS MOVED ON FROM. A poll that listed a
// running run, then reached its box after the collection had claimed the run
// and paused the box, would resume it — Connect wakes whatever it reaches —
// and nothing polls a row that is not running, so its job would run on with
// nothing watching it. The poll attaches without resuming, and a paused box
// sends it to the record, which says the run has moved on.
//
// Mutation: poll through Connect, and the box is woken and heart-beaten.
func TestThePollNeverWakesABoxItsRunHasMovedOnFrom(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	listed := rig.launch("t1")
	box := rig.provider.Box(listed.SandboxID)
	rig.runner.Finish(Result{Text: "done", Success: true})

	// The collection claimed the run off running, read the box and paused
	// it, between the poll's listing and its reach.
	if _, ok, err := rig.pending.ClaimForResume(t.Context(), "t1", CompletionTail(listed.LaunchID), rigLease); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if err := box.Pause(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got := rig.waiter.pollOne(t.Context(), rig.manager, listed); got != pollRunning {
		t.Errorf("the poll of a run that moved on = %v; want nothing done", got)
	}
	if !box.Paused() {
		t.Fatal("the poll woke a box its run no longer runs")
	}
	if box.Keepalives() != 0 {
		t.Errorf("the poll heart-beat a box its run no longer runs %d time(s)", box.Keepalives())
	}
}

// A PAUSED BOX UNDER A RUN STILL RUNNING A JOB NO COLLECTION READ BACK IS
// WOKEN, because that is the run's box and only the poll can say whether its
// job finished: it has to be kept alive and polled. The read-only attach must
// not cost that. (A job a collection read back and handed back to running is
// announced with its box left asleep instead, since it is over:
// TestAReadBackJobsBoxSleepsUntilItIsCollectedAgain.)
//
// Mutation: leave every paused box alone, and this run's box is never
// heart-beaten or polled again.
func TestThePollWakesAPausedBoxItsRunStillRuns(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	box := rig.provider.Box(run.SandboxID)
	if err := box.Pause(t.Context()); err != nil {
		t.Fatal(err)
	}
	rig.runner.Finish(Result{Text: "done", Success: true})

	if fired := rig.tick(); fired != 1 {
		t.Errorf("the run fired %d completion(s); want its finished job announced", fired)
	}
	if box.Paused() || box.Keepalives() != 1 {
		t.Errorf("paused=%v keepalives=%d; want the run's box woken and kept alive",
			box.Paused(), box.Keepalives())
	}
}

// A run parked on a question is not running: the seat is free and the box is
// deliberately NOT heart-beaten, because the pause TTL is what bounds it.
func TestAParkedRunIsNeitherPolledNorHeartBeaten(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	box := rig.provider.Box(run.SandboxID)
	rig.ask("t1", whichBranch())
	if err := rig.pending.MarkAwaiting(t.Context(), "t1", Clarification{AskedAt: rig.now}); err != nil {
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
	t.Parallel()
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
	t.Parallel()
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

// park has a launched run ask a question and moves it onto it, with its box
// paused.
func (r *waiterRig) park(turnID string) {
	r.t.Helper()
	ctx := r.t.Context()
	r.ask(turnID, Ask{Question: "which branch?", To: AskToRequester, Decision: choosing("which branch?"), Branch: "wip/t1"})
	if err := r.pending.MarkAwaiting(ctx, turnID, questionAt(r.now)); err != nil {
		r.t.Fatalf("MarkAwaiting: %v", err)
	}
	if err := r.pending.MarkBoxPaused(ctx, turnID, r.now); err != nil {
		r.t.Fatalf("MarkBoxPaused: %v", err)
	}
}

func TestAPauseInsideItsTtlIsLeftAlone(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		t.Fatal("paused_at survived the reap — the row still claims a paused box exists")
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want [%s]", killed, run.SandboxID)
	}
	if got.Branch != "wip/t1" {
		t.Fatalf("branch = %q — the durable half of the work was lost", got.Branch)
	}
}

// A recorded answer waits on its resume, and on a seat no node holds that
// wait is as open-ended as the question's was. Skipping answered rows left
// exactly those boxes paused and held; the answer itself must survive the
// reap, since the resume re-seeds from the branch.
func TestTheReaperReclaimsTheBoxOfAnAnswerStillWaitingOnItsResume(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.park("t1")
	parked := rig.get("t1")
	if _, ok, err := rig.pending.RecordAnswer(t.Context(), "t1", parked.LaunchID, sealedOnto(t, rigAnswers(),
		parked, chatWords("use main"), RecordedAnswer{EventIDs: []string{"r1"}, RecordedAt: rig.now},
	), rigLease); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}

	rig.now = rig.now.Add(DefaultPauseTTL + time.Second)
	rig.tick()

	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the answered run's box %q reclaimed", killed, run.SandboxID)
	}
	got := rig.get("t1")
	if got.Status != StatusAnswered || got.Answer == nil || got.SandboxID != "" {
		t.Fatalf("run = %q answer %+v box %q, want the answer kept and the box forgotten",
			got.Status, got.Answer, got.SandboxID)
	}
}

// Connect auto-resumes, so reclaiming through it would boot the work back up
// purely to shut it down.
func TestTheReaperKillsByIdRatherThanConnecting(t *testing.T) {
	t.Parallel()
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
// this one down when the run blocked, so there is no paused box left to expire.
func TestAZeroPauseTtlIsNotADeadlineForTheReaper(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t1")
	if err := rig.pending.AttachSandbox(t.Context(), "t1", BoxRef{
		SandboxID: rig.get("t1").SandboxID, Harness: "claude-code", PauseTTLSec: 0,
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
		t.Fatalf("killed %v for a run that holds no paused box", killed)
	}
}

// Every other paused box belongs to a tail being actively driven; expiring one
// from here would kill it out from under live work.
func TestTheReaperTouchesOnlyRunsParkedOnAQuestion(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.park("t1")

	// The answer arrives between the reaper's snapshot and its flip.
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1", Tail{Launch: rig.get("t1").LaunchID, From: Awaiting}, rigLease); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	rig.now = rig.now.Add(DefaultPauseTTL + time.Second)
	rig.waiter.reapOne(t.Context(), rig.manager,
		withPaused(run, rig.now.Add(-DefaultPauseTTL-time.Second)))

	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("the reaper destroyed %v underneath a resume that had already claimed the run", killed)
	}
	if got := rig.get("t1"); got.Status != StatusResumed {
		t.Fatalf("status = %q, want the resume's claim to stand", got.Status)
	}
}

// The pause instant is a SECOND, warn-only write after the box is already
// paused. A run whose park then lands holds a paused box nothing dates — and a
// reaper that skipped it would leave that box paused, holding its directory
// and memory on its host, for ever.
func TestAParkedBoxWhosePauseWasNeverRecordedIsStillReaped(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	if err := rig.provider.Box(run.SandboxID).Pause(t.Context()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// The park lands; the stamp that would have dated it does not.
	rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Decision: choosing("which branch?"), Branch: "wip/t1"})
	if err := rig.pending.MarkAwaiting(t.Context(), "t1", Clarification{AskedAt: rig.now}); err != nil {
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
		t.Fatalf("killed %v, want the undated paused box %q reclaimed", killed, run.SandboxID)
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
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")

	for _, status := range []string{StatusRunning, StatusResumed} {
		if err := rig.pending.SetStatus(t.Context(), "t1", status, Fence{}); err != nil {
			t.Fatalf("SetStatus %s: %v", status, err)
		}
		if _, held := rig.get("t1").HeldSince(); held {
			t.Fatalf("a %s run with no pause stamp reads as a held paused box", status)
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
// the loop
// ---------------------------------------------------------------------

// THE LOOP TICKS ON ITS INTERVAL AND STOPS CLEANLY. Its first pass comes by
// the most its jitter makes of the interval it was given, on the bubble's
// clock, which keeps the interval exactly: a bound of seconds on the
// machine's clock passed a loop ticking on a hundred times its interval, and
// failed one a loaded runner kept from firing for as long.
//
// Mutation: tick on the default interval rather than the one configured, and
// no pass comes inside the configured one.
func TestTheLoopTicksAndStopsCleanly(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig := newWaiterRig(t)
		rig.launch("t1")
		rig.runner.Finish(Result{Success: true})

		const interval = 5 * time.Millisecond
		waiter, err := NewWaiter(WaiterOptions{
			Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
			Interval: interval,
		})
		if err != nil {
			t.Fatalf("NewWaiter: %v", err)
		}
		waiter.Start(t.Context())
		// THE FIRST PASS IS DUE BY THEN, and its polls have run once every
		// goroutine of the case is blocked again.
		time.Sleep(time.Duration(float64(interval) * (1 + jitterFraction)))
		synctest.Wait()
		if rig.queue.count() == 0 {
			t.Fatalf("the loop fired no completion within its %v interval", interval)
		}
		waiter.Stop()
		// Stop is idempotent and returns only once the in-flight tick is done.
		waiter.Stop()
	})
}

// A PASS LEAVES NOTHING BEHIND IN THE CONTEXT IT WAS GIVEN. That context is
// the loop's, which lives as long as the process, so a child of it nobody
// cancelled stays in its list of children until the engine stops — one more
// every pass, on every node. Every pass here holds the launch-node watch, so
// its verdict's context, bounded by the claim, is made too.
//
// Mutation: make the walk's context twice, overwriting the first cancel func,
// or leave the verdict's uncancelled, and every pass leaves one child
// registered.
func TestAPassLeavesNoChildOfItsContextBehind(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launchWith("t1", func(run *PendingRun) { run.LaunchNode = "node-a" })
	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.managers, Node: "node-a",
		Now:       func() time.Time { return rig.now },
		Roster:    &roster{},
		ClaimDuty: func(context.Context) (bool, error) { return true, nil },
		DutyTTL:   time.Minute,
	})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	parent := newChildCounter()
	defer parent.cancel()
	const passes = 5
	for range passes {
		if _, err := waiter.Tick(parent); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	// The walk cancels its own context as it returns, AFTER the last task
	// it started has told the round it is done — so the round's end is
	// not quite the walk's. Wait for every task the waiter started.
	waiter.tasks.Wait()
	if live := parent.live(); live != 0 {
		t.Fatalf("%d passes left %d children of their context registered; want none", passes, live)
	}
}

// childCounter is a context that counts the children registered on it and not
// yet cancelled. A context the standard library did not make, with an
// AfterFunc method, is one every child derived from it registers with through
// that method, and deregisters from by calling the stop it returned — which is
// exactly a child's lifetime in its parent's list.
type childCounter struct {
	context.Context
	done chan struct{}
	once sync.Once

	mu       sync.Mutex
	children map[int]func()
	next     int
}

func newChildCounter() *childCounter {
	return &childCounter{Context: context.Background(), done: make(chan struct{}), children: map[int]func(){}}
}

func (c *childCounter) Done() <-chan struct{} { return c.done }

func (c *childCounter) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func (c *childCounter) AfterFunc(f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.next
	c.next++
	c.children[id] = f
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.children[id]
		delete(c.children, id)
		return ok
	}
}

func (c *childCounter) live() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.children)
}

func (c *childCounter) cancel() {
	c.once.Do(func() {
		close(c.done)
		c.mu.Lock()
		children := c.children
		c.children = map[int]func(){}
		c.mu.Unlock()
		for _, f := range children {
			f()
		}
	})
}

// A MANAGER SWAPPED AFTER THE WAITER STARTED IS THE ONE THE NEXT TICK POLLS
// THROUGH — and the box a job is still running in stays reachable on a cell the
// new catalogue dropped.
//
// The waiter used to capture the manager it was built with, so after a reload
// it went on reconnecting through the boot catalogue's backends while every
// launch used the new ones: a job started after the reload was on a backend
// the poll had never heard of, and was declared gone.
func TestTheWaiterPollsThroughTheManagerCurrentAtEachTick(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	// A CATALOGUE THAT MOVED THE CELL TO ANOTHER BACKEND, the way a
	// reload rebuilds every backend it configures.
	moved := NewFakeProvider()
	next, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: moved},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rig.current.Store(next)
	box, err := moved.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The job has not finished, so this tick is a keepalive and nothing else.
	rig.seedRunning("t-after", box.ID())
	if _, err := rig.waiter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if moved.Box(box.ID()).Keepalives() == 0 {
		t.Fatal("the tick after a reload did not reach a box on the reloaded " +
			"backend — the waiter polled through the manager it was built with")
	}
}

// seedRunning records a running job on a box the case minted itself.
func (r *waiterRig) seedRunning(turnID, sandboxID string) {
	r.t.Helper()
	ctx := r.t.Context()
	if _, err := r.pending.BeginLaunch(ctx, PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: "a-1", Role: "SWE",
		Harness: "claude-code", CreatedAt: r.now,
	}, Fence{}); err != nil {
		r.t.Fatalf("BeginLaunch: %v", err)
	}
	if err := r.pending.AttachSandbox(ctx, turnID, BoxRef{
		SandboxID: sandboxID, CommandID: "cmd-1", Harness: "claude-code",
	}, Fence{}); err != nil {
		r.t.Fatalf("AttachSandbox: %v", err)
	}
	r.suspend(turnID)
}

func TestAWaiterNeedsItsCollaborators(t *testing.T) {
	t.Parallel()
	if _, err := NewWaiter(WaiterOptions{}); err == nil {
		t.Fatal("a waiter with no queue, store or manager was accepted")
	}
}

// A WAITER THAT REFRESHES THE BOXES IT POLLS RECORDS EACH TOKEN FIRST, or is
// not built: a token its refresh wrote into a box that nothing recorded beside
// the job's row would be a live secret no capture of the job's session is
// scrubbed of ([WaiterOptions.Record]).
//
// Mutation: build a waiter that refreshes with nothing to record in, or refuse
// one that has both, and a check below goes red.
func TestAWaiterThatRefreshesIsRefusedWithoutARecorder(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	opts := WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
		Refresh: func(context.Context, *Manager, PendingRun, Sandbox, RecordHanded) {},
	}
	if _, err := NewWaiter(opts); err == nil {
		t.Error("a waiter that refreshes the boxes it polls was built with nothing to record each token in")
	}
	opts.Record = func(context.Context, PendingRun, string, time.Time) error { return nil }
	if _, err := NewWaiter(opts); err != nil {
		t.Errorf("a waiter that refreshes and records was refused: %v", err)
	}
}

// A COMPLETION CARRIES THE UNIT OF WORK TOO, by the same rule and for the same
// reason: see TestAnAnnouncementCarriesTheRunsUnitOfWork. This is the
// announcement the dashboard's board reads and the one that routes the resume,
// so a blank key here is a resumed turn whose writes dedupe against nothing.
func TestACompletionCarriesTheRunsUnitOfWork(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("turn-1")
	rig.runner.Finish(Result{Success: true})
	rig.tick()

	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	payload, ok := rig.queue.published[0].event.Data.(*types.ExecutorFinished)
	if !ok {
		t.Fatalf("payload is %T", rig.queue.published[0].event.Data)
	}
	if payload.WorkKey != rigWorkKey {
		t.Errorf("WorkKey = %q, want the run's unit of work %q", payload.WorkKey, rigWorkKey)
	}
}

// A RUNNING BOX'S EXPIRING CREDENTIALS ARE REFRESHED ON EVERY POLL THAT
// REACHES IT, and a box its run has moved on from is never handed over.
//
// The poll is the only thing that reaches a running box for as long as its
// job runs, so it is what keeps a credential issued at launch, which expires
// within the hour, current in the box for a job that runs for hours. A box the
// run no longer runs is not the run's to refresh.
//
// Mutation: drop the refresh from the poll, or hand it the box before the
// poll's read of the record.
func TestEveryPollHandsTheRunningBoxToTheRefresh(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	var (
		mu     sync.Mutex
		handed []string
	)
	rig.waiter.refresh = func(_ context.Context, manager *Manager, run PendingRun, box Sandbox, _ RecordHanded) {
		mu.Lock()
		defer mu.Unlock()
		if manager != rig.manager {
			t.Error("the refresh was handed a manager other than the one that reached the box")
		}
		handed = append(handed, run.TurnID+"@"+box.ID())
	}
	run := rig.launch("t1")
	for range 3 {
		rig.tick()
	}
	mu.Lock()
	got := slices.Clone(handed)
	mu.Unlock()
	if len(got) != 3 || got[0] != "t1@"+run.SandboxID {
		t.Fatalf("refreshes = %v, want the running box on each of three polls", got)
	}

	moved := newWaiterRig(t)
	calls := 0
	moved.waiter.refresh = func(context.Context, *Manager, PendingRun, Sandbox, RecordHanded) { calls++ }
	listed := moved.launch("t2")
	moved.runner.Finish(Result{Text: "done", Success: true})
	if _, ok, err := moved.pending.ClaimForResume(t.Context(), "t2", CompletionTail(listed.LaunchID), rigLease); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if err := moved.provider.Box(listed.SandboxID).Pause(t.Context()); err != nil {
		t.Fatal(err)
	}
	moved.waiter.pollOne(t.Context(), moved.manager, listed)
	if calls != 0 {
		t.Error("a box its run has moved on from was handed to the refresh")
	}
}
