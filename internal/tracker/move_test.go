package tracker_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// ---- the arithmetic ------------------------------------------------------ //

// A MOVE NEVER MINTS A KEY A CREATE COULD.
//
// The proof that a create and a drag can never collide is about the VALUE: a
// create's key is a pure integer at or above the origin, and every move's key
// carries a fraction or sits strictly below it. The cheapest key at either end
// of a board is a pure integer in the create lattice — after the last task it
// is exactly the key the next create mints — so a mint that took it would
// file the next task onto the card somebody just dragged to the bottom.
func TestAMoveNeverMintsACreatesKey(t *testing.T) {
	t.Parallel()
	gaps := []struct{ a, b tracker.Rank }{
		{"a5", ""},   // the bottom of a board of creates
		{"", "a5"},   // the top of one whose first creates moved away
		{"", "a5V"},  // the top, above a fraction
		{"", "a0"},   // the top, above the very first create
		{"a1", "a5"}, // a gap with free integers in it
		{"a5V", ""},  // the bottom, below nothing, after a drag
		{"Zz", ""},   // the bottom of a board of head placements
		{"", "Zz"},   // above a head placement
		{"", ""},     // an empty board
	}
	for _, gap := range gaps {
		for _, n := range []int{1, 16} {
			keys, err := tracker.MoveKeys(gap.a, gap.b, n)
			if err != nil {
				t.Fatalf("MoveKeys(%q, %q, %d): %v", gap.a, gap.b, n, err)
			}
			if len(keys) != n {
				t.Fatalf("MoveKeys(%q, %q, %d) minted %d keys", gap.a, gap.b, n, len(keys))
			}
			prev := gap.a
			for _, k := range keys {
				if !k.Valid() {
					t.Errorf("MoveKeys(%q, %q) minted %q, which is not well formed", gap.a, gap.b, k)
				}
				if k.FromCreate() {
					t.Errorf("MoveKeys(%q, %q) minted %q — a pure integer at or "+
						"above the origin, which is a key a create mints", gap.a, gap.b, k)
				}
				if prev != "" && string(k) <= string(prev) {
					t.Errorf("MoveKeys(%q, %q) minted %q, not above %q", gap.a, gap.b, k, prev)
				}
				if gap.b != "" && string(k) >= string(gap.b) {
					t.Errorf("MoveKeys(%q, %q) minted %q, not below %q", gap.a, gap.b, k, gap.b)
				}
				prev = k
			}
		}
	}
}

// A DROP WHERE THE CARD ALREADY SITS WRITES NOTHING.
func TestADropWhereTheCardAlreadySitsMovesNothing(t *testing.T) {
	t.Parallel()
	got, err := tracker.PlaceInGap(tracker.Placement{Task: "m", Rank: "a1"},
		[]tracker.Placement{{Task: "x", Rank: "a0"}},
		[]tracker.Placement{{Task: "y", Rank: "a2"}})
	if err != nil {
		t.Fatalf("PlaceInGap: %v", err)
	}
	if got != nil {
		t.Fatalf("a card dropped where it already sits wrote %v", got)
	}
}

