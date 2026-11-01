package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// THE READS OF A CHANGE'S CONVERSATION: the listings of the changes waiting on
// somebody ([ReadQuery.Reviewer], [ReadQuery.Needs]) and the parts of one
// change's conversation a `change` read pages ([ReadQuery.Part]) — its
// reviewers, its comments, its reviews and its findings — each served inside
// the one transaction [Reader.Read] hands it, from the rows the records on the
// change's own subject wrote (replicated 0095 and 0096), and held to the grant
// on the change's repository as every read of a change is (changeread.go).
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
// The changes that wait on their OWNER's fix, needs=fix — an owner's, the
// asker's own where it names none — walk the owner's changes in ONE live state
// (code_changes_owner_idx): open, or the state it names, since the index keeps
// an owner's changes by state and a walk of two states is no walk of one
// index's order. A change waits on its owner's fix while a reviewer's latest
// verdict asks for changes or a finding stands that none of its authors has
// answered — its conversation's counters (code_conversations
// changes_requested and open_findings), read for the page's changes in one
// statement ([needingFixIn]). A pushed revision answers neither: a request for
// changes stands until its reviewer reviews again, and a finding until an
// author answers it, so what is waiting is said by the rows and never guessed
// from a head that moved. A standing blocker an author has answered waits on
// its raiser, not on the owner (S40), and does not list the change here.
//
// What waits on the ASKER, needs=me, is both, one after the other: the changes
// whose ask of the asker stands, as needs=review lists them, and then the
// asker's own awaiting a fix, as needs=fix does — each its own index's walk, a
// page reading one of them, the fix half's cursor spelled after [fixHalf] — a
// change of both, the asker's own whose review is asked of them (a person may
// be), listed once, by the first.
//
// What waits on A PERSON, needs=person, walks code_mergeability_waits_idx
// (replicated 0101): the changes whose STORED readiness — the answer the last
// mergeability record judged and stored (mergeability.go) — waits on one,
// newest change first by its id, their rows by their keys beside them; and
// keeps those whose readiness the read judges itself, in its own snapshot
// against its own chart, still waits on one, since a stored answer is as of
// the record that stored it. A change that came to wait on a person since its
// last record is listed once its home refreshes it — at the home's next pass,
// a minute later at the most.
//
// # A part of one change
//
// A `change` read names the part it pages: its revisions — the default, with
// the change's detail on their first page (changeread.go) — its reviewers, in
// the order of their handles; its comments, in THREAD order: each thread's
// first comment and then its replies, by the position of the record that said
// each, threads in the order of their ids, which are minted as they start
// (code_comments_thread_idx); its reviews in the order they were given
// (code_reviews_change_idx); or its findings by id — the order the reviews
// that raised them were asked — each finding's text read with it
// (code_finding_texts' key, which leads with the change). Every page names the
// change by its number and is held to the grant again; a page past a cursor
// resumes after the row the cursor names, so a comment said meanwhile in a
// thread already read is read by reading the change again, at the floor its
// write answered with.

// Need is what a listing of changes asks its changes to be waiting on
// ([ReadQuery.Needs]): each a LIVE change's.
type Need string

const (
	// NeedReview is the changes waiting on a review: live, and asking it of
	// the reviewer [ReadQuery.Reviewer] names — of the asker's own seat where
	// it names none.
	NeedReview Need = "review"
	// NeedFix is the changes waiting on their owner's fix: live, owned by
	// the owner [ReadQuery.Owner] names — the asker's own seat where it names
	// none — a reviewer's latest verdict asking for changes or a finding
	// standing that none of its authors has answered.
	NeedFix Need = "fix"
	// NeedMe is the changes waiting on the asker: those asking its review,
	// then its own awaiting a fix. It names no reviewer and no owner.
	NeedMe Need = "me"
	// NeedPerson is the changes waiting on a PERSON: live, their readiness's
	// first unmet precondition waiting on persons alone
	// ([Readiness.WaitsPerson]) — a person's approval, a blocker a person
	// raised and an author answered. Whoever asks, whatever seat they hold:
	// it names no reviewer.
	NeedPerson Need = "person"
)

