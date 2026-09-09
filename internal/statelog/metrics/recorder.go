package metrics

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// bins are the histogram's boundaries, in milliseconds.
//
// TWENTY-ONE POWER-OF-TWO BUCKETS from 64 µs to 64 s, which resolves a
// percentile to within a factor of two at every scale this engine measures —
// from a 40 µs index probe to a 16 s bulk apply. Fixed rather than
// configurable: a boundary set that differs between nodes cannot be merged,
// and merging across a fleet is most of what these are for.
//
// The alternative — exact quantiles — needs either unbounded memory or a
// sketch with its own error bounds, for an answer nobody acts on more
// precisely than "is this an order of magnitude worse than yesterday".
var bins = func() []float64 {
	out := make([]float64, 0, 21)
	for v := 0.0625; len(out) < 21; v *= 2 {
		out = append(out, v)
	}
	return out
}()

// Bins exposes the histogram boundaries, so an exporter registers the same
// ones the recorder counts into.
func Bins() []float64 { return append([]float64(nil), bins...) }

// Recorder is the one place a measurement is written.
//
// It holds counters, gauges and fixed-bin histograms keyed by instrument name
// and attribute set, and it is safe for concurrent use by every goroutine in
// the process — which is the point: one atomic add is what makes a number on
// the operator record and a number on a collector's panel the same number.
//
// # Why it is not the OTel API directly
//
// Two readers, not one. The OTel instruments are one; the rolling 24-hour
// window the operator record renders is the other, and the OTel SDK offers no
// way to read back what was recorded. A second counting path for the second
// reader is the drift this design exists to prevent.
type Recorder struct {
	mu     sync.Mutex
	series map[string]*series
	byName map[string]Instrument

	// now is injectable so a window's expiry is testable without sleeping
	// through it. Nil takes the wall clock.
	now func() time.Time

	// sinks are the exporter's synchronous histogram instruments, bound
	// after the provider exists.
	//
	// HISTOGRAMS ARE THE ONE KIND THAT CANNOT BE OBSERVED AFTER THE FACT:
	// a counter and a gauge have a current value the exporter can ask for
	// at export time, and a distribution does not — the observations have
	// to reach it as they happen. So the recorder keeps its own copy (for
	// the operator record, which reads back) and forwards to the sink (for
	// the collector, which cannot).
	//
	// Nil until an exporter binds one, which is the ordinary state of a
	// deployment with no collector: the recorder's own copy still answers.
	sinks map[string]func(float64, map[string]string)
}

// series is one instrument at one attribute set.
type series struct {
	inst   Instrument
	attrs  map[string]string
	counts []uint64 // histogram bins, one past the last boundary
	sum    float64
	n      uint64
	value  float64 // gauge
	total  uint64  // counter
}

// New builds a recorder over the catalogue, refusing a malformed one.
func New() (*Recorder, error) {
	entries := Catalogue()
	if err := Validate(entries); err != nil {
		return nil, err
	}
	byName := make(map[string]Instrument, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	return &Recorder{
		series: map[string]*series{},
		byName: byName,
		sinks:  map[string]func(float64, map[string]string){},
	}, nil
}

// BindHistogram attaches an exporter's instrument to a histogram.
//
// Called once per histogram by whoever installs the MeterProvider. A nil sink
// is accepted and ignored — an instrument the exporter could not build records
// through the recorder's own copy and nowhere else, which is the honest
// degradation for telemetry.
func (r *Recorder) BindHistogram(name string, sink func(float64, map[string]string)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, known := r.byName[name]
	if !known || inst.Kind != KindHistogram {
		return fmt.Errorf("metrics: %q is not a histogram in the catalogue", name)
	}
	if sink != nil {
		r.sinks[name] = sink
	}
	return nil
}

// WithClock returns r with an injected clock, for tests.
func (r *Recorder) WithClock(now func() time.Time) *Recorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = now
	return r
}

// Attrs is one instrument's dimensions for a single measurement.
//
// A MAP RATHER THAN A SLICE so a caller cannot silently swap two values of the
// same type — `("tracker", "linearizable")` and `("linearizable", "tracker")`
// are both two strings and only one is right.
type Attrs map[string]string

// Observe records one measurement into a histogram.
//
// A DURATION rather than a float, because every histogram in this catalogue is
// a latency and a caller converting to milliseconds itself is a caller that
// can convert to seconds by mistake.
func (r *Recorder) Observe(name string, d time.Duration, attrs Attrs) {
	r.record(name, KindHistogram, float64(d)/float64(time.Millisecond), attrs)
}

// ObserveValue records one measurement into a histogram whose unit is not a
// duration — a row count, a round count.
func (r *Recorder) ObserveValue(name string, v float64, attrs Attrs) {
	r.record(name, KindHistogram, v, attrs)
}

// Add increments a counter by a whole number of events.
func (r *Recorder) Add(name string, n uint64, attrs Attrs) {
	r.record(name, KindCounter, float64(n), attrs)
}