// A GAP WHOSE KEYS HAVE GROWN LONG IS RE-SPREAD IN THE DRAG'S OWN RECORD.
//
// Repeated drops into one gap subdivide it and the keys grow. The drop that
// would mint past the threshold instead re-mints a window around the gap, in
// the order it was in, with the moved card in its place — so the board is
// never observed half-spread and the keys are short again.
func TestALongGapIsReSpreadInOrder(t *testing.T) {
	t.Parallel()
	nest := tracker.Rank("a1" + strings.Repeat("V", tracker.RankRenormaliseAt))
	keys, err := tracker.KeysBetween(nest, nest+"z", 20)
	if err != nil {
		t.Fatalf("KeysBetween: %v", err)
	}
	// Ten nested rows either side of the gap, then one short key on each
	// side that the window can escape to.
	var below, above []tracker.Placement
	for i := 9; i >= 0; i-- {
		below = append(below, tracker.Placement{Task: fmt.Sprintf("b%d", i), Rank: keys[i]})
	}
	below = append(below, tracker.Placement{Task: "floor", Rank: "a1"})
	for i := 10; i < 20; i++ {
		above = append(above, tracker.Placement{Task: fmt.Sprintf("a%d", i), Rank: keys[i]})
	}
	above = append(above, tracker.Placement{Task: "ceiling", Rank: "a2"})

	got, err := tracker.PlaceInGap(tracker.Placement{Task: "m", Rank: "a9"}, below, above)
	if err != nil {
		t.Fatalf("PlaceInGap: %v", err)
	}
	if len(got) < 3 {
		t.Fatalf("a drop into a gap of %d-character keys wrote %d placements — "+
			"it minted a long key rather than re-spreading", len(nest), len(got))
	}
	var order []string
	for i, p := range got {
		order = append(order, p.Task)
		if len(p.Rank) > tracker.RankRenormaliseAt {
			t.Errorf("the re-spread left %s at %d characters", p.Task, len(p.Rank))
		}
		if p.Rank.FromCreate() {
			t.Errorf("the re-spread gave %s the create key %q", p.Task, p.Rank)
		}
		if i > 0 && string(p.Rank) <= string(got[i-1].Rank) {
			t.Errorf("the re-spread put %s at %q, not above %s at %q",
				p.Task, p.Rank, got[i-1].Task, got[i-1].Rank)
		}
	}
	mover := slices.Index(order, "m")
	if mover < 0 || order[mover-1] != "b9" || order[mover+1] != "a10" {
		t.Fatalf("the re-spread wrote %v — the moved card must sit between the "+
			"two rows either side of the gap", order)
	}
}

// A SHARED KEY ACROSS THE GAP IS PULLED APART, rather than refused.
//
// Two cards sharing a key have nothing between them, so a drop between them
// has no key to take. The window re-mints both apart in the same record,
// which is the duty's duplicate repair done by the drag that found it.
func TestADropBetweenTwoCardsSharingAKeyPullsThemApart(t *testing.T) {
	t.Parallel()
	got, err := tracker.PlaceInGap(tracker.Placement{Task: "m", Rank: "a9"},
		[]tracker.Placement{{Task: "x", Rank: "a1V"}, {Task: "w", Rank: "a1"}},
		[]tracker.Placement{{Task: "y", Rank: "a1V"}, {Task: "z", Rank: "a2"}})
	if err != nil {
		t.Fatalf("PlaceInGap: %v", err)
	}
	var order []string
	for i, p := range got {
		order = append(order, p.Task)
		if i > 0 && string(p.Rank) <= string(got[i-1].Rank) {
			t.Fatalf("the placements %v are not strictly ascending", got)
		}
	}
	if i := slices.Index(order, "m"); i < 1 || order[i-1] != "x" || order[i+1] != "y" {
		t.Fatalf("the placements wrote %v — the card belongs between x and y", order)
	}
}

