package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
)

// objectsAt is the instant every fixture here is stamped at.
var objectsAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// serveObjects answers the fleet view with block — rendered by the API's OWN
// renderer where the caller hands it a report, so a fixture spelling this
// command's idea of the shape cannot agree with the command whatever a node
// sends. omit serves a fleet view with no objects key: a node that runs no
// object store.
func serveObjects(t *testing.T, block *queries.FleetObjects) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet", func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{"nodes": []any{}}
		if block != nil {
			body["objects"] = block
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func reported(missing ...objstore.Key) *queries.FleetObjects {
	var files []collect.MissingFile
	for i, k := range missing {
		files = append(files, collect.MissingFile{Object: k, NamedBy: fmt.Sprintf("ENG/lost-%d.md", i)})
	}
	found := &collect.AuditFindings{At: objectsAt, Completed: true, Referenced: 1828,
		Missing: len(missing), MissingFiles: files}
	block := queries.RenderObjects(engine.CollectionReport{
		Node: "data-a", Backend: "s3:https://s3.example.com/files/acme/",
		Status: collect.Status{
			Collect: collect.CollectionReport{Completed: true, Listed: 1840, Deleted: 12,
				Retired: 3, Abandoned: 1, At: objectsAt},
			Audit: collect.AuditReport{Completed: true, Referenced: 1828,
				Missing: len(missing), At: objectsAt, Found: found},
		},
	})
	return &block
}

// STATUS SAYS WHERE THE FILES ARE AND WHAT THE COLLECTOR FOUND, and names
// every file that cannot be read — the one thing an operator must act on.
func TestObjectsStatusNamesTheBackendAndWhatIsMissing(t *testing.T) {
	t.Parallel()
	out, _, err := cli(t, "objects", "status", bootstrapForURL(t, serveObjects(t, reported()).URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"an S3 bucket (https://s3.example.com/files/acme/)", "data-a",
		"1840 objects listed, 12 deleted, 3 chunk(s) of an earlier build retired, " +
			"1 unfinished upload(s) abandoned", "1828 files named, 0 missing, 0 damaged"} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not say %q:\n%s", want, out)
		}
	}
	lost := objstore.KeyAt(objectsAt)
	out, _, err = cli(t, "objects", "status",
		bootstrapForURL(t, serveObjects(t, reported(lost)).URL))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 file(s) cannot be read") ||
		!strings.Contains(out, "ENG/lost-0.md (missing, object "+lost.String()+")") {
		t.Errorf("status does not name the missing file:\n%s", out)
	}
}

// A FAILED AUDIT IS SAID BESIDE WHAT THE LAST WHOLE ONE FOUND, never in its
// place, and a collection that stopped part of the way says what it deleted
// first — never that it deleted nothing.
func TestObjectsStatusKeepsTheFindingsAndTheCountsOfAPassThatStopped(t *testing.T) {
	t.Parallel()
	lost := objstore.KeyAt(objectsAt)
	block := queries.RenderObjects(engine.CollectionReport{
		Node: "data-a", Backend: "nats",
		Status: collect.Status{
			Collect: collect.CollectionReport{Listed: 1840, Deleted: 500, At: objectsAt,
				Skipped: "a record this node could not apply may refer to objects in the store"},
			Audit: collect.AuditReport{At: objectsAt, Error: "the bucket did not answer",
				Found: &collect.AuditFindings{At: objectsAt.Add(-24 * time.Hour), Completed: true,
					Referenced: 9, Damaged: 1,
					MissingFiles: []collect.MissingFile{{Object: lost, NamedBy: "ENG/a.md", Damaged: true}}}},
		},
	})
	out, _, err := cli(t, "objects", "status", bootstrapForURL(t, serveObjects(t, &block).URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"500 deleted", "(stopped judging: a record this node",
		"9 files named, 0 missing, 1 damaged", "a later audit, at", "stopped: the bucket did not answer",
		"ENG/a.md (damaged, object " + lost.String() + ")"} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "deleted nothing") {
		t.Errorf("a pass that deleted 500 objects says it deleted nothing:\n%s", out)
	}
}

// -json IS THE BLOCK AS THE NODE ANSWERED IT: re-encoded, a field this build
// does not know would be lost.
func TestObjectsStatusJSONIsTheBlockAsAnswered(t *testing.T) {
	t.Parallel()
	out, _, err := cli(t, "objects", "status", "-json",
		bootstrapForURL(t, serveObjects(t, reported()).URL))
	if err != nil {
		t.Fatal(err)
	}
	var block queries.FleetObjects
	if err := json.Unmarshal([]byte(out), &block); err != nil {
		t.Fatalf("-json printed %q: %v", out, err)
	}
	if block.State != queries.ObjectsReported || block.ReportedObjects == nil || block.Node != "data-a" {
		t.Errorf("-json printed %+v", block)
	}
}

// EACH STATE THAT IS NOT A REPORT IS SAID APART, and a node with no object
// store is told to ask another rather than shown nothing.
func TestObjectsStatusNamesEachStateThatIsNotAReport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		block *queries.FleetObjects
		fails bool
		says  string
	}{
		{&queries.FleetObjects{State: queries.ObjectsNotYet}, false, "has not finished a pass"},
		{&queries.FleetObjects{State: queries.ObjectsUnavailable}, true, "could not be read"},
		{nil, true, "runs none"},
	} {
		out, _, err := cli(t, "objects", "status", bootstrapForURL(t, serveObjects(t, tc.block).URL))
		said := out
		if err != nil {
			said = err.Error()
		}
		if (err != nil) != tc.fails || !strings.Contains(said, tc.says) {
			t.Errorf("%+v: answered %q (err %v), want %q", tc.block, out, err, tc.says)
		}
	}
}
