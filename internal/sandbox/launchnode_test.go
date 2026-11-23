package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// ---------------------------------------------------------------------
// a box is its launching node's
// ---------------------------------------------------------------------

// hostRig is a fleet's worth of cells for one test: a direct box and a
// container one, each cell its own backend, over one run store — every node of
// the fleet reads the same rows.
type hostRig struct {
	*waiterRig
	local     *FakeProvider
	container *FakeProvider
}

func newHostRig(t *testing.T) *hostRig {
	t.Helper()
	base := newWaiterRig(t)
	rig := &hostRig{waiterRig: base, local: base.provider, container: NewFakeProvider()}
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: rig.local, Container: rig.container},
		Runners:   map[string]Runner{"claude-code": base.runner},
		// A DEFAULT, as a catalogue of two cells always names, which the
		// rows below never fall back on: each names its own cell.
		DefaultPlacement: Container,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	base.manager = manager
	base.current.Store(manager)
	return rig
}

// launchIn seeds a running job in cell, launched by node: a box made for the
// turn in that cell's backend, the row naming both, and its suspension.
func (r *hostRig) launchIn(turnID string, cell Placement, node string) PendingRun {
	r.t.Helper()
	ctx := r.t.Context()
	provider := r.local
	if cell == Container {
		provider = r.container
	}
	box, err := provider.Create(ctx, Spec{TurnID: turnID, Placement: cell})
	if err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	if _, err := r.pending.BeginLaunch(ctx, PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: sweID, Role: "SWE",
		Harness: "claude-code", TraceID: "tr-1", CreatedAt: r.now, LaunchNode: node,
	}, rigLease); err != nil {
		r.t.Fatalf("BeginLaunch: %v", err)
	}
	if err := r.pending.AttachSandbox(ctx, turnID, BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", Placement: string(cell),
		Harness: "claude-code", PauseTTLSec: DefaultPauseTTL.Seconds(),
	}, rigLease); err != nil {
		r.t.Fatalf("AttachSandbox: %v", err)
	}
	r.suspend(turnID)
	return r.get(turnID)
}

// waiter is node's waiter. Given a roster, it holds the launch-node watch on
// every pass, as the one node of the fleet that judges gone launch nodes.
func (r *hostRig) waiter(node string, roster NodeRoster) *Waiter {
	r.t.Helper()
	opts := WaiterOptions{
		Queue: r.queue, Pending: r.pending, Manager: r.managers, Node: node,
		Now: func() time.Time { return r.now },
	}
	if roster != nil {
		opts.Roster, opts.ClaimDuty, opts.DutyTTL = roster, holding, time.Minute
	}
	w, err := NewWaiter(opts)
	if err != nil {
		r.t.Fatalf("NewWaiter: %v", err)
	}
	return w
}

// holding is a claim of the launch-node watch that this node always wins.
func holding(context.Context) (bool, error) { return true, nil }

// announced is every turn a completion was announced for, once each, in order.
func (r *hostRig) announced() []string {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	var turns []string
	for _, p := range r.queue.published {
		c, ok := p.event.Data.(*types.ExecutorFinished)
		if ok && !slices.Contains(turns, c.TurnID) {
			turns = append(turns, c.TurnID)
		}
	}
	return turns
}

