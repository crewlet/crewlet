package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// seedEvent writes one bare log row at an instant.
func seedEvent(t *testing.T, log *store.EventLog, id string, at time.Time, category, actor string) {
	t.Helper()
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: "thing_happened", Time: at,
		Category: category, Actor: actor, Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// THE AXIS IS THE ENGINE'S, and every bucket is present.
//
// The client holds at most a page of rows and the store's window it never
// holds, so an axis folded there would be right for one window and absent for
// every other. And a quiet hour has to be a gap of full width rather than a bar
// the chart squeezed out, which is only possible if the empty buckets come
// back.
func TestAHistogramReturnsEveryBucketInTheWindow(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-5 * time.Hour)
	seedEvent(t, log, "a", base.Add(10*time.Minute), "system", "node-0")
	seedEvent(t, log, "b", base.Add(20*time.Minute), "system", "node-0")
	seedEvent(t, log, "c", base.Add(3*time.Hour), "task", "PM")

	got, err := log.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{Since: base, Until: base.Add(5 * time.Hour)},
		Bucket:    store.BucketHour,
	})
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	if len(got.Bars) != 5 {
		t.Fatalf("bars = %d, want the whole five-hour window", len(got.Bars))
	}
	want := []int{2, 0, 0, 1, 0}
	for i, bar := range got.Bars {
		if bar.Count != want[i] {
			t.Errorf("bar %d (%s) = %d, want %d", i, bar.At, bar.Count, want[i])
		}
	}
	// THE TOTAL IS STATED rather than left as a mental sum of forty bars.
	if got.Total != 3 {
		t.Errorf("total = %d, want 3", got.Total)
	}
	if got.Bucket != store.BucketHour {
		t.Errorf("bucket = %q, want the one asked for", got.Bucket)
	}
}

// THE BARS AND THE ROWS ANSWER ABOUT ONE SET.
//
// A bar counting rows the list below it would not show is worse than no bar at
// all, which is why both compile their filters through one function. Every
// filter the list has, the axis has.
func TestAHistogramFiltersExactlyAsTheListingDoes(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	seedEvent(t, log, "sys1", base.Add(time.Minute), "system", "node-0")
	seedEvent(t, log, "sys2", base.Add(2*time.Minute), "system", "node-0")
	seedEvent(t, log, "task1", base.Add(3*time.Minute), "task", "PM")

	for _, c := range []struct {
		name string
		q    store.ListQuery
		want int
	}{
		{"everything", store.ListQuery{}, 3},
		{"one category", store.ListQuery{Category: "system"}, 2},
		{"one actor", store.ListQuery{Actor: "PM"}, 1},
		{"a category with nothing in it", store.ListQuery{Category: "webhook"}, 0},
		{"a type", store.ListQuery{Type: "thing_happened"}, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := c.q
			q.Since, q.Until = base, base.Add(2*time.Hour)
			rows, err := log.List(t.Context(), q)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			bars, err := log.Histogram(t.Context(), store.HistogramQuery{
				ListQuery: q, Bucket: store.BucketHour,
			})
			if err != nil {
				t.Fatalf("histogram: %v", err)
			}
			if bars.Total != c.want || len(rows) != c.want {
				t.Errorf("listing has %d rows and the axis counts %d, want %d each",
					len(rows), bars.Total, c.want)
			}
		})
	}
}

// A CURSOR IS NOT A FILTER, so scrolling must not move the bars.
//
// The cursor is where a page resumes and moves with every page, while the
// filters are what the reader asked for and do not. An axis that redrew itself
// as somebody scrolled would be answering a different question every time.
func TestAHistogramIgnoresThePagingCursor(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	for i, id := range []string{"e1", "e2", "e3", "e4"} {
		seedEvent(t, log, id, base.Add(time.Duration(i)*time.Minute), "system", "node-0")
	}
	ask := func(before *store.Cursor) int {
		got, err := log.Histogram(t.Context(), store.HistogramQuery{
			ListQuery: store.ListQuery{
				Since: base, Until: base.Add(time.Hour), Before: before, Limit: 2,
			},
			Bucket: store.BucketHour,
		})
		if err != nil {
			t.Fatalf("histogram: %v", err)
		}
		return got.Total
	}
	if first, paged := ask(nil), ask(&store.Cursor{Time: base.Add(2 * time.Minute), ID: "e3"}); first != paged {
		t.Errorf("the axis counted %d on the first page and %d on the second", first, paged)
	}
}

