package worklet

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
)

// OPEN WORK NOW: who holds the company's open items, and which open items
// hold the most others up. Read off every open edge once and walked in memory,
// so a company's whole graph costs three statements, never a walk per item.

// MaxOpenRows bounds the open items and the open edges one read takes: far past
// any company's open work, and a bound because the read holds them in memory.
const MaxOpenRows = 50_000

// MaxBlockers bounds the blockers a read answers, the most waited on first.
const MaxBlockers = 20

// OpenWork is where the company's open work sits now.
type OpenWork struct {
	// Holders is every holder of open items, the most held first.
	Holders []HeldOpen `json:"holders"`
	// Blockers are the open items open work waits on, the most waited on
	// first, at most [MaxBlockers], and every item a person holds that agent
	// work with no question of its own waits on, at most as many again.
	Blockers []WaitedOn `json:"blockers"`
}

// HeldOpen is one holder's open items.
type HeldOpen struct {
	Handle string `json:"handle"`
	Open   int    `json:"open"`
	// Pressing counts the urgent and high priority ones.
	Pressing int `json:"pressing"`
	// Keys are at most five of them, the highest priority first.
	Keys []string `json:"keys"`
}

// WaitedOn is an open item other open items wait on, directly or through
// another waiting item.
type WaitedOn struct {
	Key    string `json:"key"`
	Holder string `json:"holder"`
	Status string `json:"status"`
	// Blocks counts the open items waiting on it, each once; Capped says
	// the count stopped at [MaxHoldingWalk].
	Blocks int  `json:"blocks"`
	Capped bool `json:"capped,omitempty"`
	// Waiting are at most five of them, the nearest first.
	Waiting []string `json:"waiting"`
	// Ready counts the waiting items agent seats hold with no open question
	// of their own: work nothing but this item holds, and ReadyKeys names
	// at most five.
	Ready     int      `json:"ready"`
	ReadyKeys []string `json:"ready_keys"`
	// Asked are the people an open question on it is put to, sorted, and
	// Asks at most five of those questions, by comment id.
	Asked []string `json:"asked"`
	Asks  []string `json:"asks"`
}

// HeldByPerson reports an item a person holds: assigned to one, or waiting on
// a question put to one.
func (w WaitedOn) HeldByPerson(person func(string) bool) bool {
	return person(w.Holder) || len(w.Asked) > 0
}

// openHoldersStatement is every open item that has a holder, the most
// pressing of each holder's first.
func openHoldersStatement() (string, []any) {
	open := openGroups()
	return `SELECT t.assignee, t.key, t.priority FROM tracker_tasks t
		WHERE t.removed_at IS NULL AND t.archived = 0 AND t.assignee <> ''
		  AND t.status_group IN (` + placeholders(len(open)) + `)
		ORDER BY t.assignee, t.prio_rank DESC, t.id LIMIT ?`, open
}

// openEdgesStatement is every wait of an open item on another open item,
// with both ends' key and holder. A wait on a pull request has no task row.
func openEdgesStatement() (string, []any) {
	open := openGroups()
	return `SELECT d.blocker_id, b.key, b.assignee, b.status, d.task_id, u.key, u.assignee
		FROM tracker_task_deps d
		JOIN tracker_tasks u ON u.id = d.task_id
		JOIN tracker_tasks b ON b.id = d.blocker_id
		WHERE d.blocker_open = 1 AND u.removed_at IS NULL AND b.removed_at IS NULL
		  AND u.status_group IN (` + placeholders(len(open)) + `)
		  AND b.status_group IN (` + placeholders(len(open)) + `)
		ORDER BY d.blocker_id, d.task_id LIMIT ?`, append(slices.Clone(open), open...)
}

// openQuestionsStatement is every open question on an open item: the open
// asks' predicate, which its partial index holds.
func openQuestionsStatement() (string, []any) {
	open := openGroups()
	return `SELECT c.task_id, c.id, c.ask FROM tracker_comments c
		JOIN tracker_tasks t ON t.id = c.task_id
		WHERE c.ask <> '' AND c.resolved = 0 AND c.answered_by IS NULL AND c.removed = 0
		  AND c.overtaken = '' AND t.removed_at IS NULL
		  AND t.status_group IN (` + placeholders(len(open)) + `)
		ORDER BY c.task_id, c.created_at, c.id LIMIT ?`, open
}

// openNode is one open item in the graph.
type openNode struct {
	key, holder, status string
	// waiters are the items waiting on it directly, in the read's order.
	waiters []string
	// asked is whom its open questions are put to, and asks their ids.
	asked, asks []string
}

// readOpenWork reads where open work sits now. agents and people are the
// company's agent seats and persons, by handle.
func readOpenWork(ctx context.Context, tx *sql.Tx, agents, people []string, out *OpenWork) error {
	holders, err := readOpenHolders(ctx, tx)
	if err != nil {
		return err
	}
	out.Holders = holders
	nodes, err := readOpenGraph(ctx, tx)
	if err != nil {
		return err
	}
	out.Blockers = blockersOf(nodes, agents, people)
	return nil
}

