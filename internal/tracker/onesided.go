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
// Shorter and the duty races live writers, publishing a mirror the gesture was
// about to publish itself — two records on one subject where one would do, and
// a second wake for the blocker's assignee. Longer and a dependency written
// during a node's restart sits unannounced for no reason.
//
// It is measured against the AUTHORED instant carried on the EDGE rather than
// against the task's `updated_at`, because any unrelated edit resets that
// column — and a busy task would postpone its own repair indefinitely, while
// the busiest tasks are exactly the ones that acquire dependencies.
const OneSidedRepairAge = ClaimStale

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

// OneSidedScan is one tick's worth of mirror repair, and whether the window
// held more edges than the tick carried.
//
// THE TWIN OF [UnblockScan], and deliberately the same shape: both are duty
// repairs bounded by one [WalkBatch] per sweep, leaving the rest for the next
// one, so both have to say when they left something — a company 10,000 broken
// edges deep and a healthy one that repaired 64 publish the same count
// otherwise, and the count is all an operator sees.
type OneSidedScan struct {
	// Edges holds at most the `limit` the caller passed, and the bound is
	// on EDGES rather than on a screenful. It is NOT a count of records:
	// [PlanOneSided] resolves these into one commit per subject, and a
	// blocker several broken edges name contributes one commit carrying
	// all of them.
	//
	// NOTHING IS LOST TO IT. A repaired edge leaves the predicate — the
	// mirror clears `one_sided`, a permanent one sets `one_sided_final` —
	// so what this scan did not carry is found by the next sweep from the
	// same indexed read. There is no position to poison and nothing to
	// carry forward: the STORED FLAG is the whole of the state.
	Edges []OneSided

	// Truncated says the window holds more broken edges than this scan
	// carried — ASKED, NOT INFERRED: the query reads ONE ROW PAST the
	// bound and that row is dropped rather than carried, so its presence
	// is the evidence. `len(Edges) == limit` is a different fact: a window
	// holding exactly the limit holds everything it has.
	//
	// It is the mark on the cut, and [duty.repairOneSided] is its reader —
	// without it the repair's own log line cannot tell a tick that fixed
	// everything from a tick that fixed the first 64 of a backlog.
	Truncated bool
}

