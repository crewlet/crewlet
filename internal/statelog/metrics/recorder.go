package metrics

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// defaultBounds are a histogram's boundaries when its catalogue entry names
// none, in the instrument's own unit — milliseconds for a duration.
//
// TWENTY-ONE POWERS OF TWO from 1/16 to 65 536, which for a duration is
// 62.5 µs to about 65.5 s, so a percentile read from them is within a factor of
// two anywhere in that range. Fixed rather than configurable: a boundary set
// that differs between nodes cannot be merged, and merging across a fleet is
// most of what these are for — which is also why an instrument that needs
// another range declares it in the catalogue ([Instrument.Bounds]), where every
// node reads the same one.
//
// The alternative — exact quantiles — needs either unbounded memory or a
// sketch with its own error bounds, for an answer nobody acts on more
// precisely than "is this an order of magnitude worse than yesterday".
var defaultBounds = powersOfTwo(-4, 16)

// powersOfTwo is every power of two from 2^low to 2^high, in order.
func powersOfTwo(low, high int) []float64 {
	out := make([]float64, 0, high-low+1)
	for exp := low; exp <= high; exp++ {
		out = append(out, math.Ldexp(1, exp))
	}
	return out
}

// DefaultBounds is a copy of the boundaries a histogram with none of its own
// counts into.
func DefaultBounds() []float64 { return append([]float64(nil), defaultBounds...) }

// Recorder is the one place a measurement is written.
//
// It holds counters, gauges and fixed-bin histograms keyed by instrument name
// and attribute set, and it is safe for concurrent use by every goroutine in
// the process — which is the point: every write updates both views under one
// lock, the cumulative series an exporter reads and the rolling window the
// alarms read.
//
// # Why it is not the OTel API directly
//
// Two readers, not one. The OTel instruments are one; the rolling 24-hour
// window the operator record's alarms read is the other, and the OTel SDK
// offers no way to read back what was recorded. A second counting path for the
// second reader is the drift this design exists to prevent.
type Recorder struct {
	mu     sync.Mutex
	series map[string]*series
	byName map[string]Instrument

	// sinks are the exporter's synchronous histogram instruments, bound
	// after the provider exists.
	//
	// HISTOGRAMS ARE THE ONE KIND THAT CANNOT BE OBSERVED AFTER THE FACT:
	// a counter and a gauge have a current value the exporter can ask for
	// at export time, and a distribution does not — the observations have
	// to reach it as they happen. So the recorder keeps its own copy (for
	// the alarms, which read back) and forwards to the sink (for the
	// collector, which cannot).
	//
	// A histogram with no sink bound — a recorder nothing installed a
	// provider over, as in a test — records into the recorder's own copy
	// alone, which still answers every read.
	sinks map[string]func(float64, map[string]string)

	// window is the ROLLING 24-hour view the operator record's alarms
	// read, fed from the same one write path as the cumulative series
	// beside it.
	//
	// TWO VIEWS OF ONE MEASUREMENT, and both are needed. `series` is
	// cumulative since this process started, which is what an exporter
	// scrapes and diffs. The alarms cannot use it: `search_degraded` fires
	// on a fraction being above zero, so against a monotone counter one
	// degraded search after boot lights it for the life of the process and
	// it can never go out — and `search_slow` and `barrier_slow` take a
	// p95, which over a cumulative distribution only ever dilutes a bad
	// hour and never forgets it: the alarm clears once enough good
	// observations have piled on top, not once the problem has.
	window *Window
}

// series is one instrument at one attribute set.
type series struct {
	inst   Instrument
	attrs  map[string]string
	counts []uint64 // histogram bins, one past the last boundary
	sum    float64
	n      uint64
	value  float64 // gauge

	// total is a counter's sum.
	//
	// FLOAT64 FOR EVERY COUNTER, whole or [Instrument.Fractional], so a
	// counter's total has one field and no reader can pick the other. A
	// counter that counts events loses nothing by it: a float64 holds every
	// whole number up to 2^53 exactly, which at a million increments a
	// second is 285 years of one process.
	total float64
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
		window: NewWindow(nil),
	}, nil
}

