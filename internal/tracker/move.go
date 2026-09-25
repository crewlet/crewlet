package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A board drag: one card dropped beside another, in its own lane or in the
// next one.
//
// # Two objects, and therefore two records when the lane changes
//
// The ORDER is the project's (see [Writer.MoveTasks] for why it has a subject
// of its own) and the LANE is the task's status — a field of the task, whose
// change is history, wakes the people on the task, moves its closure and is
// what a caller's `if_match` is about. One record has one subject and can be
// arbitrated on only one of them: carried on the order, a status change would
// skip the task's own arbitration, history and wake; carried on the task, a
// placement would stop contending with the drag beside it in the same project,
// and a key minted from neighbours nobody arbitrated is a guess. So a move
// that changes lanes is a SEQUENCE of two appends, stated the way
// sequence.go states the others:
//
//	A on the task (the status, conditioned on if_match) → A on the order
//	(the placement, conditioned on the version that status wrote).
//
// ORDER: the lane first, because it is the half the caller's precondition
// guards — a stale drag is refused before anything lands. CLAIM: none; each
// append is arbitrated on its own subject, and two people dragging one card
// contend at the task and then at the order like any two writers. CRASH
// RESIDUE: a card in its new lane at the place its old key puts it — a valid
// board somebody can see and drag again, not a half-state anything reads as
// broken. REPAIRER: nobody, for that reason; and the call REPORTS the residue
// ([MoveResult.Unplaced]) rather than failing, because the lane did change
// and a caller told "failed" about it would drag it back.
//
// # The key is minted INSIDE the decide
//
// A move names the NEIGHBOUR it was dropped beside, never a key. The two keys
// either side of the gap are read in the same snapshot the broker's
// expectation is formed from, so a drag that lost a race to another drag in
// the same project is re-decided against the order that won rather than
// landing between two keys that no longer bound anything. A key minted by the
// caller from the order it last rendered is the "guess" the write authority
// forbids — which is what the move this replaced did, with the neighbours'
// keys as its arguments and no caller at all.

// Move is one drag.
type Move struct {
	// Task and Project name the card, by id. Project is the one the caller
	// read it in, and a card that has left it since is refused rather than
	// placed in a board the caller was not looking at.
	Task    string
	Project string

	// Before and After name the neighbour it was dropped beside, by task
	// id — at most one of them. Before is the card it now sits above, After
	// the card it now sits below. Neither, with a Status, is a drop into a
	// lane that is empty: the lane changes and the card keeps its key.
	Before string
	After  string

	// Status is the lane it was dropped into; nil keeps its own.
	Status *Status

	// IfMatch is the task version the caller read, refused if the card
	// moved since. Zero omits it. A PLACEMENT'S OWN RECORD NEVER MOVES IT —
	// the order stamps `scoped_through` — so a person dragging one card
	// twice is not refused by their own first drag.
	IfMatch uint64
}

// MoveResult is what a drag wrote.
type MoveResult struct {
	// Lane is the status change, zero when the move changed no lane.
	Lane WriteResult

	// Order is the placement, zero when the card already sat in the gap.
	Order WriteResult

	// Rank is the card's new key, empty when it did not move.
	Rank Rank

	// Unplaced is why the card changed lanes and did NOT take its place:
	// the placement was refused after the status landed (a writer moved
	// the task between the two appends), or the status write's own
	// outcome is unknown and there is no version to condition the
	// placement on. Nil whenever the placement landed or none was asked.
	Unplaced error
}

// MoveTask drops one card beside a neighbour, changing its lane when the move
// names one.
func (w *Writer) MoveTask(ctx context.Context, opID string, move Move,
	notify *Notify) (MoveResult, error) {

	switch {
	case move.Task == "":
		return MoveResult{}, invalid("tracker: a move names no task")
	case move.Project == "":
		return MoveResult{}, invalid("tracker: a move of task %s names no "+
			"project — the caller resolved a key to reach it and therefore "+
			"holds one", move.Task)
	case move.Before != "" && move.After != "":
		return MoveResult{}, invalid("tracker: a move names both a card to " +
			"sit above and a card to sit below — name the one it was dropped " +
			"beside")
	case move.Before == move.Task || move.After == move.Task:
		return MoveResult{}, invalid("tracker: task %s cannot be placed "+
			"beside itself", move.Task)
	case move.Before == "" && move.After == "" && move.Status == nil:
		return MoveResult{}, invalid("tracker: a move of task %s names no "+
			"neighbour and no lane, so it moves nothing", move.Task)
	case move.Status != nil && !move.Status.Valid():
		return MoveResult{}, invalid("tracker: %q is not a status", *move.Status)
	}

	var out MoveResult
	placer, ifMatch := w, move.IfMatch
	if move.Status != nil {
		lane, err := w.UpdateTask(ctx, stepID(opID, "lane"), move.Task,
			move.Project, move.IfMatch, TaskPatch{Status: move.Status},
			ChangeStatus, notify)
		if err != nil {
			return MoveResult{}, err
		}
		out.Lane = lane
		if move.Before == "" && move.After == "" {
			return out, nil
		}
		if lane.Position.IsZero() {
			// AN UNKNOWN HAS NO POSITION AND THEREFORE NO VERSION, and
			// a placement conditioned on nothing could land on a task
			// somebody else has since moved.
			out.Unplaced = fmt.Errorf("tracker: the lane change of %s is %s, "+
				"so there is no version to place it against — read it again "+
				"and drag it once the change has resolved", move.Task,
				lane.Outcome)
			return out, nil
		}
		// THE VERSION THE LANE CHANGE WROTE is the placement's
		// precondition, and the writer waits for this node's applier to
		// reach it — otherwise the decide would read the task as it was
		// before the first append and refuse its own sequence.
		placer = w.After(lane.Position)
		ifMatch = uint64(lane.Position.Packed())
	}

	var rank Rank
	order, err := placer.publishOrder(ctx, stepID(opID, "order"), move.Project,
		func(tx *sql.Tx) ([]Placement, error) {
			placements, err := placeMove(ctx, tx, move, ifMatch)
			rank = ""
			for _, p := range placements {
				if p.Task == move.Task {
					rank = p.Rank
				}
			}
			return placements, err
		})
	switch {
	case err != nil && move.Status != nil:
		out.Unplaced = err
		return out, nil
	case err != nil:
		return MoveResult{}, err
	}
	out.Order, out.Rank = order, rank
	return out, nil
}

