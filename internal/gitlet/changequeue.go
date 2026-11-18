package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A CHANGE'S QUEUE ENTRIES, its `queue` part ([PartQueue]): each enqueue of the change, who made
// it and how it ended, read on code_queue_change_idx in queue order, so its timeline says when it
// was added to its target's queue and when it landed, was taken out or left it. An ended entry is
// kept as long as the queue's history ([QueueHistory]), so a change's older entries are gone from
// its timeline as from the queue's.

// ChangeQueueEntry is one queue entry of a change as its `queue` part serves it: its target and
// the head it was enqueued at, how it was to merge and who enqueued it, where it stands, and for
// one that ended why ([QueueEndReason]; "" for a landing), what its ejection named (the checks,
// the conflicting paths) and the commit it landed as; the positions and instants of the record
// that enqueued it and the one that ended it.
type ChangeQueueEntry struct {
	EntryID          string            `json:"entry_id"`
	TargetRef        string            `json:"target_ref"`
	HeadOID          string            `json:"head_oid"`
	Method           MergeMethod       `json:"method"`
	EnqueuedBy       Party             `json:"enqueued_by"`
	State            QueueState        `json:"state"`
	Reason           QueueEndReason    `json:"reason,omitempty"`
	Checks           []string          `json:"checks,omitempty"`
	Paths            []string          `json:"paths,omitempty"`
	CommitOID        string            `json:"commit_oid,omitempty"`
	EnqueuedPosition statelog.Position `json:"enqueued_position"`
	EnqueuedAt       time.Time         `json:"enqueued_at"`
	EndedPosition    statelog.Position `json:"ended_position,omitzero"`
	EndedAt          time.Time         `json:"ended_at,omitzero"`
}

// Cursor is the cursor that resumes a page of a change's queue entries after e: its enqueue's
// position, packed, its place in the change's index.
func (e ChangeQueueEntry) Cursor() string { return strconv.FormatInt(e.EnqueuedPosition.Packed(), 10) }

// changeQueue serves a page of change id's queue entries (named: its web!n) in queue order,
// after the entry a cursor names ([ChangeQueueEntry.Cursor]).
func (r *Reader) changeQueue(ctx context.Context, tx *sql.Tx, q ReadQuery, named, id string,
	out *ReadAnswer) error {

	limit := min(limitOr(q.Limit, defaultQueueEntryLimit), r.pageRows)
	after := int64(0)
	if q.Cursor != "" {
		packed, err := strconv.ParseInt(q.Cursor, 10, 64)
		if err != nil || packed <= 0 {
			return fmt.Errorf("%w: %q is not a queue entry of a change", ErrBadCursor, q.Cursor)
		}
		after = packed
	}
	rows, err := tx.QueryContext(ctx, queueOfChangeSQL, id, after, limit)
	if err != nil {
		return fmt.Errorf("gitlet: read the queue entries of %s: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		var detail string
		e, scanErr := scanEntry(func(dest ...any) error { return rows.Scan(append(dest, &detail)...) })
		if scanErr != nil {
			return fmt.Errorf("gitlet: read the queue entries of %s: %w", named, scanErr)
		}
		entry, entryErr := changeQueueEntry(e, detail)
		if entryErr != nil {
			return entryErr
		}
		out.QueueEntries = append(out.QueueEntries, entry)
		if pg.add(entry.encodedBytes()) {
			out.Next = entry.Cursor()
			break
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("gitlet: read the queue entries of %s: %w", named, err)
	}
	return nil
}

// changeQueueEntry is e as a change's `queue` part serves it, its ejection's detail as the
// `ejected` column keeps it.
func changeQueueEntry(e queueEntry, detail string) (ChangeQueueEntry, error) {
	out := ChangeQueueEntry{EntryID: e.EntryID, TargetRef: e.TargetRef, HeadOID: e.HeadOID, Method: e.Method,
		EnqueuedBy: e.EnqueuedBy, State: e.State, Reason: e.EndReason, EnqueuedPosition: unpacked(e.Seq),
		EnqueuedAt: store.DecodeTime(e.EnqueuedAt)}
	if e.EndedPosition > 0 {
		out.EndedPosition, out.EndedAt = unpacked(e.EndedPosition), store.DecodeTime(e.EndedAt)
	}
	if e.State == QueueLanded {
		out.CommitOID = e.CommitOID
	}
	if detail == "" {
		return out, nil
	}
	var x struct {
		Checks []string `json:"checks"`
		Paths  []string `json:"paths"`
	}
	if err := json.Unmarshal([]byte(detail), &x); err != nil {
		return ChangeQueueEntry{}, fmt.Errorf("gitlet: read entry %s's ejection: %w", e.EntryID, err)
	}
	out.Checks, out.Paths = x.Checks, x.Paths
	return out, nil
}

// A change's queue entries are read on code_queue_change_idx, in queue order after the enqueue a
// cursor names (0 for the first page); registered in plan_test.go.
const queueOfChangeSQL = `SELECT ` + entryColumns + `, ejected FROM code_queue INDEXED BY code_queue_change_idx
	WHERE change_id = ? AND seq > ? ORDER BY seq LIMIT ?`
