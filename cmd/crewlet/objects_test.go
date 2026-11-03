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
		files = append(files, collect.MissingFile{Object: k, NamedBy: fmt.Sprintf("ENG/lost-%d.md", i),
			Finding: collect.FindingMissing})
	}
	found := &collect.AuditFindings{At: objectsAt, Completed: true, Referenced: 1828,
		Missing: len(missing), MissingFiles: files}
	block := queries.RenderObjects(engine.CollectionReport{
		Node: "data-a", Backend: "s3:https://s3.example.com/files/acme/",
		Status: collect.Status{
			Collect: collect.CollectionReport{Completed: true, Listed: 1840, Deleted: 12,
				Abandoned: 1, At: objectsAt},
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
		"1840 objects listed, 12 deleted, 1 unfinished upload(s) abandoned",
		"1828 files named, 0 missing, 0 damaged, 0 unanswered"} {
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
	refused := objstore.KeyAt(objectsAt)
	block := queries.RenderObjects(engine.CollectionReport{
		Node: "data-a", Backend: "nats",
		Status: collect.Status{
			Collect: collect.CollectionReport{Listed: 1840, Deleted: 500, At: objectsAt,
				Skipped: "a record this node could not apply may refer to objects in the store"},
			Audit: collect.AuditReport{At: objectsAt, Error: "the bucket did not answer",
				Found: &collect.AuditFindings{At: objectsAt.Add(-24 * time.Hour), Completed: true,
					Referenced: 9, Damaged: 1, Unanswered: 1,
					MissingFiles: []collect.MissingFile{
						{Object: lost, NamedBy: "ENG/a.md", Finding: collect.FindingDamaged},
						{Object: refused, NamedBy: "ENG/b.md", Finding: collect.FindingUnanswered},
					}}},
		},
	})
	out, _, err := cli(t, "objects", "status", bootstrapForURL(t, serveObjects(t, &block).URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"500 deleted", "(stopped judging: a record this node",
		"9 files named, 0 missing, 1 damaged, 1 unanswered", "a later audit, at",
		"stopped: the bucket did not answer", "2 file(s) cannot be read",
		"ENG/a.md (damaged, object " + lost.String() + ")",
		"ENG/b.md (unanswered, object " + refused.String() + ")"} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "deleted nothing") {
		t.Errorf("a pass that deleted 500 objects says it deleted nothing:\n%s", out)
	}
}

// -json IS THE BLOCK AS THE NODE ANSWERED IT, in the engine's own shape: what
// it prints decodes back into the API's type with every value the node set. It
// is printed again rather than copied out as it came, and a field this build
// does not know survives that too ([TestEveryJSONOutputSpellsItsTextAsWritten]),
// because the block is decoded into a generic value, never into this build's
// idea of it.
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

// serveMigrate answers POST /objects/migrate with report, recording the
// query each call carried.
func serveMigrate(t *testing.T, report string, asked *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /objects/migrate", func(w http.ResponseWriter, r *http.Request) {
		*asked = append(*asked, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(report))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// MIGRATE NAMES THE TARGET ON THE ENGINE'S HOST AND SAYS WHAT HAPPENED: what
// was copied — the files and, apart, the objects derived rows name — what the
// old store could not give back, and what to do next.
func TestObjectsMigrateReportsThePassAndTheNextStep(t *testing.T) {
	t.Parallel()
	var asked []string
	srv := serveMigrate(t, `{"from":"nats","to":"s3:https://s3.example.com/files/acme/",
		"final":true,"sealed":true,"moved":true,"direct_upload":"available",
		"next":"Give every node the target file's store.objects block.",
		"pass":{"referenced":5,"present":2,"copied":2,"copied_bytes":2048,"unreadable":1,
		"complete":true,"unreadable_files":[{"object":"k","named_by":"ENG/gone.md"}],
		"derived":{"referenced":4,"present":1,"copied":2,"copied_bytes":4096,"unreadable":1}}}`, &asked)
	out, _, err := cli(t, "objects", "migrate", "-to", "s3", "-target", "/etc/crewlet/s3.yaml",
		"-final", bootstrapForURL(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "target=%2Fetc%2Fcrewlet%2Fs3.yaml&final=true" {
		t.Fatalf("the node was asked %v", asked)
	}
	for _, want := range []string{"the fleet's own NATS bucket", "an S3 bucket (https://s3.example.com/files/acme/)",
		"5 (2 already in the bucket, 2 copied", "ENG/gone.md (missing", "now keeps its files in the bucket",
		"Derived objects:   4 (1 already in the bucket, 2 copied",
		"1 derived object(s) could not be read", "nothing is lost", "sealed", "Give every node"} {
		if !strings.Contains(out, want) {
			t.Errorf("migrate does not say %q:\n%s", want, out)
		}
	}
}

// A TARGET THIS SHELL WOULD READ, OR NO DESTINATION AT ALL, IS REFUSED
// before the node is asked.
func TestObjectsMigrateRefusesWhatTheNodeCouldNotRead(t *testing.T) {
	t.Parallel()
	var asked []string
	srv := serveMigrate(t, `{}`, &asked)
	for name, args := range map[string][]string{
		"no -to":         {"-target", "/etc/crewlet/s3.yaml"},
		"another store":  {"-to", "gcs", "-target", "/etc/crewlet/s3.yaml"},
		"no target":      {"-to", "s3"},
		"a relative one": {"-to", "s3", "-target", "s3.yaml"},
	} {
		argv := append([]string{"objects", "migrate"}, args...)
		if _, _, err := cli(t, append(argv, bootstrapForURL(t, srv.URL))...); err == nil {
			t.Errorf("%s: migrate ran", name)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("refused commands reached the node: %v", asked)
	}
}
