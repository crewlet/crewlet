package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The history row, the effective instant, the spans and the inbox — the four
// things every commit produces besides its own object's row.
//
// # A QUIET COMMIT WRITES A HISTORY ROW LIKE EVERY OTHER
//
// "Quiet" means one thing and one thing only: it wakes nobody. The activity
// feed is therefore a complete account of what happened rather than an account
// of what was announced, and `notified` is how a reader tells "nothing was
// announced" from "nothing happened".

// writeHistory writes the record's history row and everything that hangs off
// it.
//
// EVERYTHING DERIVED HANGS OFF THIS WRITE RATHER THAN OFF THE OBJECT ROW, and
// the reason is the version guard: a reprocessed record whose position is
// below an applied successor is SKIPPED on the object row, so a recompute
// hanging there would leave that record out of the spans entirely. A missing
// span is strictly worse than a slightly wrong instant.
func (a *Applier) writeHistory(ctx context.Context, tx *sql.Tx, c applyContext,
	projectKey string, applied map[string]Delta) (int, error) {

	subject := c.subject()
	effective, err := effectiveAt(ctx, tx, subject.ID, c)
	if err != nil {
		return 0, err
	}
	notify := c.record.Notify
	kind, excerpt, fields, commentID := "", "", "{}", ""
	notified := 0
	late := 0
	if notify != nil {
		kind = string(notify.Kind)
		excerpt = notify.Excerpt
		commentID = notify.CommentID
		notified = 1
		late = boolInt(notify.Late)
	} else {
		// A QUIET COMMIT STILL NAMES WHAT IT DID. The kind falls back to
		// what actually moved, and then to the operation, because a
		// history row whose kind was empty would be a row no filter can
		// select and no card can render — which is indistinguishable
		// from a commit that never happened.
		kind = string(quietKind(applied, c.record.Op))
	}
	// THE DELTAS ARE THE APPLIER'S, on every commit, loud or quiet.
	//
	// They used to be the NOTIFICATION's, which made "what changed" a
	// property of what was ANNOUNCED: a quiet status change wrote `{}`
	// here and produced no span, so every report derived from the spans
	// silently omitted it. The two are the same function of the same two
	// documents — see [TaskDeltas] — so nothing is lost by taking the
	// applier's, and what is gained is that a record nobody was told
	// about is still a record of what happened.
	if len(applied) > 0 {
		fields = jsonOf(applied)
	} else if notify != nil {
		// A COMMENT, A MENTION OR AN ASK carries fields no document
		// comparison can produce, so the notification's own are kept
		// wherever the apply found nothing to compare.
		fields = jsonOf(notify.Fields)
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_history
			(id, subject_kind, subject_id, project_key, kind, actor, actor_kind,
			 operator_id, comment_id, batch_id, excerpt, fields_json, turn_id,
			 notified, late, log_seq, log_stream, log_generation, created_at,
			 broker_at, effective_at, skew_ms, document)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO NOTHING`,
		historyID(c), string(subject.Kind), subject.ID, projectKey, kind,
		c.record.Actor, string(c.record.ActorKind), c.record.OperatorID,
		commentID, batchOf(c.record), excerpt, fields, c.record.TurnID,
		notified, late, c.packed, c.position.Stream, c.position.Generation,
		store.EncodeTime(c.authored()), store.EncodeTime(c.brokerAt),
		store.EncodeTime(effective), c.skewMs(), []byte(c.record.Mutation))
	if err != nil {
		return 0, fmt.Errorf("tracker: write the history row at %s: %w", c.position, err)
	}
	rows, err := affected(res)
	if err != nil {
		return 0, err
	}
	if rows == 0 {
		// A REDELIVERY. Everything below is derived from this row, so
		// recomputing it would be work with no effect — and the
		// notification rows are keyed on the record too.
		return 0, nil
	}

	successors, err := a.raiseSuccessors(ctx, tx, subject.ID, c)
	if err != nil {
		return 0, err
	}
	inbox, err := a.writeInbox(ctx, tx, c)
	if err != nil {
		return 0, err
	}
	return rows + successors + inbox, nil
}

// raiseSuccessors re-derives the effective instant of every row ABOVE this
// one.
//
// # Why a late record can only RAISE them, and why that is the whole argument
//
// The effective instant is a MAX over a set: every row about this subject at a
// position at or below its own. A record arriving late adds one element to
// every successor's set, so each successor's max either stays where it was or
// moves UP — never down. That is what makes the recompute confluent: two nodes
// applying the same records in different orders reach the same value, because
// a maximum does not care what order it saw its inputs in.
//
// A FOLD OVER ARRIVAL ORDER would not have this property, and the failure is
// not theoretical: the two readers of this value compare it against each other,
// so a divergence makes a repair duty fire on one node for ever and never on
// its peer.
func (a *Applier) raiseSuccessors(ctx context.Context, tx *sql.Tx, subjectID string,
	c applyContext) (int, error) {

	broker := store.EncodeTime(c.brokerAt)
	var rows int
	for _, statement := range []struct{ sql, column string }{
		{`UPDATE tracker_history SET effective_at = MAX(effective_at, ?)
		  WHERE subject_id = ? AND log_seq > ? AND effective_at < ?`, "history"},
		{`UPDATE tracker_turns SET effective_at = MAX(effective_at, ?)
		  WHERE task_id = ? AND log_seq > ? AND effective_at < ?`, "turns"},
	} {
		res, err := tx.ExecContext(ctx, statement.sql, broker, subjectID, c.packed, broker)
		if err != nil {
			return 0, fmt.Errorf("tracker: raise the %s successors of %s: %w",
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

// recomputeSpans rebuilds a task's status spans WHOLESALE from its history.
//
// Wholesale rather than incrementally, and sorted by the COMPOSED POSITION
// rather than by any clock, which is what makes a late reprocess and a
// redelivery both no-ops: the input is a set and an order the log itself
// defines, so the output is a function of what has been applied and nothing
// else. Both endpoints are the EFFECTIVE instant, which is also what makes a
// duration non-negative — an authored clock can run backwards between two
// nodes and a max over broker instants cannot.
func (a *Applier) recomputeSpans(ctx context.Context, tx *sql.Tx, taskID string) (int, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_status_spans WHERE task_id = ?`, taskID); err != nil {
		return 0, fmt.Errorf("tracker: clear the spans of %s: %w", taskID, err)
	}
	// THE ROW'S OWN STATUS DELTA DECIDES, not its kind.
	//
	// A row CARRYING a status delta is a status change, whatever it was
	// filed under — and the kind cannot be trusted for this: a quiet
	// commit's kind is the operation, and a loud one's is whichever single
	// word the writer chose to announce, so a patch that moved the status
	// AND the assignee is filed under `assignee` and would have been
	// invisible here. The predicate is over the same rows the subject
	// index already selects, so it costs nothing beyond them.
	rows, err := tx.QueryContext(ctx, `
		SELECT h.id, h.effective_at,
		       json_extract(h.fields_json, '$.status.to') AS status
		FROM tracker_history h
		WHERE h.subject_id = ?
		  AND json_extract(h.fields_json, '$.status.to') IS NOT NULL
		ORDER BY h.log_seq`, taskID)
	if err != nil {
		return 0, fmt.Errorf("tracker: read the status history of %s: %w", taskID, err)
	}
	type entry struct {
		record string
		at     int64
		status string
	}
	var entries []entry
	for rows.Next() {
		var e entry
		var status sql.NullString
		if err := rows.Scan(&e.record, &e.at, &status); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("tracker: read a status row of %s: %w", taskID, err)
		}
		e.status = status.String
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("tracker: walk the status history of %s: %w", taskID, err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("tracker: close the status history of %s: %w", taskID, err)
	}

	written := 0
	for i, e := range entries {
		status := Status(e.status)
		if !status.Valid() {
			// A status this build does not know cannot be grouped, and
			// a span with no group is a span no report can read. It is
			// skipped rather than guessed, and the record that wrote it
			// is still in the history for a later build to re-derive
			// from.
			continue
		}
		var left any
		if i+1 < len(entries) {
			left = entries[i+1].at
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tracker_status_spans
				(task_id, record_id, status, grp, entered_at, left_at,
				 project_key, sprint_number)
			SELECT ?, ?, ?, ?, ?, ?, t.project_key, t.sprint_number
			FROM tracker_tasks t WHERE t.id = ?`,
			taskID, e.record, string(status), string(status.Group()), e.at, left,
			taskID); err != nil {
			return 0, fmt.Errorf("tracker: write a span of %s: %w", taskID, err)
		}
		written++
	}
	// AND THE TASK'S OWN ENTERED INSTANT IS THE OPEN SPAN'S START. It is
	// rewritten here rather than on the object row for the reason the whole
	// recompute hangs here: a reprocessed record below an applied successor
	// is skipped on the object row.
	if len(entries) > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tracker_tasks SET status_entered_at = ? WHERE id = ?`,
			entries[len(entries)-1].at, taskID); err != nil {
			return 0, fmt.Errorf("tracker: stamp the entered instant of %s: %w",
				taskID, err)
		}
		written++
	}
	return written, nil
}

