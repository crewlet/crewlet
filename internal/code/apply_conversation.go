package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// THE CONVERSATION'S APPLIES: every record on a change's own subject
// ([KindChange]), each in the one transaction the framework holds a record's
// apply to, and each PURE IN THE RECORD, ITS POSITION AND ITS INSTANT, like every
// apply here: it reads no clock, no configuration and no chart — a reviewer's
// kind rides the record, stamped by the writer — and decides nothing again.
//
// THE SUBJECT GUARD: a record on one change's subject may write only that
// change's conversation — its payload names the subject's change — and the
// change is the repository's its payload names, or the record is one no
// correct writer forms, an error rather than a skip: a record about one
// repository's change that named another would escape that repository's
// deletion gate, which [Applier.Gated] reads off the payload.
//
// EVERY OTHER GUARD IS A SKIP, never a refusal that would stop the log on every
// node: a record whose change has no row — a repository's purge removed it, and
// only a record the ledger would have collapsed reaches a change that gone —
// applies as nothing, whole, logged (code_conversation_not_applied); one at or
// below its conversation's position is a redelivery; a comment or a review
// whose id is held already, a thread whose root is gone, and an answer whose
// every finding no longer stands apply as nothing too.
//
// EVERY RECORD COUNTS ONE VERSION of its change's conversation and moves its
// position to its own (code_conversations): the version guard a redelivery
// meets, and what an answer is read from ([ChangeWriter.applied]).

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
	default:
		return 0, fmt.Errorf("code: %s is not an op this build applies to a change (the record at %s)",
			at.record.Op, at.position)
	}
	if changeID != at.record.Subject.ID {
		return 0, fmt.Errorf("code: the record at %s arbitrated change.%s and its payload names change %s — a "+
			"record on one change's subject may write only that change's conversation", at.position,
			at.record.Subject.ID, changeID)
	}
	var held string
	err = tx.QueryRowContext(ctx, changeRepoSQL, changeID).Scan(&held)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return conversationNotApplied(ctx, at, repoID, changeID, "the change has no row")
	case err != nil:
		return 0, fmt.Errorf("code: read change %s at %s: %w", changeID, at.position, err)
	case held != repoID:
		return 0, fmt.Errorf("code: the record at %s names change %s of repository %s, and the change is "+
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
	}
	if err != nil || rows == 0 {
		return 0, err
	}
	if err := touchConversation(ctx, tx, at, repoID, changeID, counts); err != nil {
		return 0, err
	}
	return rows + 1, nil
}

// conversationCounts is what one record moves its conversation's counters by:
// its comments and its reviews; its OPEN findings — none of whose authors has
// answered — and its STANDING BLOCKERS, the blockers that hold the change back
// whatever their authors answered (S40); how many reviewers' standing verdict
// asks for changes; and what its wide text weighs.
type conversationCounts struct {
	comments, reviews, openFindings, openBlockers, changesRequested, textBytes int64
}

// touchConversation moves changeID's conversation by one record: its counters
// by counts, its version by one and its position to the record's own — the
// row made by the change's first conversation record.
func touchConversation(ctx context.Context, tx *sql.Tx, at applyContext, repoID, changeID string,
	counts conversationCounts) error {

	if _, err := tx.ExecContext(ctx, touchConversationSQL, changeID, repoID, counts.comments, counts.reviews,
		counts.openFindings, counts.openBlockers, counts.changesRequested, counts.textBytes,
		at.packed); err != nil {
		return fmt.Errorf("code: move change %s's conversation at %s: %w", changeID, at.position, err)
	}
	return nil
}