// THE EDGES SNAP OUTWARD, so the first and last bars are whole ones.
//
// A partial bar at each end has a height that means something different from
// its neighbours', and a reader comparing them has no way to know.
func TestAHistogramsEdgesAreWholeBuckets(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	seedEvent(t, log, "edge", base.Add(5*time.Minute), "system", "node-0")

	got, err := log.Histogram(t.Context(), store.HistogramQuery{
		// Asked for from ten past to fifty past: inside one hour, off the
		// boundary at both ends.
		ListQuery: store.ListQuery{
			Since: base.Add(10 * time.Minute), Until: base.Add(50 * time.Minute),
		},
		Bucket: store.BucketHour,
	})
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	since, err := time.Parse(time.RFC3339, got.Since)
	if err != nil {
		t.Fatalf("since %q: %v", got.Since, err)
	}
	until, err := time.Parse(time.RFC3339, got.Until)
	if err != nil {
		t.Fatalf("until %q: %v", got.Until, err)
	}
	if !since.Equal(base) || !until.Equal(base.Add(time.Hour)) {
		t.Errorf("window = %s .. %s, want the whole hour %s .. %s",
			since, until, base, base.Add(time.Hour))
	}
	// AND THE ROW AT FIVE PAST IS IN IT, although the caller asked from ten
	// past: a widened window that did not count what it now covers would
	// draw a bar shorter than the hour it is labelled with.
	if len(got.Bars) != 1 || got.Bars[0].Count != 1 {
		t.Errorf("bars = %+v, want one whole hour holding the row", got.Bars)
	}

	// THE TOP EDGE IS ITS OWN CLAUSE, and this is the case that says so: a
	// window ending mid-bucket SEVERAL buckets along truncates to a
	// different instant from the bottom edge, so the "they collapsed to
	// one" guard does not cover it. Clipped rather than widened, the last
	// bucket is simply absent and every row in it vanishes from an axis
	// whose own heading claims to reach them.
	seedEvent(t, log, "late", base.Add(time.Hour+30*time.Minute), "system", "node-0")
	wide, err := log.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{
			Since: base.Add(10 * time.Minute), Until: base.Add(time.Hour + 50*time.Minute),
		},
		Bucket: store.BucketHour,
	})
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	if len(wide.Bars) != 2 || wide.Total != 2 {
		t.Errorf("bars = %+v (total %d), want both whole hours and both rows",
			wide.Bars, wide.Total)
	}
}

// TOO MANY BUCKETS IS A REFUSAL, never a truncation or a coarser bucket.
//
// Dropping the oldest bars silently would put a month's heading over a day of
// them, and coarsening would answer a different question from the one the axis
// is labelled with.
func TestAHistogramRefusesAWindowItCannotDraw(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC()
	_, err := log.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{Since: now.Add(-20 * 24 * time.Hour), Until: now},
		Bucket:    store.BucketMinute,
	})
	if !errors.Is(err, store.ErrHistogramSpan) {
		t.Fatalf("err = %v, want ErrHistogramSpan", err)
	}
}

// AN UNKNOWN BUCKET IS REFUSED NAMING WHAT IS ACCEPTED, never defaulted.
func TestAHistogramRefusesABucketItDoesNotKnow(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	for _, bad := range []store.EventBucket{"", "week", "5m", "MINUTE"} {
		_, err := log.Histogram(t.Context(), store.HistogramQuery{Bucket: bad})
		if !errors.Is(err, store.ErrHistogramBucket) {
			t.Errorf("bucket %q: err = %v, want ErrHistogramBucket", bad, err)
		}
	}
	for _, good := range store.EventBuckets {
		if !good.Valid() {
			t.Errorf("%q is in the closed set and reports itself invalid", good)
		}
	}
}

