package eventfan

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// pageOf is one node's keyset page over its own rows, exactly as the store
// cuts one: newest first, strictly older than the cursor, at most limit.
func pageOf(rows []store.EventRecord, before *store.EventRecord, limit int) listPart {
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, newestFirst)
	var out []store.EventRecord
	for _, r := range sorted {
		if before != nil && newestFirst(r, *before) <= 0 {
			continue
		}
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return listPart{Rows: out, Full: len(out) >= limit}
}

// A MERGED LISTING IS EXACT FOR DISJOINT STORES, page after page.
//
// The property the fleet's event log rests on: walking the merged pages with
// their own cursors visits every row of every node exactly once, in the order
// one store holding all of them would have — whatever the page size, however
// unevenly the rows are spread, with timestamps that collide, and with some
// nodes' replies CUT to fit the transport. That last is what the obvious k-way
// merge cut at the page size fails: a node that sent fewer rows than the page
// holds rows between its last one and the merged page's last one, and the next
// cursor is already past them.
//
// Mutation: cut at the limit without the horizon and this fails within the
// first few seeds.
func TestMergeListingIsExactForDisjointStores(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for seed := range uint64(200) {
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		nodes := 1 + rng.IntN(4)
		stores := make([][]store.EventRecord, nodes)
		var all []store.EventRecord
		for i := range 1 + rng.IntN(120) {
			// COLLIDING INSTANTS on purpose: burst writes share a
			// microsecond, and the id is what breaks the tie.
			rec := store.EventRecord{
				ID:   fmt.Sprintf("e%03d", i),
				Time: base.Add(time.Duration(rng.IntN(40)) * time.Second),
			}
			// A SKEWED SPREAD: most rows on one node is the shape that
			// breaks a naive merge.
			n := 0
			if rng.IntN(3) == 0 {
				n = rng.IntN(nodes)
			}
			stores[n] = append(stores[n], rec)
			all = append(all, rec)
		}
		slices.SortFunc(all, newestFirst)
		limit := 1 + rng.IntN(25)

		var walked []store.EventRecord
		var cursor *store.EventRecord
		for step := 0; ; step++ {
			if step > len(all)+2 {
				t.Fatalf("seed %d: the walk did not end after %d pages", seed, step)
			}
			parts := make([]listPart, 0, nodes)
			for _, rows := range stores {
				part := pageOf(rows, cursor, limit)
				if len(part.Rows) > 1 && rng.IntN(3) == 0 {
					// A REPLY CUT TO FIT THE TRANSPORT: fewer rows
					// than the page, and marked as holding more.
					part = part.keep(1 + rng.IntN(len(part.Rows)-1)).(listPart)
				}
				parts = append(parts, part)
			}
			page, more := MergeListing(parts, limit)
			walked = append(walked, page...)
			if len(page) == 0 {
				if more {
					t.Fatalf("seed %d: an empty page claimed more rows", seed)
				}
				break
			}
			last := page[len(page)-1]
			cursor = &last
		}
		if !slices.EqualFunc(walked, all, func(a, b store.EventRecord) bool {
			return identityOf(a) == identityOf(b)
		}) {
			t.Fatalf("seed %d (nodes %d, limit %d): walking the merged pages visited\n"+
				"  %v\nwant every row once, in order\n  %v",
				seed, nodes, limit, ids(walked), ids(all))
		}
	}
}

