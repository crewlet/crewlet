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

// RespreadPlan is a project's whole re-spread, minted ONCE at its start.
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
type RespreadPlan struct {
	// Project is whose order this rewrites.
	Project string

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
// It reads ONE snapshot: the rows whose keys are past the threshold, in
// ascending order, and the project's current minimum. Everything after that is
// arithmetic — which is what makes the assignment a pure function of the plan
// and therefore identical on whichever node completes the walk.
func PlanRespread(ctx context.Context, db *store.DB, project string) (RespreadPlan, error) {
	var rows []Placement
	var lowest Rank
	err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		// ONLY THE LONG ONES: a key already under the threshold costs
		// nothing to leave where it is, and rewriting it would spend a
		// record to change nothing.
		found, err := tx.QueryContext(ctx, `
			SELECT id, rank FROM tracker_tasks
			WHERE project_key = ? AND removed_at IS NULL AND length(rank) > ?
			ORDER BY rank, id`, project, RankRenormaliseAt)
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
		var min sql.NullString
		if err := tx.QueryRowContext(ctx, `
			SELECT MIN(rank) FROM tracker_tasks
			WHERE project_key = ? AND removed_at IS NULL`, project).
			Scan(&min); err != nil {
			return fmt.Errorf("tracker: read %s's lowest rank: %w", project, err)
		}
		if min.Valid {
			lowest = Rank(min.String)
		}
		return nil
	})
	if err != nil {
		return RespreadPlan{}, err
	}
	if len(rows) == 0 {
		return RespreadPlan{Project: project}, nil
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
	return RespreadPlan{Project: project, Placements: placements}, nil
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

// Respread runs a project's whole re-spread walk, paced.
//
// ONE RECORD PER BATCH on the project's own rank order subject, so every batch
// is arbitrated against the last — two nodes running this walk contend at the
// broker and exactly one proceeds, which is what stops them interleaving
// batches into an order neither intended.
func (w *Writer) Respread(ctx context.Context, opID, project string) (int, error) {
	if w.db == nil {
		return 0, fmt.Errorf("tracker: this writer has no store, so it cannot " +
			"read the order a re-spread rewrites")
	}
	plan, err := PlanRespread(ctx, w.db, project)
	if err != nil {
		return 0, err
	}
	for batch := range plan.Batches() {
		placements := plan.Batch(batch)
		if err := w.publishBatch(ctx,
			stepID(opID, fmt.Sprintf("r%d", batch)), project, placements); err != nil {
			return batch, fmt.Errorf("tracker: re-spread %s, batch %d of %d: %w",
				project, batch, plan.Batches(), err)
		}
		// PACED BY WHAT IT JUST COST EVERY PEER: a walk competes with
		// the company's own writes for the same serial applier on every
		// node, so its cost is not its own latency but how long every
		// other write waits behind it.
		select {
		case <-ctx.Done():
			return batch, ctx.Err()
		case <-time.After(pace(len(placements) * respreadRecordBytes)):
		}
	}
	return plan.Batches(), nil
}

// publishBatch publishes one batch, waiting out this node's own lag rather
// than failing on it.
//
// A WALK IS THE ONE WRITER THAT MUST NOT GIVE UP ON `behind`. Every batch
// arbitrates against the last, so the second one cannot be decided until this
// node has applied the first — which is ORDINARY during a walk rather than a
// fault, and a caller told "behind" here would abandon a half-finished order
// that a duty then has to be asked to complete.
func (w *Writer) publishBatch(ctx context.Context, opID, project string,
	placements []Placement) error {

	for attempt := range walkRetries {
		_, err := w.MoveTasks(ctx, opID, project, placements)
		var unavailable *statelog.Unavailable
		switch {
		case err == nil:
			return nil
		case !errors.As(err, &unavailable) || unavailable.Reason != statelog.ReasonBehind:
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * WalkPace):
		}
	}
	return fmt.Errorf("tracker: %s's re-spread could not publish a batch in %d "+
		"attempts because this node stayed behind its own previous one — the "+
		"applier is not draining, and the walk stops rather than leaving the "+
		"order half rewritten by a node that cannot see it", project, walkRetries)
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
