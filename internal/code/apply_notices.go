package code

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
)

// WHOM A CHANGE RECORD REACHED, PER RECIPIENT: the applier writes the answer
// a record about a change carries ([ChangeRouting], concerns.go; none where it
// concerns nobody, which writes no row) down as
// ONE ROW PER (record, recipient) it reached — every handle its routing's
// precedence keeps ([Concerns]) but the record's own actor — in
// code_notifications (replicated 0081), as the tracker writes
// tracker_notifications from its records. A person's code inbox is read from
// these rows, a change's "whom did this wake" is answered from them, and each
// committed batch announces whose inbox its rows moved ([inbox.Movement],
// [Applier.Committed]). A SEAT is woken through the code log's feed, never
// from here; its row is the record of the wake.
//
// # Every candidate, not every employee
//
// The applier holds no roster: a handle the company no longer employs keeps
// its row, written before anybody asked, and loses only its wake, which the
// feed's routing filters (internal/routing's Route). So the rows are a
// complete record of whom a change concerned, and the wakes a subset of them.
//
// # Two horizons, and none past its own
//
// A PERSON's notice is kept the company's `inbox.retention_days`
// ([inbox.Horizon], read off the epoch the batch is applied under), a SEAT's
// [SeatNoticeHorizon]. The applier writes no row past its recipient's horizon
// — a record whose broker instant is older than its batch's instant less the
// horizon writes none for them — and each node sweeps its own rows past each
// horizon ([NoticeJobs]): the two hold one horizon, so a replay of the whole
// log rebuilds no notice the sweep would delete. The table is DIVERGENT for
// that reason: whether a row is written depends on the batch's instant and the
// epoch's horizon, which two nodes may not share.
//
// # A change counts its notices where every node agrees
//
// Beside its rows, every record that writes notices adds to each change it
// tells anybody about how many handles hear of it ([noticesByChange]) —
// code_changes.notices (replicated 0101), REPRODUCIBLE — whether or not the
// recipient's horizon let this node write the row. The count is a pure
// function of the records, so it is the same on every node, and it is never
// below the rows any node holds of the change: a node writes at most one row
// a hearing handle, and only the sweep and a purge delete one.
//
// # A purge takes a purged change's notices, counted
//
// A repository's purge removes its changes' notices with the changes — each
// purge part those of the changes it removes whole and at most what is left
// of its bound of the change it cuts, the final purge those of the rest — and
// CHARGES THEM BY THAT COUNT against a part's bound: a part's count decides
// which replicated rows go, so it must agree on every node, which this
// table's rows do not and the count does ([Applier.purgeChanges]). So a part
// deletes at most PurgePartRows rows of both tables together on every node,
// MaxRecordRows' reason. Every removed notice moves its recipient's inbox
// ([inbox.Note.Removed]).

// NoticesTable is the table a change record's notices are kept in: DIVERGENT
// ([Domain.Tables]), neither reproducible nor the log's machinery.
const NoticesTable = "code_notifications"

// SeatNoticeHorizon is how long a seat's notice is kept: [store.EventHistory],
// THIRTY DAYS, the turn-detail horizon (ADR-0021). Past it the turn a wake
// started is gone from the event log, so the row would name a delivery
// nothing can show any more — and seats are most of a change's audience, so
// at a company's scale a year of their rows would be most of this table for
// nothing.
const SeatNoticeHorizon = store.EventHistory

// MaxNoticeExcerpt is how much of what a record says its notice keeps: SIX
// HUNDRED BYTES, the tracker's card excerpt (its MaxExcerpt) — a preview a
// list of notices shows, never the text, which the change's own page reads.
const MaxNoticeExcerpt = 600

// writeNotices writes the notices of one applied record about a change on
// repository repoID, its payload decoded: a row for every handle its
// routings concern and that hears (no handle hears of a change it made),
// within its recipient's horizon, each noted for the batch's movements
// where this transaction wrote it — a redelivery writes nothing, and moves
// nobody's inbox — and, horizon or not, each change's count of them
// ([countNotices]).
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
	record := noticeRecordID(at)
	excerpt := excerptOf(payload)
	written := 0
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
		res, err := tx.ExecContext(ctx, addNoticeSQL, record, c.Handle, string(kind), repoID, c.Change.ChangeID,
			c.Change.ChangeRef, string(c.Reason), c.Addressed, c.FallbackOnly, c.FallbackRank, excerpt, at.at(),
			at.packed, at.position.Stream, int64(at.position.Generation))
		if err != nil {
			return 0, fmt.Errorf("code: write the notice of %s for %s at %s: %w", c.Change.ChangeRef, c.Handle,
				at.position, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			written++
			a.inboxed.Note(inbox.Note{Record: record, Handle: c.Handle, Subject: c.Change.ChangeID,
				Reason: string(c.Reason), Position: at.packed})
		}
	}
	return counted + written, nil
}

// countNotices adds to each change routings tell anybody about the notices
// they may write about it ([noticesByChange]) — every hearing handle, whether
// or not its horizon lets this node write the row, so the count is the same
// on every node — and answers the change rows it updated. It runs only where
// the record's apply wrote rows ([Applier.noticed]), as the notices do, so a
// redelivery counts nothing twice.
func countNotices(ctx context.Context, tx *sql.Tx, at applyContext, routings []*ChangeRouting) (int, error) {
	updated := 0
	for _, c := range noticesByChange(routings...) {
		res, err := tx.ExecContext(ctx, countNoticesSQL, c.notices, c.changeID)
		if err != nil {
			return 0, fmt.Errorf("code: count the notices of change %s at %s: %w", c.changeID, at.position, err)
		}
		n, _ := res.RowsAffected()
		updated += int(n)
	}
	return updated, nil
}