// PAST THE INLINE CAP THE DRAG STILL LANDS.
//
// A window that cannot find short keys even at [tracker.RankRespreadInline]
// rows falls back to the one long key: the applier flags the project for the
// duty's paced walk, and the person's drag is never refused because a board is
// crowded.
func TestADragPastTheInlineCapStillLands(t *testing.T) {
	t.Parallel()
	nest := tracker.Rank("a1" + strings.Repeat("V", tracker.RankRenormaliseAt+8))
	keys, err := tracker.KeysBetween(nest, nest+"z", 2*(tracker.RankRespreadInline/2+1))
	if err != nil {
		t.Fatalf("KeysBetween: %v", err)
	}
	half := len(keys) / 2
	var below, above []tracker.Placement
	for i := half - 1; i >= 0; i-- {
		below = append(below, tracker.Placement{Task: fmt.Sprintf("b%d", i), Rank: keys[i]})
	}
	for i := half; i < len(keys); i++ {
		above = append(above, tracker.Placement{Task: fmt.Sprintf("a%d", i), Rank: keys[i]})
	}
	got, err := tracker.PlaceInGap(tracker.Placement{Task: "m", Rank: "a9"}, below, above)
	if err != nil {
		t.Fatalf("a drag into a crowded board was refused: %v", err)
	}
	if len(got) != 1 || got[0].Task != "m" {
		t.Fatalf("PlaceInGap wrote %d placements; past the cap it writes the "+
			"drag alone and the duty walks the rest", len(got))
	}
	if string(got[0].Rank) <= string(below[0].Rank) || string(got[0].Rank) >= string(above[0].Rank) {
		t.Fatalf("the drag landed at %q, outside its gap", got[0].Rank)
	}
}

// ---- the gesture ---------------------------------------------------------- //

// fourCards files t-1..t-4 into ENG, in that order.
func fourCards(t *testing.T, r *roundTrip) {
	t.Helper()
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("t-%d", i)
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
}

// A MOVE WITHIN A LANE REORDERS ONLY THAT ITEM.
//
// One record, one placement: the neighbours keep their keys, and the card's
// own version does not move — the order stamps `scoped_through` — so the
// board's next drag of the same card is not refused by this one.
func TestAMoveWithinALaneReordersOnlyThatItem(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fourCards(t, r)
	before := map[string]tracker.Task{}
	for _, id := range []string{"t-1", "t-2", "t-3", "t-4"} {
		before[id] = r.task(t, id).Task
	}

	got, err := r.writer.MoveTask(t.Context(), "op-drag", tracker.Move{
		Task: "t-4", Project: "ENG", Before: "t-2", IfMatch: before["t-4"].Version,
	}, nil)
	if err != nil {
		t.Fatalf("MoveTask: %v", err)
	}
	if got.Unplaced != nil || got.Rank == "" {
		t.Fatalf("the drag answered %+v", got)
	}
	r.drain()

	if order := strings.Join(boardOrder(t, r), ","); order != "t-1,t-4,t-2,t-3" {
		t.Fatalf("the board reads %s after dropping t-4 above t-2", order)
	}
	for _, id := range []string{"t-1", "t-2", "t-3"} {
		if after := r.task(t, id).Task.Rank; after != before[id].Rank {
			t.Errorf("%s moved from %q to %q — a drag of one card rewrote a "+
				"neighbour it did not need to", id, before[id].Rank, after)
		}
	}
	moved := r.task(t, "t-4").Task
	if moved.Version != before["t-4"].Version {
		t.Errorf("the drag moved t-4's version from %d to %d, so the board's "+
			"next if_match on it is refused", before["t-4"].Version, moved.Version)
	}
	if moved.Status != before["t-4"].Status {
		t.Errorf("a drag within a lane changed the status to %s", moved.Status)
	}

	// AND AGAIN, from the version the board still holds.
	_, err = r.writer.MoveTask(t.Context(), "op-drag-2", tracker.Move{
		Task: "t-4", Project: "ENG", After: "t-3", IfMatch: before["t-4"].Version,
	}, nil)
	if err != nil {
		t.Fatalf("the second drag of one card was refused: %v", err)
	}
	r.drain()
	if order := strings.Join(boardOrder(t, r), ","); order != "t-1,t-2,t-3,t-4" {
		t.Fatalf("the board reads %s after dropping t-4 below t-3", order)
	}
}