// ScanOneSided reads the edges whose mirror has not been written.
//
// SELECTED ON THE STORED FLAG, which the applier derives on both ends — so
// this is an indexed read of exactly the broken edges rather than a join over
// every relation in the company. On a healthy fleet it reads nothing.
func ScanOneSided(ctx context.Context, db *store.DB, before time.Time,
	limit int) (OneSidedScan, error) {

	if db == nil {
		return OneSidedScan{}, fmt.Errorf("tracker: the one-sided repair has no replicated estate")
	}
	if limit < 1 {
		// REFUSED, naming the parameter, for [ScanUnblocked]'s reason:
		// the query keeps at most `limit` rows and reads one past it as
		// evidence, so below one it carries no edge and reports itself
		// truncated on every tick — a repair that mirrors nothing while
		// returning success. There is no fallback here that would not be
		// this file guessing at its caller's pacing.
		return OneSidedScan{}, fmt.Errorf("tracker: the one-sided repair was "+
			"given limit %d, and a scan that may carry no edge writes no "+
			"mirror and clears no flag: pass a positive limit (the duty "+
			"passes WalkBatch, %d)", limit, WalkBatch)
	}
	var out OneSidedScan
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
			-- ONE ROW PAST THE BOUND: the extra row is evidence
			-- that more edges are broken than this tick repairs,
			-- never an answer. See [OneSidedScan.Truncated].
			ORDER BY r.task_id, r.other_id
			LIMIT ?`, store.EncodeTime(before), limit+1)
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
			out.Edges = append(out.Edges, edge)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// THE PROBE ROW IS EVIDENCE, never an answer: it is dropped here
		// rather than repaired, so the bound on records this tick
		// publishes is exactly `limit`. What it leaves is found again by
		// the next sweep's own read, because the flag it selects on is
		// still set on every edge this tick did not reach.
		out.Truncated = len(out.Edges) > limit
		if out.Truncated {
			out.Edges = out.Edges[:limit]
		}
		return nil
	})
	if err != nil {
		return OneSidedScan{}, err
	}
	return out, nil
}

// OneSidedCommit is every edge of one scan that lands on ONE task's subject.
//
// # Why a tick is grouped by subject at all
//
// The subject is the arbitration unit, and a second record on it cannot be
// DECIDED until this node has applied the first: the write path waits for the
// subject's own anchor and refuses the write as `behind` when that wait
// expires. So a repair publishing one record per EDGE lands the first edge
// behind a blocker and is refused on every edge after it — and edges sharing
// a blocker are the ordinary shape here, because the dependent's own commit
// lands first and it is the blocker's mirror that loses the race.
//
// Grouped, the tick publishes what the ordinary gesture publishes: one commit
// per subject carrying every dependent that subject gains, which is the shape
// [Writer.mirror] already uses for the live write.
type OneSidedCommit struct {
	// Task is the subject this commit is published on and Project its
	// container. Both halves below name the SAME task — the blocker of
	// every edge in Mirror, and the dependent that authored every edge
	// in Final — which is what lets one record carry both.
	Task, Project string

	// Mirror is the edges whose dependents Task gains, so it is Task's
	// own [Task.Dependents] that this commit adds to.
	Mirror []OneSided

	// Final is the edges stamped permanently one-sided. THE STAMP GOES ON
	// THE DEPENDENT'S OWN EDGE, because that is the row carrying the flag
	// and the row a person opens to see it.
	Final []OneSided
}

// PlanOneSided resolves a scan's edges into the commits one tick publishes.
//
// PURE OVER VALUES, for `coerce.go`'s reason: a rule that can only be
// exercised through a database and a broker is a rule nobody re-reads. What it
// decides is exactly two things — which subject each edge lands on, and how
// many mirrors a blocker still has room for — and both are arithmetic over the
// rows [ScanOneSided] already read.
//
// # THE ROOM IS RESPECTED RATHER THAN DISCOVERED BY REFUSAL
//
// [OneSided.Final] is a property of the BLOCKER, so every edge behind one
// blocker agrees about it — but it answers "is the blocker full ALREADY", and
// a blocker with room for two that five edges name is not. Adding all five in
// one commit fails `checkDependents` inside the decide, which would refuse the
// whole commit including the two that fit, on this sweep and on every sweep
// after it.
//
// So a blocker takes at most [MaxDependents] minus what it already holds. The
// edges past that are left where they are: the next sweep reads the blocker at
// its cap, [OneSided.Final] is true for them, and they are stamped rather than
// retried for ever. The caller's own count is what says they were left — see
// the `deferred` attribute on the duty's log line.
func PlanOneSided(edges []OneSided) []OneSidedCommit {
	byTask := map[string]*OneSidedCommit{}
	// ORDER OF FIRST APPEARANCE, which is deterministic because the scan
	// that feeds this reads under an ORDER BY. A map range would publish
	// one tick's commits in a different order on every run, and the opIDs
	// they dedupe on are derived from the subject rather than the index —
	// but a plan that cannot be compared is a plan no test can pin.
	var order []string
	// room is what each blocker has left, carried ACROSS the loop so five
	// edges behind one blocker with room for two admit exactly two.
	room := map[string]int{}
	commit := func(task, project string) *OneSidedCommit {
		existing, ok := byTask[task]
		if !ok {
			existing = &OneSidedCommit{Task: task, Project: project}
			byTask[task] = existing
			order = append(order, task)
		}
		return existing
	}
	for _, edge := range edges {
		if _, final := edge.Final(); final {
			into := commit(edge.Dependent, edge.DependentProject)
			into.Final = append(into.Final, edge)
			continue
		}
		left, seen := room[edge.Blocker]
		if !seen {
			left = MaxDependents - edge.BlockerDependents
		}
		if left <= 0 {
			continue
		}
		room[edge.Blocker] = left - 1
		into := commit(edge.Blocker, edge.BlockerProject)
		into.Mirror = append(into.Mirror, edge)
	}
	out := make([]OneSidedCommit, 0, len(order))
	for _, task := range order {
		out = append(out, *byTask[task])
	}
	return out
}

// RepairOneSided writes the mirror commits a dependency gesture never reached,
// and stamps the edges that can never be mirrored — ONE RECORD, on one task.
//
// ON THE BLOCKER'S SUBJECT for the mirror half, which is what makes this the
// missing STEP rather than a new gesture: the record is the one the writer
// would have published, with the same patch and the same wake, published by
// whoever holds the duty.
//
// BOTH HALVES TRAVEL TOGETHER when a task is the blocker of one broken edge
// and the dependent of another, because they are one subject and two records
// on it is the contention [PlanOneSided] exists to remove. A patch carries
// both gestures and each is resolved inside the decide — `settleRelations` for
// the stamp, `settleDependents` for the mirror — so neither is composed from
// the scan's own reading of a collection.
func (w *Writer) RepairOneSided(ctx context.Context, opID string,
	commit OneSidedCommit, leads Leads) (WriteResult, error) {

	if len(commit.Mirror) == 0 && len(commit.Final) == 0 {
		// REFUSED NAMING THE SUBJECT rather than published. An empty
		// patch is a durable record every node applies that changes
		// nothing, and the duty counts what it publishes as repaired —
		// so a caller handing this an empty commit would be told it
		// fixed edges nobody named.
		return WriteResult{}, fmt.Errorf("tracker: the one-sided repair on "+
			"task %q carries no edge: build commits with PlanOneSided rather "+
			"than by hand", commit.Task)
	}
	var patch TaskPatch
	if len(commit.Final) > 0 {
		// A GESTURE, resolved inside the decide, because this write
		// lands minutes after the scan that chose it and a collection
		// composed from that scan would discard every edge authored
		// since.
		stamp := make([]Relation, 0, len(commit.Final))
		for _, edge := range commit.Final {
			stamp = append(stamp, Relation{
				Kind: RelationWaitingOn, Other: edge.Blocker,
			})
		}
		patch.Relate = &RelationIntent{Final: stamp}
	}
	// QUIET WHEN THERE IS ONLY A STAMP. Nothing happened that anybody
	// acts on: the dependency the author asked for is not going to be
	// mirrored, and the place that says so is the attention queue the
	// flag feeds.
	var notify *Notify
	if len(commit.Mirror) > 0 {
		add := make([]string, 0, len(commit.Mirror))
		parties := make([]TaskParty, 0, len(commit.Mirror))
		for _, edge := range commit.Mirror {
			add = append(add, edge.Dependent)
			parties = append(parties, TaskParty{
				Task: edge.Dependent, Key: edge.DependentKey,
				Assignee: edge.DependentAssignee,
			})
		}
		patch.Depend = &DependentIntent{Add: add}
		// THE BLOCKER'S OWN KEY AND ASSIGNEE, off any of these edges:
		// every edge in Mirror names commit.Task as its blocker, so the
		// three columns the scan read for it are the same on all of
		// them.
		blocker := commit.Mirror[0]
		notify = &Notify{
			Kind: ChangeRelations,
			// LATE, and the flag is what tells a reader this wake is
			// the step rather than the change: a blocker's assignee
			// receiving it hours later should see why.
			Late: true,
			Snapshot: Snapshot{
				Key: blocker.BlockerKey, Project: commit.Project,
				Assignee:    blocker.BlockerAssignee,
				ProjectLead: projectLead(leads, commit.Project),
				Dependents:  parties,
			},
		}
	}
	return w.UpdateTask(ctx, opID, commit.Task, commit.Project, NoIfMatch,
		patch, ChangeRelations, notify)
}