// applyReview writes a review: its row, every finding it raises and its text,
// its reviewer as a participant — their STANDING VERDICT on the change moved
// to this review's where it judges (approve, request_changes), and left as it
// was where it comments, which judges nothing — and the supersession of every
// standing finding of its reviewer raised on an earlier revision that it does
// not carry ([supersededBy], the decide's own rule over the same rows):
// moving the conversation's counters by all of it, its changes_requested by
// the reviewer's standing verdict before this one and after.
func applyReview(ctx context.Context, tx *sql.Tx, at applyContext, r Review) (int, conversationCounts, error) {
	by, kind := r.By.party()
	if by == "" {
		n, err := conversationNotApplied(ctx, at, r.RepoID, r.ChangeID, "its reviewer names no seat")
		return n, conversationCounts{}, err
	}
	var before string
	switch err := tx.QueryRowContext(ctx, standingVerdictSQL, r.ChangeID, by).Scan(&before); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, conversationCounts{}, fmt.Errorf("code: read %s's standing verdict on change %s at %s: %w", by,
			r.ChangeID, at.position, err)
	}
	// THE CREDENTIAL A PERSON REVIEWED WITH, as a comment keeps its own.
	var operator string
	if kind == PartyPerson {
		operator = r.By.OperatorID
	}
	res, err := tx.ExecContext(ctx, addReviewSQL, r.ReviewID, r.ChangeID, r.Revision, by, string(kind), operator,
		string(r.Verdict), len(r.Findings), r.TurnID, r.Model, len(r.Summary), at.packed, at.at(), r.Summary)
	if err != nil {
		return 0, conversationCounts{}, fmt.Errorf("code: write review %s at %s: %w", r.ReviewID, at.position, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
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
		list, err := jsonList(ids)
		if err != nil {
			return 0, conversationCounts{}, err
		}
		if _, err := tx.ExecContext(ctx, supersedeFindingsSQL, string(FindingSuperseded), by, string(kind),
			at.packed, list); err != nil {
			return 0, conversationCounts{}, fmt.Errorf("code: supersede %s's findings on change %s at %s: %w", by,
				r.ChangeID, at.position, err)
		}
		rows += len(ids)
	}
	for _, f := range r.Findings {
		if _, err := tx.ExecContext(ctx, addFindingSQL, f.ID, r.ChangeID, r.ReviewID, by, string(kind), r.Revision,
			f.CommitOID, f.Path, f.BlobOID, string(f.Side), f.LineStart, f.LineEnd, string(f.Severity),
			string(f.Category), string(FindingOpen), at.packed); err != nil {
			return 0, conversationCounts{}, fmt.Errorf("code: write finding %s at %s: %w", f.ID, at.position, err)
		}
		if _, err := tx.ExecContext(ctx, addFindingTextSQL, r.ChangeID, f.ID, findingTextBytes(f), f.Title, f.Body,
			f.Suggestion); err != nil {
			return 0, conversationCounts{}, fmt.Errorf("code: write finding %s's text at %s: %w", f.ID,
				at.position, err)
		}
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
	if _, err := tx.ExecContext(ctx, participant, args...); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("code: add %s to change %s at %s: %w", by, r.ChangeID,
			at.position, err)
	}
	return rows + 1, counts, nil
}

