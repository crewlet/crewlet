package store

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// A PAGE OF TURNS READ SPAN BY SPAN IS THE PAGE THE WHOLE WINDOW LISTS.
//
// The newest page is read from its top edge back ([EventLog.pageOfTurns]), so
// a seat's newest turns cost their own rows rather than every row the seat
// wrote that week. That is only sound if a span that fills the page holds
// exactly the page the whole window would: a turn whose folded rows began
// before the span, and so is cut off at its edge, must be left to a wider span
// and ranked there by its true start, while a turn whose seat rows all lie in
// the span is listed even when the turn holds an earlier row of another seat.
//
// Mutation: drop the span's own probe from [TurnQuery.partialsSQL] and the turn
// running since before the first span is listed by its later rows; ask it of
// any row rather than of the rows the fold reads ([TurnQuery.rowWhere]) and
// the turn led by another seat's row drops off the seat's page.
func TestAPageReadSpanBySpanIsTheWholeWindowsPage(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := db.Events()
	at := now()

	write := func(turn, seat string, offsets ...time.Duration) {
		t.Helper()
		for i, off := range offsets {
			rec := EventRecord{
				ID: fmt.Sprintf("%s-%d", turn, i), Type: "agent_phase_completed",
				Time: at.Add(-off), Category: "lifecycle", Actor: seat,
				Tags: map[string]string{"turn_id": turn, "agent_id": seat, "work_key": "wk-" + turn},
			}
			if i == len(offsets)-1 && strings.HasSuffix(turn, "failed") {
				rec.Tags["failed"] = "true"
			}
			if err := log.Append(t.Context(), rec); err != nil {
				t.Fatalf("append %s: %v", rec.ID, err)
			}
		}
	}
	write("a-old", "id-a", 6*24*time.Hour, 6*24*time.Hour-time.Minute)
	write("a-failed", "id-a", 2*24*time.Hour, 2*24*time.Hour-time.Minute)
	// RUNNING SINCE BEFORE THE FIRST SPAN, with a row inside it.
	write("a-long", "id-a", 3*time.Hour, 2*time.Hour, 10*time.Minute)
	write("a-mid1", "id-a", 50*time.Minute)
	write("a-mid2", "id-a", 40*time.Minute)
	// LED BY ANOTHER SEAT'S ROW before the first span, its seat's rows in it.
	write("a-lead", "id-b", 90*time.Minute)
	write("a-lead", "id-a", 20*time.Minute, 19*time.Minute)
	write("a-new", "id-a", 5*time.Minute)
	write("b-1", "id-b", 30*time.Hour)
	write("b-2", "id-b", 3*time.Minute)

	failed := true
	for _, q := range []TurnQuery{
		{AgentID: "id-a", Limit: 1},
		{AgentID: "id-a", Limit: 2},
		{AgentID: "id-a", Limit: 3},
		{AgentID: "id-a", Limit: 50},
		{AgentID: "id-a", Limit: 2, Before: &TurnCursor{Start: at.Add(-40 * time.Minute), TurnID: "a-mid2"}},
		{AgentID: "id-a", Limit: 2, Until: at.Add(-30 * time.Minute)},
		{AgentID: "id-a", Limit: 2, Since: at.Add(-4 * time.Hour)},
		{AgentID: "id-a", Limit: 1, Failed: &failed},
		{AgentID: "id-a", WorkKey: "wk-a-lead", Limit: 1},
		{Limit: 2},
		{Limit: 50},
	} {
		q.At = at
		got, more, err := log.TurnPartials(t.Context(), q)
		if err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
		probe := q.Limit + 1
		whole, err := turnPartials(t.Context(), db.sql, q, readPage, probe, time.Time{})
		if err != nil {
			t.Fatalf("%+v over the whole window: %v", q, err)
		}
		want, wantMore := pastPage(whole, q.Limit)
		if !reflect.DeepEqual(got, want) || more != wantMore {
			t.Errorf("%+v: the page read span by span is %v (more %v), the whole window's "+
				"%v (more %v)", q, turnIDs(got), more, turnIDs(want), wantMore)
		}
	}

	// AND THE FIRST SPAN ALONE ANSWERS A PAGE IT FILLS, which is the read the
	// span exists to save: the turn running since before it is not in it, and
	// is listed from a wider span by its first row.
	q := TurnQuery{AgentID: "id-a", At: at}
	first, err := turnPartials(t.Context(), db.sql, q, readPage, 4, at.Add(-firstTurnSpan))
	if err != nil {
		t.Fatalf("the first span: %v", err)
	}
	if ids := turnIDs(first); !slices.Equal(ids, []string{"a-new", "a-lead", "a-mid2", "a-mid1"}) {
		t.Errorf("the first span lists %v, want the four turns whose seat rows all lie in it", ids)
	}
	all, _, err := log.TurnPartials(t.Context(), TurnQuery{AgentID: "id-a", Limit: 50, At: at})
	if err != nil {
		t.Fatalf("the whole window: %v", err)
	}
	for _, p := range all {
		if p.TurnID == "a-long" && !p.StartedAt.Equal(at.Add(-3*time.Hour).Truncate(time.Microsecond)) {
			t.Errorf("a-long starts at %s, want its first row's %s", p.StartedAt, at.Add(-3*time.Hour))
		}
	}
}

