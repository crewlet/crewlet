package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// THE CONVERSATION'S APPLIES: every record on a change's own subject
// ([KindChange]), pure in the record, its position and its instant (a
// reviewer's kind rides the record; no chart is read). A payload naming another
// change, or a repository its scope does not name, is an error, not a skip: it
// would escape that repository's deletion gate, which [Applier.Gated] reads off
// the scope. Every other guard is a SKIP, never a refusal that would stop the
// log on every node: a change without a row (purged) applies nothing, logged
// code_conversation_not_applied; a record at or below the conversation's
// position is a redelivery. Every record counts one version of the conversation
// and moves its position (code_conversations), the guard a redelivery meets and
// what [ChangeWriter.applied] reads; one that [movesReadiness] also counts a
// readiness version, what the home's due listing compares (changesdue.go).

// applyChange is every record on a change's own subject.
func (a *Applier) applyChange(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	var repoID, changeID string
	switch p := payload.(type) {
	case ReviewerRequest:
		repoID, changeID = p.RepoID, p.ChangeID
	case Comment:
		repoID, changeID = p.RepoID, p.ChangeID
	case ThreadResolve:
		repoID, changeID = p.RepoID, p.ChangeID
	case Review:
		repoID, changeID = p.RepoID, p.ChangeID
	case FindingResolve:
		repoID, changeID = p.RepoID, p.ChangeID
	case Revise:
		repoID, changeID = p.RepoID, p.ChangeID
	case Mergeability:
		repoID, changeID = p.RepoID, p.ChangeID
	case MergeabilityKept:
		repoID, changeID = p.RepoID, p.ChangeID
	case AskSettle:
		repoID, changeID = p.RepoID, p.ChangeID
	default:
		return 0, fmt.Errorf("gitlet: %s is not an op this build applies to a change (the record at %s)",
			at.record.Op, at.position)
	}
	if changeID != at.record.Subject.ID {
		return 0, fmt.Errorf("gitlet: the record at %s arbitrated change.%s and its payload names change %s — a "+
			"record on one change's subject may write only that change's conversation", at.position,
			at.record.Subject.ID, changeID)
	}
	if scoped, _ := scopedRepo(at.record.Scope.Set()); scoped != repoID {
		return 0, fmt.Errorf("gitlet: the record at %s names repository %s in its payload and %q in its scope — "+
			"a record no writer forms, which the deletion gate judges by the repository its scope names",
			at.position, repoID, scoped)
	}
	var held string
	err = tx.QueryRowContext(ctx, changeRepoSQL, changeID).Scan(&held)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return conversationNotApplied(ctx, at, repoID, changeID, "the change has no row")
	case err != nil:
		return 0, fmt.Errorf("gitlet: read change %s at %s: %w", changeID, at.position, err)
	case held != repoID:
		return 0, fmt.Errorf("gitlet: the record at %s names change %s of repository %s, and the change is "+
			"repository %s's — a record no writer forms, which would escape that repository's deletion gate",
			at.position, changeID, repoID, held)
	}
	_, position, _, err := conversationIn(ctx, tx, changeID)
	if err != nil {
		return 0, err
	}
	if position >= at.packed {
		return 0, nil
	}
	var rows int
	var counts conversationCounts
	switch p := payload.(type) {
	case ReviewerRequest:
		rows, err = applyReviewers(ctx, tx, at, p)
	case Comment:
		rows, counts, err = applyComment(ctx, tx, at, p)
	case ThreadResolve:
		rows, err = applyThreadResolve(ctx, tx, at, p)
	case Review:
		rows, counts, err = applyReview(ctx, tx, at, p)
	case FindingResolve:
		rows, counts, err = applyFindingResolve(ctx, tx, at, p)
	case Revise:
		rows, counts, err = applyRevise(ctx, tx, at, p)
	case Mergeability:
		rows, counts, err = applyMergeability(ctx, tx, at, p)
	case MergeabilityKept:
		rows, err = applyMergeabilityKept(ctx, tx, at, p)
	case AskSettle:
		rows, err = applyAsks(ctx, tx, at, p)
	}
	if err != nil || rows == 0 {
		return 0, err
	}
	if movesReadiness(payload) {
		counts.readiness = 1
	}
	if spoken(payload) {
		counts.spokenAt = at.at()
	}
	if err := touchConversation(ctx, tx, at, repoID, changeID, counts); err != nil {
		return 0, err
	}
	if movesAsks(payload) {
		// Recounted from the rows, never moved by a delta: a count read off the
		// reviewers' key stays the rows' whatever this record's ups and downs.
		if _, err := tx.ExecContext(ctx, countAsksSQL, changeID); err != nil {
			return 0, fmt.Errorf("gitlet: count change %s's asks at %s: %w", changeID, at.position, err)
		}
	}
	return a.noticed(ctx, tx, at, repoID, payload, rows+1, nil)
}

