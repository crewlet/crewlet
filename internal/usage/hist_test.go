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
