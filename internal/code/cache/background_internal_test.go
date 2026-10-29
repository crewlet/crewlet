package cache

import (
	"slices"
	"testing"
)

// A RUN'S CHARGE MOVES IN ITS PLACE: recharge holds a run in flight to a new
// charge without a second slot — its own old charge out of the count — at
// once where the new one is no larger, and otherwise as admit would admit it
// beside every other run in flight and the room the latest pass kept
// standing; a refusal keeps the old charge, and a run no longer in flight is
// refused.
//
// Mutations: count the old charge beside the new, and a charge the budget
// holds in its place is held back; drop the old charge on a refusal, and the
// flight forgets a run still in it.
func TestARunsChargeMovesInItsPlace(t *testing.T) {
	t.Parallel()
	for _, k := range []struct {
		name              string
		budget, allowance int64
		others            []int64
		standing          reservation
		old, next         int64
		want              verdict
		runs              []int64
	}{
		{name: "smaller, at once", budget: 100, allowance: 100, old: 60, next: 40, want: admitted, runs: []int64{40}},
		{name: "larger, in its own place", budget: 100, allowance: 100, old: 60, next: 100, want: admitted,
			runs: []int64{100}},
		{name: "past the budget alone", budget: 100, allowance: 100, old: 60, next: 101, want: pastBudget,
			runs: []int64{60}},
		{name: "beside another run", budget: 100, allowance: 100, others: []int64{30}, old: 60, next: 80,
			want: heldBack, runs: []int64{30, 60}},
		{name: "beside the room kept standing", budget: 100, allowance: 100,
			standing: reservation{on: true, memory: 30}, old: 60, next: 80, want: heldBack, runs: []int64{60}},
		{name: "past the allowance beside the largest", budget: 1000, allowance: 50, others: []int64{100}, old: 10,
			next: 60, want: heldBack, runs: []int64{10, 100}},
	} {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			f := &maintFlight{budget: k.budget, runs: append(slices.Clone(k.others), k.old), standing: k.standing}
			mem := k.old
			if got := f.recharge(&mem, k.next, k.allowance); got != k.want {
				t.Errorf("recharge from %d to %d = %v, want %v", k.old, k.next, got, k.want)
			}
			want := k.old
			if k.want == admitted {
				want = k.next
			}
			runs := slices.Sorted(slices.Values(f.runs))
			if mem != want || !slices.Equal(runs, k.runs) {
				t.Errorf("the run holds %d and the flight %v, want %d and %v", mem, runs, want, k.runs)
			}
		})
	}
	f := &maintFlight{budget: 100}
	mem := int64(60)
	if got := f.recharge(&mem, 40, 100); got != notDue || mem != 60 || len(f.runs) != 0 {
		t.Errorf("a run no longer in flight recharged = %v, holding %d in a flight of %v; want refused, as it was",
			got, mem, f.runs)
	}
}
