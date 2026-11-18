package worklet_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/worklet"
)

// TestAttributionFieldsAreTheEngines holds the dashboard's field-name list
// against the names [worklet.TaskDeltas] actually writes.
//
// The failure is silent both ways: a rail asking "who set `estimate`" under a
// name the engine does not write (the task row's `estimate_minutes`, say) just
// never shows the line, and a field added to TaskDeltas is a property the rail
// could attribute and does not. The client list may be a SUBSET, so every name
// it uses must be one the engine writes, and the rest are reported by name.
func TestAttributionFieldsAreTheEngines(t *testing.T) {
	t.Parallel()
	engine := deltaFields(t)

	body, err := clientsource.Literal(clientsource.Tree(t), "CHANGE_FIELDS")
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
			t.Errorf("the dashboard attributes %q, which worklet.TaskDeltas never "+
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
// listed: two tasks differing in every field, and the keys that come back. It
// passes a catalogue because custom-field values are named by SLUG or not at
// all, so a nil map would hide `fields` from the complete set.
func deltaFields(t *testing.T) []string {
	t.Helper()
	before, after := twoDifferentTasks()
	moved := worklet.TaskDeltas(before, after, map[string]worklet.FieldDef{
		"f-sev": {ID: "f-sev", Slug: "severity", Type: worklet.FieldText},
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
// [worklet.TaskDeltas] compares, so the set of names it returns is the
// COMPLETE set rather than whatever a fixture happened to move.
func twoDifferentTasks() (worklet.Task, worklet.Task) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	due := time.Date(2026, 3, 8, 17, 0, 0, 0, time.UTC)
	later := due.Add(24 * time.Hour)
	earlier := start.Add(-24 * time.Hour)
	oldParent, newParent := "t-old-parent", "t-new-parent"
	cascadeRoot := "t-root"
	before := worklet.Task{
		Title:           "Before",
		Body:            "was",
		Status:          worklet.Status("todo"),
		Assignee:        "ada",
		Reporter:        "cy",
		Priority:        worklet.Priority("low"),
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
		Checklists:      []worklet.Checklist{{ID: "c-1", Name: "Setup"}},
		Fields:          map[string]json.RawMessage{"f-sev": json.RawMessage(`"low"`)},
		StartAt:         &start,
		DueAt:           &due,
		EstimateMinutes: 30,
		Points:          1,
	}
	after := worklet.Task{
		Title:         "After",
		Body:          "is now",
		Status:        worklet.Status("in_progress"),
		Assignee:      "bo",
		Reporter:      "di",
		Priority:      worklet.Priority("high"),
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
		Checklists: []worklet.Checklist{{
			ID:    "c-1",
			Name:  "Setup",
			Items: []worklet.ChecklistItem{{ID: "i-1", Done: true}},
		}},
		Fields:   map[string]json.RawMessage{"f-sev": json.RawMessage(`"high"`)},
		Archived: true,
		Removed: &worklet.Tombstone{
			By: "bo", Kind: worklet.AuthorHuman, RemovedWith: &cascadeRoot,
		},
		StartAt:         &earlier,
		DueAt:           &later,
		EstimateMinutes: 90,
		Points:          5,
	}
	return before, after
}

// edgesTo is one edge of EVERY kind in [worklet.RelationKinds], the slice
// [worklet.TaskDeltas] derives its fields from, so the fixture moves every
// relation field and a new kind is covered with no second edit.
func edgesTo(other string) []worklet.Relation {
	out := make([]worklet.Relation, 0, len(worklet.RelationKinds))
	for _, kind := range worklet.RelationKinds {
		out = append(out, worklet.Relation{Kind: kind, Other: other})
	}
	return out
}
