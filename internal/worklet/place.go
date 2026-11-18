package worklet

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A board drag ([PlaceWorkItemTool]): one card dropped beside another, in its
// own lane or the next. A move to another project is [Writer.MoveTaskToProject].
//
// A drop that changes lanes is a SEQUENCE of two appends, as sequence.go states
// the others: A on the task (the status, conditioned on if_match) → A on the
// order (the placement, conditioned on the version that status wrote). Two,
// because the order is the project's subject ([Writer.MoveTasks]) and the lane
// the task's status, with its own arbitration, history and wake; one record can
// be arbitrated on only one. ORDER: the lane first, the half if_match guards.
// CLAIM: none. CRASH RESIDUE: a card in its new lane at its old key's place, a
// valid board. REPAIRER: none; the call reports it ([PlaceResult.Unplaced])
// rather than failing, since a caller told "failed" would drag the lane back.

// Place is one drag: where a card was dropped, as [Writer.PlaceTask] takes it.
type Place struct {
	// Task and Project name the card, by id. A card that has left Project
	// since is refused, not placed in a board the caller was not looking at.
	Task    string
	Project string

	// Before and After name the neighbour it was dropped beside, by task id,
	// at most one: Before the card it now sits above, After the one below.
	// Neither, with a Status, is a drop into an empty lane: the card keeps
	// its key.
	Before string
	After  string

	// Status is the lane it was dropped into, as dropped, never compared
	// with the card first; nil keeps its own. Both steps ("lane", "order")
	// are named under the operation id, so a retry is answered from the
	// ledger step by step; a caller that left Status out once its first
	// attempt's lane landed would condition the placement on the version
	// before that change and be refused as stale by itself. A drop into the
	// card's own lane writes nothing on the task ([laneOnly]), so sending it
	// is always safe.
	Status *Status

	// IfMatch is the task version the caller read, refused if the card
	// moved since; zero omits it. A placement's own record never moves it
	// (the order stamps `scoped_through`), so a second drag of one card is
	// not refused by the first.
	IfMatch uint64
}

// PlaceResult is what a drag wrote.
type PlaceResult struct {
	// Lane is the status change, zero when the drop named no lane. A lane
	// the card was already in is an APPLIED result with no position.
	Lane WriteResult

	// Order is the placement, zero when the card already sat in the gap.
	Order WriteResult

	// Rank is the card's new key, empty when it did not move or when the
	// placement was answered from the ledger, whose row keeps no key.
	Rank Rank

	// Version is the card's version after the call, the board's next
	// `if_match`: after a lane step of known outcome, the packed position of
	// the record it wrote (applied or still pending here), or the version it
	// read when it wrote nothing; otherwise the drop's own IfMatch, since a
	// placement never moves it.
	Version int64

	// Unplaced is why the card changed lanes and did NOT take its place:
	// the placement was refused after the status landed (a writer moved the
	// task between the appends), or the status write's outcome is unknown
	// and there is no version to condition on. Nil otherwise.
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
			// nothing could land on a task somebody has since moved. The
			// version stays the caller's: the lane change may not have landed.
			if place.Before != "" || place.After != "" {
				out.Unplaced = fmt.Errorf("worklet: the lane change of %s is "+
					"unknown (operation %s), so there is no version to place it "+
					"against — make the same drag again, which answers the lane "+
					"change from the ledger and places the card after it",
					place.Task, lane.OpID)
			}
			return out, nil
		}
		// THE VERSION THE LANE STEP LEFT THE TASK AT is the placement's
		// precondition. A record the step wrote (applied, answered from the
		// ledger, or still PENDING here) has a packed position that IS the
		// task's version, and the placer waits for this node's applier to
		// reach it, or the decide would refuse its own sequence. Not the
		// result's Version: a pending result carries none, and zero
		// conditions on nothing. A step that wrote nothing has no position,
		// and its Version is the one it read.
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
// placements ([PlaceInGap]). A drop names a neighbour, never a key: reading the
// keys either side in the snapshot the broker's expectation is formed from
// re-decides a drag that lost a race against the order that won, where a key
// the caller minted from its last render is the guess the write authority
// forbids.
func decidePlace(ctx context.Context, tx *sql.Tx, place Place,
	ifMatch uint64) ([]Placement, error) {
	current, held, err := readTask(ctx, tx, place.Task)
	switch {
	case err != nil:
		return nil, err
	case !held:
		return nil, missingTask(ctx, tx, place.Task, "it cannot be placed")
	case current.Removed != nil:
		return nil, inTheTrash(current, "")
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
		// A LANE CHANGE INTO AN EMPTY LANE keeps the card's key.
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
	// for `before`, ahead of it for `after`.
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
		// NOT THE CALLER'S TO FIX, nor a race: the gap holds keys only the
		// duty's duplicate repair can pull apart.
		return nil, fmt.Errorf("worklet: %s cannot be placed beside %s until "+
			"the Worklet duty has repaired the keys around it: %w",
			place.Task, neighbour.Key, err)
	}
	return placements, nil
}

// gapSideStatement is [gapSide]'s read of one side, named for the plan test.
//
// THE PIVOT'S RANK BOUNDS THE SEEK: written as `rank < p OR (rank = p AND
// id < t)`, the disjunction leaves the planner only the project to seek, and
// it reads the rank index from the board's far end towards the pivot, so a
// drop near a long board's bottom reads the whole board. Stated as the range
// `rank <= p` with the tie broken on the id, it is a seek on the project and
// the rank, read nearest first.
func gapSideStatement(above, inclusive bool) string {
	cmp, dir := "<", "DESC"
	if above {
		cmp, dir = ">", "ASC"
	}
	tie := cmp
	if inclusive {
		tie += "="
	}
	return `
		SELECT id, rank FROM tracker_tasks
		WHERE project_key = ? AND id <> ?
		  AND rank ` + cmp + `= ? AND (rank ` + cmp + ` ? OR id ` + tie + ` ?)
		ORDER BY rank ` + dir + `, id ` + dir + ` LIMIT ?`
}

// gapSide reads one side of the gap a drop opened, nearest first: below the
// pivot (descending) or above it (ascending), the pivot included when the
// drop put it on this side. It reads the trash too: a removed task keeps its
// key and a restore returns it there, so a key minted over it would be a
// duplicate. The order is (rank, id), as a board with a shared key renders.
func gapSide(ctx context.Context, tx *sql.Tx, place Place, pivot Placement,
	above, inclusive bool) ([]Placement, error) {

	rows, err := tx.QueryContext(ctx, gapSideStatement(above, inclusive),
		place.Project, place.Task, string(pivot.Rank), string(pivot.Rank),
		pivot.Task, RankRespreadInline/2+1)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the order beside %s in %s: %w",
			pivot.Task, place.Project, err)
	}
	defer rows.Close()
	var side []Placement
	for rows.Next() {
		var p Placement
		var rank string
		if err := rows.Scan(&p.Task, &rank); err != nil {
			return nil, fmt.Errorf("worklet: read a neighbour of %s: %w",
				pivot.Task, err)
		}
		p.Rank = Rank(rank)
		side = append(side, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: read the order beside %s in %s: %w",
			pivot.Task, place.Project, err)
	}
	return side, nil
}