func tickOnce(t *testing.T, w *Waiter) {
	t.Helper()
	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

// EACH WAITER DRIVES ONLY THE BOXES IT CAN REACH: those its node launched, in
// either cell. A box is a process tree or a container on the host of the node
// that launched it, and no other node can reach it: a waiter polling it from
// another node finds it gone, and gives a running job up within a minute.
//
// Mutation: let a node's waiter drive every run, or only one cell's, and a box
// another node launched is announced here, or one of its own is not.
func TestEachWaiterDrivesOnlyTheBoxesItCanReach(t *testing.T) {
	t.Parallel()
	rig := newHostRig(t)
	rig.launchIn("t-direct-on-a", Direct, "node-a")
	rig.launchIn("t-container-on-a", Container, "node-a")
	rig.launchIn("t-on-b", Direct, "node-b")
	rig.runner.Finish(Result{Success: true})

	for _, step := range []struct {
		name string
		w    *Waiter
		want []string
	}{
		{"node-a's waiter", rig.waiter("node-a", nil), []string{"t-container-on-a", "t-direct-on-a"}},
		{"node-b's waiter", rig.waiter("node-b", nil), []string{"t-container-on-a", "t-direct-on-a", "t-on-b"}},
	} {
		tickOnce(t, step.w)
		if got := slices.Sorted(slices.Values(rig.announced())); !slices.Equal(got, step.want) {
			t.Errorf("after %s, completions were announced for %v; want %v", step.name, got, step.want)
		}
	}
}

// A PAUSE IS REAPED ONLY ON ITS BOX'S HOST. Reaped from anywhere else, the
// flip released the row and the kill found no such box — so the paused box
// was held for ever on the host that has it, with nothing naming it.
//
// Mutation: let any node's waiter reap a run's pause, and node-b's releases
// node-a's run with its box still standing.
func TestAPauseIsReapedOnlyOnItsBoxsHost(t *testing.T) {
	t.Parallel()
	rig := newHostRig(t)
	run := rig.launchIn("t-parked", Direct, "node-a")
	rig.park("t-parked")
	rig.now = rig.now.Add(DefaultPauseTTL + time.Second)

	tickOnce(t, rig.waiter("node-b", nil))
	if got := rig.get("t-parked"); got.Status != StatusAwaiting || got.SandboxID != run.SandboxID {
		t.Fatalf("a waiter that cannot reach the box released its pause: %q naming %q", got.Status, got.SandboxID)
	}
	if killed := rig.local.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v from a waiter that cannot reach the box", killed)
	}

	tickOnce(t, rig.waiter("node-a", nil))
	if got := rig.get("t-parked"); got.Status != StatusReseed {
		t.Errorf("the box's own host left the expired pause as %q, want reseed", got.Status)
	}
	if killed := rig.local.KilledIDs(); !slices.Contains(killed, run.SandboxID) {
		t.Errorf("the box's own host killed %v, want its box %s", killed, run.SandboxID)
	}
}

// A WAITER TAKES THE ROSTER AND THE LAUNCH-NODE WATCH TOGETHER, or neither: a
// roster with no claim would have every node judge, or none, and a claim with
// no roster would hold the watch to judge nothing. A claim needs a TTL past the
// interval, which bounds the verdict a pass makes.
//
// Mutation: build either alone, or a claim whose TTL the interval reaches.
func TestAWaiterTakesTheRosterAndTheLaunchNodeWatchTogether(t *testing.T) {
	t.Parallel()
	rig := newHostRig(t)
	base := WaiterOptions{Queue: rig.queue, Pending: rig.pending, Manager: rig.managers}
	for name, refused := range map[string]func(*WaiterOptions){
		"a roster with no claim": func(o *WaiterOptions) { o.Roster = &roster{} },
		"a claim with no roster": func(o *WaiterOptions) { o.ClaimDuty, o.DutyTTL = holding, time.Minute },
		"a claim whose TTL the interval reaches": func(o *WaiterOptions) {
			o.Roster, o.ClaimDuty, o.DutyTTL, o.Interval = &roster{}, holding, time.Minute, time.Minute
		},
	} {
		opts := base
		refused(&opts)
		if _, err := NewWaiter(opts); err == nil {
			t.Errorf("a waiter was built with %s", name)
		}
	}
	both := base
	both.Roster, both.ClaimDuty, both.DutyTTL = &roster{}, holding, time.Minute
	if _, err := NewWaiter(both); err != nil {
		t.Errorf("a waiter with the roster and the watch was refused: %v", err)
	}
}

// ---------------------------------------------------------------------
// a launch node that never comes back
// ---------------------------------------------------------------------

