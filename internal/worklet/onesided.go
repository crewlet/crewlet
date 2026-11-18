package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The one-sided dependency repair: the LAST step of a gesture that stopped.
//
// A dependency is two commits: the authored `waiting_on` edge on the
// dependent, then a best-effort mirror in the blocker's `Dependents` (the
// first is already durable, and refusing the gesture would lose the edge). An
// edge whose mirror never landed leaves the blocker's assignee untold (the
// mirror's [Snapshot.Dependents] routes `blocking`; the dependent's commit
// routes only to its own parties) and a close unable to name whom it unblocks.
// So unlike a re-derivation the repair is LOUD: it is the missing step, and it
// carries `Late: true` so a wake hours later says why.

// OneSidedRepairAge is how old an edge must be before the duty repairs it:
// how long a running gesture may take to reach its last step. An age, because
// a dependency gesture takes no claim (a move or merge waits on [ClaimTTL]).
// Shorter races live writers into a second mirror record and a second wake;
// longer leaves a dependency written during a restart unannounced. Measured
// from the edge's AUTHORED instant, not the task's `updated_at`, which any edit
// resets, postponing the busiest tasks' repairs indefinitely.
const OneSidedRepairAge = 30 * time.Second

// OneSided is one edge whose mirror was never written.
type OneSided struct {
	// Dependent is the task that authored the `waiting_on` edge, and
	// Blocker the task that does not list it.
	Dependent, Blocker string

	// DependentKey, DependentAssignee and BlockerProject are what the
	// repair's own commit needs: the mirror is published on the BLOCKER's
	// subject and its wake names the dependent.
	DependentKey      string
	DependentProject  string
	DependentAssignee string
	BlockerProject    string
	BlockerKey        string
	BlockerAssignee   string

	// BlockerDependents, BlockerRemoved and BlockerMissing are the reasons
	// a repair is PERMANENT rather than pending ([OneSided.Final]).
	BlockerDependents int
	BlockerRemoved    bool
	BlockerMissing    bool
}

// Final reports an edge whose mirror can never be written.
//
// The blocker is gone, tombstoned, or its dependent list is full: no retry
// changes any of them, so the duty stamps the edge once and the attention
// queue surfaces it for a person.
func (o OneSided) Final() (string, bool) {
	switch {
	case o.BlockerMissing:
		return "the blocker no longer exists", true
	case o.BlockerRemoved:
		return "the blocker was removed", true
	case o.BlockerDependents >= MaxDependents:
		return fmt.Sprintf("the blocker already has %d dependents, which is "+
			"the maximum", o.BlockerDependents), true
	}
	return "", false
}

// pendingOneSidedStatement is [duty.pendingOneSided]'s probe — whether any
// edge waits for the repair at all — named for the plan test.
const pendingOneSidedStatement = `
			SELECT EXISTS (SELECT 1 FROM tracker_relations
			               WHERE one_sided = 1 AND one_sided_final = 0)`

// oneSidedStatement is [ScanOneSided]'s read, named for the plan test.
//
// IT DOES NOT COUNT THE BLOCKER'S DEPENDENTS ([blockerDependentsStatement] does,
// per batch): a correlated `COUNT(*)` here is a seek per edge only until the
// planner unnests it into a grouped read of every mirror row, a choice no line
// of this statement decides (storetest.Decorrelated).
const oneSidedStatement = `
		SELECT r.task_id, r.other_id,
		       COALESCE(t.key, ''), t.project_key, COALESCE(t.assignee, ''),
		       COALESCE(b.project_key, ''), COALESCE(b.key, ''),
		       COALESCE(b.assignee, ''),
		       CASE WHEN b.removed_at IS NULL THEN 0 ELSE 1 END,
		       CASE WHEN b.id IS NULL THEN 1 ELSE 0 END
		FROM tracker_relations r
		JOIN tracker_tasks t ON t.id = r.task_id
		LEFT JOIN tracker_tasks b ON b.id = r.other_id
		WHERE r.one_sided = 1 AND r.one_sided_final = 0
		  AND r.kind = 'waiting_on'
		  -- THE DEPENDENT'S OWN TOMBSTONE ENDS THE MATTER. A
		  -- removed task's edges are frozen, and announcing a
		  -- dependency on work nobody is doing wakes somebody
		  -- about nothing.
		  AND t.removed_at IS NULL
		  -- THE EDGE'S OWN AGE, never the task's updated_at: any
		  -- unrelated edit to the dependent resets that column, and a
		  -- busy task would postpone its own repair indefinitely --
		  -- while the busiest tasks are exactly the ones that acquire
		  -- dependencies.
		  AND r.created_at <= ?
		ORDER BY r.task_id, r.other_id
		LIMIT ?`

