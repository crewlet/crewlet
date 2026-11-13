package engine

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// oneTask answers one task's record, and counts how often it was read.
type oneTask struct {
	task  worklet.Task
	err   error
	reads int
	level statelog.ReadLevel
}

func (o *oneTask) Task(_ context.Context, _ string, _ worklet.DetailWants,
	fresh statelog.Freshness) (worklet.TaskDetail, error) {
	o.reads++
	o.level = fresh.Level
	return worklet.TaskDetail{Task: o.task}, o.err
}

// A TURN IS HANDED ITS WORK ITEM'S ENTRIES ONLY WHERE IT MAY READ THE ATLAS
// (ADR-0140, ADR-0109): the tracker is read for a turn on a Worklet task that
// may, in the task's own order and at a seat's read level, and not at all for
// a turn that may not, a turn on no item, or one on another tracker's item. A
// task that cannot be read hands over nothing.
//
// Mutation: drop the readsAtlas check and the first case reads the task.
func TestATurnReadsItsWorkItemsEntriesOnlyWhereItMayReadTheAtlas(t *testing.T) {
	t.Parallel()
	web, db := uuid.NewString(), uuid.NewString()
	task := worklet.Task{ID: "t-1", Relations: []worklet.Relation{
		{Kind: worklet.RelationEntry, Other: db}, {Kind: worklet.RelationPage, Other: "s-1"},
		{Kind: worklet.RelationEntry, Other: web}}}
	item := &types.WorkItem{Backend: types.WorkWorklet, ID: "t-1", Key: "ENG-1", Project: "ENG"}
	for name, tc := range map[string]struct {
		item  *types.WorkItem
		reads bool
	}{
		"may not read the atlas": {item, false},
		"is on no item":          {nil, true},
		"is on a vendor's item":  {&types.WorkItem{Backend: types.WorkJira, ID: "10001"}, true},
	} {
		work := &oneTask{task: task}
		if got := linkedEntriesOf(t.Context(), work, "sre", tc.item, tc.reads); got != nil || work.reads != 0 {
			t.Errorf("a turn that %s was handed %v after %d reads of the tracker", name, got, work.reads)
		}
	}
	work := &oneTask{task: task}
	got := linkedEntriesOf(t.Context(), work, "sre", item, true)
	if !slices.Equal(got, []uuid.UUID{uuid.MustParse(db), uuid.MustParse(web)}) {
		t.Errorf("the turn was handed %v, want the task's two entries in its order", got)
	}
	if work.reads != 1 || work.level != statelog.DefaultReadLevel(statelog.SurfaceSeat) {
		t.Errorf("the task was read %d times at %q", work.reads, work.level)
	}
	failing := &oneTask{task: task, err: errors.New("the estate is behind")}
	if got := linkedEntriesOf(t.Context(), failing, "sre", item, true); got != nil {
		t.Errorf("a task that could not be read handed over %v", got)
	}
}
