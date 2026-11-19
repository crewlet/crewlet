package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/store"
)

// The history row, the effective instant, the entered stamp, the project's
// last-change stamp and the inbox: the five things every commit produces
// besides its own object's row.
//
// A QUIET COMMIT WRITES A HISTORY ROW LIKE EVERY OTHER: "quiet" only means it
// wakes nobody, so the feed is a complete account of what happened and
// `notified` tells "nothing was announced" from "nothing happened".
//
// EVERY HISTORY ROW SAYS WHAT MOVED: for every kind [ObjectKind.RecordsHistory]
// names, `fields_json` holds the APPLIER's deltas between the stored object and
// the one the record says it should be ([TaskDeltas] for a task, deltas.go for
// every whole-document object), never anything about who was told.

// writeHistory writes the record's history row and everything that hangs off
// it. Everything derived hangs off this write rather than the object row: the
// version guard SKIPS a reprocessed record below an applied successor on the
// object row, so a recompute there would leave it out of the spans, and a
// missing span is worse than a slightly wrong instant.
func (a *Applier) writeHistory(ctx context.Context, tx *sql.Tx, c applyContext,
	keys subjectKeys, applied map[string]Delta) (int, error) {

	subject := c.subject()
	effective, err := effectiveAt(ctx, tx, subject.ID, c)
	if err != nil {
		return 0, err
	}
	notify := c.record.Notify
	excerpt, fields, commentID := "", "{}", ""
	notified := 0
	late := 0
	if notify != nil {
		excerpt = notify.Excerpt
		commentID = notify.CommentID
		notified = 1
		late = boolInt(notify.Late)
	}
	// THE KIND IS THE RECORD'S OWN, a different fact from whether anybody
	// was told (a quiet purge is still `purged`). [checkChangeKind] refuses
	// a record without one, so it is stored verbatim, a newer peer's kind
	// included, identically on every node. See [MutationRecord.Kind].
	kind := string(c.record.Kind)
	// THE DELTAS ARE THE APPLIER'S, loud or quiet (deltas.go): an apply that
	// moved nothing stores `{}` even when its wake says otherwise. The wake
	// is built from a snapshot read OUTSIDE the write's transaction, so a
	// change another writer already made reads there as a move and here as
	// none, and storing it would record a {from,to} this apply never made.
	// The one caller with no comparison passes the record's statement
	// explicitly ([statedDeltas]); nothing here falls back to it.
	if len(applied) > 0 {
		fields = jsonOf(applied)
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_history
			(id, subject_kind, subject_id, project_key, kind, actor, actor_kind,
			 operator_id, comment_id, excerpt, fields_json, turn_id,
			 notified, late, log_seq, log_stream, log_generation, created_at,
			 broker_at, effective_at, skew_ms, document, actor_apps, working_for)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO NOTHING`,
		historyID(c), string(subject.Kind), subject.ID, keys.Project, kind,
		c.record.Actor, string(c.record.ActorKind), c.record.OperatorID,
		commentID, excerpt, fields, c.record.TurnID,
		notified, late, c.packed, c.position.Stream, c.position.Generation,
		store.EncodeTime(c.authored()), store.EncodeTime(c.brokerAt),
		store.EncodeTime(effective), c.skewMs(), []byte(c.record.Mutation),
		appsColumn(c.record), workingForColumn(c.record))
	if err != nil {
		return 0, fmt.Errorf("worklet: write the history row at %s: %w", c.position, err)
	}
	rows, err := affected(res)
	if err != nil {
		return 0, err
	}
	if rows == 0 {
		// A REDELIVERY: everything below is derived from this row, and the
		// notification rows are keyed on the record too.
		return 0, nil
	}

	// THE REOPEN COUNTER, off the delta this row has just stored — see
	// [countReopen] for why the row rather than the documents.
	reopens, err := countReopen(ctx, tx, subject, fields)
	if err != nil {
		return 0, err
	}
	// AND THE HAND-OFF COUNT this commit left, from the history rather than
	// the task row — see apply_handoffs.go for why.
	handOffs := 0
	if subject.Kind == KindTask {
		if handOffs, err = deriveHandOffs(ctx, tx, subject.ID, c.packed); err != nil {
			return 0, err
		}
	}
	successors, err := a.raiseSuccessors(ctx, tx, subject.ID, c)
	if err != nil {
		return 0, err
	}
	stamped, err := a.stampProjectChange(ctx, tx, c, keys.Project)
	if err != nil {
		return 0, err
	}
	noticed, err := a.writeInbox(ctx, tx, c, historyID(c), keys.Key)
	if err != nil {
		return 0, err
	}
	return rows + reopens + handOffs + successors + stamped + noticed, nil
}

// stampProjectChange records that this commit changed the project's work, and
// who made it. It hangs off the HISTORY row, written on every task commit and
// filed under the task's project, so THE DIRECTORY'S LAST CHANGE IS THE HEAD OF
// THAT PROJECT'S OWN ACTIVITY FEED. Hence a turn (cost, not work), a rank
// re-order (the board's order, not in the feed) and the project document itself
// (its settings, filed under no project) do not move it, and a task moved
// between projects stamps the one it moved TO.
//
// The instant is the AUTHORED one, like every displayed instant beside it; the
// effective one is for durations. The guard is the log position: the values
// are one record's, so the row names the commit HIGHEST IN THE LOG, the same on
// every node in any order, where two records sharing an instant would tie.
func (a *Applier) stampProjectChange(ctx context.Context, tx *sql.Tx,
	c applyContext, project string) (int, error) {

	// The kind is checked rather than inferred from a non-empty key, so a
	// caller passing a project key for another subject cannot move a column
	// that means "this project's work".
	if project == "" || c.subject().Kind != KindTask {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE tracker_projects
		SET last_change_at = ?, last_change_actor = ?,
		    last_change_actor_kind = ?, last_change_seq = ?
		WHERE key = ? AND last_change_seq < ?`,
		store.EncodeTime(c.authored()), c.record.Actor,
		string(c.record.ActorKind), c.packed, project, c.packed)
	if err != nil {
		return 0, fmt.Errorf("worklet: stamp %s's last change at %s: %w",
			project, c.position, err)
	}
	return affected(res)
}

