package memread_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/learning/memread"
)

// rowOf finds one seat's row of an overview.
func rowOf(t *testing.T, o memread.Overview, handle string) memread.OverviewSeat {
	t.Helper()
	for _, row := range o.Seats {
		if row.Handle == handle {
			return row
		}
	}
	t.Fatalf("the overview has no row for %s: %+v", handle, o.Seats)
	return memread.OverviewSeat{}
}

// EVERY SEAT'S TOTALS ARE ITS HOLDER'S, gathered in ONE scatter.
//
// Three seats on three footings: @swe held by a peer (node-a), @pm held by the
// asker (node-b), @ops held by nobody. node-b also keeps a stale copy of
// @swe's memory from an earlier tenure. The overview counts @swe from node-a's
// store and @pm from node-b's, sends @ops with `held_by: none` and nothing
// counted, carries each seat's newest note, and asks the broker ONCE whatever
// the seat count. Answered locally (the mutation), @swe comes back with
// node-b's two stale notes.
func TestTheOverviewCountsEverySeatAtItsHolder(t *testing.T) {
	t.Parallel()
	f := newFleet()
	a, b := newNode(t, "node-a:1"), newNode(t, "node-b:1")
	a.remember(t, "swe", "current", 60) // past a memory page of fifty
	b.remember(t, "swe", "stale", 2)
	b.remember(t, "pm", "mine", 3)
	a.remember(t, "ops", "orphan", 4)
	f.hold(t, "swe", a.owner)
	f.hold(t, "pm", b.owner)
	f.reader(t, a, "swe")
	read := f.reader(t, b, "pm")
	asks := &counting{Asker: read.Queue}
	read.Queue = asks

	got, err := read.Overview(t.Context(), []string{"ops", "pm", "swe"})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if asks.asked != 1 {
		t.Errorf("asked the broker %d times, want once for every held seat together", asks.asked)
	}
	if len(got.Seats) != 3 || got.Seats[0].Handle != "ops" || got.Seats[2].Handle != "swe" {
		t.Errorf("rows %+v, want one per seat in the order asked", got.Seats)
	}
	swe := rowOf(t, got, "swe")
	if swe.HeldBy != "node-a" || swe.DiaryTotal != 60 || swe.EpisodesTotal != 60 {
		t.Errorf("@swe = %+v, want node-a's 60 notes and 60 episodes — the TOTAL, not a page", swe)
	}
	if swe.LatestReflection == nil || !strings.HasPrefix(swe.LatestReflection.Content, "current note 59") {
		t.Errorf("@swe's latest reflection = %+v, want node-a's newest note", swe.LatestReflection)
	}
	if swe.LastReflectionAt != swe.LatestReflection.CreatedAt || swe.LastReflectionAt == "" {
		t.Errorf("last_reflection_at %q, want the newest note's instant", swe.LastReflectionAt)
	}
	pm := rowOf(t, got, "pm")
	if pm.HeldBy != "node-b" || pm.DiaryTotal != 3 || pm.Unavailable != "" {
		t.Errorf("@pm = %+v, want node-b's own 3", pm)
	}
	ops := rowOf(t, got, "ops")
	if ops.HeldBy != memread.HolderNone || ops.DiaryTotal != 0 || ops.LatestReflection != nil {
		t.Errorf("@ops = %+v, want held by none and nothing counted from a copy nobody keeps current", ops)
	}
	if !got.Coverage.Complete || len(got.Coverage.Nodes) != 2 {
		t.Errorf("coverage %+v, want node-a and node-b, complete", got.Coverage)
	}
}

