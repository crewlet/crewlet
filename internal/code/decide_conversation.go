package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S CONVERSATION, DECIDED: an ask for review, a comment, a thread's
// resolution, a review and an answer to findings are each a
// [ConversationWrite] its asker hands the repository's committing data node
// ([ChangeWriter.Converse]), which decides it in ONE snapshot as a record of
// its own op ([ConversationOps]) on the CHANGE's subject.
//
// # What the decide holds a write to
//
// Against the rows of one snapshot, in this order; the first rule that fails
// is the answer, and none of them writes anything.
//
// Every write first meets its repository — absent, deleted or being purged is
// repo_deleted, in the sentence of the state it is in ([RepoGone]); archived is
// repo_archived, since an archived repository's changes are read and written
// nothing — then its change, on this repository (change_not_found), then its
// asker: a principal bound to no seat writes nothing (not_permitted), since
// every row of a conversation names who wrote it by the seat they act as.
//
// AN ASK FOR REVIEW (rule D21) takes a LIVE change (change_closed,
// change_merged), and an asker who is an author of the change or reviews the
// repository ([AccessReview]; a person maintains every repository) — else
// not_permitted. Each reviewer it asks is judged on the deciding node's chart
// whoever asks: a handle no seat holds is no_reviewer; a person — a human seat —
// may always be asked; a seat must review the repository and must not be an
// author of the change, whose review of their own change would count for
// nothing — else not_permitted, naming what the seat holds. An ask that would
// leave more than [MaxReviewersPerChange] reviewers asked at once is
// too_many_reviewers.
//
// A COMMENT takes a change in any state — a change is discussed after it
// merges — and an asker who reads the repository (not_permitted, in the
// grant's own sentence). A reply names the ROOT of a thread on this change, or
// a finding on it, whose thread the finding names (thread_gone otherwise): a
// thread is answered at its root, so a thread is one level deep and its
// replies are read in order. An anchored comment's revision is one the change
// has reached (unknown_revision past its head).
//
// A THREAD'S RESOLUTION names the root of a thread on this change
// (thread_gone) — a finding's thread is answered by the finding's own answer —
// and is made by the comment's own author, an author of the change, a
// principal that maintains the repository or a person (not_permitted).
//
// A REVIEW (rules D21 and D22) takes a LIVE change (change_closed,
// change_merged) and a reviewer who reviews the repository ([AccessReview]; a
// person maintains every repository) and did NOT write the change — an
// author's review of their own change, its approval above all, counts for
// nothing, so none is taken (not_permitted) — of a revision the change has
// reached (unknown_revision). What it CARRIES are its reviewer's own findings
// (not_raiser) still standing on the change (finding_gone); every other
// standing finding of its reviewer's raised on an EARLIER revision than the
// one it judges it SUPERSEDES, and a review of the same revision supersedes
// nothing — one reviewer's two passes over one revision. A reviewer keeps at
// most [MaxStandingFindings] standing on a change once its review is taken
// (too_many_findings), so what one review's record rewrites is bounded.
//
// AN ANSWER TO FINDINGS names findings STANDING on the change (finding_gone —
// none by that id, or one dismissed or superseded, which nothing moves again):
// resolved, won't fix and disputed are an author's to give (not_author), and a
// dismissal its raiser's, a maintainer's or a person's (not_raiser). Only a
// dismissal or its raiser's later review clears a blocker (S40): an author's
// answer leaves it standing, for its raiser to judge.
//
// A REVISION'S FACTS ([Revise], revise.go) are the repository's home git
// node's, which no principal asks: they take a LIVE change (change_closed,
// change_merged), and are decided only while they are of the change's head
// revision against its current target, later than the facts it holds of that
// revision against that branch, and matched against the path owners the
// snapshot holds — else SUPERSEDED, recorded nowhere, for the home's next pass.
// The chart then judges whom they ask, skip and wake (revise.go's head).
//
// A CHANGE'S MERGEABILITY ([Mergeability], mergeability.go) is the home's too,
// asked by no principal and judged by no chart: it takes a LIVE change
// (change_closed, change_merged), and is decided only while it is of the
// change's head revision against its current target, and of a tip no earlier
// than the one the change's row holds for that revision against that branch —
// the same tip only where that row is undecided — else SUPERSEDED, recorded
// nowhere.
//
// A write that asks for what the conversation already says — every reviewer
// it asks already asked and nobody it withdraws still asked, a thread's state
// it is in, every finding in the state an answer gives it — is ANSWERED
// UNCHANGED from the rows, with no record. A comment and a review are never
// unchanged: two saying the same are two, and one asked again under its own
// operation id is collapsed by the ledger.
//
// # A seat's write, and every ask for review, is decided only by a node that can judge it
//
// A rule that judges a SEAT reads the chart of the deciding node's epoch
// ([SeatRules]), as a seat's push does, and so does every ask for review,
// whoever asks, since a reviewer is judged by it ([ConversationWrite.JudgesChart]):
// a node with no chart yet, or one confirmed behind the company's
// configuration, decides none of those and says so about itself
// ([ErrNotServing]), so the router asks the repository's runner-up under the
// same operation id. A person's comment or resolution is decided as ever.

// Validate refuses a write no change's writer could decide
// ([ErrInvalidProposal], naming the field), before anything is read: anything
// but exactly one of the seven records; an operation id this engine does not
// accept (statelog.CheckCallerOpID); an asker that is no principal, or that
// names a run — and a revise or a mergeability that names any asker, each
// being the home's own; a Seen that is no position on the code log; a
// repository id that is no UUID in its canonical spelling; a change id that is no bare
// UUIDv7; a field the writer stamps — who wrote it, a reviewer's kind, a
// comment's id or thread, a review's id or a finding's — set by the asker; an
// ask that names nobody, a handle twice or one that is no seat's handle, past
// [MaxReviewersPerChange] in either list, or a note its bound refuses; a
// comment its bound refuses, one replying to a root and a finding both, an
// anchor on a reply or one [anchorProblem] refuses, or a turn its bound
// refuses; a thread named by no comment id; a review of no revision, of a
// verdict this build does not know, that comments and says nothing, past
// [MaxFindingsPerReview] findings or [MaxStandingFindings] carried, a finding
// [findingProblem] refuses, a carried id twice or no finding's id, or a turn
// or a model its bound refuses; an answer naming no finding, past
// [MaxFindingsPerReview], one twice, an answer no write gives, a commit that
// is no whole object id or a note its bound refuses; a revise or a
// mergeability its own rules refuse ([Revise], revise.go; [Mergeability],
// mergeability.go); and a record past what one write of a conversation may
// carry ([MaxConversationBytes]). What the
// repository and the change hold is the decide's to judge, from the rows.
func (w ConversationWrite) Validate() error {
	_, err := w.measure()
	return err
}

