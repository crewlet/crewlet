package placement

import (
	"math"
	"slices"
	"testing"
)

// FILL IS PROPORTIONAL WITHIN BOUNDS ON BOTH SIDES: every entry clamped to
// its bounds, the total met exactly, and every entry the bounds did not hold
// at the same rate per unit of weight — including where holding one entry at
// its ceiling pushes another under its floor, which a repair that holds
// whoever overflows and shares the excess again gets wrong on its first pass.
func TestFillIsProportionalWithinItsBounds(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		weight, lo, hi, want []float64
		total                float64
	}{
		"unbounded": {weight: []float64{1, 2, 3}, lo: []float64{0, 0, 0},
			hi: []float64{100, 100, 100}, total: 60, want: []float64{10, 20, 30}},
		"a ceiling": {weight: []float64{8, 1, 1}, lo: []float64{0, 0, 0},
			hi: []float64{10, 10, 10}, total: 18, want: []float64{10, 4, 4}},
		"a floor": {weight: []float64{1, 9}, lo: []float64{5, 0},
			hi: []float64{20, 20}, total: 20, want: []float64{5, 15}},
		"both, the ceiling pushing the floor": {weight: []float64{10, 1, 1, 1},
			lo: []float64{10, 10, 0, 0}, hi: []float64{20, 20, 20, 20}, total: 40,
			want: []float64{20, 10, 5, 5}},
		"no weight holds its floor": {weight: []float64{0, 1}, lo: []float64{3, 0},
			hi: []float64{9, 9}, total: 8, want: []float64{3, 5}},
		"exactly the floors": {weight: []float64{1, 1}, lo: []float64{4, 6},
			hi: []float64{9, 9}, total: 10, want: []float64{4, 6}},
		"exactly the ceilings": {weight: []float64{1, 5}, lo: []float64{0, 0},
			hi: []float64{2, 3}, total: 5, want: []float64{2, 3}},
	} {
		got := fill(c.weight, c.lo, c.hi, c.total)
		for u := range got {
			if math.Abs(got[u]-c.want[u]) > 1e-9 {
				t.Errorf("%s: fill = %v, want %v", name, got, c.want)
				break
			}
		}
	}
}

// A STEP GROWS WHILE ITS ERROR KEEPS ITS SIGN AND HALVES WHEN IT OVERSHOOTS,
// within its bounds, and a settled round leaves the next with nothing to
// compare against — so a member that settles and later drifts starts again
// from the step it had rather than being shortened for an overshoot it never
// made.
func TestAStepAdaptsToWhatTheLastOneDid(t *testing.T) {
	t.Parallel()
	s := stride{length: firstStep}
	steps := []float64{s.toward(3), s.toward(2), s.toward(1)}
	if want := []float64{firstStep, firstStep * stepGrowth, firstStep * stepGrowth *
		stepGrowth}; !slices.Equal(steps, want) {
		t.Fatalf("an error keeping its sign stepped %v, want %v", steps, want)
	}
	if got := s.toward(-1); got != firstStep*stepGrowth*stepGrowth*stepShrink {
		t.Fatalf("an overshoot stepped %v", got)
	}
	before := s.length
	s.toward(0)
	if got := s.toward(-2); got != before {
		t.Fatalf("after a settled round the step is %v, want %v", got, before)
	}
	for range 100 {
		s.toward(1)
	}
	if s.length != maxStep {
		t.Fatalf("a step growing for ever reached %v, want %v", s.length, maxStep)
	}
	for k := range 100 {
		s.toward(float64(1 - 2*(k%2)))
	}
	if s.length != minStep {
		t.Fatalf("a step overshooting for ever reached %v, want %v", s.length, minStep)
	}
}

// A MAP THAT CANNOT SPLIT IS BALANCED TO WHAT ITS COUNT CAN PROMISE. Reachable
// answers the tolerance asked for wherever it spans a copy and a half of every
// target, and otherwise the tolerance that spans exactly that at the smallest
// — a light member's, not the mean's. And the reason it exists, measured: forty
// equal members over the 321 fixed groups of the estate's default layout
// target 24 copies each, where two percent is half a copy — a tolerance no
// copy count may meet — and at what Reachable answers the balance converges.
func TestReachableIsTheFinestToleranceABalancePromises(t *testing.T) {
	t.Parallel()
	wide := fleet(10, 3) // 3072 copies over ten: 307.2 each, 6.1 copies at 2%
	if got := wide.Reachable(DefaultTolerance); got != DefaultTolerance {
		t.Errorf("a fleet whose targets hold the tolerance was answered %.4f, want %.4f",
			got, DefaultTolerance)
	}

	light := drawOf(3, "", member("a", 1, ""), member("b", 4, ""), member("c", 4, ""),
		member("d", 4, ""))
	light.Groups = partitionsOf(40)
	least := slices.Min(nonZero(targets(newDrawer(light))))
	if got, want := light.Reachable(DefaultTolerance), PromisedWindow/least; math.Abs(got-want) > 1e-12 {
		t.Errorf("a light member's %.2f-copy target was answered %.4f, want %.4f "+
			"— the SMALLEST target governs", least, got, want)
	}

	out := fleet(3, 3)
	for i := range out.Members {
		out.Members[i].Out = true
	}
	if got := out.Reachable(DefaultTolerance); got != DefaultTolerance {
		t.Errorf("a draw with nothing placeable was answered %.4f, want what it asked", got)
	}

	estate := fleet(40, 3)
	estate.Groups = partitionsOf(321)
	_, strict := Balance(estate, BalanceOptions{})
	tolerance := estate.Reachable(DefaultTolerance)
	_, reached := Balance(estate, BalanceOptions{Tolerance: tolerance})
	t.Logf("forty members over 321 groups: at 2%% %+v; at %.4f %+v", strict, tolerance, reached)
	if !reached.Converged {
		t.Fatalf("a balance asked for the tolerance Reachable answered (%.4f) did not "+
			"reach it: %+v", tolerance, reached)
	}
}

// nonZero is the entries that are not zero.
func nonZero(values []float64) []float64 {
	var out []float64
	for _, v := range values {
		if v != 0 {
			out = append(out, v)
		}
	}
	return out
}
