package metrics

import (
	"math"
	"strings"
	"testing"
	"time"
)

// THE CATALOGUE IS COMPLETE, and every entry says which failure it makes
// visible.
//
// The last field is the one that matters: an instrument with no reason is a
// number on a page, which is exactly what this catalogue replaces. A test
// that only checked names and units would let one in.
func TestEveryInstrumentInTheCatalogueIsRegistered(t *testing.T) {
	t.Parallel()
	entries := Catalogue()
	if len(entries) == 0 {
		t.Fatal("the catalogue is empty")
	}
	if err := Validate(entries); err != nil {
		t.Fatalf("the shipped catalogue is malformed: %v", err)
	}

	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, e := range entries {
		if _, known := r.byName[e.Name]; !known {
			t.Errorf("%q is in the catalogue and not in the recorder", e.Name)
		}
	}
}

// A MALFORMED CATALOGUE IS REFUSED at the point the recorder is built, rather
// than discovered on a dashboard.
func TestAMalformedCatalogueIsRefused(t *testing.T) {
	t.Parallel()
	for name, entries := range map[string][]Instrument{
		"no name": {{Kind: KindCounter, Unit: UnitCount, Shows: "x"}},
		"no unit": {{Name: "a", Kind: KindCounter, Shows: "x"}},
		"no reason": {
			{Name: "a", Kind: KindCounter, Unit: UnitCount},
		},
		"declared twice": {
			{Name: "a", Kind: KindCounter, Unit: UnitCount, Shows: "x"},
			{Name: "a", Kind: KindGauge, Unit: UnitCount, Shows: "y"},
		},
		"a gauge marked fractional": {
			{Name: "a", Kind: KindGauge, Unit: UnitCount, Fractional: true, Shows: "x"},
		},
		"a histogram marked fractional": {
			{Name: "a", Kind: KindHistogram, Unit: UnitMilliseconds, Fractional: true, Shows: "x"},
		},
		"a duration summed in whole seconds": {
			{Name: "a", Kind: KindCounter, Unit: UnitSeconds, Shows: "x"},
		},
		"a duration summed in whole milliseconds": {
			{Name: "a", Kind: KindCounter, Unit: UnitMilliseconds, Shows: "x"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(entries); err == nil {
				t.Error("accepted")
			}
		})
	}

	// THE CONTROL for the two duration cases: the same counter marked
	// Fractional is accepted, so the rule refuses the integer form of a
	// summed duration rather than every counter in seconds.
	if err := Validate([]Instrument{{
		Name: "a", Kind: KindCounter, Unit: UnitSeconds, Fractional: true, Shows: "x",
	}}); err != nil {
		t.Errorf("a fractional counter in seconds was refused: %v", err)
	}
}

// totalOf is one instrument's counter total across every attribute set.
func totalOf(reading []Snapshot, name string) float64 {
	var out float64
	for _, s := range reading {
		if s.Name == name {
			out += s.Total
		}
	}
	return out
}

// A SUB-SECOND CONTRIBUTION TO A FRACTIONAL COUNTER IS KEPT, in the cumulative
// series an exporter reads AND in the window the operator record reads.
//
// The fractional counter sums the applier seconds a bulk edit projects, and a
// bulk smaller than one second's drain projects a fraction of a second. As a
// whole number each of those adds zero, and a day of them sums to zero — the
// one answer that looks like a healthy fleet.
//
// Mutation: put `uint64(v)` back into the counter case of [Recorder.record]
// and both readings are zero; truncate in [Window.Add] alone and the windowed
// one is.
func TestAFractionalCounterKeepsEverySubSecondContribution(t *testing.T) {
	t.Parallel()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// NINE HUNDRED AND NINETY-NINE EIGHTHS OF A SECOND: each one under a
	// second, and a total a float64 holds exactly, so the comparison needs
	// no tolerance and no whole-number total can pass it.
	for range 999 {
		r.AddValue(TrackerBulkApplySeconds, 0.125, nil)
	}
	const want = 124.875
	if got := totalOf(r.Read(), TrackerBulkApplySeconds); got != want {
		t.Errorf("cumulative total = %v, want %v: a contribution under one "+
			"second has to be kept, not rounded away", got, want)
	}
	if got := totalOf(r.ReadWindow(), TrackerBulkApplySeconds); got != want {
		t.Errorf("windowed total = %v, want %v: the operator record reads this "+
			"one, and it has to agree with the series the collector reads",
			got, want)
	}
}

