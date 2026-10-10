package usage_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// fillRow is one row of a synthetic paged read: the group it belongs to and
// its place in it.
type fillRow struct {
	G string
	I int
}

func (r fillRow) group() usage.Group { return usage.Group{r.G} }

// rowsPast is the rows whose group sorts after after — the keyset a paged
// read resumes by.
func rowsPast(rows []fillRow, after usage.Group) iter.Seq2[fillRow, error] {
	return func(yield func(fillRow, error) bool) {
		for _, r := range rows {
			if !after.Before(r.group()) {
				continue
			}
			if !yield(r, nil) {
				return
			}
		}
	}
}

// A PAGE IS CUT ONLY AT A RECORD'S EDGE — [usage.Fill], over values.
//
// Pages are separate reads, possibly on separate nodes, and an apply replaces
// a record's rows whole: a page that ended inside a group could hand a reader
// the first cells of one version of a seat's day and the rest of the next. So
// however small the budget, a page ends on a group boundary, the group it
// ended on is the next page's resumption, and the pages concatenate to the
// unbounded read exactly.
func TestAPageIsCutOnlyAtARecordsEdge(t *testing.T) {
	t.Parallel()
	var rows []fillRow
	for g, size := range []int{1, 3, 7, 1, 7, 3} {
		for i := range size {
			rows = append(rows, fillRow{G: fmt.Sprintf("g%02d", g), I: i})
		}
	}
	whole, err := usage.Fill(0, fillRow.group, rowsPast(rows, nil))
	if err != nil || !whole.Done || whole.Next != nil || !slices.Equal(whole.Rows, rows) {
		t.Fatalf("an unbounded fill = (%+v, %v), want every row in one done page", whole, err)
	}

	for _, budget := range []int{1, 40, 200} {
		t.Run(fmt.Sprintf("budget %d", budget), func(t *testing.T) {
			var got []fillRow
			var after usage.Group
			pages := 0
			for {
				page, err := usage.Fill(budget, fillRow.group, rowsPast(rows, after))
				if err != nil {
					t.Fatal(err)
				}
				pages++
				if len(page.Rows) == 0 {
					t.Fatalf("page %d is empty", pages)
				}
				last := page.Rows[len(page.Rows)-1].group()
				if page.Done {
					if page.Next != nil {
						t.Fatalf("the last page names a next group %v", page.Next)
					}
				} else if !slices.Equal(page.Next, last) {
					t.Fatalf("page %d ended on %v and names %v to resume after", pages, last, page.Next)
				}
				got = append(got, page.Rows...)
				if page.Done {
					break
				}
				after = page.Next
				if pages > len(rows) {
					t.Fatal("the pages never ended")
				}
			}
			if !slices.Equal(got, rows) {
				t.Fatalf("the pages concatenate to %v, want %v", got, rows)
			}
			// NO GROUP SPLIT: every group's rows are on one page, which the
			// count of pages a group's rows began on says.
			if budget == 1 && pages != 6 {
				t.Fatalf("a budget below one row made %d pages of six groups, want one a group", pages)
			}
		})
	}
}

// pagedRead walks one paged read to its end at budget bytes a page, and
// returns its rows and how many pages it took — failing on a page that is
// done before the end or not after the one before it.
func pagedRead[R any](t *testing.T, budget int,
	page func(after usage.Group, budget int) (usage.Page[R], error)) ([]R, int) {

	t.Helper()
	var out []R
	var after usage.Group
	for pages := 1; ; pages++ {
		p, err := page(after, budget)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		out = append(out, p.Rows...)
		if p.Done {
			return out, pages
		}
		if !after.Before(p.Next) {
			t.Fatalf("page %d ended at %v, not after %v", pages, p.Next, after)
		}
		after = p.Next
	}
}

// seedWindow writes a window of usage across three nodes, four days, several
// seats, two people and reads of several pages, through the shipped applier.
func seedWindow(t *testing.T, db *store.DB) {
	t.Helper()
	seq := uint64(0)
	apply := func(r usage.Record) {
		t.Helper()
		seq++
		if err := applyRecord(t.Context(), db, recordAt(t, r, seq)); err != nil {
			t.Fatalf("apply %s: %v", r.Subject, err)
		}
	}
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for _, node := range []string{"node-a", "node-b", "node-c"} {
		for d := range 4 {
			day := fmt.Sprintf("2026-09-%02d", 20+d)
			for s := range 3 {
				r := seatRecord(node, day, fmt.Sprintf("seat-%d", s), "m-a", "m-b", "m-c")
				r.Turns.Durations.Add(time.Duration(s+1) * time.Minute)
				for p := range 3 {
					for _, via := range []string{"search", "skill_injected"} {
						r.Reads = append(r.Reads, usage.Read{PageID: fmt.Sprintf("page-%d", p),
							Backend: "native", Via: via, Count: int64(p + 1), LastAt: at,
							LastQuery: strings.Repeat("why ", 20)})
					}
				}
				apply(r)
			}
			for _, person := range []string{"ana", "bo"} {
				apply(personRecord(node, day, person, "haiku", "sonnet"))
			}
		}
	}
}

