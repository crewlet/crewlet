package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
)

// failingRecord is a coordination store that will not give the record up.
type failingRecord struct{ err error }

func (f failingRecord) ObjectCollection(context.Context) ([]byte, bool, error) {
	return nil, false, f.err
}

// thisStore is the object store every node in these tests agreed at boot.
const thisStore = "nats"

// agreedOn is the fleet's record read by a node that agreed store at boot —
// the engine's one reader of it, as a node's Backends reads it.
type agreedOn struct {
	records engine.CollectionRecords
	store   string
}

func (a agreedOn) ObjectCollection(ctx context.Context) (engine.CollectionReport, bool, error) {
	return engine.ObjectCollection(ctx, a.records, a.store)
}

// fleetObjects asks the fleet question of a node that agreed thisStore at
// boot, over records, and returns its objects block as a client reads it.
func fleetObjects(t *testing.T, records engine.CollectionRecords) map[string]any {
	t.Helper()
	return fleetObjectsOf(t, records, thisStore)
}

// fleetObjectsOf is [fleetObjects] on a node that agreed store at boot.
func fleetObjectsOf(t *testing.T, records engine.CollectionRecords, store string) map[string]any {
	t.Helper()
	body := asMap(t, answer(t, queries.Sources{Coord: coordmemory.New(), NodeID: "data-a",
		Objects: agreedOn{records: records, store: store}}, "fleet", nil))
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
		record engine.CollectionRecords
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

// A REPORT OF ANOTHER STORE IS NO REPORT. The record names the store its
// passes ran on and outlives a move of the fleet's files — the documented move
// deletes the backend record beside it and leaves the report, and so does an
// external cluster restored whole and the record deleted. Rendered, it showed
// every node's card and `crewlet objects status` the OLD store as the one the
// files are in, with the old store's audit and lost files, until the new
// store's first pass recorded — and for as long as no data node took the duty.
func TestTheFleetShowsNoReportOfAnotherStore(t *testing.T) {
	t.Parallel()
	const oldStore = "s3:https://old.example.com/acme-files/crewlet/"
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	fleet := coordmemory.NewFleet()
	raw, err := json.Marshal(map[string]any{
		"node": "data-b", "backend": oldStore,
		"status": collect.Status{
			Collect: collect.CollectionReport{Completed: true, Listed: 40, At: at},
			Audit: collect.AuditReport{Completed: true, Referenced: 38, Missing: 1, At: at,
				Found: &collect.AuditFindings{At: at, Completed: true, Referenced: 38, Missing: 1,
					MissingFiles: []collect.MissingFile{{Object: objstore.KeyAt(at), NamedBy: "ENG/lost.md",
						Finding: collect.FindingMissing}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.RecordObjectCollection(t.Context(), raw); err != nil {
		t.Fatal(err)
	}

	if got := fleetObjects(t, fleet); got["state"] != "not_yet" || len(got) != 1 {
		t.Errorf("a node on %q renders the record of %q as %v; want only state not_yet",
			thisStore, oldStore, got)
	}
	// The control: a node on the store the record names reads it as the report it is.
	if got := fleetObjectsOf(t, fleet, oldStore); got["state"] != "reported" || got["backend"] != oldStore {
		t.Errorf("a node on %q renders that store's own record as %v", oldStore, got)
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