// movesReadiness reports a record whose apply moves what the change's
// readiness reads ([ReadinessOf]): an ask, a review, an answer to findings, a
// revision's facts and a settling of the asks. A comment, a thread's resolution
// and a mergeability move none of it; a mergeability that moved
// readiness_version would make its change due a refresh again at every refresh.
func movesReadiness(payload any) bool {
	switch payload.(type) {
	case ReviewerRequest, Review, FindingResolve, Revise, AskSettle:
		return true
	}
	return false
}

// spoken reports a record SOMEBODY WROTE (an ask, a comment, a thread
// resolved, a review, an answer to a finding), which dates the conversation's
// last move (code_conversations.updated_at) that a listing narrowed to a window
// walks. The home's facts and the asks' settling date nothing: a moving target
// re-checks every live change into it, which would put every change of a busy
// branch in every day's window.
func spoken(payload any) bool {
	switch payload.(type) {
	case ReviewerRequest, Comment, ThreadResolve, Review, FindingResolve:
		return true
	}
	return false
}

// movesAsks reports a record whose apply moves an ask's state
// (code_reviewers): an ask, a review (which answers its reviewer's own), a
// revision's facts (asking its path owners, and again its reviewers on
// request_changes) and a settling of the asks.
func movesAsks(payload any) bool {
	switch payload.(type) {
	case ReviewerRequest, Review, Revise, AskSettle:
		return true
	}
	return false
}

// conversationCounts is what one record moves its conversation's counters by.
// openFindings are those no author has answered; openBlockers the blockers
// holding the change back whatever their authors answered (S40);
// changesRequested the reviewers whose standing verdict asks for changes;
// readiness 1 for a record that [movesReadiness]; spokenAt the broker instant
// (store.EncodeTime) of a record somebody wrote ([spoken]), else 0.
type conversationCounts struct {
	comments, reviews, openFindings, openBlockers, changesRequested, textBytes, readiness, spokenAt int64
}

// touchConversation moves changeID's conversation by one record: counters by
// counts, version by one, position to the record's (the first record makes the
// row) and, for a record [spoken], its instant. One nobody wrote names no
// instant, since the driver rewrites an index entry for every column a write
// names, whatever the value (code_conversations_updated_idx,
// code_conversations_repo_updated_idx).
func touchConversation(ctx context.Context, tx *sql.Tx, at applyContext, repoID, changeID string,
	counts conversationCounts) error {

	stmt := touchConversationSQL
	if counts.spokenAt > 0 {
		stmt = touchSpokenConversationSQL
	}
	if _, err := tx.ExecContext(ctx, stmt, changeID, repoID, counts.comments, counts.reviews,
		counts.openFindings, counts.openBlockers, counts.changesRequested, counts.textBytes, counts.readiness,
		at.packed, counts.spokenAt); err != nil {
		return fmt.Errorf("gitlet: move change %s's conversation at %s: %w", changeID, at.position, err)
	}
	return nil
}

