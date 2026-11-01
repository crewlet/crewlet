package worklet_test

import (
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/worklet"
)

// writeLog records every item a writer reports, in order and undeduplicated,
// so a case can see exactly what reached the turn — the dedupe is the turn's
// set's job, and a log that did it here would hide a writer reporting twice.
type writeLog struct {
	mu    sync.Mutex
	items []types.WorkItem
}

func (l *writeLog) Add(item types.WorkItem) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, item)
}

func (l *writeLog) got() []types.WorkItem {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.items)
}

// A TURN'S WRITER REPORTS EVERY TASK ITS RECORDS COMMIT TO, AND NOTHING ELSE.
//
// This is what lets a turn nothing at dispatch named an item for be charged,
// at completion, to the one item it wrote. So the report has to be exact in
// both directions: a create and an edit each name their task — by id, with the
// key and project a person reads — while a write to something that is not a
// work item (a project's settings) and a write the broker refused name
// nothing. Either mistake charges a turn to the wrong thing, silently.
func TestTheWriterRecordsWhatARunWrote(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	log := &writeLog{}
	w := r.writer.As("dev", worklet.AuthorAgent, worklet.Provenance{
		TurnID: "run-1", Written: log,
	})

	if _, err := w.CreateTask(t.Context(), "op-t-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	r.drain()
	done := worklet.StatusDone
	if _, err := w.UpdateTask(t.Context(), "op-t-1-done", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done},
		worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	r.drain()

	got := log.got()
	if len(got) != 2 {
		t.Fatalf("reported %d writes, want the create and the edit: %+v", len(got), got)
	}
	for i, item := range got {
		if item.Backend != types.WorkWorklet || item.ID != "t-1" || item.Project != "ENG" ||
			item.Key == "" {
			t.Errorf("write %d named %+v; want a Worklet item by the task's id, "+
				"with the key and project a person reads", i, item)
		}
	}

	// NOT A WORK ITEM: a project's settings are a write, but charging a
	// turn to one would charge it to something no per-item figure names.
	before := len(log.got())
	if _, err := w.WriteDocument(t.Context(), "op-project-2",
		worklet.ProjectSubject("OPS"), "", worklet.Project{
			V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, worklet.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("project write: %v", err)
	}
	r.drain()
	// AND NOT A WRITE THAT DID NOT COMMIT: a create whose task is already
	// filed is answered from its row with nothing published, and a write
	// that published nothing is no write.
	if _, err := w.CreateTask(t.Context(), "op-t-1-again", newTask("t-1"), nil); err != nil {
		t.Fatalf("a create of the filed t-1 made again: %v", err)
	}
	if after := log.got(); len(after) != before {
		t.Errorf("reported %+v for writes that are not a committed task write",
			after[before:])
	}
}

// A WRITER THAT IS NOT A TURN'S REPORTS NOTHING, and writes exactly as before:
// every surface that is not a turn — an operator, a duty on a tick — builds its
// writer without a log, and the report must never be what makes it fail.
func TestAWriterWithNoLogStillWrites(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	w := r.writer.As("ops", worklet.AuthorOperator, worklet.Provenance{})
	if _, err := w.CreateTask(t.Context(), "op-t-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("create with no log: %v", err)
	}
}
