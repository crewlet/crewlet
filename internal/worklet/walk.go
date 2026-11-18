package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The paced walks: the work a duty finishes that a gesture could not. A walk
// is many batches over minutes and will be interrupted, so EVERY INTERMEDIATE
// STATE MUST BE CORRECT, not merely repairable (stronger than idempotence): a
// re-spread keeps the board ordered at every batch boundary, and a move's
// completion skips what already carries its destination.

const (
	// WritePaceBytes is how fast a walk may publish, in bytes per second.
	// A walk shares the SAME serial applier on every data node with the
	// company's writes, so its cost is how long they wait behind it.
	// 1.4 MiB/s is a tenth of the log's steady rate at the design corpus:
	// a large project's re-spread fits in a minute and leaves nine tenths
	// of the applier for real work.
	WritePaceBytes = 1_468_006

	// WalkPace is the floor under that rate: the shortest gap between
	// batches. It is the applier's own linger, so a walk at the floor costs
	// at most one batch per apply transaction.
	WalkPace = 250 * time.Millisecond
)

// pace is how long to wait before the next batch of a walk, derived from the
// bytes just published: a fixed sleep would price 64 maximal records and 64
// rank placements alike, three orders of magnitude apart in applier cost.
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
// Minted up front so the board is correct after every batch: batch k takes the
// k-th ascending slice of the reserve, so moved rows always hold the lowest
// keys in their original order. Minting per batch would squeeze each batch
// between the last and the untouched rows, in a gap that halves every time.
// The reserve is one integer position BELOW the frozen minimum: upward, two
// walks with no create between them subdivide the same position and keys grow,
// the condition the walk exists to remove.
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

// respreadRowsStatement is [PlanRespread]'s read of a project's long keys, in
// key order, through the index built for them ([longRank] says why it is
// named). Named for the plan test; a variable because [longRank] spells its
// threshold.
var respreadRowsStatement = `
		SELECT id, rank FROM tracker_tasks INDEXED BY tracker_tasks_respread_idx
		WHERE project_key = ? AND removed_at IS NULL AND ` + longRank("rank") + `
		ORDER BY rank, id`

// lowestRankStatement is [PlanRespread]'s read of the project's lowest live
// rank, named for the plan test.
const lowestRankStatement = `
		SELECT MIN(rank) FROM tracker_tasks
		WHERE project_key = ? AND removed_at IS NULL`

// PlanRespread mints a project's whole re-spread from ONE snapshot (the long
// keys in order, and the project's minimum); the rest is arithmetic, so the
// assignment is identical on whichever node completes the walk.
func PlanRespread(ctx context.Context, db store.ReplicatedReader, project string) (RespreadPlan, error) {
	var rows []Placement
	var lowest Rank
	err := db.Read(ctx, func(tx *sql.Tx) error {
		// Only the long keys: rewriting a short one spends a record for
		// nothing.
		found, err := tx.QueryContext(ctx, respreadRowsStatement, project)
		if err != nil {
			return fmt.Errorf("worklet: read %s's re-spread rows: %w", project, err)
		}
		defer found.Close()
		for found.Next() {
			var p Placement
			var rank string
			if err := found.Scan(&p.Task, &rank); err != nil {
				return fmt.Errorf("worklet: read a re-spread row: %w", err)
			}
			p.Rank = Rank(rank)
			rows = append(rows, p)
		}
		if err := found.Err(); err != nil {
			return err
		}
		var min sql.NullString
		if err := tx.QueryRowContext(ctx, lowestRankStatement, project).
			Scan(&min); err != nil {
			return fmt.Errorf("worklet: read %s's lowest rank: %w", project, err)
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
	// Inside the reserve, not between it and the minimum: bisecting up to
	// the bound inherits its length and would lengthen keys. See
	// [RespreadKeys].
	keys, err := RespreadKeys(reserve, len(rows))
	if err != nil {
		return RespreadPlan{}, fmt.Errorf("worklet: mint %d keys inside %q for "+
			"%s's re-spread: %w", len(rows), reserve, project, err)
	}
	placements := make([]Placement, 0, len(rows))
	for i, row := range rows {
		placements = append(placements, Placement{Task: row.Task, Rank: keys[i]})
	}
	return RespreadPlan{Project: project, Placements: placements}, nil
}

// reserveBelow is the integer position the whole walk subdivides: the
// minimum's integer part decremented once, a position no create can take,
// since creates count up from the origin and this walks down from the order's
// start.
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
		return "", fmt.Errorf("worklet: %q has no position below it; the "+
			"magnitude head has borrowed as far as it goes, which is ≈ 30 head "+
			"placements deep and means the order itself needs rebuilding "+
			"rather than re-spreading: %w", floor, err)
	}
	return Rank(below), nil
}

// Respread runs a project's whole re-spread walk, paced. One record per batch
// on the project's rank order subject, so each batch is arbitrated against the
// last: two nodes running the walk contend at the broker and exactly one
// proceeds, rather than interleaving batches.
func (w *Writer) Respread(ctx context.Context, opID, project string) (int, error) {
	if w.db.IsZero() {
		return 0, fmt.Errorf("worklet: this writer has no store, so it cannot " +
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
			return batch, fmt.Errorf("worklet: re-spread %s, batch %d of %d: %w",
				project, batch, plan.Batches(), err)
		}
		// Paced by what it just cost every peer ([WritePaceBytes]).
		select {
		case <-ctx.Done():
			return batch, ctx.Err()
		case <-time.After(pace(len(placements) * respreadRecordBytes)):
		}
	}
	return plan.Batches(), nil
}

// publishBatch publishes one batch, waiting out this node's own lag rather
// than failing on it. A walk is the one writer that must not give up on
// `behind`: each batch arbitrates against the last, so being behind the
// previous batch is ordinary here, and giving up would abandon a half-finished
// order.
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
	return fmt.Errorf("worklet: %s's re-spread could not publish a batch in %d "+
		"attempts because this node stayed behind its own previous one — the "+
		"applier is not draining, and the walk stops rather than leaving the "+
		"order half rewritten by a node that cannot see it", project, walkRetries)
}

// walkRetries is how many times a batch waits for this node's own applier:
// sixteen linearly growing waits from [WalkPace] total ≈ 34 s, longer than any
// drain the design prices and shorter than the stall grace that moves the seat.
const walkRetries = 16

// respreadRecordBytes is one placement's measured cost on the wire: a uuid, a
// rank key and the JSON around them.
const respreadRecordBytes = 96
