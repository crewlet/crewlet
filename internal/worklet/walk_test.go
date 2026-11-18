package worklet_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// A RE-SPREAD PRESERVES THE ORDER AT EVERY BATCH BOUNDARY, not only at the
// end: a walk will be interrupted, so any batch may be the last that runs.
func TestARespreadPreservesTheOrderAtEveryBatch(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// Keys long enough to need re-spreading, in a known order.
	const total = 12
	var want []string
	for i := range total {
		task := newTask(fmt.Sprintf("t-%02d", i))
		if _, err := r.writer.CreateTask(t.Context(), fmt.Sprintf("op-%d", i),
			task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
		want = append(want, task.ID)
	}
	crowd(t, r, total)
	if got := boardOrder(t, r); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the fixture's own order is %v, want %v", got, want)
	}

	plan, err := worklet.PlanRespread(t.Context(), r.db.Reader(), "ENG")
	if err != nil {
		t.Fatalf("PlanRespread: %v", err)
	}
	if len(plan.Placements) != total {
		t.Fatalf("the plan moves %d of %d tasks, so this case is not "+
			"exercising a whole walk", len(plan.Placements), total)
	}
	// SLICED SMALLER THAN THE SHIPPED BATCH, deliberately: ANY prefix must
	// leave a correct board, which a one-batch fixture could not show.
	// TestARespreadPlanBatchesConsecutively covers the real slicing.
	const step = 4
	for from := 0; from < total; from += step {
		batch := plan.Placements[from:min(from+step, total)]
		if _, err := r.writer.MoveTasks(t.Context(),
			fmt.Sprintf("op-r%d", from), "ENG", batch); err != nil {
			t.Fatalf("the re-spread batch: %v", err)
		}
		r.drain()

		// AFTER EVERY BATCH, including this one being the last.
		if got := boardOrder(t, r); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("after the batch at %d the board reads %v and it read %v "+
				"before the walk began — an interrupted re-spread must leave "+
				"a correct board, not a repairable one", from, got, want)
		}
	}

	// AND THE KEYS ARE SHORT AGAIN, which is what the walk was for.
	for _, rank := range boardRanks(t, r) {
		if len(rank) > worklet.RankRenormaliseAt {
			t.Errorf("the walk left rank %q at %d characters, past the %d "+
				"threshold it exists to bring them under",
				rank, len(rank), worklet.RankRenormaliseAt)
		}
	}
}

// SUCCESSIVE WALKS TAKE FRESH POSITIONS RATHER THAN SUBDIVIDING ONE: a walk
// run upward would halve the same gap each time and grow the keys
// ([worklet.RespreadPlan] says why it runs downward).
func TestSuccessiveRespreadsDoNotSubdivideOnePosition(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const total = 4
	for i := range total {
		if _, err := r.writer.CreateTask(t.Context(), fmt.Sprintf("op-%d", i),
			newTask(fmt.Sprintf("t-%02d", i)), nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	longest := 0
	for walk := range 3 {
		crowd(t, r, total)
		if _, err := r.writer.Respread(t.Context(),
			fmt.Sprintf("op-walk%d", walk), "ENG"); err != nil {
			t.Fatalf("Respread %d: %v", walk, err)
		}
		r.drain()
		for _, rank := range boardRanks(t, r) {
			if len(rank) > longest {
				longest = len(rank)
			}
		}
		if longest > worklet.RankRenormaliseAt {
			t.Fatalf("after walk %d the longest key is %d characters — "+
				"successive walks are subdividing one position rather than "+
				"taking a fresh one each", walk, longest)
		}
	}
}

// crowd makes every key long enough that the walk has work to do.
func crowd(t *testing.T, r *roundTrip, total int) {
	t.Helper()
	placements := make([]worklet.Placement, 0, total)
	// Past the threshold and in id order, as a repeatedly-dragged board
	// reaches.
	for i := range total {
		placements = append(placements, worklet.Placement{
			Task: fmt.Sprintf("t-%02d", i),
			Rank: worklet.Rank("a0" + strings.Repeat("0", worklet.RankRenormaliseAt) +
				fmt.Sprintf("%02d1", i)),
		})
	}
	if _, err := r.writer.MoveTasks(t.Context(),
		fmt.Sprintf("op-crowd-%d", r.consumed), "ENG", placements); err != nil {
		t.Fatalf("crowd the order: %v", err)
	}
	r.drain()
}

func boardOrder(t *testing.T, r *roundTrip) []string {
	t.Helper()
	answer := r.ask(map[string]any{
		"container": "project:ENG", "sort": "rank", "limit": "100",
	})
	ids := make([]string, 0, len(answer.Rows))
	for _, row := range answer.Rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func boardRanks(t *testing.T, r *roundTrip) []worklet.Rank {
	t.Helper()
	answer := r.ask(map[string]any{
		"container": "project:ENG", "sort": "rank", "limit": "100",
	})
	ranks := make([]worklet.Rank, 0, len(answer.Rows))
	for _, row := range answer.Rows {
		ranks = append(ranks, row.Rank)
	}
	return ranks
}

// A PLAN'S BATCHES ARE CONSECUTIVE SLICES THAT COVER IT EXACTLY: a gap leaves
// a task its long key while the walk reports success, and an overlap publishes
// a placement twice, the second already stale on the order's subject.
func TestARespreadPlanBatchesConsecutively(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 1, worklet.WalkBatch - 1, worklet.WalkBatch,
		worklet.WalkBatch + 1, 3*worklet.WalkBatch + 7} {
		plan := worklet.RespreadPlan{Project: "ENG"}
		for i := range size {
			plan.Placements = append(plan.Placements, worklet.Placement{
				Task: fmt.Sprintf("t-%04d", i),
			})
		}
		var walked []string
		for k := range plan.Batches() {
			batch := plan.Batch(k)
			if len(batch) == 0 {
				t.Errorf("batch %d of a %d-placement plan is empty, so the "+
					"walk publishes a record that moves nothing", k, size)
			}
			for _, p := range batch {
				walked = append(walked, p.Task)
			}
		}
		if len(walked) != size {
			t.Errorf("a %d-placement plan walked %d of them — a gap leaves a "+
				"task with its long key and the walk reports success",
				size, len(walked))
		}
		for i, task := range walked {
			if want := fmt.Sprintf("t-%04d", i); task != want {
				t.Fatalf("a %d-placement plan walked %s at position %d, want "+
					"%s — the batches must cover the plan in order and "+
					"exactly once", size, task, i, want)
			}
		}
		if plan.Batch(plan.Batches()) != nil {
			t.Errorf("a %d-placement plan hands back a batch past its end",
				size)
		}
	}
}
