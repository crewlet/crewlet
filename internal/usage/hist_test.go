package usage_test

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/usage"
)

// A QUANTILE IS WITHIN THE STATED RESOLUTION OF THE EXACT ONE, anywhere in the
// range.
//
// The ±6% is a number a dashboard prints beside every percentile it reads out
// of this histogram, so it is a promise rather than a description: a bin one
// step wider, or a read-back at the bin's lower edge instead of its geometric
// centre, doubles the error and the promise is false on every seat.
func TestAQuantileIsWithinTheStatedResolution(t *testing.T) {
	t.Parallel()
	if usage.QuantileResolution > 0.06 {
		t.Fatalf("the stated resolution is %.4f, over the 6%% the answers promise",
			usage.QuantileResolution)
	}
	rng := rand.New(rand.NewPCG(7, 11))
	for trial := range 200 {
		n := 1 + rng.IntN(400)
		var h usage.Hist
		exact := make([]time.Duration, 0, n)
		for range n {
			// LOG-UNIFORM over the stated range, which is where turn
			// durations live: most are tens of seconds and a few are
			// hours, and a uniform draw would test only the top decade.
			d := time.Duration(float64(usage.HistFloor) *
				math.Exp(rng.Float64()*math.Log(float64(usage.HistCeiling)/float64(usage.HistFloor))))
			h.Add(d)
			exact = append(exact, d)
		}
		slices.Sort(exact)
		for _, q := range []float64{0.5, 0.9, 0.99} {
			want := exact[int(math.Ceil(q*float64(n)))-1]
			got, ok := h.Quantile(q)
			if !ok {
				t.Fatalf("trial %d: no p%.0f from %d durations", trial, q*100, n)
			}
			if rel := math.Abs(float64(got)-float64(want)) / float64(want); rel > usage.QuantileResolution+1e-9 {
				t.Fatalf("trial %d: p%.0f of %d durations read back as %s against "+
					"%s exactly — %.2f%% off, and the stated resolution is %.2f%%",
					trial, q*100, n, got, want, rel*100, usage.QuantileResolution*100)
			}
		}
	}
}

// AN EMPTY HISTOGRAM HAS NO QUANTILE, rather than a zero one — a zero reads as
// a seat whose turns take no time.
func TestAnEmptyHistogramHasNoQuantile(t *testing.T) {
	t.Parallel()
	var h usage.Hist
	if d, ok := h.Quantile(0.5); ok {
		t.Fatalf("an empty histogram answered p50 = %s", d)
	}
}

// TWO HISTOGRAMS MERGE TO THE HISTOGRAM OF BOTH, which is the whole reason this
// is a histogram: a week is seven days merged, a fleet is three nodes merged,
// and neither may differ from having counted every turn in one place.
func TestTwoHistogramsMergeToTheHistogramOfBoth(t *testing.T) {
	t.Parallel()
	var a, b, both usage.Hist
	for i := range 50 {
		d := time.Duration(i+1) * 7 * time.Second
		if i%3 == 0 {
			a.Add(d)
		} else {
			b.Add(d)
		}
		both.Add(d)
	}
	a.Merge(b)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(both)
	if string(ja) != string(jb) {
		t.Fatalf("merged %s, counted together %s", ja, jb)
	}
	if a.Count() != 50 {
		t.Fatalf("the merge holds %d durations, not 50", a.Count())
	}
}

// A HISTOGRAM ROUND-TRIPS, AND REFUSES WHAT NO BUILD WROTE: a bin outside the
// range, a zero count and a bin named twice would each be merged into every
// window that read it, as durations nobody measured.
func TestAHistogramRoundTripsAndRefusesWhatNoBuildWrote(t *testing.T) {
	t.Parallel()
	var h usage.Hist
	for _, d := range []time.Duration{2 * time.Second, 40 * time.Second, 40 * time.Second, 3 * time.Hour} {
		h.Add(d)
	}
	body, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var back usage.Hist
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	again, _ := json.Marshal(back)
	if string(again) != string(body) {
		t.Fatalf("round trip %s became %s", body, again)
	}
	for name, raw := range map[string]string{
		"a bin past the range": `[[500,1]]`,
		"a negative bin":       `[[-1,1]]`,
		"a zero count":         `[[3,0]]`,
		"a bin named twice":    `[[3,1],[3,2]]`,
	} {
		if err := json.Unmarshal([]byte(raw), &back); err == nil {
			t.Errorf("%s (%s) decoded", name, raw)
		}
	}
}

// histOf is a histogram of the durations given.
func histOf(ds ...time.Duration) usage.Hist {
	var h usage.Hist
	for _, d := range ds {
		h.Add(d)
	}
	return h
}

// histBytes is a histogram as it is written, for comparing two.
func histBytes(t *testing.T, h usage.Hist) string {
	t.Helper()
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A HISTOGRAM JOINS ON WHAT IS AT OR ABOVE EVERY BIN, never bin by bin.
//
// A turn whose end states no tally is measured over the segments its node
// holds, so a later segment moves it UP a bin and the bin it left loses a
// count: the earlier histogram is not below the later one bin by bin, though
// it is an earlier measurement of the same turns. Joined bin by bin, the turn
// would be counted twice — once where it was and once where it is — and the
// day would grow a turn nobody ran. What only grows is the count at or above
// every bin, so that is what the join keeps; where the fleet's histogram holds
// MORE there, the join holds that much.
//
// Mutation: join bin by bin (the larger count at every bin), and the turn that
// moved is counted twice; or take the larger total alone, and the fleet's long
// turn is lost.
func TestAHistogramJoinsOnWhatIsAtOrAboveEveryBin(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name          string
		derived, held usage.Hist
		want          usage.Hist
	}{
		{"an earlier measurement of a turn that moved up a bin is the derivation",
			histOf(time.Minute, 2*time.Hour), histOf(time.Minute, 10*time.Minute),
			histOf(time.Minute, 2*time.Hour)},
		{"a turn the fleet holds that the derivation lost is kept",
			histOf(time.Minute), histOf(time.Minute, 2*time.Hour),
			histOf(time.Minute, 2*time.Hour)},
		{"a long turn the fleet holds raises the tail, and the derivation's new short " +
			"turns keep their own bins",
			histOf(time.Second, time.Second, time.Minute), histOf(time.Minute, 2*time.Hour),
			histOf(time.Second, time.Minute, 2*time.Hour)},
		{"nothing held is the derivation", histOf(time.Minute), usage.Hist{}, histOf(time.Minute)},
		{"nothing derived is the fleet's", usage.Hist{}, histOf(time.Hour), histOf(time.Hour)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := c.derived.Join(c.held)
			if histBytes(t, got) != histBytes(t, c.want) {
				t.Errorf("the join is %s, want %s", histBytes(t, got), histBytes(t, c.want))
			}
			if got.Count() != max(c.derived.Count(), c.held.Count()) {
				t.Errorf("the join counts %d turns, want the larger of %d and %d",
					got.Count(), c.derived.Count(), c.held.Count())
			}
		})
	}
}
