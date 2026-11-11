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
// A person holds an item assigned to them, and an item whose holder put an open
// question to them (a wait on a person is an ask, ADR-0122): the item itself
// then waits on their answer, and so does everything waiting on it.

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
	// HeldSince is when it last changed hands, whichever change moved its
	// assignee ([readHandedOver]).
	HeldSince time.Time `json:"held_since"`

	// Asked is the open question to the person this row is held by, its
	// comment id, where the person holds the item by being asked rather than
	// by being assigned it: the item itself waits on their answer.
	Asked string `json:"asked,omitempty"`

	// AgentWaiting is how many agent items wait on the person through this
	// row: the waiting items agent seats hold, and the item itself when it
	// is held by being asked and an agent holds it.
	AgentWaiting int `json:"agent_waiting"`
}

// HoldingAgents is [MyWork.HoldingUp] summed over agent work: how many held
// items agent items wait on, and how many distinct agent items wait, across
// every held item walked rather than the page.
type HoldingAgents struct {
	Items      int `json:"items"`
	AgentItems int `json:"agent_items"`
	// Capped says a walk stopped at its bound or a person held more items
	// than were walked, so both counts are floors.
	Capped bool `json:"capped,omitempty"`
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

// handedOverStatement is when an item last changed hands, by whichever change
// moved its assignee (a filing, an assignment, a status change), a seek of its
// own history at the fleet-agreed instant.
const handedOverStatement = `SELECT h.effective_at FROM tracker_history h
	WHERE h.subject_id = ? AND json_extract(h.fields_json, '$.assignee.to') IS NOT NULL
	ORDER BY h.log_seq DESC LIMIT 1`

// MaxHoldingSteps bounds how many items one read asks for the waiters of,
// across every candidate: the walks share what they learn, so a graph the
// candidates have in common is read once, and past this the rows it cut say
// their counts are floors.
const MaxHoldingSteps = 2048

// heldByAsksFrom and heldByAsksWhere are the open items a person holds by an
// open question put to them, held by somebody else: the open asks' predicate,
// bound to (person, person) then the open groups.
const (
	heldByAsksFrom  = openAsksFrom
	heldByAsksWhere = openAsksWhere + ` AND t.assignee <> ? AND t.status_group IN (%s)`
)

// heldByAsksStatement is the open questions holding items, newest first: the
// first of an item's is the one its row names.
func heldByAsksStatement() (string, []any) {
	open := openGroups()
	return `SELECT c.task_id, c.id FROM ` + heldByAsksFrom + `
		WHERE ` + fmt.Sprintf(heldByAsksWhere, placeholders(len(open))) + `
		ORDER BY c.created_at DESC LIMIT ?`, open
}

// readHolding reads the block: every held blocker up to
// [MaxHoldingCandidates] (the queue's own order picks them, so for a person
// holding more the ranking is among those) and every item held by an open
// question up to the same bound, each walked, ranked by how much waits on it,
// then the page. agents are the company's agent seats, whose items the rows
// and the summary count.
func readHolding(ctx context.Context, tx *sql.Tx, handle string, agents map[string]bool,
	dayStart time.Time) ([]HoldingRow, ClaimTotal, HoldingAgents, error) {

	block := holdingBlock(handle)
	set := tasksWhere(block.where, block.args)
	candidates, more, err := readTasks(ctx, tx, set, block.terms, MaxHoldingCandidates, dayStart)
	if err != nil {
		return nil, ClaimTotal{}, HoldingAgents{}, err
	}
	total, capped, err := countHint(ctx, tx, set)
	if err != nil {
		return nil, ClaimTotal{}, HoldingAgents{}, err
	}
	asked, askedMore, err := readHeldByAsks(ctx, tx, handle, dayStart)
	if err != nil {
		return nil, ClaimTotal{}, HoldingAgents{}, err
	}
	askedTotal, askedCapped, err := countHeldByAsks(ctx, tx, handle)
	if err != nil {
		return nil, ClaimTotal{}, HoldingAgents{}, err
	}
	statement, open := holdingWaitersStatement()
	w := newHoldingWalk(handle, MaxHoldingSteps, func(id string) ([]waiter, error) {
		return readWaiters(ctx, tx, statement, open, id)
	})
	summary := HoldingAgents{Capped: more != "" || askedMore}
	seen := map[string]bool{}
	rows := make([]HoldingRow, 0, len(candidates)+len(asked))
	for _, held := range append(heldRows(candidates), asked...) {
		row, waiting, err := w.walk(held.TaskRow)
		if err != nil {
			return nil, ClaimTotal{}, HoldingAgents{}, err
		}
		row.Asked = held.Asked
		if row.HeldSince, _, err = readHandedOver(ctx, tx, held.ID); err != nil {
			return nil, ClaimTotal{}, HoldingAgents{}, err
		}
		items := row.countAgents(waiting, agents)
		if row.AgentWaiting > 0 {
			summary.Items++
		}
		for _, id := range items {
			if !seen[id] {
				seen[id] = true
				summary.AgentItems++
			}
		}
		summary.Capped = summary.Capped || row.BlocksCapped
		rows = append(rows, row)
	}
	// STABLE, so items that block as much keep the queue's own order.
	slices.SortStableFunc(rows, func(a, b HoldingRow) int { return cmp.Compare(b.waiting(), a.waiting()) })
	if len(rows) > MyWorkRows {
		rows = rows[:MyWorkRows]
	}
	claim := ClaimTotal{Total: total + askedTotal, Capped: capped || askedCapped}
	return rows, claim, summary, nil
}

// heldRows is candidates as rows held by assignment.
func heldRows(candidates []TaskRow) []HoldingRow {
	out := make([]HoldingRow, 0, len(candidates))
	for _, task := range candidates {
		out = append(out, HoldingRow{TaskRow: task})
	}
	return out
}

// waiting is how many items wait on the person through the row: what waits on
// it, and the item itself where it is held by being asked.
func (r HoldingRow) waiting() int {
	if r.Asked != "" {
		return r.Blocks + 1
	}
	return r.Blocks
}

// countAgents sets the row's agent count from what its walk found waiting and
// answers the agent items it counted, and names the holder of an item held by
// being asked among who waits.
func (r *HoldingRow) countAgents(waiting []waiter, agents map[string]bool) []string {
	var items []string
	if r.Asked != "" {
		if agents[r.Assignee] {
			items = append(items, r.ID)
		}
		if r.Assignee != "" && !slices.Contains(r.Holders, r.Assignee) &&
			len(r.Holders) < MaxHoldingHolders {
			r.Holders = append(r.Holders, r.Assignee)
			slices.Sort(r.Holders)
		}
	}
	for _, wt := range waiting {
		if agents[wt.assignee] {
			items = append(items, wt.id)
		}
	}
	r.AgentWaiting = len(items)
	return items
}

// readHeldByAsks is the items a person holds by an open question put to them,
// from their newest [MaxHoldingCandidates] questions, and whether there were
// more.
func readHeldByAsks(ctx context.Context, tx *sql.Tx, handle string,
	dayStart time.Time) ([]HoldingRow, bool, error) {

	statement, open := heldByAsksStatement()
	args := append(append([]any{handle, handle}, open...), MaxHoldingCandidates+1)
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, false, fmt.Errorf("worklet: read what waits on %s's answers: %w", handle, err)
	}
	var ids []any
	askOf := map[string]string{}
	read := 0
	for rows.Next() {
		var task, ask string
		if err := rows.Scan(&task, &ask); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("worklet: scan what waits on %s's answers: %w", handle, err)
		}
		if read++; read > MaxHoldingCandidates {
			continue
		}
		if _, listed := askOf[task]; !listed {
			ids = append(ids, task)
			askOf[task] = ask
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, false, fmt.Errorf("worklet: read what waits on %s's answers: %w", handle, err)
	}
	more := read > MaxHoldingCandidates
	if len(ids) == 0 {
		return nil, false, nil
	}
	tasks, _, err := readTasks(ctx, tx, tasksWhere("t.id IN ("+placeholders(len(ids))+")", ids),
		[]sortTerm{{Column: "t.id"}}, len(ids), dayStart)
	if err != nil {
		return nil, false, err
	}
	byID := make(map[string]TaskRow, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	out := make([]HoldingRow, 0, len(ids))
	for _, id := range ids {
		if t, held := byID[id.(string)]; held {
			out = append(out, HoldingRow{TaskRow: t, Asked: askOf[t.ID]})
		}
	}
	return out, more, nil
}

// heldByAsksCountStatement counts the items held by an ask, each once, up to
// one past the total ceiling.
func heldByAsksCountStatement() (string, []any) {
	open := openGroups()
	return `SELECT COUNT(*) FROM (SELECT DISTINCT c.task_id FROM ` + heldByAsksFrom + `
		WHERE ` + fmt.Sprintf(heldByAsksWhere, placeholders(len(open))) + ` LIMIT ?)`, open
}

// countHeldByAsks counts the items a person holds by an open question, each
// once, capped as every total is.
func countHeldByAsks(ctx context.Context, tx *sql.Tx, handle string) (int, bool, error) {
	statement, open := heldByAsksCountStatement()
	var n int
	if err := tx.QueryRowContext(ctx, statement, append(append([]any{handle, handle}, open...),
		TotalHintCeiling+1)...).Scan(&n); err != nil {
		return 0, false, fmt.Errorf("worklet: count what waits on %s's answers: %w", handle, err)
	}
	counted, capped := capHint(n)
	return counted, capped, nil
}

// waiter is one open item waiting on another, and who holds it.
type waiter struct{ id, assignee string }

// holdingWalk walks every candidate of one read over one memo of each item's
// waiters, asking for an item's waiters at most once and at most steps times.
type holdingWalk struct {
	handle  string
	steps   int
	memo    map[string][]waiter
	waiters func(id string) ([]waiter, error)
}

func newHoldingWalk(handle string, steps int, waiters func(string) ([]waiter, error)) *holdingWalk {
	return &holdingWalk{handle: handle, steps: steps, memo: map[string][]waiter{}, waiters: waiters}
}

// walk counts what waits on task, breadth first and each item once, so a
// dependency cycle ends the walk rather than looping it, and answers each
// waiting item it counted with its holder.
func (w *holdingWalk) walk(task TaskRow) (HoldingRow, []waiter, error) {
	row := HoldingRow{TaskRow: task, Holders: []string{}}
	var found []waiter
	seen := map[string]bool{task.ID: true}
	holders := map[string]bool{}
	frontier := []string{task.ID}
	for len(frontier) > 0 && !row.BlocksCapped {
		next := frontier[0]
		frontier = frontier[1:]
		waiting, known := w.memo[next]
		if !known {
			if w.steps == 0 {
				row.BlocksCapped = true
				break
			}
			w.steps--
			var err error
			if waiting, err = w.waiters(next); err != nil {
				return HoldingRow{}, nil, err
			}
			w.memo[next] = waiting
		}
		for _, wt := range waiting {
			if seen[wt.id] {
				continue
			}
			if row.Blocks == MaxHoldingWalk {
				row.BlocksCapped = true
				break
			}
			seen[wt.id] = true
			row.Blocks++
			frontier = append(frontier, wt.id)
			found = append(found, wt)
			if wt.assignee != "" && wt.assignee != w.handle {
				holders[wt.assignee] = true
			}
		}
	}
	for _, h := range slices.Sorted(maps.Keys(holders)) {
		if len(row.Holders) == MaxHoldingHolders {
			break
		}
		row.Holders = append(row.Holders, h)
	}
	return row, found, nil
}

// readWaiters is one step of the walk: the open items waiting on id.
func readWaiters(ctx context.Context, tx *sql.Tx, statement string, open []any, id string) ([]waiter, error) {
	rows, err := tx.QueryContext(ctx, statement, append([]any{id}, open...)...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read what waits on %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []waiter
	for rows.Next() {
		var wt waiter
		if err := rows.Scan(&wt.id, &wt.assignee); err != nil {
			return nil, fmt.Errorf("worklet: scan what waits on %s: %w", id, err)
		}
		out = append(out, wt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: walk what waits on %s: %w", id, err)
	}
	return out, nil
}

// readHandedOver is when item id last changed hands; ok is false for an item
// no change ever assigned. Holding up and the advisor read the one instant.
func readHandedOver(ctx context.Context, tx *sql.Tx, id string) (at time.Time, ok bool, err error) {
	var since int64
	err = tx.QueryRowContext(ctx, handedOverStatement, id).Scan(&since)
	switch {
	case err == nil:
		return store.DecodeTime(since), true, nil
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	default:
		return time.Time{}, false, fmt.Errorf("worklet: read when %s changed hands: %w", id, err)
	}
}
