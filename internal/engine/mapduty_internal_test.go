package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// THE DUTY'S PACE IS WHAT ITS OWN TICK FOUND: the tick interval after a tick
// that found a map, whatever else happened to it; the poll only after one that
// read the store cleanly and found no map where one is wanted; and the interval
// after a turn that did not hold the duty, a tick that failed before it could
// say, or one that found no map and wants none — none of them counted anything,
// and none is a reason to ask the store every second. The last is the estate
// map under the single-file layout, which is never written: a duty that polled
// for it would read the store every second for ever.
func TestTheMapDutyPacesOnWhatItsTickFound(t *testing.T) {
	t.Parallel()
	failed := errors.New("the store did not answer")
	mapped, awaited := mapTick{mapped: true}, mapTick{awaited: true}
	for name, tc := range map[string]struct {
		claim  func(context.Context) (bool, error)
		result mapTick
		err    error
		ticks  bool
		want   time.Duration
	}{
		"found a map":                      {result: mapped, ticks: true, want: mapInterval},
		"found a map, then failed":         {result: mapped, err: failed, ticks: true, want: mapInterval},
		"found none where one is wanted":   {result: awaited, ticks: true, want: mapAwaitedPoll},
		"found none, and wants none":       {ticks: true, want: mapInterval},
		"failed before it read the store":  {err: failed, ticks: true, want: mapInterval},
		"held the duty and found a map":    {claim: dutyAnswers(true, nil), result: mapped, ticks: true, want: mapInterval},
		"held the duty and found none":     {claim: dutyAnswers(true, nil), result: awaited, ticks: true, want: mapAwaitedPoll},
		"did not hold the duty":            {claim: dutyAnswers(false, nil), want: mapInterval},
		"could not ask whether it held it": {claim: dutyAnswers(false, failed), want: mapInterval},
	} {
		ticked, released := false, false
		duty := mapDuty{event: "test_map", claim: tc.claim, tick: func(context.Context) (mapTick, error) {
			ticked = true
			return tc.result, tc.err
		}, released: func() { released = true }}
		if got := duty.turn(t.Context()); got != tc.want || ticked != tc.ticks {
			t.Errorf("%s: waited %v having ticked %v, want %v having ticked %v",
				name, got, ticked, tc.want, tc.ticks)
		}
		// A TURN THAT DOES NOT TICK ENDS THE TENURE: it did not hold the
		// duty, or could not say it did.
		if released == ticked {
			t.Errorf("%s: released the tenure %v having ticked %v", name, released, ticked)
		}
	}
}

// dutyAnswers is a duty claim that answers mine and err.
func dutyAnswers(mine bool, err error) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return mine, err }
}
