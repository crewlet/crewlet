package checkrun_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A BOX NOTHING NAMED IS RECLAIMED ONCE ITS ATTEMPT ENDS. A check's box records
// its attempt as it is made, before its runner holds a handle to it, so a box
// no handle reaches is still found: one whose create's answer was lost goes at
// a walk after its attempt ends `errored box_failed`, and one a runner made and
// died before naming goes at its node's next start, once the home's sweep has
// ended the attempt `runner_lost` from its row alone.
//
// Mutations, each red: make a check's box record no attempt; keep a box whose
// ended row names no box.
func TestABoxNothingNamedIsReclaimedOnceItsAttemptEnds(t *testing.T) {
	t.Parallel()
	t.Run("its create's answer lost", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t)
		f.container.answer = func(context.Context, *sandbox.FakeSandbox) error {
			return errors.New("docker: the create's answer was lost")
		}
		r := f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))
		key := f.queue(oidOf("1"), def("unit", time.Minute))
		box := f.container.box(t)
		row := f.ended(key)
		if row.State != gitlet.CheckStateErrored || row.Reason != gitlet.CheckReasonBoxFailed || row.Box != "" {
			t.Fatalf("an attempt whose create answered nobody = %+v, want errored box_failed naming no box", row)
		}
		if record := f.container.spec(0).Check; record != f.recordOf(key) {
			t.Fatalf("the check's box recorded %q, want its attempt %q", record, f.recordOf(key))
		}
		eventually(t, "the box whose create answered nobody reclaimed", func() bool {
			r.Nudge()
			return f.killed(box)
		})
	})
	t.Run("its runner gone before naming it", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t)
		key := f.queue(oidOf("2"), def("unit", time.Minute))
		f.claim(key, "git-a", gitlet.PlacementContainer, "", time.Now().UTC())
		box := f.leftBox(f.recordOf(key))
		sweep := f.sweeper(&order{nodes: []string{"git-home"}}, nil)
		if err := sweep.Work(t.Context(), f.repo, statelogZero); err != nil {
			t.Fatalf("the home's sweep: %v", err)
		}
		if row, _ := f.row(key); row.State != gitlet.CheckStateErrored ||
			row.Reason != gitlet.CheckReasonRunnerLost || row.Box != "" {
			t.Fatalf("a gone runner's attempt = %+v, want errored runner_lost naming no box", row)
		}

		o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
		var w walks
		w.counting(&o)
		f.runner(o)
		eventually(t, "the runner's first walk", func() bool { return w.begun.Load() > 0 })
		if !f.killed(box) {
			t.Errorf("box %s, which its runner never named, was left on the node past its start", box.ID())
		}
	})
}
