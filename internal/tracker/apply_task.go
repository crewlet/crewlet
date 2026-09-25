package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
	if c.record.Op == OpPurge {
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
		return a.writeHistory(ctx, tx, c,
			subjectKeys{Project: current.Project, Key: current.Key}, nil)
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
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := stampFinish(&next, c); err != nil {
		return 0, err
	}

	// AND THE ARCHIVE STAMP, on the same rule and for a column that was
	// written as NULL on every row: `archived` is a bool the patch
	// carries, and when and by whom it was set had no writer at all —
	// so a board filtered to the archive could order it by nothing and
	// an operator asking who filed something away had no answer.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := stampArchive(&next, current, held, c); err != nil {
		return 0, err
	}

	document, err := json.Marshal(next)
	if err != nil {
		return 0, fmt.Errorf("tracker: encode task %s at %s: %w", id, c.position, err)
	}

	// THE PROJECT'S FIELD DECLARATIONS, READ ONCE FOR BOTH READERS OF THEM.
	//
	// [Applier.explodeFieldValues] needs them to decide which typed column
	// each value goes in, and [TaskDeltas] needs them to name a value by its
	// SLUG rather than by the uuid it is keyed under. Two reads of one
	// document in one transaction is waste, and the two could drift on a
	// catalogue commit interleaved between them, so the read is hoisted
	// here and the map is passed down.
	//
	// READ ONLY WHEN A SIDE CARRIES VALUES, which is what keeps this off the
	// hot path: the overwhelming majority of task commits touch no custom
	// field at all, and a company that declares none never reads a
	// catalogue here. BOTH sides, because a commit that CLEARED every value
	// still has to name what it cleared, and `next` alone would answer for
	// neither. The guard is one line from its consumers so the invariant
	// they rest on — a nil map means neither side had a value — is checkable
	// by eye.
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
	children, err := a.explodeTask(ctx, tx, next, declared, c)
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
	// history row and the entered stamp below are both derived from.
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
	// THE STATUS MOVED, not "somebody was told the status moved". Gated on
	// the notification, a quiet status change left the entered instant
	// naming an older change than the task had actually last made.
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

