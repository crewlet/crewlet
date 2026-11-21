package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
)

// WHOM A CHANGE RECORD REACHED: the applier writes ONE ROW PER (record, change,
// recipient) a record's routing reached ([ChangeRouting], [Concerns]; never the
// routing's own actor) into code_notifications (replicated 0109, keyed 0133).
// The change is in the key because one record can tell one handle about
// several ([NoticeID]). A person's code inbox and a change's "whom did this
// wake" are read from these rows, and each committed batch announces whose
// inbox moved ([inbox.Movement], [Applier.Committed]); a seat is woken by the
// code log's feed, never from here. The applier holds no roster: a handle no
// longer employed keeps its row and loses only its wake (internal/routing's
// Route). The table is DIVERGENT, since a row depends on the batch's instant
// and the epoch's horizon ([pastHorizon]), so the purge charges the count every
// node agrees on instead ([countNotices], [Applier.purgeChanges]).

// NoticesTable is the table a change record's notices are kept in: DIVERGENT
// ([Domain.Tables]), neither reproducible nor the log's machinery.
const NoticesTable = "code_notifications"

// SeatNoticeHorizon is how long a seat's notice is kept: [store.EventHistory],
// thirty days, the turn-detail horizon (ADR-0021). Past it the woken turn is
// gone from the event log, and seats are most of a change's audience, so a
// year of their rows would be most of this table for nothing.
const SeatNoticeHorizon = store.EventHistory

// MaxNoticeExcerpt is how much of what a record says its notice keeps: 600
// bytes, the tracker's card excerpt (its MaxExcerpt) — a preview, never the
// text, which the change's own page reads.
const MaxNoticeExcerpt = 600