// Needs is the closed set, in this order.
func Needs() []Need { return []Need{NeedReview, NeedFix, NeedMe, NeedPerson} }

// Valid reports a need this build knows.
func (n Need) Valid() bool { return slices.Contains(Needs(), n) }

// fixHalf spells a needs=me listing's cursor in its second half, the asker's
// own changes awaiting a fix: the half's own cursor follows it, and a cursor
// of the first half — a change's id — never starts so.
const fixHalf = "fix:"

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
	// PartReviews is the change's reviews, in the order they were given.
	PartReviews ChangePart = "reviews"
	// PartFindings is the change's findings, by id, each with its text.
	PartFindings ChangePart = "findings"
)

// ChangeParts is the closed set, in this order.
func ChangeParts() []ChangePart {
	return []ChangePart{PartRevisions, PartReviewers, PartComments, PartReviews, PartFindings}
}

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
	case PartReviews:
		return MaxReviewsPerPage
	case PartFindings:
		return MaxFindingsPerPage
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
	defaultReviewLimit   = 50
	MaxReviewsPerPage    = 200
	defaultFindingLimit  = 50
	MaxFindingsPerPage   = 200
)

// waitedOn is the handle a listing of what its changes wait on names: for
// needs=fix the owner it names, for every other the reviewer it names — or,
// naming none, the asker's own: a seat's handle, or the human seat a person
// holds — and "" for an asker bound to no seat.
func (q ReadQuery) waitedOn() string {
	switch {
	case q.Needs == NeedFix && q.Owner != "":
		return q.Owner
	case q.Needs != NeedFix && q.Needs != NeedMe && q.Reviewer != "":
		return q.Reviewer
	}
	handle, _ := q.Asker.party()
	return handle
}

// fixing is q as the walk of the changes awaiting a fix reads it: its owner
// the one it waits on ([ReadQuery.waitedOn]), and its state open where it
// names none — the one state of an owner's that walk reads.
func (q ReadQuery) fixing() ReadQuery {
	q.Owner = q.waitedOn()
	if q.State == "" {
		q.State = ChangeStateOpen
	}
	return q
}

// changesWaitingOnMe serves a page of needs=me: its first half — the changes
// whose ask of the asker stands, needs=review — until that half's walk ends,
// and then its second, after [fixHalf] — the asker's own changes awaiting a
// fix, needs=fix — each a walk of its own index, so a page reads one half and
// the one that ends the first may be short. A change of both is listed by the
// first half alone.
func (r *Reader) changesWaitingOnMe(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster,
	out *ReadAnswer) error {

	half, fixes := q.halfOfMe()
	if fixes {
		if err := r.listChanges(ctx, tx, half, chart, out, q.waitedOn()); err != nil {
			return err
		}
		if out.Next != "" {
			out.Next = fixHalf + out.Next
		}
		return nil
	}
	if err := r.listChanges(ctx, tx, half, chart, out, ""); err != nil {
		return err
	}
	if out.Next == "" {
		out.Next = fixHalf
	}
	return nil
}

// halfOfMe is the half of q — a needs=me listing — a page of it reads, by its
// cursor: the asker's own changes awaiting a fix, as needs=fix walks them,
// after [fixHalf], with the cursor behind it and true; else the changes
// asking the asker's review, as needs=review walks them. THE ONE READING of
// which half a needs=me cursor is in: the reader's and [ReadQuery.ChangesCursor]'s.
func (q ReadQuery) halfOfMe() (ReadQuery, bool) {
	half := q
	if inner, fixes := strings.CutPrefix(q.Cursor, fixHalf); fixes {
		half.Needs, half.Cursor = NeedFix, inner
		return half.fixing(), true
	}
	half.Needs, half.Reviewer = NeedReview, q.waitedOn()
	return half, false
}