// roster is the fleet's presence as a case sets it, counting the questions it
// is asked.
type roster struct {
	mu      sync.Mutex
	present map[string]bool
	err     error
	asked   int
}

func (r *roster) Present(_ context.Context, node string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked++
	return r.present[node], r.err
}

func (r *roster) questions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asked
}

func (r *roster) set(node string, present bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.present == nil {
		r.present = map[string]bool{}
	}
	r.present[node] = present
}

// lostAt is when a row's job was found lost, zero for one that was not.
func lostAt(run PendingRun) time.Time {
	if run.Lost == nil {
		return time.Time{}
	}
	return run.Lost.At
}

// A LAUNCH NODE GONE FOR THE HOST GRACE HAS ITS RUNNING JOB LOST: its box went
// with it, and nothing else can poll, collect or reclaim it, so the seat would
// wait behind the run for ever. The verdict is recorded on the row as the
// node's loss and the completion announced, for the seat's holder to carry the
// job's round on — and announced again on its schedule, as a finished job's
// is, until a claim takes it: the first time on the pass an interval later.
//
// NEVER AT ONE INSTANT'S ABSENCE: a node that drains gives up its presence at
// the drain's first step, and is merely restarting.
//
// Mutation: decide on absence alone, without the host grace, and the job is
// lost at the first pass that finds its node away.
func TestALaunchNodeGoneForTheHostGraceHasItsJobLost(t *testing.T) {
	t.Parallel()
	rig := newHostRig(t)
	run := rig.launchIn("t-gone", Direct, "node-a")
	fleet := &roster{}
	fleet.set("node-a", false)
	w := rig.waiter("node-b", fleet)

	tickOnce(t, w)
	rig.now = rig.now.Add(DefaultHostGrace - time.Second)
	tickOnce(t, w)
	if got := rig.get("t-gone"); got.Lost != nil || len(rig.announced()) != 0 {
		t.Fatalf("a node away for less than the host grace had its run judged gone (%v, %v)",
			lostAt(got), rig.announced())
	}

	rig.now = rig.now.Add(time.Second)
	tickOnce(t, w)
	got := rig.get("t-gone")
	if !lostAt(got).Equal(rig.now) || got.LaunchID != run.LaunchID ||
		got.Lost.Cause != LossLaunchNodeGone || got.Lost.Node != "node-a" {
		t.Fatalf("the row records the verdict %+v on job %s; want node-a's loss at %v on %s",
			got.Lost, got.LaunchID, rig.now, run.LaunchID)
	}
	if got := rig.announced(); !slices.Equal(got, []string{"t-gone"}) {
		t.Fatalf("completions announced for %v; want the gone run's", got)
	}
	before := rig.queue.count()
	rig.now = rig.now.Add(w.interval)
	tickOnce(t, w)
	if rig.queue.count() == before {
		t.Error("the pass an interval later did not announce the unclaimed verdict again")
	}
	if killed := rig.local.KilledIDs(); len(killed) != 0 {
		t.Errorf("node-b's waiter killed %v: a box on a gone node's host is not its to reach", killed)
	}
}

// A NODE BACK WITHIN THE HOST GRACE KEEPS ITS RUNS, and its absence is
// measured afresh the next time it goes: a restart is no departure.
//
// Mutation: keep the first absence's instant across a return, and the second
// absence is judged gone at once.
func TestALaunchNodeBackWithinTheHostGraceKeepsItsRuns(t *testing.T) {
	t.Parallel()
	rig := newHostRig(t)
	rig.launchIn("t-back", Direct, "node-a")
	fleet := &roster{}
	fleet.set("node-a", false)
	w := rig.waiter("node-b", fleet)

	tickOnce(t, w)
	rig.now = rig.now.Add(DefaultHostGrace / 2)
	fleet.set("node-a", true)
	tickOnce(t, w)
	fleet.set("node-a", false)
	rig.now = rig.now.Add(DefaultHostGrace / 2)
	tickOnce(t, w)
	rig.now = rig.now.Add(DefaultHostGrace / 2)
	tickOnce(t, w)
	if got := rig.get("t-back"); got.Lost != nil {
		t.Errorf("a node that came back within the host grace had its run judged gone at %v",
			lostAt(got))
	}
}