// AN EDIT AFTER A DRAG KEEPS THE CARD WHERE IT WAS DRAGGED.
//
// The order moves a task's rank column and never its document, and every task
// write re-upserts the row from a document — so a task write that took its
// rank from the document put the card back at the key it was filed at, and a
// board's manual order lasted until somebody next touched the card. The detail
// read answered the same stale key as the card's place.
func TestAnEditAfterADragKeepsTheCardsPlace(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fourCards(t, r)
	if _, err := r.writer.MoveTask(t.Context(), "op-drag", tracker.Move{
		Task: "t-4", Project: "ENG", Before: "t-2",
	}, nil); err != nil {
		t.Fatalf("MoveTask: %v", err)
	}
	r.drain()
	high := tracker.PriorityHigh
	if _, err := r.writer.UpdateTask(t.Context(), "op-edit", "t-4", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Priority: &high},
		tracker.ChangeFields, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()

	if order := strings.Join(boardOrder(t, r), ","); order != "t-1,t-4,t-2,t-3" {
		t.Fatalf("the board reads %s after editing a dragged card — the edit "+
			"put it back where it was filed", order)
	}
	ranks := boardRanks(t, r)
	if got := r.task(t, "t-4").Task.Rank; got != ranks[1] {
		t.Errorf("the detail read answers t-4's rank as %q and the board holds "+
			"it at %q", got, ranks[1])
	}
}

// A MOVE ACROSS LANES CHANGES THE STATUS AND THE PLACE TOGETHER.
func TestAMoveAcrossLanesChangesStatusAndRankTogether(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	fourCards(t, r)
	version := r.task(t, "t-4").Task.Version

	active := tracker.StatusInProgress
	got, err := r.writer.MoveTask(t.Context(), "op-lane", tracker.Move{
		Task: "t-4", Project: "ENG", After: "t-1", Status: &active, IfMatch: version,
	}, nil)
	if err != nil {
		t.Fatalf("MoveTask: %v", err)
	}
	if got.Unplaced != nil {
		t.Fatalf("the lane changed and the card was not placed: %v", got.Unplaced)
	}
	if !got.Lane.Wrote() || !got.Order.Wrote() {
		t.Fatalf("a move across lanes wrote lane=%v order=%v — both halves "+
			"are records", got.Lane.Outcome, got.Order.Outcome)
	}
	r.drain()

	moved := r.task(t, "t-4").Task
	if moved.Status != active {
		t.Errorf("t-4 is %s after a drop into %s", moved.Status, active)
	}
	if order := strings.Join(boardOrder(t, r), ","); order != "t-1,t-4,t-2,t-3" {
		t.Fatalf("the board reads %s after dropping t-4 below t-1", order)
	}
}

// A STALE MOVE IS REFUSED, AND NOTHING LANDS.
//
// A card somebody changed after the board was drawn is not the card the
// person dragged. Refused before either half is written — with a lane change
// and without one — so a stale drag leaves the board exactly as it was.
func TestAStaleMoveIsRefused(t *testing.T) {
	t.Parallel()
	for _, lane := range []bool{false, true} {
		t.Run(fmt.Sprintf("lane=%v", lane), func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			r.applyWhileWriting()
			fourCards(t, r)
			drawn := r.task(t, "t-4").Task
			high := tracker.PriorityHigh
			if _, err := r.writer.UpdateTask(t.Context(), "op-edit", "t-4", "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Priority: &high},
				tracker.ChangeFields, nil); err != nil {
				t.Fatalf("UpdateTask: %v", err)
			}
			r.drain()
			order := strings.Join(boardOrder(t, r), ",")

			move := tracker.Move{Task: "t-4", Project: "ENG", Before: "t-1",
				IfMatch: drawn.Version}
			if lane {
				active := tracker.StatusInProgress
				move.Status = &active
			}
			_, err := r.writer.MoveTask(t.Context(), "op-stale", move, nil)
			if !errors.Is(err, tracker.ErrStaleVersion) {
				t.Fatalf("a drag of a card changed since the board was drawn "+
					"answered %v, want ErrStaleVersion", err)
			}
			r.drain()
			if got := strings.Join(boardOrder(t, r), ","); got != order {
				t.Errorf("a refused drag moved the board from %s to %s", order, got)
			}
			if got := r.task(t, "t-4").Task.Status; got != drawn.Status {
				t.Errorf("a refused drag changed the lane to %s", got)
			}
		})
	}
}

