package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The derived columns: values the applier COMPUTES from the rows it already
// holds rather than copies out of a record.
//
// Six today, and the rule all follow is [statelog.Deriver]'s: maintained
// INCREMENTALLY by the apply, and REDERIVED by [Applier.Rederive] the first
// time a build whose rules differ from the checkpoint's boots — in this Go,
// never a second copy in a migration's SQL.
//
//   - A task's `reopens`, maintained as each task history row is written, off
//     the status delta that row records. "Backfill equals replay" is true by
//     construction rather than by two implementations agreeing: both read one
//     delta per applied status change through the one predicate, [reopened].
//   - A person's POSITIONS — `seen_through` and the position every inbox
//     entry carries — see [rederivePersons].
//   - A project's `active_count`, maintained beside the three status buckets
//     by [Applier.maintainProjectCounts] and recounted from the task rows by
//     [rederiveActiveCounts].
//   - A task history row's `reassignments`, the hand-off count that commit
//     left, carried forward in log order as each row is written and walked
//     again by [rederiveHandOffs] — see apply_handoffs.go.
//   - A task's `updated_at`, the effective instant of the newest record that
//     changed it — the `effective_at` of its newest history row — written by
//     [Applier.applyTask] and recomputed by [restampUpdated].
//   - WHO a commit's notification rows are written for: every candidate but
//     the person who made the change ([Applier.writeInbox]), which
//     [rederiveOwnNotices] restores over rows written before that rule.

// DerivationVersion is the rule set this build derives its columns at.
//
// 1 is `reopens`. 2 is a person's positions stored PACKED, generation in the
// high bits: `seen_through` had been written as the bare sequence and every
// inbox entry's position was whatever the caller sent. 3 is a project's
// `active_count`, the part of its open work somebody has started. 4 is a task
// history row's `reassignments`, the hand-off count that commit left. 5 is a
// task's `updated_at`, which only its create had ever written. 6 is the
// notification rows written for everybody a commit concerned BUT its own
// author, who had been told about their own change in their own inbox. Bumped
// by ANY change to what a derived column holds, the first one a column adds
// included — the bump is what fills a new column on a node upgrading onto rows
// its predecessor wrote without it.
const DerivationVersion = 6

var _ statelog.Deriver = (*Applier)(nil)

// DerivationVersion implements [statelog.Deriver].
func (a *Applier) DerivationVersion() int { return DerivationVersion }

// reopened is THE rule: a status change that takes a task out of a finished
// group (done or closed) into an unfinished one. Done → closed is not a
// reopen — the work stayed finished — and neither is todo → in progress,
// which never finished at all.
//
// BY GROUP, from the fixed status table, for the reason [stampFinish] cuts on
// the group: a company that adds `shipped` or renames `done` must not change
// what a reopen is. An unknown slug is in no group and so is never finished,
// which reads a status this build does not know as unfinished on BOTH sides —
// the same answer the history row's delta gives it on every node.
func reopened(from, to Status) bool {
	return from.Group().Finished() && !to.Group().Finished()
}

