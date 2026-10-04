package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A board drag: one card dropped beside another, in its own lane or in the
// next one — `place_work_item` ([PlaceWorkItemTool]). Never a move to another
// project, which re-keys what it carries and is [Writer.MoveTaskToProject].
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
// and a key minted from neighbours nobody arbitrated is a guess. So a drop
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
// ([PlaceResult.Unplaced]) rather than failing, because the lane did change
// and a caller told "failed" about it would drag it back.
//
// # A retry is gated on the OPERATION, never on the state it produced
//
// Both steps are named ("lane", "order") under the call's operation id, so a
// retry is answered from the ledger step by step — the lane change it already
// made included. The caller therefore sends the lane it DROPPED INTO, whatever
// the card reads now: a caller that compared it with the card and left it out
// once the first attempt's lane change had landed would condition the
// placement on the version it read before that change, and be refused as
// stale by nobody but itself. A drop into the lane the card is already in, on
// a card nobody changed since, is decided here to write nothing on the task
// ([laneOnly]) — which is what makes sending it always safe.
//
// # The key is minted INSIDE the decide
//
// A drop names the NEIGHBOUR it was dropped beside, never a key. The two keys
// either side of the gap are read in the same snapshot the broker's
// expectation is formed from, so a drag that lost a race to another drag in
// the same project is re-decided against the order that won rather than
// landing between two keys that no longer bound anything. A key minted by the
// caller from the order it last rendered is the "guess" the write authority
// forbids — which is what the drag this replaced did, with the neighbours'
// keys as its arguments and no caller at all.

// Place is one drag: where a card was dropped, as [Writer.PlaceTask] takes it.
type Place struct {
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

	// Status is the lane it was dropped into, as the caller dropped it —
	// never compared with the card first (see the file's note on retries);
	// nil keeps its own.
	Status *Status

	// IfMatch is the task version the caller read, refused if the card
	// moved since. Zero omits it. A PLACEMENT'S OWN RECORD NEVER MOVES IT —
	// the order stamps `scoped_through` — so a person dragging one card
	// twice is not refused by their own first drag.
	IfMatch uint64
}

// PlaceResult is what a drag wrote.
type PlaceResult struct {
	// Lane is the status change, zero when the drop named no lane. A lane
	// the card was already in is an APPLIED result with no position: the
	// step decided there was nothing to write.
	Lane WriteResult

	// Order is the placement, zero when the card already sat in the gap.
	Order WriteResult

	// Rank is the card's new key, empty when it did not move — or when the
	// placement was answered from the ledger, whose row keeps no key.
	Rank Rank

	// Version is the card's version after the call, which is what the
	// board's next `if_match` on it states: where a lane step ran and its
	// outcome is known, the packed position of the record it wrote
	// (applied or still pending here) or, when it wrote nothing, the
	// version it read; otherwise the version the drop was conditioned on —
	// a placement never moves it.
	Version int64

	// Unplaced is why the card changed lanes and did NOT take its place:
	// the placement was refused after the status landed (a writer moved
	// the task between the two appends), or the status write's own
	// outcome is unknown and there is no version to condition the
	// placement on. Nil whenever the placement landed or none was asked.
	Unplaced error
}