// EVERY PAGED READ RETURNS THE WHOLE READ IN PAGES: walked a page at a time
// at a budget of 1 KiB, each of the five reads concatenates to exactly its
// whole read's rows, in order — the seat days' token totals included, which
// each page sums over its own key range.
func TestEveryPagedReadReturnsTheWholeReadInPages(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedWindow(t, db)
	estate := db.Replicated()
	ctx := t.Context()
	days := usage.SpendQuery{From: "2026-09-20", To: "2026-09-23"}
	reads := usage.PageReadsQuery{From: days.From, To: days.To,
		PageIDs: []string{"page-2", "page-0", "page-1", "page-0"}}
	const budget = 1 << 10

	check := func(t *testing.T, name string, whole, paged any, pages int) {
		t.Helper()
		if fmt.Sprint(whole) != fmt.Sprint(paged) {
			t.Fatalf("%s: the pages concatenate to\n%v\nwant the whole read\n%v", name, paged, whole)
		}
		if pages < 3 {
			t.Fatalf("%s: %d page(s) at %d bytes — the case reads too little to page", name, pages, budget)
		}
	}
	t.Run("spend", func(t *testing.T) {
		whole, err := usage.Spend(ctx, estate, days)
		if err != nil || len(whole) == 0 {
			t.Fatalf("the whole read: (%d rows, %v)", len(whole), err)
		}
		paged, pages := pagedRead(t, budget, func(after usage.Group, budget int) (usage.Page[usage.SpendRow], error) {
			return usage.SpendPage(ctx, estate, days, after, budget)
		})
		check(t, "spend", whole, paged, pages)
	})
	t.Run("seat turns", func(t *testing.T) {
		whole, err := usage.SeatTurns(ctx, estate, days)
		if err != nil || len(whole) == 0 {
			t.Fatalf("the whole read: (%d rows, %v)", len(whole), err)
		}
		paged, pages := pagedRead(t, budget, func(after usage.Group, budget int) (usage.Page[usage.TurnsRow], error) {
			return usage.SeatTurnsPage(ctx, estate, days, after, budget)
		})
		check(t, "seat turns", whole, paged, pages)
	})
	t.Run("person spend", func(t *testing.T) {
		q := usage.PersonQuery{From: days.From, To: days.To}
		whole, err := usage.PersonSpend(ctx, estate, q)
		if err != nil || len(whole) == 0 {
			t.Fatalf("the whole read: (%d rows, %v)", len(whole), err)
		}
		paged, pages := pagedRead(t, budget, func(after usage.Group, budget int) (usage.Page[usage.PersonRow], error) {
			return usage.PersonSpendPage(ctx, estate, q, after, budget)
		})
		check(t, "person spend", whole, paged, pages)
	})
	t.Run("seat days", func(t *testing.T) {
		whole, err := usage.SeatDays(ctx, estate, days)
		if err != nil || len(whole) == 0 {
			t.Fatalf("the whole read: (%d rows, %v)", len(whole), err)
		}
		for _, d := range whole {
			if d.Tokens == 0 {
				t.Fatalf("the premise: the whole read summed no tokens for %v", d.Group())
			}
		}
		paged, pages := pagedRead(t, budget, func(after usage.Group, budget int) (usage.Page[usage.SeatDay], error) {
			return usage.SeatDaysPage(ctx, estate, days, after, budget)
		})
		check(t, "seat days", whole, paged, pages)
	})
	t.Run("page reads", func(t *testing.T) {
		whole, err := usage.PageReads(ctx, estate, reads)
		if err != nil || len(whole) == 0 {
			t.Fatalf("the whole read: (%d rows, %v)", len(whole), err)
		}
		paged, pages := pagedRead(t, budget, func(after usage.Group, budget int) (usage.Page[usage.PageRead], error) {
			return usage.PageReadsPage(ctx, estate, reads, after, budget)
		})
		check(t, "page reads", whole, paged, pages)
	})
	if _, err := usage.SpendPage(ctx, estate, days, usage.Group{"2026-09-20", "node-a"}, budget); err == nil ||
		!strings.Contains(err.Error(), "resumes after a group of 3 columns, not 2") {
		t.Fatalf("a page resumed after a group of the wrong arity: %v", err)
	}
}

