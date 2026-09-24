package types_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// repoRoot is this module's root, relative to this package's directory.
const repoRoot = "../../.."

// taskIDReaders are the dashboard source files allowed to read a `task_id`,
// each with the meaning it reads — and none of them is a tracker item.
//
// TWO-SIDED: a file here that no longer mentions the key is stale and fails,
// so the list says what the dashboard does rather than what it once did.
var taskIDReaders = map[string]string{
	"lib/phases.ts": "a delegated worker's own task id, which keys one phase " +
		"record among its siblings inside one turn",
}

// TestWorkItemIsNeverReadFromTaskID holds the one rule the typed work item
// replaced: `task_id` is never how a reader learns which tracker item a turn
// or phase was on.
//
// The key has two live meanings — a delegated worker's own task (a phase
// completion's `task_id`) and a schedule fire's run id (`task_assigned`) —
// and neither is an item. The turn's own `task_id` was a third, declared on
// `turn_completed` and never assigned, and the turn list read it anyway: every
// row listed no item, and a reader joining it to the tracker would have joined
// a worker's task or a run id to whatever task happened to share the string.
// `work_item{backend,id,key,project}` is the item; this is what stops a reader
// reaching for the old key again, on either side of the wire.
func TestWorkItemIsNeverReadFromTaskID(t *testing.T) {
	t.Parallel()

	// THE TURN'S OWN RECORDS DECLARE NO task_id AT ALL, so there is
	// nothing on them to misread.
	for _, payload := range []any{
		types.AgentTurnStarted{}, types.AgentTurnCompleted{}, types.TurnCompleted{},
	} {
		typ := reflect.TypeOf(payload)
		for field := range typ.Fields() {
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "task_id" {
				t.Errorf("%s declares `task_id` again (field %s): the item a turn is "+
					"on is `work_item`, and a second key for it is a second answer",
					typ.Name(), field.Name)
			}
		}
	}

	// NO GO READER EXTRACTS task_id FROM A STORED PAYLOAD. The turn list
	// did exactly that, with json_extract(payload, '$.task_id'), and it is
	// the shape a join to the tracker would take.
	err := filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string,
		d fs.DirEntry, err error,
	) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), "$.task_id") {
			rel, _ := filepath.Rel(repoRoot, path)
			t.Errorf("%s reads `$.task_id` off a stored payload: the item a turn "+
				"or phase is on is `$.work_item`, and task_id is a worker's task "+
				"or a schedule run", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the engine source: %v", err)
	}

	// AND THE DASHBOARD READS task_id ONLY WHERE IT MEANS SOMETHING ELSE.
	dashboard := filepath.Join(repoRoot, "dashboard", "src")
	var readers []string
	err = filepath.WalkDir(dashboard, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if (ext != ".ts" && ext != ".tsx") || strings.Contains(path, ".test.") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), "task_id") {
			rel, _ := filepath.Rel(dashboard, path)
			readers = append(readers, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the dashboard source: %v", err)
	}
	if len(readers) == 0 && len(taskIDReaders) > 0 {
		t.Fatalf("no dashboard file mentions task_id, so either the source moved " +
			"out from under this gate or every allowed reader is stale")
	}
	for _, file := range readers {
		if _, allowed := taskIDReaders[file]; !allowed {
			t.Errorf("dashboard/src/%s reads `task_id`. A turn's or phase's item is "+
				"`work_item`; if this file reads a worker's task or a schedule run, "+
				"add it to taskIDReaders with the meaning it reads", file)
		}
	}
	for file, meaning := range taskIDReaders {
		if !slices.Contains(readers, file) {
			t.Errorf("taskIDReaders allows dashboard/src/%s (%s), which no longer "+
				"reads task_id — the entry is stale", file, meaning)
		}
	}
}
