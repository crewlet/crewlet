package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The history row, the effective instant, the entered stamp and the inbox —
// the four things every commit produces besides its own object's row.
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
	// THE KIND IS THE RECORD'S OWN, and it is a different fact from
	// whether anybody was told.
	//
	// It used to be the NOTIFICATION's, which made the feed's vocabulary a
	// property of whether the change had an audience: the same catalogue
	// edit filed as `catalogue_updated` when somebody heard about it and
	// as `patch` when nobody did — and a quiet purge as `purge`, which is
	// not a [ChangeKind] at all, so `kinds=purged` could never find it.
	// See [MutationRecord.Kind].
	//
	// The ladder has three rungs and each one is reachable. The record's
	// own kind is what this build's writers state. The notification's is
	// what a record from the build BEFORE this field carries, which a
	// rolling upgrade makes ordinary traffic. And [fallbackKind] is for a
	// record from that same older build that was quiet — the only case
	// where nothing on the record says what it did, and a guess from the
	// operation is all there is.
	kind := string(c.record.Kind)
	switch {
	case c.record.Kind.Valid():
	case notify != nil:
		kind = string(notify.Kind)
	default:
		kind = string(fallbackKind(applied, c.record.Op))
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
		historyID(c), string(subject.Kind), subject.ID, keys.Project, kind,
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
	inbox, err := a.writeInbox(ctx, tx, c, keys.Key)
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

// stampStatusEntered rewrites a task's `status_entered_at` from its history.
//
// # Why the history rather than the record in hand
//
// SORTED BY THE COMPOSED POSITION rather than by any clock, which is what
// makes a late reprocess and a redelivery both no-ops: the input is an order
// the log itself defines, so the output is a function of what has been applied
// and nothing else. The instant is the EFFECTIVE one, which is what keeps a
// duration measured against this column non-negative — an authored clock can
// run backwards between two nodes and a max over broker instants cannot.
//
// It is rewritten HERE rather than on the object row because a reprocessed
// record below an applied successor is skipped on the object row, and the
// column would then hold the instant of whichever record happened to be
// applied last rather than of the newest status change.
//
// # Why only the newest row is read
//
// This derived the whole `tracker_status_spans` table until migration 0014
// dropped it — one DELETE, a walk of the task's entire status history and one
// INSERT per historical status change, to maintain rows nothing selected from.
// What survived that table is this single column, and the column is the NEWEST
// status change's instant, so the read is one seek on
// `tracker_history_subject_idx` rather than a walk. The rows the spans held
// are still derivable: they were a function of `tracker_history`, which is
// never swept.
//
// THE STATUS'S OWN VALIDITY IS NOT CHECKED, deliberately and unlike the spans
// this replaced. A span with no group was a span no report could read, so an
// unknown status was skipped; this column is an INSTANT, it needs no group,
// and skipping would leave it naming an older change than the one the task
// actually last made.
func (a *Applier) stampStatusEntered(ctx context.Context, tx *sql.Tx, taskID string) (int, error) {
	var at int64
	switch err := tx.QueryRowContext(ctx, `
		SELECT h.effective_at
		FROM tracker_history h
		WHERE h.subject_id = ?
		  AND json_extract(h.fields_json, '$.status.to') IS NOT NULL
		ORDER BY h.log_seq DESC
		LIMIT 1`, taskID).Scan(&at); {
	case errors.Is(err, sql.ErrNoRows):
		// NO STATUS CHANGE IN THE HISTORY AT ALL, which is not an error:
		// a task whose every applied record was quiet about its status
		// has nothing to stamp, and the column keeps what it holds.
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("tracker: read the newest status of %s: %w", taskID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tracker_tasks SET status_entered_at = ? WHERE id = ?`,
		at, taskID); err != nil {
		return 0, fmt.Errorf("tracker: stamp the entered instant of %s: %w",
			taskID, err)
	}
	return 1, nil
}

// InboxRetentionDefaultDays is how long a person's inbox rows are kept.
//
// THE ONE HORIZON THAT DELETES A ROW DERIVED FROM A RECORD, and it is a
// MAILBOX rather than a record: the history those rows were derived from is
// untouched and answers for ever. A year, against the vendor's three months
// uncleared, because an inbox row costs almost nothing and a founder scrolling
// back a year is a thing that happens.
const InboxRetentionDefaultDays = 365

// subjectKeys is what the applier already read off the object row, named so
// the two are not passed as two bare strings a call site can swap.
//
// BOTH COME FROM THE ROW THE APPLIER READ, which is the whole point: `Project`
// is "ENG" and `Key` is "ENG-1", and neither is something a writer can state
// on a CREATE — the item key is minted by the write itself, from the project's
// own counter. A record's notification therefore cannot carry it, and for as
// long as the inbox row took the writer's snapshot every creation in every
// company wrote a notice whose subject was a uuid. [InboxNotice.SubjectKey]
// says what it is for: "an inbox of uuids is an inbox nobody reads."
//
// Zero for a subject that has no item key — a project, a view, a person —
// which is the honest value rather than a missing one.
type subjectKeys struct{ Project, Key string }

// inboxSubjectKey is the key a notice carries.
//
// THE APPLIER'S ROW WINS over the writer's snapshot, for the reason the field
// exists: the applier read the object in this transaction and the snapshot is
// a claim formed before the write, so on a rename the two differ and only one
// of them is what the item is called now. The snapshot is the fallback for a
// subject the applier holds no row for, where it is the only thing anybody
// knows.
func inboxSubjectKey(fromRow string, notify *Notify) string {
	if fromRow != "" {
		return fromRow
	}
	return notify.Snapshot.Key
}

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
func (a *Applier) writeInbox(ctx context.Context, tx *sql.Tx, c applyContext,
	subjectKey string) (int, error) {

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
			historyID(c), candidate.Handle, c.subject().ID,
			inboxSubjectKey(subjectKey, notify),
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

// fallbackKind is what a record that names no kind and carries no notification
// is filed under.
//
// # It is the compatibility rung, and it used to be the ordinary one
//
// Every record this build writes states its own kind ([MutationRecord.Kind]),
// so this is reached only for a record an older build wrote QUIETLY — which a
// rolling upgrade makes real traffic for as long as one takes, and never
// after. It stays for exactly that window and is the only thing that can ever
// read such a row.
//
// BY WHAT MOVED, in a fixed precedence, because the kind is what every feed
// filter selects on. The order puts `status` first for the same reason the
// spans read the delta: it is the change other tables are derived from.
//
// # AND IT ANSWERS ONLY IN [ChangeKind]s, which is the half that was wrong
//
// It used to end at `ChangeKind(op)` — the OPERATION, cast. That is a
// different vocabulary: `patch`, `tombstone`, `restore` and `purge` are
// [OpKind]s, none of them is a valid [ChangeKind], and three of them are
// near-misses of one (`removed`, `restored`, `purged`). So a quiet removal
// filed as `tombstone` and `kinds=removed` did not find it — a filter looking
// at the right word for a row written under the wrong one, with nothing on
// either side to say so.
func fallbackKind(applied map[string]Delta, op OpKind) ChangeKind {
	switch op {
	case OpCreate:
		// A CREATE IS A CREATE, whatever it set on the way in: every
		// field moves from empty on a create, so deciding by what moved
		// would file every new task under the first field in the
		// precedence.
		return ChangeCreated
	case OpTombstone:
		return ChangeRemoved
	case OpRestore:
		return ChangeRestored
	case OpPurge:
		return ChangePurged
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
	// A PATCH THAT MOVED NOTHING THIS LIST NAMES IS A FIELD EDIT, which
	// is both true and nameable — where the operation was neither.
	return ChangeFields
}
