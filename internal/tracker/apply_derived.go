package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The derived columns: values the applier COMPUTES from the rows it already
// holds rather than copies out of a record.
//
// Two today, and the rule both follow is [statelog.Deriver]'s: maintained
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

// DerivationVersion is the rule set this build derives its columns at.
//
// 1 is `reopens`. 2 is a person's positions stored PACKED, generation in the
// high bits: `seen_through` had been written as the bare sequence and every
// inbox entry's position was whatever the caller sent. Bumped by ANY change to
// what a derived column holds, the first one a column adds included — the bump
// is what fills a new column on a node upgrading onto rows its predecessor
// wrote without it.
const DerivationVersion = 2

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
// the rows this transaction holds — a task's `reopens` and a person's
// positions.
func (a *Applier) Rederive(ctx context.Context, tx *sql.Tx, _ statelog.ApplyOptions) (int, error) {
	reopens, err := rederiveReopens(ctx, tx)
	if err != nil {
		return 0, err
	}
	persons, err := rederivePersons(ctx, tx)
	if err != nil {
		return 0, err
	}
	return reopens + persons, nil
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
