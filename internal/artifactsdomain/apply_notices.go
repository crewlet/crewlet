package artifactsdomain

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
)

// WHOM A COMMENT RECORD TOLD: the artifacts half of a person's inbox.
//
// A comment record that opens a thread, replies in one, resolves or reopens
// one carries its routing snapshot ([CommentNotify]), computed by the API when
// the record was formed. Its agents are woken through this log's feed; its
// PEOPLE are told here: the applier writes ONE ROW PER (record, person) in
// artifact_notifications (replicated 0175), never to the record's own author,
// as Gitlet writes code_notifications. A person's artifacts inbox is read from
// these rows (inboxread.go) and each committed batch announces whose inbox its
// rows moved ([inbox.Movement], [Applier.Committed]).
//
// # Kept the company's inbox horizon, and none past it
//
// A notice is kept the company's `inbox.retention_days` ([inbox.Horizon], read
// off the epoch the batch is applied under). The applier writes no row past it
// and each node sweeps its own rows past it ([NoticeJobs]), so a replay of the
// whole log rebuilds no notice the sweep would delete. The table is DIVERGENT
// for that reason: whether a row is written depends on the batch's instant and
// the epoch's horizon, which two nodes may not share.
//
// # What a removal and a purge take
//
// A remark's removal clears the excerpt of every notice its record wrote, so
// the words its author took back are kept nowhere; an artifact's purge deletes
// every notice about it. Each moves the inbox of the person it told.

// NoticesTable is the table a comment record's notices are kept in: DIVERGENT
// ([Domain.Tables]), neither reproducible nor the log's machinery.
const NoticesTable = "artifact_notifications"

// MaxNoticeExcerpt is how much of a remark its notice keeps: SIX HUNDRED BYTES,
// the tracker's card excerpt, a preview a list of notices shows; the thread
// reads the whole.
const MaxNoticeExcerpt = 600

// DefaultPrimaryReasons is the split a person who declared none is read with:
// a comment on what they own, one that names them, and one on an artifact made
// for their work are theirs to act on; a reply in a thread they are in is
// context.
var DefaultPrimaryReasons = []NotifyReason{NotifyOwner, NotifyMention, NotifyAssignee}

// noticed is what one comment record says to the people it tells.
type noticed struct {
	artifact, thread, remark string
	op                       OpKind
	body                     string
	people                   []Recipient
}

// noticeOf is what record payload, applied on thread of artifact, tells, and
// false for a record that tells nobody.
func noticeOf(artifact, thread string, op OpKind, payload any) (noticed, bool) {
	n := noticed{artifact: artifact, thread: thread, op: op}
	var notify *CommentNotify
	switch p := payload.(type) {
	case CommentOpenPayload:
		n.remark, n.body, notify = p.Remark, p.Body, p.Notify
	case CommentReplyPayload:
		n.remark, n.body, notify = p.Remark, p.Body, p.Notify
	case CommentStatusPayload:
		notify = p.Notify
	}
	if notify == nil || len(notify.People) == 0 {
		return noticed{}, false
	}
	n.people = notify.People
	return n, true
}