// PlaceTask drops one card beside a neighbour, changing its lane when the
// drop names one.
func (w *Writer) PlaceTask(ctx context.Context, opID string, place Place,
	notify *Notify) (PlaceResult, error) {

	switch {
	case place.Task == "":
		return PlaceResult{}, invalid("a drop names no task")
	case place.Project == "":
		return PlaceResult{}, invalid("a drop of task %s names no "+
			"project — the caller resolved a key to reach it and therefore "+
			"holds one", place.Task)
	case place.Before != "" && place.After != "":
		return PlaceResult{}, invalid("a drop names both a card to " +
			"sit above and a card to sit below — name the one it was dropped " +
			"beside")
	case place.Before == place.Task || place.After == place.Task:
		return PlaceResult{}, invalid("task %s cannot be placed "+
			"beside itself", place.Task)
	case place.Before == "" && place.After == "" && place.Status == nil:
		return PlaceResult{}, invalid("a drop of task %s names no "+
			"neighbour and no lane, so it moves nothing", place.Task)
	case place.Status != nil && !place.Status.Valid():
		return PlaceResult{}, invalid("%q is not a status", *place.Status)
	}

	out := PlaceResult{Version: int64(place.IfMatch)}
	placer, ifMatch := w, place.IfMatch
	if place.Status != nil {
		lane, err := w.UpdateTask(ctx, stepID(opID, "lane"), place.Task,
			place.Project, place.IfMatch, TaskPatch{Status: place.Status},
			ChangeStatus, notify)
		if err != nil {
			return PlaceResult{}, err
		}
		out.Lane = lane
		if lane.Outcome == statelog.OutcomeUnknown {
			// AN UNKNOWN HAS NO VERSION, and a placement conditioned on
			// nothing could land on a task somebody else has since moved.
			// The version is left as the caller's: the lane change may
			// never have landed.
			if place.Before != "" || place.After != "" {
				out.Unplaced = fmt.Errorf("tracker: the lane change of %s is "+
					"unknown (operation %s), so there is no version to place it "+
					"against — make the same drag again, which answers the lane "+
					"change from the ledger and places the card after it",
					place.Task, lane.OpID)
			}
			return out, nil
		}
		// THE VERSION THE LANE STEP LEFT THE TASK AT is the placement's
		// precondition. Where the step wrote a record — applied here,
		// answered from the ledger on a retry, or durable but still PENDING
		// on this node's applier — that record's packed position IS the
		// task's version, and the writer waits for this node's applier to
		// reach it; otherwise the decide would read the task as it was
		// before the first append and refuse its own sequence. Never the
		// result's Version there: a pending result carries none, and a
		// placement conditioned on zero is conditioned on nothing. Only a
		// step that wrote nothing (the card was already in the lane) has
		// no position, and its Version is the one it read.
		if !lane.Position.IsZero() {
			out.Version = lane.Position.Packed()
			placer = w.After(lane.Position)
		} else {
			out.Version = lane.Version
		}
		ifMatch = uint64(out.Version)
		if place.Before == "" && place.After == "" {
			return out, nil
		}
	}

	var rank Rank
	order, err := placer.publishOrder(ctx, stepID(opID, "order"), place.Project,
		func(tx *sql.Tx) ([]Placement, error) {
			placements, err := decidePlace(ctx, tx, place, ifMatch)
			rank = ""
			for _, p := range placements {
				if p.Task == place.Task {
					rank = p.Rank
				}
			}
			return placements, err
		})
	switch {
	case err != nil && place.Status != nil:
		out.Unplaced = err
		return out, nil
	case err != nil:
		return PlaceResult{}, err
	}
	out.Order, out.Rank = order, rank
	return out, nil
}

// decidePlace reads the gap inside the order's own snapshot and decides the
// placements — see [PlaceInGap].
func decidePlace(ctx context.Context, tx *sql.Tx, place Place, ifMatch uint64) ([]Placement, error) {
	current, held, err := readTask(ctx, tx, place.Task)
	switch {
	case err != nil:
		return nil, err
	case !held:
		return nil, missingTask(ctx, tx, place.Task, "it cannot be placed")
	case current.Removed != nil:
		return nil, invalid("task %s was removed by %s at %s; "+
			"restore it first", place.Task, current.Removed.By,
			current.Removed.At.Format(time.RFC3339))
	case ifMatch != 0 && current.Version != ifMatch:
		return nil, fmt.Errorf("%w: task %s is at version %d and the drop "+
			"was conditioned on %d — the card changed since the board was "+
			"drawn", ErrStaleVersion, place.Task, current.Version, ifMatch)
	case current.Project != place.Project:
		return nil, fmt.Errorf("%w: task %s is in %s now, not %s",
			statelog.ErrConflict, place.Task, current.Project, place.Project)
	}

	anchor := place.Before
	if anchor == "" {
		anchor = place.After
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
			"gone", statelog.ErrConflict, anchor, place.Task)
	case neighbour.Removed != nil:
		return nil, fmt.Errorf("%w: %s, the card %s was dropped beside, is in "+
			"the trash", statelog.ErrConflict, neighbour.Key, place.Task)
	case neighbour.Project != place.Project:
		return nil, fmt.Errorf("%w: %s, the card %s was dropped beside, is in "+
			"%s now, not %s", statelog.ErrConflict, neighbour.Key, place.Task,
			neighbour.Project, place.Project)
	}

	// THE NEIGHBOUR ITSELF IS ON THE SIDE THE DROP PUT IT: after the gap
	// in the order for `before`, ahead of it for `after`.
	pivot := Placement{Task: neighbour.ID, Rank: neighbour.Rank}
	below, err := gapSide(ctx, tx, place, pivot, false, place.After != "")
	if err != nil {
		return nil, err
	}
	above, err := gapSide(ctx, tx, place, pivot, true, place.Before != "")
	if err != nil {
		return nil, err
	}
	placements, err := PlaceInGap(Placement{Task: place.Task, Rank: current.Rank},
		below, above)
	if err != nil {
		// NOT THE CALLER'S TO FIX, and not a race either: the gap holds
		// keys only the duty's duplicate repair can pull apart.
		return nil, fmt.Errorf("tracker: %s cannot be placed beside %s until "+
			"the tracker duty has repaired the keys around it: %w",
			place.Task, neighbour.Key, err)
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
func gapSide(ctx context.Context, tx *sql.Tx, place Place, pivot Placement,
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
		place.Project, place.Task, string(pivot.Rank), string(pivot.Rank),
		pivot.Task, RankRespreadInline/2+1)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the order beside %s in %s: %w",
			pivot.Task, place.Project, err)
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
			pivot.Task, place.Project, err)
	}
	return side, nil
}