// AN ABSENCE IS TIMED WITHIN ONE STINT OF THE LAUNCH-NODE WATCH. This waiter
// saw node-a without its presence, then lost the watch; a peer held it while
// node-a came back; and when the watch came back here, the host grace later,
// node-a happened to be away again — a second restart. Timed from the first
// sighting, that one pass judged a live node gone and ended its runs. A verdict
// needs an absence this waiter watched whole, so the clock starts again with
// the stint: whether the watch was lost or its claim could not be confirmed.
//
// Mutation: keep the absences across a lost watch, or across a claim that
// failed, and the node is judged gone on the first pass back.
func TestAnAbsenceIsTimedWithinOneStintOfTheLaunchNodeWatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		lose func(watch *watchStint)
	}{
		{"the watch was lost", func(w *watchStint) { w.set(false, nil) }},
		{"its claim failed", func(w *watchStint) { w.set(false, errors.New("coordination store unreachable")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newHostRig(t)
			rig.launchIn("t-stint", Direct, "node-a")
			fleet := &roster{}
			fleet.set("node-a", false)
			watch := &watchStint{holds: true}
			w, err := NewWaiter(WaiterOptions{
				Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
				Node: "node-b", Roster: fleet, ClaimDuty: watch.claim, DutyTTL: time.Minute,
				Now: func() time.Time { return rig.now },
			})
			if err != nil {
				t.Fatalf("NewWaiter: %v", err)
			}

			tickOnce(t, w)
			tc.lose(watch)
			tickOnce(t, w)
			rig.now = rig.now.Add(DefaultHostGrace + time.Second)
			watch.set(true, nil)
			tickOnce(t, w)
			if got := rig.get("t-stint"); got.Lost != nil || len(rig.announced()) != 0 {
				t.Fatalf("a node found away on the first pass back on the watch was judged gone at %v",
					lostAt(got))
			}

			// AND WATCHED WHOLE WITHIN THE STINT, the verdict still comes.
			rig.now = rig.now.Add(DefaultHostGrace)
			tickOnce(t, w)
			if got := rig.get("t-stint"); got.Lost == nil {
				t.Fatal("a node away for the host grace within one stint was not judged gone")
			}
		})
	}
}

// watchStint is the launch-node watch as a case holds it: whether this node
// holds it now, or why it cannot say.
type watchStint struct {
	mu    sync.Mutex
	holds bool
	err   error
}

func (w *watchStint) claim(context.Context) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.holds, w.err
}

func (w *watchStint) set(holds bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.holds, w.err = holds, err
}