// readByID is one page of a walk of changes by their ids, newest first — the
// changes whose ask of the walk's reviewer stands, or whose stored readiness
// waits on a person: at most limit ids by first, or by after past a cursor —
// each binding the walk's keys, the cursor's id and the limit — and the row
// of each change they name. EVERY ONE: a change's asks and its mergeability go
// before its row in a purge, and with it in the final one. what names the walk
// in a failure's sentence.
func (w changeWalk) readByID(ctx context.Context, tx *sql.Tx, first, after, cursor string, limit int,
	what string) ([]changeRow, error) {

	query, args := first, slices.Clone(w.keys)
	if cursor != "" {
		resumed, ok := w.resume(cursor)
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrBadCursor, cursor)
		}
		query, args = after, append(args, resumed...)
	}
	rows, err := tx.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("code: list the changes %s: %w", what, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("code: list the changes %s: %w", what, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("code: list the changes %s: %w", what, err)
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
			return nil, fmt.Errorf("code: the changes %s name change %s, which the rows do not hold", what, id)
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

// needingFixIn is which of the changes a page read wait on their owner's fix
// — a reviewer's latest verdict asks for changes, or a finding stands that
// none of their authors has answered — less, where asked names a reviewer,
// those whose ask of asked stands, which the first half of a needs=me listing
// lists: one statement over the page's changes for each, a seek of a key a
// change.
func needingFixIn(ctx context.Context, tx *sql.Tx, scanned []changeRow, asked string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(scanned) == 0 {
		return out, nil
	}
	ids := make([]string, len(scanned))
	for i, row := range scanned {
		ids[i] = row.id
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, needingFixSQL, list)
	if err != nil {
		return nil, fmt.Errorf("code: read which of %d changes await a fix: %w", len(ids), err)
	}
	if err := eachID(rows, func(id string) { out[id] = true }); err != nil {
		return nil, fmt.Errorf("code: read which of %d changes await a fix: %w", len(ids), err)
	}
	if asked == "" || len(out) == 0 {
		return out, nil
	}
	if rows, err = tx.QueryContext(ctx, askedAmongSQL, list, asked); err != nil {
		return nil, fmt.Errorf("code: read which of %d changes ask %s's review: %w", len(ids), asked, err)
	}
	if err := eachID(rows, func(id string) { delete(out, id) }); err != nil {
		return nil, fmt.Errorf("code: read which of %d changes ask %s's review: %w", len(ids), asked, err)
	}
	return out, nil
}

// eachID hands each id rows answers to each, and closes rows.
func eachID(rows *sql.Rows, each func(id string)) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		each(id)
	}
	return rows.Err()
}

// conversationRowIn is change id's conversation's counters, nil where nobody
// has discussed it.
func conversationRowIn(ctx context.Context, tx *sql.Tx, id string) (*ConversationRow, error) {
	var (
		c        ConversationRow
		position int64
	)
	err := tx.QueryRowContext(ctx, conversationCountsSQL, id).Scan(&c.Comments, &c.Reviews, &c.OpenFindings,
		&c.OpenBlockers, &c.ChangesRequested, &c.Version, &position)
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
			out.Next = rv.Cursor()
			break
		}
	}
	return rows.Err()
}

// Cursor is the cursor that resumes a page of a change's reviewers after r:
// its handle, past which the next page reads.
func (r ReviewerRow) Cursor() string { return r.Reviewer }

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
			&c.Resolved, &c.ResolvedBy, &byKind, &c.TurnID, &position, &at, &c.Body, &c.FindingID); err != nil {
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
			out.Next = c.Cursor()
			break
		}
	}
	return rows.Err()
}

// Cursor is the cursor that resumes a page of a change's comments after c: a
// [logCursor] of the position its record landed at and its thread, past which
// the next page reads in thread order.
func (c CommentRow) Cursor() string { return logCursor(c.Position.Packed(), c.ThreadID) }