// placeMove reads the gap inside the order's own snapshot and decides the
// placements — see [PlaceInGap].
func placeMove(ctx context.Context, tx *sql.Tx, move Move, ifMatch uint64) ([]Placement, error) {
	current, held, err := readTask(ctx, tx, move.Task)
	switch {
	case err != nil:
		return nil, err
	case !held:
		return nil, fmt.Errorf("tracker: task %s is not on this node: %w",
			move.Task, statelog.ErrUnavailable)
	case current.Removed != nil:
		return nil, invalid("tracker: task %s was removed by %s at %s; "+
			"restore it first", move.Task, current.Removed.By,
			current.Removed.At.Format(time.RFC3339))
	case ifMatch != 0 && current.Version != ifMatch:
		return nil, fmt.Errorf("%w: task %s is at version %d and the move "+
			"was conditioned on %d — the card changed since the board was "+
			"drawn", ErrStaleVersion, move.Task, current.Version, ifMatch)
	case current.Project != move.Project:
		return nil, fmt.Errorf("%w: task %s is in %s now, not %s",
			statelog.ErrConflict, move.Task, current.Project, move.Project)
	}

	anchor := move.Before
	if anchor == "" {
		anchor = move.After
	}
	if anchor == "" {
		// A LANE CHANGE INTO AN EMPTY LANE keeps the card's key: there is
		// nobody in the lane to sit beside.
		return nil, nil
	}
	neighbour, held, err := readTask(ctx, tx, anchor)
	switch {
	case err != nil:
		return nil, err
	case !held:
		return nil, fmt.Errorf("%w: %s, the card %s was dropped beside, is "+
			"gone", statelog.ErrConflict, anchor, move.Task)
	case neighbour.Removed != nil:
		return nil, fmt.Errorf("%w: %s, the card %s was dropped beside, is in "+
			"the trash", statelog.ErrConflict, neighbour.Key, move.Task)
	case neighbour.Project != move.Project:
		return nil, fmt.Errorf("%w: %s, the card %s was dropped beside, is in "+
			"%s now, not %s", statelog.ErrConflict, neighbour.Key, move.Task,
			neighbour.Project, move.Project)
	}

	// THE NEIGHBOUR ITSELF IS ON THE SIDE THE DROP PUT IT: after the gap
	// in the order for `before`, ahead of it for `after`.
	pivot := Placement{Task: neighbour.ID, Rank: neighbour.Rank}
	below, err := gapSide(ctx, tx, move, pivot, false, move.After != "")
	if err != nil {
		return nil, err
	}
	above, err := gapSide(ctx, tx, move, pivot, true, move.Before != "")
	if err != nil {
		return nil, err
	}
	placements, err := PlaceInGap(Placement{Task: move.Task, Rank: current.Rank},
		below, above)
	if err != nil {
		// NOT THE CALLER'S TO FIX, and not a race either: the gap holds
		// keys only the duty's duplicate repair can pull apart.
		return nil, fmt.Errorf("tracker: %s cannot be placed beside %s until "+
			"the tracker duty has repaired the keys around it: %w",
			move.Task, neighbour.Key, err)
	}
	return placements, nil
}

// gapSide reads one side of the gap a drop opened, nearest first: the rows
// below the pivot (descending) or above it (ascending), the pivot itself
// included when the drop put it on this side.
//
// OVER EVERY ROW OF THE PROJECT, the trash included: a removed task still
// holds its key and a restore brings it back to exactly that place, so a key
// minted over it would be a duplicate the day it returns. And in (rank, id)
// order, which is the order a board with a shared key renders in.
func gapSide(ctx context.Context, tx *sql.Tx, move Move, pivot Placement,
	above, inclusive bool) ([]Placement, error) {

	cmp, dir := "<", "DESC"
	if above {
		cmp, dir = ">", "ASC"
	}
	tie := cmp
	if inclusive {
		tie += "="
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, rank FROM tracker_tasks
		WHERE project_key = ? AND id <> ?
		  AND (rank `+cmp+` ? OR (rank = ? AND id `+tie+` ?))
		ORDER BY rank `+dir+`, id `+dir+` LIMIT ?`,
		move.Project, move.Task, string(pivot.Rank), string(pivot.Rank),
		pivot.Task, RankRespreadInline/2+1)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the order beside %s in %s: %w",
			pivot.Task, move.Project, err)
	}
	defer rows.Close()
	var side []Placement
	for rows.Next() {
		var p Placement
		var rank string
		if err := rows.Scan(&p.Task, &rank); err != nil {
			return nil, fmt.Errorf("tracker: read a neighbour of %s: %w",
				pivot.Task, err)
		}
		p.Rank = Rank(rank)
		side = append(side, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the order beside %s in %s: %w",
			pivot.Task, move.Project, err)
	}
	return side, nil
}
