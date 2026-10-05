package eventfan

import (
	"encoding/json"
	"fmt"
	"maps"
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

// A TURN PART HELD TO THE HORIZON COUNTS EVERY ROW IT KEPT ONCE.
//
// A node holding 501 to 519 rows of a turn sends its oldest 500 and its newest
// 20, and the two OVERLAP. A peer on an earlier build, whose clock runs behind
// the asker's, sends rows from the strip under the asker's horizon too: here
// 510 rows, the oldest four under it. Held to the horizon, the part keeps 506
// distinct rows — and the count is exact, since the opening's last row is above
// the horizon. Counted as the two lengths added, it was 516, so the merged turn
// said it held ten rows more than it showed: a gap in the middle of a turn it
// holds whole.
//
// Mutation: count `len(head)+len(closing)` in [turnPart.within] and the held
// count is 516 for 506 rows.
func TestATurnPartHeldToTheHorizonCountsEachRowOnce(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var all []store.EventRecord
	for i := range 510 {
		all = append(all, store.EventRecord{ID: fmt.Sprintf("r%04d", i),
			Time: base.Add(time.Duration(i) * time.Second)})
	}
	floor := base.Add(4 * time.Second) // r0000 … r0003 lie under it
	part := turnPart{Head: all[:store.MaxTurnEvents], Closing: all[len(all)-TurnClosingEvents:],
		Total: len(all), Traces: []store.TurnTrace{}}

	held := part.within(floor)
	if want := len(all) - 4; held.Total != want || len(union(held.Head, held.Closing)) != want {
		t.Fatalf("held to the horizon the part counts %d over %d distinct rows, want %d of each",
			held.Total, len(union(held.Head, held.Closing)), want)
	}
	rows, total, _ := MergeTurn([]turnPart{held})
	if total != len(rows) || rows[0].ID != "r0004" || rows[len(rows)-1].ID != "r0509" {
		t.Errorf("the merged turn shows %d rows (%s … %s) of %d — a turn held whole reported "+
			"as cut", len(rows), rows[0].ID, rows[len(rows)-1].ID, total)
	}
}

// rowsAt is n rows named prefix0001…, one second apart from a start.
func rowsAt(prefix string, start time.Time, n int) []store.EventRecord {
	out := make([]store.EventRecord, 0, n)
	for i := range n {
		out = append(out, store.EventRecord{ID: fmt.Sprintf("%s%04d", prefix, i+1),
			Time: start.Add(time.Duration(i) * time.Second)})
	}
	return out
}

// A MERGED TRACE STOPS WHERE A NODE'S UNSENT ROWS BEGIN.
//
// Each node sends its own oldest rows up to the cap, so the fleet's oldest are
// among them — until a node sends FEWER than the cap and holds more. Its rows
// past the last one it sent are older than the other node's rows here, and
// the obvious merge filled the cap with those instead: a view that presents
// itself as the trace's opening, with a hole inside it that `truncated` did
// not say was there. Two ways a part comes to send fewer than it holds, each
// beside another node's newer rows: the asker's horizon cuts rows off the
// front of a capped read — an earlier build, behind the asker's clock, sends
// rows from the strip under the horizon — and a reply cut to fit the
// transport.
//
// Mutation: drop the cut at the oldest last sent row from [MergeTrace], and
// both cases place the other node's rows past the hole.
func TestAMergedTraceStopsWhereANodesUnsentRowsBegin(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	// The peer holds 520 rows of the trace, a0001 … a0520; the asker ten,
	// each newer than the peer's 500th and older than its 501st's
	// successors — inside the peer's unsent rows.
	peer := rowsAt("a", base, 520)
	mine := tracePart{Rows: rowsAt("b", base.Add(503*time.Second+500*time.Millisecond), 10), Total: 10}
	for name, c := range map[string]struct {
		part     tracePart
		floor    time.Time
		last     string
		shown    int
		total    int
		heldPeer bool
	}{
		// a0001 … a0003 under the horizon: the capped read is cut to 497.
		"held to the horizon": {
			part:  tracePart{Rows: peer[:store.MaxTraceEvents], Total: len(peer)},
			floor: base.Add(3 * time.Second), last: "a0500", shown: 497, total: 517 + 10,
		},
		// A reply cut to its first 300 rows.
		"cut to fit the transport": {
			part:  tracePart{Rows: peer[:store.MaxTraceEvents], Total: len(peer)}.keep(300).(tracePart),
			floor: base.Add(-time.Hour), last: "a0300", shown: 300, total: 520 + 10,
		},
	} {
		t.Run(name, func(t *testing.T) {
			parts := []tracePart{mine.within(c.floor), c.part.within(c.floor)}
			rows, total := MergeTrace(parts)
			if len(rows) == 0 || rows[len(rows)-1].ID != c.last || len(rows) != c.shown {
				t.Fatalf("the merged trace shows %d rows ending at %v, want %d ending at %s — "+
					"the peer's last sent row, with nothing past its unsent rows", len(rows),
					ids(rows[max(0, len(rows)-3):]), c.shown, c.last)
			}
			if total != c.total || total <= len(rows) {
				t.Errorf("total = %d over %d rows, want %d — the gap still reported", total, len(rows), c.total)
			}
		})
	}

	// A PART THE HORIZON CUT TO NOTHING places nothing past its unsent rows
	// either. Here every row the peer sent lies under the horizon and some it
	// did not send lie above it, older than every row the asker holds.
	floor := base.Add(500*time.Second + 500*time.Millisecond)
	allUnder := tracePart{Rows: peer[:store.MaxTraceEvents], Total: len(peer)}.within(floor)
	rows, total := MergeTrace([]tracePart{mine.within(floor), allUnder})
	if len(rows) != 0 || total != 20+10 {
		t.Errorf("with every row the peer sent under the horizon, the merge shows %v of %d, "+
			"want nothing placeable of 30", ids(rows), total)
	}
	// AND A NODE HOLDING NOTHING IT DID NOT SEND BOUNDS NOTHING: two whole
	// parts are merged whole, oldest first, up to the cap.
	rows, total = MergeTrace([]tracePart{mine, {Rows: peer[:5], Total: 5}})
	if len(rows) != 15 || total != 15 {
		t.Errorf("two whole parts merged to %d rows of %d, want all 15", len(rows), total)
	}
}

// A MERGED TURN'S OPENING STOPS WHERE A NODE'S UNSENT ROWS BEGIN, for
// [MergeTrace]'s reason — and its ending is still the turn's last rows.
//
// The peer holds 600 rows of the turn and sends its opening and its ending,
// the 80 between unsent; the asker holds ten rows in that gap. Shown in the
// opening, they would sit past a hole the opening does not admit to, and they
// are not in the ending either, which is the peer's last twenty.
//
// Mutation: drop the cut at the oldest last sent row from [MergeTurn], and
// both cases show the asker's rows past the peer's opening.
func TestAMergedTurnsOpeningStopsWhereANodesUnsentRowsBegin(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	peer := rowsAt("a", base, 600)
	whole := turnPart{Head: peer[:store.MaxTurnEvents], Closing: peer[len(peer)-TurnClosingEvents:],
		Total: len(peer), Traces: []store.TurnTrace{}}
	mine := turnPart{Head: rowsAt("b", base.Add(530*time.Second+500*time.Millisecond), 10), Total: 10,
		Traces: []store.TurnTrace{}}
	for name, c := range map[string]struct {
		part  turnPart
		floor time.Time
		last  string
		total int
	}{
		"held to the horizon": {
			part: whole, floor: base.Add(3 * time.Second), last: "a0500", total: 597 + 10,
		},
		"cut to fit the transport": {
			part: whole.keep(200).(turnPart), floor: base.Add(-time.Hour), last: "a0200", total: 600 + 10,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rows, total, _ := MergeTurn([]turnPart{mine.within(c.floor), c.part.within(c.floor)})
			var shown []string
			for _, r := range rows {
				if strings.HasPrefix(r.ID, "b") {
					shown = append(shown, r.ID)
				}
			}
			if len(shown) > 0 {
				t.Fatalf("the merged turn shows %v from inside the peer's unsent rows", shown)
			}
			opening := rows[:len(rows)-TurnClosingEvents]
			if opening[len(opening)-1].ID != c.last {
				t.Errorf("the opening ends at %s, want the peer's last sent row %s",
					opening[len(opening)-1].ID, c.last)
			}
			if ending := rows[len(rows)-TurnClosingEvents:]; ending[0].ID != "a0581" ||
				ending[len(ending)-1].ID != "a0600" {
				t.Errorf("the ending runs %s … %s, want the turn's last twenty", ending[0].ID,
					ending[len(ending)-1].ID)
			}
			if total != c.total {
				t.Errorf("total = %d, want %d — the gap still reported", total, c.total)
			}
		})
	}
}

