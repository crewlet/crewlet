package storetest_test

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// WRITTEN IN ONE TRANSACTION, STORED AS APPEND STORES THEM, and nothing left
// unsettled.
//
// A fixture seeded through WriteEvents is a fixture for the readers Append's
// rows feed, so the two must leave the same rows — the log's own and the
// parties index, every derived column included — and the custody batch the
// write rides on must not outlive it: an unsettled batch is a state a reader
// of the custody table would see. The parties are AGENT IDS, the event's own
// and each seat a participant tag names resolved through the seats the
// handle was given, so both handles are given the same seats.
//
// Mutation: skip the settle, and a batch is left unsettled; derive the rows
// by any other path — the parties resolved without the handle's seats among
// them — and the tables differ.
func TestWrittenEventsAreTheRowsAppendWrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	recs := []store.EventRecord{
		{ID: "phase", Type: "agent_phase_completed", Source: "agent", Time: at,
			Category: "agent", Actor: "lead", TraceID: "tr", SpanID: "sp",
			Tags: map[string]string{"agent_id": "id-lead"},
			Payload: []byte(`{"phase":"execute","model":"m","input_tokens":1,` +
				`"output_tokens":2,"total_tokens":3,"turn_id":"tn"}`)},
		{ID: "assigned", Type: "task_assigned", Source: "pm", Time: at.Add(time.Second),
			Category: "task", Summary: "a task", WorkKey: "wk",
			Tags: map[string]string{"recipient": "dev", "target": "qa"}},
		{ID: "failed", Type: "tool_failed", Source: "agent", Time: at.Add(2 * time.Second),
			Category: "tool", Failed: true},
	}

	seats := func(handle string) (string, bool) {
		id, ok := map[string]string{"dev": "id-dev", "qa": "id-qa"}[handle]
		return id, ok
	}
	appended := storetest.OpenNode(t, filepath.Join(t.TempDir(), "appended.db"), store.Options{})
	defer func() { _ = appended.Close() }()
	appended.SetEventSeats(seats)
	for _, rec := range recs {
		if err := appended.Events().Append(ctx, rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}
	written := storetest.OpenNode(t, filepath.Join(t.TempDir(), "written.db"), store.Options{})
	defer func() { _ = written.Close() }()
	written.SetEventSeats(seats)
	storetest.WriteEvents(t, written.Events(), recs)

	for _, table := range []string{"crewlet_events", "crewlet_event_parties"} {
		want := rowsOf(t, appended, table)
		if len(want) == 0 {
			t.Fatalf("Append left %s empty, so this case compares nothing", table)
		}
		if got := rowsOf(t, written, table); !slices.Equal(got, want) {
			t.Errorf("WriteEvents left %s as\n%v\nwhere Append leaves\n%v", table, got, want)
		}
	}
	unsettled, err := written.Events().UnsettledCustody(ctx, time.Now().Add(time.Hour), store.UnsettledBatch{}, 10)
	if err != nil {
		t.Fatalf("read the unsettled batches: %v", err)
	}
	if len(unsettled) != 0 {
		t.Errorf("WriteEvents left %d custody batch(es) unsettled: %v", len(unsettled), unsettled)
	}
}
