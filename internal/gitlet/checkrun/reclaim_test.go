package checkrun_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/checkrun"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// leftBox is a box on the case's container cell recording record, as a run of
// this node's previous process made it and left it.
func (f *fleet) leftBox(record string) *sandbox.FakeSandbox {
	f.t.Helper()
	box, err := f.container.Create(f.t.Context(), sandbox.Spec{Placement: sandbox.Container, Check: record})
	if err != nil {
		f.t.Fatalf("make a box recording %q: %v", record, err)
	}
	return box.(*sandbox.FakeSandbox)
}

// recordOf is key's attempt as its box records it.
func (f *fleet) recordOf(key gitlet.CheckKey) string {
	f.t.Helper()
	record, err := checkrun.AttemptRecord(key)
	if err != nil {
		f.t.Fatalf("record attempt %d of %s: %v", key.Attempt, key.Name, err)
	}
	return record
}

// lose ends key's running attempt `errored runner_lost`, as the home's sweep
// ends one whose runner restarted.
func (f *fleet) lose(key gitlet.CheckKey) {
	f.t.Helper()
	res, err := f.d.Served().PublishChecks(f.t.Context(), gitlet.ChecksRequest{OpID: newOpID("code_check_lost"),
		Finished: &gitlet.CheckFinished{CheckKey: key, State: gitlet.CheckStateErrored,
			Reason: gitlet.CheckReasonRunnerLost, Summary: "its runner restarted", FinishedAt: time.Now().UTC()}})
	if err != nil || res.Refusal != "" {
		f.t.Fatalf("end attempt %d of %s as its runner's lost: (%+v, %v)", key.Attempt, key.Name, res, err)
	}
}

// killed reports whether the case's container cell has reclaimed box.
func (f *fleet) killed(box *sandbox.FakeSandbox) bool {
	return slices.Contains(f.container.KilledIDs(), box.ID())
}

// reclaimedOnce nudges r until box is killed and its reclaim logged, awaiting
// the line itself, since Runner.kill records a kill before it writes the line;
// then until a walk begun after it has ended (walks.walked), so the reclaim
// that wrote it, and the next, are over. Then one line must name a reclaim.
func (f *fleet) reclaimedOnce(r *checkrun.Runner, w *walks, logged *lines, box *sandbox.FakeSandbox) {
	f.t.Helper()
	eventually(f.t, "box "+box.ID()+" reclaimed and its reclaim logged", func() bool {
		r.Nudge()
		return f.killed(box) && logged.count("code_check_box_reclaimed") >= 1
	})
	w.walked(f.t, r, 1)
	if n := logged.count("code_check_box_reclaimed"); n != 1 {
		f.t.Errorf("one reclaim was logged %d times, want once", n)
	}
}

// judging is a data node's code domain, counting the reads a runner's reclaim
// judges a box by — an attempt's row at linearizable — by attempt.
type judging struct {
	checkrun.Estate
	mu    sync.Mutex
	reads map[gitlet.CheckKey]int
}

func (e *judging) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	if q.Kind == gitlet.ReadCheckBox && q.Fresh.Level == statelog.ReadLinearizable {
		e.mu.Lock()
		e.reads[*q.CheckBox]++
		e.mu.Unlock()
	}
	return e.Estate.Read(ctx, q)
}

// judged is how many times attempt's row was read to judge a box by.
func (e *judging) judged(attempt gitlet.CheckKey) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reads[attempt]
}

// total is how many rows were read to judge a box by.
func (e *judging) total() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, reads := range e.reads {
		n += reads
	}
	return n
}

// A BOX ITS ATTEMPT LEFT IS RECLAIMED AT THE RUNNER'S START: a box on its node
// recording an attempt whose row the home's sweep ended, one whose row runs in
// another box, one whose row is gone, and one recording no attempt the runner
// reads are each killed through its backend before the runner's first walk,
// and each reclaim is logged once, however many walks follow. NO BACKSTOP
// WITHIN THE CASE: a walk begins (reading its epoch, walks.counting) once the
// reclaim before it has ended, and runs at the start and at a nudge alone.
//
// Mutations: reclaim only on a walk after the first, red; keep a box whose row
// names another, whose row is gone, or whose record names no attempt, red; log
// a reclaim twice, red.
func TestABoxItsAttemptLeftIsReclaimedAtTheRunnersStart(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	now := time.Now().UTC()
	swept := f.queue(oidOf("1"), def("unit", time.Minute))
	sweptBox := f.leftBox(f.recordOf(swept))
	f.claim(swept, "git-a", gitlet.PlacementContainer, sweptBox.ID(), now)
	f.lose(swept)
	elsewhere := f.queue(oidOf("2"), def("unit", time.Minute))
	elsewhereBox := f.leftBox(f.recordOf(elsewhere))
	f.claim(elsewhere, "git-a", gitlet.PlacementContainer, "box-elsewhere", now)
	goneBox := f.leftBox(f.recordOf(f.key(oidOf("3"), "unit", 1)))
	unreadBox := f.leftBox("not an attempt")
	left := []*sandbox.FakeSandbox{sweptBox, elsewhereBox, goneBox, unreadBox}
	if len(left) > checkrun.ReclaimPerWalk {
		t.Fatalf("the premise: %d boxes are more than one walk judges (%d)", len(left), checkrun.ReclaimPerWalk)
	}

	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Timing.Backstop = time.Hour
	logged := logging(&o)
	var w walks
	w.counting(&o)
	r := f.runner(o)
	eventually(t, "the runner's first walk", func() bool { return w.begun.Load() > 0 })
	for _, box := range left {
		if !f.killed(box) {
			t.Errorf("box %s was left on the node past the runner's start", box.ID())
		}
	}
	w.walked(t, r, 3)
	if n := logged.count("code_check_box_reclaimed"); n != len(left) {
		t.Errorf("%d reclaims were logged %d times, want once each", len(left), n)
	}
}