// A PAGE RESUMED ACROSS A REPLACEMENT NEVER MIXES A RECORD'S VERSIONS. Pages
// are separate transactions, and between two of them a node may republish a
// seat's day — replacing its rows whole. Read across that, every seat-day in
// the answer is wholly one version: the old one where its page came before
// the replacement, the new one where it came after, and never the first cells
// of one and the rest of the other.
func TestAPageResumedAcrossAReplacementNeverMixesARecordsVersions(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	estate := db.Replicated()
	ctx := t.Context()
	const day = "2026-09-23"
	seq := uint64(0)
	apply := func(r usage.Record) {
		t.Helper()
		seq++
		if err := applyRecord(ctx, db, recordAt(t, r, seq)); err != nil {
			t.Fatalf("apply %s: %v", r.Subject, err)
		}
	}
	for s := range 6 {
		apply(seatRecord("node-a", day, fmt.Sprintf("seat-%d", s), "v1-a", "v1-b", "v1-c"))
	}
	q := usage.SpendQuery{From: day, To: day}
	first, err := usage.SpendPage(ctx, estate, q, nil, 1)
	if err != nil || first.Done || len(first.Rows) != 3 {
		t.Fatalf("the first page = (%+v, %v), want one seat-day of three cells", first, err)
	}

	// A LATER SEAT-DAY IS REPUBLISHED between the pages, with other cells.
	apply(seatRecord("node-a", day, "seat-3", "v2-a", "v2-b", "v2-c", "v2-d"))
	rows := first.Rows
	after := first.Next
	for !first.Done {
		first, err = usage.SpendPage(ctx, estate, q, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, first.Rows...)
		after = first.Next
	}
	versions := map[string]map[string]int{}
	for _, r := range rows {
		seat := r.AgentID
		if versions[seat] == nil {
			versions[seat] = map[string]int{}
		}
		versions[seat][r.Model[:2]]++
	}
	for seat, seen := range versions {
		if len(seen) != 1 {
			t.Fatalf("%s's day was read as a mix of versions: %v", seat, seen)
		}
	}
	if got := versions["seat-3"]; got["v2"] != 4 {
		t.Fatalf("the republished seat-day read %v, want its newer version whole", got)
	}
	if got := versions["seat-0"]; got["v1"] != 3 {
		t.Fatalf("a seat-day read before the republish read %v, want its version whole", got)
	}
}

// A PAGE ENCODES AS ITS FIELDS, WHICHEVER WAY IT WAS FILLED. A bounded page
// keeps the encoding [usage.Fill] measured each row by and writes those bytes
// rather than encoding the rows again ([usage.Page.AppendJSON]); the far side
// decodes a page with the ordinary decoder, so the bytes must be exactly what
// json.Marshal writes for the same page — measured or not, cut short or done,
// with rows or with none.
func TestAPageEncodesAsItsFieldsWhicheverWayItWasFilled(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	var rows []usage.PageRead
	for i := range 40 {
		rows = append(rows, usage.PageRead{PageID: "page-<1>", Day: "2026-09-23",
			Node: "node-a", AgentID: fmt.Sprintf("seat-%02d", i/4), Via: fmt.Sprintf("via-%d", i%4),
			Count: int64(i), LastAt: at, LastQuery: `"quoted" & <escaped>`})
	}
	all := func(yield func(usage.PageRead, error) bool) {
		for _, r := range rows {
			if !yield(r, nil) {
				return
			}
		}
	}
	none := func(func(usage.PageRead, error) bool) {}
	cases := map[string]struct {
		budget int
		rows   iter.Seq2[usage.PageRead, error]
	}{
		"measured and cut":   {1 << 10, all},
		"measured and done":  {1 << 20, all},
		"unbounded":          {0, all},
		"empty and measured": {1 << 10, none},
		"empty":              {0, none},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			page, err := usage.Fill(c.budget, usage.PageRead.Group, c.rows)
			if err != nil {
				t.Fatal(err)
			}
			got, err := page.AppendJSON(nil)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("the page encodes as\n%s\nwant what json.Marshal writes\n%s", got, want)
			}
			var back usage.Page[usage.PageRead]
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(back.Rows, page.Rows) || !slices.Equal(back.Next, page.Next) ||
				back.Done != page.Done {
				t.Fatalf("the page decodes as %+v, want %+v", back, page)
			}
		})
	}
}
