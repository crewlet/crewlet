package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// THE READS OF A CHANGE'S CONVERSATION: a listing of the changes waiting on a
// reviewer ([ReadQuery.Reviewer], [ReadQuery.Needs]) and the parts of one
// change's conversation a `change` read pages ([ReadQuery.Part]) — its
// reviewers and its comments — each served inside the one transaction
// [Reader.Read] hands it, from the rows the records on the change's own
// subject wrote (replicated 0070), and held to the grant on the change's
// repository as every read of a change is (changeread.go).
//
// # Who a change waits on
//
// A listing that names a REVIEWER — or the changes that wait on a review,
// needs=review, the asker's own where it names no reviewer — walks
// code_reviewers_reviewer_idx: the changes whose ask of that reviewer
// STANDS, newest first by the change's id, the asks read in ONE seek a page
// and their changes' rows by their keys beside them ([changeWalk.readAsked]).
// The index leads with the ask's state after the reviewer, so a page reads the
// standing asks and none of the withdrawn ones beside them. An ask of a closed
// or merged change still stands — nothing withdraws it, since the change's
// lifecycle is its repository's records and its asks are its own subject's —
// so needs=review keeps a LIVE change alone, tested on each row the page read
// as every filter the walked index does not seek is; a walk from the newest
// change down meets the live ones first, and a reviewer who pages on reads
// the history of what they were asked.
//
// # A part of one change
//
// A `change` read names the part it pages: its revisions — the default, with
// the change's detail on their first page (changeread.go) — its reviewers, in
// the order of their handles, or its comments, in THREAD order: each thread's
// first comment and then its replies, by the position of the record that said
// each, threads in the order of their ids, which are minted as they start
// (code_comments_thread_idx). Every page names the change by its number and
// is held to the grant again; a page past a cursor resumes after the row the
// cursor names, so a comment said meanwhile in a thread already read is read
// by reading the change again, at the floor its write answered with.

// Need is what a listing of changes asks its changes to be waiting on
// ([ReadQuery.Needs]).
type Need string

// NeedReview is the changes waiting on a review: live, and asking it of the
// reviewer [ReadQuery.Reviewer] names — of the asker's own seat where it
// names none.
const NeedReview Need = "review"

// Needs is the closed set, in this order.
func Needs() []Need { return []Need{NeedReview} }

// Valid reports a need this build knows.
func (n Need) Valid() bool { return slices.Contains(Needs(), n) }

// ChangePart is the list a `change` read pages beside the change
// ([ReadQuery.Part]).
type ChangePart string

const (
	// PartRevisions is the change's revisions, newest first, its detail
	// beside the first page — what a read naming no part pages.
	PartRevisions ChangePart = "revisions"
	// PartReviewers is the reviewers the change has asked, by handle.
	PartReviewers ChangePart = "reviewers"
	// PartComments is the change's comments, a thread at a time.
	PartComments ChangePart = "comments"
)

// ChangeParts is the closed set, in this order.
func ChangeParts() []ChangePart { return []ChangePart{PartRevisions, PartReviewers, PartComments} }

// Valid reports a part this build knows.
func (p ChangePart) Valid() bool { return slices.Contains(ChangeParts(), p) }

// detailed reports the part whose first page carries the change's detail:
// its revisions, which a read naming no part pages.
func (p ChangePart) detailed() bool { return p == "" || p == PartRevisions }

// maxPerPage is the most rows one page of p may ask for.
func (p ChangePart) maxPerPage() int {
	switch p {
	case PartReviewers:
		return MaxReviewersPerPage
	case PartComments:
		return MaxCommentsPerPage
	}
	return MaxRevisionsPerPage
}

// The pages of a change's conversation, beside its revisions' (reader.go's
// bounds say why each is what it is).
const (
	defaultReviewerLimit = 200
	MaxReviewersPerPage  = 1_000
	defaultCommentLimit  = 50
	MaxCommentsPerPage   = 200
)

// waitedOn is the handle a listing of what its changes wait on names: the
// reviewer it names, or the asker's own — a seat's handle, or the human seat a
// person holds — and "" for an asker bound to no seat.
func (q ReadQuery) waitedOn() string {
	if q.Reviewer != "" {
		return q.Reviewer
	}
	handle, _ := q.Asker.party()
	return handle
}

