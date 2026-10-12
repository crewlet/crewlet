package api_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// objectsGolden is the fleet view's object-store block in each of its states,
// committed, and read by the dashboard's own suite as its fixture.
const objectsGolden = "testdata/objects_answer.json"

// objectsAt is the instant every fixture's passes are stamped at, so the
// golden is stable.
var objectsAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// renderObjectsScenarios is the golden's content: the block in each of its
// states, each rendered by the fleet question's own renderer.
func renderObjectsScenarios(t *testing.T) []byte {
	t.Helper()
	// A FIXED KEY, so the golden does not move with the random bits a
	// minted one carries.
	lost, err := objstore.ParseKey("0199b4f6-0000-7000-8000-00000000a1b2")
	if err != nil {
		t.Fatal(err)
	}
	bent, err := objstore.ParseKey("0199b4f6-0000-7000-8000-00000000c3d4")
	if err != nil {
		t.Fatal(err)
	}
	audited := objectsAt.Add(-3 * time.Hour)
	clean := engine.CollectionReport{
		Node: "data-a", Backend: "s3:https://s3.example.com/files/acme/",
		Status: collect.Status{
			Collect: collect.CollectionReport{Completed: true, Listed: 1840, Aged: 1702,
				Deleted: 12, Referenced: 1690, Abandoned: 1, At: objectsAt},
			Audit: collect.AuditReport{Completed: true, Referenced: 1828, At: audited,
				Found: &collect.AuditFindings{At: audited, Completed: true, Referenced: 1828}},
		},
	}
	findings := &collect.AuditFindings{At: audited, Completed: true, Referenced: 1828,
		Missing: 1, Damaged: 1, MissingFiles: []collect.MissingFile{
			{Object: lost, NamedBy: "ENG/reports/q3 plan.md"},
			{Object: bent, NamedBy: "OPS/runbooks/restore.md", Damaged: true},
		}}
	missing := clean
	missing.Backend = "nats"
	missing.Status.Audit = collect.AuditReport{Completed: true, Referenced: 1828,
		Missing: 1, Damaged: 1, At: audited, Found: findings}
	// AN AUDIT THAT FAILED AFTER ONE THAT FOUND SOMETHING: the attempt and
	// its error, and the findings it did not replace.
	failed := missing
	failed.Status.Audit = collect.AuditReport{Referenced: 412, At: objectsAt,
		Error: "objstore/collect: ask the store for a key: the bucket did not answer",
		Found: findings}
	skipped := clean
	skipped.Status.Collect = collect.CollectionReport{Listed: 1840, Aged: 1702, Deleted: 500,
		Skipped:    "a record this node could not apply may refer to objects in the store",
		SweepError: "s3obj: list the uploads under \"acme/\": AccessDenied",
		At:         objectsAt}
	skipped.Status.Audit = collect.AuditReport{}
	blocks := map[string]queries.FleetObjects{
		"reported":    queries.RenderObjects(clean),
		"missing":     queries.RenderObjects(missing),
		"failed":      queries.RenderObjects(failed),
		"skipped":     queries.RenderObjects(skipped),
		"unavailable": {State: queries.ObjectsUnavailable},
		"not_yet":     {State: queries.ObjectsNotYet},
	}
	out, err := json.MarshalIndent(map[string]any{"fleet": blocks}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// THE OBJECTS BLOCK IS WHAT ITS GOLDEN FILE SAYS, for the gate answer's
// reason: a screen whose suite types its own fixtures agrees with itself
// whatever the engine sends. A change to the rendering is a failing test
// until the golden is regenerated (`make objects-answer`) and a failing
// Vitest suite until the Nodes screen follows it.
func TestTheObjectsAnswerMatchesItsGoldenFile(t *testing.T) {
	t.Parallel()
	got := renderObjectsScenarios(t)
	if os.Getenv("CREWLET_REGENERATE_OBJECTS_ANSWER") == "1" {
		if err := os.MkdirAll(filepath.Dir(objectsGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(objectsGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(objectsGolden)
	if err != nil {
		t.Fatalf("read %s: %v — `make objects-answer` writes it", objectsGolden, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the objects answer no longer matches %s. Run `make objects-answer`, "+
			"read the diff, and follow it in the dashboard's Nodes screen — its suite "+
			"loads this file — and in docs/reference/api-endpoints.md's GET /fleet "+
			"example, which is held to it.\n--- rendered now ---\n%s", objectsGolden, got)
	}
}

// THE REFERENCE'S EXAMPLE IS THE RENDERER'S OWN ANSWER: the GET /fleet
// example's `objects` block is held to what the renderer writes, so a script
// or an alert built from the page reads the first real answer right.
func TestTheReferenceExampleIsTheRenderersOwnAnswer(t *testing.T) {
	t.Parallel()
	page, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "reference",
		"api-endpoints.md"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Fleet map[string]any `json:"fleet"`
	}
	if err := json.Unmarshal(renderObjectsScenarios(t), &golden); err != nil {
		t.Fatal(err)
	}
	fleet := exampleUnder(t, string(page), "### `GET /fleet`")
	if got := fleet["objects"]; !reflect.DeepEqual(got, golden.Fleet["reported"]) {
		t.Errorf("the GET /fleet example's objects block is not what the renderer writes."+
			"\n--- documented ---\n%s\n--- rendered ---\n%s",
			indent(t, got), indent(t, golden.Fleet["reported"]))
	}
}

// THE DASHBOARD KNOWS EVERY STATE THE OBJECTS BLOCK IS SENT IN, and no other:
// a state the engine sends and the card does not name falls to its default
// arm and draws as a report with nothing in it, and one the card names that
// the engine never sends is a branch nothing reaches.
func TestTheDashboardKnowsEveryObjectsState(t *testing.T) {
	t.Parallel()
	var states []string
	for _, s := range queries.ObjectsStates() {
		if !s.Valid() {
			t.Fatalf("ObjectsStates lists %q, which Valid refuses", s)
		}
		states = append(states, string(s))
	}
	holdStrings(t, clientsource.Tree(t), "OBJECTS_STATES", states, "objects state")
}

// exampleUnder is the first JSON example in the section a heading opens, decoded.
func exampleUnder(t *testing.T, page, heading string) map[string]any {
	t.Helper()
	at := strings.Index(page, "\n"+heading+"\n")
	if at < 0 {
		t.Fatalf("the reference has no section %q", heading)
	}
	section := page[at+len(heading)+2:]
	if next := strings.Index(section, "\n### "); next >= 0 {
		section = section[:next]
	}
	open := strings.Index(section, "```json\n")
	if open < 0 {
		t.Fatalf("the section %q has no JSON example", heading)
	}
	body := section[open+len("```json\n"):]
	body = body[:strings.Index(body, "\n```")]
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the example under %q is not JSON: %v", heading, err)
	}
	return out
}

func indent(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
