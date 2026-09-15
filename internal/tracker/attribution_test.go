package tracker_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// dashboardTree is the client source this gate reads — the SOURCE, never the
// built bundle, which is minified and carries no declaration to find.
//
// The list itself is found by its DECLARATION rather than by a path, for the reason
// `internal/api/queries/rooms_test.go` gives at length: a gate that hard-codes
// where a constant lives breaks when a file moves and reports a drift between
// two lists neither of which changed. The two failures that matter are the
// ones this names — nothing declares it, which is a gate certifying nothing,
// and two files declare it, which is two copies that can drift.
const dashboardTree = "../../dashboard/src"

var attributionDecl = regexp.MustCompile(`(?s)export const CHANGE_FIELDS = \[(.*?)\] as const`)

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
	client := clientFields(t)

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
func deltaFields(t *testing.T) []string {
	t.Helper()
	before, after := twoDifferentTasks()
	moved := tracker.TaskDeltas(before, after)
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

// clientFields reads the one declaration in the dashboard tree.
func clientFields(t *testing.T) []string {
	t.Helper()
	var found []string
	err := walkClient(dashboardTree, func(source string) {
		if m := attributionDecl.FindStringSubmatch(source); m != nil {
			found = append(found, m[1])
		}
	})
	if err != nil {
		t.Fatalf("the dashboard tree at %s could not be walked, so this gate "+
			"certifies nothing: %v", dashboardTree, err)
	}
	if len(found) != 1 {
		t.Fatalf("%d files under %s declare CHANGE_FIELDS, want exactly one — "+
			"none is a gate certifying nothing, and two are two copies that can "+
			"drift from each other as well as from the engine", len(found), dashboardTree)
	}
	var names []string
	for _, part := range strings.Split(found[0], ",") {
		if name := strings.Trim(strings.TrimSpace(part), `"`); name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func walkClient(root string, each func(source string)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := root + "/" + entry.Name()
		if entry.IsDir() {
			if err := walkClient(path, each); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx") {
			continue
		}
		if strings.Contains(entry.Name(), ".test.") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		each(string(source))
	}
	return nil
}

// twoDifferentTasks is one task and another differing in every field
// [tracker.TaskDeltas] compares, so the set of names it returns is the
// COMPLETE set rather than whatever a fixture happened to move.
func twoDifferentTasks() (tracker.Task, tracker.Task) {
	sprintBefore, sprintAfter := 1, 2
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	due := time.Date(2026, 3, 8, 17, 0, 0, 0, time.UTC)
	later := due.Add(24 * time.Hour)
	earlier := start.Add(-24 * time.Hour)
	before := tracker.Task{
		Title:           "Before",
		Status:          tracker.Status("todo"),
		Assignee:        "ada",
		Priority:        tracker.Priority("low"),
		Project:         "ENG",
		Type:            "task",
		Tags:            []string{"one"},
		Sprint:          &sprintBefore,
		StartAt:         &start,
		DueAt:           &due,
		EstimateMinutes: 30,
		Points:          1,
	}
	after := tracker.Task{
		Title:           "After",
		Status:          tracker.Status("in_progress"),
		Assignee:        "bo",
		Priority:        tracker.Priority("high"),
		Project:         "OPS",
		Type:            "bug",
		Tags:            []string{"two"},
		Sprint:          &sprintAfter,
		StartAt:         &earlier,
		DueAt:           &later,
		EstimateMinutes: 90,
		Points:          5,
	}
	return before, after
}
