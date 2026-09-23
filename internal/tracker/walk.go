package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The paced walks: the work a duty finishes that a gesture could not.
//
// # One property they all have, and it is what makes them safe to interrupt
//
// A walk is many records, published in batches, over minutes. It will be
// interrupted — a node restarts, a lease flaps, a process is killed — so
// EVERY INTERMEDIATE STATE MUST BE CORRECT rather than merely repairable.
// That is a stronger requirement than idempotence, and it is what decides the
// shape of each one below: a re-spread rewrites keys in an order that
// preserves the board at every batch boundary, and a move's completion is a
// selection that skips what already carries its destination.

const (
	// WritePaceBytes is how fast a walk may publish.
	//
	// A walk competes with the company's own writes for the SAME serial
	// applier on every node, so its cost is not its own latency — it is
	// how long every other write on the fleet waits behind it. 1.4 MiB/s
	// is a tenth of the log's own steady rate at the design corpus, which
	// keeps a full re-spread of a large project inside a minute while
	// leaving nine tenths of the applier for work somebody is waiting on.
	WritePaceBytes = 1_468_006

	// WalkPace is the floor under that rate: a batch never goes out more
	// often than this however small it is.
	//
	// It is the applier's own linger, so a walk publishing at the floor
	// produces at most one batch per apply transaction rather than
	// several that must each be committed separately.
	WalkPace = 250 * time.Millisecond
)

// pace is how long to wait before the next batch of a walk.
//
// DERIVED FROM THE BYTES JUST PUBLISHED, not from a fixed sleep: a batch of 64
// maximal records and a batch of 64 rank placements differ by three orders of
// magnitude in what they cost every peer's applier, and a fixed interval
// prices them alike.
func pace(bytes int) time.Duration {
	if bytes <= 0 {
		return WalkPace
	}
	wait := time.Duration(float64(bytes) / WritePaceBytes * float64(time.Second))
	if wait < WalkPace {
		return WalkPace
	}
	return wait
}

// RespreadPlan is a project's whole re-spread, minted in one piece before its
// first batch.
//
// # Why the keys are all minted up front
//
// A walk has to leave a correct board after EVERY batch, and that is what
// decides this shape. Rewriting the lowest rows into the space below the
// project's minimum works for the first batch — the moved rows are the lowest
// originals and everything left is above them — and then fails for the second,
// which must land ABOVE the first batch and BELOW the untouched rows, in a gap
// that halves every time. Minting the whole run at once instead partitions the
// reserve in advance: batch k takes the k-th slice of it, in ascending order,
// so at every boundary the moved rows hold the lowest keys in their original
// order and the untouched ones hold everything above.
//
// # And the direction is load-bearing
//
// The reserve is one whole integer position BELOW the project's frozen
// minimum. Written upward instead, the walk's ceiling is the frozen maximum's
// own integer part, so two walks with no create between them subdivide the
// SAME position twice and the keys grow rather than shrink — the exact
// condition the walk exists to remove. Downward, the floor moves with the
// minimum and every walk takes an integer of its own.
//
// # And the plan knows which order it was minted from
//
// A walk takes minutes, and its keys are only right for the order they were
// minted against: a card somebody drags between two batches is an order the
// plan never saw, and the next batch would put that card back where the plan
// had it — or place the rows around it so the drop no longer sits where it
// was made. So the plan carries the order's version ([OrderVersion]) from the
// same read as its rows, and every batch is published against the version
// the batch before it produced ([Writer.MoveTasks] refuses anything else).
// A refusal means the order moved: the walk mints a fresh plan from the order
// as it now is, drop included, and starts again.
type RespreadPlan struct {
	// Project is whose order this rewrites.
	Project string

	// Order is the version of the project's order the plan was minted
	// from, read in the same transaction as its rows — the value its first
	// batch is published against.
	Order int64

	// Placements are every move the walk will make, in ascending key
	// order. Batches are consecutive slices of it.
	Placements []Placement
}

// Batches is how many records this plan publishes.
func (p RespreadPlan) Batches() int {
	return (len(p.Placements) + WalkBatch - 1) / WalkBatch
}

// Batch is the k-th slice, zero-based.
func (p RespreadPlan) Batch(k int) []Placement {
	from := k * WalkBatch
	if from >= len(p.Placements) {
		return nil
	}
	return p.Placements[from:min(from+WalkBatch, len(p.Placements))]
}

