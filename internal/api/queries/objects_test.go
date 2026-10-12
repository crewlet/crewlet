package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore/collect"
)

// failingRecord is a coordination store that will not give the record up.
type failingRecord struct{ err error }

func (f failingRecord) ObjectCollection(context.Context) ([]byte, bool, error) {
	return nil, false, f.err
}

// fleetObjects asks the fleet question and returns its objects block as a
// client reads it.
func fleetObjects(t *testing.T, record queries.ObjectsReader) map[string]any {
	t.Helper()
	body := asMap(t, answer(t, queries.Sources{Coord: coordmemory.New(), NodeID: "data-a",
		Objects: record}, "fleet", nil))
	objects, _ := body["objects"].(map[string]any)
	if objects == nil {
		t.Fatalf("no objects in %v", body)
	}
	return objects
}

// THE FLEET VIEW SHOWS WHERE THE COMPANY'S FILES ARE AND WHAT THE COLLECTOR
// FOUND, from the fleet's record — so every node answers the same whichever a
// request reached, and names the node that ran the pass.
func TestTheFleetShowsWhatTheCollectorFound(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	fleet := coordmemory.NewFleet()
	raw, err := json.Marshal(map[string]any{
		"node": "data-b", "backend": "nats",
		"status": collect.Status{
			Collect: collect.CollectionReport{Completed: true, Listed: 40, Deleted: 2, At: at},
			Audit:   collect.AuditReport{Completed: true, Referenced: 38, At: at},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.RecordObjectCollection(t.Context(), raw); err != nil {
		t.Fatal(err)
	}
	got := fleetObjects(t, fleet)
	collected, _ := got["collect"].(map[string]any)
	audited, _ := got["audit"].(map[string]any)
	if got["state"] != "reported" || got["backend"] != "nats" || got["node"] != "data-b" ||
		collected["deleted"] != float64(2) || audited["referenced"] != float64(38) ||
		audited["missing"] != float64(0) {
		t.Fatalf("objects = %v", got)
	}
}

// THE STATES ARE NAMED APART, never folded into an empty report — a store
// that would not answer, and a fleet whose collector has not reported — and
// neither carries a report's counts, whose zeros would read as readings.
func TestTheFleetNamesEachStateOfTheRecordApart(t *testing.T) {
	t.Parallel()
	down := coordmemory.NewFleet()
	for name, tc := range map[string]struct {
		record queries.ObjectsReader
		want   string
	}{
		"a store that would not answer": {failingRecord{errors.New("down")}, "unavailable"},
		"a fleet with no report yet":    {down, "not_yet"},
	} {
		got := fleetObjects(t, tc.record)
		if got["state"] != tc.want || len(got) != 1 {
			t.Errorf("%s: objects = %v, want only state %q", name, got, tc.want)
		}
		if !queries.ObjectsState(tc.want).Valid() {
			t.Errorf("%s: %q is not a state this build declares", name, tc.want)
		}
	}
	if queries.ObjectsState("gone").Valid() {
		t.Error("an undeclared state reads valid")
	}
}

// AND A SURFACE WITH NO RECORD TO READ CARRIES NO BLOCK, rather than one
// reporting a collector that never ran.
func TestNoRecordReaderNoObjectsBlock(t *testing.T) {
	t.Parallel()
	body := asMap(t, answer(t, queries.Sources{Coord: coordmemory.New(), NodeID: "data-a"}, "fleet", nil))
	if _, present := body["objects"]; present {
		t.Error("a surface with no record reader reported an object store anyway")
	}
}