// A NODE WITHOUT THE LAUNCH-NODE WATCH STILL DRIVES ITS OWN BOXES, and judges no
// launch node gone, whether a peer holds the watch or its claim could not be
// read. Its boxes are on its own host, which no other node reaches, and a node
// that is no fleet worker is refused the watch on every claim: a pass that
// waited on the claim would leave every box it launched unpolled and its job
// never collected. The verdict is the watch's alone, so N nodes never each end
// a gone node's runs and announce them: one without it asks the roster nothing.
//
// Mutation: skip a pass's polls where the claim is refused or fails, or judge
// launch nodes without holding the watch.
func TestANodeWithoutTheLaunchNodeWatchStillDrivesItsOwnBoxes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		claim DutyFunc
	}{
		{"a peer holds it", func(context.Context) (bool, error) { return false, nil }},
		{"its claim failed", func(context.Context) (bool, error) {
			return false, errors.New("coordination store unreachable")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newHostRig(t)
			own := rig.launchIn("t-own", Container, "node-b")
			rig.launchIn("t-on-gone-node", Direct, "node-a")
			fleet := &roster{}
			fleet.set("node-a", false)
			fleet.set("node-b", true)
			w, err := NewWaiter(WaiterOptions{
				Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
				Node: "node-b", Roster: fleet, ClaimDuty: tc.claim, DutyTTL: time.Minute,
				Now: func() time.Time { return rig.now },
			})
			if err != nil {
				t.Fatalf("NewWaiter: %v", err)
			}
			rig.runner.PollFunc = func(_ context.Context, box Sandbox) (JobState, error) {
				if box.ID() == own.SandboxID {
					return JobEnded, nil
				}
				return JobRunning, nil
			}
			tickOnce(t, w)
			rig.now = rig.now.Add(DefaultHostGrace + time.Second)
			tickOnce(t, w)
			if got := rig.announced(); !slices.Equal(got, []string{"t-own"}) {
				t.Errorf("completions announced for %v; want this node's own finished job alone", got)
			}
			if keepalives := rig.container.Box(own.SandboxID).Keepalives(); keepalives == 0 {
				t.Error("this node's own box was never kept alive")
			}
			if got := rig.get("t-on-gone-node"); got.Lost != nil || fleet.questions() != 0 {
				t.Errorf("a node without the watch judged launch nodes: asked the roster %d times, "+
					"node-a's run lost at %v", fleet.questions(), lostAt(got))
			}
		})
	}
}

// A ROSTER THAT CANNOT ANSWER DECIDES NOTHING: an unreachable store is no
// evidence that a node left.
//
// Mutation: read a roster's error as an absent node, and the run is judged
// gone once the host grace has passed with nobody able to say.
func TestARosterThatCannotAnswerDecidesNothing(t *testing.T) {
	t.Parallel()
	rig := newHostRig(t)
	rig.launchIn("t-unknown", Direct, "node-a")
	fleet := &roster{err: errors.New("coordination store unreachable")}
	w := rig.waiter("node-b", fleet)

	tickOnce(t, w)
	rig.now = rig.now.Add(2 * DefaultHostGrace)
	tickOnce(t, w)
	if got := rig.get("t-unknown"); got.Lost != nil || len(rig.announced()) != 0 {
		t.Errorf("an unanswered roster ended the run (%v, %v)", lostAt(got), rig.announced())
	}
}

// A VERDICT ENDS WITH THE CLAIM THAT AUTHORISED IT: it outlives its pass, and
// past the claim's window a peer may hold the launch-node watch and judge the
// same node, so the verdict is bounded by that window ([Waiter.deadline]): the
// watch's TTL less one interval, from the instant the claim was asked.
//
// IN A BUBBLE, so the verdict's end is the window's to the instant: the roster
// waits on its context alone, and the clock moves only to the timers that end
// it.
//
// Mutation: bound the verdict by context.WithCancel alone, and it runs on to
// the ten seconds this case gives its tick.
func TestAVerdictEndsWithTheClaimThatAuthorisedIt(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig := newHostRig(t)
		rig.launchIn("t-judged", Direct, "node-a")
		const interval, ttl = 20 * time.Millisecond, 200 * time.Millisecond
		held := &heldRoster{ended: make(chan heldEnd, 1)}
		w, err := NewWaiter(WaiterOptions{
			Queue: rig.queue, Pending: rig.pending, Manager: rig.managers, Node: "node-b",
			Roster: held, ClaimDuty: holding, DutyTTL: ttl, Interval: interval,
			Now: func() time.Time { return rig.now },
		})
		if err != nil {
			t.Fatalf("NewWaiter: %v", err)
		}
		tickCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		claimed := time.Now()
		if _, err := w.Tick(tickCtx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		got := <-held.ended
		if ran, window := got.at.Sub(claimed), ttl-interval; ran != window {
			t.Fatalf("the verdict ran %v after its claim under a watch of %v; want it ended with the "+
				"claim's window, at %v", ran, ttl, window)
		}
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Errorf("the verdict ended with %v; want the claim's deadline", got.err)
		}
	})
}

