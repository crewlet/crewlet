package tracker_test

import (
	"errors"
	"fmt"
	"slices"
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

// A RE-SPREAD KEEPS A LONG RUN BETWEEN THE SHORT KEYS AROUND IT.
//
// Long keys are a NEST — the product of dropping into one gap over and over —
// so they sit between short-keyed neighbours rather than making up the whole
// board. The walk rewrites into a reserve below the project's minimum, so a
// plan that moved only the long rows would put them in front of every short
// one: the case above, which crowds every row, cannot see that.
func TestARespreadKeepsALongRunBetweenItsNeighbours(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const total = 6
	for i := range total {
		if _, err := r.writer.CreateTask(t.Context(), fmt.Sprintf("op-%d", i),
			newTask(fmt.Sprintf("t-%02d", i)), nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}
	long := strings.Repeat("0", tracker.RankRenormaliseAt)
	if _, err := r.writer.MoveTasks(t.Context(), "op-nest", "ENG",
		[]tracker.Placement{
			{Task: "t-00", Rank: "a1"},
			{Task: "t-01", Rank: tracker.Rank("a1" + long + "1")},
			{Task: "t-02", Rank: tracker.Rank("a1" + long + "2")},
			{Task: "t-03", Rank: tracker.Rank("a1" + long + "3")},
			{Task: "t-04", Rank: "a2"},
			{Task: "t-05", Rank: "a3"},
		}); err != nil {
		t.Fatalf("nest the order: %v", err)
	}
	r.drain()
	want := boardOrder(t, r)

	plan, err := tracker.PlanRespread(t.Context(), r.db, "ENG")
	if err != nil {
		t.Fatalf("PlanRespread: %v", err)
	}
	// ONE ROW PER BATCH, so every boundary the walk can stop at is looked
	// at — including the ones between a short row and a long one.
	for i, placement := range plan.Placements {
		if _, err := r.writer.MoveTasks(t.Context(),
			fmt.Sprintf("op-r%d", i), "ENG", []tracker.Placement{placement}); err != nil {
			t.Fatalf("the re-spread batch: %v", err)
		}
		r.drain()
		if got := boardOrder(t, r); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("after batch %d the board reads %v and it read %v before "+
				"the walk began", i, got, want)
		}
	}
	for _, rank := range boardRanks(t, r) {
		if len(rank) > tracker.RankRenormaliseAt {
			t.Errorf("the walk left rank %q at %d characters", rank, len(rank))
		}
	}
}

// A WALK'S BATCH FITS ONE MOVE.
//
// The re-spread walk and the duplicate repair publish [tracker.WalkBatch]
// placements a record through [tracker.Writer.MoveTasks], which refuses more
// than [tracker.MaxBulkTasks]. A batch wider than the writer accepts would be
// refused on its first record and leave the project flagged for ever.
func TestAWalksBatchFitsOneMove(t *testing.T) {
	t.Parallel()
	if tracker.WalkBatch > tracker.MaxBulkTasks {
		t.Fatalf("a walk publishes %d placements a record and a move carries "+
			"at most %d", tracker.WalkBatch, tracker.MaxBulkTasks)
	}
}

// A DRAG MOVES THE TASK DRAGGED AND NOTHING ELSE.
//
// A key is long because its gap is narrow, and every key inside a narrow gap
// is long — so re-keying the tasks a drag finds in its gap brings none of them
// back under the threshold. And the gap a person dropped into is not always empty: a card a filter
// hides sits in it too. So a drag writes its own key, lands long when the gap
// is narrow, and hands the project to the walk through the applier's flag;
// the hidden card keeps its key and its place.
func TestADragMovesTheTaskDraggedAndNothingElse(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for i := range 4 {
		if _, err := r.writer.CreateTask(t.Context(), fmt.Sprintf("op-%d", i),
			newTask(fmt.Sprintf("t-%02d", i)), nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}
	long := "a1" + strings.Repeat("0", tracker.RankRenormaliseAt)
	after, hidden, before := tracker.Rank(long+"1"), tracker.Rank(long+"2V"),
		tracker.Rank(long+"3")
	if _, err := r.writer.MoveTasks(t.Context(), "op-nest", "ENG",
		[]tracker.Placement{
			{Task: "t-00", Rank: after}, {Task: "t-01", Rank: hidden},
			{Task: "t-02", Rank: before}, {Task: "t-03", Rank: "a3"},
		}); err != nil {
		t.Fatalf("nest the order: %v", err)
	}
	r.drain()

	// t-03 DROPPED BETWEEN t-00 AND t-02, which is the gap t-01 sits in.
	if _, err := r.writer.MoveTask(t.Context(), "op-drag", "ENG", "t-03",
		after, before); err != nil {
		t.Fatalf("MoveTask: %v", err)
	}
	r.drain()

	if got := strings.Join(boardOrder(t, r), ","); got != "t-00,t-03,t-01,t-02" {
		t.Fatalf("the board reads %s, want t-00,t-03,t-01,t-02", got)
	}
	if ranks := boardRanks(t, r); ranks[2] != hidden {
		t.Errorf("the card in the gap was re-keyed from %q to %q — a drag "+
			"moves the task dragged, and a re-key inside a narrow gap "+
			"brings nothing under the threshold", hidden, ranks[2])
	}
	if !flagged(t, r, "rank_respread_pending") {
		t.Error("the drag landed a key past the threshold and the project is " +
			"not flagged, so nothing hands its order to the walk")
	}
}

// A DRAG PAST THE SCHEMA'S CEILING IS REFUSED, AND THE REFUSAL NAMES ITS CURE.
//
// Between [tracker.RankRenormaliseAt] and [tracker.RankRefuseAt] a drag lands
// long and the walk tidies after it; past the second there is no key to land,
// and nothing but the walk's schedule stands between the two. So a person can
// reach this refusal with a request that is not malformed, and what they are
// owed is the length, the limit and the walk — not a verdict that their key is
// broken. The case then RUNS the cure: the same two cards, read after a walk,
// take the same drop.
func TestADragPastTheSchemasCeilingIsRefusedNamingItsCure(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for i := range 3 {
		if _, err := r.writer.CreateTask(t.Context(), fmt.Sprintf("op-%d", i),
			newTask(fmt.Sprintf("t-%02d", i)), nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}
	// TWO NEIGHBOURS AT THE CEILING, differing only in their last symbol,
	// so every key between them is one character past it.
	prefix := "a0" + strings.Repeat("V", tracker.RankRefuseAt-3)
	after, before := tracker.Rank(prefix+"1"), tracker.Rank(prefix+"2")
	if _, err := r.writer.MoveTasks(t.Context(), "op-nest", "ENG",
		[]tracker.Placement{
			{Task: "t-00", Rank: after}, {Task: "t-01", Rank: before},
		}); err != nil {
		t.Fatalf("nest the order at the ceiling: %v", err)
	}
	r.drain()
	was := boardRanks(t, r)

	_, err := r.writer.MoveTask(t.Context(), "op-drag", "ENG", "t-02",
		after, before)
	if !errors.Is(err, tracker.ErrRankTooLong) {
		t.Fatalf("a drag needing a %d-character key answered %v, want "+
			"ErrRankTooLong — a surface cannot tell this person to wait for "+
			"the walk without it", tracker.RankRefuseAt+1, err)
	}
	for _, want := range []string{
		fmt.Sprint(tracker.RankRefuseAt + 1), fmt.Sprint(tracker.RankRefuseAt),
		"re-spread walk",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q — it has to name the "+
				"length, the limit and the cure", err, want)
		}
	}
	r.drain()
	if got := boardRanks(t, r); !slices.Equal(got, was) {
		t.Fatalf("a refused drag moved the board from %v to %v", was, got)
	}

	// THE CURE, run: after the walk the same two cards are far apart.
	if _, err := r.writer.Respread(t.Context(), "op-walk", "ENG"); err != nil {
		t.Fatalf("Respread: %v", err)
	}
	r.drain()
	order, ranks := boardOrder(t, r), boardRanks(t, r)
	at := map[string]tracker.Rank{}
	for i, id := range order {
		at[id] = ranks[i]
	}
	if _, err := r.writer.MoveTask(t.Context(), "op-drag-again", "ENG", "t-02",
		at["t-00"], at["t-01"]); err != nil {
		t.Fatalf("the same drop after the walk: %v — the refusal promised the "+
			"walk makes room", err)
	}
	r.drain()
	if got := strings.Join(boardOrder(t, r), ","); got != "t-00,t-02,t-01" {
		t.Fatalf("the board reads %s, want t-00,t-02,t-01", got)
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
