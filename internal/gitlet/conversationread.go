package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE READS OF A CHANGE'S CONVERSATION: the listings of the changes waiting on
// somebody ([ReadQuery.Reviewer], [ReadQuery.Needs]) and the parts of one
// change a `change` read pages ([ReadQuery.Part]), each inside the one
// transaction [Reader.Read] hands it, from the rows of the change's own
// subject (replicated 0100 and 0101), and held to the grant on the change's
// repository as every read of a change is (changeread.go). The package doc's
// "A change's conversation is rows on its own subject" has the model.

// WHO A CHANGE WAITS ON. needs=review (the asker's where it names no
// reviewer) walks code_reviewers_reviewer_idx, which leads with the ask's
// state after the reviewer, so a page reads standing asks only
// ([changeWalk.readAsked]). An ask of a closed or merged change still stands
// (the lifecycle is the repository's records, the asks the change's
// subject's), so needs=review keeps a LIVE change by testing each row read.
//
// needs=fix (the asker's own where it names no owner) walks the owner's
// changes in ONE state, open or the one named (code_changes_owner_idx: two
// states are no one index order), keeping those whose changes_requested or
// open_findings counter is set ([needingFixIn]). A pushed revision answers
// neither; an answered standing blocker waits on its raiser (S40).

// needs=me is needs=review, then needs=fix, each its own index's walk, the fix
// half's cursor prefixed with [fixHalf], a change in both listed by the first; a
// page the review walk ends on fills from the fix walk, each row naming its half
// ([ChangeSummary.WaitsFor]), which spells the cursor after it.
//
// needs=person walks code_mergeability_waits_idx (replicated 0106), the
// STORED readiness (mergeability.go), and keeps those the read's own judgment
// against its own chart still finds waiting, since a stored answer is as of
// its record. A change that came to wait since (a review, an ask, a chart
// change: Mergeability.ChartEpoch) is listed once its home refreshes it, a
// minute later at most once the home and the repository's committing data
// node hold that chart. An ended change stores no answer (clearReadiness).

// A PART OF ONE CHANGE. Revisions (the default, the detail on the first page,
// changeread.go); reviewers by handle; comments in THREAD order, each
// thread's first comment then its replies by record position, threads by
// their ids, minted as they start (code_comments_thread_idx); reviews as given
// (code_reviews_change_idx); findings by id, each with its text
// (code_finding_texts' key leads with the change). Every page is held to the
// grant again and resumes after the row its cursor names, so a comment said
// meanwhile in a thread already read is read by reading the change again.

// THE ASKER'S STANDING FINDINGS, the `standing` part: the asker's open or
// answered-but-unjudged findings by id, exactly the set their review carries
// or supersedes ([Review.Carries]). The findings part pages oldest first, so
// a reviewer's newest standing findings sat past its first page, superseded
// unseen by a review sent from it. It reads what the review's decide reads
// (standingFindingsSQL, code_findings_raiser_idx), at most
// [MaxStandingFindings] narrow rows, texts by key ([findingsNamed]); an asker
// bound to no seat is answered none.

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
	// PartStanding is the ASKER's own standing findings on the change, by
	// id, each with its text: what a review of theirs carries. An older build
	// refuses it as an unknown part rather than answer another set; it is an
	// estate request's word, no record's, so it takes no versionedFields row.
	PartStanding ChangePart = "standing"
	// PartMoves is the moves of the change's lifecycle, oldest first
	// ([ChangeMove]): who closed, reopened, drafted, readied, edited or
	// retargeted it. An older build refuses it as an unknown part, as it does
	// [PartStanding].
	PartMoves ChangePart = "moves"
	// PartQueue is the change's queue entries, in queue order
	// ([ChangeQueueEntry]): who added it to its target's queue, and how each
	// entry landed, was taken out or left. An older build refuses it as an
	// unknown part.
	PartQueue ChangePart = "queue"
)

