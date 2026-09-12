package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The one-sided dependency repair: the LAST step of a gesture that stopped.
//
// # What a one-sided edge is, and what it costs
//
// A dependency is two commits — the authored `waiting_on` edge on the
// dependent, and the mirrored entry in the blocker's own `Dependents`. The
// second is best effort, because the first is already durable and refusing the
// whole gesture over it would lose an edge somebody asked for.
//
// So the residue is an edge whose mirror never landed, and it costs two
// distinct things:
//
//  1. THE BLOCKER'S ASSIGNEE WAS NEVER TOLD. The mirror carries
//     [Snapshot.Dependents], which is what routes `blocking` — and the
//     dependent's own commit routes to the DEPENDENT's parties, so "the wake
//     went out with the other commit" is false.
//  2. A CLOSE CANNOT NAME WHO IT UNBLOCKS from the blocker's own row.
//
// # Why the repair is LOUD
//
// Every other repair in this file's neighbourhood re-derives something that is
// already true and tells nobody. This one is the missing STEP of a sequence,
// not a correction of a value, and the step it is missing is the one that
// announces itself. It carries `Late: true` so a person receiving it hours
// after the fact can see why.

// OneSidedRepairAge is how old an edge must be before the duty repairs it.
//
// THIRTY SECONDS, which is [ClaimStale] and the same quantity: it is how long
// a gesture that is still running may reasonably take to reach its own last
// step. Shorter and the duty races live writers, publishing a mirror the
// gesture was about to publish itself — two records on one subject where one
// would do, and a second wake for the blocker's assignee. Longer and a
// dependency written during a node's restart sits unannounced for no reason.
//
// It is measured against the AUTHORED instant carried on the EDGE rather than
// against the task's `updated_at`, because any unrelated edit resets that
// column — and a busy task would postpone its own repair indefinitely, while
// the busiest tasks are exactly the ones that acquire dependencies.
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

	// BlockerDependents is how many the blocker already has, and
	// BlockerRemoved whether it has since been tombstoned. Both are
	// reasons the repair is PERMANENT rather than pending.
	BlockerDependents int
	BlockerRemoved    bool
	BlockerMissing    bool
}

// Final reports an edge whose mirror can never be written.
//
// THREE REASONS, and they are the three the design names: the blocker is gone,
// it was tombstoned since, or its dependent list is full. Each is a fact a
// person resolves rather than something a retry could change — so the duty
// stamps the edge and never looks at it again, and the attention queue is
// where it surfaces.
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

// ScanOneSided reads the edges whose mirror has not been written.
//
// SELECTED ON THE STORED FLAG, which the applier derives on both ends — so
// this is an indexed read of exactly the broken edges rather than a join over
// every relation in the company. On a healthy fleet it reads nothing.
func ScanOneSided(ctx context.Context, db *store.DB, before time.Time,
	limit int) ([]OneSided, error) {

	if db == nil {
		return nil, fmt.Errorf("tracker: the one-sided repair has no replicated estate")
	}
	var out []OneSided
	err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT r.task_id, r.other_id,
			       COALESCE(t.key, ''), t.project_key, COALESCE(t.assignee, ''),
			       COALESCE(b.project_key, ''), COALESCE(b.key, ''),
			       COALESCE(b.assignee, ''),
			       (SELECT COUNT(*) FROM tracker_task_dependents d
			        WHERE d.task_id = r.other_id),
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
			LIMIT ?`, store.EncodeTime(before), limit)
		if err != nil {
			return fmt.Errorf("tracker: read the one-sided dependency edges: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var edge OneSided
			var removed, missing int
			if err := rows.Scan(&edge.Dependent, &edge.Blocker,
				&edge.DependentKey, &edge.DependentProject, &edge.DependentAssignee,
				&edge.BlockerProject, &edge.BlockerKey, &edge.BlockerAssignee,
				&edge.BlockerDependents, &removed, &missing); err != nil {
				return fmt.Errorf("tracker: read a one-sided edge: %w", err)
			}
			edge.BlockerRemoved, edge.BlockerMissing = removed == 1, missing == 1
			out = append(out, edge)
		}
		return rows.Err()
	})
	return out, err
}

// RepairOneSided writes the mirror commit a dependency gesture never reached,
// or stamps the edge permanently one-sided.
//
// ON THE BLOCKER'S SUBJECT, which is what makes this the missing STEP rather
// than a new gesture: the record is the one the writer would have published,
// with the same patch and the same wake, published by whoever holds the duty.
func (w *Writer) RepairOneSided(ctx context.Context, opID string, edge OneSided,
	leads Leads) (WriteResult, error) {

	if _, final := edge.Final(); final {
		// THE STAMP GOES ON THE DEPENDENT'S OWN EDGE, because that is
		// the row that carries the flag and the row a person opens to
		// see it — and it is a GESTURE, resolved inside the decide,
		// because this write lands minutes after the scan that chose
		// it and a collection composed from that scan would discard
		// every edge authored since.
		//
		// QUIET. Nothing happened that anybody acts on: the dependency
		// the author asked for is not going to be mirrored, and the
		// place that says so is the attention queue the flag feeds.
		return w.UpdateTask(ctx, opID, edge.Dependent, edge.DependentProject,
			NoIfMatch, TaskPatch{Relate: &RelationIntent{Final: []Relation{{
				Kind: RelationWaitingOn, Other: edge.Blocker,
			}}}}, ChangeRelations, nil)
	}
	return w.UpdateTask(ctx, opID, edge.Blocker, edge.BlockerProject,
		NoIfMatch, TaskPatch{Depend: &DependentIntent{Add: []string{edge.Dependent}}},
		ChangeRelations, &Notify{
			Kind: ChangeRelations,
			// LATE, and the flag is what tells a reader this wake is
			// the step rather than the change: a blocker's assignee
			// receiving it hours later should see why.
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