// heldRoster is a roster that answers nothing until the verdict asking it
// ends, and says when and how it ended.
type heldRoster struct{ ended chan heldEnd }

type heldEnd struct {
	at  time.Time
	err error
}

func (r *heldRoster) Present(ctx context.Context, _ string) (bool, error) {
	<-ctx.Done()
	r.ended <- heldEnd{at: time.Now(), err: ctx.Err()}
	return false, ctx.Err()
}

// A RUN PARKED ON A QUESTION ON A GONE NODE WAITS WITHOUT ITS BOX: its answer
// can still come, and its resume starts from the pushed branch — so the box
// is released as an expired pause's is, with nothing to kill.
//
// Mutation: leave parked runs out of the verdict, and the row goes on naming a
// box that went with its host.
func TestAParkedRunOnAGoneNodeWaitsWithoutItsBox(t *testing.T) {
	t.Parallel()
	rig := newHostRig(t)
	rig.launchIn("t-asked", Direct, "node-a")
	rig.park("t-asked")
	fleet := &roster{}
	fleet.set("node-a", false)
	w := rig.waiter("node-b", fleet)

	tickOnce(t, w)
	rig.now = rig.now.Add(DefaultHostGrace)
	tickOnce(t, w)
	got := rig.get("t-asked")
	if got.Status != StatusReseed || got.SandboxID != "" {
		t.Errorf("the parked run is %q naming %q; want it waiting for its answer with no box",
			got.Status, got.SandboxID)
	}
	if killed := rig.local.KilledIDs(); len(killed) != 0 {
		t.Errorf("killed %v on a host that is gone", killed)
	}
}

// ---------------------------------------------------------------------
// a host reclaims the boxes no run names
// ---------------------------------------------------------------------

// A BOX ON THIS HOST THAT NO RUN NAMES ANY MORE IS RECLAIMED HERE: a run whose
// seat moved was ended, stopped or relaunched by another node, which could not
// kill a box on this host and left it — and a box whose run ended while this
// node was down is found by the first pass after it starts, since the question
// is asked of the box.
//
// Mutation: reclaim nothing in the sweep, and every left box outlives its run.
func TestAHostReclaimsTheBoxesNoRunNamesAnyMore(t *testing.T) {
	t.Parallel()
	rig := newHostRig(t)
	gone := rig.launchIn("t-gone", Direct, "node-a")
	moved := rig.launchIn("t-moved", Direct, "node-a")
	kept := rig.launchIn("t-kept", Direct, "node-a")

	// The first run ended elsewhere: its row is gone.
	if _, _, err := rig.pending.DecideEnding(t.Context(), "t-gone", Decision{
		License: License{WhileIn: Active, Launch: EveryLaunch}, Reason: "x", Detail: "x",
	}); err != nil {
		t.Fatalf("DecideEnding: %v", err)
	}
	ended := rig.get("t-gone")
	if _, _, err := rig.pending.Finish(t.Context(), "t-gone", ended.Ending.ID); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	// The second was relaunched on another node, in a box of that node's.
	if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{
		TurnID: "t-moved", AgentHandle: "swe", AgentID: sweID, LaunchNode: "node-b",
	}, rigLease); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	w := rig.waiter("node-a", nil)
	tickOnce(t, w)
	if killed := rig.local.KilledIDs(); slices.Contains(killed, moved.SandboxID) {
		t.Fatalf("killed %s while its run was launching anew, which may continue in it", moved.SandboxID)
	}
	if err := rig.pending.AttachSandbox(t.Context(), "t-moved", BoxRef{
		SandboxID: "box-on-b", Placement: string(Direct), Harness: "claude-code",
	}, rigLease); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	rig.suspend("t-moved")
	tickOnce(t, w)

	killed := rig.local.KilledIDs()
	for _, want := range []string{gone.SandboxID, moved.SandboxID} {
		if !slices.Contains(killed, want) {
			t.Errorf("the host left %s standing; killed %v", want, killed)
		}
	}
	if slices.Contains(killed, kept.SandboxID) {
		t.Errorf("the host killed %s, which its running job still names", kept.SandboxID)
	}
}