// pastHorizon reports a record whose broker instant is older than its batch's
// instant less horizon: a notice its sweep would delete, which the applier
// never writes. A horizon of none keeps everything, and a batch with no
// instant — an apply no loop drove — dates nothing.
func pastHorizon(at applyContext, horizon time.Duration) bool {
	return horizon > 0 && !at.now.IsZero() && at.brokerAt.Before(at.now.Add(-horizon))
}

// noticeRecordID is the notice's record ([RecordID]): the record's own
// operation id, else its position — never an id minted here, which would
// differ between two nodes applying one record and write two notices for one
// change, and the one the feed derives the record's wakes from.
func noticeRecordID(at applyContext) string { return RecordID(at.record.OpID, at.position) }

// excerptOf is what a notice keeps of what its record says, at most
// [MaxNoticeExcerpt] bytes cut at a character: an open's description, an
// edit's new one, a close's reason, an ask's note, a comment's body, a review's
// summary — and nothing for a record that says nothing of its own, a push's
// among them, which can name several changes.
func excerptOf(payload any) string {
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
	}
	return textcut.Within(text, MaxNoticeExcerpt)
}

// removeNotices deletes every notice of changeIDs — changes a purge removes
// whole, each charged its count ([Applier.purgeChanges], [Store.PurgeStep])
// — noting each for the batch's movements, and answers how many it deleted.
func (a *Applier) removeNotices(ctx context.Context, tx *sql.Tx, changeIDs []string) (int, error) {
	if len(changeIDs) == 0 {
		return 0, nil
	}
	list, err := jsonList(changeIDs)
	if err != nil {
		return 0, err
	}
	n, err := a.removed(ctx, tx, removeNoticesSQL, list)
	if err != nil {
		return 0, fmt.Errorf("code: remove the notices of the changes a purge removes: %w", err)
	}
	return n, nil
}

// removeCutNotices deletes at most limit notices of changeID — the change a
// purge part cuts, charged limit of its count — oldest first, noting each for
// the batch's movements, and answers how many it deleted: up to limit, and
// fewer where the sweep left this node fewer than its count says.
func (a *Applier) removeCutNotices(ctx context.Context, tx *sql.Tx, changeID string, limit int) (int, error) {
	n, err := a.removed(ctx, tx, removeCutNoticesSQL, changeID, limit)
	if err != nil {
		return 0, fmt.Errorf("code: remove the notices of change %s a purge cuts: %w", changeID, err)
	}
	return n, nil
}

// removed runs one of the purge's notice deletes, whose RETURNING is the rows
// it deleted, and notes each as removed from its recipient's inbox: ONE PASS,
// the delete answering what it took, never a read of the rows and a second
// seek of the same index to delete them.
func (a *Applier) removed(ctx context.Context, tx *sql.Tx, stmt string, args ...any) (int, error) {
	rows, err := tx.QueryContext(ctx, stmt, args...)
	if err != nil {
		return 0, err
	}
	n := 0
	for rows.Next() {
		var note inbox.Note
		if err := rows.Scan(&note.Record, &note.Handle, &note.Subject, &note.Reason, &note.Position); err != nil {
			_ = rows.Close()
			return 0, err
		}
		note.Removed = true
		a.inboxed.Note(note)
		n++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	return n, rows.Close()
}

// changesOf is every change repoID still holds, by id: what its final purge
// finds past its parts — a remainder its parts kept small — whose notices
// go before the rows that are the only way to them.
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
// plan_test.go: the write, a conflict on the key a skip; a change's count of
// them, by the change's key; the notices of a list of changes removed by
// code_notifications_change_idx, one seek an element, and the oldest of one
// change's by a range of it, each answering the rows it deleted; and the
// changes a repository still holds, read for its final purge by
// code_changes_number_idx. The sweep's is noticesweep.go's.
const (
	addNoticeSQL = `INSERT INTO code_notifications (record_id, recipient, recipient_kind, repo_id, change_id,
			change_ref, reason, addressed, fallback_only, fallback_rank, excerpt, created_at, log_seq, log_stream,
			log_generation)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (record_id, recipient) DO NOTHING`
	countNoticesSQL  = `UPDATE code_changes SET notices = notices + ? WHERE id = ?`
	removeNoticesSQL = `DELETE FROM code_notifications WHERE rowid IN (SELECT n.rowid
		FROM json_each(?) AS j CROSS JOIN code_notifications AS n INDEXED BY code_notifications_change_idx
		WHERE n.change_id = CAST(j.value AS TEXT))
		RETURNING record_id, recipient, change_id, reason, log_seq`
	removeCutNoticesSQL = `DELETE FROM code_notifications WHERE rowid IN (SELECT rowid
		FROM code_notifications INDEXED BY code_notifications_change_idx WHERE change_id = ?
		ORDER BY log_seq LIMIT ?)
		RETURNING record_id, recipient, change_id, reason, log_seq`
	purgedChangesSQL = `SELECT id FROM code_changes INDEXED BY code_changes_number_idx WHERE repo_id = ?`
)
