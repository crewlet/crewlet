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
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(entries); err == nil {
				t.Error("accepted")
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
	r.Add("crewlet.statelog.read.served", 1, Attrs{
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
		r.Observe("crewlet.statelog.barrier.duration", 2*time.Millisecond, Attrs{"domain": "d"})
	}
	r.Observe("crewlet.statelog.barrier.duration", 8*time.Second, Attrs{"domain": "d"})

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
		t.Fatalf("total = %d, want 5", got)
	}
	if got := w.Peak("longest_tx_ms"); got != 16000 {
		t.Fatalf("peak = %v, want 16000", got)
	}

	// TWELVE HOURS ON: still inside the window.
	at = at.Add(12 * time.Hour)
	w.Add("refusals", 2)
	if got := w.Total("refusals"); got != 7 {
		t.Errorf("total after 12h = %d, want 7", got)
	}

	// TWENTY-FIVE HOURS FROM THE FIRST: the first hour is gone.
	at = at.Add(13 * time.Hour)
	if got := w.Total("refusals"); got != 2 {
		t.Errorf("total after 25h = %d, want only the later 2: the first "+
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