// raiseHistoryStatement is [Applier.raiseSuccessors]' statement over the
// history rows, named for the plan test: an UPDATE finds its rows by its WHERE
// exactly as a SELECT does, on every late record.
const raiseHistoryStatement = `
		UPDATE tracker_history SET effective_at = MAX(effective_at, ?)
		WHERE subject_id = ? AND log_seq > ? AND effective_at < ?`

// raiseSuccessors re-derives the effective instant of every row ABOVE this
// one. The instant is a MAX over every row about the subject at or below its
// position, so a late record can only RAISE a successor's, and a max is
// confluent: nodes applying the same records in any order agree. A fold over
// arrival order is not, and the value's two readers compare it against each
// other, so a divergence would fire a repair duty on one node for ever.
func (a *Applier) raiseSuccessors(ctx context.Context, tx *sql.Tx, subjectID string,
	c applyContext) (int, error) {

	broker := store.EncodeTime(c.brokerAt)
	var rows int
	for _, statement := range []struct{ sql, column string }{
		{raiseHistoryStatement, "history"},
		{`UPDATE tracker_turns SET effective_at = MAX(effective_at, ?)
		  WHERE task_id = ? AND log_seq > ? AND effective_at < ?`, "turns"},
	} {
		res, err := tx.ExecContext(ctx, statement.sql, broker, subjectID, c.packed, broker)
		if err != nil {
			return 0, fmt.Errorf("worklet: raise the %s successors of %s: %w",
				statement.column, subjectID, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		rows += n
	}
	return rows, nil
}

// newestStatusStatement is [Applier.stampStatusEntered]'s read, named for the
// plan test.
const newestStatusStatement = `
		SELECT h.effective_at
		FROM tracker_history h
		WHERE h.subject_id = ?
		  AND json_extract(h.fields_json, '$.status.to') IS NOT NULL
		ORDER BY h.log_seq DESC
		LIMIT 1`

// statusEnteredStatement is [Applier.stampStatusEntered]'s write, named for
// the plan test.
const statusEnteredStatement = `UPDATE tracker_tasks SET status_entered_at = ? WHERE id = ?`

// stampStatusEntered rewrites a task's `status_entered_at` from its history:
// the newest status change's EFFECTIVE instant, ordered by the composed
// position, so a late reprocess and a redelivery are no-ops and a duration
// measured against it is never negative (an authored clock can run backwards
// between nodes). It is rewritten here rather than on the object row because
// the version guard skips a reprocessed record there. Only the newest row is
// read, one seek on `tracker_history_subject_idx`. The status's validity is
// not checked: the column is an instant needing no group, and skipping an
// unknown status would leave it naming an older change.
func (a *Applier) stampStatusEntered(ctx context.Context, tx *sql.Tx, taskID string) (int, error) {
	var at int64
	switch err := tx.QueryRowContext(ctx, newestStatusStatement, taskID).Scan(&at); {
	case errors.Is(err, sql.ErrNoRows):
		// No status change in the history: nothing to stamp, and the
		// column keeps what it holds.
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("worklet: read the newest status of %s: %w", taskID, err)
	}
	if _, err := tx.ExecContext(ctx, statusEnteredStatement, at, taskID); err != nil {
		return 0, fmt.Errorf("worklet: stamp the entered instant of %s: %w",
			taskID, err)
	}
	return 1, nil
}

// subjectKeys is what the applier read off the object row ("ENG", "ENG-1"),
// named so a call site cannot swap two bare strings. Both come from the row
// because a writer cannot state them on a CREATE: the item key is minted by
// the write itself, from the project's counter, so a notification cannot carry
// it ([InboxNotice.SubjectKey]). Zero for a subject with no item key (a
// project, a view, a person).
type subjectKeys struct{ Project, Key string }

// inboxSubjectKey is the key a notice carries. The applier's row wins over the
// writer's snapshot, a claim formed before the write that a rename makes
// stale; the snapshot is the fallback for a subject the applier holds no row
// for.
func inboxSubjectKey(fromRow string, notify *Notify) string {
	if fromRow != "" {
		return fromRow
	}
	return notify.Snapshot.Key
}

// writeInbox writes one row per candidate the commit concerned: every
// candidate, not every recipient, since the row is the complete record of whom
// the change concerned and the routing filter, with a registry this applier
// does not have, later decides who is woken.
//
// NEVER THE PERSON WHO MADE THE CHANGE, except for [Reason.WakesActor]: the
// same predicate [Route] uses ([routing.Candidate.Hears]). It needs no
// registry, and this table is what `work_inbox` reads, so it is kept here: a
// row for the actor is a notice in their own inbox about what they just did.
//
// The retention horizon is applied around [Candidates], keeping it free of a
// clock. Every row is keyed on record: the history row's id, or a flow
// notice's own key ([Applier.applyFlow]).
func (a *Applier) writeInbox(ctx context.Context, tx *sql.Tx, c applyContext,
	record, subjectKey string) (int, error) {

	notify := c.record.Notify
	if notify == nil {
		// A nil guard, not the rule: that a quiet commit concerns nobody
		// is [Candidates]' property and is asserted there.
		return 0, nil
	}
	if horizon := inbox.Horizon(c.epoch); horizon > 0 && !c.brokerAt.IsZero() {
		// A RECORD OLDER THAN THE HORIZON WRITES NO INBOX ROWS, so a
		// whole-log replay rebuilds no expired inbox rather than writing
		// a year of rows to sweep.
		if c.authored().Before(c.brokerAt.Add(-horizon)) {
			return 0, nil
		}
	}
	candidates := Candidates(notify)
	// THE TASK THE NOTICE IS ABOUT, beside its subject: for a lead's
	// priorities write the subject is the PERSON and the key the TASK's,
	// and without the id a reader cannot tell whether that key still opens
	// the task the wake meant. See [InboxNotice.Task].
	task := notify.Snapshot.TaskID(c.subject())
	woken := noticesWoke(c.record)
	written := 0
	for _, candidate := range candidates {
		// THE CHANGE'S AUTHOR GETS NO NOTICE OF IT unless
		// [Reason.WakesActor]. The actor is the seat's identity whenever a
		// person writes as their seat ([iam.ActorFor]), the spelling a
		// candidate's handle is in, so one comparison covers a seat, a
		// person at the dashboard and their own assistant.
		if !candidate.Hears(c.record.Actor, Reason.WakesActor) {
			continue
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tracker_notifications
				(record_id, recipient, subject_id, subject_key, task_id, kind,
				 reason, addressed, fallback_only, fallback_rank, excerpt,
				 created_at, log_seq, log_stream, log_generation, asks, starts_turn)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (record_id, recipient) DO NOTHING`,
			record, candidate.Handle, c.subject().ID,
			inboxSubjectKey(subjectKey, notify), task,
			string(notify.Kind), string(candidate.Reason),
			boolInt(candidate.Addressed), boolInt(candidate.FallbackOnly),
			candidate.FallbackRank, notify.Excerpt,
			store.EncodeTime(c.authored()), c.packed, c.position.Stream,
			c.position.Generation, statedAsks(c.record), boolInt(woken == nil || woken[candidate.Handle]))
		if err != nil {
			return 0, fmt.Errorf("worklet: write the inbox row for %s at %s: %w",
				candidate.Handle, c.position, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		if n > 0 {
			// Only a row this transaction wrote: a redelivery moved
			// nobody's inbox ([inbox.Movement]).
			a.inboxed.Note(inbox.Note{
				Record: record, Handle: candidate.Handle,
				Subject: c.subject().ID, Reason: string(candidate.Reason),
				Position: c.packed, At: c.brokerAt,
			})
		}
		written += n
	}
	return written, nil
}

// historyID is the history row's key: the record's own operation id when it
// has one, else its position: never a new uuid, which would differ between
// nodes, and the inbox rows are keyed on this value.
func historyID(c applyContext) string {
	if c.record.OpID != "" {
		return c.record.OpID
	}
	return fmt.Sprintf("%s#%d", c.position.Stream, c.packed)
}

// statedDeltas is the record's WRITER'S account of what it moved (its wake's
// fields), for the one apply with no comparison of its own: a deferred record
// the version guard skipped, reprocessed below an applied successor. Its
// document is replaced by the successor's, and a delta against that would name
// a move this record never made. A plain redelivery takes the same branch and
// stores nothing (`ON CONFLICT (id) DO NOTHING`).
func statedDeltas(c applyContext) map[string]Delta {
	if c.record.Notify == nil {
		return nil
	}
	return c.record.Notify.Fields
}

// appsColumn is a writer's apps as `tracker_history.actor_apps` holds them: a
// JSON list from a record at [AppsKeptVersion] or above that kept them, and
// empty for every other, so every node writes the same row.
func appsColumn(record MutationRecord) string {
	if record.V < AppsKeptVersion || !record.ActorAppsKept || len(record.ActorApps) == 0 {
		return ""
	}
	return jsonOf(record.ActorApps)
}

// workingForColumn is whom a record was written for as
// `tracker_history.working_for` holds it: the carrier's JSON from a record at
// [WorkingForVersion] or above that carries one, and NULL for every other, so
// every node writes the same row (ADR-0109). The work pickup reads it
// ([readyAssignedArm], [readyCreatedArm]).
//
// Kept from the carrier's first build on, before anything read it: a
// replicated migration runs on each node when it opens, not at a log position,
// so a column filled only by a later build would differ between nodes by when
// each upgraded.
func workingForColumn(record MutationRecord) any {
	if record.V < WorkingForVersion || record.WorkingFor == nil {
		return nil
	}
	return jsonOf(record.WorkingFor)
}

// noticesWoke is who a record's change woke, for its notice rows to keep: nil,
// every recipient woken, for a record below [NoticeWokeVersion] or one that
// tells nobody it does not wake, so every node writes the same row.
func noticesWoke(record MutationRecord) map[string]bool {
	if record.V < NoticeWokeVersion || !record.NoticeWokeKept || record.Notify == nil {
		return nil
	}
	woken, _ := noticeWakes(record.Notify, record.Actor)
	return woken
}

// tellsWithoutWaking is whether a change tells somebody it does not wake, which
// is when its record marks that its notice rows keep the decision.
func tellsWithoutWaking(n *Notify, actor string) bool {
	if n == nil {
		return false
	}
	_, tells := noticeWakes(n, actor)
	return tells
}

// statedAsks is a comment author's statement as the notice row keeps it: 1 or
// 0 from a record at [NoticeAsksVersion] or above that kept it, and NULL for
// every other, so every node writes the same row.
func statedAsks(record MutationRecord) any {
	if record.V < NoticeAsksVersion || !record.NoticeAsksKept || record.Notify == nil ||
		record.Notify.Snapshot.CommentAsks == nil {
		return nil
	}
	return boolInt(*record.Notify.Snapshot.CommentAsks)
}