func ids(rows []store.EventRecord) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// THE BARS OF ONE WINDOW SUM ACROSS NODES, and a window that is not the same
// window is refused rather than added.
func TestSeriesSumsAcrossNodes(t *testing.T) {
	t.Parallel()
	axis := func(counts ...int) store.EventHistogram {
		h := store.EventHistogram{Bucket: store.BucketHour, Since: "2026-09-01T00:00:00Z",
			Until: "2026-09-01T03:00:00Z", ByCategory: map[string]int{}}
		for i, c := range counts {
			h.Bars = append(h.Bars, store.EventBar{
				At: fmt.Sprintf("2026-09-01T%02d:00:00Z", i), Count: c})
			h.Total += c
		}
		h.ByCategory["task"] = h.Total
		return h
	}
	a, b := axis(1, 0, 4), axis(2, 3, 0)
	skewed := axis(9, 9, 9)
	skewed.Since = "2026-09-01T01:00:00Z"

	got, refused := MergeSeries(a, []store.EventHistogram{b, skewed})
	if want := []int{3, 3, 4}; !slices.Equal(counts(got), want) {
		t.Errorf("bars = %v, want %v — each bar is the sum of the same bar on every node",
			counts(got), want)
	}
	if got.Total != 10 || got.ByCategory["task"] != 10 {
		t.Errorf("total %d, task facet %d; want both 10", got.Total, got.ByCategory["task"])
	}
	if !slices.Equal(refused, []int{1}) {
		t.Errorf("refused %v, want the skewed window (index 1) and only it — adding "+
			"a node's bars to another's next hour is a chart nobody can read", refused)
	}
	if counts(a)[0] != 1 {
		t.Error("the merge wrote through to the asker's own answer")
	}
}

func counts(h store.EventHistogram) []int {
	out := make([]int, 0, len(h.Bars))
	for _, b := range h.Bars {
		out = append(out, b.Count)
	}
	return out
}

// A TURN SPLIT ACROSS TWO NODES MERGES TO ITS OPENING AND ITS ENDING, and says
// how much lies between.
func TestAMergedTurnKeepsBothEndsAndCountsTheMiddle(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	row := func(i int) store.EventRecord {
		return store.EventRecord{ID: fmt.Sprintf("r%04d", i), Time: base.Add(time.Duration(i) * time.Second)}
	}
	// Node A ran the first 400 rows, node B the next 300, each short of
	// the cap on its own: neither fetched a closing, and together they are
	// past it.
	var a, b []store.EventRecord
	for i := range 400 {
		a = append(a, row(i))
	}
	for i := 400; i < 700; i++ {
		b = append(b, row(i))
	}
	rows, total, _ := MergeTurn([]turnPart{
		{Head: a, Total: len(a)}, {Head: b, Total: len(b)},
	})
	if total != 700 {
		t.Fatalf("total = %d, want 700", total)
	}
	if len(rows) != store.MaxTurnEvents+TurnClosingEvents {
		t.Fatalf("rows = %d, want the opening %d plus the ending %d",
			len(rows), store.MaxTurnEvents, TurnClosingEvents)
	}
	if rows[0].ID != "r0000" || rows[len(rows)-1].ID != "r0699" {
		t.Errorf("rows run %s … %s, want the turn's first and last", rows[0].ID, rows[len(rows)-1].ID)
	}
}

