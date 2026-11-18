package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/store"
)

// The task apply. A patch is MERGED into the decoded stored document and
// re-encoded, never written over the row's columns: fields this build does not
// know round-trip through the document, where a column write would lose them
// at the first rolling upgrade.

// applyTask writes one task record and everything derived from it.
func (a *Applier) applyTask(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	n, err := a.applyTaskRecord(ctx, tx, c)
	if err != nil {
		return 0, err
	}
	// AND ITS ASKS, which any change to the task may have overtaken or freed.
	overtaken, err := stampOvertaken(ctx, tx, c.subject().ID)
	if err != nil {
		return 0, err
	}
	return n + overtaken, nil
}

// applyTaskRecord applies one task record: its object row, history and comments.
func (a *Applier) applyTaskRecord(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	id := c.subject().ID
	if c.record.Op == OpPurge {
		return a.purgeTask(ctx, tx, c)
	}

	current, held, err := readTask(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if held && c.packed <= int64(max(current.Version, current.ScopedThrough)) {
		// A redelivery or reprocess. The history row is still written (its
		// own guard makes it idempotent), with the writer's stated deltas:
		// the document is already a successor's ([statedDeltas]).
		history, werr := a.writeHistory(ctx, tx, c,
			subjectKeys{Project: current.Project, Key: current.Key}, statedDeltas(c))
		if werr != nil {
			return 0, werr
		}
		// A late record still moves `updated_at`: its history row raised
		// every successor's effective instant ([restampUpdated]).
		restamped, rerr := restampUpdated(ctx, tx, id)
		if rerr != nil {
			return 0, rerr
		}
		return history + restamped, nil
	}

	next, err := mergeTask(current, held, c)
	if err != nil {
		return 0, err
	}
	next.Version = uint64(c.packed)

	// `updated_at` is the applier's: the fleet-agreed `effective_at` this
	// record's history row takes, never a writer's clock ([restampUpdated]
	// re-derives the same rule over every row).
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	updated, err := effectiveAt(ctx, tx, id, c)
	if err != nil {
		return 0, err
	}
	next.UpdatedAt = updated

	// The finish stamps are derived from the group just entered, never
	// carried: a writer's instant would be one node's clock, and a blocker
	// whose finish instant is null clears no dependency edge.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := stampFinish(&next, c); err != nil {
		return 0, err
	}

	// The archive stamp, on the same rule: the patch carries only the bool.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := stampArchive(&next, current, held, c); err != nil {
		return 0, err
	}

	document, err := json.Marshal(next)
	if err != nil {
		return 0, fmt.Errorf("worklet: encode task %s at %s: %w", id, c.position, err)
	}

	// The project's field declarations, read once for both readers:
	// [Applier.explodeFieldValues] (which typed column) and [TaskDeltas]
	// (name a value by slug), so the two cannot see different catalogues.
	// Read only when either side carries values, which keeps it off the hot
	// path; both sides, since a commit that cleared every value still names
	// what it cleared. Consumers rely on: nil map means neither side had one.
	var declared map[string]FieldDef
	if len(current.Fields) > 0 || len(next.Fields) > 0 {
		if declared, err = declaredFields(ctx, tx, next.Project); err != nil {
			return 0, err
		}
	}

	rows, err := upsertTask(ctx, tx, next, document, c)
	if err != nil {
		return 0, err
	}
	// The key directory moves only on a create or a re-key
	// ([Applier.maintainKeys]).
	rekeyed := !held || keysMoved(current, next)
	children, err := a.explodeTask(ctx, tx, next, declared, rekeyed, c)
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
	// What the apply actually did, from the two documents only this frame
	// holds; the history row and the entered stamp both derive from it.
	before := current
	if !held {
		before = Task{}
	}
	applied := TaskDeltas(before, next, declared)
	history, err := a.writeHistory(ctx, tx, c,
		subjectKeys{Project: next.Project, Key: next.Key}, applied)
	if err != nil {
		return 0, err
	}
	entered := 0
	// Gated on the status moving, not on a notification: a quiet status
	// change still moves the entered instant.
	if _, moved := applied["status"]; moved {
		if entered, err = a.stampStatusEntered(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	told, err := a.stampUnblocked(ctx, tx, c)
	if err != nil {
		return 0, err
	}
	return rows + children + thread + counts + history + entered + told, nil
}

// unblockedToldStatement is [Applier.stampUnblocked]'s write, named for the
// plan test.
const unblockedToldStatement = `
		UPDATE tracker_tasks
		SET unblocked_told_at = MAX(COALESCE(unblocked_told_at, 0), ?)
		WHERE id = ? AND COALESCE(unblocked_told_at, 0) < ?`

// stampUnblocked records that this commit told a dependent it is workable.
//
// The applier writes it, as a MAX over every applied commit naming the task
// unblocked: order-independent and identical on every node, so no redelivery,
// reprocess or replay re-issues a wake. A writer's instant would be one node's
// clock. It is the EFFECTIVE instant, never the authored one, because the
// repair compares it with the edge's fleet-agreed clearing time; on authored
// clocks a skew re-issues a late wake for every task cleared inside it.
func (a *Applier) stampUnblocked(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	if c.record.Notify == nil || len(c.record.Notify.Snapshot.Unblocked) == 0 {
		return 0, nil
	}
	written := 0
	for _, party := range c.record.Notify.Snapshot.Unblocked {
		if party.Task == "" {
			continue
		}
		// The dependent's own effective instant: a MAX over the records
		// applied at or below this position, so apply order cannot move it.
		effective, err := effectiveAt(ctx, tx, party.Task, c)
		if err != nil {
			return 0, err
		}
		at := store.EncodeTime(effective)
		res, err := tx.ExecContext(ctx, unblockedToldStatement, at, party.Task, at)
		if err != nil {
			return 0, fmt.Errorf("worklet: record that %s was told it is "+
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
// By group, not slug, since companies add and rename statuses. First entry
// wins within a group, and a reopen clears both, so the stamp is the entry
// into the state the task is in now, not the first time it ever got there.
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
		task.DoneAt, task.ClosedAt = nil, nil
	}
	return nil
}

// stampArchive records a task being filed away, and by whom.
//
// On the edge only: an edit of an archived task keeps its archive instant.
// Un-archiving clears both, on [stampFinish]'s rule. Who comes from the record
// and when from the broker, as for every derived stamp.
func stampArchive(task *Task, current Task, held bool, c applyContext) error {
	was := held && current.Archived
	switch {
	case task.Archived && !was:
		at, err := effectiveOf(c)
		if err != nil {
			return err
		}
		task.ArchivedAt, task.ArchivedBy = &at, c.record.Actor
	case !task.Archived:
		task.ArchivedAt, task.ArchivedBy = nil, ""
	}
	return nil
}

// effectiveOf is the instant a stamp derived from this record takes.
//
// The broker's, never a clock, so a derived instant is byte-identical on every
// node; already in the store's encoding ([statelog.Record.StoredAt]).
func effectiveOf(c applyContext) (time.Time, error) {
	if c.brokerAt.IsZero() {
		return time.Time{}, fmt.Errorf("worklet: the record at %s carries no "+
			"broker instant, and every derived stamp is that instant — a "+
			"clock read here would put a different number on every node",
			c.position)
	}
	return c.brokerAt, nil
}

// readTaskStatement is [readTask]'s read, named for the plan test.
const readTaskStatement = `SELECT document, version, scoped_through, rank FROM tracker_tasks WHERE id = ?`

// readTask reads the stored task, reporting whether the row exists, with the
// rank its ROW holds: after the create a task's place is the order's to write
// ([Applier.applyRankOrder]), so a task write carries the column's rank
// through and only a record that mints one (a create, a cross-project move)
// changes it.
func readTask(ctx context.Context, tx *sql.Tx, id string) (Task, bool, error) {
	var document []byte
	var version, scoped int64
	var rank string
	err := tx.QueryRowContext(ctx, readTaskStatement, id).Scan(&document, &version,
		&scoped, &rank)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Task{}, false, nil
	case err != nil:
		return Task{}, false, fmt.Errorf("worklet: read task %s: %w", id, err)
	}
	var task Task
	if err := json.Unmarshal(document, &task); err != nil {
		return Task{}, false, fmt.Errorf("worklet: decode the stored task %s: %w", id, err)
	}
	task.Version = uint64(version)
	task.ScopedThrough = uint64(scoped)
	task.Rank = Rank(rank)
	return task, true, nil
}

// mergeTask produces the new state from the stored one and the record.
//
// A create carries the whole task and a patch what moved; both end at a
// complete document, so a replay's result does not depend on what a node held.
func mergeTask(current Task, held bool, c applyContext) (Task, error) {
	switch c.record.Op {
	case OpCreate:
		// The question it was filed as is a comment row
		// ([Applier.writeThread]), never part of the document.
		var payload TaskCreate
		if err := decodePayload(c.record.Mutation, &payload); err != nil {
			return Task{}, fmt.Errorf("worklet: decode the create at %s: %w",
				c.position, err)
		}
		created := payload.Task
		// A create on a live row is applied, not refused: a committed
		// record never stalls the log. The broker refuses a second create
		// (expectation zero), so this is a redelivery whose operation-ledger
		// row was swept, and the complete state it carries ends the row
		// where the record says either way.
		created.ID = c.subject().ID
		_ = held
		return created, nil
	case OpPatch, OpTombstone, OpRestore:
		var patch TaskPatch
		if err := decodePayload(c.record.Mutation, &patch); err != nil {
			return Task{}, fmt.Errorf("worklet: decode the patch at %s: %w",
				c.position, err)
		}
		if !held {
			// The create is below this position, so a gate dropped it;
			// applying the patch would build a task from a partial one.
			return Task{}, fmt.Errorf("worklet: the patch at %s names task %s, "+
				"which this node does not have; its create was dropped by a "+
				"gate or never published", c.position, c.subject().ID)
		}
		return applyPatch(current, patch), nil
	}
	return Task{}, fmt.Errorf("worklet: %s is not an operation on a task", c.record.Op)
}

// Patched is [applyPatch] exported for a tool composing the WAKE that
// announces a write it is about to make. Exported rather than reimplemented,
// so a field added to [TaskPatch] cannot reach the row and miss the wake's
// snapshot. The caller layers only its own part on top: the recipients of
// the watch gesture in flight.
func Patched(task Task, patch TaskPatch) Task { return applyPatch(task, patch) }

// applyPatch is the pointer-semantics merge.
//
// A nil field is unchanged and a non-nil one carries its complete new value,
// which tells "set to empty" from "leave alone"; hence a pointer to every
// collection's slice.
func applyPatch(task Task, patch TaskPatch) Task {
	setString(&task.Title, patch.Title)
	setString(&task.Body, patch.Body)
	setString(&task.Type, patch.Type)
	setString(&task.Assignee, patch.Assignee)
	setString(&task.RoutingUnit, patch.RoutingUnit)
	setString(&task.Project, patch.Project)
	if patch.Mint != nil {
		// Key and rank both derive from the mint, so they cannot disagree.
		task.Key = mintedKey(task.Project, patch.Mint.N)
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
	// A cleared date is its flag, never the zero instant, which the writer
	// refuses, as it does a patch both setting and clearing one
	// ([TaskPatch.CheckDates]).
	switch {
	case patch.ClearStart:
		task.StartAt = nil
	case patch.StartAt != nil:
		task.StartAt = patch.StartAt
	}
	if patch.WaitReason != nil {
		task.WaitReason = *patch.WaitReason
	}
	if s := patch.ChangeSettled; s != nil && s.Ref != "" {
		task.Changes = maps.Clone(task.Changes)
		if task.Changes == nil {
			task.Changes = map[string]ChangeSettlement{}
		}
		task.Changes[s.Ref] = *s
	}
	if d := patch.DeadWait; d != nil && d.Ref != "" {
		task.DeadWaits = maps.Clone(task.DeadWaits)
		if task.DeadWaits == nil {
			task.DeadWaits = map[string]DeadWait{}
		}
		task.DeadWaits[d.Ref] = *d
	}
	if ref := patch.DeadWaitEnded; ref != nil {
		task.DeadWaits = maps.Clone(task.DeadWaits)
		delete(task.DeadWaits, *ref)
	}
	switch {
	case patch.ClearDue:
		task.DueAt = nil
	case patch.DueAt != nil:
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
	if patch.MergeReparent != nil {
		task.MergeReparent = *patch.MergeReparent
	}
	if patch.Merging != nil {
		task.Merging = *patch.Merging
		if !task.Merging {
			// The re-parent intent describes a running walk, so it
			// goes down with the marker whatever this patch said.
			task.MergeReparent = false
		}
	}
	if patch.Moving != nil {
		task.Moving = *patch.Moving
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
	setSlice(&task.AskWatch, patch.AskWatch)
	setSlice(&task.Muted, patch.Muted)
	setSlice(&task.Tags, patch.Tags)
	setSlice(&task.Relations, patch.Relations)
	setSlice(&task.Dependents, patch.Dependents)
	setSlice(&task.Checklists, patch.Checklists)
	setSlice(&task.FormerKeys, patch.FormerKeys)
	if patch.Fields != nil {
		task.Fields = *patch.Fields
	}
	if patch.BodyRevision != nil {
		task.BodyVersion = patch.BodyRevision.Version + 1
	}
	return pruneDeadWaits(task)
}

// pruneDeadWaits drops each dead wait on a change the task no longer waits on,
// or one recorded settled: only an open wait can be dead.
func pruneDeadWaits(task Task) Task {
	kept := maps.Clone(task.DeadWaits)
	maps.DeleteFunc(kept, func(ref string, _ DeadWait) bool {
		_, settled := task.Changes[ref]
		return settled || !waitsOn(task, ref)
	})
	if len(kept) == 0 {
		kept = nil
	}
	task.DeadWaits = kept
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
		// The column's CHECK refuses an empty rank, and a committed record
		// is never refused, so a writer that minted none gets the origin.
		rank = RankOrigin
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_tasks
			(id, key, project_key, filed_unit, routing_unit,
			 parent_id, root_id, depth, type, title, body_version, status,
			 status_group, status_entered_at, priority, prio_rank, rank,
			 reporter, assignee, start_at, due_at, due_all_day, estimate_min,
			 points, spend_turns, spend_rounds, spend_input, spend_output,
			 spend_cache_read, spend_cache_write, spend_wall_ms, spend_tokens,
			 spend_workers, spend_sent_back, done_at, closed_at, finished_at, archived, archived_at, removed_at,
			 removed_with, merging, moving, reassignments, policy_stamp,
			 unblocked_told_at, inconsistent_project,
			 cycle, too_deep, key_collision, created_at, updated_at, version,
			 scoped_through, document)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,?,?,?,?,?,?,?,?,
		        ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,0,0,0,0,?,?,?,0,?)
		ON CONFLICT (id) DO UPDATE SET
			key = excluded.key, project_key = excluded.project_key,
			routing_unit = excluded.routing_unit,
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
			removed_with = excluded.removed_with,
			merging = excluded.merging, moving = excluded.moving,
			reassignments = excluded.reassignments,
			policy_stamp = excluded.policy_stamp,
			updated_at = excluded.updated_at, version = excluded.version,
			document = excluded.document
		WHERE excluded.version > tracker_tasks.version`,
		task.ID, task.Key, task.Project, task.FiledUnit, task.RoutingUnit,
		nullableStringPtr(task.Parent), rootOf(task),
		task.Depth, typeOf(task), task.Title, task.BodyVersion,
		string(statusOf(task)), string(statusOf(task).Group()),
		string(priorityOf(task)), priorityOf(task).Rank(), string(rank),
		task.Reporter, task.Assignee, nullableTime(task.StartAt),
		nullableTime(task.DueAt), boolInt(task.DueAllDay), task.EstimateMinutes,
		task.Points, task.Spend.Turns, task.Spend.Rounds, task.Spend.Input,
		task.Spend.Output, task.Spend.CacheRead, task.Spend.CacheWrite,
		task.Spend.WallMs, task.Spend.Tokens, task.Spend.Workers,
		task.Spend.SentBack, nullableTime(task.DoneAt),
		nullableTime(task.ClosedAt), nullableTime(task.FinishedAt()),
		boolInt(task.Archived), nullableTime(task.ArchivedAt),
		removedAt(task), removedWith(task),
		boolInt(task.Merging), boolInt(task.Moving), task.Reassignments,
		task.PolicyStamp,
		store.EncodeTime(task.CreatedAt), store.EncodeTime(task.UpdatedAt),
		c.packed, document)
	if err != nil {
		return 0, fmt.Errorf("worklet: write task %s at %s: %w", task.ID, c.position, err)
	}
	return affected(res)
}

// clearFieldValuesStatement is the delete of one task's value rows that
// [Applier.explodeTask] rewrites them after and a purge ends them with, named
// for the plan test.
const clearFieldValuesStatement = `DELETE FROM tracker_field_values WHERE task_id = ?`

// childClearStatement is the delete of one task's rows of one child table
// that [Applier.explodeTask] writes them again after, named for the plan test.
func childClearStatement(table string) string {
	return `DELETE FROM ` + table + ` WHERE task_id = ?`
}

// explodeTask rewrites the child rows a task's collections produce.
//
// Wholesale per collection, not diffed: a delete-then-insert is a pure
// function of the document, where a diff depends on what this node held. The
// key directory is the exception, written only when rekeyed
// ([Applier.maintainKeys]).
func (a *Applier) explodeTask(ctx context.Context, tx *sql.Tx,
	task Task, declared map[string]FieldDef, rekeyed bool, c applyContext) (int, error) {

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
			return insertMany(ctx, tx, c.maxVariables,
				`INSERT INTO tracker_watchers (task_id, handle, muted) VALUES`,
				`(?,?,?)`,
				`ON CONFLICT (task_id, handle) DO UPDATE SET
					muted = excluded.muted`,
				task.Watchers, func(handle string) []any {
					return []any{task.ID, handle, boolInt(muted[handle])}
				})
		}},
		{"tracker_collaborators", func() (int, error) {
			return insertMany(ctx, tx, c.maxVariables,
				`INSERT INTO tracker_collaborators (task_id, handle) VALUES`,
				`(?,?)`,
				`ON CONFLICT (task_id, handle) DO NOTHING`,
				task.Collaborators, func(h string) []any { return []any{task.ID, h} })
		}},
		{"tracker_task_tags", func() (int, error) {
			// Under the task's current project: the tag filter seeks slug
			// and project together ([tagClause]), so a moved task's row
			// must move with it.
			return insertMany(ctx, tx, c.maxVariables,
				`INSERT INTO tracker_task_tags (task_id, project_key, slug) VALUES`,
				`(?,?,?)`,
				`ON CONFLICT (task_id, slug) DO NOTHING`,
				task.Tags, func(s string) []any { return []any{task.ID, task.Project, s} })
		}},
		{"tracker_relations", func() (int, error) {
			// `one_sided` is derived here, never carried: it is a fact
			// about the other end's rows. `one_sided_final` stays on the
			// record: it is the repair duty's decision, which nothing
			// derives.
			//
			// The multi-row form is safe: each row's subquery reads
			// tracker_task_dependents, which this statement does not
			// write and which the next loop entry writes only after it.
			return insertMany(ctx, tx, c.maxVariables, relationsInsertPrefix,
				relationsInsertRow, relationsInsertConflict,
				task.Relations, func(r Relation) []any {
					// A CHANGE HAS NO MIRROR, so its edge is never one-sided.
					mirrored := string(r.Kind)
					if IsChangeRef(r.Other) {
						mirrored = ""
					}
					return []any{task.ID, r.Other, string(r.Kind),
						mirrored, r.Other, task.ID,
						boolInt(r.OneSidedFinal), r.Note,
						// The authored instant, never re-stamped on
						// conflict: the repair ages edges on it.
						store.EncodeTime(r.CreatedAt)}
				})
		}},
		{"tracker_task_dependents", func() (int, error) {
			// The mirror as a row, so the derivation above is an
			// indexed probe rather than a document decode per edge.
			return insertMany(ctx, tx, c.maxVariables, mirrorInsertPrefix,
				mirrorInsertRow, mirrorInsertConflict,
				task.Dependents, func(id string) []any {
					return []any{task.ID, id}
				})
		}},
	} {
		// Cleared first, so a collection the record emptied ends empty.
		if _, err := tx.ExecContext(ctx,
			childClearStatement(child.table), task.ID); err != nil {
			return 0, fmt.Errorf("worklet: clear %s for %s: %w",
				child.table, task.ID, err)
		}
		n, err := child.write()
		if err != nil {
			return 0, fmt.Errorf("worklet: write %s for %s: %w",
				child.table, task.ID, err)
		}
		written += n
	}

	// Field values are outside the loop because they need the declarations
	// (handed down, not re-read) to pick each value's typed column.
	if _, err := tx.ExecContext(ctx, clearFieldValuesStatement, task.ID); err != nil {
		return 0, fmt.Errorf("worklet: clear tracker_field_values for %s: %w",
			task.ID, err)
	}
	values, err := a.explodeFieldValues(ctx, tx, task, declared, c)
	if err != nil {
		return 0, err
	}
	written += values

	deps, err := a.maintainDeps(ctx, tx, task, c)
	if err != nil {
		return 0, err
	}
	refs, err := a.maintainReferences(ctx, tx, task)
	if err != nil {
		return 0, err
	}
	keys := 0
	if rekeyed {
		if keys, err = a.maintainKeys(ctx, tx, task, c); err != nil {
			return 0, err
		}
	}
	closure, err := a.maintainClosure(ctx, tx, task)
	if err != nil {
		return 0, err
	}
	return written + deps + refs + keys + closure, nil
}

// The statements [Applier.maintainDeps] writes a task's dependency rows with,
// named for the plan test: its own edges cleared by the task, rewritten one
// row per blocker — each row reading its blocker's state by the blocker's key —
// and every edge naming it as the blocker settled by the blocker.
const (
	clearDepsStatement = `DELETE FROM tracker_task_deps WHERE task_id = ?`
	depsInsertPrefix   = `
		INSERT INTO tracker_task_deps (blocker_id, task_id, blocker_open, cleared_at, dead_head)
		VALUES`
	depsInsertRow = `(?, ?,
		  COALESCE((SELECT CASE WHEN b.status_group IN ('done','closed')
		                        THEN 0 ELSE 1 END
		            FROM tracker_tasks b WHERE b.id = ?), ?),
		  COALESCE((SELECT b.finished_at FROM tracker_tasks b WHERE b.id = ?), ?), ?)`
	depsInsertConflict = `ON CONFLICT (blocker_id, task_id) DO UPDATE SET
			blocker_open = excluded.blocker_open,
			cleared_at = excluded.cleared_at,
			dead_head = excluded.dead_head`
	clearDependentsStatement = `
		UPDATE tracker_task_deps SET
			blocker_open = CASE WHEN ? IN ('done','closed') THEN 0 ELSE 1 END,
			cleared_at = ?
		WHERE blocker_id = ?`
)

// maintainDeps derives the dependency edges from the task's own relations.
//
// Derived rather than carried, so "blocked" and "cleared when" are a pure
// function of the rows present, whatever order the records arrived in.
func (a *Applier) maintainDeps(ctx context.Context, tx *sql.Tx, task Task,
	c applyContext) (int, error) {

	if _, err := tx.ExecContext(ctx, clearDepsStatement, task.ID); err != nil {
		return 0, fmt.Errorf("worklet: clear the dependencies of %s: %w", task.ID, err)
	}
	// Only `waiting_on` relations are dependencies.
	blockers := make([]Relation, 0, len(task.Relations))
	for _, relation := range task.Relations {
		if relation.Kind == RelationWaitingOn {
			blockers = append(blockers, relation)
		}
	}
	// A VALUES row of scalar subqueries, the template a multi-row insert
	// repeats. Its probes read tracker_tasks while the statement writes
	// tracker_task_deps, so batching cannot change what any of them sees.
	written, err := insertMany(ctx, tx, c.maxVariables, depsInsertPrefix,
		depsInsertRow, depsInsertConflict, blockers, func(r Relation) []any {
			// A change is no task: open until the task records it settled,
			// dead at the head the task records nothing moving it at.
			open, cleared := 1, any(nil)
			if s, settled := task.Changes[r.Other]; settled {
				open, cleared = 0, nullableTime(&s.At)
			}
			return []any{r.Other, task.ID, r.Other, open, r.Other, cleared, task.DeadWaits[r.Other].Head}
		})
	if err != nil {
		return 0, fmt.Errorf("worklet: write the dependencies of %s: %w",
			task.ID, err)
	}
	// The other direction: this task finishing clears every edge naming it
	// as blocker, from the row this transaction just wrote.
	res, err := tx.ExecContext(ctx, clearDependentsStatement,
		string(statusOf(task).Group()), nullableTime(task.FinishedAt()), task.ID)
	if err != nil {
		return 0, fmt.Errorf("worklet: clear the dependents of %s: %w", task.ID, err)
	}
	n, err := affected(res)
	if err != nil {
		return 0, err
	}
	written += n

	// And `one_sided` from the blocker's side: its `Dependents` just
	// changed, so every edge pointing at it is re-judged. Without this, a
	// mirror landing second never clears the flag the dependent's apply
	// set, and the duty repairs an already-whole edge for ever.
	res, err = tx.ExecContext(ctx, rejudgeEdgesStatement, task.ID, task.ID)
	if err != nil {
		return 0, fmt.Errorf("worklet: re-judge the edges waiting on %s: %w",
			task.ID, err)
	}
	n, err = affected(res)
	if err != nil {
		return 0, err
	}
	return written + n, nil
}

// taskKeyPattern is what a task's description is scanned for.
// Compiled at first use, not at init: Gitlet's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
var taskKeyPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`[A-Z][A-Z0-9]{1,9}-[0-9]+`)
})

// taskKeysIn is every distinct key a body names, IN DOCUMENT ORDER.
//
// Document order, not sorted, decides which keys survive the cap: the ones the
// author wrote first. One scanner serves the applier's rows and the write
// path's warning, so a body cannot be warned at one count and capped at
// another.
func taskKeysIn(body string) []string {
	found := taskKeyPattern().FindAllString(body, -1)
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
// The description only: although replicated 0002's header says comments are
// scanned, no comment apply writes a row, so from_comment is always empty and
// `references=` lists tasks whose DESCRIPTION mentions a key. Capped after
// dedupe, so a pathological body costs a bounded number of lookups per node.
func (a *Applier) maintainReferences(ctx context.Context, tx *sql.Tx, task Task) (int, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_references WHERE from_task = ? AND from_comment = ''`,
		task.ID); err != nil {
		return 0, fmt.Errorf("worklet: clear the references of %s: %w", task.ID, err)
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
			return 0, fmt.Errorf("worklet: write a reference from %s: %w", task.ID, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// maintainKeys keeps the key directory, current and former: one row per key,
// naming the task that claimed it. A re-key keeps the old key as a FORMER one,
// so old links resolve; a key another task holds is left alone, or every
// reference to it would re-point.
//
// The claim is first-writer-wins and records about different tasks reorder
// unless their scopes meet, so only records naming the keys they move
// ([keyTerm]) write the directory: a create, a re-key, a purge's hand-on. The
// broker cannot refuse two tasks holding one key (a restored counter
// re-mints), so the loser carries `key_collision`, derived from the directory
// by every record filed under the key, hence identical on every node.
func (a *Applier) maintainKeys(ctx context.Context, tx *sql.Tx, task Task,
	c applyContext) (int, error) {

	if task.Key == "" {
		return 0, nil
	}
	// Current and former keys as one collection, empties dropped here
	// because a row template repeats for every element. A key in both lists
	// is two agreeing rows: `current` comes from the entry, not its position.
	entries := make([]string, 0, 1+len(task.FormerKeys))
	for _, entry := range append([]string{task.Key}, task.FormerKeys...) {
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	claimed, err := insertMany(ctx, tx, c.maxVariables,
		`INSERT INTO tracker_task_keys (key, task_id, current) VALUES`,
		`(?,?,?)`,
		`ON CONFLICT (key) DO UPDATE SET current = excluded.current
		 WHERE tracker_task_keys.task_id = excluded.task_id`,
		entries, func(entry string) []any {
			return []any{entry, task.ID, boolInt(entry == task.Key)}
		})
	if err != nil {
		return 0, fmt.Errorf("worklet: claim the keys of %s: %w", task.ID, err)
	}
	flagged, err := flagKeyCollisions(ctx, tx, task.Key)
	if err != nil {
		return 0, err
	}
	return claimed + flagged, nil
}

// keyCollisionStatement is [flagKeyCollisions]' write, named for the plan
// test: the holders of one key, found by the key.
const keyCollisionStatement = `
		UPDATE tracker_tasks
		SET key_collision = NOT EXISTS (
			SELECT 1 FROM tracker_task_keys k
			WHERE k.key = tracker_tasks.key AND k.task_id = tracker_tasks.id)
		WHERE key = ? AND key_collision = EXISTS (
			SELECT 1 FROM tracker_task_keys k
			WHERE k.key = tracker_tasks.key AND k.task_id = tracker_tasks.id)`

// flagKeyCollisions re-derives `key_collision` for every task holding a key:
// set on each one the directory does not name, clear on the one it does.
//
// Only rows whose flag disagrees are written, so the ordinary case writes none.
func flagKeyCollisions(ctx context.Context, tx *sql.Tx, key string) (int, error) {
	res, err := tx.ExecContext(ctx, keyCollisionStatement, key)
	if err != nil {
		return 0, fmt.Errorf("worklet: flag the holders of key %s: %w", key, err)
	}
	return affected(res)
}

// maintainProjectCounts moves the maintained census.
//
// Maintained, never scanned: a per-poll aggregate is half a million index
// entries at year five. A pure function of the applied records, so the
// identity assertion can recompute and compare.
//
// Two partitions: the disjoint buckets (open, done, closed) cover every task
// not removed, and `active_count` is the open bucket's `active` group (`todo`
// is the remainder), so todo → in progress moves only active.
func (a *Applier) maintainProjectCounts(ctx context.Context, tx *sql.Tx,
	current, next Task, held bool) (int, error) {

	was, is := "", bucketOf(next)
	wasActive, isActive := false, activeOf(next)
	if held {
		was, wasActive = bucketOf(current), activeOf(current)
	}
	// Counts are per project, so a cross-project move moves them too.
	if was == is && wasActive == isActive && current.Project == next.Project {
		return 0, nil
	}
	moved := current.Project != next.Project
	written := 0
	if was != is || moved {
		if was != "" {
			n, err := moveProjectCount(ctx, tx, was, current.Project, -1)
			if err != nil {
				return 0, err
			}
			written += n
		}
		if is != "" {
			n, err := moveProjectCount(ctx, tx, is, next.Project, +1)
			if err != nil {
				return 0, err
			}
			written += n
		}
	}
	if wasActive != isActive || (moved && isActive) {
		if wasActive {
			n, err := moveProjectCount(ctx, tx, censusActive, current.Project, -1)
			if err != nil {
				return 0, err
			}
			written += n
		}
		if isActive {
			n, err := moveProjectCount(ctx, tx, censusActive, next.Project, +1)
			if err != nil {
				return 0, err
			}
			written += n
		}
	}
	return written, nil
}

// censusActive is the column name `active_count` is moved under — see
// [moveProjectCount].
const censusActive = "active"

// moveProjectCount moves one of the four maintained counters by one.
//
// Interpolating bucket into the column name is safe only because it is always
// [bucketOf]'s closed answer or [censusActive], never a value off the wire.
// The decrement clamps at zero: a negative census means nothing to anyone.
func moveProjectCount(ctx context.Context, tx *sql.Tx, bucket, project string,
	by int) (int, error) {

	set := fmt.Sprintf(`%s_count = %s_count + 1`, bucket, bucket)
	verb := "raise"
	if by < 0 {
		set = fmt.Sprintf(`%s_count = MAX(%s_count - 1, 0)`, bucket, bucket)
		verb = "lower"
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE tracker_projects SET `+set+` WHERE key = ?`, project)
	if err != nil {
		return 0, fmt.Errorf("worklet: %s the %s count of %s: %w",
			verb, bucket, project, err)
	}
	return affected(res)
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

// activeOf is whether a task counts in `active_count`: not removed, and in the
// `active` status group. The one statement of the rule; [rederiveActiveCounts]
// reads the columns this apply writes from the same two facts.
func activeOf(task Task) bool {
	return task.Removed == nil && statusOf(task).Group() == GroupActive
}

// purgeTask is the one operation that removes rows, everywhere the task is
// named, in one transaction; its marker stops a later redelivery resurrecting
// any of it. Inbound references go with the key rows that resolved them, or
// they would be dangling links indistinguishable from typos. Direct children
// move onto the purged task's own parent (destroying loses work nobody named;
// orphaning leaves pointers to nothing); depth only falls, so no cap breaks.
// Another task's rows re-derive from its document, so each one the scope names
// ([purgeReach]; package doc, "What a purge writes beside its own task") is
// rewritten once ([Applier.rewriteNaming]), stamped `scoped_through`, never
// `version` ([Task]). Its records' content goes too ([forgetRecords]). Done
// here, not refused at the writer: a child's create does not contend with this
// subject, so it can land after a childless decide.
func (a *Applier) purgeTask(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	id := c.subject().ID
	// Read before the deletes: the marker records the task's key and project.
	task, held, err := readTask(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	// And its children, for the same reason.
	children, err := childrenOf(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	written := 0
	// The census comes down with the row: a purge writes no record through
	// [Applier.maintainProjectCounts], and a maintained count never
	// re-aggregates. Gated on held, because the deletion gate lets this
	// record's own redelivery through, and a second decrement would go one
	// below the truth, which [moveProjectCount]'s clamp cannot see.
	if bucket := bucketOf(task); held && bucket != "" {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		n, err := moveProjectCount(ctx, tx, bucket, task.Project, -1)
		if err != nil {
			return 0, err
		}
		written += n
	}
	// And its share of the active count, on the same gate.
	if held && activeOf(task) {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		n, err := moveProjectCount(ctx, tx, censusActive, task.Project, -1)
		if err != nil {
			return 0, err
		}
		written += n
	}
	// Which other tasks name it, read while the rows that say so remain.
	others, err := othersNaming(ctx, tx, id, children)
	if err != nil {
		return 0, err
	}
	for _, statement := range purgeDeletes(id) {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := tx.ExecContext(ctx, statement.sql, statement.args...)
		if err != nil {
			return 0, fmt.Errorf("worklet: purge %s at %s: %w", id, c.position, err)
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
		return 0, fmt.Errorf("worklet: mark %s purged at %s: %w", id, c.position, err)
	}
	marker, err := affected(res)
	if err != nil {
		return 0, err
	}
	moved, err := a.rewriteNaming(ctx, tx, id, task.Parent, children, others, c)
	if err != nil {
		return 0, err
	}
	// Every key it claimed, former ones included: a task minted onto a
	// moved task's alias holds it only as a collision until the claimant
	// goes. Each is in the record's scope ([keyTerm]).
	handed := 0
	for _, key := range append([]string{task.Key}, task.FormerKeys...) {
		n, handErr := handOnKey(ctx, tx, key)
		if handErr != nil {
			return 0, handErr
		}
		handed += n
	}
	// No deltas: they would be the content the purge exists to destroy.
	history, err := a.writeHistory(ctx, tx, c,
		subjectKeys{Project: task.Project, Key: task.Key}, nil)
	if err != nil {
		return 0, err
	}
	// What its own records wrote ([Applier.forgetRecords]).
	forgotten, err := a.forgetRecords(ctx, tx, c, id, historyID(c))
	if err != nil {
		return 0, fmt.Errorf("worklet: purge %s at %s: %w", id, c.position, err)
	}
	return written + marker + moved + handed + history + forgotten, nil
}

// handOnKeyStatement is [handOnKey]'s write, named for the plan test: the
// key's current holders found by the key, the lowest id taking it.
const handOnKeyStatement = `
		INSERT INTO tracker_task_keys (key, task_id)
		SELECT key, id FROM tracker_tasks WHERE key = ? ORDER BY id LIMIT 1
		ON CONFLICT (key) DO NOTHING`

// handOnKey gives a purged task's key to whoever else holds it.
//
// Otherwise the key stays unclaimed and the collider flagged for ever, since
// only a re-key writes the directory ([Applier.maintainKeys]). The lowest id
// holding it as its CURRENT key takes it, a choice every node makes alike; a
// former-key holder never held it. The holders are the same on every node
// because every record changing them names the key ([keyTerm]).
func handOnKey(ctx context.Context, tx *sql.Tx, key string) (int, error) {
	if key == "" {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, handOnKeyStatement, key)
	if err != nil {
		return 0, fmt.Errorf("worklet: hand on key %s: %w", key, err)
	}
	claimed, err := affected(res)
	if err != nil {
		return 0, err
	}
	flagged, err := flagKeyCollisions(ctx, tx, key)
	if err != nil {
		return 0, err
	}
	return claimed + flagged, nil
}

// purgeStatement is one DELETE a purge issues, with its arguments.
type purgeStatement struct {
	sql  string
	args []any
}

// purgeDeletes is every DELETE a purge of id issues against the object tables,
// in the order it issues them.
//
// The dependency mirror goes too, both halves: a row keyed on an id nothing
// resolves is a copy of the task's edges that nothing else removes.
func purgeDeletes(id string) []purgeStatement {
	return []purgeStatement{
		{`DELETE FROM tracker_references WHERE from_task = ? OR to_task = ?`, []any{id, id}},
		{`DELETE FROM tracker_task_keys WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_watchers WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_collaborators WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_task_tags WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_relations WHERE task_id = ? OR other_id = ?`, []any{id, id}},
		{`DELETE FROM tracker_task_deps WHERE task_id = ? OR blocker_id = ?`, []any{id, id}},
		{`DELETE FROM tracker_task_dependents WHERE task_id = ? OR dependent_id = ?`, []any{id, id}},
		{`DELETE FROM tracker_checklist_items WHERE task_id = ?`, []any{id}},
		{clearFieldValuesStatement, []any{id}},
		{`DELETE FROM tracker_task_closure WHERE ancestor_id = ? OR descendant_id = ?`, []any{id, id}},
		{`DELETE FROM tracker_body_revisions WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_comments WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_tasks WHERE id = ?`, []any{id}},
	}
}

// The statements a purge runs over the task's history, named for the plan
// test: the notices its rows routed, which [forgottenNotices] reads first by
// the same predicate, both finding the rows by the subject; and the skeleton
// each row the census walks keeps, written to that row by its own id.
const (
	forgottenNoticesStatement = `
		SELECT record_id, recipient, reason FROM tracker_notifications
		WHERE record_id IN (
			SELECT id FROM tracker_history
			WHERE subject_id = ? AND subject_kind = ? AND id <> ?)
		ORDER BY record_id, recipient`
	forgetNoticesStatement = `
		DELETE FROM tracker_notifications WHERE record_id IN (
			SELECT id FROM tracker_history
			WHERE subject_id = ? AND subject_kind = ? AND id <> ?)`
	forgetSkeletonStatement = `
		UPDATE tracker_history
		SET excerpt = '', comment_id = '', document = ?, fields_json = ?
		WHERE id = ?`
)

// The purge's two statements over the census's own rows, found by the subject:
// the rows it does not walk, which go, and the walk's own read
// ([censusRows]). Variables rather than constants because both are spelled
// with [historyMoves], the predicate the flow walk reads, so a purge keeps
// exactly the rows the census counts.
var (
	forgetHistoryStatement = `
		DELETE FROM tracker_history
		WHERE subject_id = ? AND subject_kind = ? AND id <> ?
		  AND NOT (` + historyMoves("") + `)`
	censusRowsStatement = `
		SELECT id, fields_json FROM tracker_history
		WHERE subject_id = ? AND subject_kind = ? AND id <> ?
		  AND ` + historyMoves("") + `
		ORDER BY id`
)

// forgetRecords destroys the content a purged task's own records wrote: its
// inbox notices and turn records go, and of its history only a SKELETON stays.
// The purge's own row (keep) and its notices stay whole: the lead's `purged`
// wake reads them.
//
// The skeleton is every row [historyMoves] selects, the same SQL predicate the
// flow census ([Reader.Flow]) walks, keeping only kind, instant and the status,
// assignee and project deltas, so the census still counts the task before its
// purge. Run after the purge's history row, so the effective instant stays a
// MAX over every row. It notes each emptied inbox ([inbox.Movement]), read
// before the rows go, so an open screen drops the notices at once.
func (a *Applier) forgetRecords(ctx context.Context, tx *sql.Tx, c applyContext,
	id, keep string) (int, error) {

	kind := string(KindTask)
	written := 0
	exec := func(query string, args ...any) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err := affected(res)
		if err != nil {
			return err
		}
		written += n
		return nil
	}
	// Notices first, found by their record key through the history rows (a
	// subject match would scan every inbox); the skeleton's too, since an
	// excerpt is content.
	gone, err := forgottenNotices(ctx, tx, id, keep)
	if err != nil {
		return 0, err
	}
	for _, notice := range gone {
		a.inboxed.Note(inbox.Note{
			Record: notice.record, Handle: notice.recipient, Subject: id,
			Reason: string(notice.reason), Position: c.packed, At: c.brokerAt,
			Removed: true,
		})
	}
	if err = exec(forgetNoticesStatement, id, kind, keep); err != nil {
		return 0, err
	}
	skeleton, err := censusRows(ctx, tx, id, keep)
	if err != nil {
		return 0, err
	}
	if err := exec(forgetHistoryStatement, id, kind, keep); err != nil {
		return 0, err
	}
	for _, row := range skeleton {
		if err := exec(forgetSkeletonStatement, []byte("{}"), row.fields, row.id); err != nil {
			return 0, err
		}
	}
	if err := exec(`DELETE FROM tracker_turns WHERE task_id = ?`, id); err != nil {
		return 0, err
	}
	return written, nil
}

// forgottenNotice is one inbox notice a purge is about to delete: whose it
// was, under which record, and the reason it reached them.
type forgottenNotice struct {
	record, recipient string
	reason            Reason
}

// forgottenNotices reads the notices [Applier.forgetRecords] deletes, by the
// same predicate it deletes them with, ordered so every node notes them alike.
func forgottenNotices(ctx context.Context, tx *sql.Tx, id, keep string) ([]forgottenNotice, error) {
	rows, err := tx.QueryContext(ctx, forgottenNoticesStatement, id, string(KindTask), keep)
	if err != nil {
		return nil, fmt.Errorf("worklet: read whose inbox a purge of %s empties: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []forgottenNotice
	for rows.Next() {
		var notice forgottenNotice
		var reason string
		if err := rows.Scan(&notice.record, &notice.recipient, &reason); err != nil {
			return nil, err
		}
		notice.reason = Reason(reason)
		out = append(out, notice)
	}
	return out, rows.Err()
}

// censusFields are the deltas a purged task's skeleton keeps: the three the
// flow census and the feed's change kinds read ([historyMoves]).
var censusFields = []string{"status", "assignee", "project"}

// skeletonRow is one history row a purge keeps, and the fields it keeps.
type skeletonRow struct {
	id     string
	fields string
}

// censusRows reads the purged task's history rows the census walks, each with
// its `fields_json` cut to [censusFields]. Ordered by id, so every node issues
// the same updates in the same order.
func censusRows(ctx context.Context, tx *sql.Tx, id, keep string) ([]skeletonRow, error) {
	rows, err := tx.QueryContext(ctx, censusRowsStatement, id, string(KindTask), keep)
	if err != nil {
		return nil, fmt.Errorf("worklet: read what the census keeps of a purged task: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []skeletonRow
	for rows.Next() {
		var row skeletonRow
		var raw string
		if err := rows.Scan(&row.id, &raw); err != nil {
			return nil, err
		}
		var all map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &all); err != nil {
			return nil, fmt.Errorf("worklet: history row %s carries fields that are "+
				"not an object: %w", row.id, err)
		}
		kept := map[string]json.RawMessage{}
		for _, name := range censusFields {
			if delta, ok := all[name]; ok {
				kept[name] = delta
			}
		}
		// encoding/json sorts map keys, so every node writes the same bytes.
		encoded, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		row.fields = string(encoded)
		out = append(out, row)
	}
	return out, rows.Err()
}

// otherEdit is everything a rewritten purge changes in ONE other task's
// document.
//
// Gathered per task, because several effects can reach one task (a child that
// is also a blocker) and [rewriteOther] writes a task at most once per record:
// a second rewrite reads as the record's redelivery and is dropped.
type otherEdit struct {
	// unrelate drops every relation the task holds to the purged task —
	// its `waiting_on` edge when it is a dependent.
	unrelate bool

	// unmirror drops the purged task from the task's [Task.Dependents].
	unmirror bool

	// reparent moves the task, a direct child, onto the purged task's own
	// parent.
	reparent bool
}

// othersNaming reads which OTHER tasks a rewritten purge of id rewrites, and
// what it changes in each: the tasks relating to it, the blockers listing it
// among their dependents, and its direct children.
//
// Read before the deletes remove the rows that say so.
func othersNaming(ctx context.Context, tx *sql.Tx, id string,
	children []string) (map[string]otherEdit, error) {

	relating, err := idsOf(ctx, tx, relatingStatement, id, id)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the tasks relating to %s: %w", id, err)
	}
	mirroring, err := idsOf(ctx, tx, mirroringStatement, id, id)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the blockers listing %s: %w", id, err)
	}
	edits := map[string]otherEdit{}
	for _, other := range relating {
		edit := edits[other]
		edit.unrelate = true
		edits[other] = edit
	}
	for _, other := range mirroring {
		edit := edits[other]
		edit.unmirror = true
		edits[other] = edit
	}
	for _, child := range children {
		edit := edits[child]
		edit.reparent = true
		edits[child] = edit
	}
	return edits, nil
}

// rewriteNaming is a rewritten purge's effect on every other task: each one's
// document rewritten ONCE with everything the purge changes in it
// ([otherEdit]), and then each re-parented child's ancestry rebuilt.
//
// In id order, so every node issues the same statements. After the deletes,
// and closures only after every document: [Applier.maintainClosure] walks the
// parent chain, which must no longer pass through the purged row.
func (a *Applier) rewriteNaming(ctx context.Context, tx *sql.Tx, id string,
	parent *string, children []string, edits map[string]otherEdit,
	c applyContext) (int, error) {

	written := 0
	for _, other := range slices.Sorted(maps.Keys(edits)) {
		edit := edits[other]
		n, err := rewriteOther(ctx, tx, other, c, movedColumns{parent: edit.reparent},
			func(task *Task) bool {
				changed := false
				if edit.unrelate {
					kept := slices.DeleteFunc(slices.Clone(task.Relations),
						func(r Relation) bool { return r.Other == id })
					changed = changed || len(kept) != len(task.Relations)
					task.Relations = kept
				}
				if edit.unmirror {
					kept := slices.DeleteFunc(slices.Clone(task.Dependents),
						func(d string) bool { return d == id })
					changed = changed || len(kept) != len(task.Dependents)
					task.Dependents = kept
				}
				if edit.reparent {
					if parent == nil {
						task.Parent = nil
					} else {
						moved := *parent
						task.Parent = &moved
					}
					changed = true
				}
				return changed
			})
		if err != nil {
			return 0, fmt.Errorf("worklet: take %s out of task %s: %w", id, other, err)
		}
		written += n
	}
	for _, child := range children {
		n, err := a.maintainClosure(ctx, tx, Task{ID: child, Parent: parent})
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// movedColumns are the row columns a rewrite of ANOTHER task moves beside its
// document.
//
// Only those (a placement's rank, a re-parent's pointer), so a rewrite never
// overwrites a column another record authored.
type movedColumns struct {
	parent bool
	rank   bool
}

// rewriteOtherStatement is [rewriteOther]'s write, named for the plan test:
// the document and the scope stamp, the columns moved names beside them, and
// the task found by its key. Its arguments are the document, the stamp, then
// each moved column in this order, then the id and the stamp again.
func rewriteOtherStatement(moved movedColumns) string {
	set := `document = ?, scoped_through = ?`
	if moved.parent {
		set += `, parent_id = ?`
	}
	if moved.rank {
		set += `, rank = ?`
	}
	return `UPDATE tracker_tasks SET ` + set + `
		WHERE id = ? AND ? > MAX(version, scoped_through)`
}

// rewriteOther applies change to ANOTHER task's document, from a record on
// some other subject, and reports the rows written.
//
// Stamps `scoped_through`, never `version` ([Task]): the version is the task's
// own subject's arbitration anchor. A row at or past this record is its
// redelivery and is left alone, so a second rewrite from one record writes
// nothing ([otherEdit]). The moved columns are written from the document,
// which every later record derives them from.
func rewriteOther(ctx context.Context, tx *sql.Tx, id string, c applyContext,
	moved movedColumns, change func(*Task) bool) (int, error) {

	task, held, err := readTask(ctx, tx, id)
	if err != nil || !held {
		return 0, err
	}
	if !change(&task) {
		return 0, nil
	}
	task.ScopedThrough = uint64(c.packed)
	document, err := json.Marshal(task)
	if err != nil {
		return 0, fmt.Errorf("worklet: encode task %s at %s: %w", id, c.position, err)
	}
	args := []any{document, c.packed}
	if moved.parent {
		args = append(args, nullableStringPtr(task.Parent))
	}
	if moved.rank {
		args = append(args, string(task.Rank))
	}
	res, err := tx.ExecContext(ctx, rewriteOtherStatement(moved),
		append(args, id, c.packed)...)
	if err != nil {
		return 0, fmt.Errorf("worklet: rewrite task %s at %s: %w", id, c.position, err)
	}
	return affected(res)
}

// idsOf reads one column of ids.
func idsOf(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// childrenStatement is [childrenOf]'s read, named for the plan test: a seek on
// the parent in the index of the tree (replicated 0058).
const childrenStatement = `SELECT id FROM tracker_tasks WHERE parent_id = ? ORDER BY id`

// childrenOf reads a task's DIRECT children.
//
// From parent pointers, not the closure: only direct children's `parent_id`
// would dangle.
func childrenOf(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, childrenStatement, id)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the children of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var child string
		if err := rows.Scan(&child); err != nil {
			return nil, fmt.Errorf("worklet: scan a child of %s: %w", id, err)
		}
		out = append(out, child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: read the children of %s: %w", id, err)
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
// The task's own id: the closure rebuild in this transaction writes the real
// root, and a second derivation here would disagree on an out-of-order
// re-parent.
func rootOf(task Task) string { return task.ID }

func removedAt(task Task) any {
	if task.Removed == nil {
		return nil
	}
	return store.EncodeTime(task.Removed.At)
}

// removedWith binds the cascade root into the task's own column, or NULL.
//
// Derived from [removedWithText], the delta's form, so the column and the
// history row cannot name different roots.
func removedWith(task Task) any {
	with := removedWithText(task.Removed)
	if with == "" {
		return nil
	}
	return with
}

func nullableStringPtr(v *string) any {
	if v == nil || *v == "" {
		return nil
	}
	return *v
}

// insertMany writes a whole collection as MULTI-ROW INSERTS, chunked to the
// estate's parameter limit.
//
// Collections reach hundreds of rows ([MaxTagsPerProject]), and a statement
// per row is the slow shape BenchmarkLogApplyDrain calls "unprepared". The
// builder is [store.InsertRows]; this adds a typed slice and a per-item
// argument builder. The statement comes in three parts (prefix through VALUES,
// one row template, the ON CONFLICT suffix) because the templates hold CASEs,
// subqueries and literals that no split on "VALUES" survives. maxVariables is
// [applyContext.maxVariables]; zero degrades to a row per statement.
func insertMany[T any](ctx context.Context, tx *sql.Tx, maxVariables int,
	prefix, row, suffix string, items []T, args func(T) []any) (int, error) {

	return store.InsertRows(ctx, tx, maxVariables, prefix, row, suffix,
		len(items), func(i int) []any { return args(items[i]) })
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
			return 0, fmt.Errorf("worklet: clear the tags of %s: %w", subject.ID, err)
		}
		return insertMany(ctx, tx, c.maxVariables, `
			INSERT INTO tracker_tags
				(project_key, slug, label, label_norm, color, description,
				 archived, tags_version)
			VALUES`,
			`(?,?,?,?,?,?,?,?)`,
			`ON CONFLICT (project_key, slug) DO UPDATE SET
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
	case KindView:
		return a.settleDefaultView(ctx, tx, subject.ID, c)
	}
	return 0, nil
}

// settleDefaultView makes "one per container can be the default" TRUE.
//
// The applier's, not the writer's: a companion append would leave a window
// with two defaults. In one transaction with the view's row, the container
// ends with exactly one or the record did not apply. The fan-out is unbounded,
// so the scope names the CONTAINER ([MaxScopeTerms]). A non-default view
// clears nothing: setting another default is a gesture nobody made.
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
		return 0, fmt.Errorf("worklet: clear the other defaults in %s %s: %w",
			view.Container.Kind, view.Container.ID, err)
	}
	return affected(res)
}

// explodeCatalogue settles what a catalogue apply changes about the ROWS.
//
// Declarations explode into no rows: nothing seeks one, and every reader
// decodes the one document (tens of fields, wanted whole). What a field
// catalogue changes is the visibility of the per-task VALUES every `f.<ref>`
// filter seeks.
func (a *Applier) explodeCatalogue(ctx context.Context, tx *sql.Tx, name string,
	c applyContext) (int, error) {

	if name == CatalogueTypes {
		// A type catalogue touches no row: a task's type column moves only
		// with the task's own commit.
		return 0, nil
	}
	var catalogue FieldCatalogue
	if err := decodePayload(c.record.Mutation, &catalogue); err != nil {
		return 0, err
	}
	// Value rows' `hidden` follows the declaration in this transaction;
	// nothing else would update a task untouched since the change.
	return a.settleFieldVisibility(ctx, tx, catalogue.Fields)
}

// settleFieldVisibility makes every value row agree with its declaration.
//
// A value is hidden when its field is archived or its `AppliesTo` excludes the
// task's type. Both directions: a value written under a stale declaration, or
// a type added back, must become visible. The type comparison is case-folded
// as [appliesTo]'s is, or visibility would depend on which write came last.
// Foreign rows are untouched: they are hidden by kind, and a declaration
// re-explodes one as native on the task's next write.
func (a *Applier) settleFieldVisibility(ctx context.Context, tx *sql.Tx,
	fields []FieldDef) (int, error) {

	written := 0
	for _, field := range fields {
		for _, write := range settleStatements(field) {
			res, err := tx.ExecContext(ctx, write.sql, write.args...)
			if err != nil {
				return 0, fmt.Errorf("worklet: settle %s's value rows against its "+
					"declaration: %w", field.Slug, err)
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

// The index of the value rows a declaration hides (replicated 0059): the size
// of what is hidden, which a healthy company keeps small.
const hiddenFieldValuesIndex = "tracker_field_values_hidden_idx"

// settleWrite is one statement [Applier.settleFieldVisibility] runs, with its
// arguments.
type settleWrite struct {
	sql  string
	args []any
}

// settleStatements is what makes one field's value rows agree with its
// declaration: the visible rows that should be hidden, then the hidden rows
// that should be visible — each one only where the declaration can produce
// such a row, and each named for the plan test.
//
// Two writes, one per side of `hidden`, because `hidden <> x` no index serves
// and this runs per field on every catalogue apply. The visible side uses the
// field type's value index (partial on `hidden = 0`), the hidden side
// [hiddenFieldValuesIndex]; INDEXED BY because the planner otherwise may skip
// a partial index, and it refuses (`no query solution`) a write that stops
// stating its side. A field with nothing to move costs an empty seek; only one
// restricting its types reads every live value.
func settleStatements(field FieldDef) []settleWrite {
	visible := "UPDATE tracker_field_values INDEXED BY " + fieldValueIndex(field.Type) +
		" SET hidden = 1 WHERE field_id = ? AND kind = ? AND hidden = 0"
	hidden := "UPDATE tracker_field_values INDEXED BY " + hiddenFieldValuesIndex +
		" SET hidden = 0 WHERE field_id = ? AND kind = ? AND hidden = 1"
	bound := []any{field.ID, FieldValueNative}
	switch {
	case field.Archived:
		// An archived field hides every value.
		return []settleWrite{{visible, bound}}
	case len(field.AppliesTo) == 0:
		// An unrestricted one hides none.
		return []settleWrite{{hidden, bound}}
	}
	types := make([]any, 0, len(field.AppliesTo))
	for _, name := range field.AppliesTo {
		types = append(types, strings.ToLower(name))
	}
	// The COALESCE hides a row whose task is gone (the column is NOT NULL).
	excluded := ` AND COALESCE((SELECT CASE WHEN LOWER(t.type) IN (` +
		placeholders(len(types)) + `) THEN 0 ELSE 1 END ` +
		`FROM tracker_tasks t ` +
		`WHERE t.id = tracker_field_values.task_id), 1) = `
	args := append(append([]any{}, bound...), types...)
	return []settleWrite{
		{visible + excluded + "1", args},
		{hidden + excluded + "0", args},
	}
}

// writeThread writes the rows a record's own payload carries, as against the
// collections the DOCUMENT carries.
//
// Not in [explodeTask], which rebuilds collections ON the document: a comment
// and a body revision are append-only rows one record produced, absent from
// the document. Checklists are a document collection, here only because the
// same guard covers all three: a redelivery must not write a second copy.
func (a *Applier) writeThread(ctx context.Context, tx *sql.Tx, task Task,
	c applyContext) (int, error) {

	written := 0
	items, err := writeChecklistItems(ctx, tx, task, c.maxVariables)
	if err != nil {
		return 0, err
	}
	written += items

	if c.record.Op == OpCreate {
		// A create carries at most the question it was filed as
		// ([TaskCreate]), and no body revision: there is no earlier body.
		var created TaskCreate
		if err := decodePayload(c.record.Mutation, &created); err != nil {
			return 0, fmt.Errorf("worklet: decode the create at %s: %w", c.position, err)
		}
		if created.Comment != nil {
			n, err := writeComment(ctx, tx, task, *created.Comment, c)
			if err != nil {
				return 0, err
			}
			written += n
		}
		return written, nil
	}
	if c.record.Op != OpPatch {
		// Only a patch or a create carries a thread.
		return written, nil
	}
	var patch TaskPatch
	if err := decodePayload(c.record.Mutation, &patch); err != nil {
		return 0, fmt.Errorf("worklet: decode the patch at %s: %w", c.position, err)
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

// writeComment writes what one record says about one comment: a POST creates
// its row, an EDIT rewrites its words and a RESOLVE marks it resolved.
//
// Each kind writes only what it owns, so none can reach a column another
// record wrote: a post inserts the row; an edit owns the words; a resolve the
// resolution; a removal the taking down ([removeComment]), never taken as a
// post, which would invent the remark on a node not holding it. None owns
// `answered_by`: the answering remark's post stamps it, and an edit writing
// it would reopen an answered question. `record_id` is the posting record's
// history row ([historyID]), placing the remark for [rederiveAnswers].
func writeComment(ctx context.Context, tx *sql.Tx, task Task, comment Comment,
	c applyContext) (int, error) {

	if comment.ID == "" {
		// Dropped, not given an id: one minted here would differ per node.
		return 0, nil
	}
	switch c.record.Kind {
	case ChangeCommentEdited:
		return editComment(ctx, tx, task, comment, c)
	case ChangeCommentResolved:
		return resolveComment(ctx, tx, task, comment, c)
	case ChangeCommentRemoved:
		return removeComment(ctx, tx, task, comment, c)
	}
	comment.Task = task.ID
	document, err := json.Marshal(comment)
	if err != nil {
		return 0, fmt.Errorf("worklet: encode comment %s at %s: %w",
			comment.ID, c.position, err)
	}
	// A row already under this id is left alone: its edits and answer since
	// are later records' to have written.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_comments
			(id, task_id, author, author_kind, body, reply_to, ask, answers,
			 record_id, created_at, updated_at, document)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO NOTHING`,
		comment.ID, task.ID, comment.Author, string(comment.AuthorKind),
		comment.Body, comment.ReplyTo, comment.Ask, comment.Answers,
		historyID(c), store.EncodeTime(comment.CreatedAt),
		store.EncodeTime(commentUpdatedAt(comment)), document)
	if err != nil {
		return 0, fmt.Errorf("worklet: write comment %s at %s: %w",
			comment.ID, c.position, err)
	}
	written, err := affected(res)
	if err != nil {
		return 0, err
	}
	if written == 0 {
		// Nothing posted, so nothing answered: the record that did post it
		// closed the ask, or did not.
		return 0, nil
	}
	// An answer closes the ask it names, a separate row.
	if comment.Answers != nil && *comment.Answers != "" {
		res, err := tx.ExecContext(ctx, `
			UPDATE tracker_comments SET answered_by = ?
			WHERE id = ? AND task_id = ? AND answered_by IS NULL`,
			comment.ID, *comment.Answers, task.ID)
		if err != nil {
			return 0, fmt.Errorf("worklet: close the ask %s answers at %s: %w",
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

// editComment rewrites a stored comment's words: its body, the instant they
// were changed and the document carrying both.
//
// The stored comment with its words replaced, not the record's comment written
// over it, so everything else stays what its post made it whatever the record
// carries ([Writer.EditComment] forms it the same way), and the columns and
// document stay one account. A row this node does not hold writes nothing: the
// post is below in the log, so it is a remark nobody posted.
func editComment(ctx context.Context, tx *sql.Tx, task Task, comment Comment,
	c applyContext) (int, error) {

	var stored []byte
	err := tx.QueryRowContext(ctx, `
		SELECT document FROM tracker_comments WHERE id = ? AND task_id = ?`,
		comment.ID, task.ID).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("worklet: read comment %s to edit at %s: %w",
			comment.ID, c.position, err)
	}
	var edited Comment
	if err = json.Unmarshal(stored, &edited); err != nil {
		return 0, fmt.Errorf("worklet: decode comment %s to edit at %s: %w",
			comment.ID, c.position, err)
	}
	edited.Body = comment.Body
	if !comment.UpdatedAt.IsZero() {
		edited.UpdatedAt = comment.UpdatedAt
	}
	document, err := json.Marshal(edited)
	if err != nil {
		return 0, fmt.Errorf("worklet: encode comment %s at %s: %w",
			comment.ID, c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE tracker_comments SET body = ?, updated_at = ?, document = ?
		WHERE id = ? AND task_id = ?`,
		edited.Body, store.EncodeTime(commentUpdatedAt(edited)), document,
		comment.ID, task.ID)
	if err != nil {
		return 0, fmt.Errorf("worklet: edit comment %s at %s: %w",
			comment.ID, c.position, err)
	}
	return affected(res)
}

// resolveComment marks a stored comment resolved: who resolved it and when,
// in its columns and in the document carrying them.
//
// The stored comment with its resolution set, for [editComment]'s reason: a
// question resolved after an answer stays answered. The kind is the gesture:
// it resolves whatever flag the record's comment carries, and a repeat is
// written again, so the row holds the newest resolution. A row this node does
// not hold writes nothing, for an edit's reason.
func resolveComment(ctx context.Context, tx *sql.Tx, task Task, comment Comment,
	c applyContext) (int, error) {

	var stored []byte
	err := tx.QueryRowContext(ctx, `
		SELECT document FROM tracker_comments WHERE id = ? AND task_id = ?`,
		comment.ID, task.ID).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("worklet: read comment %s to resolve at %s: %w",
			comment.ID, c.position, err)
	}
	var resolved Comment
	if err = json.Unmarshal(stored, &resolved); err != nil {
		return 0, fmt.Errorf("worklet: decode comment %s to resolve at %s: %w",
			comment.ID, c.position, err)
	}
	resolved.Resolved = true
	resolved.ResolvedBy, resolved.ResolvedAt = comment.ResolvedBy, comment.ResolvedAt
	// A later resolve carrying no withdrawal keeps the stored one.
	if comment.Withdrawal != nil {
		resolved.Withdrawal = comment.Withdrawal
	}
	document, err := json.Marshal(resolved)
	if err != nil {
		return 0, fmt.Errorf("worklet: encode comment %s at %s: %w",
			comment.ID, c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE tracker_comments
		SET resolved = 1, resolved_by = ?, resolved_at = ?, document = ?
		WHERE id = ? AND task_id = ?`,
		sql.NullString{String: resolved.ResolvedBy, Valid: resolved.ResolvedBy != ""},
		nullableTime(resolved.ResolvedAt), document, comment.ID, task.ID)
	if err != nil {
		return 0, fmt.Errorf("worklet: resolve comment %s at %s: %w",
			comment.ID, c.position, err)
	}
	return affected(res)
}

// removeComment takes a stored comment down: the flag, its words blanked in
// the column and the document, and the instant it was taken down.
//
// The row stays, so a reply under it still resolves, and the rest stays what
// its post made it ([editComment]). The excerpts in its post's history row and
// inbox notices are blanked too, or the feed and inbox would go on quoting it;
// the record stays in the log, and only a purge forgets that. A row this node
// does not hold writes nothing, for an edit's reason.
func removeComment(ctx context.Context, tx *sql.Tx, task Task, comment Comment,
	c applyContext) (int, error) {

	var stored []byte
	err := tx.QueryRowContext(ctx, `
		SELECT document FROM tracker_comments WHERE id = ? AND task_id = ?`,
		comment.ID, task.ID).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("worklet: read comment %s to remove at %s: %w",
			comment.ID, c.position, err)
	}
	var removed Comment
	if err = json.Unmarshal(stored, &removed); err != nil {
		return 0, fmt.Errorf("worklet: decode comment %s to remove at %s: %w",
			comment.ID, c.position, err)
	}
	removed.Body, removed.Removed = "", true
	if !comment.UpdatedAt.IsZero() {
		removed.UpdatedAt = comment.UpdatedAt
	}
	document, err := json.Marshal(removed)
	if err != nil {
		return 0, fmt.Errorf("worklet: encode comment %s at %s: %w",
			comment.ID, c.position, err)
	}
	res, err := tx.ExecContext(ctx, removeCommentStatement,
		store.EncodeTime(commentUpdatedAt(removed)), document, comment.ID, task.ID)
	if err != nil {
		return 0, fmt.Errorf("worklet: remove comment %s at %s: %w",
			comment.ID, c.position, err)
	}
	written, err := affected(res)
	if err != nil || written == 0 {
		return written, err
	}
	if _, err = tx.ExecContext(ctx, unquoteNoticesStatement, task.ID, comment.ID); err != nil {
		return 0, fmt.Errorf("worklet: stop quoting comment %s in the inbox at %s: %w",
			comment.ID, c.position, err)
	}
	if _, err = tx.ExecContext(ctx, unquoteHistoryStatement, task.ID, comment.ID); err != nil {
		return 0, fmt.Errorf("worklet: stop quoting comment %s in the history at %s: %w",
			comment.ID, c.position, err)
	}
	return written, nil
}

// The removal's statements: the row taken down, and the excerpts of it the
// post's history row and inbox notices carry, found among the task's own
// history (`tracker_history_subject_idx`) by the comment's id, and the notices
// by those rows' ids.
const (
	removeCommentStatement = `
		UPDATE tracker_comments SET removed = 1, body = '', updated_at = ?, document = ?
		WHERE id = ? AND task_id = ?`
	unquoteNoticesStatement = `
		UPDATE tracker_notifications SET excerpt = ''
		WHERE record_id IN (
			SELECT id FROM tracker_history
			WHERE subject_id = ? AND comment_id = ? AND excerpt <> '')`
	unquoteHistoryStatement = `
		UPDATE tracker_history SET excerpt = ''
		WHERE subject_id = ? AND comment_id = ? AND excerpt <> ''`
)

// commentUpdatedAt is the edit instant, defaulting to the creation one so the
// column is never zero on a comment nobody has touched.
func commentUpdatedAt(comment Comment) time.Time {
	if comment.UpdatedAt.IsZero() {
		return comment.CreatedAt
	}
	return comment.UpdatedAt
}

// writeBodyRevision keeps the body a write replaced, and prunes what the
// record says it pruned.
//
// The prune is on the commit, so every node deletes the same rows rather than
// deciding from a local count.
func writeBodyRevision(ctx context.Context, tx *sql.Tx, task Task,
	revision BodyRevision, retires []int) (int, error) {

	document, err := json.Marshal(revision)
	if err != nil {
		return 0, fmt.Errorf("worklet: encode revision %d of %s: %w",
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
		return 0, fmt.Errorf("worklet: write revision %d of %s: %w",
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
			return 0, fmt.Errorf("worklet: retire revision %d of %s: %w",
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
// Clear-and-rebuild, so a reprocess converges and a deleted item leaves.
func writeChecklistItems(ctx context.Context, tx *sql.Tx, task Task,
	maxVariables int) (int, error) {

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_checklist_items WHERE task_id = ?`, task.ID); err != nil {
		return 0, fmt.Errorf("worklet: clear the checklist of %s: %w", task.ID, err)
	}
	// Flattened, since a row template repeats over one slice; `ord` stays
	// the index within its own list, not the flat position.
	type row struct {
		list string
		item ChecklistItem
		ord  int
	}
	rows := make([]row, 0, len(task.Checklists))
	for _, list := range task.Checklists {
		for i, item := range list.Items {
			rows = append(rows, row{list: list.ID, item: item, ord: i})
		}
	}
	written, err := insertMany(ctx, tx, maxVariables, `
		INSERT INTO tracker_checklist_items
			(task_id, checklist_id, item_id, name, done, assignee,
			 parent_id, ord)
		VALUES`,
		`(?,?,?,?,?,?,?,?)`,
		`ON CONFLICT (task_id, checklist_id, item_id) DO UPDATE SET
			name = excluded.name, done = excluded.done,
			assignee = excluded.assignee, parent_id = excluded.parent_id,
			ord = excluded.ord`,
		rows, func(r row) []any {
			return []any{task.ID, r.list, r.item.ID, r.item.Name,
				boolInt(r.item.Done), r.item.Assignee, r.item.Parent, r.ord}
		})
	if err != nil {
		return 0, fmt.Errorf("worklet: write the checklists of %s: %w", task.ID, err)
	}
	return written, nil
}