// Window is the rolling 24-hour view of everything recorded here.
//
// EVERY ALARM WITH A `_24h` IN ITS NAME READS THIS, and none may read the
// cumulative series beside it. A threshold applied to a counter that only
// ever grows is a threshold that latches: it fires at the first observation
// past it and stays lit until the process restarts, which is an alarm an
// operator learns to ignore rather than one they act on.
func (r *Recorder) Window() *Window {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.window
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
	// THE WINDOW IS THE ONLY THING HERE THAT READS A CLOCK: the recorder's
	// own series are counters, gauges and histograms, none of which carries
	// a timestamp. So the clock goes straight into the window whose expiry
	// it exists to test, rather than onto a field that would advertise an
	// injectability the recorder does not have.
	r.window = NewWindow(now)
	return r
}

// Attrs is one instrument's dimensions for a single measurement.
//
// A MAP RATHER THAN A SLICE so a caller cannot silently swap two values of the
// same type — `("tracker", "linearizable")` and `("linearizable", "tracker")`
// are both two strings and only one is right.
type Attrs map[string]string

// Observe records one measurement into a histogram in milliseconds.
//
// A DURATION rather than a float, because a caller converting to milliseconds
// itself is a caller that can convert to seconds by mistake.
func (r *Recorder) Observe(name string, d time.Duration, attrs Attrs) {
	r.record(name, KindHistogram, float64(d)/float64(time.Millisecond), attrs)
}

// ObserveValue records one measurement into a histogram whose unit is not a
// duration — a row count, a round count.
func (r *Recorder) ObserveValue(name string, v float64, attrs Attrs) {
	r.record(name, KindHistogram, v, attrs)
}

// Add increments a counter by a whole amount — for most counters, a number of
// events.
//
// EVERY COUNTER TAKES IT, an [Instrument.Fractional] one included: a whole
// increment is exact in either arithmetic.
func (r *Recorder) Add(name string, n uint64, attrs Attrs) {
	r.record(name, KindCounter, float64(n), attrs)
}