// A REPLY TOO LARGE FOR THE TRANSPORT IS CUT, NOT LOST — and a turn's reply
// gives up its middle before its ending.
func TestAnOversizedReplyIsCutAndKeepsTheTurnsEnding(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var head []store.EventRecord
	for i := range 100 {
		head = append(head, store.EventRecord{
			ID: fmt.Sprintf("r%03d", i), Time: base.Add(time.Duration(i) * time.Second),
			Payload: json.RawMessage(`"` + strings.Repeat("x", 1000) + `"`),
		})
	}
	const limit = 40 << 10
	body, err := fit("node-b", turnPart{Head: head, Total: len(head)}, limit, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > limit {
		t.Fatalf("the reply is %d bytes against a %d-byte limit", len(body), limit)
	}
	node, part, why := decodeReply[turnPart](body, 1)
	if node != "node-b" || why != "" {
		t.Fatalf("decoded %q with %q", node, why)
	}
	if len(part.Closing) != TurnClosingEvents || part.Closing[len(part.Closing)-1].ID != "r099" {
		t.Fatalf("the cut reply ends at %v — the turn's last rows are its outcome and "+
			"must be the last thing a cut gives up", ids(part.Closing))
	}
	if part.Total != 100 || len(part.Head)+len(part.Closing) >= 100 {
		t.Errorf("head %d + closing %d of total %d: the cut must say it holds more",
			len(part.Head), len(part.Closing), part.Total)
	}

	// A LISTING CUT TO NOTHING cannot say where its rows are, so it is an
	// error rather than an empty page.
	huge := listPart{Rows: head[:1], Full: false}
	body, err = fit("node-b", huge, 200, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, why := decodeReply[listPart](body, 1); why != ErrTooLarge.Error() {
		t.Errorf("a row larger than the limit answered %q, want %q", why, ErrTooLarge)
	}
}

// TWO SCATTERS' COVERAGE: a node missing from either is missing.
func TestCoverageOfTwoScattersNamesANodeMissingFromEither(t *testing.T) {
	t.Parallel()
	first := Coverage{Complete: true, Nodes: []NodeCoverage{
		{ID: "a", Answered: true}, {ID: "b", Answered: true}}}
	second := Coverage{Complete: false, Nodes: []NodeCoverage{
		{ID: "a", Answered: true}, {ID: "b", Error: "gone"}}}
	got := first.And(second)
	if got.Complete || !slices.Equal(got.Missing(), []string{"b"}) {
		t.Errorf("combined = %+v, want b missing and the whole incomplete", got)
	}
}

// A RELATED PAGE WITH MORE NEVER PAGES PAST ITS OWN DIRECT ROWS.
//
// The next page resumes from this page's last row. A fleet's merged page can
// stop short of the limit — at the newest point a reply cut to fit the
// transport stopped — and an older trace sibling left free to fill that gap
// becomes the last row, so the cursor steps over direct rows no page has shown.
//
// Mutation: drop the boundary filter in MergeRelated and the sibling becomes
// the page's last row.
func TestARelatedPageWithMoreKeepsNoSiblingPastItsLastDirectRow(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	direct := []store.EventRecord{{ID: "d10", Time: at(10)}, {ID: "d8", Time: at(8)}}
	siblings := []store.EventRecord{{ID: "s9", Time: at(9)}, {ID: "s1", Time: at(1)}}

	got := ids(MergeRelated(direct, siblings, 5, true))
	if !slices.Equal(got, []string{"d10", "s9", "d8"}) {
		t.Fatalf("a page with more merged to %v, want the older sibling left for its own page", got)
	}
	// AT THE END OF THE RECORD there is no next page to skip into, so every
	// sibling belongs on this one.
	got = ids(MergeRelated(direct, siblings, 5, false))
	if !slices.Equal(got, []string{"d10", "s9", "d8", "s1"}) {
		t.Fatalf("the last page merged to %v, want every sibling", got)
	}
}

// A SPEND MERGE STOPS WHERE A FULL PART STOPPED, for MergeListing's reason: a
// node whose reply was cut to fit the transport holds older records nobody has
// seen, so a record older than its last one cannot be placed — another node's
// record there would be summed while the cut node's beside it was not.
//
// Mutation: drop the horizon cut and pb-3 is kept.
func TestMergeSpendStopsWhereAFullPartStopped(t *testing.T) {
	t.Parallel()
	at := func(minute int) string {
		return time.Date(2026, 6, 14, 12, minute, 0, 0, time.UTC).Format(time.RFC3339Nano)
	}
	cut := spendPart{Full: true, Records: []tokens.Record{
		{EventID: "pa-10", Timestamp: at(10)}, {EventID: "pa-9", Timestamp: at(9)},
	}}
	whole := spendPart{Records: []tokens.Record{
		{EventID: "pb-12", Timestamp: at(12)}, {EventID: "pb-3", Timestamp: at(3)},
		// The same record twice is one record: a record is SUMMED.
		{EventID: "pa-10", Timestamp: at(10)},
	}}
	var got []string
	for _, r := range MergeSpend([]spendPart{cut, whole}, 10) {
		got = append(got, r.EventID)
	}
	if !slices.Equal(got, []string{"pb-12", "pa-10", "pa-9"}) {
		t.Fatalf("merged %v, want pb-12, pa-10, pa-9 and nothing past the cut", got)
	}
	if n := len(MergeSpend([]spendPart{whole}, 1)); n != 1 {
		t.Errorf("a merge cut at one kept %d", n)
	}
}