// countReopen adds one to a task's reopen counter when the history row this
// apply has just written records a reopen.
//
// OFF THE ROW'S OWN `fields_json`, the exact bytes [Applier.Rederive] reads
// back — never off the two documents again. The history row is the one record
// of a status change both halves can see, and it is written in cases the
// object row is not (a record reprocessed below an applied successor writes
// its history row and skips the object), so a rule keyed anywhere else would
// count on one side what the other never sees.
func countReopen(ctx context.Context, tx *sql.Tx, subject Subject, fields string) (int, error) {
	if subject.Kind != KindTask {
		return 0, nil
	}
	var moved struct {
		Status *Delta `json:"status"`
	}
	if err := json.Unmarshal([]byte(fields), &moved); err != nil {
		return 0, fmt.Errorf("tracker: read the deltas of task %s for a reopen: %w",
			subject.ID, err)
	}
	if moved.Status == nil || !reopened(Status(moved.Status.From), Status(moved.Status.To)) {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tracker_tasks SET reopens = reopens + 1 WHERE id = ?`, subject.ID); err != nil {
		return 0, fmt.Errorf("tracker: count the reopen of task %s: %w", subject.ID, err)
	}
	return 1, nil
}

// Rederive implements [statelog.Deriver]: every derived column recomputed from
// the rows this transaction holds — a task's `reopens`, a person's positions,
// a project's `active_count`, each task history row's hand-off count, a task's
// `updated_at`, and which of a commit's notification rows are its author's own.
func (a *Applier) Rederive(ctx context.Context, tx *sql.Tx, _ statelog.ApplyOptions) (int, error) {
	reopens, err := rederiveReopens(ctx, tx)
	if err != nil {
		return 0, err
	}
	persons, err := rederivePersons(ctx, tx)
	if err != nil {
		return 0, err
	}
	active, err := rederiveActiveCounts(ctx, tx)
	if err != nil {
		return 0, err
	}
	handOffs, err := rederiveHandOffs(ctx, tx)
	if err != nil {
		return 0, err
	}
	updated, err := restampUpdated(ctx, tx, "")
	if err != nil {
		return 0, err
	}
	own, err := rederiveOwnNotices(ctx, tx)
	if err != nil {
		return 0, err
	}
	return reopens + persons + active + handOffs + updated + own, nil
}

// rederiveOwnNotices removes the notification rows a predecessor wrote for the
// person who made the change — see [Applier.writeInbox] for the rule and what
// breaking it cost.
//
// THE SAME TEST THE WRITE MAKES, over what the history row kept of the record:
// the actor and the seat a bound token wrote for, which are exactly the two
// names [MutationRecord.ActorParty] holds, and the one reason that is news to
// the actor ([ReasonUnblocked], [Reason.WakesActor]) left alone. A row this
// build wrote matches nothing, so an upgrade with nothing to repair reports
// none.
func rederiveOwnNotices(ctx context.Context, tx *sql.Tx) (int, error) {
	res, err := tx.ExecContext(ctx, `
		DELETE FROM tracker_notifications
		WHERE reason <> ?
		  AND EXISTS (
		      SELECT 1 FROM tracker_history h
		      WHERE h.id = tracker_notifications.record_id
		        AND (h.actor = tracker_notifications.recipient
		             OR (h.actor_seat <> '' AND h.actor_seat = tracker_notifications.recipient)))`,
		string(ReasonUnblocked))
	if err != nil {
		return 0, fmt.Errorf("tracker: remove the notices written for their own author: %w", err)
	}
	return affected(res)
}

// restampUpdated is every task's `updated_at` recomputed as the newest
// `effective_at` among its own history rows — every task's when id is empty,
// and the one task's otherwise.
//
// THE SAME INSTANT THE INCREMENTAL RULE WRITES: [Applier.applyTask] stamps the
// task with [effectiveAt] at the record it applies, and the history row that
// apply writes carries that same value — so the newest history row IS the
// newest stamp, and a row this build maintained re-derives to itself. Only
// rows that differ are written, which is how an upgrade with nothing to repair
// reports none.
//
// ONE TASK is the late record's half of the same rule. A record applied below
// the task's version changes no document, but its history row still RAISES
// every successor's effective instant (see [Applier.raiseSuccessors]) — so
// the task's newest instant moved while its stamp did not, and a node that
// applied the same records in log order would hold the raised one. Restamping
// that task from its history is what keeps the two nodes identical.
//
// THE DOCUMENT MOVES WITH THE COLUMN. A task page reads the document and a
// list reads the column, and a repair of one would draw two different "last
// changed" instants for one task — so both are rewritten, in Go, because the
// document's instant is RFC 3339 text and the column's is the store's integer.
func restampUpdated(ctx context.Context, tx *sql.Tx, id string) (int, error) {
	// The one task's filter sits INSIDE the aggregate, so a late record
	// reads its own history rows through the subject index rather than
	// grouping the company's whole history to keep one group of it.
	scope, args := "", []any{string(KindTask)}
	if id != "" {
		scope, args = " AND subject_id = ?", append(args, id)
	}
	query := `
		SELECT t.id, t.document, h.newest
		FROM tracker_tasks t
		JOIN (SELECT subject_id, MAX(effective_at) AS newest
		        FROM tracker_history WHERE subject_kind = ?` + scope + `
		       GROUP BY subject_id) h ON h.subject_id = t.id
		WHERE h.newest > 0 AND t.updated_at <> h.newest`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("tracker: read the tasks whose last change moved: %w", err)
	}
	type repair struct {
		id       string
		document []byte
		newest   int64
	}
	var repairs []repair
	for rows.Next() {
		var r repair
		if err := rows.Scan(&r.id, &r.document, &r.newest); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("tracker: read a task's last change: %w", err)
		}
		repairs = append(repairs, r)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("tracker: read the tasks whose last change moved: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("tracker: read the tasks whose last change moved: %w", err)
	}
	for _, r := range repairs {
		var task Task
		if err := json.Unmarshal(r.document, &task); err != nil {
			return 0, fmt.Errorf("tracker: decode task %s to stamp its last change: %w", r.id, err)
		}
		task.UpdatedAt = store.DecodeTime(r.newest).UTC()
		document, err := json.Marshal(task)
		if err != nil {
			return 0, fmt.Errorf("tracker: encode task %s: %w", r.id, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tracker_tasks SET updated_at = ?, document = ? WHERE id = ?`,
			r.newest, document, r.id); err != nil {
			return 0, fmt.Errorf("tracker: stamp task %s's last change: %w", r.id, err)
		}
	}
	return len(repairs), nil
}