// A BOX OF A LIVE ATTEMPT IS LEFT ALONE UNTIL ITS ATTEMPT ENDS: a box on the
// runner's node recording an attempt whose row runs in it, and one whose row
// runs naming no box yet (its box still being made), are kept by every walk
// that judges them; once the first's row ends, a later walk reclaims it.
//
// Mutations: reclaim a box whatever its row says, red at the start; read a row
// naming no box as naming another, red; judge a box only at the start, never
// on a walk, and the ended attempt's box is never reclaimed, red; log every box
// judged, red.
func TestABoxOfALiveAttemptIsLeftAloneUntilItsAttemptEnds(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	now := time.Now().UTC()
	live := f.queue(oidOf("1"), def("unit", time.Minute))
	liveBox := f.leftBox(f.recordOf(live))
	f.claim(live, "git-a", gitlet.PlacementContainer, liveBox.ID(), now)
	making := f.queue(oidOf("2"), def("unit", time.Minute))
	makingBox := f.leftBox(f.recordOf(making))
	f.claim(making, "git-a", gitlet.PlacementContainer, "", now)

	reads := &judging{Estate: estate{f.d}, reads: map[gitlet.CheckKey]int{}}
	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Estate = reads
	o.Timing.Backstop = time.Hour
	logged := logging(&o)
	var w walks
	w.counting(&o)
	r := f.runner(o)
	// JUDGED AND KEPT TWICE, each a poll apart: at the start and on a walk.
	eventually(t, "both boxes judged twice", func() bool {
		r.Nudge()
		return reads.judged(live) >= 2 && reads.judged(making) >= 2
	})
	if f.killed(liveBox) || f.killed(makingBox) {
		t.Fatalf("a walk reclaimed a box its attempt still runs in: killed %v", f.container.KilledIDs())
	}

	f.lose(live)
	f.reclaimedOnce(r, &w, logged, liveBox)
	if f.killed(makingBox) {
		t.Error("a walk reclaimed the box of an attempt still running, which names no box yet")
	}
}

// ONE WALK JUDGES AT MOST RECLAIMPERWALK BOXES: of more boxes left on a node
// than that, the runner's start reclaims that many, and the next walk the
// rest.
//
// Mutation: judge every box a walk, red.
func TestOneWalkJudgesAtMostReclaimPerWalkBoxes(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	now := time.Now().UTC()
	var left []*sandbox.FakeSandbox
	for i := range checkrun.ReclaimPerWalk + 1 {
		key := f.queue(oidOf(string(rune('1'+i))), def("unit", time.Minute))
		box := f.leftBox(f.recordOf(key))
		f.claim(key, "git-a", gitlet.PlacementContainer, box.ID(), now)
		f.lose(key)
		left = append(left, box)
	}

	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Timing.Backstop = time.Hour
	var w walks
	w.counting(&o)
	r := f.runner(o)
	eventually(t, "the runner's first walk", func() bool { return w.begun.Load() > 0 })
	if n := len(f.container.KilledIDs()); n != checkrun.ReclaimPerWalk {
		t.Fatalf("the runner's start reclaimed %d of %d boxes, want %d", n, len(left), checkrun.ReclaimPerWalk)
	}
	w.walked(t, r, 1)
	for _, box := range left {
		if !f.killed(box) {
			t.Errorf("box %s was left on the node past the walk after the start", box.ID())
		}
	}
}