// measure validates w and answers its record's payload's bytes as the writer
// stamps it, held under [MaxConversationBytes]: a review past it is refused,
// split into two reviews of the same revision — which supersede nothing of
// each other — since its findings are its own to raise in either.
func (w ConversationWrite) measure() (int, error) {
	if err := w.validFields(); err != nil {
		return 0, err
	}
	op, payload := w.record()
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: encode it: %w", ErrInvalidProposal, op, err)
	}
	// THE STAMPED FIELDS AT THEIR WIDEST: the asker as who wrote it, a kind
	// on every reviewer, a comment's id and thread, and a review's id and
	// its findings' at a UUID's length.
	const stampedID = len(`,"comment_id":"00000000-0000-7000-8000-000000000000"`)
	stamped := len(encoded) + principalBytes(w.Asker)
	switch {
	case w.Reviewers != nil:
		stamped += len(w.Reviewers.Request) * len(`,"kind":"person"`)
	case w.Comment != nil:
		stamped += 2 * stampedID
	case w.Review != nil:
		stamped += (1 + len(w.Review.Findings)) * stampedID
	case w.Revise != nil:
		// THE HOME'S OWN WRITE NAMES NO ASKER, so nothing is stamped as who
		// wrote it: what the writer stamps is whom the facts ask, skip and
		// wake.
		stamped += stampedReviseBytes(*w.Revise) - principalBytes(w.Asker)
	case w.Mergeability != nil:
		// THE HOME'S OWN WRITE, which names no asker and is stamped with
		// nothing: the record is what the home sent.
		stamped -= principalBytes(w.Asker)
	}
	if stamped > MaxConversationBytes {
		why := fmt.Sprintf("%d bytes, past the %d one write of a change's conversation may take of its record "+
			"(MaxConversationBytes)", stamped, MaxConversationBytes)
		if w.Review != nil {
			why += ": split it into two reviews of the same revision, which supersede nothing of each other"
		}
		return 0, fmt.Errorf("%w: %s: %s", ErrInvalidProposal, op, why)
	}
	return stamped, nil
}

// principalBytes is what p takes of a record it is stamped into as who wrote
// it.
func principalBytes(p Principal) int {
	encoded, err := json.Marshal(p)
	if err != nil {
		return 0
	}
	return len(encoded) + len(`,"by":`)
}

// record is the op and the payload w carries, as its asker sent it — "" and
// nil for a write that carries none, which [ConversationWrite.validFields]
// refuses before anything reads this.
func (w ConversationWrite) record() (OpKind, any) {
	switch {
	case w.Reviewers != nil:
		return OpReviewers, *w.Reviewers
	case w.Comment != nil:
		return OpComment, *w.Comment
	case w.ThreadResolve != nil:
		return OpThreadResolve, *w.ThreadResolve
	case w.Review != nil:
		return OpReview, *w.Review
	case w.FindingResolve != nil:
		return OpFindingResolve, *w.FindingResolve
	case w.Revise != nil:
		return OpRevise, *w.Revise
	case w.Mergeability != nil:
		return OpMergeability, *w.Mergeability
	}
	return "", nil
}

// RepoID is the repository w's change is on, by whichever record it carries —
// "" for one that carries none, which the estate's router refuses as it
// refuses a push naming no repository. It is the ONE answer the router keys w
// by and the estate's seam raises the repository's floor for, so the two never
// name different repositories.
func (w ConversationWrite) RepoID() string {
	switch {
	case w.Reviewers != nil:
		return w.Reviewers.RepoID
	case w.Comment != nil:
		return w.Comment.RepoID
	case w.ThreadResolve != nil:
		return w.ThreadResolve.RepoID
	case w.Review != nil:
		return w.Review.RepoID
	case w.FindingResolve != nil:
		return w.FindingResolve.RepoID
	case w.Revise != nil:
		return w.Revise.RepoID
	case w.Mergeability != nil:
		return w.Mergeability.RepoID
	}
	return ""
}

// ChangeID is the change w is about, by whichever record it carries: the
// subject its record arbitrates on.
func (w ConversationWrite) ChangeID() string {
	switch {
	case w.Reviewers != nil:
		return w.Reviewers.ChangeID
	case w.Comment != nil:
		return w.Comment.ChangeID
	case w.ThreadResolve != nil:
		return w.ThreadResolve.ChangeID
	case w.Review != nil:
		return w.Review.ChangeID
	case w.FindingResolve != nil:
		return w.FindingResolve.ChangeID
	case w.Revise != nil:
		return w.Revise.ChangeID
	case w.Mergeability != nil:
		return w.Mergeability.ChangeID
	}
	return ""
}

// JudgesChart reports a write whose decide reads the deciding node's chart: a
// SEAT's — its grant on the repository — every ask of reviewers, whoever
// asks, since each reviewer is judged by the chart, and every revision's
// facts, whose path owners the chart resolves to the seats they are asked of
// and judges. A change's mergeability reads none: it is two commits merged.
// ONE ANSWER, which the
// estate declares the operation's grant by and the writer refuses a node that
// cannot judge one by, so the two never disagree about which writes a node
// behind the company's configuration may decide.
func (w ConversationWrite) JudgesChart() bool {
	return w.Revise != nil || w.Asker.Kind.readsChart() || (w.Reviewers != nil && len(w.Reviewers.Request) > 0)
}