// A STOPPED RUN'S BOX IS RECLAIMED BY ITS HOST ONCE ITS SPEND IS READ, and not
// before: the stop's ending reads what the run spent off the box — asked of
// this host where the seat is another node's — and then would have killed it.
//
// Mutation: reclaim a box under any decided ending, and the stop's read finds
// it gone.
func TestAStoppedRunsBoxIsReclaimedOnceItsSpendIsRead(t *testing.T) {
	t.Parallel()
	rig := newHostRig(t)
	run := rig.launchIn("t-stopped", Direct, "node-a")
	decided, _, err := rig.pending.DecideEnding(t.Context(), "t-stopped", Decision{
		License: License{WhileIn: Active, Launch: EveryLaunch}, Reclaim: true,
		Stop: &RunStop{By: "founder", ByKind: "human"},
	})
	if err != nil {
		t.Fatalf("DecideEnding: %v", err)
	}
	w := rig.waiter("node-a", nil)
	tickOnce(t, w)
	if killed := rig.local.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v before the stop read what the run spent", killed)
	}
	if _, _, err := rig.pending.CollectStop(t.Context(), "t-stopped",
		StopCollection{Ending: decided.Ending.ID}); err != nil {
		t.Fatalf("CollectStop: %v", err)
	}
	tickOnce(t, w)
	if killed := rig.local.KilledIDs(); !slices.Contains(killed, run.SandboxID) {
		t.Errorf("the host left the stopped run's box standing once its spend was read; killed %v", killed)
	}
}

// ---------------------------------------------------------------------
// completion latency
// ---------------------------------------------------------------------

// THE SCHEDULE A NUDGE POLLS ON stays inside one poll interval: past it, the
// periodic poll comes no later.
func TestANudgesScheduleStaysInsideOnePollInterval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		interval time.Duration
		want     []time.Duration
	}{
		{DefaultPollInterval, []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}},
		{time.Second, []time.Duration{0}},
		{100 * time.Millisecond, []time.Duration{0}},
	} {
		if got := nudgeOffsets(tc.interval); !slices.Equal(got, tc.want) {
			t.Errorf("at %v the nudge polls at %v; want %v", tc.interval, got, tc.want)
		}
	}
}

// startedWaiter is node's waiter, its own pass an hour away, so only a nudge
// can be what polls inside a case.
func (r *hostRig) startedWaiter(node string) *Waiter {
	r.t.Helper()
	w, err := NewWaiter(WaiterOptions{
		Queue: r.queue, Pending: r.pending, Manager: r.managers,
		Node: node, Interval: time.Hour,
		Now: func() time.Time { return r.now },
	})
	if err != nil {
		r.t.Fatalf("NewWaiter: %v", err)
	}
	w.Start(r.t.Context())
	r.t.Cleanup(w.Stop)
	return w
}

// waitAnnounced waits for a completion to be announced for turn, for as long
// as within on the bubble's clock, which moves only once every goroutine of
// the case is blocked: the bound is the nudge schedule's own time, never how
// late a loaded runner woke the case.
//
//fixedwait:bubble
func (r *hostRig) waitAnnounced(t *testing.T, turn string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if slices.Contains(r.announced(), turn) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// A RUN THAT SUBMITTED ITS WORK IS POLLED AT ONCE by the waiter that drives
// it, and again on a short schedule while its harness exits — so its completion is
// found within seconds of its exit, not up to a poll interval later. In a
// bubble, where the schedule keeps its own seconds.
//
// Mutation: make a nudge do nothing, and the completion waits the hour.
func TestASubmittedRunIsPolledAtOnceAndAgainWhileItExits(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig := newHostRig(t)
		run := rig.launchIn("t-submitted", Direct, "node-a")
		// THE HARNESS IS STILL EXITING at the first poll, and done at the next.
		var polls sync.WaitGroup
		polls.Add(1)
		var once sync.Once
		rig.runner.PollFunc = func(context.Context, Sandbox) (JobState, error) {
			first := false
			once.Do(func() { first = true; polls.Done() })
			if first {
				return JobRunning, nil
			}
			return JobEnded, nil
		}
		w := rig.startedWaiter("node-a")
		w.Nudge(Nudge{TurnID: "t-submitted", LaunchID: run.LaunchID, Why: NudgeSubmitted})
		if !rig.waitAnnounced(t, "t-submitted", 10*time.Second) {
			t.Fatal("a nudged run's completion was not announced within the nudge's schedule")
		}
	})
}