// A RUNNER JUDGES NO BOX OF ITS OWN RUNS, AND A KEPT BOX ONCE A POLL: a check
// it runs records its attempt on its box, and while it runs, its walks read
// nothing to judge that box by; a box of a live attempt it does not run is
// read once at the start and not again within a poll, however many walks.
//
// THE POLL IS AN HOUR, so no poll falls within the case: the run's box is
// named and never polled, and the kept box is due again only past the case.
//
// Mutations: record no attempt on the run's box, red; judge the runner's own
// box, red; judge a kept box at every walk, red.
func TestARunnerJudgesNoBoxOfItsOwnRunsAndAKeptBoxOnceAPoll(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	live := f.queue(oidOf("1"), def("unit", time.Minute))
	liveBox := f.leftBox(f.recordOf(live))
	f.claim(live, "git-a", gitlet.PlacementContainer, liveBox.ID(), time.Now().UTC())
	if made := f.container.box(t); made != liveBox {
		t.Fatalf("the premise: the cell's first box is %s, want the box left, %s", made.ID(), liveBox.ID())
	}
	own := f.queue(oidOf("2"), def("unit", time.Minute))

	reads := &judging{Estate: estate{f.d}, reads: map[gitlet.CheckKey]int{}}
	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Estate = reads
	o.Timing.Poll = time.Hour
	o.Timing.Backstop = time.Hour
	var w walks
	w.counting(&o)
	r := f.runner(o)
	ownBox := f.container.box(t)
	f.await(own, "named its box", func(row gitlet.CheckRow) bool { return row.Box == ownBox.ID() })
	if got, want := f.container.spec(1).Check, f.recordOf(own); got != want {
		t.Fatalf("the run's box records %q, want its attempt %q", got, want)
	}
	w.walked(t, r, 3)
	if n := reads.judged(own); n != 0 {
		t.Errorf("the runner read its own run's row %d times to judge its box by, want none", n)
	}
	if n := reads.total(); n != 1 {
		t.Errorf("a kept box was judged %d times within a poll, want once", n)
	}
	if f.killed(ownBox) || f.killed(liveBox) {
		t.Errorf("a box in use was reclaimed: killed %v", f.container.KilledIDs())
	}
}

// listed reports whether the fleet's catalogue still lists box among its check
// boxes, as a runner's next walk finds one to judge.
func (f *fleet) listed(box *sandbox.FakeSandbox) bool {
	f.t.Helper()
	boxes, err := f.manager.CheckBoxes()
	if err != nil {
		f.t.Fatalf("CheckBoxes: %v", err)
	}
	return slices.ContainsFunc(boxes, func(b sandbox.CheckBox) bool { return b.SandboxID == box.ID() })
}

// A KILL THE RUNTIME REFUSES IS NO RECLAIM, AND IS TRIED AGAIN: a box its
// attempt left whose kill the backend refuses (a local container its runtime
// kept, its box and record kept with it) is logged as a failed reclaim, never
// as reclaimed, stays listed, and is judged again a poll later; once a kill
// lands, the reclaim is logged once.
//
// Mutations: log a refused kill as reclaimed, red; never judge again a box
// whose kill failed, and the box outlives its runtime's recovery, red.
func TestAKillTheRuntimeRefusesIsNoReclaimAndIsTriedAgain(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	var refusing atomic.Bool
	refusing.Store(true)
	var refused atomic.Int64
	f.container.KillFunc = func(context.Context, string) error {
		if refusing.Load() {
			refused.Add(1)
			return errors.New("cannot remove container: could not kill: device or resource busy")
		}
		return nil
	}
	swept := f.queue(oidOf("1"), def("unit", time.Minute))
	box := f.leftBox(f.recordOf(swept))
	f.claim(swept, "git-a", gitlet.PlacementContainer, box.ID(), time.Now().UTC())
	f.lose(swept)

	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Timing.Backstop = time.Hour
	logged := logging(&o)
	var w walks
	w.counting(&o)
	r := f.runner(o)
	eventually(t, "the refused kill tried again on a walk", func() bool {
		r.Nudge()
		return refused.Load() >= 2
	})
	// The first refusal's lines are all written: the second is tried only once
	// Runner.kill has returned from the first.
	if n := logged.count("code_check_box_reclaimed"); n != 0 {
		t.Fatalf("a box whose kill was refused was logged reclaimed %d times", n)
	}
	// KillFunc counts a refusal before the runner logs it, so the lines are
	// awaited, never read at once.
	tried := int(refused.Load())
	eventually(t, "each refused kill logged as a failed reclaim", func() bool {
		return logged.count("code_check_box_reclaim_failed") >= tried
	})
	if !f.listed(box) {
		t.Fatal("a box whose kill was refused is no longer listed, so no walk would judge it again")
	}

	refusing.Store(false)
	f.reclaimedOnce(r, &w, logged, box)
}

// A BOX ITS RUN COULD NOT CLOSE IS RECLAIMED ONCE ITS ATTEMPT HAS ENDED: a
// close the backend refuses at the run's end (a local container its runtime
// kept) is logged, and leaves the box listed with its attempt's record; a walk
// after the attempt's ending kills it and logs it reclaimed once.
//
// Mutation: keep judging a box as the runner's own run after its attempt
// ended, and the box is never reclaimed, red.
func TestABoxItsRunCouldNotCloseIsReclaimedOnceItsAttemptHasEnded(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.container.closeErr = errors.New("cannot remove container: could not kill: device or resource busy")
	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	logged := logging(&o)
	var w walks
	w.counting(&o)
	r := f.runner(o)
	key := f.queue(oidOf("1"), def("unit", time.Minute))
	box := f.container.box(t)
	f.container.end(box, 0, "ok\n")
	f.ended(key)
	awaitLog(t, logged, "code_check_box_not_closed")
	if box.Closed() {
		t.Fatal("the premise: a box whose close was refused reads as closed")
	}

	f.reclaimedOnce(r, &w, logged, box)
}
