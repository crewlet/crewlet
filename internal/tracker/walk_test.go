package tracker_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A RE-SPREAD PRESERVES THE ORDER AT EVERY BATCH BOUNDARY.
//
// # Why that is stronger than "the walk finishes correctly"
//
// A walk is many records over minutes and WILL be interrupted — a node
// restarts, a lease flaps, a process is killed. So it is not enough that the
// order is right when the walk completes: it has to be right after every
// batch, because any batch may be the last one that runs. An operator watching
// a board through a re-spread must see nothing at all.
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

	plan, err := tracker.PlanRespread(t.Context(), r.db, "ENG")
	if err != nil {
		t.Fatalf("PlanRespread: %v", err)
	}
	if len(plan.Placements) != total {
		t.Fatalf("the plan moves %d of %d tasks, so this case is not "+
			"exercising a whole walk", len(plan.Placements), total)
	}
	// SLICED SMALLER THAN THE SHIPPED BATCH, deliberately: the property
	// under test is that ANY prefix of the plan leaves a correct board,
	// and a fixture small enough to be one shipped batch could not show
	// it. TestARespreadPlanBatchesConsecutively covers the real slicing.
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
		if len(rank) > tracker.RankRenormaliseAt {
			t.Errorf("the walk left rank %q at %d characters, past the %d "+
				"threshold it exists to bring them under",
				rank, len(rank), tracker.RankRenormaliseAt)
		}
	}
}

// SUCCESSIVE WALKS TAKE FRESH POSITIONS RATHER THAN SUBDIVIDING ONE.
//
// # The failure this exists to catch
//
// Run upward, a walk's ceiling is fixed by the frozen maximum's own integer
// part — so two walks with no create between them halve the SAME gap twice and
// the keys grow instead of shrinking, which is the exact condition the walk
// exists to remove. Downward, the floor moves with the project's minimum and
// every walk takes an integer of its own.
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
		if longest > tracker.RankRenormaliseAt {
			t.Fatalf("after walk %d the longest key is %d characters — "+
				"successive walks are subdividing one position rather than "+
				"taking a fresh one each", walk, longest)
		}
	}
}

// crowd makes every key long enough that the walk has work to do.
func crowd(t *testing.T, r *roundTrip, total int) {
	t.Helper()
	placements := make([]tracker.Placement, 0, total)
	// Keys that sort in the same order as the ids and are past the
	// threshold, which is the state a repeatedly-dragged board reaches.
	for i := range total {
		placements = append(placements, tracker.Placement{
			Task: fmt.Sprintf("t-%02d", i),
			Rank: tracker.Rank("a0" + strings.Repeat("0", tracker.RankRenormaliseAt) +
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

func boardRanks(t *testing.T, r *roundTrip) []tracker.Rank {
	t.Helper()
	answer := r.ask(map[string]any{
		"container": "project:ENG", "sort": "rank", "limit": "100",
	})
	ranks := make([]tracker.Rank, 0, len(answer.Rows))
	for _, row := range answer.Rows {
		ranks = append(ranks, row.Rank)
	}
	return ranks
}

// A PLAN'S BATCHES ARE CONSECUTIVE SLICES THAT COVER IT EXACTLY.
//
// A gap loses a task's placement — it keeps its long key and the walk reports
// success — and an overlap publishes one placement twice, which is a second
// record on the order's own subject that the first has already made stale.
func TestARespreadPlanBatchesConsecutively(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 1, tracker.WalkBatch - 1, tracker.WalkBatch,
		tracker.WalkBatch + 1, 3*tracker.WalkBatch + 7} {
		plan := tracker.RespreadPlan{Project: "ENG"}
		for i := range size {
			plan.Placements = append(plan.Placements, tracker.Placement{
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