// readAsked is one page of the changes whose ask of the walk's reviewer
// stands: at most limit asks, after the change a cursor names, newest first,
// and the row of each change they name — every one, since a change's asks go
// before its row in a purge and with it in the final one.
func (w changeWalk) readAsked(ctx context.Context, tx *sql.Tx, cursor string, limit int) ([]changeRow, error) {
	query, args := changesAskedSQL, slices.Clone(w.keys)
	if cursor != "" {
		resumed, ok := w.resume(cursor)
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrBadCursor, cursor)
		}
		query, args = changesAskedAfterSQL, append(args, resumed...)
	}
	rows, err := tx.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("code: list the changes asking %v: %w", w.keys[0], err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("code: list the changes asking %v: %w", w.keys[0], err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("code: list the changes asking %v: %w", w.keys[0], err)
	}
	_ = rows.Close()
	if len(ids) == 0 {
		return nil, nil
	}
	byID, err := changesByIDIn(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]changeRow, 0, len(ids))
	for _, id := range ids {
		row, found := byID[id]
		if !found {
			return nil, fmt.Errorf("code: an ask of %v stands on change %s, which the rows do not hold", w.keys[0],
				id)
		}
		out = append(out, row)
	}
	return out, nil
}

// changesByIDIn is the row of each change ids names, by id — one statement
// driven from their list, a seek of the key each.
func changesByIDIn(ctx context.Context, tx *sql.Tx, ids []string) (map[string]changeRow, error) {
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, changesByIDSQL, list)
	if err != nil {
		return nil, fmt.Errorf("code: read %d changes by id: %w", len(ids), err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]changeRow, len(ids))
	for rows.Next() {
		row, err := scanChange(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("code: read %d changes by id: %w", len(ids), err)
		}
		out[row.id] = row
	}
	return out, rows.Err()
}

// conversationRowIn is change id's conversation's counters, nil where nobody
// has discussed it.
func conversationRowIn(ctx context.Context, tx *sql.Tx, id string) (*ConversationRow, error) {
	var (
		c        ConversationRow
		position int64
	)
	err := tx.QueryRowContext(ctx, conversationCountsSQL, id).Scan(&c.Comments, &c.Version, &position)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("code: read change %s's conversation: %w", id, err)
	}
	c.Position = unpacked(position)
	return &c, nil
}

// changeReviewers serves a page of change id's reviewers — named, its web!n —
// in the order of their handles, after the handle a cursor names.
func (r *Reader) changeReviewers(ctx context.Context, tx *sql.Tx, q ReadQuery, named, id string,
	out *ReadAnswer) error {

	limit := min(limitOr(q.Limit, defaultReviewerLimit), r.pageRows)
	query, args := reviewersOfSQL, []any{id}
	if q.Cursor != "" {
		if !org.ValidHandle(q.Cursor) {
			return fmt.Errorf("%w: %q is not a reviewer's handle", ErrBadCursor, q.Cursor)
		}
		query, args = reviewersOfAfterSQL, append(args, q.Cursor)
	}
	rows, err := tx.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return fmt.Errorf("code: read the reviewers of %s: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		var (
			rv                          ReviewerRow
			kind, reason, state, byKind string
			position                    int64
		)
		if err := rows.Scan(&rv.Reviewer, &kind, &reason, &state, &rv.RequestedBy, &byKind, &rv.Note,
			&position); err != nil {
			return fmt.Errorf("code: read the reviewers of %s: %w", named, err)
		}
		rv.Kind, rv.Reason, rv.State = PartyKind(kind), ReviewerReason(reason), ReviewerState(state)
		rv.RequestedByKind, rv.Position = PartyKind(byKind), unpacked(position)
		out.Reviewers = append(out.Reviewers, rv)
		if pg.add(rv.encodedBytes()) {
			out.Next = rv.Reviewer
			break
		}
	}
	return rows.Err()
}