// A CARD DRAGGED TO THE BOTTOM LEAVES THE NEXT CREATE'S KEY FREE.
//
// The bottom of a board is where the next create lands, and its key is the
// next integer. A drag that took that integer would be filed onto by the next
// task anybody creates — a duplicate the applier flags and the duty repairs,
// manufactured by an ordinary drag.
func TestADragToTheBottomLeavesTheNextCreatesKeyFree(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fourCards(t, r)
	if _, err := r.writer.MoveTask(t.Context(), "op-bottom", tracker.Move{
		Task: "t-1", Project: "ENG", After: "t-4",
	}, nil); err != nil {
		t.Fatalf("MoveTask: %v", err)
	}
	r.drain()
	if _, err := r.writer.CreateTask(t.Context(), "op-t-5", newTask("t-5"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	// DISTINCT KEYS, asked of the rows: a create is never probed for a
	// duplicate — the proof says it cannot collide — so a collision here
	// would sit on the board unflagged, ordered only by id.
	ranks := boardRanks(t, r)
	for i := 1; i < len(ranks); i++ {
		if ranks[i] == ranks[i-1] {
			t.Fatalf("the create after a drag to the bottom took the dragged "+
				"card's key %q: %v", ranks[i], ranks)
		}
	}
	if order := strings.Join(boardOrder(t, r), ","); order != "t-2,t-3,t-4,t-1,t-5" {
		t.Fatalf("the board reads %s — the dragged card sits at the bottom and "+
			"the next create below it", order)
	}
}

// A DROP THAT NAMES NO GAP ON THE CARD'S OWN BOARD IS REFUSED: beside itself,
// beside two cards at once, or of a card into a board it is not on. A drop
// beside a neighbour that is not on the board has its own case below.
func TestADropThatNamesNoGapIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fourCards(t, r)
	if _, err := r.writer.MoveTask(t.Context(), "op-self", tracker.Move{
		Task: "t-1", Project: "ENG", Before: "t-1",
	}, nil); !errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("a drop beside itself answered %v, want ErrInvalid", err)
	}
	if _, err := r.writer.MoveTask(t.Context(), "op-both", tracker.Move{
		Task: "t-1", Project: "ENG", Before: "t-2", After: "t-3",
	}, nil); !errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("a drop naming two neighbours answered %v, want ErrInvalid", err)
	}
	if _, err := r.writer.MoveTask(t.Context(), "op-elsewhere", tracker.Move{
		Task: "t-1", Project: "OPS", Before: "t-2",
	}, nil); err == nil {
		t.Error("a drop of a card into a board it is not on was placed")
	}
}