// ChangeParts is the closed set, in this order.
func ChangeParts() []ChangePart {
	return []ChangePart{PartRevisions, PartReviewers, PartComments, PartReviews, PartFindings, PartStanding,
		PartMoves, PartQueue}
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
	case PartFindings, PartStanding:
		return MaxFindingsPerPage
	case PartMoves:
		return MaxMovesPerPage
	case PartQueue:
		return MaxQueueEntriesPerPage
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
	// A change makes a handful of moves, each about 150 bytes encoded: a
	// first page of 100 is a change's whole timeline but a pathological one's,
	// and 500, some 75 KB, is under a page's bytes.
	defaultMoveLimit = 100
	MaxMovesPerPage  = 500
	// A change is enqueued a few times at most, each entry a few hundred
	// bytes (a conflict's paths, at most MaxEjectedPaths, its widest): 50 is
	// a change's whole history in the queue, 200 a page's bytes.
	defaultQueueEntryLimit = 50
	MaxQueueEntriesPerPage = 200
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

// changesWaitingOnMe serves a page of needs=me: the needs=review half until
// its walk ends, then the needs=fix half after [fixHalf]. Each half walks its
// own index; a page whose review walk ends goes on into the fix walk with
// what is left of its rows and bytes, so it is short only where the fix walk
// is too. A change in both is listed by the first alone.
func (r *Reader) changesWaitingOnMe(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster,
	cp statelog.Position, out *ReadAnswer) error {

	half, fixes := q.halfOfMe()
	if !fixes {
		if err := r.listChanges(ctx, tx, half, chart, cp, out, "", NeedReview); err != nil {
			return err
		}
		if out.Next != "" {
			return nil
		}
		// THE REVIEW WALK ENDED on this page: the fix walk, from its start, takes what is left.
		if pg := r.changesPage(q, out.Changes); pg.full() {
			out.Next = fixHalf
			return nil
		}
		next := q
		next.Cursor = fixHalf
		half, _ = next.halfOfMe()
	}
	if err := r.listChanges(ctx, tx, half, chart, cp, out, q.waitedOn(), NeedFix); err != nil {
		return err
	}
	if out.Next != "" {
		out.Next = fixHalf + out.Next
	}
	return nil
}

// halfOfMe is the half of needs=me listing q a page reads, by its cursor:
// the needs=fix half and true after [fixHalf], else the needs=review half.
// THE ONE READING of which half a needs=me cursor is in: the reader's and
// [ReadQuery.ChangesCursor]'s.
func (q ReadQuery) halfOfMe() (ReadQuery, bool) {
	half := q
	if inner, fixes := strings.CutPrefix(q.Cursor, fixHalf); fixes {
		half.Needs, half.Cursor = NeedFix, inner
		return half.fixing(), true
	}
	half.Needs, half.Reviewer = NeedReview, q.waitedOn()
	return half, false
}

// readByID is one page of a walk of changes by their ids, newest first (a
// reviewer's standing asks, or stored readiness): at most limit ids by first,
// or by after past a cursor (each binding the walk's keys, the cursor's id and
// the limit), and the row of each. A missing row is an error: a purge removes
// a change's asks and mergeability before or with its row. what names the
// walk in a failure's sentence.
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
		return nil, fmt.Errorf("gitlet: list the changes %s: %w", what, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("gitlet: list the changes %s: %w", what, err)
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("gitlet: list the changes %s: %w", what, err)
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
			return nil, fmt.Errorf("gitlet: the changes %s name change %s, which the rows do not hold", what, id)
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
		return nil, fmt.Errorf("gitlet: read %d changes by id: %w", len(ids), err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]changeRow, len(ids))
	for rows.Next() {
		row, err := scanChange(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gitlet: read %d changes by id: %w", len(ids), err)
		}
		out[row.id] = row
	}
	return out, rows.Err()
}