// applyReview writes a review, its findings and their texts, its reviewer as a
// participant whose STANDING VERDICT moves to this one's where it judges
// (approve, request_changes; a comment judges nothing), and supersedes its
// reviewer's standing findings from earlier revisions it does not carry
// ([supersededBy], the decide's own rule). changes_requested moves by the
// reviewer's standing verdict before and after.
func applyReview(ctx context.Context, tx queryer, at applyContext, r Review) (int, conversationCounts, error) {
	by, kind := r.By.party()
	if by == "" {
		n, err := conversationNotApplied(ctx, at, r.RepoID, r.ChangeID, "its reviewer names no seat")
		return n, conversationCounts{}, err
	}
	var before string
	switch err := tx.QueryRowContext(ctx, standingVerdictSQL, r.ChangeID, by).Scan(&before); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, conversationCounts{}, fmt.Errorf("gitlet: read %s's standing verdict on change %s at %s: %w", by,
			r.ChangeID, at.position, err)
	}
	// The credential a person reviewed with, as a comment keeps its own.
	var operator string
	if kind == PartyPerson {
		operator = r.By.OperatorID
	}
	res, err := tx.ExecContext(ctx, addReviewSQL, r.ReviewID, r.ChangeID, r.Revision, by, string(kind), operator,
		string(r.Verdict), len(r.Findings), r.TurnID, r.Model, len(r.Summary), at.packed, at.at(), r.Summary)
	if err != nil {
		return 0, conversationCounts{}, fmt.Errorf("gitlet: write review %s at %s: %w", r.ReviewID, at.position, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		n, err := conversationNotApplied(ctx, at, r.RepoID, r.ChangeID, "a review already holds its id")
		return n, conversationCounts{}, err
	}
	counts := conversationCounts{reviews: 1, textBytes: int64(len(r.Summary))}
	judges := r.Verdict != VerdictComment
	switch was, is := Verdict(before) == VerdictRequestChanges, r.Verdict == VerdictRequestChanges; {
	case !judges:
	case is && !was:
		counts.changesRequested = 1
	case was && !is:
		counts.changesRequested = -1
	}
	rows := 1
	_, superseded, err := supersededBy(ctx, tx, r.ChangeID, by, kind, r)
	if err != nil {
		return 0, conversationCounts{}, err
	}
	if len(superseded) > 0 {
		ids := make([]string, len(superseded))
		for i, f := range superseded {
			ids[i] = f.id
			if f.state == FindingOpen {
				counts.openFindings--
			}
			if f.severity == SeverityBlocker {
				counts.openBlockers--
			}
		}
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		list, err := jsonList(ids)
		if err != nil {
			return 0, conversationCounts{}, err
		}
		if _, err := tx.ExecContext(ctx, supersedeFindingsSQL, string(FindingSuperseded), by, string(kind),
			at.packed, list); err != nil {
			return 0, conversationCounts{}, fmt.Errorf("gitlet: supersede %s's findings on change %s at %s: %w", by,
				r.ChangeID, at.position, err)
		}
		rows += len(ids)
	}
	// As few multi-row statements as the bind limit allows, never one a row:
	// this runs in the code domain's one applier on every data node, which the
	// pushes queue behind.
	if _, err = store.InsertRows(ctx, tx, at.maxVariables, addFindingsSQL, findingValuesSQL,
		`ON CONFLICT (id) DO NOTHING`, len(r.Findings), func(i int) []any {
			f := r.Findings[i]
			return []any{f.ID, r.ChangeID, r.ReviewID, by, string(kind), r.Revision, f.CommitOID, f.Path,
				f.BlobOID, string(f.Side), f.LineStart, f.LineEnd, string(f.Severity), string(f.Category),
				string(FindingOpen), at.packed}
		}); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("gitlet: write review %s's findings at %s: %w", r.ReviewID,
			at.position, err)
	}
	if _, err = store.InsertRows(ctx, tx, at.maxVariables, addFindingTextsSQL, `(?, ?, ?, ?, ?, ?)`,
		`ON CONFLICT (change_id, finding_id) DO NOTHING`, len(r.Findings), func(i int) []any {
			f := r.Findings[i]
			return []any{r.ChangeID, f.ID, findingTextBytes(f), f.Title, f.Body, f.Suggestion}
		}); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("gitlet: write review %s's findings' texts at %s: %w",
			r.ReviewID, at.position, err)
	}
	for _, f := range r.Findings {
		counts.openFindings++
		if f.Severity == SeverityBlocker {
			counts.openBlockers++
		}
		counts.textBytes += findingTextBytes(f)
		rows += 2
	}
	participant, args := addParticipantSQL, []any{r.ChangeID, by, string(kind), at.packed}
	if judges {
		participant, args = judgeParticipantSQL, append(args, string(r.Verdict), r.Revision, r.ReviewID)
	}
	if _, err = tx.ExecContext(ctx, participant, args...); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("gitlet: add %s to change %s at %s: %w", by, r.ChangeID,
			at.position, err)
	}
	mentioned, err := addMentioned(ctx, tx, at, r.ChangeID, r.Routing)
	if err != nil {
		return 0, conversationCounts{}, err
	}
	// The review answers its reviewer's own ask whatever its verdict, until
	// somebody asks again or a later revision asks again on request_changes
	// (applyRevise).
	answered, err := moveAsks(ctx, tx, at, r.ChangeID, ReviewerRequested, ReviewerReviewed, []string{by})
	if err != nil {
		return 0, conversationCounts{}, err
	}
	return rows + 1 + mentioned + answered, counts, nil
}