// writeNotices writes a row for every handle the record's routings concern and
// that hears (none of a change it made), within its recipient's horizon, noting
// each this transaction wrote for the batch's movements (a redelivery writes
// and moves nothing), and, horizon or not, each change's count ([countNotices]).
//
// AS FEW STATEMENTS AS THE PARAMETER LIMIT ALLOWS ([store.InsertRowsReturning]),
// each answering the rows it wrote: the widest record writes 3 904 notices
// (BenchmarkApplyAPushRecord), and one statement a row held the write lane
// most of statelog.ApplyTxTimeBudget (doc.go's measured section).
func (a *Applier) writeNotices(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	payload any) (int, error) {

	routings := RoutingsOf(payload)
	if len(routings) == 0 {
		return 0, nil
	}
	counted, err := countNotices(ctx, tx, at, routings)
	if err != nil {
		return 0, err
	}
	type notice struct {
		Concern
		kind PartyKind
	}
	var kept []notice
	for _, c := range Concerns(routings) {
		if !c.Hears() {
			continue
		}
		kind, horizon := PartyAgent, SeatNoticeHorizon
		if c.Change.Person(c.Handle) {
			kind, horizon = PartyPerson, inbox.Horizon(at.epoch)
		}
		if pastHorizon(at, horizon) {
			continue
		}
		kept = append(kept, notice{Concern: c, kind: kind})
	}
	// [Concerns] keeps a handle once a change, so an answered (change,
	// recipient) names the one concern written.
	type noticeKey struct{ change, handle string }
	byKey := make(map[noticeKey]Concern, len(kept))
	for _, k := range kept {
		byKey[noticeKey{k.Change.ChangeID, k.Handle}] = k.Concern
	}
	record := noticeRecordID(at)
	written, err := store.InsertRowsReturning(ctx, tx, at.maxVariables, addNoticesSQL, noticeRowSQL,
		addNoticesTailSQL, len(kept), func(i int) []any {
			k := kept[i]
			return []any{record, k.Handle, string(k.kind), repoID, k.Change.ChangeID, k.Change.ChangeRef,
				string(k.Reason), k.Addressed, k.FallbackOnly, k.FallbackRank, excerptOf(payload, k.Change),
				at.at(), at.packed, at.position.Stream, int64(at.position.Generation)}
		}, func(rows *sql.Rows) (err error) {
			var key noticeKey
			if err = rows.Scan(&key.handle, &key.change); err != nil {
				return err
			}
			c := byKey[key]
			a.inboxed.Note(inbox.Note{Record: NoticeID(record, key.change), Handle: key.handle,
				Subject: key.change, Reason: string(c.Reason), Position: at.packed, At: at.brokerAt})
			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("gitlet: write the notices of the record at %s: %w", at.position, err)
	}
	return counted + written, nil
}

// countNotices adds to each change's code_changes.notices every hearing handle
// routings tell of it ([noticesByChange]), whether or not its horizon let this
// node write the row: a pure function of the records, so REPRODUCIBLE and never
// below the rows any node holds (one a handle; only the sweep and a purge
// delete). It answers the change rows it updated, and runs only where the
// apply wrote rows ([Applier.noticed]), so a redelivery counts nothing twice.
func countNotices(ctx context.Context, tx *sql.Tx, at applyContext, routings []*ChangeRouting) (int, error) {
	updated := 0
	for _, c := range noticesByChange(routings...) {
		res, err := tx.ExecContext(ctx, countNoticesSQL, c.notices, c.changeID)
		if err != nil {
			return 0, fmt.Errorf("gitlet: count the notices of change %s at %s: %w", c.changeID, at.position, err)
		}
		n, _ := res.RowsAffected()
		updated += int(n)
	}
	return updated, nil
}

// pastHorizon reports a record whose broker instant is older than its batch's
// instant less horizon (a person's [inbox.Horizon] of the batch's epoch, a
// seat's [SeatNoticeHorizon]): a notice the sweep ([NoticeJobs]) would delete,
// which the applier never writes, so a replay of the log rebuilds none. A
// horizon of none keeps everything, and a batch with no instant (no loop drove
// it) dates nothing.
func pastHorizon(at applyContext, horizon time.Duration) bool {
	return horizon > 0 && !at.now.IsZero() && at.brokerAt.Before(at.now.Add(-horizon))
}

// noticeRecordID is the notice's record ([RecordID]), the one the feed derives
// the wakes from: the record's operation id, else its position — never an id
// minted here, which would differ between nodes applying one record.
func noticeRecordID(at applyContext) string { return RecordID(at.record.OpID, at.position) }

// excerptOf is what a notice about change keeps of its record's own text, at
// most [MaxNoticeExcerpt] bytes cut at a character, and nothing for a record
// with no text of its own, such as a push.
func excerptOf(payload any, change *ChangeRouting) string {
	var text string
	switch p := payload.(type) {
	case ChangeOpen:
		text = p.Body
	case ChangeEdit:
		if p.Body != nil {
			text = *p.Body
		}
	case ChangeClose:
		text = p.Reason
	case ReviewerRequest:
		text = p.Note
	case Comment:
		text = p.Body
	case Review:
		text = p.Summary
	case CloseSettled:
		// The engine's sentence on why this change's work item was left open
		// (`merged; ENG-42 not closed: …`), from its routing, since one settle
		// tells of several changes.
		text = change.NotClosed
	}
	return textcut.Within(text, MaxNoticeExcerpt)
}

// removeNotices deletes every notice of changeIDs, changes a purge removes
// whole, each charged its count ([Applier.purgeChanges]), noting each removal
// for the batch's movements at the purge's broker instant
// ([inbox.Movement.At]), and answers how many it deleted.
func (a *Applier) removeNotices(ctx context.Context, tx *sql.Tx, at applyContext, changeIDs []string) (int, error) {
	if len(changeIDs) == 0 {
		return 0, nil
	}
	list, err := jsonList(changeIDs)
	if err != nil {
		return 0, err
	}
	n, err := a.noted(ctx, tx, at, inbox.Note{Removed: true}, removeNoticesSQL, list)
	if err != nil {
		return 0, fmt.Errorf("gitlet: remove the notices of the changes a purge removes: %w", err)
	}
	return n, nil
}

// removeCutNotices deletes at most limit notices of changeID, the change a
// purge part cuts, oldest first, noting each as [Applier.removeNotices] does,
// and answers how many it deleted: fewer than limit where the sweep left this
// node fewer than the count says.
func (a *Applier) removeCutNotices(ctx context.Context, tx *sql.Tx, at applyContext, changeID string,
	limit int) (int, error) {
	n, err := a.noted(ctx, tx, at, inbox.Note{Removed: true}, removeCutNoticesSQL, changeID, limit)
	if err != nil {
		return 0, fmt.Errorf("gitlet: remove the notices of change %s a purge cuts: %w", changeID, err)
	}
	return n, nil
}

// unquoteNotices clears the excerpt of every notice a deleted comment's record
// wrote, found by its change and its record's position, noting each changed.
func (a *Applier) unquoteNotices(ctx context.Context, tx *sql.Tx, at applyContext, changeID string,
	position int64) (int, error) {

	n, err := a.noted(ctx, tx, at, inbox.Note{Changed: true}, unquoteNoticesSQL, changeID, position)
	if err != nil {
		return 0, fmt.Errorf("gitlet: clear the notices quoting a comment of change %s: %w", changeID, err)
	}
	return n, nil
}

// noted runs one notice statement and notes each row its RETURNING answers as
// like says (removed or changed), by its [NoticeID], at the record's broker
// instant: one pass, never a read and a second seek of the same index.
func (a *Applier) noted(ctx context.Context, tx *sql.Tx, at applyContext, like inbox.Note, stmt string,
	args ...any) (int, error) {
	rows, err := tx.QueryContext(ctx, stmt, args...)
	if err != nil {
		return 0, err
	}
	n := 0
	for rows.Next() {
		note := like
		if err := rows.Scan(&note.Record, &note.Handle, &note.Subject, &note.Reason, &note.Position); err != nil {
			_ = rows.Close()
			return 0, err
		}
		note.Record, note.At = NoticeID(note.Record, note.Subject), at.brokerAt
		a.inboxed.Note(note)
		n++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	return n, rows.Close()
}

// changesOf is every change repoID still holds, by id: the remainder its final
// purge finds, whose notices go before the rows that are the only way to them.
func changesOf(ctx context.Context, tx *sql.Tx, repoID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, purgedChangesSQL, repoID)
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

// The applier's notice statements, each with a WHERE registered in
// plan_test.go: the write, in three parts for [store.InsertRowsReturning], a
// conflict on the key skipped; the deletes by code_notifications_change_idx,
// one seek a list element or a range of one change, oldest first; a deleted
// comment's excerpts cleared by the same key; and the final purge's read by
// code_changes_number_idx. The sweep's is noticesweep.go's.
const (
	addNoticesSQL = `INSERT INTO code_notifications (record_id, recipient, recipient_kind, repo_id, change_id,
			change_ref, reason, addressed, fallback_only, fallback_rank, excerpt, created_at, log_seq, log_stream,
			log_generation) VALUES `
	noticeRowSQL      = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	addNoticesTailSQL = `ON CONFLICT (record_id, change_id, recipient) DO NOTHING RETURNING recipient, change_id`
	countNoticesSQL   = `UPDATE code_changes SET notices = notices + ? WHERE id = ?`
	removeNoticesSQL  = `DELETE FROM code_notifications WHERE rowid IN (SELECT n.rowid
		FROM json_each(?) AS j CROSS JOIN code_notifications AS n INDEXED BY code_notifications_change_idx
		WHERE n.change_id = CAST(j.value AS TEXT))
		RETURNING record_id, recipient, change_id, reason, log_seq`
	removeCutNoticesSQL = `DELETE FROM code_notifications WHERE rowid IN (SELECT rowid
		FROM code_notifications INDEXED BY code_notifications_change_idx WHERE change_id = ?
		ORDER BY log_seq LIMIT ?)
		RETURNING record_id, recipient, change_id, reason, log_seq`
	unquoteNoticesSQL = `UPDATE code_notifications SET excerpt = ''
		WHERE rowid IN (SELECT rowid FROM code_notifications INDEXED BY code_notifications_change_idx
			WHERE change_id = ? AND log_seq = ? AND excerpt != '')
		RETURNING record_id, recipient, change_id, reason, log_seq`
	purgedChangesSQL = `SELECT id FROM code_changes INDEXED BY code_changes_number_idx WHERE repo_id = ?`
)
