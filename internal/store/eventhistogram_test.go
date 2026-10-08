package store_test

import (
	"errors"
	"slices"
	"strings"
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

// AN UNBOUNDED TOP EDGE IS "UP TO NOW", AND NOW IS INSIDE A BUCKET.
//
// `/events/series` with no `until` is the commonest ask there is — "what just
// happened" — and the outward snap has to widen that edge like any other. It
// did not: the snap compared its truncated result to the query's own `Until`,
// which is the ZERO time for exactly this caller, so the top edge rounded DOWN
// to the start of the bucket in progress. Every event of the current minute,
// hour or day fell in no bar at all, `Total` left them out, and the newest bar
// an axis labelled "up to now" drew was the one before this one.
func TestAHistogramWithNoTopEdgeDrawsTheBucketInProgress(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC()
	seedEvent(t, log, "in-progress", at, "system", "node-0")

	got, err := log.Histogram(t.Context(), store.HistogramQuery{
		// NO Until. The bottom edge is named so the window is a couple
		// of bars rather than the whole retention floor.
		ListQuery: store.ListQuery{Since: at.Truncate(time.Hour).Add(-time.Hour)},
		Bucket:    store.BucketHour,
	})
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	until, err := time.Parse(time.RFC3339, got.Until)
	if err != nil {
		t.Fatalf("until %q: %v", got.Until, err)
	}
	// PAST the instant that was published, because the bucket holding it
	// is whole. Truncated down, this edge is the row's own bucket start.
	if !until.After(at) {
		t.Errorf("until = %s, want an edge past %s — the bucket in progress is "+
			"a whole bar like every other", until, at)
	}
	if got.Total != 1 {
		t.Errorf("total = %d, want the row published in the bucket in progress",
			got.Total)
	}
	// AND IN THE RIGHT BAR. Found by the instant rather than by position,
	// so an hour that rolls over between the append and the read moves
	// which bar is last without moving which bar holds the row.
	held := false
	for _, bar := range got.Bars {
		start, err := time.Parse(time.RFC3339, bar.At)
		if err != nil {
			t.Fatalf("bar at %q: %v", bar.At, err)
		}
		if !start.After(at) && at.Before(start.Add(time.Hour)) {
			held = true
			if bar.Count != 1 {
				t.Errorf("the bar at %s counts %d, want the row in it",
					bar.At, bar.Count)
			}
		}
	}
	if !held {
		t.Errorf("bars = %+v cover no bucket holding %s", got.Bars, at)
	}
}

// EVERY COUNT ON THE AXIS IS FLOORED AT THE INSTANT IT IS CUT AGAINST.
//
// [store.ListQuery.At] is where the window is cut and where the history
// floor sits — and a fleet pins it on the asker, so by the time any other node
// reads it is in that node's past. The bars, the totals and the facet counts
// were floored at a SECOND read of the log's own clock instead, so a row
// between At's floor and the reader's sat inside the first bar the answer
// reported and was in none of its counts. The row here is that row: above
// At − EventHistory, below now − EventHistory, and still on disk because
// retention keeps a day past the floor.
//
// Mutation: floor [store.ListQuery]'s predicate at `now()` rather than the
// instant it is handed, and the first bar, Total, Failed and the task chip all
// lose the row; hand the facet count a fresh `now()` and the chip alone does;
// drop the floor altogether and the system chip gains the row beneath it.
func TestEveryCountOnTheAxisIsFlooredAtTheInstantItIsCutAgainst(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	// AN HOUR BOUNDARY one to two hours back, so the floor under it is an
	// hour boundary too and the first bar begins exactly there.
	at := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	floor := at.Add(-store.EventHistory)
	for _, r := range []store.EventRecord{
		// Above At's floor and below the clock's: the row this is about.
		{ID: "above-the-floor", Category: "task", Time: floor.Add(time.Minute),
			Tags: map[string]string{"failed": "true"}},
		// Below At's floor: in no count — the chips' included, which
		// take the caller's own edges and so have only the floor to stop
		// them.
		{ID: "under-the-floor", Category: "system", Time: floor.Add(-time.Minute)},
		// In the window's last bar, which every reading of the clock
		// agrees about.
		{ID: "recent", Category: "system", Time: at.Add(-30 * time.Minute)},
	} {
		r.Type, r.Payload = "thing_happened", []byte(`{}`)
		if err := log.Append(t.Context(), r); err != nil {
			t.Fatalf("append %s: %v", r.ID, err)
		}
	}

	got, err := log.Histogram(t.Context(), store.HistogramQuery{Bucket: store.BucketHour, At: at})
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	if got.Since != floor.Format(time.RFC3339) || got.Until != at.Format(time.RFC3339) {
		t.Fatalf("window = %s .. %s, want %s .. %s — the floor under At to At itself",
			got.Since, got.Until, floor.Format(time.RFC3339), at.Format(time.RFC3339))
	}
	if first := got.Bars[0]; first.Count != 1 || first.Failed != 1 {
		t.Errorf("the first bar (%s) is %d with %d failed, want the row above the floor, "+
			"failed — it is inside the bar the answer reports", first.At, first.Count, first.Failed)
	}
	if got.Total != 2 || got.Failed != 1 {
		t.Errorf("total %d with %d failed, want 2 with 1", got.Total, got.Failed)
	}
	want := map[string]int{"task": 1, "system": 1}
	if len(got.ByCategory) != len(want) || got.ByCategory["task"] != 1 || got.ByCategory["system"] != 1 {
		t.Errorf("by_category = %v, want %v — floored where the bars are", got.ByCategory, want)
	}
}

// A WINDOW THE FLOOR CLIPS IS CUT AS EVERY BUILD CUTS IT, AND SHOWN FROM ITS
// FIRST WHOLE BAR.
//
// Every count is floored at the instant minus the thirty-day history, and the
// default window — any ask that names no `since` — starts at that floor, which
// lies mid-bucket whenever the instant does. Here the instant is half past an
// hour, so the floor is half past too, and the bucket it falls in holds a row
// on each side of it.
//
// TWO SHAPES OF ONE AXIS, and each is held. One node's part is cut DOWN to the
// bucket the floor falls in, that bar counting only the row above the floor:
// it is the window every node cuts from the asker's instant, and a fleet sums
// its nodes' parts bar for bar. What a caller is shown drops that partial bar
// and begins at the first whole bucket inside the history, since a bar
// labelled with the whole bucket and counting only its upper part is what the
// outward snap exists to prevent.
//
// Mutation: start [store.HistogramQuery.Window] at the first whole bucket and
// the part's `since` is a bar later than the window it was asked for; drop
// nothing in [store.EventHistogram.InsideHistory] and the axis shown begins
// with the partial bar; leave its Total alone and it counts the row the
// dropped bar held.
func TestAWindowTheFloorClipsIsCutAsEveryNodeCutsItAndShownFromItsFirstWholeBar(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Truncate(time.Hour).Add(-90 * time.Minute)
	floor := at.Add(-store.EventHistory)
	cut := floor.Truncate(time.Hour) // the bucket the floor falls in
	first := cut.Add(time.Hour)      // the first whole one inside the history
	for _, r := range []store.EventRecord{
		{ID: "under-the-floor", Category: "system", Time: floor.Add(-10 * time.Minute)},
		{ID: "over-the-floor", Category: "task", Time: floor.Add(10 * time.Minute),
			Tags: map[string]string{"failed": "true"}},
		{ID: "first-bar", Category: "task", Time: first.Add(10 * time.Minute)},
		{ID: "recent", Category: "system", Time: at.Add(-10 * time.Minute)},
	} {
		r.Type, r.Payload = "thing_happened", []byte(`{}`)
		if err := log.Append(t.Context(), r); err != nil {
			t.Fatalf("append %s: %v", r.ID, err)
		}
	}
	if got := store.BucketHour.HistoryStart(at); !got.Equal(first) {
		t.Fatalf("the history starts at %s, want the first whole hour inside it, %s", got, first)
	}

	part, err := log.Histogram(t.Context(), store.HistogramQuery{Bucket: store.BucketHour,
		ListQuery: store.ListQuery{At: at}})
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	until := at.Truncate(time.Hour).Add(time.Hour)
	// THE NODE'S PART: from the hour the floor cuts, as every node cuts it.
	if part.Since != cut.Format(time.RFC3339) || part.Until != until.Format(time.RFC3339) {
		t.Fatalf("the part's window = %s .. %s, want %s .. %s — down to the hour the "+
			"floor falls in, the window every node cuts", part.Since, part.Until,
			cut.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	if b := part.Bars[0]; b.Count != 1 || b.Failed != 1 || part.Total != 3 || part.Failed != 1 {
		t.Errorf("the part's first bar counts %d (%d failed) of total %d (%d failed), want "+
			"the one row above the floor, failed, of three", b.Count, b.Failed, part.Total, part.Failed)
	}

	// WHAT A CALLER IS SHOWN: from the first whole hour inside the history.
	got := part.InsideHistory(at)
	if got.Since != first.Format(time.RFC3339) || got.Until != until.Format(time.RFC3339) {
		t.Fatalf("window = %s .. %s, want %s .. %s — from the first whole hour inside the "+
			"history to the end of the one in progress", got.Since, got.Until,
			first.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	if want := int(until.Sub(first) / time.Hour); len(got.Bars) != want {
		t.Fatalf("%d bars, want %d", len(got.Bars), want)
	}
	// EVERY ROW INSIDE A REPORTED BAR IS COUNTED IN IT, which a bar starting
	// below the floor cannot do.
	if b := got.Bars[0]; b.At != first.Format(time.RFC3339) || b.Count != 1 {
		t.Errorf("the first bar is %s counting %d, want %s counting the one row in it",
			b.At, b.Count, first.Format(time.RFC3339))
	}
	if got.Total != 2 || got.Failed != 0 {
		t.Errorf("total = %d (%d failed), want the two rows inside the bars, none failed",
			got.Total, got.Failed)
	}
	if len(part.Bars) != len(got.Bars)+1 || part.Total != 3 {
		t.Errorf("the part was written through: %d bars, total %d", len(part.Bars), part.Total)
	}
	// THE CHIPS take the caller's own edges — the floor, here — so they hold
	// the row above the floor in the bucket no bar draws, and need not sum to
	// the bars' total, as the API reference says.
	if got.ByCategory["task"] != 2 || got.ByCategory["system"] != 1 {
		t.Errorf("by_category = %v, want task 2 and system 1", got.ByCategory)
	}

	// A WINDOW WHOLLY BELOW THE FIRST WHOLE BAR IS SHOWN EMPTY, rather than
	// as a bar it cannot fill or the next one, which the caller did not ask
	// for — whether it ends inside the floor's bucket or under the floor.
	for name, top := range map[string]time.Time{
		"inside the floor's bucket": floor.Add(20 * time.Minute),
		"under the floor":           floor.Add(-2 * time.Hour),
	} {
		below, err := log.Histogram(t.Context(), store.HistogramQuery{Bucket: store.BucketHour,
			ListQuery: store.ListQuery{At: at, Until: top}})
		if err != nil {
			t.Fatalf("histogram: %v", err)
		}
		shown := below.InsideHistory(at)
		if shown.Since != first.Format(time.RFC3339) || shown.Until != shown.Since ||
			len(shown.Bars) != 0 || shown.Total != 0 || shown.Failed != 0 {
			t.Errorf("a window ending %s is shown as %s .. %s with %d bars and total %d "+
				"(%d failed), want an empty window at %s", name, shown.Since, shown.Until,
				len(shown.Bars), shown.Total, shown.Failed, first.Format(time.RFC3339))
		}
	}
	// AND A DEGENERATE WINDOW INSIDE THE HISTORY STILL WIDENS to the one
	// bucket holding it, with nothing to drop.
	point := first.Add(2 * time.Hour)
	one, err := log.Histogram(t.Context(), store.HistogramQuery{Bucket: store.BucketHour,
		ListQuery: store.ListQuery{At: at, Since: point, Until: point}})
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	if shown := one.InsideHistory(at); shown.Since != point.Format(time.RFC3339) || len(shown.Bars) != 1 {
		t.Errorf("a zero-width window at %s is shown as %s .. %s with %d bars, want the one bucket",
			point, shown.Since, shown.Until, len(shown.Bars))
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

// A BAR SPLITS OUT ITS FAILURES, by the rule a turn's failed mark uses.
//
// A row fails when its writer said so (the `failed` tag) OR when its type IS a
// failure — the records a turn the engine killed between phases leaves, which
// carry no flag at all. Counted by the engine per bar, because a browser folding
// the share over the rows it holds is right for one page and absent for every
// other; and a SPLIT of the bar, so the failed share never exceeds the count.
//
// Mutation: count only the tag, and the budget refusal drops out of its bar.
func TestAHistogramSplitsEachBarsFailures(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	for _, r := range []store.EventRecord{
		{ID: "ok", Type: "agent_phase_completed", Time: base.Add(time.Minute)},
		{ID: "flagged", Type: "agent_phase_completed", Time: base.Add(2 * time.Minute),
			Tags: map[string]string{"failed": "true"}},
		{ID: "refused", Type: "budget_exhausted", Time: base.Add(2 * time.Hour)},
		{ID: "later", Type: "agent_phase_completed", Time: base.Add(2*time.Hour + time.Minute)},
	} {
		r.Category, r.Payload = "task", []byte(`{}`)
		if err := log.Append(t.Context(), r); err != nil {
			t.Fatalf("append %s: %v", r.ID, err)
		}
	}
	got, err := log.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{Since: base, Until: base.Add(3 * time.Hour)},
		Bucket:    store.BucketHour,
	})
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	wantCount, wantFailed := []int{2, 0, 2}, []int{1, 0, 1}
	for i, bar := range got.Bars {
		if bar.Count != wantCount[i] || bar.Failed != wantFailed[i] {
			t.Errorf("bar %d = %d with %d failed, want %d with %d failed",
				i, bar.Count, bar.Failed, wantCount[i], wantFailed[i])
		}
	}
	if got.Total != 4 || got.Failed != 2 {
		t.Errorf("total %d with %d failed, want 4 with 2", got.Total, got.Failed)
	}
}

// ONE CONVERSATION AND ONE SEAT, on the listing and its axis alike.
//
// `channel_id` names an agent-to-agent conversation's events and `agent_id` the
// events one seat published — the seat's derived id, which two unit seats
// sharing a role name do not share. Both go through the one predicate, so the
// axis counts exactly the rows the listing returns.
//
// Mutation: leave either filter out of [ListQuery]'s predicate and its rows are
// every row.
func TestTheChannelAndSeatFiltersNarrowTheListingAndItsAxis(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	for i, r := range []store.EventRecord{
		{ID: "asked", Type: "a2a_asked", Tags: map[string]string{"channel_id": "ch-1", "agent_id": "id-sre"}},
		{ID: "answered", Type: "a2a_answered", Tags: map[string]string{"channel_id": "ch-1", "agent_id": "id-ops"}},
		{ID: "elsewhere", Type: "a2a_asked", Tags: map[string]string{"channel_id": "ch-2", "agent_id": "id-sre"}},
		{ID: "unrelated", Type: "agent_phase_completed", Tags: map[string]string{"agent_role": "Site Reliability"}},
	} {
		r.Time, r.Category, r.Payload = base.Add(time.Duration(i)*time.Minute), "task", []byte(`{}`)
		if err := log.Append(t.Context(), r); err != nil {
			t.Fatalf("append %s: %v", r.ID, err)
		}
	}
	for _, c := range []struct {
		name string
		q    store.ListQuery
		want []string
	}{
		{"channel", store.ListQuery{ChannelID: "ch-1"}, []string{"answered", "asked"}},
		{"seat", store.ListQuery{AgentID: "id-sre"}, []string{"elsewhere", "asked"}},
		{"both", store.ListQuery{ChannelID: "ch-1", AgentID: "id-sre"}, []string{"asked"}},
	} {
		rows, err := log.List(t.Context(), c.q)
		if err != nil {
			t.Fatalf("%s: list: %v", c.name, err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, r.ID)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: listed %v, want %v", c.name, got, c.want)
		}
		axis, err := log.Histogram(t.Context(), store.HistogramQuery{ListQuery: c.q, Bucket: store.BucketHour})
		if err != nil {
			t.Fatalf("%s: histogram: %v", c.name, err)
		}
		if axis.Total != len(c.want) {
			t.Errorf("%s: the axis counts %d, the listing shows %d", c.name, axis.Total, len(c.want))
		}
	}
}

// "FAILURES ONLY" IS A FILTER, answered by the rule every read stamps.
//
// The event log narrowed the rows it had already paged in, so its axis counted
// every event in the window while the list showed the failures among the
// newest hundred. Filtered here, the list and the axis are one set again, and
// the rule is the one [store.EventRecord.Failed] is read by: a `failed` tag
// OR a type in the failure set — and its negation keeps a row that carries no
// tag at all, which a bare NOT over a NULL comparison would drop. Asked with a
// related agent too, because that FROM joins the party table and the rule's
// columns have to be qualified to mean the log's.
//
// Mutation: drop the clause from the predicate, and every case counts four;
// drop the COALESCE, and the clean half loses the untagged row.
func TestTheFailedFilterIsTheRuleEveryReadStamps(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	failureType := ""
	for _, rec := range []store.EventRecord{
		{ID: "tagged", Type: "thing_happened", Tags: map[string]string{"failed": "true"}},
		{ID: "clean-tag", Type: "thing_happened", Tags: map[string]string{"failed": "false"}},
		{ID: "untagged", Type: "thing_happened"},
		{ID: "by-type", Type: "sandbox_run_failed"},
	} {
		rec.Time = base.Add(time.Minute)
		rec.Category = "system"
		rec.Actor = "PM"
		rec.Payload = []byte(`{}`)
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}
	// THE TYPE IS THE CATALOGUE'S, read back through the row's own mark
	// rather than assumed: if `sandbox_run_failed` ever leaves the failure
	// set this names the case instead of passing on a wrong expectation.
	all, err := log.List(t.Context(), store.ListQuery{Since: base, Until: base.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.ID == "by-type" && r.Failed {
			failureType = r.Type
		}
	}
	if failureType == "" {
		t.Fatal("sandbox_run_failed is not read back as a failure — pick a type in types.FailureEventNames")
	}
	yes, no := true, false
	for _, c := range []struct {
		name string
		q    store.ListQuery
		want []string
	}{
		{"failures", store.ListQuery{Failed: &yes}, []string{"by-type", "tagged"}},
		{"clean", store.ListQuery{Failed: &no}, []string{"clean-tag", "untagged"}},
		{"either", store.ListQuery{}, []string{"by-type", "clean-tag", "tagged", "untagged"}},
		{"failures involving a seat", store.ListQuery{Failed: &yes, RelatedAgent: "PM"}, []string{"by-type", "tagged"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := c.q
			q.Since, q.Until = base, base.Add(time.Hour)
			rows, err := log.List(t.Context(), q)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			got := idsOf(rows)
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("rows = %v, want %v", got, c.want)
			}
			if q.RelatedAgent != "" {
				return // a related-agent axis is refused by design
			}
			bars, err := log.Histogram(t.Context(), store.HistogramQuery{ListQuery: q, Bucket: store.BucketHour})
			if err != nil {
				t.Fatalf("histogram: %v", err)
			}
			if bars.Total != len(c.want) {
				t.Errorf("the axis counts %d, want %d — the list's own set", bars.Total, len(c.want))
			}
			if c.q.Failed != nil && *c.q.Failed && bars.Failed != bars.Total {
				t.Errorf("an axis of failures has a failed share of %d of %d, want all of it", bars.Failed, bars.Total)
			}
		})
	}
}