// A NUDGE IS FOR THE WAITER THAT DRIVES THE RUN, and for the job it names: a
// waiter that cannot reach the box, a job the row moved on from, and a run
// whose ending is decided are polled for nobody.
//
// READ ONCE EVERY NUDGE IS DONE WITH, never over a quiet window: every task
// the waiter started has returned (what [Waiter.Stop] waits for), each having
// let its run be or polled it, and a poll of a finished job announces it
// before its task returns. In a bubble, so a task that followed its run on
// the schedule would cost the wait no time.
//
// Mutation: poll whatever a nudge names, and each of these is announced.
func TestANudgePollsOnlyWhatItsWaiterDrives(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig := newHostRig(t)
		other := rig.launchIn("t-other-host", Direct, "node-b")
		stale := rig.launchIn("t-stale-job", Direct, "node-a")
		ending := rig.launchIn("t-ending", Direct, "node-a")
		if _, _, err := rig.pending.DecideEnding(t.Context(), "t-ending", Decision{
			License: License{WhileIn: Active, Launch: EveryLaunch}, Reason: "x", Detail: "x",
		}); err != nil {
			t.Fatalf("DecideEnding: %v", err)
		}
		rig.runner.Finish(Result{Success: true})
		w := rig.startedWaiter("node-a")
		w.Nudge(Nudge{TurnID: "t-other-host", LaunchID: other.LaunchID, Why: NudgeSubmitted})
		w.Nudge(Nudge{TurnID: "t-stale-job", LaunchID: "not-" + stale.LaunchID, Why: NudgeSubmitted})
		w.Nudge(Nudge{TurnID: "t-ending", LaunchID: ending.LaunchID, Why: NudgeSubmitted})
		w.tasks.Wait()
		if got := rig.announced(); len(got) != 0 {
			t.Errorf("a nudge polled a run its waiter does not drive: %v", got)
		}
	})
}

// A BOX LEFT TO ITS HOST IS RECLAIMED THE MOMENT THE HOST IS TOLD, not at its
// next pass.
//
// Mutation: make a released nudge do nothing, and the box waits the hour.
func TestABoxLeftToItsHostIsReclaimedWhenTheHostIsTold(t *testing.T) {
	t.Parallel()
	rig := newHostRig(t)
	run := rig.launchIn("t-left", Direct, "node-a")
	if _, _, err := rig.pending.DecideEnding(t.Context(), "t-left", Decision{
		License: License{WhileIn: Active, Launch: EveryLaunch}, Reclaim: true, Reason: "x", Detail: "x",
	}); err != nil {
		t.Fatalf("DecideEnding: %v", err)
	}
	w := rig.startedWaiter("node-a")
	w.Nudge(Nudge{TurnID: "t-left", LaunchID: run.LaunchID, Why: NudgeReleased})
	deadline := time.Now().Add(10 * time.Second)
	//fixedwait:measure a poll's own deadline: the kill is asserted after it, and a box left to the next pass waits the hour
	for time.Now().Before(deadline) && !slices.Contains(rig.local.KilledIDs(), run.SandboxID) {
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
	if !slices.Contains(rig.local.KilledIDs(), run.SandboxID) {
		t.Error("the host was told and did not reclaim the box no run needs any more")
	}
}
