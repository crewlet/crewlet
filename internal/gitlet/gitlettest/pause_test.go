package gitlettest_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A paused applier holds the barrier it reached. A halt gives back a barrier
// decoded and not yet checkpointed, and a fleet case that levels a member and
// then holds it decides on that member as levelled (F30). Staged with the
// barrier's checkpoint commit held, then failed as a halt that cuts it leaves it.
func TestAPausedApplierHoldsTheBarrierItReached(t *testing.T) {
	hold := storetest.HoldCommits(t)
	f := gitlettest.SoloNode(t, gitlettest.SoloOptions{WrapDriver: hold.Wrap})
	f.Declare(t, gitlet.Declared{Name: "paused", Settings: gitlettest.Settings()})
	data := f.Data()
	ctx := t.Context()
	if err := f.Level(ctx, ""); err != nil {
		t.Fatal(err)
	}
	before := reachedBy(t, data)

	hold.Arm()
	// Released before the node's teardown, which waits for a held commit.
	defer hold.Release()
	if _, err := (gitlet.ObjectEstate{Reader: data.Served().Reader}).Barrier(ctx); err != nil {
		t.Fatalf("a linearizable read: %v", err)
	}
	// The applier reaches a barrier before it offers its checkpoint to the
	// commit beside its loop, so a held commit is a barrier reached.
	if err := hold.AwaitHeld(ctx, 1); err != nil {
		t.Fatalf("the barrier's checkpoint never reached its commit: %v", err)
	}
	at := reachedBy(t, data)
	if at.Packed() <= before.Packed() {
		t.Fatalf("the applier reached %s after a linearizable read, want past %s: the read appended no barrier",
			at, before)
	}
	if got := data.Checkpoint(); got.Packed() >= at.Packed() {
		t.Fatalf("the checkpoint is at %s with its commit held, want below the barrier at %s: the premise is a "+
			"barrier reached and not committed", got, at)
	}
	// The applier commits the barrier again only once its idle fetch returns,
	// so a halt that does not wait for it meets it uncommitted.
	hold.ReleaseFailing(errors.New("codetest: the barrier's checkpoint commit failed"))
	resume := data.PauseApplier(t)
	defer resume()

	if got := reachedBy(t, data); got.Packed() < at.Packed() {
		t.Errorf("the paused applier holds %s, below the barrier at %s it had reached: the halt gave it back",
			got, at)
	}
	if got := data.Checkpoint(); got.Packed() < at.Packed() {
		t.Errorf("the paused applier's checkpoint is at %s, below the barrier at %s it had reached", got, at)
	}
}

// reachedBy is the position data's applier has reached, as its health reads it.
func reachedBy(t *testing.T, data *gitlettest.DataNode) statelog.Position {
	t.Helper()
	h, err := data.Health(t.Context())
	if err != nil {
		t.Fatalf("the data node's health: %v", err)
	}
	return h.Position
}