// A FRACTION REACHES ONLY A COUNTER THAT CAN HOLD ONE.
//
// A counter that is not Fractional is exported as an integer, so a fraction the
// recorder kept in it would be on the operator record and dropped on the
// collector's panel. A whole increment is exact in either arithmetic, which is
// why a fractional counter still takes one from [Recorder.Add].
//
// Mutation: drop the Fractional check in [Recorder.AddValue] and the integer
// counter holds half a read.
func TestAFractionReachesOnlyAFractionalCounter(t *testing.T) {
	t.Parallel()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	served := Attrs{"domain": "tracker", "level": "linearizable"}
	r.AddValue(StatelogReadServed, 0.5, served)
	if got := totalOf(r.Read(), StatelogReadServed); got != 0 {
		t.Errorf("a counter exported as an integer holds %v after a fraction "+
			"was offered to it, want nothing: the collector would read a "+
			"different number from the operator record", got)
	}
	// THE CONTROL: the same counter takes the whole increments it counts.
	r.Add(StatelogReadServed, 2, served)
	if got := totalOf(r.Read(), StatelogReadServed); got != 2 {
		t.Errorf("the integer counter holds %v after Add(2), want 2", got)
	}

	r.Add(TrackerBulkApplySeconds, 2, nil)
	if got := totalOf(r.Read(), TrackerBulkApplySeconds); got != 2 {
		t.Errorf("a fractional counter holds %v after Add(2), want 2: a whole "+
			"increment is exact in either arithmetic", got)
	}
}

// A COUNTER ONLY RISES, so an amount that would lower it or poison it is
// refused.
//
// Mutation: drop the check at the top of [Recorder.AddValue] and each case
// reads its bad amount in the total: -0.5, NaN, +Inf.
func TestAFractionalCounterRefusesAnAmountThatWouldNotRise(t *testing.T) {
	t.Parallel()
	for name, amount := range map[string]float64{
		"negative": -1,
		"NaN":      math.NaN(),
		"infinite": math.Inf(1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, err := New()
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			r.AddValue(TrackerBulkApplySeconds, 0.5, nil)
			r.AddValue(TrackerBulkApplySeconds, amount, nil)
			if got := totalOf(r.Read(), TrackerBulkApplySeconds); got != 0.5 {
				t.Errorf("cumulative total after %v = %v, want the 0.5 before it",
					amount, got)
			}
			if got := totalOf(r.ReadWindow(), TrackerBulkApplySeconds); got != 0.5 {
				t.Errorf("windowed total after %v = %v, want the 0.5 before it",
					amount, got)
			}
		})
	}
}

// ATTRIBUTE CARDINALITY IS CLOSED BY CONSTRUCTION: an attribute the catalogue
// does not declare is dropped rather than carried.
//
// Mutation: keep undeclared attributes and one caller passing a task key opens
// a time series per task, which is how a metrics backend falls over.
func TestAttributeCardinalityIsClosed(t *testing.T) {
	t.Parallel()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Add(StatelogReadServed, 1, Attrs{
		"domain": "tracker", "level": "linearizable",
		// The one that must not survive.
		"task_key": "ENG-1",
	})
	got := r.Read()
	if len(got) != 1 {
		t.Fatalf("series = %d, want 1", len(got))
	}
	if _, leaked := got[0].Attrs["task_key"]; leaked {
		t.Error("an undeclared attribute reached the series: cardinality here " +
			"is bounded by the catalogue, and a per-object attribute is a " +
			"time series per object")
	}
	if got[0].Attrs["domain"] != "tracker" || got[0].Attrs["level"] != "linearizable" {
		t.Errorf("declared attributes = %v, want both kept", got[0].Attrs)
	}
}

// AN UNKNOWN NAME IS DROPPED, so a typo cannot become an undocumented series.
func TestAnUnknownInstrumentIsDropped(t *testing.T) {
	t.Parallel()
	r, _ := New()
	r.Add("crewlet.statelog.read.serverd", 1, nil) // typo
	if got := r.Read(); len(got) != 0 {
		t.Errorf("a misspelled instrument created %d series: it would be a "+
			"time series with no catalogue entry and no documentation", len(got))
	}
}