// EVERY OUTCOME ROW IS COUNTED ONCE, whichever data nodes hold it.
//
// A node counts the rows it keeps and names the rows of a custody batch it has
// written and not settled; the same batch can sit on a second data node, kept
// or not yet settled either. So the merge sums the counts and adds every named
// row once — unless a node that keeps it and did not name it counted it
// already, which is the batch's keeper having settled first. A node that named
// a row and keeps it by the second question settled between the two, and
// counted it in neither, so the row is still added. And a row is its instant
// and its id together: the same id at another instant is another row.
//
// Mutation: add every named row whatever the second question answered, and the
// row its keeper counted is counted twice; add each once per node naming it,
// and the row two nodes hold unsettled is; skip a row any node keeps, and the
// row settled between the two questions is in no count; key the rows by id
// alone, and the two instants of one id are one row.
func TestEveryOutcomeRowIsCountedOnce(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	skip := func(id string, at time.Time) store.OutcomeRow {
		return store.OutcomeRow{Time: at, ID: id, Type: "notification_skipped", App: "gitlab"}
	}
	merge := func(id string, at time.Time) store.OutcomeRow {
		return store.OutcomeRow{Time: at, ID: id, Type: "notifications_coalesced", App: "slack"}
	}
	counted := func(skipped, coalesced map[string]int, unsettled ...store.OutcomeRow) store.NotificationOutcomes {
		return store.NotificationOutcomes{Skipped: skipped, Coalesced: coalesced, Unsettled: unsettled}
	}
	key := func(r store.OutcomeRow) store.OutcomeRow { return store.OutcomeRow{Time: r.Time, ID: r.ID} }
	twice, keptAtB, settledBetween := skip("twice", at), skip("kept-at-b", at), merge("settled-between", at)
	sameID, laterSameID := skip("same-id", at), skip("same-id", at.Add(time.Microsecond))

	got := MergeOutcomes([]OutcomePart{
		{Node: "node-a", Counted: counted(map[string]int{"gitlab": 2}, map[string]int{},
			twice, keptAtB, settledBetween, sameID),
			Kept: []store.OutcomeRow{key(settledBetween)}},
		{Node: "node-b", Counted: counted(map[string]int{"gitlab": 1}, map[string]int{"slack": 1},
			twice, laterSameID),
			Kept: []store.OutcomeRow{key(keptAtB)}},
		{Node: "node-c", Counted: counted(map[string]int{}, map[string]int{"slack": 2})},
	})
	// gitlab: a's 2 and b's 1 counted (b's being kept-at-b), plus twice once,
	// and same-id at both its instants; slack: b's 1 and c's 2, plus the row
	// settled between the questions.
	if want := map[string]int{"gitlab": 6}; !maps.Equal(got.Skipped, want) {
		t.Errorf("skipped = %v, want %v", got.Skipped, want)
	}
	if want := map[string]int{"slack": 4}; !maps.Equal(got.Coalesced, want) {
		t.Errorf("coalesced = %v, want %v", got.Coalesced, want)
	}
	if len(got.Unsettled) != 0 {
		t.Errorf("the merge still names %v, want every row counted", got.Unsettled)
	}
	if none := MergeOutcomes(nil); none.Skipped == nil || none.Coalesced == nil {
		t.Errorf("an empty merge is %+v, want two empty maps", none)
	}
}
