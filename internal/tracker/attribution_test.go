package tracker_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/tracker"
)

// TestAttributionFieldsAreTheEngines holds the dashboard's field-name list
// against the names [tracker.TaskDeltas] actually writes.
//
// THE FAILURE IS SILENT IN BOTH DIRECTIONS, which is the whole reason for a
// gate. A properties rail asks "who set `estimate`" and gets nothing back when
// the engine records it under another name — no error, no warning, the line
// simply never appears — and the client's own first attempt asked for
// `estimate_minutes`, which is what the TASK ROW calls the same number. In the
// other direction, a field added to `TaskDeltas` is a property the rail could
// now attribute and does not, which nothing else would ever report.
//
// The client list may be a SUBSET — a delta the rail has no row for is not a
// defect — so what is asserted is that every name it uses is one the engine
// writes, plus a named report of the ones it has not taken up.
func TestAttributionFieldsAreTheEngines(t *testing.T) {
	engine := deltaFields(t)

	body, err := clientsource.Declaration(clientsource.Tree,
		`(?s)export const CHANGE_FIELDS = \[(.*?)\] as const`)
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	slices.Sort(client)
	if len(client) == 0 {
		t.Fatal("the dashboard attributes no fields, so this gate certifies nothing")
	}

	for _, name := range client {
		if !slices.Contains(engine, name) {
			t.Errorf("the dashboard attributes %q, which tracker.TaskDeltas never "+
				"writes: the rail's row for it can never carry a line, and nothing "+
				"reports that. The engine writes %v", name, engine)
		}
	}
	var missing []string
	for _, name := range engine {
		if !slices.Contains(client, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Logf("the engine records %v, which the dashboard does not attribute — "+
			"not a failure, but a property whose row could say who set it", missing)
	}
}

// deltaFields is every field name TaskDeltas writes, observed rather than
// listed: two tasks differing in every field, and the keys that come back.
//
// WITH A CATALOGUE, because one of the names is only reachable with one: a
// task's custom-field values are keyed by field id and are named by SLUG or
// not at all, so a nil map here would hide `fields` from a gate whose whole
// job is to report the complete set.
func deltaFields(t *testing.T) []string {
	t.Helper()
	before, after := twoDifferentTasks()
	moved := tracker.TaskDeltas(before, after, map[string]tracker.FieldDef{
		"f-sev": {ID: "f-sev", Slug: "severity", Type: tracker.FieldText},
	})
	if len(moved) == 0 {
		t.Fatal("two tasks differing in every field produced no deltas, so this " +
			"gate is comparing the client against an empty list")
	}
	names := make([]string, 0, len(moved))
	for name := range moved {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// twoDifferentTasks is one task and another differing in every field
// [tracker.TaskDeltas] compares, so the set of names it returns is the
// COMPLETE set rather than whatever a fixture happened to move.
func twoDifferentTasks() (tracker.Task, tracker.Task) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	due := time.Date(2026, 3, 8, 17, 0, 0, 0, time.UTC)
	later := due.Add(24 * time.Hour)
	earlier := start.Add(-24 * time.Hour)
	oldParent, newParent := "t-old-parent", "t-new-parent"
	cascadeRoot := "t-root"
	before := tracker.Task{
		Title:           "Before",
		Body:            "was",
		Status:          tracker.Status("todo"),
		Assignee:        "ada",
		Reporter:        "cy",
		Priority:        tracker.Priority("low"),
		Project:         "ENG",
		RoutingUnit:     "Engineering",
		Parent:          &oldParent,
		Type:            "task",
		Tags:            []string{"one"},
		Watchers:        []string{"ada"},
		Muted:           []string{"ada"},
		Collaborators:   []string{"ada"},
		Dependents:      []string{"t-1"},
		Relations:       edgesTo("t-2"),
		DueAllDay:       true,
		Checklists:      []tracker.Checklist{{ID: "c-1", Name: "Setup"}},
		Fields:          map[string]json.RawMessage{"f-sev": json.RawMessage(`"low"`)},
		StartAt:         &start,
		DueAt:           &due,
		EstimateMinutes: 30,
		Points:          1,
	}
	after := tracker.Task{
		Title:         "After",
		Body:          "is now",
		Status:        tracker.Status("in_progress"),
		Assignee:      "bo",
		Reporter:      "di",
		Priority:      tracker.Priority("high"),
		Project:       "OPS",
		RoutingUnit:   "Platform",
		Parent:        &newParent,
		Type:          "bug",
		Tags:          []string{"two"},
		Watchers:      []string{"bo"},
		Muted:         []string{"bo"},
		Collaborators: []string{"bo"},
		Dependents:    []string{"t-3"},
		Relations:     edgesTo("t-4"),
		Checklists: []tracker.Checklist{{
			ID:    "c-1",
			Name:  "Setup",
			Items: []tracker.ChecklistItem{{ID: "i-1", Done: true}},
		}},
		Fields:   map[string]json.RawMessage{"f-sev": json.RawMessage(`"high"`)},
		Archived: true,
		Removed: &tracker.Tombstone{
			By: "bo", Kind: tracker.AuthorHuman, RemovedWith: &cascadeRoot,
		},
		StartAt:         &earlier,
		DueAt:           &later,
		EstimateMinutes: 90,
		Points:          5,
	}
	return before, after
}

// edgesTo is one edge of EVERY declared kind, so the fixture moves every
// relation field rather than whichever one it happened to name.
//
// FROM [tracker.RelationKinds], for the reason [tracker.TaskDeltas] derives
// its fields from the same slice: a fifth kind of edge is covered here with no
// second edit.
func edgesTo(other string) []tracker.Relation {
	out := make([]tracker.Relation, 0, len(tracker.RelationKinds))
	for _, kind := range tracker.RelationKinds {
		out = append(out, tracker.Relation{Kind: kind, Other: other})
	}
	return out
}