// validFields is every rule of [ConversationWrite.Validate] but the record's
// bytes.
func (w ConversationWrite) validFields() error {
	set := 0
	for _, isSet := range []bool{w.Reviewers != nil, w.Comment != nil, w.ThreadResolve != nil, w.Review != nil,
		w.FindingResolve != nil, w.Revise != nil, w.Mergeability != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%w: a write of a change's conversation is exactly one of an ask for review, a "+
			"comment, a thread's resolution, a review, an answer to findings, a revision's facts and a change's "+
			"mergeability, and this one sets %d", ErrInvalidProposal, set)
	}
	if err := statelog.CheckCallerOpID(w.OpID); err != nil {
		return badField("op_id", "%v", err)
	}
	switch why := w.Asker.invalid(); {
	case w.Revise != nil || w.Mergeability != nil:
		// THE HOME'S OWN WRITE: a revision's facts and a change's
		// mergeability are read by the repository's home git node and asked
		// by no principal, so one naming a principal would be judged as
		// somebody nobody named.
		if w.Asker != (Principal{}) {
			return badField("asker", "a revision's facts and a change's mergeability are the repository's "+
				"home git node's own, which no principal asks")
		}
	case why != "":
		return fmt.Errorf("%w: asker: %s", ErrInvalidProposal, why)
	case !w.Asker.Run.IsZero():
		return badField("asker.run", "a comment keeps the turn that said it in its own field, and an asker "+
			"names no run")
	}
	if !w.Seen.IsZero() {
		switch {
		case w.Seen.Stream != topics.CodeLogStream:
			return badField("seen", "%s is a position on another log than the code log (%s)", w.Seen,
				topics.CodeLogStream)
		case w.Seen.Valid() != nil:
			return fmt.Errorf("%w: seen: %w", ErrInvalidProposal, w.Seen.Valid())
		}
	}
	if err := validRepoID("repo_id", w.RepoID()); err != nil {
		return err
	}
	if err := bareUUIDv7("change_id", w.ChangeID()); err != nil {
		return err
	}
	switch {
	case w.Reviewers != nil:
		return w.Reviewers.validFields()
	case w.Comment != nil:
		return w.Comment.validFields()
	case w.Review != nil:
		return w.Review.validFields()
	case w.FindingResolve != nil:
		return w.FindingResolve.validFields()
	case w.Revise != nil:
		return w.Revise.validFields()
	case w.Mergeability != nil:
		return w.Mergeability.validFields()
	}
	if err := bareUUIDv7("thread_resolve.thread_id", w.ThreadResolve.ThreadID); err != nil {
		return err
	}
	return stampedBy("thread_resolve", w.ThreadResolve.By)
}

// validFields holds a review to [ConversationWrite.Validate]'s rules: a
// revision from 1, a verdict this build knows, a summary and every finding
// within their bounds — and a comment says something, its summary or a
// finding — at most [MaxFindingsPerReview] findings, and its carried
// findings named once each, as bare UUIDv7s, at most [MaxStandingFindings];
// its id and its reviewer the writer's to stamp.
func (r *Review) validFields() error {
	switch {
	case r.ReviewID != "":
		return badField("review.review_id", "the writer derives a review's id from the write's operation id, "+
			"and an asker sends none")
	case r.Revision < 1:
		return badField("review.revision", "%d; a change's revisions count from 1", r.Revision)
	case !r.Verdict.Valid():
		return badField("review.verdict", "%q is not one of %v", r.Verdict, Verdicts())
	case r.Verdict == VerdictComment && strings.TrimSpace(r.Summary) == "" && len(r.Findings) == 0:
		return badField("review", "a review that judges nothing says something: a summary or a finding")
	case len(r.Findings) > MaxFindingsPerReview:
		return badField("review.findings", "%d findings, past the %d one review raises (MaxFindingsPerReview): "+
			"split it into two reviews of the same revision", len(r.Findings), MaxFindingsPerReview)
	case len(r.Carries) > MaxStandingFindings:
		return badField("review.carries", "%d findings, past the %d one reviewer keeps standing on a change "+
			"(MaxStandingFindings)", len(r.Carries), MaxStandingFindings)
	}
	if why := textProblem(r.Summary, MaxReviewSummaryBytes, "review's summary"); why != "" {
		return badField("review.summary", "%s", why)
	}
	for i, f := range r.Findings {
		if why := findingProblem(f); why != "" {
			return badField(fmt.Sprintf("review.findings[%d]", i), "%s", why)
		}
	}
	carried := map[string]bool{}
	for i, id := range r.Carries {
		if err := bareUUIDv7(fmt.Sprintf("review.carries[%d]", i), id); err != nil {
			return err
		}
		if carried[id] {
			return badField(fmt.Sprintf("review.carries[%d]", i), "%s is carried twice", id)
		}
		carried[id] = true
	}
	for _, label := range []struct{ field, value string }{
		{"review.turn_id", r.TurnID}, {"review.model", r.Model},
	} {
		if why := labelProblem(label.value); why != "" {
			return badField(label.field, "%s", why)
		}
	}
	return stampedBy("review", r.By)
}

// validFields holds an answer to findings to [ConversationWrite.Validate]'s
// rules: 1 to [MaxFindingsPerReview] findings, each named once as a bare
// UUIDv7, its resolution one a write may make, its note and its commit within
// their bounds; who answers the writer's to stamp.
func (r *FindingResolve) validFields() error {
	switch {
	case len(r.Findings) == 0:
		return badField("finding_resolve.findings", "an answer names the findings it answers, and this one "+
			"names none")
	case len(r.Findings) > MaxFindingsPerReview:
		return badField("finding_resolve.findings", "%d findings, past the %d one answer names "+
			"(MaxFindingsPerReview)", len(r.Findings), MaxFindingsPerReview)
	}
	named := map[string]bool{}
	for i, f := range r.Findings {
		field := fmt.Sprintf("finding_resolve.findings[%d]", i)
		if err := bareUUIDv7(field+".finding_id", f.FindingID); err != nil {
			return err
		}
		switch {
		case named[f.FindingID]:
			return badField(field+".finding_id", "%s is answered twice", f.FindingID)
		case !f.Resolution.Resolution():
			return badField(field+".resolution", "%q is no answer to a finding: resolved, wont_fix, disputed or "+
				"dismissed", f.Resolution)
		case f.Commit != "" && !gitref.ValidOID(f.Commit):
			return badField(field+".commit", "%q is not a whole object id", f.Commit)
		}
		if why := textProblem(f.Note, MaxReviewNoteBytes, "answer's note"); why != "" {
			return badField(field+".note", "%s", why)
		}
		named[f.FindingID] = true
	}
	return stampedBy("finding_resolve", r.By)
}

