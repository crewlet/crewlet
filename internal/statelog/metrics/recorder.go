package metrics

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Recorder is the one place a measurement is written: counters, gauges and
// fixed-bin histograms keyed by instrument name and attribute set, safe for
// concurrent use. It is not the OTel API directly because the OTel SDK cannot
// read back what was recorded, and the rolling window is a second reader
// (package doc, "One recorder, two readers").
type Recorder struct {
	mu     sync.Mutex
	series map[string]*series
	byName map[string]Instrument

	// sinks are the exporter's synchronous histogram instruments, bound
	// after the provider exists. Only a histogram needs one: a counter or
	// gauge has a current value the exporter reads at export time, while a
	// distribution's observations must reach it as they happen. Empty with
	// no collector; the recorder's own copy still answers.
	sinks map[string]func(float64, map[string]string)

	// window is the rolling 24-hour view the alarms read, fed from the same
	// write path as `series`, which is cumulative since start for an
	// exporter to diff. An alarm on the cumulative series latches: one
	// degraded search lights `search_degraded` for the life of the process.
	window *Window
}

// series is one instrument at one attribute set.
type series struct {
	inst   Instrument
	attrs  map[string]string
	bounds []float64 // a histogram's boundaries, its unit's ([Bounds])
	counts []uint64  // histogram bins, one past the last boundary
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
		window: NewWindow(nil),
	}, nil
}

// Window is the rolling 24-hour view of everything recorded here. Every
// `_24h` alarm reads it, never the cumulative series: a threshold on a counter
// that only grows latches until the process restarts.
func (r *Recorder) Window() *Window {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.window
}

// BindHistogram attaches an exporter's instrument to a histogram, once per
// histogram, by whoever installs the MeterProvider. A nil sink is ignored: an
// instrument the exporter could not build records into the recorder's own copy
// alone.
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
	// The window is the only clock reader here, so the clock goes into it
	// rather than onto a recorder field that nothing else would read.
	r.window = NewWindow(now)
	return r
}

// Attrs is one instrument's dimensions for a single measurement. A map rather
// than a slice so a caller cannot silently swap two string values.
type Attrs map[string]string

// Observe records one latency into a histogram. It takes a duration so no
// caller converts to milliseconds itself and gets seconds by mistake.
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

// AddAt increments a counter by a whole number of events that happened at at,
// where the rolling window files them ([Window.AddAt]); the cumulative series
// counts them when recorded, since an exporter diffs its own scrapes.
func (r *Recorder) AddAt(name string, n uint64, at time.Time, attrs Attrs) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, known := r.byName[name]
	if !known || inst.Kind != KindCounter {
		return
	}
	s := r.seriesFor(inst, attrs)
	s.total += n
	r.window.AddAt(seriesKey(inst.Name, inst.Attributes, attrs), n, at)
}

// Set writes a gauge.
func (r *Recorder) Set(name string, v float64, attrs Attrs) {
	r.record(name, KindGauge, v, attrs)
}

// Unset withdraws a gauge's series from both readers, for a measurement that
// became unknown, which is neither zero nor its last write. A gauge would
// otherwise keep exporting its last value while every other surface shows it
// unmeasured; a vanished series reads as "no data" on a collector's panel.
//
// Only a gauge can be unset: a counter or histogram is a history, and
// withdrawing it would rewrite what happened. An unknown name, a non-gauge and
// a series never written are no-ops.
func (r *Recorder) Unset(name string, attrs Attrs) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, known := r.byName[name]
	if !known || inst.Kind != KindGauge {
		return
	}
	key := seriesKey(inst.Name, inst.Attributes, attrs)
	delete(r.series, key)
	r.window.Forget(key)
}

// record is the one write path. An unknown name is dropped, not registered,
// so a typo never becomes an undocumented series. A nil Recorder (a node run
// without metrics) records nothing, so no write site needs a guard.
func (r *Recorder) record(name string, kind Kind, v float64, attrs Attrs) {
	if r == nil {
		return
	}
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
		s.total += uint64(v)
		r.window.Add(key, uint64(v))
	case KindGauge:
		s.value = v
		r.window.Max(key, v)
	case KindHistogram:
		r.window.Observe(key, s.bounds, v)
		s.counts[binFor(s.bounds, v)]++
		s.sum += v
		s.n++
		if sink := r.sinks[name]; sink != nil {
			// Under the lock, so the exporter sees observations in the
			// recorder's order; the SDK's Record is lock-free and cheap.
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
		// Only declared attributes, so the catalogue bounds cardinality
		// and a stray task key cannot open a series per task.
		if v, ok := attrs[a]; ok {
			kept[a] = v
		}
	}
	s := &series{inst: inst, attrs: kept}
	if inst.Kind == KindHistogram {
		s.bounds = boundsOf(inst.Unit)
		s.counts = make([]uint64, len(s.bounds)+1)
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

// Snapshot is one series as a reader sees it.
type Snapshot struct {
	Name  string
	Kind  Kind
	Unit  string
	Attrs map[string]string

	// Total is a counter's sum; Value is a gauge's current reading.
	Total uint64
	Value float64

	// Count, Sum, Counts and Bounds describe a histogram: Counts has one
	// bin per boundary of its unit ([Bounds]) plus one for the overflow.
	Count  uint64
	Sum    float64
	Counts []uint64
	Bounds []float64
}

// Quantile estimates the qth quantile of a histogram, in the instrument's own
// unit: the UPPER boundary of the bucket the qth observation falls in, so it is
// within a factor of two and a half and never understates (5 ms means "over
// 2 ms and at most 5 ms"). A budget check is exact where the budget is a
// boundary, which is why every alarm's percentile budget is one ([Bounds]).
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

// ReadWindow returns every series as the rolling window holds it, in the same
// shape and order as [Recorder.Read], but with `Total`, `Count`, `Sum` and the
// bins covering the last [Buckets] hours rather than since start. An alarm
// reads this; an exporter, which diffs its own scrapes, reads Read.
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
			// A gauge's windowed reading is its peak: its current value
			// is not a fact about the window.
			snap.Value = r.window.Peak(key)
		case KindHistogram:
			// The bins, not a max, so [Snapshot.Quantile] gives a windowed
			// p95; a max would fire every p95 alarm on the worst outlier.
			snap.Bounds = s.bounds
			snap.Counts, snap.Count, snap.Sum = r.window.Bins(key, s.bounds)
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
		out = append(out, Snapshot{
			Name: s.inst.Name, Kind: s.inst.Kind, Unit: s.inst.Unit,
			Attrs: cloneAttrs(s.attrs),
			Total: s.total, Value: s.value,
			Count: s.n, Sum: s.sum, Counts: append([]uint64(nil), s.counts...),
			Bounds: s.bounds,
		})
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