// applyFindingResolve answers findings: each that still stands moves to the
// state the record gives it, with its note, who answered, the commit named and
// the record's position — a finding no longer standing, or in that state
// already, applies as nothing — and the conversation's counters move with
// them: an open finding answered leaves the open ones, and a blocker dismissed
// the standing blockers (S40: resolved, won't fix and disputed keep it).
func applyFindingResolve(ctx context.Context, tx *sql.Tx, at applyContext, fr FindingResolve) (int,
	conversationCounts, error) {

	by, kind := fr.By.party()
	if by == "" {
		n, err := conversationNotApplied(ctx, at, fr.RepoID, fr.ChangeID, "who answered names no seat")
		return n, conversationCounts{}, err
	}
	var counts conversationCounts
	rows := 0
	for _, a := range fr.Findings {
		f, found, err := findingIn(ctx, tx, a.FindingID)
		if err != nil {
			return 0, conversationCounts{}, err
		}
		if !found || f.changeID != fr.ChangeID || !f.state.Stands() || f.state == a.Resolution {
			continue
		}
		if _, err := tx.ExecContext(ctx, answerFindingSQL, string(a.Resolution), a.Note, by, string(kind), a.Commit,
			at.packed, a.FindingID); err != nil {
			return 0, conversationCounts{}, fmt.Errorf("code: answer finding %s at %s: %w", a.FindingID,
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

// applyReviewers writes an ask for review: each reviewer asked — named again
// where their ask was withdrawn — and a participant of the change, and each
// withdrawn ask still standing marked so.
func applyReviewers(ctx context.Context, tx *sql.Tx, at applyContext, p ReviewerRequest) (int, error) {
	by, byKind := p.By.party()
	if by == "" {
		return conversationNotApplied(ctx, at, p.RepoID, p.ChangeID, "who asked names no seat")
	}
	rows := 0
	for _, r := range p.Request {
		if !r.Kind.Valid() {
			return 0, fmt.Errorf("code: the ask for review at %s names reviewer %q of kind %q, which no writer "+
				"stamps", at.position, r.Handle, r.Kind)
		}
		if _, err := tx.ExecContext(ctx, askReviewerSQL, p.ChangeID, r.Handle, string(r.Kind),
			string(ReviewerAsked), string(ReviewerRequested), by, string(byKind), p.Note, at.packed); err != nil {
			return 0, fmt.Errorf("code: ask %s of change %s at %s: %w", r.Handle, p.ChangeID, at.position, err)
		}
		if _, err := tx.ExecContext(ctx, addParticipantSQL, p.ChangeID, r.Handle, string(r.Kind),
			at.packed); err != nil {
			return 0, fmt.Errorf("code: add %s to change %s at %s: %w", r.Handle, p.ChangeID, at.position, err)
		}
		rows += 2
	}
	for _, h := range p.Withdraw {
		res, err := tx.ExecContext(ctx, withdrawReviewerSQL, string(ReviewerWithdrawn), at.packed, p.ChangeID, h)
		if err != nil {
			return 0, fmt.Errorf("code: withdraw %s from change %s at %s: %w", h, p.ChangeID, at.position, err)
		}
		n, _ := res.RowsAffected()
		rows += int(n)
	}
	// AN ASK THAT MOVED NOTHING still moves the conversation: its decide
	// found something to move, and a record on the change's subject is one
	// version of it whatever its rows.
	return max(rows, 1), nil
}

// applyComment writes a comment and its author as a participant, counting it
// and what its body weighs on its conversation.
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
			return 0, conversationCounts{}, fmt.Errorf("code: encode comment %s's anchor: %w", c.CommentID, err)
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
		return 0, conversationCounts{}, fmt.Errorf("code: write comment %s at %s: %w", c.CommentID, at.position,
			err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		n, err := conversationNotApplied(ctx, at, c.RepoID, c.ChangeID, "a comment already holds its id")
		return n, conversationCounts{}, err
	}
	if _, err := tx.ExecContext(ctx, addParticipantSQL, c.ChangeID, by, string(kind), at.packed); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("code: add %s to change %s at %s: %w", by, c.ChangeID,
			at.position, err)
	}
	return 2, conversationCounts{comments: 1, textBytes: int64(len(c.Body))}, nil
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
		return 0, fmt.Errorf("code: resolve thread %s of change %s at %s: %w", t.ThreadID, t.ChangeID,
			at.position, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return conversationNotApplied(ctx, at, t.RepoID, t.ChangeID, "the change holds no thread "+t.ThreadID)
	}
	return 1, nil
}

// conversationNotApplied logs a record on a change's subject that applies as
// nothing, and answers that it wrote no row.
func conversationNotApplied(ctx context.Context, at applyContext, repoID, changeID, why string) (int, error) {
	applyLog.WarnContext(ctx, "code_conversation_not_applied", "repo", repoID, "change", changeID,
		"op", string(at.record.Op), "op_id", at.record.OpID, "position", at.position.String(), "why", why)
	return 0, nil
}

// changeRepoOf is the repository a change-subject record's payload names, read
// off the record's body — what the deletion gate drops it by ([gatedRepo]). A
// body this build cannot decode, or one naming no repository, answers false:
// such a record is DEFERRED rather than applied, and the deferral is
// reprocessed by a build that reads it, whose gate then runs properly.
func changeRepoOf(body []byte) (string, bool) {
	record, err := Decode(body)
	if err != nil || len(record.Mutation) == 0 {
		return "", false
	}
	// EVERY PAYLOAD ON A CHANGE'S SUBJECT NAMES ITS REPOSITORY under one key,
	// so the gate reads that key alone rather than knowing every op: an op a
	// later record adds is gated by its repository the day it lands.
	var named struct {
		RepoID string `json:"repo_id"`
	}
	if err := json.Unmarshal(record.Mutation, &named); err != nil || named.RepoID == "" {
		return "", false
	}
	return named.RepoID, true
}

// The conversation's writes, and the reads that find the rows they write: the
// change's repository and its conversation by their keys, a reviewer and a
// participant by theirs, a comment by its id and a thread's root by its id and
// change. Every one with a WHERE is registered in plan_test.go, and a conflict
// on a key is a skip or a merge, never an abort, which would stall the log on
// every node.
const (
	changeRepoSQL        = `SELECT repo_id FROM code_changes WHERE id = ?`
	touchConversationSQL = `INSERT INTO code_conversations (change_id, repo_id, comments, reviews, open_findings,
			open_blockers, changes_requested, text_bytes, version, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT (change_id) DO UPDATE SET comments = code_conversations.comments + excluded.comments,
			reviews = code_conversations.reviews + excluded.reviews,
			open_findings = code_conversations.open_findings + excluded.open_findings,
			open_blockers = code_conversations.open_blockers + excluded.open_blockers,
			changes_requested = code_conversations.changes_requested + excluded.changes_requested,
			text_bytes = code_conversations.text_bytes + excluded.text_bytes,
			version = code_conversations.version + 1, position = excluded.position`
	// A review's writes: the reviewer's standing verdict before this one, by
	// their participant row's key; the review, its findings and their texts,
	// each a conflict on its key a skip; the supersession of the findings the
	// decide's rule names, driven from the list of their ids; the reviewer as
	// a participant, their standing verdict moved by a review that judges; and
	// a finding answered by its key.
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
	addFindingSQL = `INSERT INTO code_findings (id, change_id, review_id, raiser, raiser_kind, revision,
			commit_oid, path, blob_oid, side, line_start, line_end, severity, category, state, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`
	addFindingTextSQL = `INSERT INTO code_finding_texts (change_id, finding_id, bytes, title, body, suggestion)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (change_id, finding_id) DO NOTHING`
	supersedeFindingsSQL = `UPDATE code_findings SET state = ?, resolution_note = '', resolved_by = ?,
			resolved_by_kind = ?, resolved_commit = '', resolved_position = ?
		WHERE rowid IN (SELECT f.rowid FROM json_each(?) AS j CROSS JOIN code_findings AS f
			WHERE f.id = CAST(j.value AS TEXT))`
	answerFindingSQL = `UPDATE code_findings SET state = ?, resolution_note = ?, resolved_by = ?,
			resolved_by_kind = ?, resolved_commit = ?, resolved_position = ?
		WHERE id = ?`
	askReviewerSQL = `INSERT INTO code_reviewers (change_id, reviewer, reviewer_kind, reason, state, requested_by,
			requested_by_kind, note, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (change_id, reviewer) DO UPDATE SET reviewer_kind = excluded.reviewer_kind,
			reason = excluded.reason, state = excluded.state, requested_by = excluded.requested_by,
			requested_by_kind = excluded.requested_by_kind, note = excluded.note, position = excluded.position`
	withdrawReviewerSQL = `UPDATE code_reviewers INDEXED BY ` + reviewersKey + ` SET state = ?, position = ?
		WHERE change_id = ? AND reviewer = ? AND state = 'requested'`
	addParticipantSQL = `INSERT INTO code_participants (change_id, handle, kind, position) VALUES (?, ?, ?, ?)
		ON CONFLICT (change_id, handle) DO NOTHING`
	addCommentSQL = `INSERT INTO code_comments (id, change_id, thread_id, parent_id, finding_id, author,
			author_kind, author_operator_id, anchor, turn_id, body_bytes, position, at, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`
	resolveThreadSQL = `UPDATE code_comments SET resolved = ?, resolved_by = ?, resolved_by_kind = ?
		WHERE id = ? AND change_id = ? AND thread_id = id`
)