// THE QUANTILE NEVER UNDERSTATES, which is the direction a budget check wants
// to be wrong in.
func TestQuantileIsAnUpperBound(t *testing.T) {
	t.Parallel()
	r, _ := New()
	// 99 fast observations and one slow one, so p95 must land on the fast
	// side and p99+ must see the slow one.
	for range 99 {
		r.Observe(StatelogBarrierDuration, 2*time.Millisecond, Attrs{"domain": "d"})
	}
	r.Observe(StatelogBarrierDuration, 8*time.Second, Attrs{"domain": "d"})

	s := r.Read()[0]
	p95 := s.Quantile(0.95)
	if p95 > 4 {
		t.Errorf("p95 = %v ms, want the fast bucket: one slow observation in a "+
			"hundred must not move it", p95)
	}
	if p95 < 2 {
		t.Errorf("p95 = %v ms, want at least the 2 ms actually observed: the "+
			"estimate is the bucket's UPPER boundary and must never "+
			"understate", p95)
	}
	if p100 := s.Quantile(1); p100 < 8000 {
		t.Errorf("p100 = %v ms, want the 8 s observation to be visible", p100)
	}
	if got := s.Quantile(0); got != 0 {
		t.Errorf("Quantile(0) = %v, want 0", got)
	}
	if s.Count != 100 {
		t.Errorf("count = %d, want 100", s.Count)
	}
	if math.Abs(s.Sum-(99*2+8000)) > 1 {
		t.Errorf("sum = %v, want ~%v", s.Sum, 99*2+8000)
	}
}

// A WINDOWED VALUE FORGETS AFTER 24 HOURS, which is what makes `_24h` mean
// something definite.
//
// Mutation: a maximum since boot, and one sixteen-second transaction last
// Tuesday masks every transaction after it.
func TestAWindowedValueForgetsAfter24Hours(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	w := NewWindow(clock)

	w.Add("refusals", 5)
	w.Max("longest_tx_ms", 16000)
	if got := w.Total("refusals"); got != 5 {
		t.Fatalf("total = %v, want 5", got)
	}
	if got := w.Peak("longest_tx_ms"); got != 16000 {
		t.Fatalf("peak = %v, want 16000", got)
	}

	// TWELVE HOURS ON: still inside the window.
	at = at.Add(12 * time.Hour)
	w.Add("refusals", 2)
	if got := w.Total("refusals"); got != 7 {
		t.Errorf("total after 12h = %v, want 7", got)
	}

	// TWENTY-FIVE HOURS FROM THE FIRST: the first hour is gone.
	at = at.Add(13 * time.Hour)
	if got := w.Total("refusals"); got != 2 {
		t.Errorf("total after 25h = %v, want only the later 2: the first "+
			"hour's contribution has to leave the window", got)
	}
	if got := w.Peak("longest_tx_ms"); got != 0 {
		t.Errorf("peak after 25h = %v, want 0: a maximum with no window is a "+
			"number from last week masking today's", got)
	}
}

// A YOUNG WINDOW SAYS SO. A node up for ten minutes reporting no refusals in
// a day is telling an operator something it cannot know.
func TestAYoungWindowIsLabelledPartial(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	w := NewWindow(func() time.Time { return at })

	if !w.Partial() {
		t.Error("a window created a moment ago is not marked partial")
	}
	at = at.Add(10 * time.Minute)
	if !w.Partial() {
		t.Error("a ten-minute-old window is not marked partial")
	}
	if got := w.Since(); got != 10*time.Minute {
		t.Errorf("Since() = %v, want 10m", got)
	}
	at = at.Add(24 * time.Hour)
	if w.Partial() {
		t.Error("a window older than its own period is still marked partial")
	}
}