// blockerDependentsStatement counts how many dependents each of n blockers
// already lists, by the mirror primary key's leading column. A blocker listing
// none has no row, and counts zero.
func blockerDependentsStatement(n int) string {
	return `
		SELECT task_id, COUNT(*) FROM tracker_task_dependents
		WHERE task_id IN (` + placeholders(n) + `)
		GROUP BY task_id`
}

// ScanOneSided reads the edges whose mirror has not been written.
//
// Selected on the stored flag the applier derives on both ends: an indexed
// read of exactly the broken edges, nothing on a healthy fleet.
func ScanOneSided(ctx context.Context, db store.ReplicatedReader, before time.Time,
	limit int) ([]OneSided, error) {

	if db.IsZero() {
		return nil, fmt.Errorf("worklet: the one-sided repair has no replicated estate")
	}
	var out []OneSided
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, oneSidedStatement, store.EncodeTime(before), limit)
		if err != nil {
			return fmt.Errorf("worklet: read the one-sided dependency edges: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var edge OneSided
			var removed, missing int
			if err := rows.Scan(&edge.Dependent, &edge.Blocker,
				&edge.DependentKey, &edge.DependentProject, &edge.DependentAssignee,
				&edge.BlockerProject, &edge.BlockerKey, &edge.BlockerAssignee,
				&removed, &missing); err != nil {
				return fmt.Errorf("worklet: read a one-sided edge: %w", err)
			}
			edge.BlockerRemoved, edge.BlockerMissing = removed == 1, missing == 1
			out = append(out, edge)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("worklet: read the one-sided dependency edges: %w", err)
		}
		_ = rows.Close()
		return countBlockerDependents(ctx, tx, out)
	})
	return out, err
}

// countBlockerDependents fills each edge's [OneSided.BlockerDependents] from
// one grouped read of the batch's distinct blockers, IN THE SCAN'S OWN
// TRANSACTION, so it counts the edges' snapshot. The scan's limit bounds the
// key list.
func countBlockerDependents(ctx context.Context, tx *sql.Tx, edges []OneSided) error {
	var blockers []any
	seen := map[string]bool{}
	for _, edge := range edges {
		if !seen[edge.Blocker] {
			seen[edge.Blocker] = true
			blockers = append(blockers, edge.Blocker)
		}
	}
	if len(blockers) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, blockerDependentsStatement(len(blockers)), blockers...)
	if err != nil {
		return fmt.Errorf("worklet: count the one-sided edges' blockers' dependents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[string]int, len(blockers))
	for rows.Next() {
		var blocker string
		var n int
		if err := rows.Scan(&blocker, &n); err != nil {
			return fmt.Errorf("worklet: read a blocker's dependent count: %w", err)
		}
		counts[blocker] = n
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("worklet: count the one-sided edges' blockers' dependents: %w", err)
	}
	for i := range edges {
		edges[i].BlockerDependents = counts[edges[i].Blocker]
	}
	return nil
}

// RepairOneSided writes the mirror commit a dependency gesture never reached,
// or stamps the edge permanently one-sided.
//
// On the BLOCKER'S subject, with the patch and wake the writer would have
// published: the missing STEP, not a new gesture.
func (w *Writer) RepairOneSided(ctx context.Context, opID string, edge OneSided,
	leads Leads) (WriteResult, error) {

	if _, final := edge.Final(); final {
		// Stamped on the dependent's own edge, the row carrying the
		// flag, as a GESTURE resolved inside the decide: the scan is
		// minutes old, and a collection composed from it would discard
		// edges authored since. QUIET: the attention queue says so.
		return w.UpdateTask(ctx, opID, edge.Dependent, edge.DependentProject,
			NoIfMatch, TaskPatch{Relate: &RelationIntent{Final: []Relation{{
				Kind: RelationWaitingOn, Other: edge.Blocker,
			}}}}, ChangeRelations, nil)
	}
	return w.UpdateTask(ctx, opID, edge.Blocker, edge.BlockerProject,
		NoIfMatch, TaskPatch{Depend: &DependentIntent{Add: []string{edge.Dependent}}},
		ChangeRelations, &Notify{
			Kind: ChangeRelations,
			// Late tells an assignee woken hours later that this is
			// the missed step, not a new change.
			Late: true,
			Snapshot: Snapshot{
				Key: edge.BlockerKey, Project: edge.BlockerProject,
				Assignee:    edge.BlockerAssignee,
				ProjectLead: projectLead(leads, edge.BlockerProject),
				Dependents: []TaskParty{{
					Task: edge.Dependent, Key: edge.DependentKey,
					Assignee: edge.DependentAssignee,
				}},
			},
		})
}
