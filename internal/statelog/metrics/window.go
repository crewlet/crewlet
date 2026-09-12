package metrics

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Buckets is how many hourly buckets a window keeps.
//
// TWENTY-FOUR PLUS THE CURRENT PARTIAL HOUR. A ring of whole hours makes the
// window ROLLING rather than calendar: an operator reading at 09:00 sees the
// last twenty-four hours, not "since midnight", and the two differ most
// exactly when somebody is looking because something happened.
const Buckets = 24

// Window is a rolling 24-hour view over the recorder's counters and maxima.
//
// # Why this exists at all
//
// The estate it replaces said `_24h` forty-odd times and never said what a
// `_24h` was: since when, reset how, per node or per fleet. A maximum was
// "observed", with no window at all, so one sixteen-second transaction last
// Tuesday masked every transaction after it.
//
// # A young window says so
//
// A node up for ten minutes reporting zero refusals is not a quiet node, and
// the difference matters to whoever is deciding whether a deploy went well.
// [Window.Since] is how long this window has actually been collecting, and
// [Window.Partial] is the flag a renderer shows beside a number younger than
// its own period.
type Window struct {
	mu      sync.Mutex
	started time.Time
	now     func() time.Time

	// ring is bucket index -> series key -> value. The index is the hour
	// number, so a bucket is expired by comparing its hour rather than by
	// sweeping: nothing runs on a timer to keep this correct.
	ring  [Buckets + 1]map[string]bucketValue
	hours [Buckets + 1]int64
}

// bucketValue is one series' contribution to one hour.
//
// IT CARRIES THE HISTOGRAM BINS, not just a maximum, and that is what makes a
// windowed p95 possible at all. The alarms this window feeds compare a p95
// against a budget — `barrier_slow` and `search_slow` both — and a maximum
// substituted for a p95 is not a stale threshold but a different one: a
// maximum is above the p95 by construction, so every such alarm would fire on
// the single worst observation in the window rather than on a distribution
// that had actually moved.
//
// The bins are the recorder's own, so a windowed quantile and a cumulative one
// are computed the same way and can be compared.
type bucketValue struct {
	total  uint64
	max    float64
	counts []uint64
	n      uint64
	sum    float64
}

// NewWindow builds an empty window.
func NewWindow(now func() time.Time) *Window {
	if now == nil {
		now = time.Now
	}
	w := &Window{started: now().UTC(), now: now}
	for i := range w.hours {
		w.hours[i] = -1
	}
	return w
}

// Add contributes to a counter in the current hour.
func (w *Window) Add(key string, n uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := w.bucket()
	v := b[key]
	v.total += n
	b[key] = v
}

// Max contributes to a maximum in the current hour.
func (w *Window) Max(key string, v float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := w.bucket()
	cur := b[key]
	if v > cur.max {
		cur.max = v
	}
	b[key] = cur
}

// Observe contributes one measurement to a distribution in the current hour.
//
// Separate from [Window.Max] because a maximum and a distribution answer
// different questions and the alarms need the second: "the worst barrier in a
// day" is a fact about one request, and "the p95 barrier over a day" is a fact
// about the service.
func (w *Window) Observe(key string, v float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := w.bucket()
	cur := b[key]
	if cur.counts == nil {
		cur.counts = make([]uint64, len(bins)+1)
	}
	cur.counts[binFor(v)]++
	cur.n++
	cur.sum += v
	if v > cur.max {
		cur.max = v
	}
	b[key] = cur
}

// Quantile is the qth quantile of one series over the window.
//
// The same upper-boundary reading as [Snapshot.Quantile], for the same reason:
// accurate to within one bucket and never understating, which is the direction
// a budget check wants to be wrong in.
func (w *Window) Quantile(key string, q float64) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	var total uint64
	merged := make([]uint64, len(bins)+1)
	for _, b := range w.live() {
		v := b[key]
		total += v.n
		for i, c := range v.counts {
			merged[i] += c
		}
	}
	if total == 0 || q <= 0 || q > 1 {
		return 0
	}
	want := uint64(math.Ceil(q * float64(total)))
	var seen uint64
	for i, c := range merged {
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

// Bins is one distribution's merged histogram over the window: its bin counts,
// how many observations they hold and their sum.
//
// RETURNED TOGETHER because a [Snapshot] carries all three and a caller that
// took them in three calls could take them across an hour boundary, pairing
// counts from one window with a total from another.
func (w *Window) Bins(key string) (counts []uint64, n uint64, sum float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	counts = make([]uint64, len(bins)+1)
	for _, b := range w.live() {
		v := b[key]
		n += v.n
		sum += v.sum
		for i, c := range v.counts {
			counts[i] += c
		}
	}
	return counts, n, sum
}

// Count is how many observations a distribution holds over the window.
//
// ZERO IS AN ANSWER, and a different one from a quantile of zero: a series
// nothing has observed has no p95, and an alarm that read a missing
// distribution as "0 ms, well inside budget" would go quiet exactly when a
// subsystem stopped running.
func (w *Window) Count(key string) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out uint64
	for _, b := range w.live() {
		out += b[key].n
	}
	return out
}

// bucket returns the current hour's map, resetting it when the ring has
// wrapped onto a new hour. Callers hold the lock.
func (w *Window) bucket() map[string]bucketValue {
	hour := w.now().UTC().Unix() / 3600
	idx := int(hour % int64(len(w.hours)))
	if w.hours[idx] != hour {
		// A NEW HOUR REUSES THE SLOT, which is what makes this a ring
		// rather than a growing map — and what makes expiry free: the
		// bucket 25 hours ago is the one this hour overwrites.
		w.hours[idx] = hour
		w.ring[idx] = map[string]bucketValue{}
	}
	return w.ring[idx]
}

// Total sums a counter over the window.
func (w *Window) Total(key string) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out uint64
	for _, b := range w.live() {
		out += b[key].total
	}
	return out
}

// Peak is the largest value seen over the window.
func (w *Window) Peak(key string) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out float64
	for _, b := range w.live() {
		if v := b[key].max; v > out {
			out = v
		}
	}
	return out
}

// Keys lists every series the window holds, in order.
func (w *Window) Keys() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := map[string]bool{}
	for _, b := range w.live() {
		for k := range b {
			seen[k] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// live returns the buckets inside the window. Callers hold the lock.
//
// The bound is the hour number rather than a sweep: a bucket whose hour is
// more than [Buckets] behind the current one is stale ring memory that has not
// been reused yet, and counting it would make the window longer than it says.
func (w *Window) live() []map[string]bucketValue {
	hour := w.now().UTC().Unix() / 3600
	out := make([]map[string]bucketValue, 0, len(w.hours))
	for i, h := range w.hours {
		if h < 0 || hour-h > int64(Buckets) || w.ring[i] == nil {
			continue
		}
		out = append(out, w.ring[i])
	}
	return out
}

// Since is how long this window has been collecting.
func (w *Window) Since() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now().UTC().Sub(w.started)
}

// Partial reports whether the window is younger than the period it names.
//
// A renderer that shows a `_24h` number without this is telling an operator
// that a node up for ten minutes has had no refusals in a day.
func (w *Window) Partial() bool { return w.Since() < Buckets*time.Hour }
