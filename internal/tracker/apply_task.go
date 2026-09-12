package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The task apply, which is where almost every record lands.
//
// # Why a patch is MERGED against the stored document rather than written over
// it
//
// The record carries what changed and the row carries what it was. A patch
// applied to the decoded document and re-encoded produces the complete new
// state — including fields THIS BUILD DOES NOT KNOW, which round-trip through
// the document untouched. Writing the patch's columns straight onto the row
// would lose them at the first rolling upgrade, permanently, on the node that
// happened to apply the record.

// applyTask writes one task record and everything derived from it.
func (a *Applier) applyTask(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	id := c.subject().ID
	if OpKind(c.record.Op) == OpPurge {
		return a.purgeTask(ctx, tx, c)
	}

	current, held, err := readTask(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if held && c.packed <= int64(max(current.Version, current.ScopedThrough)) {
		// A REDELIVERY OR A REPROCESS, and both are ordinary traffic. The
		// history row below is still written, because a record that
		// produced no object change is still something that happened —
		// and its own guard makes THAT idempotent.
		// NO DELTAS: this record changed no document, so there is
		// nothing for the history row to say moved.
		return a.writeHistory(ctx, tx, c, current.Project, nil)
	}

	next, err := mergeTask(current, held, c)
	if err != nil {
		return 0, err
	}
	next.Version = uint64(c.packed)

	// THE FINISH STAMPS ARE THE APPLIER'S, derived from the group the
	// task has just entered rather than carried by the record.
	//
	// A writer-supplied instant would be one node's clock on a column
	// every "recent" filter, every cycle- and lead-time report and every
	// dependency edge's own clearing time is compared against — and the
	// dependency edge is the one that matters most, because a blocker
	// whose finish instant is null clears nothing and every dependent
	// waits for ever on work that is done.
	if err := stampFinish(&next, c); err != nil {
		return 0, err
	}

	// AND THE SPRINT HISTORY IS THE APPLIER'S TOO, for the same reason:
	// a stay is a pair of instants, and instants this engine derives come
	// from the broker rather than from whoever wrote the record.
	if err := stampSprintStay(&next, current, held, c); err != nil {
		return 0, err
	}

	document, err := json.Marshal(next)
	if err != nil {
		return 0, fmt.Errorf("tracker: encode task %s at %s: %w", id, c.position, err)
	}
	rows, err := upsertTask(ctx, tx, next, document, c)
	if err != nil {
		return 0, err
	}
	children, err := a.explodeTask(ctx, tx, next, c)
	if err != nil {
		return 0, err
	}
	thread, err := a.writeThread(ctx, tx, next, c)
	if err != nil {
		return 0, err
	}
	counts, err := a.maintainProjectCounts(ctx, tx, current, next, held)
	if err != nil {
		return 0, err
	}
	// WHAT THE APPLY ACTUALLY DID, computed from the two documents this
	// frame holds — which is the only frame that can, and the one the
	// history row and the spans below are both derived from.
	before := current
	if !held {
		before = Task{}
	}
	applied := TaskDeltas(before, next)
	history, err := a.writeHistory(ctx, tx, c, next.Project, applied)
	if err != nil {
		return 0, err
	}
	spans := 0
	// THE STATUS MOVED, not "somebody was told the status moved". Gated on
	// the notification, a quiet status change produced no span at all —
	// and every report derived from the spans omitted it silently.
	if _, moved := applied["status"]; moved {
		if spans, err = a.recomputeSpans(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	told, err := a.stampUnblocked(ctx, tx, c)
	if err != nil {
		return 0, err
	}
	return rows + children + thread + counts + history + spans + told, nil
}

// stampUnblocked records that this commit told a dependent it is workable.
//
// # Why the applier writes it and not the writer
//
// The column is a MAX over every applied commit that named this task as
// unblocked, which makes it order-independent and identical on every node — a
// redelivery, a reprocess and a replay all leave the same value, so none of
// them re-issues a wake somebody already had. A writer-supplied instant could
// not have that property: it would be one node's clock, and two nodes telling
// the same dependent would leave two different answers to "have they been
// told".
//
// It is the EFFECTIVE instant, never the authored one, for the reason the
// dependency edge's own clearing time is: the comparison the repair makes is
// between two fleet-agreed values, and on authored clocks a sixty-second skew
// re-issues a late wake for every task cleared inside that window, on every
// tick.
func (a *Applier) stampUnblocked(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	if c.record.Notify == nil || len(c.record.Notify.Snapshot.Unblocked) == 0 {
		return 0, nil
	}
	written := 0
	for _, party := range c.record.Notify.Snapshot.Unblocked {
		if party.Task == "" {
			continue
		}
		// THE DEPENDENT'S OWN EFFECTIVE INSTANT, computed exactly as
		// every other instant on its row is: a MAX over the records
		// this node has applied at or below this position, so two nodes
		// applying in different orders reach the same number.
		effective, err := effectiveAt(ctx, tx, party.Task, c)
		if err != nil {
			return 0, err
		}
		at := store.EncodeTime(effective)
		res, err := tx.ExecContext(ctx, `
			UPDATE tracker_tasks
			SET unblocked_told_at = MAX(COALESCE(unblocked_told_at, 0), ?)
			WHERE id = ? AND COALESCE(unblocked_told_at, 0) < ?`,
			at, party.Task, at)
		if err != nil {
			return 0, fmt.Errorf("tracker: record that %s was told it is "+
				"unblocked at %s: %w", party.Task, c.position, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// stampFinish sets or clears the two finish instants from the group the task
// is now in.
//
// BY GROUP RATHER THAN BY SLUG, because a company renames its statuses and
// adds its own: `cancelled` and `done` are both the done group, and a report
// that keyed on the slug would silently stop counting the day somebody added
// `shipped`. FIRST ENTRY WINS in each group, so a task that goes done, is
// reopened and goes done again keeps the instant of the state it is in and not
// of the first time it ever reached it — which is why a reopen CLEARS both.
func stampFinish(task *Task, c applyContext) error {
	at, err := effectiveOf(c)
	if err != nil {
		return err
	}
	switch statusOf(*task).Group() {
	case GroupDone:
		if task.DoneAt == nil {
			task.DoneAt = &at
		}
		task.ClosedAt = nil
	case GroupClosed:
		if task.DoneAt == nil {
			task.DoneAt = &at
		}
		if task.ClosedAt == nil {
			task.ClosedAt = &at
		}
	default:
		// A REOPEN CLEARS BOTH, so "finished in the last week" agrees
		// with the task's actual state rather than with a state it left.
		task.DoneAt, task.ClosedAt = nil, nil
	}
	return nil
}

// stampSprintStay records a task entering or leaving a sprint.
//
// # Why the applier derives it and no writer carries it
//
// `tracker_task_sprints` is ONE ROW PER STAY, and a stay is what every sprint
// report is derived from: committed, added, removed, remaining and velocity
// are all read off the from/to pair rather than off the task's current sprint
// number, because a task that carried over is in TWO sprints and a column
// could only say one. The pair is two INSTANTS, and an instant this engine
// derives comes from the broker (see [effectiveOf]) — a writer-supplied one
// would be one node's clock deciding which sprint a piece of work landed in.
//
// It is also why nothing published a stay before: a writer cannot form one. It
// would have to read the task's current sprint in one transaction and write
// the boundary in another, and two writers moving one task between sprints
// would then each close a stay the other had already closed.
//
// # The shape
//
// A stay OPENS when the task's sprint becomes non-nil and CLOSES when it stops
// being that number — a move from 4 to 5 does both, in that order, so the
// history reads as a carry-over rather than as two unrelated memberships. A
// task that was never in a sprint and still is not produces nothing at all,
// which is the common case and costs one comparison.
//
// THE CAP DROPS THE OLDEST CLOSED STAY and keeps the count, so a report can
// say how many it is not showing rather than quietly showing fewer. An OPEN
// stay is never dropped: it is the one the task is in now.
func stampSprintStay(next *Task, current Task, held bool, c applyContext) error {
	was := current.Sprint
	if !held {
		was = nil
	}
	now := next.Sprint
	if sameSprint(was, now) {
		return nil
	}
	at, err := effectiveOf(c)
	if err != nil {
		return err
	}
	stays := slices.Clone(next.SprintHistory)
	if was != nil {
		// CLOSE THE OPEN STAY ON THE SPRINT IT IS LEAVING, and only that
		// one: a task re-added to a sprint it already left has two stays
		// on that number, which is what the primary key's `from_at`
		// component is for.
		for i := range stays {
			if stays[i].Sprint == *was && stays[i].To == nil {
				stays[i].To = &at
				if now != nil {
					// A CARRY-OVER SAYS WHERE IT WENT, so a report
					// can tell work that rolled forward from work
					// that was dropped.
					rolled := *now
					stays[i].RolledTo = &rolled
				}
			}
		}
	}
	if now != nil {
		stays = append(stays, SprintStay{Sprint: *now, From: at})
	}
	next.SprintHistory, next.SprintStaysDropped = capStays(stays,
		next.SprintStaysDropped)
	return nil
}

// sameSprint compares two optional sprint numbers.
func sameSprint(a, b *int) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return *a == *b
}

// capStays drops the oldest CLOSED stays past [MaxSprintStays], counting them.
//
// Closed only, because an open stay is where the task is now — dropping it
// would make the task's own sprint unreadable from its history. Order is
// preserved, so the history stays oldest-first and a reader can page it.
func capStays(stays []SprintStay, dropped int) ([]SprintStay, int) {
	for len(stays) > MaxSprintStays {
		cut := -1
		for i := range stays {
			if stays[i].To != nil {
				cut = i
				break
			}
		}
		if cut < 0 {
			// EVERY STAY IS OPEN, which a task in sixteen concurrent
			// sprints would be and nothing else is. Nothing is dropped:
			// the cap bounds history, not the present.
			break
		}
		stays = append(stays[:cut], stays[cut+1:]...)
		dropped++
	}
	return stays, dropped
}

// effectiveOf is the instant a stamp derived from this record takes.
//
// THE BROKER'S, never a clock: the applier reads no clock at all, which is
// what makes a derived instant byte-identical on every node that applies the
// same record.
func effectiveOf(c applyContext) (time.Time, error) {
	if c.brokerAt.IsZero() {
		return time.Time{}, fmt.Errorf("tracker: the record at %s carries no "+
			"broker instant, and every derived stamp is that instant — a "+
			"clock read here would put a different number on every node",
			c.position)
	}
	return c.brokerAt.UTC(), nil
}

// readTask reads the stored document, reporting whether the row exists.
func readTask(ctx context.Context, tx *sql.Tx, id string) (Task, bool, error) {
	var document []byte
	var version, scoped int64
	err := tx.QueryRowContext(ctx,
		`SELECT document, version, scoped_through FROM tracker_tasks WHERE id = ?`,
		id).Scan(&document, &version, &scoped)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Task{}, false, nil
	case err != nil:
		return Task{}, false, fmt.Errorf("tracker: read task %s: %w", id, err)
	}
	var task Task
	if err := json.Unmarshal(document, &task); err != nil {
		return Task{}, false, fmt.Errorf("tracker: decode the stored task %s: %w", id, err)
	}
	task.Version = uint64(version)
	task.ScopedThrough = uint64(scoped)
	return task, true, nil
}

// mergeTask produces the new state from the stored one and the record.
//
// A CREATE CARRIES THE WHOLE TASK and a patch carries only what moved; both
// end at a complete document, which is what makes a replay's result
// independent of which records a node happened to have already.
func mergeTask(current Task, held bool, c applyContext) (Task, error) {
	switch OpKind(c.record.Op) {
	case OpCreate:
		var created Task
		if err := decodePayload(c.record.Mutation, &created); err != nil {
			return Task{}, fmt.Errorf("tracker: decode the create at %s: %w",
				c.position, err)
		}
		// A CREATE ON A LIVE ROW IS APPLIED, NOT REFUSED — and the
		// reason is the rule the whole applier is written to: a record
		// the broker committed is a record every node must be able to
		// consume, so a shape this node did not expect raises a flag or
		// writes what it says, and never stalls the log.
		//
		// It cannot arise from an ordinary write path: a create is
		// published at an expectation of zero and the broker arbitrates
		// it, so a second one on the same subject loses there. What
		// reaches here is a REDELIVERY at a position the checkpoint has
		// already passed — collapsed by the operation ledger on the
		// ordinary path and, when that ledger has been scrubbed by an
		// adoption, by this branch. Applying the payload is correct
		// because a create carries the COMPLETE state, so the row ends
		// where the record says either way.
		created.ID = c.subject().ID
		_ = held
		return created, nil
	case OpPatch, OpTombstone, OpRestore:
		var patch TaskPatch
		if err := decodePayload(c.record.Mutation, &patch); err != nil {
			return Task{}, fmt.Errorf("tracker: decode the patch at %s: %w",
				c.position, err)
		}
		if !held {
			// A PATCH FOR A TASK THIS NODE DOES NOT HAVE. Under a strict
			// replay the create is below this position, so this cannot
			// happen from the log — it is a gate that dropped the
			// create, and applying the patch would materialise a task
			// out of a partial description.
			return Task{}, fmt.Errorf("tracker: the patch at %s names task %s, "+
				"which this node does not have; its create was dropped by a "+
				"gate or never published", c.position, c.subject().ID)
		}
		return applyPatch(current, patch), nil
	}
	return Task{}, fmt.Errorf("tracker: %s is not an operation on a task", c.record.Op)
}

// applyPatch is the pointer-semantics merge.
//
// A NIL FIELD IS UNCHANGED and a non-nil one carries its COMPLETE new value —
// which is what tells "set this to empty" from "leave it alone". Every
// collection is a pointer to a slice for the same reason: an absent tag list
// means the write did not touch tags, and an empty one means it cleared them.
func applyPatch(task Task, patch TaskPatch) Task {
	setString(&task.Title, patch.Title)
	setString(&task.Body, patch.Body)
	setString(&task.Type, patch.Type)
	setString(&task.Assignee, patch.Assignee)
	setString(&task.RoutingUnit, patch.RoutingUnit)
	setString(&task.Project, patch.Project)
	if patch.Mint != nil {
		// THE MINT IS THE AUTHORITY FOR BOTH, derived rather than
		// carried, so a record cannot claim a key and a rank that
		// disagree about which counter value it took.
		task.Key = fmt.Sprintf("%s-%d", task.Project, patch.Mint.N)
		if rank, err := IntegerAt(patch.Mint.N); err == nil {
			task.Rank = rank
		}
	}
	if patch.Status != nil {
		task.Status = *patch.Status
		task.StatusGroup = task.Status.Group()
	}
	if patch.Priority != nil {
		task.Priority = *patch.Priority
	}
	if patch.Parent != nil {
		if *patch.Parent == "" {
			task.Parent = nil
		} else {
			parent := *patch.Parent
			task.Parent = &parent
		}
	}
	if patch.Sprint != nil {
		if *patch.Sprint == 0 {
			task.Sprint = nil
		} else {
			sprint := *patch.Sprint
			task.Sprint = &sprint
		}
	}
	if patch.StartAt != nil {
		task.StartAt = patch.StartAt
	}
	if patch.DueAt != nil {
		task.DueAt = patch.DueAt
	}
	if patch.DueAllDay != nil {
		task.DueAllDay = *patch.DueAllDay
	}
	if patch.EstimateMinutes != nil {
		task.EstimateMinutes = *patch.EstimateMinutes
	}
	if patch.Points != nil {
		task.Points = *patch.Points
	}
	if patch.Merging != nil {
		task.Merging = *patch.Merging
	}
	if patch.Reassignments != nil {
		task.Reassignments = *patch.Reassignments
	}
	if patch.Archived != nil {
		task.Archived = *patch.Archived
	}
	if patch.Removed != nil {
		if patch.Removed.At.IsZero() {
			task.Removed = nil
		} else {
			task.Removed = patch.Removed
		}
	}
	setSlice(&task.Collaborators, patch.Collaborators)
	setSlice(&task.Watchers, patch.Watchers)
	setSlice(&task.Muted, patch.Muted)
	setSlice(&task.Tags, patch.Tags)
	setSlice(&task.Relations, patch.Relations)
	setSlice(&task.Dependents, patch.Dependents)
	setSlice(&task.Checklists, patch.Checklists)
	setSlice(&task.SprintHistory, patch.SprintHistory)
	setSlice(&task.FormerKeys, patch.FormerKeys)
	if patch.Fields != nil {
		task.Fields = *patch.Fields
	}
	if patch.BodyRevision != nil {
		task.BodyVersion = patch.BodyRevision.Version + 1
	}
	return task
}

func setString(into *string, from *string) {
	if from != nil {
		*into = *from
	}
}

func setSlice[T any](into *[]T, from *[]T) {
	if from != nil {
		*into = *from
	}
}

// upsertTask writes the task's own row.
func upsertTask(ctx context.Context, tx *sql.Tx, task Task, document []byte,
	c applyContext) (int, error) {

	rank := task.Rank
	if rank == "" {
		// A ROW WITHOUT A RANK CANNOT EXIST — the column carries a CHECK
		// — and a task that reached here without one is a writer that
		// did not mint it. The origin is the honest fallback: it puts
		// the task somewhere rather than refusing a record the broker
		// already committed.
		rank = RankOrigin
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_tasks
			(id, key, project_key, filed_unit, routing_unit, sprint_number,
			 parent_id, root_id, depth, type, title, body_version, status,
			 status_group, status_entered_at, priority, prio_rank, rank,
			 reporter, assignee, start_at, due_at, due_all_day, estimate_min,
			 points, spend_turns, spend_rounds, spend_input, spend_output,
			 spend_cache_read, spend_cache_write, spend_wall_ms, spend_tokens,
			 done_at, closed_at, finished_at, archived, archived_at, removed_at,
			 removed_with, batch_id, merging, reassignments, policy_stamp,
			 unblocked_told_at, search_rev, embed_rev, inconsistent_project,
			 cycle, too_deep, key_collision, created_at, updated_at, version,
			 scoped_through, document)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,?,?,?,?,?,?,?,?,
		        ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,0,0,0,0,0,0,?,?,?,0,?)
		ON CONFLICT (id) DO UPDATE SET
			key = excluded.key, project_key = excluded.project_key,
			routing_unit = excluded.routing_unit,
			sprint_number = excluded.sprint_number,
			parent_id = excluded.parent_id, root_id = excluded.root_id,
			depth = excluded.depth, type = excluded.type, title = excluded.title,
			body_version = excluded.body_version, status = excluded.status,
			status_group = excluded.status_group, priority = excluded.priority,
			prio_rank = excluded.prio_rank, rank = excluded.rank,
			assignee = excluded.assignee, start_at = excluded.start_at,
			due_at = excluded.due_at, due_all_day = excluded.due_all_day,
			estimate_min = excluded.estimate_min, points = excluded.points,
			done_at = excluded.done_at, closed_at = excluded.closed_at,
			finished_at = excluded.finished_at, archived = excluded.archived,
			archived_at = excluded.archived_at, removed_at = excluded.removed_at,
			removed_with = excluded.removed_with, batch_id = excluded.batch_id,
			merging = excluded.merging,
			reassignments = excluded.reassignments,
			policy_stamp = excluded.policy_stamp,
			updated_at = excluded.updated_at, version = excluded.version,
			document = excluded.document
		WHERE excluded.version > tracker_tasks.version`,
		task.ID, task.Key, task.Project, task.FiledUnit, task.RoutingUnit,
		nullableInt(task.Sprint), nullableStringPtr(task.Parent), rootOf(task),
		task.Depth, typeOf(task), task.Title, task.BodyVersion,
		string(statusOf(task)), string(statusOf(task).Group()),
		string(priorityOf(task)), priorityOf(task).Rank(), string(rank),
		task.Reporter, task.Assignee, nullableTime(task.StartAt),
		nullableTime(task.DueAt), boolInt(task.DueAllDay), task.EstimateMinutes,
		task.Points, task.Spend.Turns, task.Spend.Rounds, task.Spend.Input,
		task.Spend.Output, task.Spend.CacheRead, task.Spend.CacheWrite,
		task.Spend.WallMs, task.Spend.Tokens, nullableTime(task.DoneAt),
		nullableTime(task.ClosedAt), nullableTime(task.FinishedAt()),
		boolInt(task.Archived), nullableTime(task.ArchivedAt),
		removedAt(task), removedWith(task), batchOf(c.record),
		boolInt(task.Merging), task.Reassignments, task.PolicyStamp,
		store.EncodeTime(task.CreatedAt), store.EncodeTime(task.UpdatedAt),
		c.packed, document)
	if err != nil {
		return 0, fmt.Errorf("tracker: write task %s at %s: %w", task.ID, c.position, err)
	}
	return affected(res)
}

// explodeTask rewrites the child rows a task's collections produce.
//
// WHOLESALE PER COLLECTION rather than diffed, because the record carries a
// touched collection WHOLE: a delete-then-insert of one task's tags is a
// handful of rows and is a pure function of the document, where a diff would
// depend on what this node happened to hold.
func (a *Applier) explodeTask(ctx context.Context, tx *sql.Tx, task Task,
	c applyContext) (int, error) {

	written := 0
	for _, child := range []struct {
		table string
		write func() (int, error)
	}{
		{"tracker_watchers", func() (int, error) {
			muted := map[string]bool{}
			for _, handle := range task.Muted {
				muted[handle] = true
			}
			n := 0
			for _, handle := range task.Watchers {
				res, err := tx.ExecContext(ctx, `
					INSERT INTO tracker_watchers (task_id, handle, muted)
					VALUES (?,?,?)
					ON CONFLICT (task_id, handle) DO UPDATE SET
						muted = excluded.muted`,
					task.ID, handle, boolInt(muted[handle]))
				if err != nil {
					return 0, err
				}
				rows, err := affected(res)
				if err != nil {
					return 0, err
				}
				n += rows
			}
			return n, nil
		}},
		{"tracker_collaborators", func() (int, error) {
			return insertMany(ctx, tx,
				`INSERT INTO tracker_collaborators (task_id, handle) VALUES (?,?)
				 ON CONFLICT (task_id, handle) DO NOTHING`,
				task.Collaborators, func(h string) []any { return []any{task.ID, h} })
		}},
		{"tracker_task_tags", func() (int, error) {
			return insertMany(ctx, tx,
				`INSERT INTO tracker_task_tags (task_id, project_key, slug)
				 VALUES (?,?,?)
				 ON CONFLICT (task_id, slug) DO NOTHING`,
				task.Tags, func(s string) []any { return []any{task.ID, task.Project, s} })
		}},
		{"tracker_task_sprints", func() (int, error) {
			// ONE ROW PER STAY, which is what every sprint report is
			// derived from — and what made `sprint=` answer zero for
			// every task until something wrote one. The rows are
			// rebuilt from the document on every apply, like every
			// other collection here, so a reprocess converges rather
			// than accumulating.
			return insertMany(ctx, tx, `
				INSERT INTO tracker_task_sprints
					(task_id, sprint, project_key, from_at, to_at, rolled_to)
				VALUES (?,?,?,?,?,?)
				ON CONFLICT (task_id, sprint, from_at) DO UPDATE SET
					to_at = excluded.to_at,
					rolled_to = excluded.rolled_to`,
				task.SprintHistory, func(v SprintStay) []any {
					return []any{task.ID, v.Sprint, task.Project,
						store.EncodeTime(v.From), nullableTime(v.To),
						nullableInt(v.RolledTo)}
				})
		}},
		{"tracker_relations", func() (int, error) {
			return insertMany(ctx, tx, `
				INSERT INTO tracker_relations
					(task_id, other_id, kind, derived, one_sided,
					 one_sided_final, note)
				VALUES (?,?,?,0,?,?,?)
				ON CONFLICT (task_id, other_id, kind) DO UPDATE SET
					one_sided = excluded.one_sided,
					one_sided_final = excluded.one_sided_final,
					note = excluded.note`,
				task.Relations, func(r Relation) []any {
					return []any{task.ID, r.Other, string(r.Kind),
						boolInt(r.OneSided), boolInt(r.OneSidedFinal), r.Note}
				})
		}},
	} {
		// The delete and the inserts are one statement list, so a
		// collection the record cleared ends empty rather than keeping
		// whatever this node happened to hold.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+child.table+` WHERE task_id = ?`, task.ID); err != nil {
			return 0, fmt.Errorf("tracker: clear %s for %s: %w",
				child.table, task.ID, err)
		}
		n, err := child.write()
		if err != nil {
			return 0, fmt.Errorf("tracker: write %s for %s: %w",
				child.table, task.ID, err)
		}
		written += n
	}

	// THE FIELD VALUES ARE THEIR OWN COLLECTION, and they are not in the
	// loop above because the delete-then-insert there is keyed on task_id
	// alone while this one needs the DECLARATIONS to decide which typed
	// column each value goes in — a read the loop's shape has no room for.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_field_values WHERE task_id = ?`, task.ID); err != nil {
		return 0, fmt.Errorf("tracker: clear tracker_field_values for %s: %w",
			task.ID, err)
	}
	values, err := a.explodeFieldValues(ctx, tx, task)
	if err != nil {
		return 0, err
	}
	written += values

	deps, err := a.maintainDeps(ctx, tx, task)
	if err != nil {
		return 0, err
	}
	refs, err := a.maintainReferences(ctx, tx, task)
	if err != nil {
		return 0, err
	}
	keys, err := a.maintainKeys(ctx, tx, task)
	if err != nil {
		return 0, err
	}
	closure, err := a.maintainClosure(ctx, tx, task)
	if err != nil {
		return 0, err
	}
	return written + deps + refs + keys + closure, nil
}

// maintainDeps derives the dependency edges from the task's own relations.
//
// DERIVED RATHER THAN CARRIED, so the two flags a query reads — is this task
// blocked, and when was it cleared — are a pure function of the rows present
// and identical whatever order the records arrived in.
func (a *Applier) maintainDeps(ctx context.Context, tx *sql.Tx, task Task) (int, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_task_deps WHERE task_id = ?`, task.ID); err != nil {
		return 0, fmt.Errorf("tracker: clear the dependencies of %s: %w", task.ID, err)
	}
	written := 0
	for _, relation := range task.Relations {
		if relation.Kind != RelationWaitingOn {
			continue
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tracker_task_deps (blocker_id, task_id, blocker_open, cleared_at)
			SELECT ?, ?,
			       COALESCE((SELECT CASE WHEN b.status_group IN ('done','closed')
			                             THEN 0 ELSE 1 END
			                 FROM tracker_tasks b WHERE b.id = ?), 1),
			       (SELECT b.finished_at FROM tracker_tasks b WHERE b.id = ?)
			ON CONFLICT (blocker_id, task_id) DO UPDATE SET
				blocker_open = excluded.blocker_open,
				cleared_at = excluded.cleared_at`,
			relation.Other, task.ID, relation.Other, relation.Other)
		if err != nil {
			return 0, fmt.Errorf("tracker: write the dependency %s→%s: %w",
				relation.Other, task.ID, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	// AND THE OTHER DIRECTION: this task finishing clears every edge that
	// names it as a blocker. It is the same statement on every node,
	// against the row this transaction has just written, so two nodes
	// cannot disagree about whether a dependent is still blocked.
	res, err := tx.ExecContext(ctx, `
		UPDATE tracker_task_deps SET
			blocker_open = CASE WHEN ? IN ('done','closed') THEN 0 ELSE 1 END,
			cleared_at = ?
		WHERE blocker_id = ?`,
		string(statusOf(task).Group()), nullableTime(task.FinishedAt()), task.ID)
	if err != nil {
		return 0, fmt.Errorf("tracker: clear the dependents of %s: %w", task.ID, err)
	}
	n, err := affected(res)
	if err != nil {
		return 0, err
	}
	return written + n, nil
}

// taskKeyPattern is what a body or a comment is scanned for.
var taskKeyPattern = regexp.MustCompile(`[A-Z][A-Z0-9]{1,9}-[0-9]+`)

// taskKeysIn is every distinct key a body names, IN DOCUMENT ORDER.
//
// Document order rather than sorted, and the difference is which sixty-four
// survive the cap: sorting drops the key somebody wrote first in favour of
// sixty-four that happen to start with an earlier letter. Both are
// deterministic and therefore both replicate — this one also links what the
// author was writing about.
//
// ONE SCANNER for the applier's rows and the write path's warning, because
// two would let a body be warned about at one count and capped at another.
func taskKeysIn(body string) []string {
	found := taskKeyPattern.FindAllString(body, -1)
	seen := make(map[string]bool, len(found))
	keys := make([]string, 0, len(found))
	for _, key := range found {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}

// maintainReferences derives the mention graph from the task's own body.
//
// CAPPED AFTER DEDUPE, so a pathological body is a bounded number of lookups
// on every node rather than one per occurrence — and the cap is on what the
// body NAMES rather than on what it contains, which is the difference between
// sixty-four lookups and five thousand.
func (a *Applier) maintainReferences(ctx context.Context, tx *sql.Tx, task Task) (int, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_references WHERE from_task = ? AND from_comment = ''`,
		task.ID); err != nil {
		return 0, fmt.Errorf("tracker: clear the references of %s: %w", task.ID, err)
	}
	keys := taskKeysIn(task.Body)
	if len(keys) > MaxReferencesPerBody {
		keys = keys[:MaxReferencesPerBody]
	}
	written := 0
	for _, key := range keys {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tracker_references (to_task, from_task, from_comment)
			SELECT k.task_id, ?, '' FROM tracker_task_keys k
			WHERE k.key = ? AND k.task_id <> ?
			ON CONFLICT (to_task, from_task, from_comment) DO NOTHING`,
			task.ID, key, task.ID)
		if err != nil {
			return 0, fmt.Errorf("tracker: write a reference from %s: %w", task.ID, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// maintainKeys keeps the key directory, current and former.
func (a *Applier) maintainKeys(ctx context.Context, tx *sql.Tx, task Task) (int, error) {
	written := 0
	for _, entry := range append([]string{task.Key}, task.FormerKeys...) {
		if entry == "" {
			continue
		}
		current := boolInt(entry == task.Key)
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tracker_task_keys (key, task_id, current) VALUES (?,?,?)
			ON CONFLICT (key) DO UPDATE SET current = excluded.current
			WHERE tracker_task_keys.task_id = excluded.task_id`,
			entry, task.ID, current)
		if err != nil {
			return 0, fmt.Errorf("tracker: claim key %s for %s: %w",
				entry, task.ID, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// maintainProjectCounts moves the three maintained counters.
//
// MAINTAINED, NEVER SCANNED: an aggregate over every task in every project on
// every sixty-second poll is half a million index entries at year five, for
// three numbers a commit already knows how to move. They are a pure function
// of the applied records, which is what lets the identity assertion recompute
// them and compare.
func (a *Applier) maintainProjectCounts(ctx context.Context, tx *sql.Tx,
	current, next Task, held bool) (int, error) {

	was, is := "", bucketOf(next)
	if held {
		was = bucketOf(current)
	}
	if was == is && current.Project == next.Project {
		return 0, nil
	}
	written := 0
	if was != "" {
		res, err := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE tracker_projects SET %s_count = MAX(%s_count - 1, 0) WHERE key = ?`,
			was, was), current.Project)
		if err != nil {
			return 0, fmt.Errorf("tracker: lower the %s count of %s: %w",
				was, current.Project, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	if is != "" {
		res, err := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE tracker_projects SET %s_count = %s_count + 1 WHERE key = ?`,
			is, is), next.Project)
		if err != nil {
			return 0, fmt.Errorf("tracker: raise the %s count of %s: %w",
				is, next.Project, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// bucketOf is which maintained count a task belongs to, or empty for a task
// that belongs to none — a removed one.
func bucketOf(task Task) string {
	if task.Removed != nil {
		return ""
	}
	switch statusOf(task).Group() {
	case GroupDone:
		return "done"
	case GroupClosed:
		return "closed"
	}
	return "open"
}

// purgeTask is the one operation that removes rows, and it removes them
// EVERYWHERE the task is named.
//
// Its own rows and its OUTBOUND references go with it; so do the INBOUND ones,
// with the alias rows that made the key resolvable at all — because a
// reference to a key nothing resolves is a dangling link a reader cannot tell
// from a typo. All in one transaction, and the marker it writes is what stops
// a redelivery months later resurrecting any of it.
//
// # Its CHILDREN are re-parented, not destroyed and not orphaned
//
// A purge names ONE task. Destroying the subtree under it would destroy work
// nobody asked about — a purge has no inverse, so "it was under the thing you
// purged" is not a confirmation anybody gave. Leaving the children alone is
// worse: each one's `parent_id` would name a row that no longer exists, which
// no reader can distinguish from a task whose parent is merely on another
// node, and the next edit to such a child would derive its depth and root from
// a chain that stops at nothing.
//
// So each direct child is moved onto the purged task's OWN parent — the
// grandparent, or the root when the purged task was one — and its subtree's
// ancestry is rebuilt. Depth can only fall, so no cap is crossed by the move.
//
// AND IT IS DONE IN THE APPLIER RATHER THAN REFUSED AT THE WRITER, because a
// refusal cannot close the race: a child is created by a write to the CHILD's
// subject, which does not contend with a write to this one, so a purge decided
// against a childless snapshot can still land after a child arrives. The
// applier sees the rows as they are at this position, and every node sees the
// same ones.
func (a *Applier) purgeTask(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	id := c.subject().ID
	// The row is read BEFORE it is deleted, because the marker records
	// what the task WAS: a deletion whose key and project are empty is a
	// marker nobody can resolve back to anything.
	task, _, err := readTask(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	// AND SO ARE ITS CHILDREN, for the same reason: after the deletes
	// below there is nothing left to ask.
	children, err := childrenOf(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM tracker_references WHERE from_task = ? OR to_task = ?`, []any{id, id}},
		{`DELETE FROM tracker_task_keys WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_watchers WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_collaborators WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_task_tags WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_relations WHERE task_id = ? OR other_id = ?`, []any{id, id}},
		{`DELETE FROM tracker_task_deps WHERE task_id = ? OR blocker_id = ?`, []any{id, id}},
		{`DELETE FROM tracker_checklist_items WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_field_values WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_task_sprints WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_task_closure WHERE ancestor_id = ? OR descendant_id = ?`, []any{id, id}},
		{`DELETE FROM tracker_body_revisions WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_comments WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_status_spans WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_tasks WHERE id = ?`, []any{id}},
	} {
		res, err := tx.ExecContext(ctx, statement.sql, statement.args...)
		if err != nil {
			return 0, fmt.Errorf("tracker: purge %s at %s: %w", id, c.position, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_deletions
			(task_id, task_key, project_key, purge_record_id, by, by_kind,
			 reason, committed_seq, log_stream, log_generation, at, document)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (task_id) DO NOTHING`,
		id, task.Key, task.Project, c.record.OpID, c.record.Actor,
		string(c.record.ActorKind), purgeReason(c), c.packed,
		c.position.Stream, c.position.Generation,
		store.EncodeTime(c.brokerAt), []byte(c.record.Mutation))
	if err != nil {
		return 0, fmt.Errorf("tracker: mark %s purged at %s: %w", id, c.position, err)
	}
	marker, err := affected(res)
	if err != nil {
		return 0, err
	}
	moved, err := a.reparent(ctx, tx, children, task.Parent)
	if err != nil {
		return 0, err
	}
	// A PURGE MOVES NO FIELD — the row is gone, and a delta naming what
	// it used to hold would be the content the purge exists to destroy.
	history, err := a.writeHistory(ctx, tx, c, task.Project, nil)
	if err != nil {
		return 0, err
	}
	return written + marker + moved + history, nil
}

// reparent moves each child onto parent and rebuilds its subtree's ancestry.
//
// AFTER THE DELETES, deliberately: [Applier.maintainClosure] walks the parent
// chain, and run before them it would walk through the row this purge is
// removing and write an ancestry naming it.
func (a *Applier) reparent(ctx context.Context, tx *sql.Tx, children []string,
	parent *string) (int, error) {

	written := 0
	for _, child := range children {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tracker_tasks SET parent_id = ? WHERE id = ?`,
			parent, child); err != nil {
			return 0, fmt.Errorf("tracker: re-parent %s: %w", child, err)
		}
		written++
		n, err := a.maintainClosure(ctx, tx, Task{ID: child, Parent: parent})
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// childrenOf reads a task's DIRECT children.
//
// From the parent pointers rather than the closure, because what this needs is
// the tasks whose own `parent_id` would dangle — and the closure holds every
// descendant, whose pointers name their own parents and are unaffected.
func childrenOf(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM tracker_tasks WHERE parent_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the children of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var child string
		if err := rows.Scan(&child); err != nil {
			return nil, fmt.Errorf("tracker: scan a child of %s: %w", id, err)
		}
		out = append(out, child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the children of %s: %w", id, err)
	}
	return out, nil
}

// purgeReason reads the operator's stated reason out of the payload, which is
// the one field of a purge anybody reads later.
func purgeReason(c applyContext) string {
	var payload struct {
		Reason string `json:"reason"`
	}
	_ = decodePayload(c.record.Mutation, &payload)
	return payload.Reason
}

// The small readers, so a zero value means the same thing at every call site.

func statusOf(task Task) Status {
	if task.Status.Valid() {
		return task.Status
	}
	return StatusTodo
}

func priorityOf(task Task) Priority {
	if task.Priority.Valid() {
		return task.Priority
	}
	return PriorityNone
}

func typeOf(task Task) string {
	if task.Type == "" {
		return "task"
	}
	return task.Type
}

// rootOf is the value the object row is CREATED with, before its closure
// exists.
//
// Always the task's own id, and that is not a placeholder: the closure rebuild
// runs in this same transaction and overwrites it with the real root. Guessing
// the parent's root here would be a second derivation of the same fact, and the
// two would disagree the first time a re-parent landed out of order.
func rootOf(task Task) string { return task.ID }

func removedAt(task Task) any {
	if task.Removed == nil {
		return nil
	}
	return store.EncodeTime(task.Removed.At)
}

func removedWith(task Task) any {
	if task.Removed == nil || task.Removed.RemovedWith == nil {
		return nil
	}
	return *task.Removed.RemovedWith
}

func nullableStringPtr(v *string) any {
	if v == nil || *v == "" {
		return nil
	}
	return *v
}

// insertMany runs one statement per element, which is what a bounded
// collection deserves: every one of them is capped, so the loop is a handful
// of statements and a generated multi-row insert would be a second statement
// builder to keep correct.
func insertMany[T any](ctx context.Context, tx *sql.Tx, statement string,
	items []T, args func(T) []any) (int, error) {

	written := 0
	for _, item := range items {
		res, err := tx.ExecContext(ctx, statement, args(item)...)
		if err != nil {
			return 0, err
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// explode writes the child rows a whole-document object produces.
func (a *Applier) explode(ctx context.Context, tx *sql.Tx, subject Subject,
	c applyContext) (int, error) {

	switch subject.Kind {
	case KindTags:
		var set TagSet
		if err := decodePayload(c.record.Mutation, &set); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tracker_tags WHERE project_key = ?`, subject.ID); err != nil {
			return 0, fmt.Errorf("tracker: clear the tags of %s: %w", subject.ID, err)
		}
		return insertMany(ctx, tx, `
			INSERT INTO tracker_tags
				(project_key, slug, label, label_norm, color, description,
				 archived, tags_version)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT (project_key, slug) DO UPDATE SET
				label = excluded.label, label_norm = excluded.label_norm,
				color = excluded.color, description = excluded.description,
				archived = excluded.archived,
				tags_version = excluded.tags_version`,
			set.Tags, func(tag Tag) []any {
				return []any{subject.ID, tag.Slug, tag.Label,
					strings.ToLower(tag.Label), tag.Color, tag.Description,
					boolInt(tag.Archived), set.TagsVersion}
			})
	case KindCatalogue:
		return a.explodeCatalogue(ctx, tx, subject.ID, c)
	case KindGoal:
		return a.explodeGoal(ctx, tx, subject.ID, c)
	case KindView:
		return a.settleDefaultView(ctx, tx, subject.ID, c)
	}
	return 0, nil
}

// settleDefaultView makes "one per container can be the default" TRUE.
//
// # Why this is the applier's and not the writer's
//
// The rule is about the ROWS rather than about any one write. A writer that
// cleared the previous default with a companion append would leave a window in
// which two rows claim it, and a reader would then need a tie-break rule
// nobody wrote down — where the nearest surface, a tab strip, would just draw
// two active tabs.
//
// Here it is one transaction with the view's own row: either the container has
// exactly one default afterwards or the record did not apply. The fan-out is
// every OTHER view in the container, which is unbounded in principle — and
// that is precisely why the record's scope names the CONTAINER rather than
// enumerating the views, which is [MaxScopeTerms]'s own rule.
//
// A view that is NOT the default clears nothing: withdrawing a default is
// saving that view with the flag off, and doing it by writing some other view
// as the default is a second gesture the person did not make.
func (a *Applier) settleDefaultView(ctx context.Context, tx *sql.Tx, id string,
	c applyContext) (int, error) {

	var view View
	if err := decodePayload(c.record.Mutation, &view); err != nil {
		return 0, err
	}
	if !view.Default {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE tracker_views SET is_default = 0
		WHERE container_kind = ? AND container_id = ? AND id <> ?
		  AND is_default = 1`,
		view.Container.Kind, view.Container.ID, id)
	if err != nil {
		return 0, fmt.Errorf("tracker: clear the other defaults in %s %s: %w",
			view.Container.Kind, view.Container.ID, err)
	}
	return affected(res)
}

// explodeCatalogue writes the type or field rows a catalogue produces.
func (a *Applier) explodeCatalogue(ctx context.Context, tx *sql.Tx, name string,
	c applyContext) (int, error) {

	if name == CatalogueTypes {
		var catalogue TypeCatalogue
		if err := decodePayload(c.record.Mutation, &catalogue); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tracker_types`); err != nil {
			return 0, fmt.Errorf("tracker: clear the type catalogue: %w", err)
		}
		return insertMany(ctx, tx, `
			INSERT INTO tracker_types
				(slug, name, name_norm, plural, icon, description, builtin, archived)
			VALUES (?,?,?,?,?,?,?,?)`,
			catalogue.Types, func(t TaskType) []any {
				return []any{t.Slug, t.Name, strings.ToLower(t.Name), t.Plural,
					t.Icon, t.Description, boolInt(t.Builtin), boolInt(t.Archived)}
			})
	}
	var catalogue FieldCatalogue
	if err := decodePayload(c.record.Mutation, &catalogue); err != nil {
		return 0, err
	}
	written, err := writeFieldDefs(ctx, tx, FieldScopeWorkspace, "", catalogue.Fields)
	if err != nil {
		return 0, err
	}
	// AND THE VALUES FOLLOW THE DECLARATION'S ARCHIVE, in this same
	// transaction.
	//
	// `hidden` on a value row is the DECLARATION's archived state, and
	// nothing else propagated it: a task untouched since the archive kept
	// `hidden = 0` for ever, so the DDL's own rule — "every filter and
	// total adds `hidden = 0 AND kind <> 'foreign'`" — described a column
	// that did not carry what it claimed. The filter is shielded anyway,
	// because an archived field does not RESOLVE, but a row that lies
	// about its own state is a trap for the next reader of it.
	swept, err := a.settleFieldVisibility(ctx, tx, catalogue.Fields)
	if err != nil {
		return 0, err
	}
	return written + swept, nil
}

// The two SCOPES a field is declared at, as the rows spell them.
//
// CONSTANTS RATHER THAN LITERALS, because the string appears in an INSERT, in
// a scoped DELETE and in every future read of the union — and three spellings
// of "workspace" is a row set that deletes nothing and accumulates for ever.
const (
	FieldScopeWorkspace = "workspace"
	FieldScopeProject   = "project"
)

// writeFieldDefs explodes one declaring document's fields into the rows.
//
// ONE FUNCTION FOR BOTH SCOPES. Fields are declared in two places — the
// workspace catalogue and a project — and the row set is a union keyed on
// `(scope_kind, scope_id)`, so a second copy of this explosion is how one
// scope's rows come to carry a column the other's do not. The DELETE is scoped
// the same way, which is what lets a project's edit leave the workspace's rows
// alone and the reverse.
func writeFieldDefs(ctx context.Context, tx *sql.Tx, scopeKind, scopeID string,
	fields []FieldDef) (int, error) {

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_fields WHERE scope_kind = ? AND scope_id = ?`,
		scopeKind, scopeID); err != nil {

		return 0, fmt.Errorf("tracker: clear the %s field declarations: %w",
			scopeKind, err)
	}
	written := 0
	for _, field := range fields {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tracker_fields
				(id, scope_kind, scope_id, slug, name, name_norm, description,
				 type, applies_to_json, required, required_in_subtasks, archived,
				 pinned, hide_from_agents, config_json, default_json, shadowed)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0)`,
			field.ID, scopeKind, scopeID, field.Slug, field.Name,
			strings.ToLower(field.Name), field.Description, string(field.Type),
			jsonOf(field.AppliesTo), boolInt(field.Required),
			boolInt(field.RequiredInSubtasks), boolInt(field.Archived),
			boolInt(field.Pinned), boolInt(field.HideFromAgents),
			jsonOf(field.Config), nullableRaw(field.Default))
		if err != nil {
			return 0, fmt.Errorf("tracker: write field %s: %w", field.Slug, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tracker_field_options WHERE field_id = ?`, field.ID); err != nil {
			return 0, fmt.Errorf("tracker: clear the options of %s: %w", field.Slug, err)
		}
		options, err := insertMany(ctx, tx, `
			INSERT INTO tracker_field_options
				(field_id, option_id, slug, name, name_norm, color, ord, archived)
			VALUES (?,?,?,?,?,?,?,?)`,
			field.Config.Options, func(o Option) []any {
				return []any{field.ID, o.ID, o.Slug, o.Name,
					strings.ToLower(o.Name), o.Color, o.Order, boolInt(o.Archived)}
			})
		if err != nil {
			return 0, fmt.Errorf("tracker: write the options of %s: %w", field.Slug, err)
		}
		written += options
	}
	return written, nil
}

// settleFieldVisibility makes every value row agree with its declaration.
//
// BOTH DIRECTIONS, because neither fact this column carries is one-way FOR THE
// COLUMN. An archive is one-way for the DECLARATION and this merely mirrors
// it, so a field that was never archived must not have hidden rows either, or
// a value written while a stale declaration was in force would stay out of
// every filter for ever. `AppliesTo` is not one-way at all: a declaration that
// adds a type has to bring those tasks' values back into the filterable set.
//
// TWO FACTS IN ONE COLUMN, and the statement computes both — a value is hidden
// when its field is ARCHIVED or when the field's `AppliesTo` excludes the TYPE
// of the task holding it. The type comparison is case-folded because
// [appliesTo] decides the same thing at explode time and folds too, and the
// two disagreeing would make a row's visibility depend on which write happened
// last.
//
// FOREIGN ROWS ARE UNTOUCHED. They are hidden by what they ARE rather than by
// any declaration, and a declaration arriving for one re-explodes it as native
// on the task's next write rather than flipping a column here.
func (a *Applier) settleFieldVisibility(ctx context.Context, tx *sql.Tx,
	fields []FieldDef) (int, error) {

	written := 0
	for _, field := range fields {
		var res sql.Result
		var err error
		switch {
		case field.Archived || len(field.AppliesTo) == 0:
			// ONE ANSWER FOR EVERY ROW: an archived field hides all
			// of its values and an unrestricted one hides none, and
			// neither needs to know which task a row sits on.
			want := boolInt(field.Archived)
			res, err = tx.ExecContext(ctx, `
				UPDATE tracker_field_values SET hidden = ?
				WHERE field_id = ? AND kind = ? AND hidden <> ?`,
				want, field.ID, FieldValueNative, want)
		default:
			types := make([]any, 0, len(field.AppliesTo))
			for _, name := range field.AppliesTo {
				types = append(types, strings.ToLower(name))
			}
			// A ROW WHOSE TASK IS GONE IS HIDDEN, which is what the
			// COALESCE says: the column is NOT NULL, and a value with
			// no task to be about cannot be shown to belong to one.
			expr := `COALESCE((SELECT CASE WHEN LOWER(t.type) IN (` +
				placeholders(len(types)) + `) THEN 0 ELSE 1 END ` +
				`FROM tracker_tasks t ` +
				`WHERE t.id = tracker_field_values.task_id), 1)`
			// THE SET CLAUSE'S ARGUMENTS BIND FIRST, then the
			// WHERE's, then the same expression's again: a
			// placeholder binds in statement order and this
			// expression appears twice.
			bound := append(append([]any{}, types...), field.ID, FieldValueNative)
			bound = append(bound, types...)
			res, err = tx.ExecContext(ctx, `
				UPDATE tracker_field_values SET hidden = `+expr+`
				WHERE field_id = ? AND kind = ? AND hidden <> `+expr, bound...)
		}
		if err != nil {
			return 0, fmt.Errorf("tracker: settle %s's value rows against its "+
				"declaration: %w", field.Slug, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// explodeGoal writes a goal's owners, targets and target references.
func (a *Applier) explodeGoal(ctx context.Context, tx *sql.Tx, id string,
	c applyContext) (int, error) {

	var goal Goal
	if err := decodePayload(c.record.Mutation, &goal); err != nil {
		return 0, err
	}
	for _, table := range []string{
		"tracker_goal_owners", "tracker_goal_targets", "tracker_goal_target_refs",
	} {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+table+` WHERE goal_id = ?`, id); err != nil {
			return 0, fmt.Errorf("tracker: clear %s for %s: %w", table, id, err)
		}
	}
	written := 0
	for member, handles := range map[int][]string{0: goal.Owners, 1: goal.Members} {
		n, err := insertMany(ctx, tx, `
			INSERT INTO tracker_goal_owners (goal_id, handle, member) VALUES (?,?,?)
			ON CONFLICT (goal_id, handle) DO UPDATE SET
				member = MIN(tracker_goal_owners.member, excluded.member)`,
			handles, func(h string) []any { return []any{id, h, member} })
		if err != nil {
			return 0, fmt.Errorf("tracker: write the owners of %s: %w", id, err)
		}
		written += n
	}
	for _, target := range goal.Targets {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tracker_goal_targets
				(goal_id, target_id, name, type, start, goal, current, unit, done)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			id, target.ID, target.Name, target.Type, target.Start, target.Goal,
			target.Current, target.Unit, boolInt(target.Done))
		if err != nil {
			return 0, fmt.Errorf("tracker: write a target of %s: %w", id, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
		for kind, refs := range map[string][]string{
			"task": target.Tasks, "project": target.Projects,
		} {
			n, err := insertMany(ctx, tx, `
				INSERT INTO tracker_goal_target_refs (goal_id, target_id, kind, ref)
				VALUES (?,?,?,?)
				ON CONFLICT (goal_id, target_id, kind, ref) DO NOTHING`,
				refs, func(ref string) []any { return []any{id, target.ID, kind, ref} })
			if err != nil {
				return 0, fmt.Errorf("tracker: write a target reference of %s: %w",
					id, err)
			}
			written += n
		}
	}
	return written, nil
}

func nullableRaw(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// writeThread writes the rows a record's own payload carries, as against the
// collections the DOCUMENT carries.
//
// # Why these three are not in [explodeTask]
//
// Everything exploded there is a collection ON the task document, rebuilt from
// it on every apply so a reprocess converges. A comment and a body revision
// are not: they are append-only rows one RECORD produced, and rebuilding them
// from the document is impossible because the document does not hold them —
// which is exactly what made them vanish. A task carried its thread on the
// wire and produced no row, so `get_task(include=comments)` answered an empty
// thread for every task in the company, `has_open_asks` was false for every
// one, and `my_work.asked_of_me` could not see a question anybody had asked.
//
// The checklists ARE a document collection and are exploded here rather than
// beside the others only because the same guard covers all three: a redelivery
// must not write a second copy of an append-only row.
func (a *Applier) writeThread(ctx context.Context, tx *sql.Tx, task Task,
	c applyContext) (int, error) {

	written := 0
	items, err := writeChecklistItems(ctx, tx, task)
	if err != nil {
		return 0, err
	}
	written += items

	if OpKind(c.record.Op) != OpPatch {
		// ONLY A PATCH CARRIES ONE. A create carries the task and a
		// tombstone carries a stamp, and decoding either as a patch to
		// look for a comment would be reading a shape that is not there.
		return written, nil
	}
	var patch TaskPatch
	if err := decodePayload(c.record.Mutation, &patch); err != nil {
		return 0, fmt.Errorf("tracker: decode the patch at %s: %w", c.position, err)
	}
	if patch.Comment != nil {
		n, err := writeComment(ctx, tx, task, *patch.Comment, c)
		if err != nil {
			return 0, err
		}
		written += n
	}
	if patch.BodyRevision != nil {
		n, err := writeBodyRevision(ctx, tx, task, *patch.BodyRevision, patch.Retires)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// writeComment writes one comment row, and every EDIT of it.
//
// AN UPSERT ON THE ID, because a comment is edited, answered, resolved and
// removed in place — each of those is a later patch naming the same comment,
// and an insert-only write would leave the thread showing the first version of
// every remark for ever. The id is derived from the operation, so a retried
// turn lands on the row it already wrote rather than saying the same thing
// twice.
func writeComment(ctx context.Context, tx *sql.Tx, task Task, comment Comment,
	c applyContext) (int, error) {

	if comment.ID == "" {
		// A COMMENT WITH NO ID IS NOT A ROW. It is dropped rather than
		// given one here: a minted id would differ on every node, and
		// two nodes' threads would stop matching.
		return 0, nil
	}
	comment.Task = task.ID
	document, err := json.Marshal(comment)
	if err != nil {
		return 0, fmt.Errorf("tracker: encode comment %s at %s: %w",
			comment.ID, c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_comments
			(id, task_id, author, author_kind, body, reply_to, ask, answers,
			 answered_by, resolved, resolved_by, resolved_at, removed,
			 record_id, created_at, updated_at, document)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO UPDATE SET
			body = excluded.body, ask = excluded.ask,
			answers = excluded.answers, answered_by = excluded.answered_by,
			resolved = excluded.resolved, resolved_by = excluded.resolved_by,
			resolved_at = excluded.resolved_at, removed = excluded.removed,
			record_id = excluded.record_id, updated_at = excluded.updated_at,
			document = excluded.document`,
		comment.ID, task.ID, comment.Author, string(comment.AuthorKind),
		comment.Body, comment.ReplyTo, comment.Ask,
		comment.Answers, nil,
		boolInt(comment.Resolved), nonEmpty(comment.ResolvedBy),
		nullableTime(comment.ResolvedAt), boolInt(comment.Removed),
		c.record.OpID, store.EncodeTime(comment.CreatedAt),
		store.EncodeTime(commentUpdatedAt(comment)), document)
	if err != nil {
		return 0, fmt.Errorf("tracker: write comment %s at %s: %w",
			comment.ID, c.position, err)
	}
	written, err := affected(res)
	if err != nil {
		return 0, err
	}
	// AND AN ANSWER CLOSES THE ASK IT NAMES. The two are separate rows —
	// a reply is its own comment — so the ask stays open on every board,
	// in every `has_open_asks` filter and in the answerer's own my_work
	// until this write lands.
	if comment.Answers != nil && *comment.Answers != "" {
		res, err := tx.ExecContext(ctx, `
			UPDATE tracker_comments SET answered_by = ?
			WHERE id = ? AND task_id = ? AND answered_by IS NULL`,
			comment.ID, *comment.Answers, task.ID)
		if err != nil {
			return 0, fmt.Errorf("tracker: close the ask %s answers at %s: %w",
				comment.ID, c.position, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// commentUpdatedAt is the edit instant, defaulting to the creation one so the
// column is never zero on a comment nobody has touched.
func commentUpdatedAt(comment Comment) time.Time {
	if comment.UpdatedAt.IsZero() {
		return comment.CreatedAt
	}
	return comment.UpdatedAt
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// writeBodyRevision keeps the body a write replaced, and prunes what the
// record says it pruned.
//
// THE PRUNE IS ON THE COMMIT, so every node deletes exactly the same rows at
// exactly the same position rather than each deciding for itself from a count
// it read locally.
func writeBodyRevision(ctx context.Context, tx *sql.Tx, task Task,
	revision BodyRevision, retires []int) (int, error) {

	document, err := json.Marshal(revision)
	if err != nil {
		return 0, fmt.Errorf("tracker: encode revision %d of %s: %w",
			revision.Version, task.ID, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_body_revisions
			(task_id, version, author, author_kind, at, size, document)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (task_id, version) DO NOTHING`,
		task.ID, revision.Version, revision.Author, string(revision.AuthorKind),
		store.EncodeTime(revision.At), len(revision.Body), document)
	if err != nil {
		return 0, fmt.Errorf("tracker: write revision %d of %s: %w",
			revision.Version, task.ID, err)
	}
	written, err := affected(res)
	if err != nil {
		return 0, err
	}
	for _, version := range retires {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM tracker_body_revisions WHERE task_id = ? AND version = ?`,
			task.ID, version)
		if err != nil {
			return 0, fmt.Errorf("tracker: retire revision %d of %s: %w",
				version, task.ID, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// writeChecklistItems rebuilds a task's sub-items from its document.
//
// A CLEAR-AND-REBUILD, like every other document collection here, so a
// reprocess converges rather than accumulating — and so an item DELETED from a
// checklist leaves the table, which an upsert-only write would never do.
func writeChecklistItems(ctx context.Context, tx *sql.Tx, task Task) (int, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_checklist_items WHERE task_id = ?`, task.ID); err != nil {
		return 0, fmt.Errorf("tracker: clear the checklist of %s: %w", task.ID, err)
	}
	written := 0
	for _, list := range task.Checklists {
		for i, item := range list.Items {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO tracker_checklist_items
					(task_id, checklist_id, item_id, name, done, assignee,
					 parent_id, ord, promoted_to)
				VALUES (?,?,?,?,?,?,?,?,?)
				ON CONFLICT (task_id, checklist_id, item_id) DO UPDATE SET
					name = excluded.name, done = excluded.done,
					assignee = excluded.assignee, parent_id = excluded.parent_id,
					ord = excluded.ord, promoted_to = excluded.promoted_to`,
				task.ID, list.ID, item.ID, item.Name, boolInt(item.Done),
				item.Assignee, item.Parent, i, item.PromotedTo)
			if err != nil {
				return 0, fmt.Errorf("tracker: write checklist item %s of %s: %w",
					item.ID, task.ID, err)
			}
			n, err := affected(res)
			if err != nil {
				return 0, err
			}
			written += n
		}
	}
	return written, nil
}
