package store

import (
	"cmp"
	"slices"
	"testing"
	"time"
)

// at builds a record at a whole-second offset, with a trace and an id whose
// lexical order matches its recency — the tiebreak [mergeRelated] sorts on.
func at(id, trace string, second int) EventRecord {
	return EventRecord{
		ID:      id,
		TraceID: trace,
		Time:    time.Date(2026, 6, 14, 12, 0, second, 0, time.UTC),
	}
}

// THE PAGE ENDS WHERE THE WALK RESUMES.
//
// The related-agent listing pages by keyset: the caller's next cursor is the
// LAST row of the page, and the page after it asks for rows strictly older.
// So every record the merge drops has to be strictly older than the record
// the page ends on — otherwise the next page starts past it and the walk has
// a hole rather than a shortening, which is the failure mode that made
// [cutToPage] load-bearing rather than cosmetic.
func TestAMergedPageNeverDropsARecordItsCursorHasAlreadyPassed(t *testing.T) {
	t.Parallel()
	// Siblings deliberately straddle the direct matches in time: some are
	// newer than the oldest direct match, some older, so the sort has to
	// interleave them and the cut cannot fall on a group boundary.
	direct := []EventRecord{
		at("d9", "t1", 90),
		at("d7", "t2", 70),
		at("d5", "t3", 50),
		at("d3", "t4", 30),
	}
	siblings := []EventRecord{
		at("s8", "t1", 80),
		at("s6", "t2", 60),
		at("s4", "t3", 40),
		at("s2", "t4", 20),
		at("s1", "t4", 10),
	}
	const limit = 4

	page := mergeRelated(direct, siblings, limit)
	if len(page) != limit {
		t.Fatalf("page holds %d records, want the limit of %d", len(page), limit)
	}
	cursor := page[len(page)-1]

	kept := map[string]struct{}{}
	for _, rec := range page {
		kept[rec.ID] = struct{}{}
	}
	for _, group := range [][]EventRecord{direct, siblings} {
		for _, rec := range group {
			if _, on := kept[rec.ID]; on {
				continue
			}
			// Strictly older: equal to the cursor would be returned
			// by neither page, since the next one is exclusive.
			if !rec.Time.Before(cursor.Time) {
				t.Errorf("the merge dropped %s at %s, which is not older than the "+
					"cursor %s at %s — the next page starts before the cursor, so "+
					"that record is returned by no page at all",
					rec.ID, rec.Time.Format(time.RFC3339), cursor.ID,
					cursor.Time.Format(time.RFC3339))
			}
		}
	}
}

// EVERY DROPPED SIBLING'S TRACE IS NAMED BY A RECORD THE CALLER GETS.
//
// This is the whole of [cutToPage]'s recoverability claim, and it is the half
// that is NOT covered by paging: a dropped direct match comes back on a later
// page, while a dropped sibling's trace may never be queried again. What makes
// that acceptable is that the sibling only exists because it shares a trace
// with a row on this page, so that row's TraceID is what reaches it —
// [cutToPage] says through which reads. If the cut could ever drop a sibling
// whose trace no returned record names, the row would be unreachable and the
// doc would be a fiction.
func TestEveryDroppedSiblingLeavesItsTraceOnThePage(t *testing.T) {
	t.Parallel()
	direct := []EventRecord{
		at("d9", "t1", 90),
		at("d8", "t2", 80),
	}
	// Both traces' causes are old — the trigger that woke the seat fired
	// long before the turn ran — so the cut takes them and only them.
	siblings := []EventRecord{
		at("s2", "t1", 20),
		at("s1", "t2", 10),
	}
	const limit = 2

	page := mergeRelated(direct, siblings, limit)
	traces := map[string]struct{}{}
	kept := map[string]struct{}{}
	for _, rec := range page {
		traces[rec.TraceID] = struct{}{}
		kept[rec.ID] = struct{}{}
	}
	dropped := 0
	for _, rec := range siblings {
		if _, on := kept[rec.ID]; on {
			continue
		}
		dropped++
		if _, named := traces[rec.TraceID]; !named {
			t.Errorf("sibling %s of trace %s was dropped and no record on the page "+
				"names that trace, so the caller holds nothing that reaches it",
				rec.ID, rec.TraceID)
		}
	}
	if dropped == 0 {
		t.Fatal("no sibling was dropped, so this test asserted nothing about the cut")
	}
}

// A MERGE UNDER THE LIMIT IS NOT CUT AT ALL, and the dedupe is what keeps it
// there: the sibling query selects on trace_id, so it returns the direct
// matches themselves alongside their siblings. Counting those twice would
// push a page that fits over its own limit and drop real rows to make room
// for duplicates of rows already on it.
func TestASiblingThatIsAlsoADirectMatchIsNotCountedTwice(t *testing.T) {
	t.Parallel()
	direct := []EventRecord{at("d2", "t1", 20), at("d1", "t1", 10)}
	siblings := []EventRecord{at("d2", "t1", 20), at("s3", "t1", 30), at("d1", "t1", 10)}

	page := mergeRelated(direct, siblings, 3)
	if len(page) != 3 {
		t.Fatalf("page holds %d records, want 3 distinct ones", len(page))
	}
	want := []string{"s3", "d2", "d1"}
	for i, id := range want {
		if page[i].ID != id {
			t.Fatalf("page[%d] = %s, want %s (newest first, no duplicate)", i, page[i].ID, id)
		}
	}
}

