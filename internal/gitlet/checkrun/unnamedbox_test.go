package checkrun_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A REMOTE BOX NOTHING NAMED IS KILLED WITH ITS ATTEMPT. A check's box is
// stamped with its claim, the attempt and its runner, so a box no handle
// reaches is still found: one whose create's answer was lost is killed by the
// runner's failed acquire as the attempt ends `errored box_failed`, and one a
// runner made and died before naming is killed by the home's sweep, which
// spells the same stamp from the row alone.
//
// Mutations, each red: stamp no box; leave a failed acquire's box to its TTL;
// have the sweep kill a named box alone; spell the sweep's stamp from anything
// but the row's attempt and runner.
func TestARemoteBoxNothingNamedIsKilledWithItsAttempt(t *testing.T) {
	t.Parallel()
	t.Run("its create's answer lost", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t)
		f.e2b.answer = func(context.Context, *sandbox.FakeSandbox) error {
			return errors.New("e2b: the create's answer was lost")
		}
		f.runner(f.options("git-a", 0, &order{nodes: []string{"git-a"}}))
		key := f.queue(oidOf("1"), remote("unit", time.Minute))
		box := f.e2b.box(t)
		row := f.ended(key)
		if row.State != gitlet.CheckStateErrored || row.Reason != gitlet.CheckReasonBoxFailed || row.Box != "" {
			t.Fatalf("an attempt whose create answered nobody = %+v, want errored box_failed naming no box", row)
		}
		if launch := f.e2b.spec(0).LaunchID; launch == "" {
			t.Fatal("the check's box was made with no stamp")
		}
		if killed := f.e2b.KilledIDs(); !slices.Contains(killed, box.ID()) {
			t.Errorf("the box whose create answered nobody was left to its TTL: killed %v", killed)
		}
	})
	t.Run("its runner gone before naming it", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t)
		release := make(chan struct{})
		f.e2b.answer = func(ctx context.Context, _ *sandbox.FakeSandbox) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		f.runner(f.options("git-a", 0, &order{nodes: []string{"git-a"}}))
		// BEFORE THE RUNNER'S STOP, which cleanups run last-first: its create
		// answers once the case is done with the box.
		t.Cleanup(func() { close(release) })
		key := f.queue(oidOf("2"), remote("unit", time.Minute))
		box := f.e2b.box(t)
		f.await(key, "claimed by git-a", func(r gitlet.CheckRow) bool {
			return r.State == gitlet.CheckStateRunning && r.Runner == "git-a" && r.Box == ""
		})
		sweep := f.sweeper(&order{nodes: []string{"git-home"}}, nil)
		if err := sweep.Work(t.Context(), f.repo, statelogZero); err != nil {
			t.Fatalf("the home's sweep: %v", err)
		}
		if row, _ := f.row(key); row.State != gitlet.CheckStateErrored || row.Reason != gitlet.CheckReasonRunnerLost {
			t.Fatalf("a gone runner's attempt = %+v, want errored runner_lost", row)
		}
		if killed := f.e2b.KilledIDs(); !slices.Equal(killed, []string{box.ID()}) {
			t.Errorf("the boxes killed = %v, want the box its runner never named, %s, alone", killed, box.ID())
		}
	})
}