// changeReviews serves a page of change id's reviews — named, its web!n — in
// the order they were given, after the review a cursor names: the packed
// position of its record, which is its own, since a record gives one review.
func (r *Reader) changeReviews(ctx context.Context, tx *sql.Tx, q ReadQuery, named, id string,
	out *ReadAnswer) error {

	limit := min(limitOr(q.Limit, defaultReviewLimit), r.pageRows)
	query, args := reviewsOfSQL, []any{id}
	if q.Cursor != "" {
		position, err := strconv.ParseInt(q.Cursor, 10, 64)
		if err != nil || position < 1 {
			return fmt.Errorf("%w: %q is not a review's place in its change", ErrBadCursor, q.Cursor)
		}
		query, args = reviewsOfAfterSQL, append(args, position)
	}
	rows, err := tx.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return fmt.Errorf("code: read the reviews of %s: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		var (
			rv            ReviewRow
			kind, verdict string
			position, at  int64
		)
		if err := rows.Scan(&rv.ID, &rv.Revision, &rv.Reviewer, &kind, &rv.OperatorID, &verdict, &rv.Findings,
			&rv.TurnID, &rv.Model, &position, &at, &rv.Summary); err != nil {
			return fmt.Errorf("code: read the reviews of %s: %w", named, err)
		}
		rv.ReviewerKind, rv.Verdict = PartyKind(kind), Verdict(verdict)
		rv.Position, rv.At = unpacked(position), store.DecodeTime(at)
		out.Reviews = append(out.Reviews, rv)
		if pg.add(rv.encodedBytes()) {
			out.Next = rv.Cursor()
			break
		}
	}
	return rows.Err()
}

// Cursor is the cursor that resumes a page of a change's reviews after r: the
// packed position of the record that gave it, a review's own, past which the
// next page reads.
func (r ReviewRow) Cursor() string { return strconv.FormatInt(r.Position.Packed(), 10) }

// changeFindings serves a page of change id's findings — named, its web!n —
// by id, after the finding a cursor names: the texts' key walked, which leads
// with the change, and each finding's narrow row read by its key beside its
// text.
func (r *Reader) changeFindings(ctx context.Context, tx *sql.Tx, q ReadQuery, named, id string,
	out *ReadAnswer) error {

	limit := min(limitOr(q.Limit, defaultFindingLimit), r.pageRows)
	query, args := findingsOfSQL, []any{id}
	if q.Cursor != "" {
		if bareUUIDv7("cursor", q.Cursor) != nil {
			return fmt.Errorf("%w: %q is not a finding's id", ErrBadCursor, q.Cursor)
		}
		query, args = findingsOfAfterSQL, append(args, q.Cursor)
	}
	rows, err := tx.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return fmt.Errorf("code: read the findings of %s: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		var (
			f                                             FindingRow
			kind, side, severity, category, state, byKind string
			resolved, position                            int64
			suggestion                                    sql.NullString
		)
		if err := rows.Scan(&f.ID, &f.ReviewID, &f.Raiser, &kind, &f.Revision, &f.CommitOID, &f.Path, &f.BlobOID,
			&side, &f.LineStart, &f.LineEnd, &severity, &category, &state, &f.ResolutionNote, &f.ResolvedBy,
			&byKind, &f.ResolvedCommit, &resolved, &position, &f.Title, &f.Body, &suggestion); err != nil {
			return fmt.Errorf("code: read the findings of %s: %w", named, err)
		}
		f.RaiserKind, f.Side, f.Severity, f.Category = PartyKind(kind), Side(side), Severity(severity),
			Category(category)
		f.State, f.ResolvedByKind, f.Position = FindingState(state), PartyKind(byKind), unpacked(position)
		if resolved != 0 {
			f.ResolvedPosition = unpacked(resolved)
		}
		if suggestion.Valid {
			f.Suggestion = &suggestion.String
		}
		out.Findings = append(out.Findings, f)
		if pg.add(f.encodedBytes()) {
			out.Next = f.Cursor()
			break
		}
	}
	return rows.Err()
}

// Cursor is the cursor that resumes a page of a change's findings after f: its
// id, past which the next page reads.
func (f FindingRow) Cursor() string { return f.ID }