// rederiveActiveCounts is every project's `active_count` recounted from its
// task rows.
//
// A COUNT OVER THE ROWS rather than a replay of history, because the census is
// a function of what the rows hold NOW, and the rows are exactly what the
// incremental rule was maintaining it against: `status_group` and `removed_at`
// are written by the task apply from the same two facts [activeOf] reads, so
// "active and not removed" here and there cannot disagree. A purged task has
// no row, which is the purge's own decrement.
//
// ONE STATEMENT over every project, WRITING ONLY THE ONES THAT DIFFER, so a
// re-derivation over rows this build maintained rewrites nothing — which is
// how [Applier.Rederive] reports an upgrade with nothing to repair. A
// company's projects are tens; the count seeks each through the tasks'
// project index.
func rederiveActiveCounts(ctx context.Context, tx *sql.Tx) (int, error) {
	const recount = `(SELECT COUNT(*) FROM tracker_tasks t
		WHERE t.project_key = tracker_projects.key
		  AND t.status_group = ? AND t.removed_at IS NULL)`
	res, err := tx.ExecContext(ctx,
		`UPDATE tracker_projects SET active_count = `+recount+
			` WHERE active_count <> `+recount,
		string(GroupActive), string(GroupActive))
	if err != nil {
		return 0, fmt.Errorf("tracker: recount the active work of every project: %w", err)
	}
	return affected(res)
}

// rederiveReopens is every task's `reopens` recomputed from the status deltas
// in its history rows.
//
// ORDER-FREE, because a count of transitions does not depend on the order it
// is taken in — each history row records one change with both of its ends, so
// the walk needs no sort and no per-task state. The rows are read whole and
// the counters written after, because a statement cannot be issued on a
// transaction while a query on it is still being read.
//
// A PURGED task has no row left to write, and its history rows are skipped
// with it: the UPDATE of an id with no row affects nothing.
func rederiveReopens(ctx context.Context, tx *sql.Tx) (int, error) {
	counts, err := reopensFromHistory(ctx, tx)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE tracker_tasks SET reopens = 0 WHERE reopens <> 0`)
	if err != nil {
		return 0, fmt.Errorf("tracker: clear the reopen counters to re-derive them: %w", err)
	}
	cleared, err := affected(res)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	// SORTED so the writes are issued in one order on every node; the
	// result does not depend on it, but a re-derivation that is
	// deterministic down to its statements is one a failure reproduces.
	slices.Sort(ids)
	written := 0
	for _, id := range ids {
		res, err := tx.ExecContext(ctx,
			`UPDATE tracker_tasks SET reopens = ? WHERE id = ?`, counts[id], id)
		if err != nil {
			return 0, fmt.Errorf("tracker: write the re-derived reopens of task %s: %w", id, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return max(cleared, written), nil
}

// reopensFromHistory counts each task's reopens over its history rows.
func reopensFromHistory(ctx context.Context, tx *sql.Tx) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT subject_id, json_extract(fields_json, '$.status')
		FROM tracker_history
		WHERE subject_kind = ? AND json_extract(fields_json, '$.status') IS NOT NULL`,
		string(KindTask))
	if err != nil {
		return nil, fmt.Errorf("tracker: read the status history to re-derive reopens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("tracker: scan a status history row: %w", err)
		}
		var status Delta
		if err := json.Unmarshal(raw, &status); err != nil {
			return nil, fmt.Errorf("tracker: decode the status delta of task %s: %w", id, err)
		}
		if reopened(Status(status.From), Status(status.To)) {
			counts[id]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the status history to re-derive reopens: %w", err)
	}
	return counts, nil
}