// validFields holds an ask for review to [ConversationWrite.Validate]'s rules.
func (r *ReviewerRequest) validFields() error {
	switch {
	case len(r.Request) == 0 && len(r.Withdraw) == 0:
		return badField("reviewers", "an ask for review names a reviewer to ask or one to withdraw, and "+
			"this one names neither")
	case len(r.Request) > MaxReviewersPerChange:
		return badField("reviewers.request", "%d reviewers, past the %d a change asks at once "+
			"(MaxReviewersPerChange)", len(r.Request), MaxReviewersPerChange)
	case len(r.Withdraw) > MaxReviewersPerChange:
		return badField("reviewers.withdraw", "%d reviewers, past the %d a change asks at once "+
			"(MaxReviewersPerChange)", len(r.Withdraw), MaxReviewersPerChange)
	}
	named := map[string]bool{}
	for i, rv := range r.Request {
		field := fmt.Sprintf("reviewers.request[%d]", i)
		switch {
		case handleProblem(rv.Handle) != "":
			return badField(field+".handle", "%s", handleProblem(rv.Handle))
		case named[rv.Handle]:
			return badField(field+".handle", "%q is named twice", rv.Handle)
		case rv.Kind != "":
			return badField(field+".kind", "the writer stamps a reviewer's kind from the chart, and an asker "+
				"names the handle alone")
		}
		named[rv.Handle] = true
	}
	for i, h := range r.Withdraw {
		field := fmt.Sprintf("reviewers.withdraw[%d]", i)
		switch {
		case handleProblem(h) != "":
			return badField(field, "%s", handleProblem(h))
		case named[h]:
			return badField(field, "%q is named twice — asked and withdrawn, or withdrawn twice", h)
		}
		named[h] = true
	}
	if why := textProblem(r.Note, MaxReviewNoteBytes, "request's note"); why != "" {
		return badField("reviewers.note", "%s", why)
	}
	return stampedBy("reviewers", r.By)
}

// validFields holds a comment to [ConversationWrite.Validate]'s rules.
func (c *Comment) validFields() error {
	switch {
	case c.CommentID != "":
		return badField("comment.comment_id", "the writer derives a comment's id from the write's operation "+
			"id, and an asker sends none")
	case c.ThreadID != "":
		return badField("comment.thread_id", "the writer stamps the thread a comment is in — a reply names "+
			"its thread's root as its parent_id")
	}
	switch {
	case c.ParentID != "" && c.FindingID != "":
		return badField("comment.finding_id", "a comment replies to a thread's root or to a finding, never both")
	case c.ParentID != "":
		if err := bareUUIDv7("comment.parent_id", c.ParentID); err != nil {
			return err
		}
	case c.FindingID != "":
		if err := bareUUIDv7("comment.finding_id", c.FindingID); err != nil {
			return err
		}
	}
	if (c.ParentID != "" || c.FindingID != "") && c.Anchor != nil {
		return badField("comment.anchor", "a reply is about its thread, whose first comment or finding carries "+
			"the anchor; a reply carries none")
	}
	if why := commentProblem(c.Body); why != "" {
		return badField("comment.body", "%s", why)
	}
	if c.Anchor != nil {
		if why := anchorProblem(*c.Anchor); why != "" {
			return badField("comment.anchor", "%s", why)
		}
	}
	if why := labelProblem(c.TurnID); why != "" {
		return badField("comment.turn_id", "%s", why)
	}
	return stampedBy("comment", c.By)
}

// conversationDecision is one round's decision of a write of a change's
// conversation: its refusal and sentence where it is refused; unchanged where
// the rows already say what it asks; the record's payload, stamped, where it
// forms one, and the rows its apply will touch; and the conversation's version
// and position as the snapshot read them either way.
type conversationDecision struct {
	refusal   ConversationRefusal
	detail    string
	unchanged bool
	payload   any
	rows      int
	version   int64
	position  int64
}

// refuse is d refused under code with detail.
func (d conversationDecision) refuse(code ConversationRefusal, detail string) (conversationDecision, error) {
	d.refusal, d.detail = code, detail
	return d, nil
}

// decideConversation is one round's decision of w over the rows tx holds — see
// this file's head for the rules, in their order. Pure over (the rows, w,
// seats, retention): seats is the deciding epoch's chart, which a write that
// [ConversationWrite.JudgesChart] is judged by and any other may leave unset;
// retention is code.deleted_retention_days on the deciding epoch, which a
// repo_deleted refusal names the purge's earliest instant by.
func decideConversation(ctx context.Context, tx *sql.Tx, w ConversationWrite, seats SeatRules,
	retention time.Duration) (conversationDecision, error) {

	repoID, changeID := w.RepoID(), w.ChangeID()
	repo, found, err := pushRepoIn(ctx, tx, repoID)
	if err != nil {
		return conversationDecision{}, err
	}
	var d conversationDecision
	switch {
	case !found || repo.status == RepoStatusDeleted || repo.status == RepoStatusPurging:
		detail, err := goneDetail(ctx, tx, repoID, repo, found, retention)
		if err != nil {
			return conversationDecision{}, err
		}
		return d.refuse(ConversationRefusalRepoDeleted, detail)
	case repo.status == RepoStatusArchived:
		return d.refuse(ConversationRefusalRepoArchived, fmt.Sprintf("repository %s is archived and "+
			"read-only (code.repos archived: true): its changes are read, and written nothing", repo.name))
	}
	if w.JudgesChart() && seats.Roster == nil {
		// THE CALLER'S BUG: the writer hands such a write to the
		// repository's next writer before deciding it with no chart.
		return conversationDecision{}, fmt.Errorf("code: a write of %s's conversation that the chart judges "+
			"reached the decide with no chart — the writer hands such a write to the repository's next "+
			"committing node", changeID)
	}
	row, found, err := changeRowIn(ctx, tx, changeID)
	if err != nil {
		return conversationDecision{}, err
	}
	if !found || row.repoID != repoID {
		return d.refuse(ConversationRefusalChangeNotFound, fmt.Sprintf("repository %s holds no change %s",
			repo.name, changeID))
	}
	named := ChangeRef(repo.name, row.number)
	if d.version, d.position, _, err = conversationIn(ctx, tx, changeID); err != nil {
		return conversationDecision{}, err
	}
	switch {
	case w.Revise != nil:
		// THE HOME'S OWN, which names no asker: its rules are the facts'
		// guard and the chart's judgment of the path owners.
		return decideRevise(ctx, tx, w, seats.Roster, repo.grantRef(repoID), row, named, d)
	case w.Mergeability != nil:
		// THE HOME'S OWN too, judged by no chart: its rules are the merge's
		// guard alone.
		return decideMergeability(ctx, tx, w, row, named, d)
	}
	asker, kind := w.Asker.party()
	if kind == "" {
		return d.refuse(ConversationRefusalNotPermitted, "this credential is bound to no seat: a change is "+
			"discussed by a person, through a credential bound to their human seat, or by a seat; a credential "+
			"bound to nobody reads only")
	}
	grant := GrantOf(w.Asker, repo.grantRef(repoID), seats.Roster)
	switch {
	case w.Reviewers != nil:
		return decideReviewers(ctx, tx, w, seats.Roster, repo.grantRef(repoID), row, named, grant, d)
	case w.Comment != nil:
		return decideComment(ctx, tx, w, row, named, grant, d)
	case w.Review != nil:
		return decideReview(ctx, tx, w, asker, kind, row, named, grant, d)
	case w.FindingResolve != nil:
		return decideFindingResolve(ctx, tx, w, asker, kind, row, named, grant, d)
	}
	return decideThreadResolve(ctx, tx, w, asker, kind, row, named, grant, d)
}

