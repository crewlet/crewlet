package worklet_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// HOW LONG WORK STOOD IN EACH STATE IS READ OFF THE FLOW RECORD: each change
// stands until the item's next one, clipped to the window, summed per seat,
// state and cause with the items it covers; a change before the window counts
// from the window's start. Mutation: end a change at the window's end rather
// than at the next, and the due hours swallow the held ones.
func TestHowLongWorkStoodInEachStateIsReadOffTheFlowRecord(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileInProgress(t, r, "t-1", "ada", nil)
	fileInProgress(t, r, "t-2", "ada", nil)
	writer, err := worklet.NewWriter(worklet.WriterDeps{
		Publisher: r.publisher, DB: r.db.Reader(), NodeID: r.nodeID, Claims: r.claims,
		Actor: r.nodeID, ActorKind: worklet.AuthorSystem, Now: func() time.Time { return r.at },
	})
	if err != nil {
		t.Fatalf("system writer: %v", err)
	}
	from := time.Date(2030, 1, 7, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := from.Add(d); return &x }
	previous := map[string]string{}
	change := func(task string, state worklet.FlowState, cause worklet.FlowCause, since *time.Time) {
		t.Helper()
		id := worklet.FlowChangeID(task, state, cause, since, previous[task])
		previous[task] = id
		if _, err := writer.RecordFlow(t.Context(), "op-"+id, worklet.FlowRecord{ID: id, Task: task,
			State: state, Cause: cause, Since: since, Seat: "ada"}); err != nil {
			t.Fatalf("record %s %s: %v", task, state, err)
		}
		r.drain()
	}
	change("t-2", worklet.FlowHeld, worklet.FlowCauseLimit, at(-24*time.Hour))
	change("t-1", worklet.FlowDue, "", at(0))
	change("t-1", worklet.FlowHeld, worklet.FlowCauseLimit, at(2*time.Hour))
	change("t-1", worklet.FlowMoving, "", at(3*time.Hour))
	got, err := r.reader.FlowHours(t.Context(), worklet.FlowHoursQuery{Since: from, Until: from.Add(4 * time.Hour),
		Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	want := []worklet.FlowHours{
		{Seat: "ada", State: worklet.FlowHeld, Cause: worklet.FlowCauseLimit, Stood: 5 * time.Hour, Items: 2},
		{Seat: "ada", State: worklet.FlowDue, Stood: 2 * time.Hour, Items: 1},
		{Seat: "ada", State: worklet.FlowMoving, Stood: time.Hour, Items: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("flow hours %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	items, err := r.reader.FlowItems(t.Context(), worklet.FlowHoursQuery{Since: from, Until: from.Add(4 * time.Hour),
		Level: statelog.ReadStale})
	if err != nil || len(items) != 1 || items["ada"] != 2 {
		t.Errorf("items %v (%v), want ada's two", items, err)
	}
	if _, err := r.reader.FlowHours(t.Context(), worklet.FlowHoursQuery{Since: from, Until: from.Add(15 * 24 * time.Hour),
		Level: statelog.ReadStale}); err == nil {
		t.Error("a window past two weeks was read")
	}
}
