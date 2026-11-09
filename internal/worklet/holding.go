package worklet

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// HOLDING UP: the open work a person holds that other open work waits on, the
// most waited on first. Every nudge the engine has is aimed at agent seats, so
// a person's held item can stall a team with nothing saying so; this says it.

// MaxHoldingWalk bounds how many waiting items one walk counts: far past any
// chain a company keeps open, and a bound because the walk follows the graph.
const MaxHoldingWalk = 512

// MaxHoldingCandidates bounds how many held blockers are walked before the
// page is ranked. Five pages: a person holding more blockers than this has a
// list nobody ranks by reading it, and the total still counts them all.
const MaxHoldingCandidates = 5 * MyWorkRows

// MaxHoldingHolders bounds the names a row carries of who waits on it.
const MaxHoldingHolders = 8

// HoldingRow is one open item a person holds that other open work waits on.
type HoldingRow struct {
	TaskRow

	// Blocks is how many open items wait on it, directly or through
	// another waiting item, each counted once.
	Blocks int `json:"blocks"`
	// BlocksCapped says the count stopped at [MaxHoldingWalk].
	BlocksCapped bool `json:"blocks_capped,omitempty"`
	// Holders is who holds the waiting work, the person left out, sorted,
	// at most [MaxHoldingHolders].
	Holders []string `json:"holders"`
	// HeldSince is when it was last assigned, or when it was filed where
	// it was filed already assigned.
	HeldSince time.Time `json:"held_since"`
}

// holdingBlock is the person's open work that open work waits on.
func holdingBlock(handle string) blockQuery {
	open := openGroups()
	return blockQuery{
		where: "t.removed_at IS NULL AND t.assignee = ? AND t.status_group IN (" +
			placeholders(len(open)) + ") AND EXISTS (SELECT 1 FROM tracker_task_deps d " +
			"JOIN tracker_tasks u ON u.id = d.task_id WHERE d.blocker_id = t.id " +
			"AND d.blocker_open = 1 AND u.removed_at IS NULL AND u.status_group IN (" +
			placeholders(len(open)) + "))",
		args: append(append([]any{handle}, open...), open...),
		terms: []sortTerm{
			{Column: "t.prio_rank", Descending: true},
			{Column: "t.due_at"}, {Column: "t.id"},
		},
	}
}

// holdingWaitersStatement is one step of the walk: the open items waiting on
// one item, and who holds each. Seeks the dependency key's blocker prefix.
func holdingWaitersStatement() (string, []any) {
	open := openGroups()
	return `SELECT d.task_id, u.assignee FROM tracker_task_deps d
		JOIN tracker_tasks u ON u.id = d.task_id
		WHERE d.blocker_id = ? AND d.blocker_open = 1 AND u.removed_at IS NULL
		  AND u.status_group IN (` + placeholders(len(open)) + `)`, open
}

// holdingSinceStatement is an item's newest assignment, or failing one its
// first record, which is its filing.
const holdingSinceStatement = `SELECT h.created_at FROM tracker_history h
	WHERE h.subject_id = ? AND h.kind IN ('assignee', 'created')
	ORDER BY CASE h.kind WHEN 'assignee' THEN 0 ELSE 1 END, h.log_seq DESC LIMIT 1`

// readHolding reads the block: every held blocker up to
// [MaxHoldingCandidates], each walked, ranked by how much waits on it, then
// the page.
func readHolding(ctx context.Context, tx *sql.Tx, handle string,
	dayStart time.Time) ([]HoldingRow, ClaimTotal, error) {

	block := holdingBlock(handle)
	set := tasksWhere(block.where, block.args)
	candidates, _, err := readTasks(ctx, tx, set, block.terms, MaxHoldingCandidates, dayStart)
	if err != nil {
		return nil, ClaimTotal{}, err
	}
	total, capped, err := countHint(ctx, tx, set)
	if err != nil {
		return nil, ClaimTotal{}, err
	}
	rows := make([]HoldingRow, 0, len(candidates))
	for _, task := range candidates {
		row, err := walkHolding(ctx, tx, handle, task)
		if err != nil {
			return nil, ClaimTotal{}, err
		}
		rows = append(rows, row)
	}
	// STABLE, so items that block as much keep the queue's own order.
	slices.SortStableFunc(rows, func(a, b HoldingRow) int { return cmp.Compare(b.Blocks, a.Blocks) })
	if len(rows) > MyWorkRows {
		rows = rows[:MyWorkRows]
	}
	return rows, ClaimTotal{Total: total, Capped: capped}, nil
}

// walkHolding counts what waits on task, breadth first and each item once,
// so a dependency cycle ends the walk rather than looping it.
func walkHolding(ctx context.Context, tx *sql.Tx, handle string, task TaskRow) (HoldingRow, error) {
	row := HoldingRow{TaskRow: task, Holders: []string{}}
	statement, open := holdingWaitersStatement()
	seen := map[string]bool{task.ID: true}
	holders := map[string]bool{}
	frontier := []string{task.ID}
	for len(frontier) > 0 && !row.BlocksCapped {
		next := frontier[0]
		frontier = frontier[1:]
		waiters, err := tx.QueryContext(ctx, statement, append([]any{next}, open...)...)
		if err != nil {
			return HoldingRow{}, fmt.Errorf("worklet: read what waits on %s: %w", next, err)
		}
		for waiters.Next() {
			var id, assignee string
			if err := waiters.Scan(&id, &assignee); err != nil {
				_ = waiters.Close()
				return HoldingRow{}, fmt.Errorf("worklet: scan what waits on %s: %w", next, err)
			}
			if seen[id] {
				continue
			}
			if row.Blocks == MaxHoldingWalk {
				row.BlocksCapped = true
				break
			}
			seen[id] = true
			row.Blocks++
			frontier = append(frontier, id)
			if assignee != "" && assignee != handle {
				holders[assignee] = true
			}
		}
		if err := waiters.Err(); err != nil {
			_ = waiters.Close()
			return HoldingRow{}, fmt.Errorf("worklet: walk what waits on %s: %w", next, err)
		}
		if err := waiters.Close(); err != nil {
			return HoldingRow{}, fmt.Errorf("worklet: close the walk of %s: %w", next, err)
		}
	}
	for _, h := range slices.Sorted(maps.Keys(holders)) {
		if len(row.Holders) == MaxHoldingHolders {
			break
		}
		row.Holders = append(row.Holders, h)
	}
	var since int64
	err := tx.QueryRowContext(ctx, holdingSinceStatement, task.ID).Scan(&since)
	switch {
	case err == nil:
		row.HeldSince = store.DecodeTime(since)
	case !errors.Is(err, sql.ErrNoRows):
		return HoldingRow{}, fmt.Errorf("worklet: read when %s was assigned: %w", task.ID, err)
	}
	return row, nil
}
