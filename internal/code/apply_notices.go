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
// every record about a change carries ([ChangeRouting], concerns.go) down as
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
// # A purge takes a purged change's notices
//
// A repository's purge removes its changes' notices with the changes — each
// purge part those of the changes it removes whole, the final purge those of
// the rest — WITHOUT COUNTING THEM against a part's bound: the part's count
// decides which replicated rows go, which every node must agree on, and this
// table is not the same on every node. Every removed notice moves its
// recipient's inbox ([inbox.Note.Removed]).

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
// nobody's inbox.
func (a *Applier) writeNotices(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	payload any) (int, error) {

	routings := RoutingsOf(payload)
	if len(routings) == 0 {
		return 0, nil
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
	return written, nil
}

// pastHorizon reports a record whose broker instant is older than its batch's
// instant less horizon: a notice its sweep would delete, which the applier
// never writes. A horizon of none keeps everything, and a batch with no
// instant — an apply no loop drove — dates nothing.
func pastHorizon(at applyContext, horizon time.Duration) bool {
	return horizon > 0 && !at.now.IsZero() && at.brokerAt.Before(at.now.Add(-horizon))
}

// noticeRecordID is the notice's record: the record's own operation id, else
// its position — never an id minted here, which would differ between two nodes
// applying one record and write two notices for one change.
func noticeRecordID(at applyContext) string {
	if at.record.OpID != "" {
		return at.record.OpID
	}
	return at.position.String()
}

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

// removeNotices deletes the notices of changeIDs — changes a purge removes —
// noting each for the batch's movements, and answers how many it deleted,
// WHICH NO PURGE COUNTS against its bound (this file's head).
func (a *Applier) removeNotices(ctx context.Context, tx *sql.Tx, changeIDs []string) (int, error) {
	if len(changeIDs) == 0 {
		return 0, nil
	}
	list, err := jsonList(changeIDs)
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, noticesOfChangesSQL, list)
	if err != nil {
		return 0, fmt.Errorf("code: read the notices of the changes a purge removes: %w", err)
	}
	for rows.Next() {
		var n inbox.Note
		if err := rows.Scan(&n.Record, &n.Handle, &n.Subject, &n.Reason, &n.Position); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("code: read the notices of the changes a purge removes: %w", err)
		}
		n.Removed = true
		a.inboxed.Note(n)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("code: read the notices of the changes a purge removes: %w", err)
	}
	_ = rows.Close()
	res, err := tx.ExecContext(ctx, removeNoticesSQL, list)
	if err != nil {
		return 0, fmt.Errorf("code: remove the notices of the changes a purge removes: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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
// plan_test.go: the write, a conflict on the key a skip; the notices of a list
// of changes, read and removed by code_notifications_change_idx, one seek an
// element; and the changes a repository still holds, read for its final purge
// by code_changes_number_idx. The sweep's is noticesweep.go's.
const (
	addNoticeSQL = `INSERT INTO code_notifications (record_id, recipient, recipient_kind, repo_id, change_id,
			change_ref, reason, addressed, fallback_only, fallback_rank, excerpt, created_at, log_seq, log_stream,
			log_generation)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (record_id, recipient) DO NOTHING`
	noticesOfChangesSQL = `SELECT n.record_id, n.recipient, n.change_id, n.reason, n.log_seq
		FROM json_each(?) AS j CROSS JOIN code_notifications AS n INDEXED BY code_notifications_change_idx
		WHERE n.change_id = CAST(j.value AS TEXT)`
	removeNoticesSQL = `DELETE FROM code_notifications WHERE rowid IN (SELECT n.rowid
		FROM json_each(?) AS j CROSS JOIN code_notifications AS n INDEXED BY code_notifications_change_idx
		WHERE n.change_id = CAST(j.value AS TEXT))`
	purgedChangesSQL = `SELECT id FROM code_changes INDEXED BY code_changes_number_idx WHERE repo_id = ?`
)