// addMentioned makes everybody r mentions a participant of the change (a
// watcher, concerns.go), a person or a seat as r says, and answers the rows it
// wrote. A participant already there stays: a mention moves no standing verdict.
func addMentioned(ctx context.Context, tx store.Execer, at applyContext, changeID string, r *ChangeRouting) (int,
	error) {

	if r == nil {
		return 0, nil
	}
	rows := 0
	for _, h := range r.Mentions {
		kind := PartyAgent
		if r.Person(h) {
			kind = PartyPerson
		}
		res, err := tx.ExecContext(ctx, addParticipantSQL, changeID, h, string(kind), at.packed)
		if err != nil {
			return 0, fmt.Errorf("gitlet: add %s, mentioned, to change %s at %s: %w", h, changeID, at.position, err)
		}
		n, _ := res.RowsAffected()
		rows += int(n)
	}
	return rows, nil
}

// applyFindingResolve answers findings: each that still stands moves to the
// record's state with its note, answerer, commit and position (one no longer
// standing, or already in that state, applies nothing). An open finding
// answered leaves the open count; only a dismissed blocker leaves the standing
// blockers (S40).
func applyFindingResolve(ctx context.Context, tx *sql.Tx, at applyContext, fr FindingResolve) (int,
	conversationCounts, error) {

	by, kind := fr.By.party()
	if by == "" {
		n, err := conversationNotApplied(ctx, at, fr.RepoID, fr.ChangeID, "who answered names no seat")
		return n, conversationCounts{}, err
	}
	// Read in one statement driven from their list; each answer written by its key.
	ids := make([]string, len(fr.Findings))
	for i, a := range fr.Findings {
		ids[i] = a.FindingID
	}
	held, err := findingsAmong(ctx, tx, ids)
	if err != nil {
		return 0, conversationCounts{}, err
	}
	var counts conversationCounts
	rows := 0
	for _, a := range fr.Findings {
		f, found := held[a.FindingID]
		if !found || f.changeID != fr.ChangeID || !f.state.Stands() || f.state == a.Resolution {
			continue
		}
		if _, err := tx.ExecContext(ctx, answerFindingSQL, string(a.Resolution), a.Note, by, string(kind), a.Commit,
			at.packed, a.FindingID); err != nil {
			return 0, conversationCounts{}, fmt.Errorf("gitlet: answer finding %s at %s: %w", a.FindingID,
				at.position, err)
		}
		if f.state == FindingOpen {
			counts.openFindings--
		}
		if a.Resolution == FindingDismissed && f.severity == SeverityBlocker {
			counts.openBlockers--
		}
		rows++
	}
	if rows == 0 {
		n, err := conversationNotApplied(ctx, at, fr.RepoID, fr.ChangeID, "no finding it answers still stands")
		return n, conversationCounts{}, err
	}
	return rows, counts, nil
}