func readOpenHolders(ctx context.Context, tx *sql.Tx) ([]HeldOpen, error) {
	statement, open := openHoldersStatement()
	rows, err := tx.QueryContext(ctx, statement, append(open, MaxOpenRows+1)...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read who holds the open work: %w", err)
	}
	byHandle := map[string]*HeldOpen{}
	read := 0
	for rows.Next() {
		var handle, key, priority string
		if err := rows.Scan(&handle, &key, &priority); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("worklet: scan an open item: %w", err)
		}
		if read++; read > MaxOpenRows {
			_ = rows.Close()
			return nil, fmt.Errorf("worklet: the company holds more than %d open items", MaxOpenRows)
		}
		h := byHandle[handle]
		if h == nil {
			h = &HeldOpen{Handle: handle, Keys: []string{}}
			byHandle[handle] = h
		}
		h.Open++
		if p := Priority(priority); p == PriorityUrgent || p == PriorityHigh {
			h.Pressing++
		}
		if len(h.Keys) < MaxPatternExamples {
			h.Keys = append(h.Keys, key)
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("worklet: read who holds the open work: %w", err)
	}
	out := make([]HeldOpen, 0, len(byHandle))
	for _, h := range byHandle {
		out = append(out, *h)
	}
	slices.SortFunc(out, func(a, b HeldOpen) int { return cmp.Or(cmp.Compare(b.Open, a.Open), cmp.Compare(a.Handle, b.Handle)) })
	return out, nil
}

// readOpenGraph is every open item that waits or is waited on, with its
// waiters and its open questions.
func readOpenGraph(ctx context.Context, tx *sql.Tx) (map[string]*openNode, error) {
	nodes := map[string]*openNode{}
	node := func(id, key, holder, status string) *openNode {
		n := nodes[id]
		if n == nil {
			n = &openNode{key: key, holder: holder}
			nodes[id] = n
		}
		n.status = cmp.Or(n.status, status)
		return n
	}
	statement, args := openEdgesStatement()
	rows, err := tx.QueryContext(ctx, statement, append(args, MaxOpenRows+1)...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the open waits: %w", err)
	}
	read := 0
	for rows.Next() {
		var blocker, bKey, bHolder, bStatus, task, tKey, tHolder string
		if err = rows.Scan(&blocker, &bKey, &bHolder, &bStatus, &task, &tKey, &tHolder); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("worklet: scan an open wait: %w", err)
		}
		if read++; read > MaxOpenRows {
			_ = rows.Close()
			return nil, fmt.Errorf("worklet: the company's open work holds more than %d waits", MaxOpenRows)
		}
		b := node(blocker, bKey, bHolder, bStatus)
		b.waiters = append(b.waiters, task)
		node(task, tKey, tHolder, "")
	}
	if err = closeRows(rows); err != nil {
		return nil, fmt.Errorf("worklet: read the open waits: %w", err)
	}
	statement, args = openQuestionsStatement()
	rows, err = tx.QueryContext(ctx, statement, append(args, MaxOpenRows+1)...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the open questions: %w", err)
	}
	read = 0
	for rows.Next() {
		var task, id, asked string
		if err := rows.Scan(&task, &id, &asked); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("worklet: scan an open question: %w", err)
		}
		if read++; read > MaxOpenRows {
			_ = rows.Close()
			return nil, fmt.Errorf("worklet: the company's open work holds more than %d open questions", MaxOpenRows)
		}
		// ONLY ITEMS IN THE GRAPH: a question on an item nothing waits on, and
		// that waits on nothing, holds nothing up.
		if n := nodes[task]; n != nil {
			n.asked = append(n.asked, asked)
			n.asks = append(n.asks, id)
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("worklet: read the open questions: %w", err)
	}
	return nodes, nil
}

// blockersOf walks what waits on every item something waits on, breadth first
// and each item once, and answers the most waited on, and every item a
// person holds that ready agent work waits on.
func blockersOf(nodes map[string]*openNode, agents, people []string) []WaitedOn {
	agent := func(h string) bool { return slices.Contains(agents, h) }
	person := func(h string) bool { return slices.Contains(people, h) }
	var all []WaitedOn
	for _, id := range slices.Sorted(maps.Keys(nodes)) {
		n := nodes[id]
		if len(n.waiters) == 0 {
			continue
		}
		w := WaitedOn{Key: n.key, Holder: n.holder, Status: n.status, Waiting: []string{},
			ReadyKeys: []string{}, Asks: []string{}}
		asked := map[string]bool{}
		for i, to := range n.asked {
			if !person(to) {
				continue
			}
			asked[to] = true
			if len(w.Asks) < MaxPatternExamples {
				w.Asks = append(w.Asks, n.asks[i])
			}
		}
		w.Asked = slices.Sorted(maps.Keys(asked))
		seen := map[string]bool{id: true}
		frontier := []string{id}
		for len(frontier) > 0 && !w.Capped {
			next := nodes[frontier[0]]
			frontier = frontier[1:]
			for _, wid := range next.waiters {
				if seen[wid] {
					continue
				}
				if w.Blocks == MaxHoldingWalk {
					w.Capped = true
					break
				}
				seen[wid] = true
				w.Blocks++
				frontier = append(frontier, wid)
				waiting := nodes[wid]
				if len(w.Waiting) < MaxPatternExamples {
					w.Waiting = append(w.Waiting, waiting.key)
				}
				if agent(waiting.holder) && !slices.ContainsFunc(waiting.asked, person) {
					w.Ready++
					if len(w.ReadyKeys) < MaxPatternExamples {
						w.ReadyKeys = append(w.ReadyKeys, waiting.key)
					}
				}
			}
		}
		all = append(all, w)
	}
	slices.SortStableFunc(all, func(a, b WaitedOn) int { return cmp.Compare(b.Blocks, a.Blocks) })
	out := []WaitedOn{}
	held := 0
	for i, w := range all {
		switch {
		case i < MaxBlockers:
			out = append(out, w)
		case w.HeldByPerson(person) && w.Ready > 0 && held < MaxBlockers:
			out = append(out, w)
			held++
		}
	}
	return out
}