// A PAGE READS THE SPANS IT NEEDS AND NO MORE: one span when the newest fills
// it, doubling from the top edge otherwise, and the whole window last, at the
// window's own floor, when none does.
//
// Mutation: start the walk at the floor, or skip the read at the floor, and the
// spans read differ from these.
func TestAPageReadsBackOnlyAsFarAsItsPage(t *testing.T) {
	t.Parallel()
	top := now()
	floor := top.Add(-7 * 24 * time.Hour)
	walk := func(fillsAt int) []time.Duration {
		t.Helper()
		var spans []time.Duration
		err := spansBack(top, floor, func(from time.Time) (bool, error) {
			spans = append(spans, top.Sub(from)) //fixedwait:stated top and the span's start computed from it
			return len(spans) == fillsAt, nil
		})
		if err != nil {
			t.Fatalf("spansBack: %v", err)
		}
		return spans
	}
	if got := walk(1); !slices.Equal(got, []time.Duration{firstTurnSpan}) {
		t.Errorf("a page the first span fills read %v, want only that span", got)
	}
	if got := walk(3); !slices.Equal(got, []time.Duration{firstTurnSpan, 2 * firstTurnSpan, 4 * firstTurnSpan}) {
		t.Errorf("a page the third span fills read %v, want three doubling spans", got)
	}
	got := walk(0)
	if last := got[len(got)-1]; last != top.Sub(floor) { //fixedwait:stated top and the floor computed from it
		t.Errorf("a page no span fills ended at %s back, want the window's floor %s back",
			last, top.Sub(floor)) //fixedwait:stated top and the floor computed from it
	}
	for i := 1; i < len(got)-1; i++ {
		if got[i] != 2*got[i-1] {
			t.Errorf("the spans %v do not double", got)
		}
	}
}

// A PAGE'S SPAN IS A RANGE OF THE INDEX IT SEEKS, so the span bounds the rows a
// page reads: the seat's index sought from the span's start, and each turn's
// earlier rows probed on the turn index rather than scanned.
//
// Mutation: read a span's rows from the window's floor and its seek names the
// floor alone, leaving the span a filter over the whole window's rows.
func TestAPageReadFromASpanSeeksOnlyTheSpan(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	at := now()
	from := at.Add(-firstTurnSpan)
	for _, c := range []struct {
		name  string
		q     TurnQuery
		index string
	}{
		{"by seat", TurnQuery{AgentID: "id-1"}, "crewlet_events_agent_id_time_idx"},
		{"by unit of work", TurnQuery{WorkKey: "wk-1"}, "crewlet_events_work_key_idx"},
		{"the company's", TurnQuery{}, ""},
	} {
		query, args := c.q.partialsSQL(at, readPage, DefaultTurnPage+1, from)
		plan := planOf(t, db, query, args)
		if c.index != "" && !strings.Contains(plan, c.index) {
			t.Errorf("%s: the plan is %q: it does not seek %s", c.name, plan, c.index)
		}
		if strings.Contains(plan, "MULTI-INDEX") {
			t.Errorf("%s: the plan is %q: it intersects the primary key's range", c.name, plan)
		}
		if !strings.Contains(plan, "SEARCH older USING") || !strings.Contains(plan, "crewlet_events_turn_idx") {
			t.Errorf("%s: the plan is %q: it does not probe a turn's earlier rows on "+
				"the turn index", c.name, plan)
		}
		// THE SPAN'S START IS THE ROWS' LOWER BOUND, bound before the floor
		// the probes are asked under, so the range the seek reads is the span.
		floor, _ := c.q.Window(at)
		if s, f := slices.Index(args, any(EncodeTime(from))), slices.Index(args, any(EncodeTime(floor))); s < 0 || s > f {
			t.Errorf("%s: the rows are read from argument %d, the floor is argument %d of %v: "+
				"want the span's start first, as the rows' own bound", c.name, s, f, args)
		}
	}
}

// turnIDs is the ids of some partials, in order.
func turnIDs(parts []TurnPartial) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.TurnID)
	}
	return out
}