// AddValue increments a counter by a fractional amount.
//
// A COUNTER NEED NOT COUNT EVENTS. Some of what is summed here is a duration
// or a projection — seconds of applier occupancy a bulk edit imposes, where
// one call's contribution is a few hundredths — and rounding each to a whole
// number sums a company's whole day to zero, which is the one answer that
// looks like a healthy fleet.
func (r *Recorder) AddValue(name string, v float64, attrs Attrs) {
	r.record(name, KindCounter, v, attrs)
}

// Set writes a gauge.
func (r *Recorder) Set(name string, v float64, attrs Attrs) {
	r.record(name, KindGauge, v, attrs)
}

// record is the one write path. An unknown name is DROPPED rather than
// registered on the fly: a typo would otherwise become a time series with no
// entry in the catalogue and no documentation, which is the state the
// catalogue exists to make impossible.
func (r *Recorder) record(name string, kind Kind, v float64, attrs Attrs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, known := r.byName[name]
	if !known || inst.Kind != kind {
		return
	}
	s := r.seriesFor(inst, attrs)
	switch kind {
	case KindCounter:
		s.total += uint64(v)
	case KindGauge:
		s.value = v
	case KindHistogram:
		s.counts[binFor(v)]++
		s.sum += v
		s.n++
		if sink := r.sinks[name]; sink != nil {
			// FORWARDED UNDER THE LOCK, which is deliberate: the SDK's
			// own Record is lock-free and cheap, and releasing here to
			// re-acquire would let two observations reach the exporter in
			// the opposite order to the recorder's own copy.
			sink(v, s.attrs)
		}
	}
}

// seriesFor finds or creates the series for one attribute set. Callers hold
// the lock.
func (r *Recorder) seriesFor(inst Instrument, attrs Attrs) *series {
	key := seriesKey(inst.Name, inst.Attributes, attrs)
	if s, ok := r.series[key]; ok {
		return s
	}
	kept := map[string]string{}
	for _, a := range inst.Attributes {
		// ONLY DECLARED ATTRIBUTES. An undeclared one is dropped rather
		// than carried: cardinality here is bounded by the catalogue, and
		// a caller passing a task key would otherwise open a time series
		// per task.
		if v, ok := attrs[a]; ok {
			kept[a] = v
		}
	}
	s := &series{inst: inst, attrs: kept, counts: make([]uint64, len(bins)+1)}
	r.series[key] = s
	return s
}

// seriesKey renders an instrument plus its declared attributes as one string.
func seriesKey(name string, declared []string, attrs Attrs) string {
	var b strings.Builder
	b.WriteString(name)
	for _, a := range declared {
		b.WriteString("\x00")
		b.WriteString(a)
		b.WriteString("=")
		b.WriteString(attrs[a])
	}
	return b.String()
}

// binFor is the index of the bucket a value falls in.
func binFor(v float64) int {
	i := sort.SearchFloat64s(bins, v)
	if i > len(bins) {
		i = len(bins)
	}
	return i
}

// Snapshot is one series as a reader sees it.
type Snapshot struct {
	Name  string
	Kind  Kind
	Unit  string
	Attrs map[string]string

	// Total is a counter's sum; Value is a gauge's current reading.
	Total uint64
	Value float64

	// Count, Sum and Counts describe a histogram: how many observations,
	// their total, and the per-bin counts against [Bins].
	Count  uint64
	Sum    float64
	Counts []uint64
}

// Quantile estimates the qth quantile of a histogram, in the instrument's own
// unit.
//
// AN ESTIMATE, and the doc says so where a caller reads it: the answer is the
// UPPER boundary of the bucket the qth observation falls in, so it is accurate
// to within one bucket — a factor of two — and never understates. A p95 that
// reads 4 ms means "at most 4 ms", which is the direction a budget check wants
// to be wrong in.
func (s Snapshot) Quantile(q float64) float64 {
	if s.Count == 0 || q <= 0 || q > 1 {
		return 0
	}
	want := uint64(math.Ceil(q * float64(s.Count)))
	var seen uint64
	for i, c := range s.Counts {
		seen += c
		if seen >= want {
			if i >= len(bins) {
				return math.Inf(1)
			}
			return bins[i]
		}
	}
	return math.Inf(1)
}

// Read returns every series, in name and attribute order so two captures are
// diffable.
func (r *Recorder) Read() []Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Snapshot, 0, len(r.series))
	for _, s := range r.series {
		out = append(out, Snapshot{
			Name: s.inst.Name, Kind: s.inst.Kind, Unit: s.inst.Unit,
			Attrs: cloneAttrs(s.attrs),
			Total: s.total, Value: s.value,
			Count: s.n, Sum: s.sum, Counts: append([]uint64(nil), s.counts...),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return attrString(out[i].Attrs) < attrString(out[j].Attrs)
	})
	return out
}

func cloneAttrs(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func attrString(in map[string]string) string {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s;", k, in[k])
	}
	return b.String()
}

// clock reads the injected or wall clock.
func (r *Recorder) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