// A TASK RECORD FROM AN OLDER BUILD APPLIES ITS RANK AS THAT BUILD DID.
//
// A build reading record version 7 merges every task write into the task's
// DOCUMENT and upserts the row from it, so after a drag it re-writes the key
// the task was filed at. This build carries the ROW's key through instead —
// but only for a record at the version that says so. Were the new rule applied
// to an older build's record, the nodes of one rolling upgrade would write
// different rows for the same log position, permanently, on a table the fleet
// compares byte for byte. So a version-7 edit after a drag puts the card back
// on every node (as it always did), and the version-8 edit this build writes
// keeps it where it was dragged on every node.
func TestATaskRecordFromAnOlderBuildAppliesItsRankAsThatBuildDid(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	if _, err := h.apply(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil), at); err != nil {
		t.Fatalf("create: %v", err)
	}
	dragged, err := tracker.MoveKeys(tracker.RankOrigin, "", 1)
	if err != nil {
		t.Fatalf("MoveKeys: %v", err)
	}
	drag := func(opID string) {
		t.Helper()
		body, _ := json.Marshal(tracker.RankOrder{
			V: tracker.DocumentVersion, Project: "ENG",
			Placements: []tracker.Placement{{Task: "t-1", Rank: dragged[0]}},
		})
		if _, err := h.apply(tracker.MutationRecord{
			RecordEnvelope: tracker.RecordEnvelope{
				V: 1, OpID: opID, Subject: tracker.RankOrderSubject("ENG"),
				Op: tracker.OpPatch, CreatedAt: at, Writer: "node-a",
				Scope: tracker.ScopeSet{Terms: []tracker.ScopeTerm{{
					Kind: tracker.TermContainer, ID: "ENG"}}},
			},
			Mutation: body, Actor: "ana", ActorKind: tracker.AuthorHuman,
		}, at); err != nil {
			t.Fatalf("drag: %v", err)
		}
	}
	edit := func(opID string, v int, keeps bool) {
		t.Helper()
		high := tracker.PriorityHigh
		record := taskRecord("t-1", tracker.OpPatch, tracker.TaskPatch{Priority: &high}, nil)
		record.OpID, record.V, record.KeepsPlace = opID, v, keeps
		record.Kind = tracker.ChangeFields
		if _, err := h.apply(record, at); err != nil {
			t.Fatalf("edit at version %d: %v", v, err)
		}
	}
	place := func() (tracker.Rank, tracker.Rank) {
		t.Helper()
		var column string
		var document []byte
		if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(),
				`SELECT rank, document FROM tracker_tasks WHERE id = 't-1'`).
				Scan(&column, &document)
		}); err != nil {
			t.Fatalf("read t-1: %v", err)
		}
		var task tracker.Task
		if err := json.Unmarshal(document, &task); err != nil {
			t.Fatalf("decode t-1: %v", err)
		}
		return tracker.Rank(column), task.Rank
	}

	drag("drag-1")
	edit("edit-old", 7, false)
	if column, document := place(); column != tracker.RankOrigin || document != tracker.RankOrigin {
		t.Fatalf("a version-7 edit after a drag left the row at %q and the "+
			"document at %q; the build that wrote it re-files the card at %q, "+
			"and a node applying it any other way disagrees with that build "+
			"about this row for good", column, document, tracker.RankOrigin)
	}

	drag("drag-2")
	edit("edit-new", tracker.RecordVersion, true)
	if column, document := place(); column != dragged[0] || document != dragged[0] {
		t.Fatalf("a version-%d edit after a drag left the row at %q and the "+
			"document at %q, want the dragged key %q on both",
			tracker.RecordVersion, column, document, dragged[0])
	}
}

// EVERY TASK EDIT THIS BUILD WRITES SAYS IT KEEPS ITS PLACE — and is therefore
// stamped at a version a build still re-filing the rank retains rather than
// applies by its own rule.
func TestEveryTaskEditIsStampedToKeepItsPlace(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fourCards(t, r)
	high := tracker.PriorityHigh
	res, err := r.writer.UpdateTask(t.Context(), "op-edit", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Priority: &high}, tracker.ChangeFields, nil)
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	_, payload, _, ok, err := r.log.At(t.Context(), res.Position.Seq)
	if err != nil || !ok {
		t.Fatalf("read the edit back: ok=%v err=%v", ok, err)
	}
	record, err := tracker.Decode(payload)
	if err != nil {
		t.Fatalf("decode the edit: %v", err)
	}
	if !record.KeepsPlace || record.V < tracker.RecordVersion {
		t.Fatalf("a task edit was written at version %d with keeps_place=%v — "+
			"a build reading 7 would apply it by re-filing the card's rank",
			record.V, record.KeepsPlace)
	}
}