// THE DEDUPE IS ON THE ROW, NOT THE ID. The table's primary key is
// (event_time, event_id) and nothing constrains the id alone, so two rows of
// one trace can carry the same id at different instants. The sibling read
// returns the direct match again, and that copy has to go; the OTHER row
// sharing its id is a distinct row and has to stay, or the page silently
// holds one fewer row than the log does.
func TestADistinctRowSharingAnIDIsNotDeduplicatedAway(t *testing.T) {
	t.Parallel()
	direct := []EventRecord{at("x", "t1", 20)}
	siblings := []EventRecord{at("x", "t1", 30), at("x", "t1", 20)}

	page := mergeRelated(direct, siblings, 5)
	if len(page) != 2 {
		t.Fatalf("page holds %d records, want 2: the direct match once, and the "+
			"distinct row at another instant that shares its id", len(page))
	}
	if !page[0].Time.Equal(siblings[0].Time) || !page[1].Time.Equal(direct[0].Time) {
		t.Fatalf("page is %s then %s, want the row at :30 then the one at :20",
			page[0].Time.Format(time.RFC3339), page[1].Time.Format(time.RFC3339))
	}
}

// THE IDENTITY CASE IS THE ARITHMETIC'S, NEVER A PAGE SIZE. [EventLog.List]
// substitutes defaultListLimit before the merge runs, so a non-positive limit
// reaching [cutToPage] means "do not cut" rather than "return nothing" — the
// opposite reading would turn a caller's unset field into an empty page that
// looks exactly like a company with no history.
func TestANonPositiveLimitLeavesTheMergedSetWhole(t *testing.T) {
	t.Parallel()
	recs := []EventRecord{at("b", "t1", 20), at("a", "t1", 10)}
	for _, limit := range []int{0, -1} {
		if got := cutToPage(recs, limit); len(got) != len(recs) {
			t.Errorf("cutToPage(_, %d) returned %d records, want all %d",
				limit, len(got), len(recs))
		}
	}
}

// THE SIBLING READ'S BUDGET IS EXACTLY SUFFICIENT, which is what makes
// [EventLog.traceSiblings] reusing the page's own limit a justified value
// rather than a number picked for want of another.
//
// The claim is that the page built from the newest `limit` rows of the traces
// is the same page that would be built from ALL of their rows. It holds
// because the answer is the newest `limit` rows of the union and the direct
// matches live in those same traces, so anything the narrower read left out is
// older than the cut either way. If it did not hold, a sibling would be
// missing from a page it belonged on and the only evidence would be a cause
// that quietly never appeared — so the arithmetic is pinned here, where it can
// be exercised without a database.
func TestReadingEverySiblingWouldBuildTheSamePage(t *testing.T) {
	t.Parallel()
	// One trace per direct match, each trace carrying rows both newer and
	// older than the match itself, so the newest-N read genuinely cuts.
	var everything []EventRecord
	var direct []EventRecord
	for trace := range 6 {
		name := string(rune('a' + trace))
		base := 100 - trace*10
		match := at("d"+name, name, base)
		direct = append(direct, match)
		everything = append(everything, match)
		// One sibling NEWER than the match (a reply, a completion) and
		// three older (the trigger that caused it), so the newest-N read
		// cuts within a trace rather than only between traces.
		for _, step := range []int{1, -1, -2, -3} {
			everything = append(everything,
				at("s"+name+string(rune('0'+step+4)), name, base+step))
		}
	}
	newestFirst := func(recs []EventRecord) []EventRecord {
		out := append([]EventRecord(nil), recs...)
		slices.SortStableFunc(out, func(a, b EventRecord) int {
			return cmp.Or(b.Time.Compare(a.Time), cmp.Compare(b.ID, a.ID))
		})
		return out
	}
	const limit = 8

	// What the engine actually does: the newest `limit` rows of the traces.
	narrow := mergeRelated(direct, cutToPage(newestFirst(everything), limit), limit)
	// What an unbounded sibling read would have produced.
	wide := mergeRelated(direct, newestFirst(everything), limit)

	if len(narrow) != len(wide) {
		t.Fatalf("the bounded read built %d rows and the unbounded one %d",
			len(narrow), len(wide))
	}
	for i := range narrow {
		if narrow[i].ID != wide[i].ID {
			t.Fatalf("row %d is %s with the bounded sibling read and %s with the "+
				"unbounded one; the page limit is not a sufficient budget",
				i, narrow[i].ID, wide[i].ID)
		}
	}
}