// decideReview is a review's rules over row (rules D21 and D22): a LIVE change
// (change_closed, change_merged); a reviewer who may review the repository and
// did not write the change — an author's review of their own change, its
// approval above all, counts for nothing, so none is taken (not_permitted); a
// revision the change has reached (unknown_revision); every finding it carries
// one of its reviewer's own (not_raiser) still standing on the change
// (finding_gone); and its reviewer left with at most [MaxStandingFindings]
// standing findings once the review supersedes those of earlier revisions it
// does not carry (too_many_findings).
func decideReview(ctx context.Context, tx *sql.Tx, w ConversationWrite, asker string, kind PartyKind,
	row changeRow, named string, grant Grant, d conversationDecision) (conversationDecision, error) {

	switch row.state {
	case ChangeStateClosed:
		return d.refuse(ConversationRefusalChangeClosed, fmt.Sprintf("%s is closed: reopen it before reviewing "+
			"it", named))
	case ChangeStateMerged:
		return d.refuse(ConversationRefusalChangeMerged, fmt.Sprintf("%s is merged, and a merged change is "+
			"reviewed no further", named))
	}
	r := *w.Review
	authors, err := authorsAmong(ctx, tx, row.id, []string{asker})
	if err != nil {
		return conversationDecision{}, err
	}
	switch {
	case authors[asker] == kind:
		return d.refuse(ConversationRefusalNotPermitted, fmt.Sprintf("%s is an author of %s, and an author's "+
			"review of their own change counts for nothing, so none is taken: ask a reviewer", grant.who(), named))
	case grant.Access < AccessReview:
		return d.refuse(ConversationRefusalNotPermitted, fmt.Sprintf("%s holds %s on %s's repository, and a "+
			"review needs review: %s", grant.who(), grant.Access, named, reviewRemedy(grant)))
	case r.Revision > row.revision:
		return d.refuse(ConversationRefusalUnknownRevision, fmt.Sprintf("%s is at revision %d, and the review "+
			"judges revision %d: read the change again", named, row.revision, r.Revision))
	}
	for _, id := range r.Carries {
		f, found, err := findingIn(ctx, tx, id)
		if err != nil {
			return conversationDecision{}, err
		}
		switch {
		case !found || f.changeID != row.id:
			return d.refuse(ConversationRefusalFindingGone, fmt.Sprintf("%s holds no finding %s to carry", named,
				id))
		case f.raiser != asker || f.raiserKind != kind:
			return d.refuse(ConversationRefusalNotRaiser, fmt.Sprintf("finding %s was raised by %s, and a review "+
				"carries its own reviewer's findings alone", id, f.raiser))
		case !f.state.Stands():
			return d.refuse(ConversationRefusalFindingGone, fmt.Sprintf("finding %s is %s: it no longer stands, "+
				"so no review carries it", id, f.state))
		}
	}
	standing, superseded, err := supersededBy(ctx, tx, row.id, asker, kind, r)
	if err != nil {
		return conversationDecision{}, err
	}
	if after := standing - len(superseded) + len(r.Findings); after > MaxStandingFindings {
		return d.refuse(ConversationRefusalTooManyFindings, fmt.Sprintf("the review would leave %s %d standing "+
			"findings on %s, past the %d one reviewer keeps (MaxStandingFindings): dismiss some, or review a "+
			"later revision, which supersedes those it does not carry", grant.who(), after, named,
			MaxStandingFindings))
	}
	r.ReviewID, r.By = ReviewID(w.OpID), w.Asker
	r.Findings = slices.Clone(r.Findings)
	for i := range r.Findings {
		r.Findings[i].ID = FindingID(w.OpID, i)
	}
	d.payload = r
	// THE REVIEW, EVERY FINDING AND ITS TEXT, every finding it supersedes,
	// its reviewer as a participant, and the conversation's row.
	d.rows = 1 + 2*len(r.Findings) + len(superseded) + 2
	return d, nil
}

// decideFindingResolve is an answer to findings' rules over row (rule D21):
// every finding it names standing on the change (finding_gone); resolved,
// won't fix and disputed an author's to give (not_author); a dismissal its
// raiser's, a maintainer's or a person's (not_raiser); and answered unchanged
// where every finding is already in the state it asks for.
func decideFindingResolve(ctx context.Context, tx *sql.Tx, w ConversationWrite, asker string, kind PartyKind,
	row changeRow, named string, grant Grant, d conversationDecision) (conversationDecision, error) {

	fr := *w.FindingResolve
	authors, err := authorsAmong(ctx, tx, row.id, []string{asker})
	if err != nil {
		return conversationDecision{}, err
	}
	moved := 0
	for _, a := range fr.Findings {
		f, found, err := findingIn(ctx, tx, a.FindingID)
		if err != nil {
			return conversationDecision{}, err
		}
		switch {
		case !found || f.changeID != row.id:
			return d.refuse(ConversationRefusalFindingGone, fmt.Sprintf("%s holds no finding %s", named,
				a.FindingID))
		case !f.state.Stands():
			return d.refuse(ConversationRefusalFindingGone, fmt.Sprintf("finding %s is %s: it no longer stands, "+
				"and nothing answers it again", a.FindingID, f.state))
		}
		switch {
		case a.Resolution == FindingDismissed:
			if kind != PartyPerson && (f.raiser != asker || f.raiserKind != kind) && grant.Access < AccessMaintain {
				return d.refuse(ConversationRefusalNotRaiser, fmt.Sprintf("%s did not raise finding %s and holds "+
					"%s on %s's repository: a finding is dismissed by whoever raised it, a principal that "+
					"maintains the repository, or a person", grant.who(), a.FindingID, grant.Access, named))
			}
		case authors[asker] != kind:
			return d.refuse(ConversationRefusalNotAuthor, fmt.Sprintf("%s did not write %s: a finding is resolved, "+
				"marked won't fix or disputed by an author of the change", grant.who(), named))
		}
		if f.state != a.Resolution {
			moved++
		}
	}
	if moved == 0 {
		d.unchanged = true
		return d, nil
	}
	fr.By = w.Asker
	d.payload = fr
	// EVERY FINDING IT ANSWERS, and the conversation's row.
	d.rows = len(fr.Findings) + 1
	return d, nil
}