// A HOLDER THAT DID NOT ANSWER IS NAMED, and its seats say so rather than zero.
//
// @swe is held by node-c, whose build answers and which serves nothing. The
// overview comes back — the asker's own seat counted — with @swe unavailable,
// node-c missing from the coverage with the budget it did not answer inside,
// and the answer not complete. A row of zeros there would read as a seat that
// remembers nothing.
func TestTheOverviewNamesAHolderThatDidNotAnswer(t *testing.T) {
	t.Parallel()
	f := newFleet()
	b := newNode(t, "node-b:1")
	b.remember(t, "pm", "mine", 2)
	f.hold(t, "swe", "node-c:9")
	f.hold(t, "pm", b.owner)
	f.present(t, "node-c:9", coord.FeatureHeldRead)
	read := f.reader(t, b, "pm")

	got, err := read.Overview(t.Context(), []string{"pm", "swe"})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if got.Coverage.Complete {
		t.Error("coverage complete with node-c silent")
	}
	missing := got.Coverage.Missing()
	if len(missing) != 1 || missing[0] != "node-c" {
		t.Fatalf("missing %v, want node-c", missing)
	}
	for _, n := range got.Coverage.Nodes {
		if n.ID == "node-c" && !strings.Contains(n.Error, "budget") {
			t.Errorf("node-c's reason %q, want the budget it did not answer inside", n.Error)
		}
	}
	swe := rowOf(t, got, "swe")
	if swe.HeldBy != "node-c" || swe.Unavailable == "" {
		t.Errorf("@swe = %+v, want held by node-c and unavailable", swe)
	}
	if pm := rowOf(t, got, "pm"); pm.DiaryTotal != 2 {
		t.Errorf("@pm = %+v — one silent holder must not cost the rest", pm)
	}
}

// A HOLDER ON AN OLDER BUILD IS NAMED AT ONCE AND NEVER ASKED, as a single read
// names one.
func TestTheOverviewNamesAnOlderHolderWithoutAsking(t *testing.T) {
	t.Parallel()
	f := newFleet()
	b := newNode(t, "node-b:1")
	f.hold(t, "swe", "node-c:9")
	f.present(t, "node-c:9") // advertises nothing
	read := f.reader(t, b)
	read.Budget = 10 * time.Second
	asks := &counting{Asker: read.Queue}
	read.Queue = asks

	start := time.Now()
	got, err := read.Overview(t.Context(), []string{"swe"})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if asks.asked != 0 {
		t.Errorf("asked the broker %d times — a build that cannot answer is not asked", asks.asked)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s — the answer is known without waiting", took)
	}
	if swe := rowOf(t, got, "swe"); !strings.Contains(swe.Unavailable, "older build") {
		t.Errorf("@swe = %+v, want it named as held on an older build", swe)
	}
	if got.Coverage.Complete {
		t.Error("coverage complete with node-c unable to answer")
	}
}

// A SEAT STILL ARRIVING IS NOT COUNTED, on the asker or on a peer.
//
// node-a holds @swe and node-b holds @pm, and neither has attached its seat
// yet. Each row says it is still arriving — the count from a copy that is
// still being hydrated is short — and both nodes still answered.
func TestTheOverviewDoesNotCountASeatStillArriving(t *testing.T) {
	t.Parallel()
	f := newFleet()
	a, b := newNode(t, "node-a:1"), newNode(t, "node-b:1")
	a.remember(t, "swe", "arriving", 1)
	b.remember(t, "pm", "arriving", 1)
	f.hold(t, "swe", a.owner)
	f.hold(t, "pm", b.owner)
	f.reader(t, a)
	read := f.reader(t, b)

	got, err := read.Overview(t.Context(), []string{"pm", "swe"})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	for _, h := range []string{"pm", "swe"} {
		row := rowOf(t, got, h)
		if row.Unavailable == "" || row.DiaryTotal != 0 {
			t.Errorf("@%s = %+v, want unavailable and uncounted while it arrives", h, row)
		}
	}
	if !got.Coverage.Complete {
		t.Errorf("coverage %+v — both nodes answered, each about a seat it is taking", got.Coverage)
	}
}

// A NODE WITH NO BROKER COUNTS EVERY SEAT ITSELF, and says it answered.
func TestTheOverviewOfANodeWithNoBrokerIsItsOwn(t *testing.T) {
	t.Parallel()
	b := newNode(t, "solo:1")
	b.remember(t, "swe", "only", 3)
	read := &memread.Reader{Owner: b.owner, Local: b.stores}

	got, err := read.Overview(t.Context(), []string{"swe"})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if swe := rowOf(t, got, "swe"); swe.HeldBy != "solo" || swe.DiaryTotal != 3 {
		t.Errorf("@swe = %+v, want solo's 3", swe)
	}
	if !got.Coverage.Complete || len(got.Coverage.Nodes) != 1 {
		t.Errorf("coverage %+v, want solo alone, complete", got.Coverage)
	}
}