// PlanRespread mints a project's whole re-spread.
//
// It reads ONE snapshot: every row of the project, in ascending order, and
// the order's version. Everything after that is arithmetic, so two nodes that
// read the same order mint the same plan.
//
// EVERY ROW, NOT ONLY THE LONG ONES. The walk moves what it rewrites into a
// reserve BELOW the project's minimum, so a row it left at its old key would
// end up after every row it moved: a board whose long keys sat between two
// short ones would come back with the long run at its head. Rewriting every row
// in ascending order is what makes each batch boundary the original order —
// the moved rows the lowest originals, the untouched ones everything above.
//
// AND THAT INCLUDES THE REMOVED ONES. A restore brings a task back at the key
// it had, so a removed row the walk skipped kept a key above every row the
// walk moved: restored, it came back after all of them rather than where it
// was. And a removed row's key may sit inside the integer the reserve takes,
// where the walk's own keys would interleave with it. [rankAbove] counts a
// removed task's key for the same reason.
func PlanRespread(ctx context.Context, db *store.DB, project string) (RespreadPlan, error) {
	var rows []Placement
	var lowest Rank
	var order int64
	err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		if order, err = OrderVersion(ctx, tx, project); err != nil {
			return err
		}
		found, err := tx.QueryContext(ctx, `
			SELECT id, rank FROM tracker_tasks
			WHERE project_key = ?
			ORDER BY rank, id`, project)
		if err != nil {
			return fmt.Errorf("tracker: read %s's re-spread rows: %w", project, err)
		}
		defer found.Close()
		for found.Next() {
			var p Placement
			var rank string
			if err := found.Scan(&p.Task, &rank); err != nil {
				return fmt.Errorf("tracker: read a re-spread row: %w", err)
			}
			p.Rank = Rank(rank)
			rows = append(rows, p)
		}
		if err := found.Err(); err != nil {
			return err
		}
		// THE FIRST ROW IS THE MINIMUM: the read above is ordered by rank
		// over every row of the project.
		if len(rows) > 0 {
			lowest = rows[0].Rank
		}
		return nil
	})
	if err != nil {
		return RespreadPlan{}, err
	}
	if len(rows) == 0 {
		return RespreadPlan{Project: project, Order: order}, nil
	}

	reserve, err := reserveBelow(lowest)
	if err != nil {
		return RespreadPlan{}, err
	}
	// INSIDE THE RESERVE, not between the reserve and the minimum: a
	// bisected interval crowds half its keys against the upper bound and
	// inherits its length, so the walk that exists to shorten keys would
	// lengthen them. See [RespreadKeys].
	keys, err := RespreadKeys(reserve, len(rows))
	if err != nil {
		return RespreadPlan{}, fmt.Errorf("tracker: mint %d keys inside %q for "+
			"%s's re-spread: %w", len(rows), reserve, project, err)
	}
	placements := make([]Placement, 0, len(rows))
	for i, row := range rows {
		placements = append(placements, Placement{Task: row.Task, Rank: keys[i]})
	}
	return RespreadPlan{Project: project, Order: order, Placements: placements}, nil
}

// reserveBelow is the integer position the whole walk subdivides.
//
// It is the integer part of the project's minimum decremented once — a
// POSITION NO CREATE CAN EVER TAKE, because a create's key is the n-th of the
// create lattice counting up from the origin and this walks down from wherever
// the project's order already begins.
func reserveBelow(floor Rank) (Rank, error) {
	if floor == "" {
		below, err := decrementInteger(RankOrigin)
		return Rank(below), err
	}
	head, err := integerPart(string(floor))
	if err != nil {
		return "", err
	}
	below, err := decrementInteger(head)
	if err != nil {
		return "", fmt.Errorf("tracker: %q has no position below it — the "+
			"magnitude head has borrowed as far as it goes, which is ≈ 30 head "+
			"placements deep and means the order itself needs rebuilding "+
			"rather than re-spreading: %w", floor, err)
	}
	return Rank(below), nil
}

// RespreadReport is what one re-spread did.
type RespreadReport struct {
	// Batches is how many records it published, across every plan.
	Batches int

	// Plans is how many plans it minted: one more than the number of times
	// the order moved under it. See [RespreadPlans].
	Plans int
}

// Respread runs a project's whole re-spread walk, paced.
//
// ONE RECORD PER BATCH on the project's own rank order subject, each published
// against the version the one before it produced — see [RespreadPlan] — so
// nothing else can land on the order between two batches without the next one
// being refused. Two nodes running this walk at once refuse each other the
// same way, which is what stops them interleaving batches into an order
// neither intended.
//
// A REFUSED BATCH RE-PLANS rather than failing the walk: the order moved, and
// the right keys are the ones minted from it as it now is. Every intermediate
// state is still a correct board, because a batch either lands on the order
// its plan was minted from or does not land at all. [RespreadPlans] bounds how
// many times one call starts again before it leaves the rest to the next.
func (w *Writer) Respread(ctx context.Context, opID, project string) (RespreadReport, error) {
	if w.db == nil {
		return RespreadReport{}, fmt.Errorf("tracker: this writer has no " +
			"store, so it cannot read the order a re-spread rewrites")
	}
	var report RespreadReport
	for {
		plan, err := PlanRespread(ctx, w.db, project)
		if err != nil {
			return report, err
		}
		report.Plans++
		// A PLAN'S OWN OPERATION IDS: the second plan's batch k carries
		// different keys from the first plan's, and an id it shared would
		// resolve as the first plan's record having landed.
		n, err := w.walk(ctx, stepID(opID, fmt.Sprintf("p%d", report.Plans)), plan)
		report.Batches += n
		if !errors.Is(err, errReplan) {
			return report, err
		}
		if report.Plans == RespreadPlans {
			return report, fmt.Errorf("tracker: re-spread %s: the order moved "+
				"under %d plans in a row, so this walk stops and the next sweep "+
				"plans again from where it is: %w", project, RespreadPlans, err)
		}
	}
}