// stampArchive records a task being filed away, and by whom.
//
// ON THE EDGE, not on every commit: a task that is already archived and is
// then edited keeps the instant it was archived at, which is what a reader
// asking "when did this leave the board" means. Un-archiving clears both, on
// [stampFinish]'s rule — a stamp that outlived the state it describes would
// make "archived last Tuesday" true of a task sitting on the board.
//
// THE AUTHOR COMES FROM THE RECORD and the instant from the broker, which is
// the same split every other derived stamp here uses: who did it is a fact
// about the write, and when is a fact about the log.
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
	switch c.record.Op {
	case OpCreate:
		// A [TaskCreate]: the task, and the question it was filed as —
		// which is a comment ROW, written by [Applier.writeThread], and
		// never part of the document this returns.
		var payload TaskCreate
		if err := decodePayload(c.record.Mutation, &payload); err != nil {
			return Task{}, fmt.Errorf("tracker: decode the create at %s: %w",
				c.position, err)
		}
		created := payload.Task
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

// clearableInstant is a patched date, with the zero value read as a clear.
func clearableInstant(at *time.Time) *time.Time {
	if at == nil || at.IsZero() {
		return nil
	}
	return at
}

// Patched is [applyPatch] under an exported name, for the ONE caller outside
// this package that has to answer the same question: a tool composing the
// WAKE that announces a write it is about to make.
//
// EXPORTED RATHER THAN REIMPLEMENTED, because the second implementation is
// what this exists to end. The tool kept its own field-by-field copy of this
// merge, and every field added to [TaskPatch] since had to be remembered in
// two places — so when the schedule fields arrived, the durable row took them
// and the wake's snapshot did not. The delta was then computed between a task
// and itself, and every due date, estimate and size a seat moved reached its
// notification as a change that changed nothing.
//
// The caller still layers what is genuinely ITS own on top: a wake's
// recipient list reflects the watch gesture in flight, which the durable sets
// settle later inside the writer's transaction.
func Patched(task Task, patch TaskPatch) Task { return applyPatch(task, patch) }

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
	// A ZERO INSTANT IS HOW A PATCH SPELLS "CLEAR IT": an absent field
	// means "leave it alone", so a clear has to be a VALUE. Stored
	// verbatim, a zero would be a due date in January of year one — which
	// every overdue predicate in the tracker reads as the most overdue
	// task the company has ever had.
	if patch.StartAt != nil {
		task.StartAt = clearableInstant(patch.StartAt)
	}
	if patch.DueAt != nil {
		task.DueAt = clearableInstant(patch.DueAt)
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
			// THE INTENT GOES DOWN WITH THE MARKER, whatever this
			// same patch said about it and in whatever order. It
			// describes a walk that is running, so "not merging,
			// but re-parenting" is a state the duty would read as
			// an instruction with nothing to instruct.
			task.MergeReparent = false
		}
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
			(id, key, project_key, filed_unit, routing_unit,
			 parent_id, root_id, depth, type, title, body_version, status,
			 status_group, status_entered_at, priority, prio_rank, rank,
			 reporter, assignee, start_at, due_at, due_all_day, estimate_min,
			 points, spend_turns, spend_rounds, spend_input, spend_output,
			 spend_cache_read, spend_cache_write, spend_wall_ms, spend_tokens,
			 spend_workers, spend_sent_back, done_at, closed_at, finished_at, archived, archived_at, removed_at,
			 removed_with, batch_id, merging, reassignments, policy_stamp,
			 unblocked_told_at, search_rev, embed_rev, inconsistent_project,
			 cycle, too_deep, key_collision, created_at, updated_at, version,
			 scoped_through, document)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,?,?,?,?,?,?,?,?,
		        ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,0,0,0,0,0,0,?,?,?,0,?)
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
			removed_with = excluded.removed_with, batch_id = excluded.batch_id,
			merging = excluded.merging,
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
func (a *Applier) explodeTask(ctx context.Context, tx *sql.Tx,
	task Task, declared map[string]FieldDef, c applyContext) (int, error) {

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
			return insertMany(ctx, tx, c.maxVariables,
				`INSERT INTO tracker_task_tags (task_id, project_key, slug) VALUES`,
				`(?,?,?)`,
				`ON CONFLICT (task_id, slug) DO NOTHING`,
				task.Tags, func(s string) []any { return []any{task.ID, task.Project, s} })
		}},
		{"tracker_relations", func() (int, error) {
			// `one_sided` IS DERIVED HERE AND NEVER CARRIED. Whether a
			// blocker lists this task among its dependents is a fact
			// about the OTHER end's rows, so a writer stating it would
			// be stating something it read in a different transaction
			// on a different subject — and, until this derivation
			// existed, every record said nothing and the column was
			// zero on every row a repair duty was supposed to find.
			//
			// `one_sided_final` is the opposite kind of fact and stays
			// on the record: it is the duty's DECISION that this edge
			// will never be mirrored, which no derivation can reach.
			//
			// THE SUBQUERY REPEATS PER ROW AND STILL READS WHAT IT
			// READ BEFORE, which is what makes the multi-row form
			// safe here. It probes tracker_task_dependents while
			// this statement writes tracker_relations, so no row of
			// the batch can change what a later row of the same
			// batch sees — and the only write to that table in this
			// apply is the entry BELOW this one in the same loop,
			// which is still ahead of us whether the rows went out
			// one at a time or all at once.
			return insertMany(ctx, tx, c.maxVariables, `
				INSERT INTO tracker_relations
					(task_id, other_id, kind, derived, one_sided,
					 one_sided_final, note, created_at)
				VALUES`,
				`(?,?,?,0,
					CASE WHEN ? = 'waiting_on' AND NOT EXISTS (
						SELECT 1 FROM tracker_task_dependents d
						WHERE d.task_id = ? AND d.dependent_id = ?)
					THEN 1 ELSE 0 END, ?, ?, ?)`,
				`ON CONFLICT (task_id, other_id, kind) DO UPDATE SET
					one_sided = excluded.one_sided,
					one_sided_final = excluded.one_sided_final,
					note = excluded.note`,
				task.Relations, func(r Relation) []any {
					return []any{task.ID, r.Other, string(r.Kind),
						string(r.Kind), r.Other, task.ID,
						boolInt(r.OneSidedFinal), r.Note,
						// THE AUTHORED INSTANT, and NOT re-stamped on
						// conflict: this is what the repair ages on, and
						// an edge whose clock restarted every time its
						// task was touched is one the duty would never
						// reach on a busy task.
						store.EncodeTime(r.CreatedAt)}
				})
		}},
		{"tracker_task_dependents", func() (int, error) {
			// THE MIRROR AS A ROW, so the derivation above is an
			// indexed probe rather than a document decode per edge.
			return insertMany(ctx, tx, c.maxVariables,
				`INSERT INTO tracker_task_dependents (task_id, dependent_id) VALUES`,
				`(?,?)`,
				`ON CONFLICT (task_id, dependent_id) DO NOTHING`,
				task.Dependents, func(id string) []any {
					return []any{task.ID, id}
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
	// column each value goes in — which the loop's shape has no room for,
	// and which [Applier.applyTask] reads and hands down rather than
	// re-reading here.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_field_values WHERE task_id = ?`, task.ID); err != nil {
		return 0, fmt.Errorf("tracker: clear tracker_field_values for %s: %w",
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
	keys, err := a.maintainKeys(ctx, tx, task, c)
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
func (a *Applier) maintainDeps(ctx context.Context, tx *sql.Tx, task Task,
	c applyContext) (int, error) {

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_task_deps WHERE task_id = ?`, task.ID); err != nil {
		return 0, fmt.Errorf("tracker: clear the dependencies of %s: %w", task.ID, err)
	}
	// THE WAITING_ON EDGES ARE THE COLLECTION, filtered out of the
	// relations the record carries, because a task's other relations
	// (`relates_to`, `duplicates`) are not dependencies and write no row
	// here.
	blockers := make([]Relation, 0, len(task.Relations))
	for _, relation := range task.Relations {
		if relation.Kind == RelationWaitingOn {
			blockers = append(blockers, relation)
		}
	}
	// A ROW OF LITERAL SUBQUERIES rather than the INSERT ... SELECT this
	// was: with no FROM clause the SELECT produced exactly one row, so the
	// two forms insert the same row from the same four binds — and only
	// the VALUES form is a row template a multi-row insert can repeat.
	// Each row's two probes read tracker_tasks while this statement writes
	// tracker_task_deps, so batching cannot change what any of them sees.
	written, err := insertMany(ctx, tx, c.maxVariables, `
		INSERT INTO tracker_task_deps (blocker_id, task_id, blocker_open, cleared_at)
		VALUES`,
		`(?, ?,
		  COALESCE((SELECT CASE WHEN b.status_group IN ('done','closed')
		                        THEN 0 ELSE 1 END
		            FROM tracker_tasks b WHERE b.id = ?), 1),
		  (SELECT b.finished_at FROM tracker_tasks b WHERE b.id = ?))`,
		`ON CONFLICT (blocker_id, task_id) DO UPDATE SET
			blocker_open = excluded.blocker_open,
			cleared_at = excluded.cleared_at`,
		blockers, func(r Relation) []any {
			return []any{r.Other, task.ID, r.Other, r.Other}
		})
	if err != nil {
		return 0, fmt.Errorf("tracker: write the dependencies of %s: %w",
			task.ID, err)
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
	written += n

	// AND THE MIRROR FLAG, FROM THIS SIDE. `one_sided` is derived on the
	// DEPENDENT's apply from whether this blocker listed it; this commit
	// is the other half — the blocker's own `Dependents` just changed, so
	// every edge pointing at it is re-judged against the rows this
	// transaction has written.
	//
	// BOTH DIRECTIONS ARE NEEDED and neither is redundant: without this
	// one a mirror that lands SECOND never clears the flag its own
	// dependent set while waiting, and the duty repairs an edge that is
	// already whole — for ever, because its repair writes the mirror that
	// is already there.
	res, err = tx.ExecContext(ctx, `
		UPDATE tracker_relations SET one_sided =
			CASE WHEN EXISTS (SELECT 1 FROM tracker_task_dependents d
			                  WHERE d.task_id = ? AND d.dependent_id = tracker_relations.task_id)
			THEN 0 ELSE 1 END
		WHERE other_id = ? AND kind = 'waiting_on'`, task.ID, task.ID)
	if err != nil {
		return 0, fmt.Errorf("tracker: re-judge the edges waiting on %s: %w",
			task.ID, err)
	}
	n, err = affected(res)
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
func (a *Applier) maintainKeys(ctx context.Context, tx *sql.Tx, task Task,
	c applyContext) (int, error) {

	// THE CURRENT KEY AND THE FORMER ONES ARE ONE COLLECTION, with the
	// empty ones dropped rather than skipped mid-loop — a row template
	// repeats for every element it is given, so the filtering happens here
	// instead.
	//
	// A key that is in both lists — a rename that came back — is two rows
	// in one statement, and they agree: `current` is computed from the
	// entry rather than from its position, so the upsert resolves the pair
	// to the same value whichever of them lands second.
	entries := make([]string, 0, 1+len(task.FormerKeys))
	for _, entry := range append([]string{task.Key}, task.FormerKeys...) {
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	written, err := insertMany(ctx, tx, c.maxVariables,
		`INSERT INTO tracker_task_keys (key, task_id, current) VALUES`,
		`(?,?,?)`,
		`ON CONFLICT (key) DO UPDATE SET current = excluded.current
		 WHERE tracker_task_keys.task_id = excluded.task_id`,
		entries, func(entry string) []any {
			return []any{entry, task.ID, boolInt(entry == task.Key)}
		})
	if err != nil {
		return 0, fmt.Errorf("tracker: claim the keys of %s: %w", task.ID, err)
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
	return written, nil
}

// moveProjectCount moves one of the three maintained counters by one.
//
// ONE STATEMENT BUILDER FOR THE THREE CALLERS — a task entering a bucket, a
// task leaving one, and the purge that deletes the row instead of writing one.
// The bucket is always [bucketOf]'s own closed answer and never a value off
// the wire, which is what makes interpolating it into the column name safe: a
// column cannot be a parameter.
//
// The decrement CLAMPS AT ZERO. A negative census is a number no screen can
// render and no repair can interpret, and the clamp costs nothing on the path
// where the count is right.
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
		return 0, fmt.Errorf("tracker: %s the %s count of %s: %w",
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
	task, held, err := readTask(ctx, tx, id)
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
	// AND THE PROJECT'S CENSUS COMES DOWN WITH THE ROW.
	//
	// Every other departure from a bucket is a task RECORD and goes
	// through [Applier.maintainProjectCounts]; a purge deletes the row
	// instead of writing one, so nothing lowered the count it was in. A
	// task purged while it was open left `open_count` one too high on
	// every node — for ever, because these counts are MAINTAINED and
	// nothing anywhere aggregates them back into agreement — and a purge
	// does not require the task to have been removed first, so this is
	// the ordinary shape rather than a corner of one.
	//
	// GATED ON THE ROW HAVING BEEN HERE, because the deletion gate lets
	// this record's own redelivery through by op id (it is what wrote the
	// marker) and every statement below is then a no-op. A second
	// decrement would take the census one BELOW the truth, which is the
	// half the clamp in [moveProjectCount] cannot see.
	if bucket := bucketOf(task); held && bucket != "" {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		n, err := moveProjectCount(ctx, tx, bucket, task.Project, -1)
		if err != nil {
			return 0, err
		}
		written += n
	}
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
		{`DELETE FROM tracker_task_closure WHERE ancestor_id = ? OR descendant_id = ?`, []any{id, id}},
		{`DELETE FROM tracker_body_revisions WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_comments WHERE task_id = ?`, []any{id}},
		{`DELETE FROM tracker_tasks WHERE id = ?`, []any{id}},
	} {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
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
	history, err := a.writeHistory(ctx, tx, c,
		subjectKeys{Project: task.Project, Key: task.Key}, nil)
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

// removedWith binds the cascade root into the task's own column, or NULL.
//
// DERIVED FROM [removedWithText], which is the delta's form of the same fact,
// so the column and the history row cannot name two different roots.
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
// It used to run one statement per element, on the reasoning that every
// collection here is capped so the loop is "a handful of statements" and a
// generated multi-row insert would be a second statement builder to keep
// correct. Both halves were wrong. The caps are [MaxWatchers]'s 64,
// [MaxTagsPerTask]'s 40 and [MaxTagsPerProject]'s 512 rather than "small", and
// every one of those rows cost a statement of its own — the shape
// BenchmarkLogApplyDrain names "unprepared" and measures as the slowest of the
// three. And the statement builder is not a second one: it is
// [store.InsertRows], written once beside the chunker it uses, which is the
// arrangement [textcut] and [whsec] exist to record the cost of not having.
//
// WHAT IS KEPT FROM THE OLD SHAPE is the generic ergonomics: a caller hands a
// typed slice and a per-item argument builder rather than an index closure, so
// a call site reads as the collection it writes. What CHANGES is that the
// statement arrives in three parts — the prefix through VALUES, one
// parenthesised row template, and the ON CONFLICT clause that follows the
// value list — because the row template is what gets repeated and the row
// templates here carry a CASE, a scalar subquery and literals, so no rule that
// splits a whole statement string on "VALUES" survives them.
//
// maxVariables is [applyContext.maxVariables]. Zero degrades to a row per
// statement, which is exactly the behaviour this replaced.
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
			return 0, fmt.Errorf("tracker: clear the tags of %s: %w", subject.ID, err)
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

// explodeCatalogue settles what a catalogue apply changes about the ROWS.
//
// # Why a type catalogue explodes into nothing at all
//
// Because nothing seeks a declaration. Every reader of one goes through the
// DOCUMENT — one row, one decode — since a company has tens of fields rather
// than thousands and the whole set is wanted at once. The relational copy this
// used to keep (`tracker_types`, `tracker_fields`, `tracker_field_options`)
// was a delete-and-rewrite of the entire catalogue per apply, per node, over
// four maintained indexes, answering no question; migration 0010 drops it.
// The `shadowed` column is what gave it away: inserted as a literal 0 on every
// row, and the reader that reports shadowing derives it in Go from the two
// declaration documents.
//
// What a catalogue apply DOES change about rows is the per-task VALUES, which
// every `f.<ref>` filter seeks — and specifically their visibility.
func (a *Applier) explodeCatalogue(ctx context.Context, tx *sql.Tx, name string,
	c applyContext) (int, error) {

	if name == CatalogueTypes {
		// A TYPE CATALOGUE TOUCHES NO ROW. A task's type is a column on
		// the task itself, written by the task's own commit, so a type
		// being renamed or archived changes nothing a query reads until
		// somebody writes the task.
		return 0, nil
	}
	var catalogue FieldCatalogue
	if err := decodePayload(c.record.Mutation, &catalogue); err != nil {
		return 0, err
	}
	// THE VALUES FOLLOW THE DECLARATION'S ARCHIVE, in this same
	// transaction.
	//
	// `hidden` on a value row is the DECLARATION's archived state, and
	// nothing else propagated it: a task untouched since the archive kept
	// `hidden = 0` for ever, so the DDL's own rule — "every filter and
	// total adds `hidden = 0 AND kind <> 'foreign'`" — described a column
	// that did not carry what it claimed. The filter is shielded anyway,
	// because an archived field does not RESOLVE, but a row that lies
	// about its own state is a trap for the next reader of it.
	return a.settleFieldVisibility(ctx, tx, catalogue.Fields)
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
	items, err := writeChecklistItems(ctx, tx, task, c.maxVariables)
	if err != nil {
		return 0, err
	}
	written += items

	if c.record.Op == OpCreate {
		// A CREATE CARRIES AT MOST ONE COMMENT — the question the task
		// was filed as ([TaskCreate]) — and never a body revision, since
		// there is no earlier body for one to keep.
		var created TaskCreate
		if err := decodePayload(c.record.Mutation, &created); err != nil {
			return 0, fmt.Errorf("tracker: decode the create at %s: %w", c.position, err)
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
		// ONLY A PATCH OR A CREATE CARRIES ONE. A tombstone carries a
		// stamp, and decoding it as a patch to look for a comment would
		// be reading a shape that is not there.
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
func writeChecklistItems(ctx context.Context, tx *sql.Tx, task Task,
	maxVariables int) (int, error) {

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_checklist_items WHERE task_id = ?`, task.ID); err != nil {
		return 0, fmt.Errorf("tracker: clear the checklist of %s: %w", task.ID, err)
	}
	// EVERY LIST'S ITEMS ARE ONE COLLECTION, flattened before the write
	// because a row template repeats over one slice and the nesting here
	// is a detail of the document rather than of the table: the rows carry
	// their own `checklist_id`, and `ord` is the item's index WITHIN ITS
	// OWN LIST, which is why the flattening keeps it rather than using the
	// flat position.
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
			 parent_id, ord, promoted_to)
		VALUES`,
		`(?,?,?,?,?,?,?,?,?)`,
		`ON CONFLICT (task_id, checklist_id, item_id) DO UPDATE SET
			name = excluded.name, done = excluded.done,
			assignee = excluded.assignee, parent_id = excluded.parent_id,
			ord = excluded.ord, promoted_to = excluded.promoted_to`,
		rows, func(r row) []any {
			return []any{task.ID, r.list, r.item.ID, r.item.Name,
				boolInt(r.item.Done), r.item.Assignee, r.item.Parent, r.ord,
				r.item.PromotedTo}
		})
	if err != nil {
		return 0, fmt.Errorf("tracker: write the checklists of %s: %w", task.ID, err)
	}
	return written, nil
}
