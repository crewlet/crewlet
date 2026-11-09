package pickup_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/pickup"
)

// idle is a seat this node may hand work: owned, unpaused, with a model,
// nothing running, nothing parked or refused, nothing waiting, and idle on
// the previous pass too.
func idle() pickup.Conditions {
	return pickup.Conditions{Owned: true, TurnEngineReady: true, IdleBefore: true}
}

// A SEAT THAT IS NOT IDLE IS HANDED NOTHING, and each refusal is named, so an
// operator reading `work_pickup_skipped` knows which condition held it.
func TestASeatThatIsNotIdleIsHandedNothing(t *testing.T) {
	t.Parallel()
	if ok, skip := pickup.Idle(idle()); !ok {
		t.Fatalf("an idle seat was refused for %q", skip)
	}
	for _, c := range []struct {
		name string
		set  func(*pickup.Conditions)
		want pickup.Skip
	}{
		{"held by another node", func(c *pickup.Conditions) { c.Owned = false }, pickup.SkipNotOwned},
		{"the node is stopping", func(c *pickup.Conditions) { c.Draining = true }, pickup.SkipDraining},
		{"the node is shedding", func(c *pickup.Conditions) { c.Shedding = true }, pickup.SkipShedding},
		{"a person paused it", func(c *pickup.Conditions) { c.Paused = true }, pickup.SkipPaused},
		{"its pause is not read yet", func(c *pickup.Conditions) { c.PauseUnknown = true }, pickup.SkipPaused},
		{"the company has no model", func(c *pickup.Conditions) { c.TurnEngineReady = false }, pickup.SkipNoModel},
		{"a turn is dispatching", func(c *pickup.Conditions) { c.Dispatching = true }, pickup.SkipBusy},
		{"an executor run holds it", func(c *pickup.Conditions) { c.ExecutorHolds = true }, pickup.SkipExecutor},
		{"a run waits on a person", func(c *pickup.Conditions) { c.ExecutorAwaits = true }, pickup.SkipExecutor},
		{"a budget or provider park", func(c *pickup.Conditions) { c.Parked = true }, pickup.SkipParked},
		{"a window or a limit refuses", func(c *pickup.Conditions) { c.Refused = true }, pickup.SkipRefused},
		{"mail waits in its inbox", func(c *pickup.Conditions) { c.MailWaiting = true }, pickup.SkipMailWaiting},
		{"it was busy on the last pass", func(c *pickup.Conditions) { c.IdleBefore = false }, pickup.SkipSettling},
	} {
		cond := idle()
		c.set(&cond)
		ok, skip := pickup.Idle(cond)
		if ok || skip != c.want {
			t.Errorf("%s: Idle = %v, %q; want refused for %q", c.name, ok, skip, c.want)
		}
	}
}

// THE SETTLE IS THE LAST CONDITION AND THE ONLY ONE IdleNow IGNORES: a seat
// idle now is what the next pass reads as idle before.
func TestTheSettleIsTheLastCondition(t *testing.T) {
	t.Parallel()
	settling := idle()
	settling.IdleBefore = false
	if !pickup.IdleNow(settling) {
		t.Error("a seat idle on this pass was not recorded idle for the next one")
	}
	busy := settling
	busy.Dispatching = true
	if pickup.IdleNow(busy) {
		t.Error("a busy seat was recorded idle, so the next pass would hand it work at once")
	}
}