// needingFixIn is which of a page's changes wait on their owner's fix (their
// counters ask for changes or hold an unanswered finding), less, where asked
// names a reviewer, those whose ask of asked stands, which needs=me's first
// half lists: one statement for each, a key seek a change.
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
		return nil, fmt.Errorf("gitlet: read which of %d changes await a fix: %w", len(ids), err)
	}
	if err = eachID(rows, func(id string) { out[id] = true }); err != nil {
		return nil, fmt.Errorf("gitlet: read which of %d changes await a fix: %w", len(ids), err)
	}
	if asked == "" || len(out) == 0 {
		return out, nil
	}
	if rows, err = tx.QueryContext(ctx, askedAmongSQL, list, asked); err != nil {
		return nil, fmt.Errorf("gitlet: read which of %d changes ask %s's review: %w", len(ids), asked, err)
	}
	if err := eachID(rows, func(id string) { delete(out, id) }); err != nil {
		return nil, fmt.Errorf("gitlet: read which of %d changes ask %s's review: %w", len(ids), asked, err)
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
// has discussed it, and the broker instant (store.EncodeTime) of its last
// record, 0 where none ([ConversationRow.SpokenAt]): what a change read dates
// the change by, from this one row rather than again by [withConversations].
func conversationRowIn(ctx context.Context, tx *sql.Tx, id string) (*ConversationRow, int64, error) {
	var (
		c                ConversationRow
		position, spoken int64
	)
	err := tx.QueryRowContext(ctx, conversationCountsSQL, id).Scan(&c.Comments, &c.Reviews, &c.OpenFindings,
		&c.OpenBlockers, &c.ChangesRequested, &c.Version, &position, &spoken)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, 0, nil
	case err != nil:
		return nil, 0, fmt.Errorf("gitlet: read change %s's conversation: %w", id, err)
	}
	c.Position = unpacked(position)
	if spoken > 0 {
		c.SpokenAt = store.DecodeTime(spoken)
	}
	return &c, spoken, nil
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
		return fmt.Errorf("gitlet: read the reviewers of %s: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	base, packed := len(out.Reviewers), []int64(nil)
	for rows.Next() {
		var (
			rv                                 ReviewerRow
			kind, reason, state, byKind, owned string
			position                           int64
		)
		if scanErr := rows.Scan(&rv.Reviewer, &kind, &reason, &state, &rv.RequestedBy, &byKind, &rv.Note,
			&position, &owned, &rv.OwnedPathsOmitted); scanErr != nil {
			return fmt.Errorf("gitlet: read the reviewers of %s: %w", named, scanErr)
		}
		if owned != "" {
			if globsErr := json.Unmarshal([]byte(owned), &rv.OwnedPaths); globsErr != nil {
				return fmt.Errorf("gitlet: read the reviewers of %s: an ask's globs: %w", named, globsErr)
			}
		}
		rv.Kind, rv.Reason, rv.State = PartyKind(kind), ReviewerReason(reason), ReviewerState(state)
		// Costed at its widest instant, which is read after the page is cut.
		rv.RequestedByKind, rv.Position, rv.At = PartyKind(byKind), unpacked(position), widestInstant
		out.Reviewers = append(out.Reviewers, rv)
		packed = append(packed, position)
		if pg.add(rv.encodedBytes()) {
			out.Next = rv.Cursor()
			break
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("gitlet: read the reviewers of %s: %w", named, err)
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("gitlet: read the reviewers of %s: %w", named, err)
	}
	moved, err := movedAtIn(ctx, tx, id, packed)
	if err != nil {
		return fmt.Errorf("gitlet: read when the reviewers of %s last moved: %w", named, err)
	}
	for k, position := range packed {
		out.Reviewers[base+k].At = moved[position]
	}
	return nil
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
		return fmt.Errorf("gitlet: read the comments of %s: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c                          CommentRow
			kind, byKind, deletedKind  string
			anchor                     string
			position, at, edited, gone int64
		)
		if err := rows.Scan(&c.ID, &c.ThreadID, &c.ParentID, &c.Author, &kind, &c.AuthorOperatorID, &anchor,
			&c.Resolved, &c.ResolvedBy, &byKind, &c.TurnID, &position, &at, &c.Body, &c.FindingID, &edited, &gone,
			&c.DeletedBy, &deletedKind); err != nil {
			return fmt.Errorf("gitlet: read the comments of %s: %w", named, err)
		}
		c.AuthorKind, c.ResolvedByKind, c.DeletedByKind = PartyKind(kind), PartyKind(byKind), PartyKind(deletedKind)
		c.Position, c.At = unpacked(position), store.DecodeTime(at)
		if edited != 0 {
			c.EditedAt = store.DecodeTime(edited)
		}
		if gone != 0 {
			c.DeletedAt = store.DecodeTime(gone)
		}
		if anchor != "" {
			c.Anchor = &CommentAnchor{}
			if err := json.Unmarshal([]byte(anchor), c.Anchor); err != nil {
				return fmt.Errorf("gitlet: read comment %s of %s: its anchor: %w", c.ID, named, err)
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
		return fmt.Errorf("gitlet: read the reviews of %s: %w", named, err)
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
			return fmt.Errorf("gitlet: read the reviews of %s: %w", named, err)
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

// ValidFindingID reports whether id is a finding's id as a review's record
// mints one: a UUIDv7 in its canonical lowercase spelling — the grammar a read
// narrowed to findings holds each id to ([ReadQuery.FindingIDs]), so a caller
// can tell an id no change holds from a read it would have refused.
func ValidFindingID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.Version() == 7 && u.String() == id
}

// validateFindingIDs is [ReadQuery.validate] for the findings a read is
// narrowed to ([ReadQuery.FindingIDs]): none, or 1 to [MaxFindingsPerReview]
// findings' ids, each a UUIDv7 in its canonical spelling, each once.
func validateFindingIDs(ids []string) error {
	if len(ids) > MaxFindingsPerReview {
		return refusedf("%w: finding_ids: %d findings, past the %d one read names (MaxFindingsPerReview)",
			ErrInvalid, len(ids), MaxFindingsPerReview)
	}
	for i, id := range ids {
		if !ValidFindingID(id) {
			return fmt.Errorf("%w: finding_ids[%d]: %q is not a finding's id, a UUIDv7 in its canonical "+
				"lowercase spelling", ErrInvalid, i, id)
		}
		if slices.Contains(ids[:i], id) {
			return fmt.Errorf("%w: finding_ids[%d]: %q is named twice", ErrInvalid, i, id)
		}
	}
	return nil
}

// changeFindings serves a page of change id's findings — named, its web!n —
// by id, after the finding a cursor names: the texts' key walked, which leads
// with the change, and each finding's narrow row read by its key beside its
// text; or, for a read narrowed to the findings it names
// ([ReadQuery.FindingIDs]), those of them after the cursor, each one seek of
// the texts' key and one of the row's.
func (r *Reader) changeFindings(ctx context.Context, tx *sql.Tx, q ReadQuery, named, id string,
	out *ReadAnswer) error {

	if q.Cursor != "" && bareUUIDv7("cursor", q.Cursor) != nil {
		return fmt.Errorf("%w: %q is not a finding's id", ErrBadCursor, q.Cursor)
	}
	if len(q.FindingIDs) > 0 {
		return r.namedFindings(ctx, tx, q, named, id, out)
	}
	limit := min(limitOr(q.Limit, defaultFindingLimit), r.pageRows)
	query, args := findingsOfSQL, []any{id, limit}
	if q.Cursor != "" {
		query, args = findingsOfAfterSQL, []any{id, q.Cursor, limit}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("gitlet: read the findings of %s: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return fmt.Errorf("gitlet: read the findings of %s: %w", named, err)
		}
		out.Findings = append(out.Findings, f)
		if pg.add(f.encodedBytes()) {
			out.Next = f.Cursor()
			break
		}
	}
	return rows.Err()
}

// namedFindings serves the findings of change id — named, its web!n — that a
// read is narrowed to ([ReadQuery.FindingIDs]), past its cursor, in id order
// so a cursor resumes them as it resumes the part: ONE statement
// ([findingsNamed]), whatever the change's review history holds, reading at
// most MaxFindingsPerReview rows. The page's bytes may fill before that, with
// findings whose suggestions reach their bound; the cursor then names the
// last one carried, never one past the last named.
func (r *Reader) namedFindings(ctx context.Context, tx *sql.Tx, q ReadQuery, named, id string,
	out *ReadAnswer) error {

	asked := slices.DeleteFunc(slices.Clone(q.FindingIDs), func(f string) bool {
		return q.Cursor != "" && f <= q.Cursor
	})
	read, err := findingsNamed(ctx, tx, named, id, asked)
	if err != nil {
		return err
	}
	r.pageFindings(read, false, out)
	return nil
}

// changeStanding serves a page of the findings the ASKER keeps standing on
// change id — named, its web!n — by id, after a cursor ([PartStanding]): all
// of theirs read as their review's decide reads them (standingFindings, at
// most [MaxStandingFindings] narrow rows), those past the cursor sorted by id,
// the first `limit` read whole ([findingsNamed]). The cursor names the last
// one carried wherever one remains, past the limit or past the page's bytes.
func (r *Reader) changeStanding(ctx context.Context, tx *sql.Tx, q ReadQuery, named, id string,
	out *ReadAnswer) error {

	if q.Cursor != "" && bareUUIDv7("cursor", q.Cursor) != nil {
		return fmt.Errorf("%w: %q is not a finding's id", ErrBadCursor, q.Cursor)
	}
	raiser, kind := q.Asker.party()
	if raiser == "" {
		return nil
	}
	standing, err := standingFindings(ctx, tx, id, raiser, kind)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(standing))
	for _, f := range standing {
		if q.Cursor == "" || f.id > q.Cursor {
			ids = append(ids, f.id)
		}
	}
	slices.Sort(ids)
	limit := min(limitOr(q.Limit, defaultFindingLimit), r.pageRows)
	more := len(ids) > limit
	read, err := findingsNamed(ctx, tx, named, id, ids[:min(len(ids), limit)])
	if err != nil {
		return err
	}
	r.pageFindings(read, more, out)
	return nil
}

// findingsNamed is the findings of change id — named, its web!n — whose ids
// asked lists, each with its text, in id order: ONE statement
// (findingsNamedSQL), each id one seek of the texts' key beside the change
// and one of the narrow row's key. An id the change does not hold is no row.
func findingsNamed(ctx context.Context, tx *sql.Tx, named, id string, asked []string) ([]FindingRow, error) {
	if len(asked) == 0 {
		return nil, nil
	}
	list, err := json.Marshal(asked)
	if err != nil {
		return nil, fmt.Errorf("gitlet: name the findings of %s: %w", named, err)
	}
	rows, err := tx.QueryContext(ctx, findingsNamedSQL, string(list), id)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the named findings of %s: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	var read []FindingRow
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, fmt.Errorf("gitlet: read the named findings of %s: %w", named, err)
		}
		read = append(read, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read the named findings of %s: %w", named, err)
	}
	slices.SortFunc(read, func(a, b FindingRow) int { return strings.Compare(a.ID, b.ID) })
	return read, nil
}

// pageFindings puts read — findings in id order — on out's page until the
// page's bytes are full, the cursor naming the last one it carries where one
// is left behind it: a finding of read the bytes cut, or, where more says so,
// one past read itself.
func (r *Reader) pageFindings(read []FindingRow, more bool, out *ReadAnswer) {
	pg := page{maxRows: len(read), maxB: r.pageBytes}
	for i, f := range read {
		out.Findings = append(out.Findings, f)
		if pg.add(f.encodedBytes()) && i < len(read)-1 {
			out.Next = f.Cursor()
			return
		}
	}
	if more && len(read) > 0 {
		out.Next = read[len(read)-1].Cursor()
	}
}

// scanFinding is one row of findingsOfSQL, findingsOfAfterSQL or
// findingsNamedSQL: a finding's narrow row and its text.
func scanFinding(rows *sql.Rows) (FindingRow, error) {
	var (
		f                                             FindingRow
		kind, side, severity, category, state, byKind string
		resolved, position, raised                    int64
		suggestion                                    sql.NullString
	)
	if err := rows.Scan(&f.ID, &f.ReviewID, &f.Raiser, &kind, &f.Revision, &f.CommitOID, &f.Path, &f.BlobOID,
		&side, &f.LineStart, &f.LineEnd, &severity, &category, &state, &f.ResolutionNote, &f.ResolvedBy,
		&byKind, &f.ResolvedCommit, &resolved, &position, &f.Title, &f.Body, &suggestion, &raised); err != nil {
		return FindingRow{}, err
	}
	f.RaisedAt = store.DecodeTime(raised)
	f.RaiserKind, f.Side, f.Severity, f.Category = PartyKind(kind), Side(side), Severity(severity),
		Category(category)
	f.State, f.ResolvedByKind, f.Position = FindingState(state), PartyKind(byKind), unpacked(position)
	if resolved != 0 {
		f.ResolvedPosition = unpacked(resolved)
	}
	if suggestion.Valid {
		f.Suggestion = &suggestion.String
	}
	return f, nil
}

// Cursor is the cursor that resumes a page of a change's findings after f: its
// id, past which the next page reads.
func (f FindingRow) Cursor() string { return f.ID }

// The keys of a change's conversation's indexes a read names — a GUARD as
// planner.go says — held by plan_test.go to the seek each is written for.
const (
	// reviewerAsksKey is code_reviewers_reviewer_idx, (reviewer, state,
	// change_id) (replicated 0100): a reviewer's asks by whether they stand,
	// newest change first.
	reviewerAsksKey = "code_reviewers_reviewer_idx"
	// reviewersKey is code_reviewers' PRIMARY KEY index, (change_id,
	// reviewer), which every statement over one change's asks NAMES: an
	// equality on reviewer and state reaches reviewerAsksKey's leading
	// columns, and a batch driven from a list of reviewers would seek it on
	// the reviewer alone, reading every change's ask of each to find the one.
	reviewersKey = "sqlite_autoindex_code_reviewers_1"
	// waitsKey is code_mergeability_waits_idx, (waits_person, change_id)
	// WHERE waits_person = 1 (replicated 0106): the changes whose stored
	// readiness waits on a person, newest change first.
	waitsKey = "code_mergeability_waits_idx"
)

// The reads of a change's conversation, each registered in plan_test.go so
// the pinned driver's planner is held to the index each is written for and to
// reading in its order. The comments' >= beside the OR keeps the seek past
// the thread; a finding's review is joined by its key for when it was raised.
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
			position, updated_at
		FROM code_conversations WHERE change_id = ?`
	reviewersOfSQL = `SELECT reviewer, reviewer_kind, reason, state, requested_by, requested_by_kind,
			note, position, owned_paths, owned_paths_omitted
		FROM code_reviewers INDEXED BY ` + reviewersKey + ` WHERE change_id = ? ORDER BY reviewer LIMIT ?`
	reviewersOfAfterSQL = `SELECT reviewer, reviewer_kind, reason, state, requested_by, requested_by_kind,
			note, position, owned_paths, owned_paths_omitted
		FROM code_reviewers INDEXED BY ` + reviewersKey + ` WHERE change_id = ? AND reviewer > ?
		ORDER BY reviewer LIMIT ?`
	commentsOfSQL = `SELECT id, thread_id, parent_id, author, author_kind, author_operator_id, anchor, resolved,
			resolved_by, resolved_by_kind, turn_id, position, at, body, finding_id, edited_at, deleted_at,
			deleted_by, deleted_by_kind
		FROM code_comments WHERE change_id = ? ORDER BY thread_id, position LIMIT ?`
	commentsOfAfterSQL = `SELECT id, thread_id, parent_id, author, author_kind, author_operator_id, anchor,
			resolved, resolved_by, resolved_by_kind, turn_id, position, at, body, finding_id, edited_at, deleted_at,
			deleted_by, deleted_by_kind
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
			x.body, x.suggestion, v.at
		FROM code_finding_texts AS x CROSS JOIN code_findings AS f CROSS JOIN code_reviews AS v
		WHERE x.change_id = ? AND f.id = x.finding_id AND v.id = f.review_id ORDER BY x.finding_id LIMIT ?`
	findingsOfAfterSQL = `SELECT f.id, f.review_id, f.raiser, f.raiser_kind, f.revision, f.commit_oid, f.path,
			f.blob_oid, f.side, f.line_start, f.line_end, f.severity, f.category, f.state, f.resolution_note,
			f.resolved_by, f.resolved_by_kind, f.resolved_commit, f.resolved_position, f.position, x.title,
			x.body, x.suggestion, v.at
		FROM code_finding_texts AS x CROSS JOIN code_findings AS f CROSS JOIN code_reviews AS v
		WHERE x.change_id = ? AND x.finding_id > ? AND f.id = x.finding_id AND v.id = f.review_id
		ORDER BY x.finding_id LIMIT ?`
	// findingsNamedSQL is the findings a read names ([ReadQuery.FindingIDs]),
	// a JSON array of their ids: each one seek of the texts' key (change and
	// id) and one of the narrow row's, no walk of the change's findings.
	// Unordered: at most MaxFindingsPerReview rows, which the reader sorts.
	findingsNamedSQL = `SELECT f.id, f.review_id, f.raiser, f.raiser_kind, f.revision, f.commit_oid, f.path,
			f.blob_oid, f.side, f.line_start, f.line_end, f.severity, f.category, f.state, f.resolution_note,
			f.resolved_by, f.resolved_by_kind, f.resolved_commit, f.resolved_position, f.position, x.title,
			x.body, x.suggestion, v.at
		FROM json_each(?) AS j CROSS JOIN code_finding_texts AS x CROSS JOIN code_findings AS f
			CROSS JOIN code_reviews AS v
		WHERE x.change_id = ? AND x.finding_id = CAST(j.value AS TEXT) AND f.id = x.finding_id
			AND v.id = f.review_id`
)

// reviewsGiven serves a page of ONE REVIEWER'S REVIEWS — the `reviews`
// listing, a seat's or a person's "reviews given" — newest first, after a
// cursor, across every repository the asker may read: one range of
// code_reviews_reviewer_idx (replicated 0118), the page's changes read by
// their keys in one statement, and their repositories judged once a page
// against the grant ([listedReposAmong]), leaving out a review the asker may
// not read. A page READS its limit of reviews, however few the grant keeps,
// and resumes after the last it read.
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
		return fmt.Errorf("gitlet: read the reviews %s gave: %w", q.Reviewer, err)
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
		return refusedf("%w: reviewer: a listing of reviews lists one reviewer's, by the seat's handle or "+
			"the human seat a person holds", ErrInvalid)
	case !org.ValidHandle(q.Reviewer):
		return refusedf("%w: reviewer: %q is not a seat's handle: a reviewer is a seat, or the human seat a "+
			"person holds", ErrInvalid, q.Reviewer)
	}
	return nil
}

// reviewsGivenKey is code_reviews_reviewer_idx, (reviewer, position)
// (replicated 0118): one reviewer's reviews in the order their records landed
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