// InboxRetentionDefaultDays is how long a person's inbox rows are kept.
//
// THE ONE HORIZON THAT DELETES A ROW DERIVED FROM A RECORD, and it is a
// MAILBOX rather than a record: the history those rows were derived from is
// untouched and answers for ever. A year, against the vendor's three months
// uncleared, because an inbox row costs almost nothing and a founder scrolling
// back a year is a thing that happens.
const InboxRetentionDefaultDays = 365

// writeInbox writes one row per candidate the commit concerned.
//
// EVERY CANDIDATE, not every recipient: the row is the complete record of who
// the change concerned, and the WAKE goes only to the people still employed —
// which is a decision the routing filter makes, later, with a registry this
// applier deliberately does not have.
//
// The retention horizon is applied AROUND the candidate computation rather
// than inside it, which is what keeps that function free of a clock: a record
// older than the horizon writes no inbox rows at all, so a whole-log replay
// rebuilds no expired inbox.
func (a *Applier) writeInbox(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {

	notify := c.record.Notify
	if notify == nil {
		// A NIL GUARD, not the rule. That a quiet commit concerns nobody
		// is a property of [Candidates] and is asserted there; this is
		// what stops the loop below dereferencing a notification that is
		// not there. Both are cheap and only one of them is the place
		// the property lives.
		return 0, nil
	}
	if horizon := inboxHorizon(c.epoch); horizon > 0 && !c.brokerAt.IsZero() {
		// A RECORD OLDER THAN THE HORIZON WRITES NO INBOX ROWS AT ALL,
		// which is what makes a whole-log replay rebuild no expired
		// inbox — the alternative, sweeping afterwards, means every
		// replay writes a year of rows and then deletes them.
		if c.authored().Before(c.brokerAt.Add(-horizon)) {
			return 0, nil
		}
	}
	candidates := Candidates(notify, c.record.Batched())
	written := 0
	for _, candidate := range candidates {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tracker_notifications
				(record_id, recipient, subject_id, subject_key, kind, reason,
				 addressed, fallback_only, fallback_rank, excerpt, created_at,
				 log_seq, log_stream, log_generation)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (record_id, recipient) DO NOTHING`,
			historyID(c), candidate.Handle, c.subject().ID, notify.Snapshot.Key,
			string(notify.Kind), string(candidate.Reason),
			boolInt(candidate.Addressed), boolInt(candidate.FallbackOnly),
			candidate.FallbackRank, notify.Excerpt,
			store.EncodeTime(c.authored()), c.packed, c.position.Stream,
			c.position.Generation)
		if err != nil {
			return 0, fmt.Errorf("tracker: write the inbox row for %s at %s: %w",
				candidate.Handle, c.position, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// inboxHorizon reads the retention days out of the epoch the framework passed
// in.
//
// FROM THE EPOCH RATHER THAN FROM CONFIG, because the applier reads no
// configuration at all: two nodes briefly on different epochs may write
// different inbox rows, which is precisely why that table is classed as
// diverging rather than compared by the identity claim.
func inboxHorizon(epoch map[string]any) time.Duration {
	days := InboxRetentionDefaultDays
	if raw, held := epoch["tracker.native.inbox_retention_days"]; held {
		switch v := raw.(type) {
		case int:
			days = v
		case int64:
			days = int(v)
		case float64:
			days = int(v)
		}
	}
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

// historyID is the history row's key: the record's own operation id when it
// has one, else its position.
//
// THE POSITION IS THE FALLBACK RATHER THAN A NEW UUID, because a generated id
// would differ between two nodes applying the same record — and the inbox rows
// are keyed on this value, so two nodes would write two inboxes for one change.
func historyID(c applyContext) string {
	if c.record.OpID != "" {
		return c.record.OpID
	}
	return fmt.Sprintf("%s#%d", c.position.Stream, c.packed)
}

// batchOf answers the batch id or nil, so the column distinguishes "not part
// of a batch" from "part of a batch with an empty name".
func batchOf(rec MutationRecord) any {
	if rec.BatchID == nil {
		return nil
	}
	return *rec.BatchID
}

// quietKind is what a commit nobody was told about is filed under.
//
// BY WHAT MOVED, in a fixed precedence, because the kind is what every feed
// filter selects on and a whole class of change filed under the bare operation
// is a class no filter can reach. The order puts `status` first for the same
// reason the spans read the delta: it is the change other tables are derived
// from.
func quietKind(applied map[string]Delta, op OpKind) ChangeKind {
	if op == OpCreate {
		// A CREATE IS A CREATE, whatever it set on the way in: every
		// field moves from empty on a create, so deciding by what moved
		// would file every new task under the first field in the
		// precedence.
		return ChangeCreated
	}
	for _, moved := range []struct {
		field string
		kind  ChangeKind
	}{
		{"status", ChangeStatus},
		{"assignee", ChangeAssignee},
		{"project", ChangeMoved},
		{"priority", ChangeFields},
		{"title", ChangeFields},
		{"type", ChangeFields},
		{"tags", ChangeFields},
	} {
		if _, changed := applied[moved.field]; changed {
			return moved.kind
		}
	}
	return ChangeKind(op)
}
