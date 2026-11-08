package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessreview"
	"github.com/crewlet/crewlet/internal/worklet"
)

// trackedTask is one work item as a [countingTracker] holds it.
type trackedTask struct {
	key, askedBy, assignee string
	group                  worklet.StatusGroup
	blocked                bool
}

// countingTracker answers the task reads open work is counted through, for the
// fields those reads set, in key order.
type countingTracker []trackedTask

func (c countingTracker) Tasks(_ context.Context, q worklet.Query, _ time.Time) (worklet.Answer, error) {
	var out worklet.Answer
	for _, task := range c {
		if !c.matches(task, q) {
			continue
		}
		out.TotalHint++
		if len(out.Rows) < q.Limit {
			out.Rows = append(out.Rows, worklet.TaskRow{Key: task.key, StatusGroup: task.group,
				Blocked: task.blocked})
		}
	}
	return out, nil
}

func (c countingTracker) matches(task trackedTask, q worklet.Query) bool {
	switch {
	case q.AskedBy != "" && task.askedBy != q.AskedBy,
		len(q.Assignee) > 0 && !slices.Contains(q.Assignee, task.assignee),
		len(q.StatusGroups) > 0 && !slices.Contains(q.StatusGroups, task.group),
		q.Blocked != nil && *q.Blocked != task.blocked,
		len(q.Keys) > 0 && !slices.Contains(q.Keys, task.key):
		return false
	}
	return len(q.Any) == 0 || slices.ContainsFunc(q.Any, func(branch worklet.Query) bool {
		return c.matches(task, branch)
	})
}

// OPEN WORK COUNTS EACH ITEM ONCE, PAST THE LISTED PAGES TOO: twelve items the
// SRE asked on and is assigned, a blocked one, a run on one of the twelve, a
// run on an item of nobody's and a run on none are fifteen, however the pages
// cut them; each kind still says how many it did not list.
//
// Mutation: add the unlisted tails to the distinct listed items and the twelve
// count again past the first page.
func TestOpenWorkCountsEachItemOncePastThePage(t *testing.T) {
	t.Parallel()
	var tracker countingTracker
	for i := range 12 {
		tracker = append(tracker, trackedTask{key: "ENG-" + string(rune('A'+i)), askedBy: "sre",
			assignee: "sre", group: worklet.GroupActive})
	}
	tracker = append(tracker, trackedTask{key: "ENG-Z", assignee: "sre", group: worklet.GroupNotStarted,
		blocked: true}, trackedTask{key: "OPS-1", assignee: "pm", group: worklet.GroupActive})
	runs := []accessreview.Run{{Turn: "t-1", Task: "ENG-C"}, {Turn: "t-2", Task: "OPS-9"}, {Turn: "t-3"}}
	w, err := openWorkOf(t.Context(), tracker, time.UTC, "sre", runs)
	if err != nil {
		t.Fatalf("openWorkOf: %v", err)
	}
	if w.Open != 15 || w.OpenAtLeast {
		t.Errorf("the work counts %d open items (at least: %v), want exactly 15", w.Open, w.OpenAtLeast)
	}
	if len(w.Asks) != accessreview.MaxListed || w.MoreAsks != 2 || len(w.Items) != accessreview.MaxListed ||
		w.MoreItems != 3 || len(w.Runs) != 3 || w.MoreRuns != 0 {
		t.Errorf("the work lists %d asks (+%d), %d items (+%d) and %d runs (+%d)",
			len(w.Asks), w.MoreAsks, len(w.Items), w.MoreItems, len(w.Runs), w.MoreRuns)
	}
	if w, err := openWorkOf(t.Context(), nil, time.UTC, "sre", runs); err != nil || w.Open != 3 {
		t.Errorf("with no tracker the runs count %d (%v), want 3", w.Open, err)
	}
}