// supersededBy is how many findings raiser keeps standing on changeID, and
// which of them review supersedes: those raised on a revision before the one
// it judges that it does not carry — a review of the same revision supersedes
// nothing, since two reviews of one revision are one reviewer's two passes over
// it. The decide and the apply read the same rows by the same rule.
func supersededBy(ctx context.Context, tx *sql.Tx, changeID, raiser string, kind PartyKind, r Review) (int,
	[]standingFinding, error) {

	all, err := standingFindings(ctx, tx, changeID, raiser, kind)
	if err != nil {
		return 0, nil, err
	}
	// A SET OF WHAT IT CARRIES: up to MaxStandingFindings against as many
	// standing, which a scan of the list for each would make a million
	// comparisons of a decide and an apply.
	carried := make(map[string]bool, len(r.Carries))
	for _, id := range r.Carries {
		carried[id] = true
	}
	var out []standingFinding
	for _, f := range all {
		if f.revision < r.Revision && !carried[f.id] {
			out = append(out, f)
		}
	}
	return len(all), out, nil
}

// standingFinding is one finding of a raiser's still standing, as a review's
// supersession reads it.
type standingFinding struct {
	id       string
	revision int64
	severity Severity
	state    FindingState
}

// standingFindings is every finding raiser keeps standing on changeID, in one
// seek of code_findings_raiser_idx a standing state: at most
// [MaxStandingFindings] rows, which every review that would add past it is
// refused.
func standingFindings(ctx context.Context, tx *sql.Tx, changeID, raiser string, kind PartyKind) (
	[]standingFinding, error) {

	rows, err := tx.QueryContext(ctx, standingFindingsSQL, changeID, raiser, string(kind), standingStates)
	if err != nil {
		return nil, fmt.Errorf("code: read %s's standing findings on %s: %w", raiser, changeID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []standingFinding
	for rows.Next() {
		var f standingFinding
		var severity, state string
		if err := rows.Scan(&f.id, &f.revision, &severity, &state); err != nil {
			return nil, fmt.Errorf("code: read %s's standing findings on %s: %w", raiser, changeID, err)
		}
		f.severity, f.state = Severity(severity), FindingState(state)
		out = append(out, f)
	}
	return out, rows.Err()
}

// findingRow is what a decide and an apply read of one finding.
type findingRow struct {
	id, changeID string
	raiser       string
	raiserKind   PartyKind
	revision     int64
	severity     Severity
	state        FindingState
}

// findingIn reads the finding id names, and false where no finding has it.
func findingIn(ctx context.Context, tx *sql.Tx, id string) (findingRow, bool, error) {
	var f findingRow
	var kind, severity, state string
	err := tx.QueryRowContext(ctx, findingSQL, id).Scan(&f.id, &f.changeID, &f.raiser, &kind, &f.revision,
		&severity, &state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return findingRow{}, false, nil
	case err != nil:
		return findingRow{}, false, fmt.Errorf("code: read finding %s: %w", id, err)
	}
	f.raiserKind, f.severity, f.state = PartyKind(kind), Severity(severity), FindingState(state)
	return f, true, nil
}

// decideReviewers is an ask for review's rules over row — see this file's head.
func decideReviewers(ctx context.Context, tx *sql.Tx, w ConversationWrite, roster Roster, repo RepoRef,
	row changeRow, named string, grant Grant, d conversationDecision) (conversationDecision, error) {

	switch row.state {
	case ChangeStateClosed:
		return d.refuse(ConversationRefusalChangeClosed, fmt.Sprintf("%s is closed: reopen it before asking "+
			"for its review", named))
	case ChangeStateMerged:
		return d.refuse(ConversationRefusalChangeMerged, fmt.Sprintf("%s is merged, and a merged change is "+
			"reviewed no further", named))
	}
	r := *w.Reviewers
	handles := []string{w.Asker.Seat}
	for _, rv := range r.Request {
		handles = append(handles, rv.Handle)
	}
	authors, err := authorsAmong(ctx, tx, row.id, handles)
	if err != nil {
		return conversationDecision{}, err
	}
	asker, kind := w.Asker.party()
	if kind != PartyPerson && authors[asker] != kind && grant.Access < AccessReview {
		return d.refuse(ConversationRefusalNotPermitted, fmt.Sprintf("%s did not write %s and holds %s on its "+
			"repository: reviewers are asked by an author of the change, a principal that reviews the "+
			"repository, or a person", grant.who(), named, grant.Access))
	}
	stamped := make([]Reviewer, 0, len(r.Request))
	for _, rv := range r.Request {
		seat, ok := roster.Seat(rv.Handle)
		if !ok {
			return d.refuse(ConversationRefusalNoReviewer, fmt.Sprintf("%q is no seat of the chart: a reviewer "+
				"is a seat by its handle, or a person by the human seat they hold", rv.Handle))
		}
		if seat.Human {
			stamped = append(stamped, Reviewer{Handle: rv.Handle, Kind: PartyPerson})
			continue
		}
		g := GrantOf(Principal{Kind: PrincipalAgent, Seat: rv.Handle}, repo, roster)
		switch {
		case g.none != "":
			return d.refuse(ConversationRefusalNotPermitted, fmt.Sprintf("seat %q cannot be asked: %s",
				rv.Handle, g.none))
		case g.Access < AccessReview:
			return d.refuse(ConversationRefusalNotPermitted, fmt.Sprintf("seat %q holds %s on this repository "+
				"(tier %s, relation %s), and a reviewer reviews it: %s", rv.Handle, g.Access, g.Tier.spelled(),
				g.Relation, reviewRemedy(g)))
		case authors[rv.Handle] == PartyAgent:
			return d.refuse(ConversationRefusalNotPermitted, fmt.Sprintf("seat %q is an author of %s, and an "+
				"author's review of their own change counts for nothing, so none is asked of them", rv.Handle,
				named))
		}
		stamped = append(stamped, Reviewer{Handle: rv.Handle, Kind: PartyAgent})
	}
	states, requested, err := reviewerStates(ctx, tx, row.id, append(slices.Clone(handles[1:]), r.Withdraw...))
	if err != nil {
		return conversationDecision{}, err
	}
	after, moved := requested, 0
	for _, rv := range stamped {
		if states[rv.Handle] != ReviewerRequested {
			after++
			moved++
		}
	}
	for _, h := range r.Withdraw {
		if states[h] == ReviewerRequested {
			after--
			moved++
		}
	}
	if moved == 0 {
		d.unchanged = true
		return d, nil
	}
	if after > MaxReviewersPerChange {
		return d.refuse(ConversationRefusalTooManyReviewers, fmt.Sprintf("%s would ask %d reviewers at once, "+
			"past the %d a change asks (MaxReviewersPerChange): withdraw an ask first", named, after,
			MaxReviewersPerChange))
	}
	r.By, r.Request = w.Asker, stamped
	d.payload = r
	// A ROW EACH REVIEWER ASKED AND EACH WITHDRAWN, a participant row each
	// asked, and the conversation's own.
	d.rows = 2*len(r.Request) + len(r.Withdraw) + 1
	return d, nil
}

// reviewRemedy is how a seat below review on a repository comes to review it:
// its tier, where the chart already relates it to the repository by more than
// a read and the tier alone caps it; else whoever leads the repository's
// owner, whose chart the relation is — a seat related by a read alone reviews
// nothing whatever its tier.
func reviewRemedy(g Grant) string {
	if g.Relation >= AccessReview {
		return "a person can set its role.code.access to review or full_access"
	}
	return "ask " + g.lead()
}

// decideComment is a comment's rules over row — see this file's head.
func decideComment(ctx context.Context, tx *sql.Tx, w ConversationWrite, row changeRow, named string,
	grant Grant, d conversationDecision) (conversationDecision, error) {

	if !grant.MayRead() {
		return d.refuse(ConversationRefusalNotPermitted, grant.ReadRefusal())
	}
	c := *w.Comment
	c.CommentID, c.By = CommentID(w.OpID), w.Asker
	c.ThreadID = c.CommentID
	switch {
	case c.ParentID != "":
		parent, found, err := commentIn(ctx, tx, c.ParentID)
		if err != nil {
			return conversationDecision{}, err
		}
		switch {
		case !found || parent.changeID != row.id:
			return d.refuse(ConversationRefusalThreadGone, fmt.Sprintf("%s holds no comment %s to reply to",
				named, c.ParentID))
		case !parent.root():
			return d.refuse(ConversationRefusalThreadGone, fmt.Sprintf("comment %s is a reply in thread %s: a "+
				"thread is answered at its root", c.ParentID, parent.threadID))
		}
		c.ThreadID = parent.id
	case c.FindingID != "":
		// A FINDING'S THREAD IS NAMED BY THE FINDING, whatever state it is
		// in: what a dismissed or superseded finding said is still talked
		// about.
		finding, found, err := findingIn(ctx, tx, c.FindingID)
		if err != nil {
			return conversationDecision{}, err
		}
		if !found || finding.changeID != row.id {
			return d.refuse(ConversationRefusalThreadGone, fmt.Sprintf("%s holds no finding %s to reply to",
				named, c.FindingID))
		}
		c.ThreadID = c.FindingID
	}
	if c.Anchor != nil && c.Anchor.Revision > row.revision {
		return d.refuse(ConversationRefusalUnknownRevision, fmt.Sprintf("%s is at revision %d, and the comment "+
			"is anchored to revision %d: read the change again", named, row.revision, c.Anchor.Revision))
	}
	d.payload = c
	// THE COMMENT, ITS AUTHOR AS A PARTICIPANT, and the conversation's row.
	d.rows = 3
	return d, nil
}

// decideThreadResolve is a thread's resolution's rules over row — see this
// file's head.
func decideThreadResolve(ctx context.Context, tx *sql.Tx, w ConversationWrite, asker string, kind PartyKind,
	row changeRow, named string, grant Grant, d conversationDecision) (conversationDecision, error) {

	t := *w.ThreadResolve
	root, found, err := commentIn(ctx, tx, t.ThreadID)
	if err != nil {
		return conversationDecision{}, err
	}
	if !found || root.changeID != row.id || !root.root() {
		return d.refuse(ConversationRefusalThreadGone, fmt.Sprintf("%s holds no thread %s: a thread is named "+
			"by its first comment's id, and a finding's is answered by the finding's resolution", named,
			t.ThreadID))
	}
	authors, err := authorsAmong(ctx, tx, row.id, []string{asker})
	if err != nil {
		return conversationDecision{}, err
	}
	switch {
	case kind == PartyPerson, root.author == asker && root.authorKind == kind, authors[asker] == kind,
		grant.Access >= AccessMaintain:
	default:
		return d.refuse(ConversationRefusalNotPermitted, fmt.Sprintf("%s did not start thread %s, did not "+
			"write %s and holds %s on its repository: a thread is resolved by whoever started it, an author of "+
			"the change, a principal that maintains the repository, or a person", grant.who(), t.ThreadID,
			named, grant.Access))
	}
	if root.resolved == t.Resolved {
		d.unchanged = true
		return d, nil
	}
	t.By = w.Asker
	d.payload = t
	// THE ROOT, and the conversation's row.
	d.rows = 2
	return d, nil
}

// withinConversation refuses a decision past a record's bytes or rows, which
// [ConversationWrite.Validate]'s bounds keep from ever forming — the check
// makes the invariant the record's own, as [withinChange] does a change's.
func withinConversation(dec statelog.Decision, d conversationDecision) error {
	switch {
	case len(dec.Payload) > MaxRecordBytes:
		return fmt.Errorf("%w: %d bytes, past MaxRecordBytes (%d)", ErrRecordCap, len(dec.Payload), MaxRecordBytes)
	case d.rows > MaxRecordRows:
		return fmt.Errorf("%w: up to %d rows, past MaxRecordRows (%d)", ErrRecordCap, d.rows, MaxRecordRows)
	}
	return nil
}

// conversationIn reads a change's conversation row: its version, the packed
// position of its last record and what its wide text weighs — zeros where the
// change has none yet.
func conversationIn(ctx context.Context, tx *sql.Tx, changeID string) (version, position, textBytes int64,
	err error) {

	err = tx.QueryRowContext(ctx, conversationSQL, changeID).Scan(&version, &position, &textBytes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, 0, 0, nil
	case err != nil:
		return 0, 0, 0, fmt.Errorf("code: read change %s's conversation: %w", changeID, err)
	}
	return version, position, textBytes, nil
}

// commentRow is what a decide and an apply read of one comment.
type commentRow struct {
	id, changeID, threadID, parentID string
	author                           string
	authorKind                       PartyKind
	resolved                         bool
}

// root reports the first comment of a thread of its own: its thread is named
// by its own id. A reply's thread is its root's, and a reply to a finding's the
// finding's.
func (c commentRow) root() bool { return c.threadID == c.id }

// commentIn reads the comment id names, and false where no comment has it.
func commentIn(ctx context.Context, tx *sql.Tx, id string) (commentRow, bool, error) {
	var c commentRow
	var kind string
	err := tx.QueryRowContext(ctx, commentSQL, id).Scan(&c.id, &c.changeID, &c.threadID, &c.parentID, &c.author,
		&kind, &c.resolved)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return commentRow{}, false, nil
	case err != nil:
		return commentRow{}, false, fmt.Errorf("code: read comment %s: %w", id, err)
	}
	c.authorKind = PartyKind(kind)
	return c, true, nil
}

// authorsAmong is which of handles wrote changeID, by the kind each wrote it
// as — in ONE statement over the authors' key.
func authorsAmong(ctx context.Context, tx *sql.Tx, changeID string, handles []string) (map[string]PartyKind,
	error) {

	out := map[string]PartyKind{}
	list, err := jsonList(handles)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, authorsAmongSQL, changeID, list)
	if err != nil {
		return nil, fmt.Errorf("code: read change %s's authors: %w", changeID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var author, kind string
		if err := rows.Scan(&author, &kind); err != nil {
			return nil, fmt.Errorf("code: read change %s's authors: %w", changeID, err)
		}
		out[author] = PartyKind(kind)
	}
	return out, rows.Err()
}

// reviewerStates is the state of each of handles' ask on changeID — absent
// where none was ever made — and how many reviewers the change asks now: one
// statement over the reviewers' key for the handles, and a bounded count.
func reviewerStates(ctx context.Context, tx *sql.Tx, changeID string, handles []string) (map[string]ReviewerState,
	int, error) {

	out := map[string]ReviewerState{}
	list, err := jsonList(handles)
	if err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, reviewersNamedSQL, changeID, list)
	if err != nil {
		return nil, 0, fmt.Errorf("code: read change %s's reviewers: %w", changeID, err)
	}
	for rows.Next() {
		var handle, state string
		if err := rows.Scan(&handle, &state); err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("code: read change %s's reviewers: %w", changeID, err)
		}
		out[handle] = ReviewerState(state)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, fmt.Errorf("code: read change %s's reviewers: %w", changeID, err)
	}
	_ = rows.Close()
	var requested int
	if err := tx.QueryRowContext(ctx, reviewersAskedSQL, changeID, MaxReviewersPerChange+1).Scan(
		&requested); err != nil {
		return nil, 0, fmt.Errorf("code: count change %s's reviewers: %w", changeID, err)
	}
	return out, requested, nil
}