// writeNotices writes the notices of one applied comment record: a row for
// every person it tells but its own author, once each, within the horizon,
// each noted for the batch's movements where this transaction wrote it, so a
// redelivery writes nothing and moves nobody's inbox.
func (a *Applier) writeNotices(ctx context.Context, tx *sql.Tx, c applyContext, n noticed) (int, error) {
	if pastHorizon(c, inbox.Horizon(c.epoch)) {
		return 0, nil
	}
	actor := c.record.actor()
	var told []Recipient
	for _, r := range n.people {
		if r.Seat == actor.Name || slices.ContainsFunc(told, func(t Recipient) bool { return t.Seat == r.Seat }) {
			continue
		}
		told = append(told, r)
	}
	if len(told) == 0 {
		return 0, nil
	}
	reasons := make(map[string]Recipient, len(told))
	for _, r := range told {
		reasons[r.Seat] = r
	}
	excerpt := textcut.Within(n.body, MaxNoticeExcerpt)
	record := c.record.OpID
	written, err := store.InsertRowsReturning(ctx, tx, c.maxVariables, addNoticesSQL, noticeRowSQL,
		addNoticesTailSQL, len(told), func(i int) []any {
			r := told[i]
			return []any{record, r.Seat, n.artifact, n.thread, n.remark, string(n.op), string(r.Reason),
				r.Addressed, excerpt, actor.Name, string(actor.Kind), c.at(), c.packed, c.position.Stream,
				int64(c.position.Generation)}
		}, func(rows *sql.Rows) error {
			var handle string
			if err := rows.Scan(&handle); err != nil {
				return err
			}
			a.inboxed.Note(inbox.Note{Record: record, Handle: handle, Subject: n.artifact,
				Reason: string(reasons[handle].Reason), Position: c.packed, At: c.brokerAt})
			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: write the notices of the record at %s: %w", c.position, err)
	}
	return written, nil
}

// pastHorizon reports a record whose broker instant is older than its batch's
// instant less horizon: a notice the sweep would delete, which the applier
// never writes. A horizon of none keeps everything, and a batch with no
// instant dates nothing.
func pastHorizon(c applyContext, horizon time.Duration) bool {
	return horizon > 0 && !c.now.IsZero() && c.brokerAt.Before(c.now.Add(-horizon))
}

// clearNoticeExcerpts takes a removed remark's words out of every notice its
// record wrote, noting each as changed for the batch's movements.
func (a *Applier) clearNoticeExcerpts(ctx context.Context, tx *sql.Tx, c applyContext, artifact,
	remark string) (int, error) {

	n, err := a.noted(ctx, tx, c, inbox.Note{Changed: true}, clearExcerptsSQL, artifact, remark)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: clear the notices of remark %s of %s: %w", remark, artifact, err)
	}
	return n, nil
}

// removeNotices deletes every notice about a purged artifact, noting each as
// removed for the batch's movements.
func (a *Applier) removeNotices(ctx context.Context, tx *sql.Tx, c applyContext, artifact string) (int, error) {
	n, err := a.noted(ctx, tx, c, inbox.Note{Removed: true}, removeNoticesSQL, artifact)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: remove the notices of %s: %w", artifact, err)
	}
	return n, nil
}

// noted runs one statement whose RETURNING is the notices it touched, noting
// each as like says (removed or changed), at the record's broker instant: ONE
// PASS, never a read of the rows and a second seek to change them.
func (a *Applier) noted(ctx context.Context, tx *sql.Tx, c applyContext, like inbox.Note, stmt string,
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
		note.At = c.brokerAt
		a.inboxed.Note(note)
		n++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	return n, rows.Close()
}

// The applier's notice statements: the write in three parts for
// [store.InsertRowsReturning], a conflict on the key a skip and the recipients
// it wrote its answer; a removed remark's excerpts cleared and a purged
// artifact's notices deleted, each by artifact_notifications_remark_idx and
// each answering the notices it touched.
const (
	addNoticesSQL = `INSERT INTO artifact_notifications (record_id, recipient, artifact_id, thread_id, remark_id,
			op, reason, addressed, excerpt, actor, actor_kind, created_at, log_seq, log_stream, log_generation)
		VALUES `
	noticeRowSQL      = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	addNoticesTailSQL = `ON CONFLICT (record_id, recipient) DO NOTHING RETURNING recipient`
	clearExcerptsSQL  = `UPDATE artifact_notifications SET excerpt = ''
		WHERE artifact_id = ? AND remark_id = ? AND excerpt != ''
		RETURNING record_id, recipient, artifact_id, reason, log_seq`
	removeNoticesSQL = `DELETE FROM artifact_notifications WHERE artifact_id = ?
		RETURNING record_id, recipient, artifact_id, reason, log_seq`
)
