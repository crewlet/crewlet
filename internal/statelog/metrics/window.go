package metrics

import (
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
type bucketValue struct {
	total uint64
	max   float64
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