// rederivePersons re-derives every person row's positions, packed.
//
// # Why these are derived columns
//
// The generation of a position used to be lost in two places. The applier
// wrote `seen_through` as the bare sequence, and every inbox entry carried
// whatever position its caller sent — a bare sequence at best, zero at worst.
// Both are now packed (`(generation << 40) | seq`), and a row an older applier
// wrote is not: after a reanchor its position reads as generation zero, below
// every notice in the live generation, and an entry's stale position is pruned
// against the packed one on the next write although the notice it marks is
// still above it — a notice marked read coming back unread.
//
// # From the rows, never from a guess
//
//   - `seen_through` comes from the DOCUMENT of the history row that wrote this
//     person row: the row's `version` is that record's packed position, and
//     the history row stores the record's own payload beside the same
//     position. It is exactly what the current applier stores from that
//     record, which is what makes a re-derivation at P maintained to Q equal
//     one at Q. Where that record itself carried generation zero — an older
//     writer that read the lossy row back and restated it — there is nothing
//     truer on this node to recover, and the person's next `read_through`
//     restores it, since an advance compares generation first.
//   - An entry's position comes from the history row of the notice it names,
//     which is where [Writer.MarkInbox] reads it inside the decide. An entry
//     naming no recorded change keeps what it holds: nothing here knows
//     better.
//
// Ordered by handle, and written after the read for the reason
// [rederiveReopens] gives.
func rederivePersons(ctx context.Context, tx *sql.Tx) (int, error) {
	type personRow struct {
		handle                string
		seen                  int64
		document              []byte
		read, unread, snoozed []byte
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT p.handle, p.seen_through, h.document, p.read_json,
		       p.unread_json, p.snoozed_json
		FROM tracker_persons p
		LEFT JOIN tracker_history h
		       ON h.subject_kind = ? AND h.subject_id = p.handle
		      AND h.log_seq = p.version
		ORDER BY p.handle`, string(KindPerson))
	if err != nil {
		return 0, fmt.Errorf("tracker: read the person rows to re-derive their positions: %w", err)
	}
	var persons []personRow
	for rows.Next() {
		var row personRow
		if err := rows.Scan(&row.handle, &row.seen, &row.document, &row.read,
			&row.unread, &row.snoozed); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("tracker: scan a person row: %w", err)
		}
		persons = append(persons, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("tracker: read the person rows to re-derive their positions: %w", err)
	}
	_ = rows.Close()

	written := 0
	for _, row := range persons {
		seen := row.seen
		if len(row.document) > 0 {
			var person Person
			if err := decodePayload(row.document, &person); err != nil {
				return 0, fmt.Errorf("tracker: decode the record that wrote %s's row: %w",
					row.handle, err)
			}
			seen = int64(person.SeenThrough.packed())
		}
		lists := make([]string, 0, 3)
		for _, body := range [][]byte{row.read, row.unread, row.snoozed} {
			list, err := entryPositions(ctx, tx, row.handle, body)
			if err != nil {
				return 0, err
			}
			lists = append(lists, list)
		}
		if seen == row.seen && lists[0] == string(row.read) &&
			lists[1] == string(row.unread) && lists[2] == string(row.snoozed) {
			continue
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE tracker_persons SET seen_through = ?, read_json = ?,
			       unread_json = ?, snoozed_json = ?
			WHERE handle = ?`, seen, lists[0], lists[1], lists[2], row.handle)
		if err != nil {
			return 0, fmt.Errorf("tracker: write %s's re-derived positions: %w", row.handle, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// entryPositions is one stored inbox list with every entry's position read
// from the history row of the notice it names, as the column text.
//
// THE STORED TEXT BACK UNCHANGED when no entry moves, so a row this build
// wrote compares equal and is not rewritten.
func entryPositions(ctx context.Context, tx *sql.Tx, handle string, body []byte) (string, error) {
	if len(body) == 0 {
		return string(body), nil
	}
	var entries []InboxEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return "", fmt.Errorf("tracker: decode %s's inbox list to re-derive it: %w", handle, err)
	}
	moved := false
	for i, entry := range entries {
		var packed int64
		switch err := tx.QueryRowContext(ctx,
			`SELECT log_seq FROM tracker_history WHERE id = ?`, entry.RecordID).
			Scan(&packed); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return "", fmt.Errorf("tracker: read where record %s sits: %w",
				entry.RecordID, err)
		}
		if uint64(packed) != entry.Position {
			entries[i].Position, moved = uint64(packed), true
		}
	}
	if !moved {
		return string(body), nil
	}
	return jsonOf(nilIfEmpty(entries)), nil
}