// AddValue increments an [Instrument.Fractional] counter by an amount that
// need not be whole.
//
// A COUNTER NEED NOT COUNT EVENTS. Some of what is summed here is a duration
// or a projection — the seconds of applier occupancy a bulk edit is projected
// to impose, which is a fraction of a second for every bulk smaller than one
// second's drain — and truncated to a whole number each of those adds nothing,
// so a day of them sums to zero, which is the one answer that looks like a
// healthy fleet.
//
// REFUSED ON A COUNTER THAT IS NOT FRACTIONAL, because that counter is
// exported as an integer: a fraction added to it would stay in the recorder's
// own readings and be truncated out of the export, two numbers for one
// measurement. byName is read here without the lock because only [New] writes
// it, before the recorder is returned.
//
// REFUSED WHEN THE AMOUNT IS NEGATIVE, NaN OR INFINITE, because a counter only
// rises: a negative amount would make it fall, and a NaN or an infinity would
// stay in the total for the life of the process.
func (r *Recorder) AddValue(name string, v float64, attrs Attrs) {
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	if inst, known := r.byName[name]; !known || !inst.Fractional {
		return
	}
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
	key := seriesKey(inst.Name, inst.Attributes, attrs)
	switch kind {
	case KindCounter:
		s.total += v
		r.window.Add(key, v)
	case KindGauge:
		s.value = v
		r.window.Max(key, v)
	case KindHistogram:
		bounds := inst.bounds()
		r.window.Observe(key, bounds, v)
		s.counts[binFor(bounds, v)]++
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
	s := &series{inst: inst, attrs: kept}
	if inst.Kind == KindHistogram {
		s.counts = make([]uint64, len(inst.bounds())+1)
	}
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

// binFor is the index of the bucket a value falls in among bounds: the first
// boundary at or above it, or len(bounds) — the overflow bucket — when none is,
// which is what [sort.SearchFloat64s] returns for a value past the last
// boundary and for a NaN alike.
func binFor(bounds []float64, v float64) int {
	return sort.SearchFloat64s(bounds, v)
}

// Snapshot is one series as a reader sees it.
type Snapshot struct {
	Name  string
	Kind  Kind
	Unit  string
	Attrs map[string]string

	// Total is a counter's sum; Value is a gauge's reading — its current
	// one from [Recorder.Read], the highest it reached from
	// [Recorder.ReadWindow].
	//
	// Total is a float64 for every counter, for the reason at the series'
	// own total: a counter that counts events reads back as a whole number,
	// exactly, and an [Instrument.Fractional] one keeps its fraction.
	Total float64
	Value float64

	// Count, Sum and Counts describe a histogram: how many observations,
	// their total, and the per-bin counts against Bounds — one more count
	// than boundaries, the last being the overflow past every one of them.
	Count  uint64
	Sum    float64
	Counts []uint64

	// Bounds are the histogram's own boundaries ([Instrument.Buckets]),
	// carried with the counts because a count means nothing without the
	// boundary it was counted against.
	Bounds []float64
}

// Quantile estimates the qth quantile of a histogram, in the instrument's own
// unit.
//
// AN ESTIMATE, and the doc says so where a caller reads it: the answer is the
// UPPER boundary of the bucket the qth observation falls in, so it is accurate
// to within one bucket — a factor of two on every set this catalogue declares
// — and never understates. A p95 that reads 4 ms means "at most 4 ms", which is
// the direction a budget check wants to be wrong in.
func (s Snapshot) Quantile(q float64) float64 {
	if s.Count == 0 || q <= 0 || q > 1 {
		return 0
	}
	want := uint64(math.Ceil(q * float64(s.Count)))
	var seen uint64
	for i, c := range s.Counts {
		seen += c
		if seen >= want {
			if i >= len(s.Bounds) {
				return math.Inf(1)
			}
			return s.Bounds[i]
		}
	}
	return math.Inf(1)
}

// ReadWindow returns every series as the ROLLING WINDOW holds it, in the same
// shape and order as [Recorder.Read].
//
// The two differ in the period they cover, and that is the whole point:
// `Total`, `Count`, `Sum` and the bins here cover the window — see [Buckets] —
// where Read's cover every hour since this process started. A gauge's `Value`
// differs with it, as the highest reading in the window rather than the
// current one. An alarm reads this one; an exporter, which diffs successive
// scrapes itself, reads the other.
func (r *Recorder) ReadWindow() []Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Snapshot, 0, len(r.series))
	for key, s := range r.series {
		snap := Snapshot{
			Name: s.inst.Name, Kind: s.inst.Kind, Unit: s.inst.Unit,
			Attrs: cloneAttrs(s.attrs),
			Total: r.window.Total(key),
			Count: r.window.Count(key),
		}
		switch s.inst.Kind {
		case KindGauge:
			// A GAUGE'S WINDOW IS ITS PEAK. A gauge has no total to sum
			// and its current value is not a fact about the window, so
			// the honest windowed reading is the highest it reached.
			snap.Value = r.window.Peak(key)
		case KindHistogram:
			// THE BINS, so [Snapshot.Quantile] computes a WINDOWED p95
			// with no second implementation. A max substituted here
			// would fire every p95 alarm on the single worst
			// observation in the window.
			snap.Counts, snap.Count, snap.Sum = r.window.Bins(key, s.inst.bounds())
			snap.Bounds = s.inst.Buckets()
		}
		out = append(out, snap)
	}
	sortSnapshots(out)
	return out
}

// Read returns every series, in name and attribute order so two captures are
// diffable.
func (r *Recorder) Read() []Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Snapshot, 0, len(r.series))
	for _, s := range r.series {
		snap := Snapshot{
			Name: s.inst.Name, Kind: s.inst.Kind, Unit: s.inst.Unit,
			Attrs: cloneAttrs(s.attrs),
			Total: s.total, Value: s.value,
			Count: s.n, Sum: s.sum, Counts: append([]uint64(nil), s.counts...),
		}
		if s.inst.Kind == KindHistogram {
			snap.Bounds = s.inst.Buckets()
		}
		out = append(out, snap)
	}
	sortSnapshots(out)
	return out
}

// sortSnapshots puts a reading in name and attribute order, so two captures
// are diffable and the two readers cannot disagree about the order.
func sortSnapshots(out []Snapshot) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return attrString(out[i].Attrs) < attrString(out[j].Attrs)
	})
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