// The conversation's decide reads, each registered in plan_test.go: the
// conversation and a comment by their keys; the authors and the reviewers a
// write names, driven from the list of their handles, one seek of the key a
// handle — the reviewers' key NAMED ([reviewersKey] says why); and how many reviewers a change asks, counted at most
// MaxReviewersPerChange + 1 deep over the reviewers' key — a change's withdrawn
// asks are rows too, and the count need only say whether one more fits.
const (
	conversationSQL = `SELECT version, position, text_bytes FROM code_conversations WHERE change_id = ?`
	commentSQL      = `SELECT id, change_id, thread_id, parent_id, author, author_kind, resolved
		FROM code_comments WHERE id = ?`
	authorsAmongSQL = `SELECT a.author, a.author_kind FROM json_each(?2) AS j CROSS JOIN code_change_authors AS a
		WHERE a.change_id = ?1 AND a.author = CAST(j.value AS TEXT)`
	reviewersNamedSQL = `SELECT r.reviewer, r.state FROM json_each(?2) AS j
		CROSS JOIN code_reviewers AS r INDEXED BY ` + reviewersKey + `
		WHERE r.change_id = ?1 AND r.reviewer = CAST(j.value AS TEXT)`
	reviewersAskedSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_reviewers
		WHERE change_id = ? AND state = 'requested' LIMIT ?)`
	// findingSQL is a finding by its key; standingFindingsSQL a raiser's
	// standing findings on a change, driven from the list of the standing
	// states ([standingStates]), one seek of code_findings_raiser_idx a state —
	// never `state IN (…)`, which the pinned driver seeks no deeper than the
	// raiser, reading every finding they raised on the change, superseded and
	// dismissed ones of every earlier revision among them.
	findingSQL = `SELECT id, change_id, raiser, raiser_kind, revision, severity, state
		FROM code_findings WHERE id = ?`
	standingFindingsSQL = `SELECT f.id, f.revision, f.severity, f.state
		FROM json_each(?4) AS j CROSS JOIN code_findings AS f
		WHERE f.change_id = ?1 AND f.raiser = ?2 AND f.state = CAST(j.value AS TEXT) AND f.raiser_kind = ?3`
	// standingStates is the states [FindingState.Stands] names, as the JSON
	// list standingFindingsSQL is driven from; a test holds the two to each
	// other.
	standingStates = `["open","resolved","wont_fix","disputed"]`
)

// conversationAgain is what a conversation write's asker does after a refusal
// that decided nothing.
const conversationAgain = "send the write again"