// THE RELATED-AGENT FILTER IS REFUSED RATHER THAN APPROXIMATED.
//
// It over-fetches and post-filters, pulling in every event sharing a trace with
// a direct match, so a count over the predicate alone is a smaller set than the
// list beside it shows. A bar that disagrees with its own rows is the one thing
// this axis exists not to be.
func TestAHistogramRefusesTheFilterItCannotMatch(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	_, err := log.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{RelatedAgent: "PM"},
		Bucket:    store.BucketHour,
	})
	if !errors.Is(err, store.ErrHistogramRelated) {
		t.Fatalf("err = %v, want ErrHistogramRelated", err)
	}
}

// A FACET COUNT SAYS HOW MANY ROWS CHOOSING IT WOULD SHOW.
//
// So it cannot be counted through the filter it is offering to set: counted
// with it, every chip but the selected one reads zero, which is not a fact
// about anything. Every OTHER filter still applies, because a chip has to
// answer "how many, given what is already narrowed".
func TestFacetCountsLiftTheirOwnFilterAndKeepTheRest(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	seedEvent(t, log, "s1", base.Add(time.Minute), "system", "node-0")
	seedEvent(t, log, "s2", base.Add(2*time.Minute), "system", "node-0")
	seedEvent(t, log, "t1", base.Add(3*time.Minute), "task", "node-0")
	seedEvent(t, log, "t2", base.Add(4*time.Minute), "task", "PM")

	ask := func(q store.ListQuery) map[string]int {
		q.Since, q.Until = base, base.Add(time.Hour)
		got, err := log.Histogram(t.Context(), store.HistogramQuery{
			ListQuery: q, Bucket: store.BucketHour,
		})
		if err != nil {
			t.Fatalf("histogram: %v", err)
		}
		return got.ByCategory
	}

	// UNFILTERED: both categories, with their real counts.
	if got := ask(store.ListQuery{}); got["system"] != 2 || got["task"] != 2 {
		t.Errorf("by_category = %v, want system 2 and task 2", got)
	}
	// WITH A CATEGORY CHOSEN: still both, so the reader can see what
	// switching would give. Counted through its own filter, `task` would
	// read 0 here and the chip would be a dead end.
	if got := ask(store.ListQuery{Category: "system"}); got["system"] != 2 || got["task"] != 2 {
		t.Errorf("by_category with a category chosen = %v, want both still counted", got)
	}
	// WITH ANOTHER FILTER: narrowed by it. A chip has to answer "how many,
	// given what is already narrowed", not "how many in the whole window".
	got := ask(store.ListQuery{Actor: "PM"})
	if got["task"] != 1 {
		t.Errorf("by_category for one actor = %v, want task 1", got)
	}
	if _, present := got["system"]; present {
		t.Errorf("by_category for one actor = %v, want no system key at all", got)
	}
}

// A FACET COUNTS WHAT THE LISTING WILL SHOW, not what the bars cover.
//
// The bars snap their edges outward to whole buckets; the listing takes the
// caller's own. Counted over the snapped window a chip would include up to two
// buckets the list never shows — thousands of rows on a busy hour — and the
// chip's whole job is to say how many rows choosing it would give.
func TestFacetCountsCoverTheWindowAskedFor(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	// One row inside the window the caller names, and one in the part of the
	// hour the SNAP pulls in but the caller did not ask for.
	seedEvent(t, log, "asked", hour.Add(20*time.Minute), "system", "node-0")
	seedEvent(t, log, "snapped-in", hour.Add(5*time.Minute), "system", "node-0")

	asked := store.ListQuery{Since: hour.Add(10 * time.Minute), Until: hour.Add(30 * time.Minute)}
	got, err := log.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: asked, Bucket: store.BucketHour,
	})
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	// The BAR covers the whole hour and therefore counts both.
	if got.Total != 2 {
		t.Errorf("total = %d, want both rows — the bar is a whole hour", got.Total)
	}
	// The CHIP counts only what choosing it would show.
	if got.ByCategory["system"] != 1 {
		t.Errorf("by_category = %v, want 1 — the row outside the asked window is "+
			"on the bar but not in the list", got.ByCategory)
	}
	rows, err := log.List(t.Context(), asked)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != got.ByCategory["system"] {
		t.Errorf("the listing has %d rows and the chip claims %d", len(rows), got.ByCategory["system"])
	}
}