// RespreadPlans bounds how many plans one re-spread mints before it stops.
//
// A plan is abandoned only when something else wrote the project's order
// between two batches, and every new plan rewrites the whole project again —
// so a walk that keeps losing is a walk beside somebody who is arranging that
// board right now, re-publishing every row for each card they move. FOUR, so
// a drop or two during a walk costs a re-plan each and a board being worked on
// continuously costs four rewrites before the walk yields. It gives up nothing
// by stopping: the applier keeps the project flagged while any of its keys is
// still long, and the duty's next sweep plans again from the order as it then
// is.
const RespreadPlans = 4

// errReplan is a walk whose plan no longer describes the order.
var errReplan = errors.New("tracker: the re-spread plan no longer describes the order")

// walk publishes one plan's batches, each against the version the last one
// produced, and reports how many landed.
func (w *Writer) walk(ctx context.Context, opID string, plan RespreadPlan) (int, error) {
	against := plan.Order
	writer := w
	for batch := range plan.Batches() {
		if batch > 0 {
			// PACED BY WHAT THE LAST BATCH COST EVERY PEER: a walk
			// competes with the company's own writes for the same
			// serial applier on every node, so its cost is not its own
			// latency but how long every other write waits behind it.
			// Nothing waits after the last one, which has nothing
			// behind it to pace.
			select {
			case <-ctx.Done():
				return batch, ctx.Err()
			case <-time.After(pace(len(plan.Batch(batch-1)) * respreadRecordBytes)):
			}
		}
		placements := plan.Batch(batch)
		res, err := writer.publishBatch(ctx,
			stepID(opID, fmt.Sprintf("r%d", batch)), plan.Project, against,
			placements)
		switch {
		case errors.Is(err, ErrOrderMoved):
			return batch, fmt.Errorf("%w: batch %d of %d: %w", errReplan, batch,
				plan.Batches(), err)
		case err != nil:
			return batch, fmt.Errorf("tracker: re-spread %s, batch %d of %d: %w",
				plan.Project, batch, plan.Batches(), err)
		case res.Outcome == statelog.OutcomeUnknown:
			// THE CHAIN IS BROKEN rather than the order moved: the batch
			// may or may not have landed, so there is no version to
			// publish the next one against. A fresh plan is right either
			// way — it reads whichever order exists, and if the batch
			// lands after that read, the new plan's first batch is refused
			// like any other write that came between.
			return batch, fmt.Errorf("%w: batch %d of %d has an unknown "+
				"outcome", errReplan, batch, plan.Batches())
		}
		// THE NEXT BATCH IS DECIDED AFTER THIS ONE IS APPLIED HERE, and
		// against the version it produced: the position it landed at is
		// what the order's row records when this node applies it.
		against = res.Position.Packed()
		writer = w.After(res.Position)
	}
	return plan.Batches(), nil
}

// publishBatch publishes one batch, waiting out this node's own lag rather
// than failing on it.
//
// A WALK IS THE ONE WRITER THAT MUST NOT GIVE UP ON `behind`. Every batch is
// decided against the one before it, so the second cannot be decided until
// this node has applied the first — which is ORDINARY during a walk rather
// than a fault, and a caller told "behind" here would abandon a half-finished
// order that a duty then has to be asked to complete.
func (w *Writer) publishBatch(ctx context.Context, opID, project string,
	against int64, placements []Placement) (WriteResult, error) {

	for attempt := range walkRetries {
		res, err := w.MoveTasks(ctx, opID, project, against, placements)
		var unavailable *statelog.Unavailable
		switch {
		case err == nil:
			return res, nil
		case !errors.As(err, &unavailable) || unavailable.Reason != statelog.ReasonBehind:
			return res, err
		}
		select {
		case <-ctx.Done():
			return WriteResult{}, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * WalkPace):
		}
	}
	return WriteResult{}, fmt.Errorf("tracker: %s's re-spread could not publish "+
		"a batch in %d attempts because this node stayed behind its own "+
		"previous one — the applier is not draining, and the walk stops rather "+
		"than leaving the order half rewritten by a node that cannot see it",
		project, walkRetries)
}

// walkRetries is how many times a batch waits for this node's own applier.
//
// Sixteen at a linearly growing wait from the applier's own linger is ≈ 34 s,
// which is longer than any drain the design prices and shorter than the stall
// grace that would move the seat elsewhere.
const walkRetries = 16

// respreadRecordBytes is what one placement costs on the wire, measured on the
// record format rather than guessed: a uuid, a rank key and the JSON around
// them.
const respreadRecordBytes = 96