// changeComments serves a page of change id's comments — named, its web!n —
// in thread order, after the comment a cursor names: a [logCursor] of its
// position and its thread.
func (r *Reader) changeComments(ctx context.Context, tx *sql.Tx, q ReadQuery, named, id string,
	out *ReadAnswer) error {

	limit := min(limitOr(q.Limit, defaultCommentLimit), r.pageRows)
	query, args := commentsOfSQL, []any{id}
	if q.Cursor != "" {
		// THE >= BESIDE THE OR keeps the index's range seek past the thread,
		// which a bare OR of the two terms would give up (planner.go).
		position, thread, ok := cutLogCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q is not a comment's place in its change", ErrBadCursor, q.Cursor)
		}
		query, args = commentsOfAfterSQL, append(args, thread, thread, position)
	}
	rows, err := tx.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return fmt.Errorf("code: read the comments of %s: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c            CommentRow
			kind, byKind string
			anchor       string
			position, at int64
		)
		if err := rows.Scan(&c.ID, &c.ThreadID, &c.ParentID, &c.Author, &kind, &c.AuthorOperatorID, &anchor,
			&c.Resolved, &c.ResolvedBy, &byKind, &c.TurnID, &position, &at, &c.Body); err != nil {
			return fmt.Errorf("code: read the comments of %s: %w", named, err)
		}
		c.AuthorKind, c.ResolvedByKind = PartyKind(kind), PartyKind(byKind)
		c.Position, c.At = unpacked(position), store.DecodeTime(at)
		if anchor != "" {
			c.Anchor = &CommentAnchor{}
			if err := json.Unmarshal([]byte(anchor), c.Anchor); err != nil {
				return fmt.Errorf("code: read comment %s of %s: its anchor: %w", c.ID, named, err)
			}
		}
		out.Comments = append(out.Comments, c)
		if pg.add(c.encodedBytes()) {
			out.Next = logCursor(position, c.ThreadID)
			break
		}
	}
	return rows.Err()
}

// The keys of a change's conversation's indexes a read names — a GUARD as
// planner.go says — held by plan_test.go to the seek each is written for.
const (
	// reviewerAsksKey is code_reviewers_reviewer_idx, (reviewer, state,
	// change_id) (replicated 0070): a reviewer's asks by whether they stand,
	// newest change first.
	reviewerAsksKey = "code_reviewers_reviewer_idx"
	// reviewersKey is code_reviewers' PRIMARY KEY index, (change_id,
	// reviewer), which every statement over one change's asks NAMES: an
	// equality on the reviewer beside an ask's state reaches
	// reviewerAsksKey's leading columns, and a batch of asks driven from a
	// list of reviewers sought that index on the reviewer alone — every ask
	// of that reviewer on every change, read to find the one.
	reviewersKey = "sqlite_autoindex_code_reviewers_1"
)

// The reads of a change's conversation, each registered in plan_test.go so
// the pinned driver's planner is held to the index each is written for, and to
// reading in its order: a reviewer's standing asks newest first, a first page
// and a page after a cursor; the changes they name, driven from the list of
// their ids; one change's counters by its key; its reviewers by theirs; and
// its comments in thread order, the >= beside the OR keeping the seek past
// the thread.
const (
	changesAskedSQL = `SELECT change_id FROM code_reviewers INDEXED BY ` + reviewerAsksKey + `
		WHERE reviewer = ? AND state = 'requested' ORDER BY change_id DESC LIMIT ?`
	changesAskedAfterSQL = `SELECT change_id FROM code_reviewers INDEXED BY ` + reviewerAsksKey + `
		WHERE reviewer = ? AND state = 'requested' AND change_id < ? ORDER BY change_id DESC LIMIT ?`
	changesByIDSQL = `SELECT ` + changeColumns + ` FROM code_changes
		WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	conversationCountsSQL = `SELECT comments, version, position FROM code_conversations WHERE change_id = ?`
	reviewersOfSQL        = `SELECT reviewer, reviewer_kind, reason, state, requested_by, requested_by_kind,
			note, position
		FROM code_reviewers INDEXED BY ` + reviewersKey + ` WHERE change_id = ? ORDER BY reviewer LIMIT ?`
	reviewersOfAfterSQL = `SELECT reviewer, reviewer_kind, reason, state, requested_by, requested_by_kind,
			note, position
		FROM code_reviewers INDEXED BY ` + reviewersKey + ` WHERE change_id = ? AND reviewer > ?
		ORDER BY reviewer LIMIT ?`
	commentsOfSQL = `SELECT id, thread_id, parent_id, author, author_kind, author_operator_id, anchor, resolved,
			resolved_by, resolved_by_kind, turn_id, position, at, body
		FROM code_comments WHERE change_id = ? ORDER BY thread_id, position LIMIT ?`
	commentsOfAfterSQL = `SELECT id, thread_id, parent_id, author, author_kind, author_operator_id, anchor,
			resolved, resolved_by, resolved_by_kind, turn_id, position, at, body
		FROM code_comments WHERE change_id = ? AND thread_id >= ? AND (thread_id > ? OR position > ?)
		ORDER BY thread_id, position LIMIT ?`
)