// The keys of a change's conversation's indexes a read names — a GUARD as
// planner.go says — held by plan_test.go to the seek each is written for.
const (
	// reviewerAsksKey is code_reviewers_reviewer_idx, (reviewer, state,
	// change_id) (replicated 0095): a reviewer's asks by whether they stand,
	// newest change first.
	reviewerAsksKey = "code_reviewers_reviewer_idx"
	// reviewersKey is code_reviewers' PRIMARY KEY index, (change_id,
	// reviewer), which every statement over one change's asks NAMES: an
	// equality on the reviewer beside an ask's state reaches
	// reviewerAsksKey's leading columns, and a batch of asks driven from a
	// list of reviewers sought that index on the reviewer alone — every ask
	// of that reviewer on every change, read to find the one.
	reviewersKey = "sqlite_autoindex_code_reviewers_1"
	// waitsKey is code_mergeability_waits_idx, (waits_person, change_id)
	// WHERE waits_person = 1 (replicated 0101): the changes whose stored
	// readiness waits on a person, newest change first.
	waitsKey = "code_mergeability_waits_idx"
)

// The reads of a change's conversation, each registered in plan_test.go so
// the pinned driver's planner is held to the index each is written for, and to
// reading in its order: a reviewer's standing asks newest first, a first page
// and a page after a cursor; the changes whose stored readiness waits on a
// person, the same; the changes they name, driven from the list of
// their ids; which of a page's changes await their owner's fix, and which of
// them ask the asker's review, driven from that list too; one change's
// counters by its key; its reviewers by theirs; its comments in thread order,
// the >= beside the OR keeping the seek past the thread; its reviews in the
// order given; and its findings by id, the texts' key walked and each
// finding's sought by its own.
const (
	changesAskedSQL = `SELECT change_id FROM code_reviewers INDEXED BY ` + reviewerAsksKey + `
		WHERE reviewer = ? AND state = 'requested' ORDER BY change_id DESC LIMIT ?`
	changesAskedAfterSQL = `SELECT change_id FROM code_reviewers INDEXED BY ` + reviewerAsksKey + `
		WHERE reviewer = ? AND state = 'requested' AND change_id < ? ORDER BY change_id DESC LIMIT ?`
	// THE CHANGES WAITING ON A PERSON, the index's partial predicate repeated
	// verbatim: the pinned planner takes a partial index only for a statement
	// that implies it in its own words.
	changesWaitingSQL = `SELECT change_id FROM code_mergeability INDEXED BY ` + waitsKey + `
		WHERE waits_person = 1 ORDER BY change_id DESC LIMIT ?`
	changesWaitingAfterSQL = `SELECT change_id FROM code_mergeability INDEXED BY ` + waitsKey + `
		WHERE waits_person = 1 AND change_id < ? ORDER BY change_id DESC LIMIT ?`
	changesByIDSQL = `SELECT ` + changeColumns + ` FROM code_changes
		WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	needingFixSQL = `SELECT v.change_id FROM json_each(?) AS j CROSS JOIN code_conversations AS v
		WHERE v.change_id = CAST(j.value AS TEXT) AND (v.changes_requested > 0 OR v.open_findings > 0)`
	askedAmongSQL = `SELECT r.change_id FROM json_each(?1) AS j
		CROSS JOIN code_reviewers AS r INDEXED BY ` + reviewersKey + `
		WHERE r.change_id = CAST(j.value AS TEXT) AND r.reviewer = ?2 AND r.state = 'requested'`
	conversationCountsSQL = `SELECT comments, reviews, open_findings, open_blockers, changes_requested, version,
			position
		FROM code_conversations WHERE change_id = ?`
	reviewersOfSQL = `SELECT reviewer, reviewer_kind, reason, state, requested_by, requested_by_kind,
			note, position
		FROM code_reviewers INDEXED BY ` + reviewersKey + ` WHERE change_id = ? ORDER BY reviewer LIMIT ?`
	reviewersOfAfterSQL = `SELECT reviewer, reviewer_kind, reason, state, requested_by, requested_by_kind,
			note, position
		FROM code_reviewers INDEXED BY ` + reviewersKey + ` WHERE change_id = ? AND reviewer > ?
		ORDER BY reviewer LIMIT ?`
	commentsOfSQL = `SELECT id, thread_id, parent_id, author, author_kind, author_operator_id, anchor, resolved,
			resolved_by, resolved_by_kind, turn_id, position, at, body, finding_id
		FROM code_comments WHERE change_id = ? ORDER BY thread_id, position LIMIT ?`
	commentsOfAfterSQL = `SELECT id, thread_id, parent_id, author, author_kind, author_operator_id, anchor,
			resolved, resolved_by, resolved_by_kind, turn_id, position, at, body, finding_id
		FROM code_comments WHERE change_id = ? AND thread_id >= ? AND (thread_id > ? OR position > ?)
		ORDER BY thread_id, position LIMIT ?`
	reviewsOfSQL = `SELECT id, revision, reviewer, reviewer_kind, operator_id, verdict, findings, turn_id, model,
			position, at, summary
		FROM code_reviews WHERE change_id = ? ORDER BY position LIMIT ?`
	reviewsOfAfterSQL = `SELECT id, revision, reviewer, reviewer_kind, operator_id, verdict, findings, turn_id,
			model, position, at, summary
		FROM code_reviews WHERE change_id = ? AND position > ? ORDER BY position LIMIT ?`
	findingsOfSQL = `SELECT f.id, f.review_id, f.raiser, f.raiser_kind, f.revision, f.commit_oid, f.path,
			f.blob_oid, f.side, f.line_start, f.line_end, f.severity, f.category, f.state, f.resolution_note,
			f.resolved_by, f.resolved_by_kind, f.resolved_commit, f.resolved_position, f.position, x.title,
			x.body, x.suggestion
		FROM code_finding_texts AS x CROSS JOIN code_findings AS f
		WHERE x.change_id = ? AND f.id = x.finding_id ORDER BY x.finding_id LIMIT ?`
	findingsOfAfterSQL = `SELECT f.id, f.review_id, f.raiser, f.raiser_kind, f.revision, f.commit_oid, f.path,
			f.blob_oid, f.side, f.line_start, f.line_end, f.severity, f.category, f.state, f.resolution_note,
			f.resolved_by, f.resolved_by_kind, f.resolved_commit, f.resolved_position, f.position, x.title,
			x.body, x.suggestion
		FROM code_finding_texts AS x CROSS JOIN code_findings AS f
		WHERE x.change_id = ? AND x.finding_id > ? AND f.id = x.finding_id ORDER BY x.finding_id LIMIT ?`
)

// reviewsGiven serves a page of ONE REVIEWER'S REVIEWS — the `reviews`
// listing, what a seat's or a person's "reviews given" reads — newest first,
// after the review a cursor names, across every change of every repository
// the asker may read: one range of code_reviews_reviewer_idx (replicated
// 0113), the changes the page names read by their keys in one statement, and
// their repositories judged once a page against the grant ([listedReposAmong]),
// so a review of a change on a repository the asker may not read is left out
// as a listing of changes leaves the change out. A page READS its limit of
// reviews, whatever few of them the grant keeps, and resumes after the last
// it read.
func (r *Reader) reviewsGiven(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	limit := min(limitOr(q.Limit, defaultReviewLimit), r.pageRows)
	query, args := reviewsGivenSQL, []any{q.Reviewer}
	if q.Cursor != "" {
		position, err := strconv.ParseInt(q.Cursor, 10, 64)
		if err != nil || position < 1 {
			return fmt.Errorf("%w: %q is not a review's place on the code log", ErrBadCursor, q.Cursor)
		}
		query, args = reviewsGivenAfterSQL, append(args, position)
	}
	type given struct {
		review ReviewRow
		change string
	}
	read := func() ([]given, error) {
		rows, err := tx.QueryContext(ctx, query, append(args, limit)...)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var scanned []given
		for rows.Next() {
			var (
				g             given
				kind, verdict string
				position, at  int64
			)
			if err := rows.Scan(&g.review.ID, &g.change, &g.review.Revision, &g.review.Reviewer, &kind,
				&g.review.OperatorID, &verdict, &g.review.Findings, &g.review.TurnID, &g.review.Model, &position,
				&at, &g.review.Summary); err != nil {
				return nil, err
			}
			g.review.ReviewerKind, g.review.Verdict = PartyKind(kind), Verdict(verdict)
			g.review.Position, g.review.At = unpacked(position), store.DecodeTime(at)
			scanned = append(scanned, g)
		}
		return scanned, rows.Err()
	}
	scanned, err := read()
	if err != nil {
		return fmt.Errorf("code: read the reviews %s gave: %w", q.Reviewer, err)
	}
	ids := make([]string, 0, len(scanned))
	for _, g := range scanned {
		ids = append(ids, g.change)
	}
	changes, err := changesByIDIn(ctx, tx, ids)
	if err != nil {
		return err
	}
	repoIDs := make([]string, 0, len(changes))
	for _, c := range changes {
		repoIDs = append(repoIDs, c.repoID)
	}
	repos, err := listedReposAmong(ctx, tx, repoIDs, q.Asker, q.CeilingSeat, chart, "a page of reviews")
	if err != nil {
		return err
	}
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for _, g := range scanned {
		c, held := changes[g.change]
		repo, listed := repos[c.repoID]
		if !held || !listed {
			continue
		}
		rv := g.review
		rv.Change = &ChangeName{Repo: repo.ref.Name, Ref: ChangeRef(repo.ref.Name, c.number), Title: c.title,
			State: c.state}
		out.Reviews = append(out.Reviews, rv)
		if pg.add(rv.encodedBytes()) {
			out.Next = rv.Cursor()
			return nil
		}
	}
	if len(scanned) == limit {
		out.Next = scanned[len(scanned)-1].review.Cursor()
	}
	return nil
}

// validateReviews is [ReadQuery.validate] for a `reviews` listing: the
// reviewer whose reviews it lists, a seat's handle — or the human seat a
// person holds — REQUIRED, since the listing walks one reviewer's index and
// no index holds every review the company has.
func (q ReadQuery) validateReviews() error {
	switch {
	case q.Reviewer == "":
		return fmt.Errorf("%w: reviewer: a listing of reviews lists one reviewer's, by the seat's handle or "+
			"the human seat a person holds", ErrInvalid)
	case !org.ValidHandle(q.Reviewer):
		return fmt.Errorf("%w: reviewer: %q is not a seat's handle: a reviewer is a seat, or the human seat a "+
			"person holds", ErrInvalid, q.Reviewer)
	}
	return nil
}

// reviewsGivenKey is code_reviews_reviewer_idx, (reviewer, position)
// (replicated 0113): one reviewer's reviews in the order their records landed
// — a GUARD as planner.go says, held by plan_test.go to its seek.
const reviewsGivenKey = "code_reviews_reviewer_idx"

// One reviewer's reviews newest first, a first page and a page below the
// position a cursor names: one range of reviewsGivenKey walked backward.
// Registered in plan_test.go.
const (
	reviewsGivenSQL = `SELECT id, change_id, revision, reviewer, reviewer_kind, operator_id, verdict, findings,
			turn_id, model, position, at, summary
		FROM code_reviews INDEXED BY ` + reviewsGivenKey + ` WHERE reviewer = ? ORDER BY position DESC LIMIT ?`
	reviewsGivenAfterSQL = `SELECT id, change_id, revision, reviewer, reviewer_kind, operator_id, verdict,
			findings, turn_id, model, position, at, summary
		FROM code_reviews INDEXED BY ` + reviewsGivenKey + ` WHERE reviewer = ? AND position < ?
		ORDER BY position DESC LIMIT ?`
)