// applyReviewers writes an ask for review: each reviewer asked (again, where
// withdrawn) and made a participant, and each withdrawn ask still standing
// marked so.
func applyReviewers(ctx context.Context, tx *sql.Tx, at applyContext, p ReviewerRequest) (int, error) {
	by, byKind := p.By.party()
	if by == "" {
		return conversationNotApplied(ctx, at, p.RepoID, p.ChangeID, "who asked names no seat")
	}
	for _, r := range p.Request {
		if !r.Kind.Valid() {
			return 0, fmt.Errorf("gitlet: the ask for review at %s names reviewer %q of kind %q, which no writer "+
				"stamps", at.position, r.Handle, r.Kind)
		}
	}
	// As few statements as the bind limit allows, never one a reviewer: the asks
	// and participants a collection each, the withdrawals one statement.
	if _, err := store.InsertRows(ctx, tx, at.maxVariables, askReviewersSQL, `(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		askReviewersConflictSQL, len(p.Request), func(i int) []any {
			r := p.Request[i]
			return []any{p.ChangeID, r.Handle, string(r.Kind), string(ReviewerAsked), string(ReviewerRequested), by,
				string(byKind), p.Note, at.packed}
		}); err != nil {
		return 0, fmt.Errorf("gitlet: ask change %s's reviewers at %s: %w", p.ChangeID, at.position, err)
	}
	if err := addParticipants(ctx, tx, at, p.ChangeID, p.Request); err != nil {
		return 0, err
	}
	rows := 2 * len(p.Request)
	withdrawn, err := moveAsks(ctx, tx, at, p.ChangeID, ReviewerRequested, ReviewerWithdrawn, p.Withdraw)
	if err != nil {
		return 0, err
	}
	rows += withdrawn
	// An ask that moved no row is still one version of the conversation: its
	// decide found something to move.
	return max(rows, 1), nil
}

// applyComment writes a comment, its author as a participant and everybody it
// mentions as one too, counting it and what its body weighs on its
// conversation.
func applyComment(ctx context.Context, tx *sql.Tx, at applyContext, c Comment) (int, conversationCounts, error) {
	by, kind := c.By.party()
	if by == "" {
		n, err := conversationNotApplied(ctx, at, c.RepoID, c.ChangeID, "who said it names no seat")
		return n, conversationCounts{}, err
	}
	var anchor string
	if c.Anchor != nil {
		encoded, err := json.Marshal(c.Anchor)
		if err != nil {
			return 0, conversationCounts{}, fmt.Errorf("gitlet: encode comment %s's anchor: %w", c.CommentID, err)
		}
		anchor = string(encoded)
	}
	// THE CREDENTIAL A PERSON SAID IT WITH, as every record a write leaves
	// names it (iam.ActorFor); a seat acts as itself with none.
	var operator string
	if kind == PartyPerson {
		operator = c.By.OperatorID
	}
	res, err := tx.ExecContext(ctx, addCommentSQL, c.CommentID, c.ChangeID, c.ThreadID, c.ParentID, c.FindingID,
		by, string(kind), operator, anchor, c.TurnID, len(c.Body), at.packed, at.at(), c.Body)
	if err != nil {
		return 0, conversationCounts{}, fmt.Errorf("gitlet: write comment %s at %s: %w", c.CommentID, at.position,
			err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		n, err := conversationNotApplied(ctx, at, c.RepoID, c.ChangeID, "a comment already holds its id")
		return n, conversationCounts{}, err
	}
	if _, err = tx.ExecContext(ctx, addParticipantSQL, c.ChangeID, by, string(kind), at.packed); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("gitlet: add %s to change %s at %s: %w", by, c.ChangeID,
			at.position, err)
	}
	mentioned, err := addMentioned(ctx, tx, at, c.ChangeID, c.Routing)
	if err != nil {
		return 0, conversationCounts{}, err
	}
	return 2 + mentioned, conversationCounts{comments: 1, textBytes: int64(len(c.Body))}, nil
}

// applyThreadResolve marks a thread's root resolved or open again, naming who.
func applyThreadResolve(ctx context.Context, tx *sql.Tx, at applyContext, t ThreadResolve) (int, error) {
	by, kind := t.By.party()
	if by == "" {
		return conversationNotApplied(ctx, at, t.RepoID, t.ChangeID, "who resolved it names no seat")
	}
	resolvedBy, resolvedKind := by, string(kind)
	if !t.Resolved {
		resolvedBy, resolvedKind = "", ""
	}
	res, err := tx.ExecContext(ctx, resolveThreadSQL, t.Resolved, resolvedBy, resolvedKind, t.ThreadID,
		t.ChangeID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: resolve thread %s of change %s at %s: %w", t.ThreadID, t.ChangeID,
			at.position, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return conversationNotApplied(ctx, at, t.RepoID, t.ChangeID, "the change holds no thread "+t.ThreadID)
	}
	return 1, nil
}

// applyAsks settles a change's asks with its lifecycle ([AskSettle]): each
// named ask moves from the state the other direction leaves (waiting to ended,
// ended to requested) only while the change, as this transaction reads it,
// stands where the decide found it: closed or merged for an end, live for an
// ask again. A change moved since keeps its asks and the record is one version
// of the conversation alone; the home's next pass reads the rows again.
func applyAsks(ctx context.Context, tx *sql.Tx, at applyContext, a AskSettle) (int, error) {
	row, found, err := changeRowIn(ctx, tx, a.ChangeID)
	if err != nil || !found {
		return 0, err
	}
	from, ends := ReviewerEnded, a.State == ReviewerEnded
	if ends {
		from = ReviewerRequested
	}
	if ends == row.state.Live() {
		return 1, nil
	}
	moved, err := moveAsks(ctx, tx, at, a.ChangeID, from, a.State, a.Reviewers)
	if err != nil {
		return 0, err
	}
	return max(moved, 1), nil
}

// queryer is what an apply of a change's conversation issues its statements
// through: a *sql.Tx, and in a test one that counts them
// (TestTheWidestReviewAppliesInAFixedNumberOfStatements).
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// addParticipants writes each of reviewers a participant of changeID where the
// change holds none of them, at the record's position, as one collection.
func addParticipants(ctx context.Context, tx store.Execer, at applyContext, changeID string,
	reviewers []Reviewer) error {

	if _, err := store.InsertRows(ctx, tx, at.maxVariables, addParticipantsSQL, `(?, ?, ?, ?)`,
		`ON CONFLICT (change_id, handle) DO NOTHING`, len(reviewers), func(i int) []any {
			return []any{changeID, reviewers[i].Handle, string(reviewers[i].Kind), at.packed}
		}); err != nil {
		return fmt.Errorf("gitlet: add change %s's reviewers as its participants at %s: %w", changeID, at.position,
			err)
	}
	return nil
}

// moveAsks moves each ask of changeID handles names that is in state from to
// state to, at the record's position, in one statement driven from the list of
// the handles, and answers how many moved.
func moveAsks(ctx context.Context, tx store.Execer, at applyContext, changeID string, from, to ReviewerState,
	handles []string) (int, error) {

	if len(handles) == 0 {
		return 0, nil
	}
	list, err := jsonList(handles)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, moveAsksSQL, string(to), at.packed, changeID, string(from), list)
	if err != nil {
		return 0, fmt.Errorf("gitlet: move change %s's asks %s to %s at %s: %w", changeID, from, to, at.position,
			err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// conversationNotApplied logs a record on a change's subject that applies as
// nothing, and answers that it wrote no row.
func conversationNotApplied(ctx context.Context, at applyContext, repoID, changeID, why string) (int, error) {
	applyLog.WarnContext(ctx, "code_conversation_not_applied", "repo", repoID, "change", changeID,
		"op", string(at.record.Op), "op_id", at.record.OpID, "position", at.position.String(), "why", why)
	return 0, nil
}

// The conversation's writes and the reads that find their rows; every one with
// a WHERE is registered in plan_test.go. A conflict on a key is a skip or a
// merge, never an abort, which would stall the log on every node.
const (
	changeRepoSQL = `SELECT repo_id FROM code_changes WHERE id = ?`
	// touchConversationSQL is a record's move of its conversation, and
	// touchSpokenConversationSQL a move by one somebody wrote, which dates it.
	touchConversationSQL = `INSERT INTO code_conversations (change_id, repo_id, comments, reviews, open_findings,
			open_blockers, changes_requested, text_bytes, readiness_version, version, position, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (change_id) DO UPDATE SET comments = code_conversations.comments + excluded.comments,
			reviews = code_conversations.reviews + excluded.reviews,
			open_findings = code_conversations.open_findings + excluded.open_findings,
			open_blockers = code_conversations.open_blockers + excluded.open_blockers,
			changes_requested = code_conversations.changes_requested + excluded.changes_requested,
			text_bytes = code_conversations.text_bytes + excluded.text_bytes,
			readiness_version = code_conversations.readiness_version + excluded.readiness_version,
			version = code_conversations.version + 1, position = excluded.position`
	touchSpokenConversationSQL = touchConversationSQL + `, updated_at = excluded.updated_at`
	// A review's statements: its reviewer's standing verdict before it; the
	// supersession driven from the list of the ids the decide's rule names.
	standingVerdictSQL  = `SELECT verdict FROM code_participants WHERE change_id = ? AND handle = ?`
	judgeParticipantSQL = `INSERT INTO code_participants (change_id, handle, kind, position, verdict,
			verdict_revision, verdict_review)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (change_id, handle) DO UPDATE SET verdict = excluded.verdict,
			verdict_revision = excluded.verdict_revision, verdict_review = excluded.verdict_review`
	addReviewSQL = `INSERT INTO code_reviews (id, change_id, revision, reviewer, reviewer_kind, operator_id,
			verdict, findings, turn_id, model, summary_bytes, position, at, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`
	// addFindingsSQL and addFindingTextsSQL are a review's findings and their
	// texts, each a collection store.InsertRows writes, findingValuesSQL a
	// finding's row.
	addFindingsSQL = `INSERT INTO code_findings (id, change_id, review_id, raiser, raiser_kind, revision,
			commit_oid, path, blob_oid, side, line_start, line_end, severity, category, state, position) VALUES `
	findingValuesSQL   = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	addFindingTextsSQL = `INSERT INTO code_finding_texts (change_id, finding_id, bytes, title, body, suggestion)
		VALUES `
	supersedeFindingsSQL = `UPDATE code_findings SET state = ?, resolution_note = '', resolved_by = ?,
			resolved_by_kind = ?, resolved_commit = '', resolved_position = ?
		WHERE rowid IN (SELECT f.rowid FROM json_each(?) AS j CROSS JOIN code_findings AS f
			WHERE f.id = CAST(j.value AS TEXT))`
	answerFindingSQL = `UPDATE code_findings SET state = ?, resolution_note = ?, resolved_by = ?,
			resolved_by_kind = ?, resolved_commit = ?, resolved_position = ?
		WHERE id = ?`
	// askReviewersSQL and askReviewersConflictSQL are an ask's reviewers, a
	// collection store.InsertRows writes, each asked again where the change
	// held an ask of them in any state, and a path owner's globs cleared.
	askReviewersSQL = `INSERT INTO code_reviewers (change_id, reviewer, reviewer_kind, reason, state, requested_by,
			requested_by_kind, note, position) VALUES `
	askReviewersConflictSQL = `ON CONFLICT (change_id, reviewer) DO UPDATE SET
			reviewer_kind = excluded.reviewer_kind, reason = excluded.reason, state = excluded.state,
			requested_by = excluded.requested_by, requested_by_kind = excluded.requested_by_kind,
			note = excluded.note, position = excluded.position, owned_paths = '', owned_paths_omitted = 0`
	// moveAsksSQL moves a change's named asks from one state to another,
	// driven from the list, one seek of the reviewers' key a handle (NAMED:
	// [reviewersKey] says why).
	moveAsksSQL = `UPDATE code_reviewers SET state = ?1, position = ?2
		WHERE rowid IN (SELECT r.rowid FROM json_each(?5) AS j
			CROSS JOIN code_reviewers AS r INDEXED BY ` + reviewersKey + `
			WHERE r.change_id = ?3 AND r.reviewer = CAST(j.value AS TEXT) AND r.state = ?4)`
	// countAsksSQL counts a change's asks that wait and those its end left
	// onto its conversation's row, each a range of the reviewers' key.
	countAsksSQL = `UPDATE code_conversations SET
			asked = (SELECT COUNT(*) FROM code_reviewers INDEXED BY ` + reviewersKey + `
				WHERE change_id = ?1 AND state = 'requested'),
			ended = (SELECT COUNT(*) FROM code_reviewers INDEXED BY ` + reviewersKey + `
				WHERE change_id = ?1 AND state = 'ended')
		WHERE change_id = ?1`
	addParticipantSQL = `INSERT INTO code_participants (change_id, handle, kind, position) VALUES (?, ?, ?, ?)
		ON CONFLICT (change_id, handle) DO NOTHING`
	// addParticipantsSQL is reviewers as a change's participants, a collection
	// store.InsertRows writes, each where the change holds none of them.
	addParticipantsSQL = `INSERT INTO code_participants (change_id, handle, kind, position) VALUES `
	addCommentSQL      = `INSERT INTO code_comments (id, change_id, thread_id, parent_id, finding_id, author,
			author_kind, author_operator_id, anchor, turn_id, body_bytes, position, at, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`
	resolveThreadSQL = `UPDATE code_comments SET resolved = ?, resolved_by = ?, resolved_by_kind = ?
		WHERE id = ? AND change_id = ? AND thread_id = id`
)
