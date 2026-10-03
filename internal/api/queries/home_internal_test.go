package queries

import (
	"fmt"
	"testing"
	"time"
)

var feedBase = time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)

// fakeRow is one row of a fake source: its minute before [feedBase] and, for
// a schedule row, which schedule-and-runner it is a run of.
type fakeRow struct {
	minute int
	run    string
}

// fakeSources serves each named source strictly after its cursor, a page of
// `limit` at a time, as a real reader does.
func fakeSources(t *testing.T, all map[string][]fakeRow, order []string,
	cursor feedCursor, limit int) []*feedSource {

	t.Helper()
	read := func(name string) func(string) (feedPageOf, error) {
		return func(position string) (feedPageOf, error) {
			var page feedPageOf
			for _, row := range all[name] {
				if position != "" && row.minute <= mustAtoi(t, position) {
					continue
				}
				if len(page.entries) == limit {
					page.more = true
					break
				}
				entry := FeedEntry{At: feedBase.Add(-time.Duration(row.minute) * time.Minute)}
				if row.run != "" {
					entry.Kind = FeedSchedule
					entry.Schedule = &FeedScheduleRun{ScopeType: "role", ScopeID: "pm",
						Name: "sweep", Target: row.run, Runs: 1}
				}
				page.entries = append(page.entries, entry)
				page.positions = append(page.positions, fmt.Sprint(row.minute))
			}
			return page, nil
		}
	}
	var sources []*feedSource
	for _, name := range order {
		if cursor[name] == feedEnd {
			continue
		}
		next := read(name)
		first, err := next(cursor[name])
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, &feedSource{name: name, feedPageOf: first, next: next})
	}
	return sources
}

// scroll reads every page to the end and returns them.
func scroll(t *testing.T, all map[string][]fakeRow, limit int) [][]FeedEntry {
	t.Helper()
	cursor := feedCursor{}
	var pages [][]FeedEntry
	for {
		if len(pages) > 100 {
			t.Fatal("the scroll does not end")
		}
		sources := fakeSources(t, all, []string{"work", "pages", "schedule"}, cursor, limit)
		entries, next, err := mergeFeed(sources, cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, entries)
		if next == nil {
			return pages
		}
		cursor = next
	}
}

// A MERGED SCROLL NEITHER REPEATS NOR SKIPS A ROW OF ANY SOURCE.
//
// Three sources interleaved by instant, read three rows at a time: each page's
// cursor must record, per source, exactly the last row of it the page
// consumed — a cursor that advanced only the source that filled the page, or
// every source to its page's end, loses or repeats the rows the merge left
// behind.
func TestAMergedFeedResumesEverySourceWhereItStopped(t *testing.T) {
	t.Parallel()
	all := map[string][]fakeRow{
		"work":     {{minute: 0}, {minute: 3}, {minute: 4}, {minute: 8}, {minute: 9}},
		"pages":    {{minute: 1}, {minute: 5}, {minute: 6}},
		"schedule": {{minute: 2, run: "pm"}, {minute: 7, run: "pm"}},
	}
	var seen []time.Time
	for _, page := range scroll(t, all, 3) {
		for _, entry := range page {
			seen = append(seen, entry.At)
		}
	}
	if len(seen) != 10 {
		t.Fatalf("the scroll saw %d rows, want all 10", len(seen))
	}
	for i := range seen {
		if want := feedBase.Add(-time.Duration(i) * time.Minute); !seen[i].Equal(want) {
			t.Fatalf("row %d is %v, want %v — merged newest first with no gap or repeat",
				i, seen[i], want)
		}
	}
}

// ONE SCHEDULE'S HEARTBEAT IS ONE ROW, NOT A PAGE OF IT.
//
// Forty runs of one sweep sit between the newest work and a filing: the first
// page must hold the work, ONE folded row counting all forty and naming the
// oldest as `since`, and the filing — not three rows of the sweep. A run for
// another runner, or one a work row falls between, is its own row.
func TestConsecutiveRunsOfOneScheduleAreOneRow(t *testing.T) {
	t.Parallel()
	var sweep []fakeRow
	for i := range 40 {
		sweep = append(sweep, fakeRow{minute: 1 + i, run: "pm"})
	}
	sweep = append(sweep, fakeRow{minute: 42, run: "cto"}, fakeRow{minute: 44, run: "cto"})
	all := map[string][]fakeRow{
		"work":     {{minute: 0}, {minute: 41}, {minute: 43}},
		"schedule": sweep,
	}
	pages := scroll(t, all, DefaultFeedPage)
	var rows []FeedEntry
	for _, page := range pages {
		rows = append(rows, page...)
	}
	// work · sweep for pm ×40 · work · cto · work · cto — the work row at
	// minute 43 falls between the two cto runs, so they are two rows.
	if len(rows) != 6 || rows[0].Schedule != nil || rows[1].Schedule == nil ||
		rows[2].Schedule != nil || rows[3].Schedule == nil || rows[4].Schedule != nil ||
		rows[5].Schedule == nil {
		t.Fatalf("the feed is %+v, want work, one folded sweep, work, cto, work, cto", rows)
	}
	run := rows[1].Schedule
	if run.Runs != 40 || run.Since == nil || !run.Since.Equal(feedBase.Add(-40*time.Minute)) ||
		!rows[1].At.Equal(feedBase.Add(-time.Minute)) {
		t.Errorf("the folded row is %+v at %v, want 40 runs from minute 1 back to minute 40",
			run, rows[1].At)
	}
	if rows[3].Schedule.Runs != 1 || rows[3].Schedule.Since != nil || rows[5].Schedule.Runs != 1 {
		t.Errorf("the cto runs are %+v and %+v, want one run each", rows[3].Schedule, rows[5].Schedule)
	}
}

// A SOURCE THAT RUNS DRY IS READ AGAIN, AND A PAGE THAT MAY NOT READ IT AGAIN
// ENDS SHORT RATHER THAN OUT OF ORDER — and every run is still counted once
// across the scroll.
func TestAFoldedRunLongerThanTheReadBoundSpansPagesWithoutLosingARun(t *testing.T) {
	t.Parallel()
	const runs = 50
	var sweep []fakeRow
	for i := range runs {
		sweep = append(sweep, fakeRow{minute: 1 + i, run: "pm"})
	}
	all := map[string][]fakeRow{
		"work":     {{minute: 0}, {minute: runs + 1}},
		"schedule": sweep,
	}
	const limit = 5
	pages := scroll(t, all, limit)
	total, works := 0, 0
	var last time.Time
	for _, page := range pages {
		for _, entry := range page {
			if !last.IsZero() && entry.At.After(last) {
				t.Fatalf("a row at %v follows one at %v — out of order", entry.At, last)
			}
			last = entry.At
			if entry.Schedule != nil {
				total += entry.Schedule.Runs
			} else {
				works++
			}
		}
	}
	if total != runs || works != 2 {
		t.Errorf("the scroll counted %d runs and %d work rows, want %d and 2", total, works, runs)
	}
	if got := pages[0]; len(got) != 2 || got[1].Schedule == nil ||
		got[1].Schedule.Runs != feedSourceReads*limit {
		t.Errorf("the first page is %+v, want the work and one row folding the %d runs "+
			"%d reads of %d hold", got, feedSourceReads*limit, feedSourceReads, limit)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return n
}