// A DROP BESIDE A CARD IN ANOTHER PROJECT, OR IN THE TRASH, IS REFUSED.
//
// The neighbour is read inside the order's own snapshot, and a neighbour that
// is not on this board any more bounds no gap in it: a key minted beside it
// would put the card somewhere nobody dropped it.
func TestADropBesideACardNotOnTheBoardIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations"})
	fourCards(t, r)
	elsewhere := newTask("o-1")
	elsewhere.Project, elsewhere.Key = "OPS", "OPS-1"
	if _, err := r.writer.CreateTask(t.Context(), "op-o-1", elsewhere, nil); err != nil {
		t.Fatalf("CreateTask o-1: %v", err)
	}
	r.drain()
	refused := func(opID string, move tracker.Move, what string) {
		t.Helper()
		order := strings.Join(boardOrder(t, r), ",")
		if _, err := r.writer.MoveTask(t.Context(), opID, move, nil); !errors.Is(err, statelog.ErrConflict) {
			t.Errorf("a drop beside a card %s answered %v, want ErrConflict", what, err)
		}
		r.drain()
		if got := strings.Join(boardOrder(t, r), ","); got != order {
			t.Errorf("a refused drop beside a card %s moved the board from %s "+
				"to %s", what, order, got)
		}
	}

	refused("op-elsewhere", tracker.Move{Task: "t-1", Project: "ENG", Before: "o-1"},
		"in another project")

	if _, err := r.writer.RemoveTask(t.Context(), "op-trash", "t-3", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()
	refused("op-trashed", tracker.Move{Task: "t-1", Project: "ENG", After: "t-3"},
		"in the trash")
}

// A LANE CHANGE THAT LANDS AND A PLACE THAT DOES NOT IS REPORTED, NOT FAILED.
//
// A move across lanes is two appends. When somebody writes the task between
// them, the placement — conditioned on the version the lane change wrote — is
// refused, and the lane change stands: the call answers what landed and names
// what did not, because a caller told "failed" would drag the card back.
func TestALaneChangeWhosePlaceLostARaceStands(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fourCards(t, r)
	version := r.task(t, "t-4").Task.Version
	order := strings.Join(boardOrder(t, r), ",")

	// THE INTERLOPER, run from inside the placement's wait for the lane
	// change: this node applies the lane change, then somebody edits the
	// card before the order's decide reads it.
	var mu sync.Mutex
	interloped := false
	r.waiter.mu.Lock()
	r.waiter.advance = func() {
		r.drain()
		mu.Lock()
		first := !interloped
		interloped = true
		mu.Unlock()
		if !first {
			return
		}
		high := tracker.PriorityHigh
		if _, err := r.writer.UpdateTask(t.Context(), "op-interloper", "t-4", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Priority: &high},
			tracker.ChangeFields, nil); err != nil {
			t.Errorf("the interloping edit: %v", err)
		}
		r.drain()
	}
	r.waiter.mu.Unlock()

	active := tracker.StatusInProgress
	got, err := r.writer.MoveTask(t.Context(), "op-lane", tracker.Move{
		Task: "t-4", Project: "ENG", After: "t-1", Status: &active, IfMatch: version,
	}, nil)
	if err != nil {
		t.Fatalf("a lane change whose placement lost a race failed the call: %v", err)
	}
	if !interloped {
		t.Fatal("the placement never waited for the lane change, so nothing " +
			"could write between the two appends — the case proves nothing")
	}
	if !got.Lane.Wrote() {
		t.Fatalf("the lane change answered %v", got.Lane.Outcome)
	}
	if got.Unplaced == nil || got.Order.Wrote() {
		t.Fatalf("the placement landed on a card edited after its lane changed "+
			"(unplaced=%v, order=%v)", got.Unplaced, got.Order.Outcome)
	}
	r.drain()
	if status := r.task(t, "t-4").Task.Status; status != active {
		t.Errorf("t-4 is %s — the lane change is the half that landed, and "+
			"it must stand", status)
	}
	if now := strings.Join(boardOrder(t, r), ","); now != order {
		t.Errorf("the board moved from %s to %s though the placement was refused",
			order, now)
	}
}