// EVERY INSTRUMENT HAS ITS OWN NAME, and nothing else asserted it.
//
// Two entries for one measurement is not a tidiness problem: the catalogue is
// what the reference page and the recorder are both derived from, so a
// duplicate ships as two rows an operator has to choose between and two series
// that each carry half the events. It happened — `apply.tx.aborts` and
// `apply.tx_aborts` were one measurement under two spellings, differing only
// in a separator — and nothing here noticed.
func TestNoTwoInstrumentsShareAName(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, inst := range Catalogue() {
		if _, dup := seen[inst.Name]; dup {
			t.Errorf("%q appears twice — one measurement under two entries is "+
				"two series each carrying half the events", inst.Name)
		}
		seen[inst.Name] = inst.Shows
	}

	// AND NO TWO NAMES DIFFER ONLY IN A SEPARATOR, which is how the
	// duplicate above got in: `a.b.c` and `a.b_c` are one name to a reader
	// and two to every collector.
	flat := map[string]string{}
	for name := range seen {
		key := strings.ReplaceAll(name, "_", ".")
		if first, dup := flat[key]; dup {
			t.Errorf("%q and %q differ only in a separator — one of them is a "+
				"second spelling of the other", first, name)
		}
		flat[key] = name
	}
}

// AN ALARM MUST BE ABLE TO GO OUT, and against a cumulative counter it cannot.
//
// This is the defect the window was written for and then never wired to.
// `search_degraded` fires on a fraction being above zero; computed from
// [Recorder.Read]'s monotone totals, one degraded search after boot lights it
// for the life of the process, because the numerator can only grow. Computed
// from [Recorder.ReadWindow] it clears once the hour holding it rolls out.
//
// `crewlet retention status` derives its exit code from these alarms, so an
// alarm that cannot clear is a cron that fires for ever.
func TestAWindowedCounterFallsBackToZeroAndACumulativeOneNever(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r = r.WithClock(clock)

	total := func(read func() []Snapshot) float64 {
		return totalOf(read(), StatelogReadRefusals)
	}

	r.Add(StatelogReadRefusals, 1, Attrs{"domain": "tracker", "code": "stalled"})
	if got := total(r.ReadWindow); got != 1 {
		t.Fatalf("windowed total right after the event = %v, want 1", got)
	}
	if got := total(r.Read); got != 1 {
		t.Fatalf("cumulative total right after the event = %v, want 1", got)
	}

	// A DAY LATER, WITH NOTHING SINCE. The window has rolled the hour that
	// held it out; the cumulative series never will.
	at = at.Add(Buckets*time.Hour + time.Hour)
	if got := total(r.ReadWindow); got != 0 {
		t.Errorf("windowed total a day later = %v, want 0 — an alarm built on "+
			"this can never go out", got)
	}
	if got := total(r.Read); got != 1 {
		t.Errorf("cumulative total a day later = %v, want 1 — the counter an "+
			"exporter diffs must keep growing", got)
	}
}

// A WINDOWED QUANTILE IS A QUANTILE, not the window's maximum.
//
// The p95 alarms compare against a budget, and a maximum is above the p95 by
// construction — so substituting one would fire every such alarm on the single
// worst observation in the window rather than on a distribution that moved.
func TestTheWindowKeepsADistributionRatherThanAPeak(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r = r.WithClock(func() time.Time { return at })

	// Ninety-nine fast barriers and one very slow one.
	for range 99 {
		r.Observe(StatelogBarrierDuration, time.Millisecond, Attrs{"domain": "tracker"})
	}
	r.Observe(StatelogBarrierDuration, 30*time.Second, Attrs{"domain": "tracker"})

	var windowed Snapshot
	for _, s := range r.ReadWindow() {
		if s.Name == StatelogBarrierDuration {
			windowed = s
		}
	}
	if windowed.Count != 100 {
		t.Fatalf("windowed count = %d, want 100 — the bins did not reach the reading",
			windowed.Count)
	}
	p95 := windowed.Quantile(0.95)
	peak := r.Window().Peak(seriesKey(StatelogBarrierDuration,
		[]string{"domain"}, Attrs{"domain": "tracker"}))
	if p95 >= peak {
		t.Errorf("windowed p95 = %v and peak = %v — a p95 at or above the peak "+
			"means the reading is a maximum wearing a quantile's name", p95, peak)
	}
	if p95 > 16 {
		t.Errorf("windowed p95 = %v ms, want the fast bucket: ninety-nine "+
			"observations at 1 ms and one at 30 s has a p95 near 1 ms", p95)
	}
}
