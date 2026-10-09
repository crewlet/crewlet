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
// of the custody table would see.
//
// Mutation: skip the settle, and a batch is left unsettled; derive the rows
// by any other path, and the tables differ.
func TestWrittenEventsAreTheRowsAppendWrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	recs := []store.EventRecord{
		{ID: "phase", Type: "agent_phase_completed", Source: "agent", Time: at,
			Category: "agent", Actor: "lead", TraceID: "tr", SpanID: "sp",
			Payload: []byte(`{"phase":"execute","model":"m","input_tokens":1,` +
				`"output_tokens":2,"total_tokens":3,"turn_id":"tn"}`)},
		{ID: "assigned", Type: "task_assigned", Source: "pm", Time: at.Add(time.Second),
			Category: "task", Summary: "a task", WorkKey: "wk",
			Tags: map[string]string{"recipient": "dev", "target": "qa"}},
		{ID: "failed", Type: "tool_failed", Source: "agent", Time: at.Add(2 * time.Second),
			Category: "tool", Failed: true},
	}

	appended := storetest.OpenNode(t, filepath.Join(t.TempDir(), "appended.db"), store.Options{})
	defer func() { _ = appended.Close() }()
	for _, rec := range recs {
		if err := appended.Events().Append(ctx, rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}
	written := storetest.OpenNode(t, filepath.Join(t.TempDir(), "written.db"), store.Options{})
	defer func() { _ = written.Close() }()
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
	unsettled, err := written.Events().UnsettledCustody(ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("read the unsettled batches: %v", err)
	}
	if len(unsettled) != 0 {
		t.Errorf("WriteEvents left %d custody batch(es) unsettled: %v", len(unsettled), unsettled)
	}
}
