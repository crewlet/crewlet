package gitlet

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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// A CHANGE'S CONVERSATION, DECIDED: each [ConversationWrite] ([ConversationOps])
// is decided by the repository's committing data node ([ChangeWriter.Converse])
// in ONE snapshot, as a record of its own op on the change's subject.
// [decideConversation] holds the rules in order, the first that fails being
// the answer and none writing anything: the repository, the change, the
// asker's grant to READ the repository (before any rule admitting a writer by
// its relation to the change), each write's own decideX, and then whoever a
// seat's turn works for ([WriteCeiling.changeRefusal]). A write asking what the
// rows already say is answered unchanged, with no record — never a comment or
// a review: two saying the same are two, and the ledger collapses a retry.

// Validate refuses, before anything is read, a write no change's writer could
// decide ([ErrInvalidProposal], naming the field): not exactly one of the ten
// records; an operation id statelog.CheckCallerOpID refuses; an asker that is
// no principal or names a run, or any asker on the home's own revise,
// mergeability or settling of asks; a WrittenFor [writtenForUnder] refuses; a
// Seen off the code log; a malformed repository or change id; a field the
// writer stamps set by the asker; and each record's own validFields, up to
// [MaxConversationBytes]. What the rows hold is the decide's to judge. A
// WrittenFor value this build does not know is answered first, as its wire
// decoder answers it, never as ErrInvalidProposal ([unknownToThisBuild]): only
// a build that knows the value can judge the write.
func (w ConversationWrite) Validate() error {
	_, err := w.measure()
	return err
}

// measure validates w and answers its record's bytes as the writer will stamp
// it, held under [MaxConversationBytes]: a review past it is refused, for its
// asker to split into two of the same revision, which supersede nothing of
// each other.
func (w ConversationWrite) measure() (int, error) {
	if err := w.validFields(); err != nil {
		return 0, err
	}
	op, payload := w.record()
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: encode it: %w", ErrInvalidProposal, op, err)
	}
	// THE STAMPED FIELDS AT THEIR WIDEST: who wrote it, each reviewer's kind,
	// and every stamped id at a UUID's length.
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
		// THE HOME'S OWN WRITE NAMES NO ASKER: the writer stamps whom the
		// facts ask, skip and wake instead.
		stamped += stampedReviseBytes(*w.Revise) - principalBytes(w.Asker)
	case w.Mergeability != nil:
		// THE HOME'S OWN WRITE: the writer stamps the readiness it judges,
		// and a refresh's merge from the row.
		stamped += stampedMergeabilityBytes(*w.Mergeability) - principalBytes(w.Asker)
	case w.Asks != nil:
		// THE HOME'S OWN WRITE: the writer stamps the asks it moves, at most
		// MaxAsksSettled at a seat's widest handle.
		stamped += len(`,"reviewers":[]`) + MaxAsksSettled*(len(`"",`)+iam.MaxLogin) - principalBytes(w.Asker)
	}
	if stamped > MaxConversationBytes {
		why := fmt.Sprintf("%d bytes, past the %d one write of a change's conversation may take of its record "+
			"(MaxConversationBytes)", stamped, MaxConversationBytes)
		if w.Review != nil {
			why += ": split it into two reviews of the same revision, which supersede nothing of each other"
		}
		return 0, refusedf("%w: %s: %s", ErrInvalidProposal, op, why)
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

// record is the op and the payload w carries, as its asker sent it; "" and nil
// for none, which [ConversationWrite.validFields] refuses first.
func (w ConversationWrite) record() (OpKind, any) {
	switch {
	case w.Reviewers != nil:
		return OpReviewers, *w.Reviewers
	case w.Comment != nil:
		return OpComment, *w.Comment
	case w.ThreadResolve != nil:
		return OpThreadResolve, *w.ThreadResolve
	case w.CommentEdit != nil:
		return OpCommentEdit, *w.CommentEdit
	case w.CommentDelete != nil:
		return OpCommentDelete, *w.CommentDelete
	case w.Review != nil:
		return OpReview, *w.Review
	case w.FindingResolve != nil:
		return OpFindingResolve, *w.FindingResolve
	case w.Revise != nil:
		return OpRevise, *w.Revise
	case w.Mergeability != nil:
		return OpMergeability, *w.Mergeability
	case w.Asks != nil:
		return OpAsks, *w.Asks
	}
	return "", nil
}

// RepoID is the repository w's change is on, by whichever record it carries;
// "" for none, which the estate's router refuses as it does a push naming no
// repository. It is the ONE answer the router keys w by and the estate's seam
// raises the repository's floor for, so the two never disagree.
func (w ConversationWrite) RepoID() string {
	switch {
	case w.Reviewers != nil:
		return w.Reviewers.RepoID
	case w.Comment != nil:
		return w.Comment.RepoID
	case w.ThreadResolve != nil:
		return w.ThreadResolve.RepoID
	case w.CommentEdit != nil:
		return w.CommentEdit.RepoID
	case w.CommentDelete != nil:
		return w.CommentDelete.RepoID
	case w.Review != nil:
		return w.Review.RepoID
	case w.FindingResolve != nil:
		return w.FindingResolve.RepoID
	case w.Revise != nil:
		return w.Revise.RepoID
	case w.Mergeability != nil:
		return w.Mergeability.RepoID
	case w.Asks != nil:
		return w.Asks.RepoID
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
	case w.CommentEdit != nil:
		return w.CommentEdit.ChangeID
	case w.CommentDelete != nil:
		return w.CommentDelete.ChangeID
	case w.Review != nil:
		return w.Review.ChangeID
	case w.FindingResolve != nil:
		return w.FindingResolve.ChangeID
	case w.Revise != nil:
		return w.Revise.ChangeID
	case w.Mergeability != nil:
		return w.Mergeability.ChangeID
	case w.Asks != nil:
		return w.Asks.ChangeID
	}
	return ""
}

// JudgesChart reports a write whose decide reads the deciding node's chart: a
// SEAT's (its grant), every ask of reviewers whoever asks, every revision's
// facts (their path owners), every mergeability (its readiness,
// [ReadinessOf]), and every comment and review that mentions somebody
// (concerns.go). A node with no chart, or one confirmed behind the company's
// configuration, decides none of these and says so ([ErrNotServing]), so the
// router asks the runner-up under the same operation id. ONE ANSWER, which
// the estate declares the operation's grant by and the writer refuses by, so
// the two never disagree about which writes such a node may decide.
func (w ConversationWrite) JudgesChart() bool {
	return w.Revise != nil || w.Mergeability != nil || w.Asker.Kind.readsChart() ||
		(w.Reviewers != nil && len(w.Reviewers.Request) > 0) ||
		(w.Comment != nil && len(w.Comment.Mentions) > 0) || (w.Review != nil && len(w.Review.Mentions) > 0)
}

// validFields is every rule of [ConversationWrite.Validate] but the record's
// bytes.
func (w ConversationWrite) validFields() error {
	// FIRST, A VALUE THIS BUILD DOES NOT KNOW, before any rule judges the
	// write (writtenfor.go's head).
	if err := unknownToThisBuild(writtenForAt, w.WrittenFor); err != nil {
		return err
	}
	set := 0
	for _, isSet := range []bool{w.Reviewers != nil, w.Comment != nil, w.ThreadResolve != nil, w.CommentEdit != nil,
		w.CommentDelete != nil, w.Review != nil, w.FindingResolve != nil, w.Revise != nil, w.Mergeability != nil,
		w.Asks != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%w: a write of a change's conversation is exactly one of an ask for review, a "+
			"comment, a thread's resolution, a comment's edit, a comment's delete, a review, an answer to "+
			"findings, a revision's facts, a change's mergeability and a settling of its asks, and this one sets "+
			"%d", ErrInvalidProposal, set)
	}
	if err := statelog.CheckCallerOpID(w.OpID); err != nil {
		return badField("op_id", "%v", err)
	}
	switch why := w.Asker.invalid(); {
	case w.Revise != nil || w.Mergeability != nil || w.Asks != nil:
		// THE HOME'S OWN WRITE is asked by no principal: one naming a
		// principal would be judged as somebody nobody named.
		if w.Asker != (Principal{}) {
			return badField("asker", "a revision's facts, a change's mergeability and the settling of its asks "+
				"are the repository's home Gitlet node's own, which no principal asks")
		}
	case why != "":
		return refusedf("%w: asker: %s", ErrInvalidProposal, why)
	case !w.Asker.Run.IsZero():
		return badField("asker.run", "a comment keeps the turn that said it in its own field, and an asker "+
			"names no run")
	}
	if w.Ceiling != nil {
		if why := w.Ceiling.invalid(w.Asker); why != "" {
			return fmt.Errorf("%w: %s", ErrInvalidProposal, why)
		}
	}
	if err := writtenForUnder(w.Asker, w.WrittenFor, w.Ceiling, nil); err != nil {
		return refusedCarrier(ErrInvalidProposal, err)
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
	case w.CommentEdit != nil:
		return w.CommentEdit.validFields()
	case w.CommentDelete != nil:
		return w.CommentDelete.validFields()
	case w.Review != nil:
		return w.Review.validFields()
	case w.FindingResolve != nil:
		return w.FindingResolve.validFields()
	case w.Revise != nil:
		return w.Revise.validFields()
	case w.Mergeability != nil:
		return w.Mergeability.validFields()
	case w.Asks != nil:
		return w.Asks.validFields()
	}
	if err := bareUUIDv7("thread_resolve.thread_id", w.ThreadResolve.ThreadID); err != nil {
		return err
	}
	if err := stampedRouting("thread_resolve", w.ThreadResolve.Routing); err != nil {
		return err
	}
	return stampedBy("thread_resolve", w.ThreadResolve.By)
}

// validFields holds a review to [ConversationWrite.Validate]'s rules; its id
// and its reviewer are the writer's to stamp.
func (r *Review) validFields() error {
	switch {
	case r.ReviewID != "":
		return badField("review.review_id", "the writer derives a review's id from the write's operation id, "+
			"and an asker sends none")
	case r.Revision < 1:
		return badField("review.revision", "%d; a change's revisions count from 1", r.Revision)
	case !r.Verdict.Valid():
		return badField("review.verdict", "%q is not one of %s", r.Verdict, words.Or(Verdicts()))
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
	if why := mentionsProblem(r.Mentions); why != "" {
		return badField("review.mentions", "%s", why)
	}
	if err := stampedRouting("review", r.Routing); err != nil {
		return err
	}
	return stampedBy("review", r.By)
}

// validFields holds a settling of a change's asks to
// [ConversationWrite.Validate]'s rules; the asks it moves are the writer's to
// stamp.
func (a *AskSettle) validFields() error {
	switch {
	case a.State != ReviewerEnded && a.State != ReviewerRequested:
		return badField("asks.state", "%q: a change's asks end with it (%s) or are asked again once it is "+
			"reopened (%s)", a.State, ReviewerEnded, ReviewerRequested)
	case len(a.Reviewers) != 0:
		return badField("asks.reviewers", "the writer stamps the asks it moves from its snapshot, and the home "+
			"sends none")
	}
	return nil
}

// validFields holds an answer to findings to [ConversationWrite.Validate]'s
// rules; who answers is the writer's to stamp.
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
	if err := stampedRouting("finding_resolve", r.Routing); err != nil {
		return err
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
	if err := stampedRouting("reviewers", r.Routing); err != nil {
		return err
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
	if why := mentionsProblem(c.Mentions); why != "" {
		return badField("comment.mentions", "%s", why)
	}
	if err := stampedRouting("comment", c.Routing); err != nil {
		return err
	}
	return stampedBy("comment", c.By)
}

// validFields holds a comment's edit to [ConversationWrite.Validate]'s rules;
// who edits is the writer's to stamp.
func (e *CommentEdit) validFields() error {
	if err := bareUUIDv7("comment_edit.comment_id", e.CommentID); err != nil {
		return err
	}
	if why := commentProblem(e.Body); why != "" {
		return badField("comment_edit.body", "%s", why)
	}
	return stampedBy("comment_edit", e.By)
}

// validFields holds a comment's delete to [ConversationWrite.Validate]'s
// rules; who deletes is the writer's to stamp.
func (c *CommentDelete) validFields() error {
	if err := bareUUIDv7("comment_delete.comment_id", c.CommentID); err != nil {
		return err
	}
	return stampedBy("comment_delete", c.By)
}

// conversationDecision is one round's decision of a conversation write: a
// refusal and its sentence, unchanged, or the stamped payload and the rows its
// apply will touch; and either way the conversation's version, position and
// readiness inputs' version as the snapshot read them.
type conversationDecision struct {
	refusal   ConversationRefusal
	detail    string
	unchanged bool
	payload   any
	rows      int
	version   int64
	position  int64
	readiness int64
	// op is the record's op where it is not the write's — a mergeability
	// whose outcome its row holds already, as [OpMergeabilityKept] — else "".
	op OpKind
	// concern is what the record's own op says of whom it concerns, which
	// [routeConversation] completes and stamps ([routingFor]).
	concern concerning
}

// routeConversation stamps the routing of the record d forms ([routingFor]):
// whom its decide said it concerns, completed with the change, who wrote it
// and the deciding chart. A comment's or a review's mentions join the
// change's participants, a row each ([ChangeRouting.Mentions]).
func routeConversation(ctx context.Context, tx *sql.Tx, w ConversationWrite, roster Roster, repoName string,
	row changeRow, d conversationDecision) (conversationDecision, error) {

	if d.payload == nil || d.refusal != "" {
		return d, nil
	}
	c := d.concern
	c.repoName, c.change, c.roster = repoName, row, roster
	// WHO WROTE IT: the asker; for the home's own records, whom the decide
	// found — a revision's pusher ([decideRevise]) — and nobody for a
	// mergeability. FOR WHOM: the asker's WrittenFor, or whom a revision's
	// push was made for (concerns.go's head, "Whom the actor acted for").
	if c.actor == "" {
		c.actor, c.actorKind = w.Asker.party()
		c.workingFor = w.WrittenFor.Carrier()
	}
	r, err := routingFor(ctx, tx, c)
	if err != nil {
		return conversationDecision{}, err
	}
	if r == nil {
		// A RECORD WITH NO AUDIENCE carries no routing: no notice, no wake,
		// no row, and none of its asker's mentions ([withRouting]).
		switch d.payload.(type) {
		case Comment, Review:
			d.payload = withRouting(d.payload, nil)
		}
		return d, nil
	}
	switch {
	case w.Comment != nil, w.Review != nil:
		d.rows += len(r.Mentions)
	}
	// AND A NOTICE EACH HANDLE IT TELLS, and their count (apply_notices.go).
	d.rows += noticeWrites(r)
	d.payload = withRouting(d.payload, r)
	return d, nil
}

// refuse is d refused under code with detail.
func (d conversationDecision) refuse(code ConversationRefusal, detail string) (conversationDecision, error) {
	d.refusal, d.detail = code, detail
	return d, nil
}

// decideConversation is one round's decision of w over tx's rows, its rules in
// order (this file's head). Pure over the rows, w, seats, retention and
// noGitlet: seats is the deciding epoch's chart, which a write that
// [ConversationWrite.JudgesChart] needs and any other may leave unset;
// retention is code.deleted_retention_days, by which a repo_deleted refusal
// names the purge's earliest instant; noGitlet is the persons an ask names who
// cannot use Gitlet, read before the publish ([ChangeWriter.withoutGitlet]) so
// no round reads anything but its snapshot.
func decideConversation(ctx context.Context, tx *sql.Tx, w ConversationWrite, seats SeatRules,
	retention time.Duration, noGitlet map[string]bool) (conversationDecision, error) {

	// WHOM IT IS MADE FOR, ON THE DECIDING CHART, first ([writtenForUnder]):
	// a carrier its ceiling does not bound is its sender's bug, and one only
	// this chart refuses means the chart moved under the turn ([ChartRefusal]).
	if err := writtenForUnder(w.Asker, w.WrittenFor, w.Ceiling, seats.Roster); err != nil {
		return conversationDecision{}, refusedCarrier(ErrInvalidProposal, err)
	}
	repoID, changeID := w.RepoID(), w.ChangeID()
	repo, found, err := pushRepoIn(ctx, tx, repoID)
	if err != nil {
		return conversationDecision{}, err
	}
	var d conversationDecision
	switch {
	case !found || repo.status == RepoStatusDeleted || repo.status == RepoStatusPurging:
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
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
		// THE CALLER'S BUG: the writer hands such a write to the next
		// writer rather than decide it with no chart (JudgesChart).
		return conversationDecision{}, fmt.Errorf("gitlet: a write of %s's conversation that the chart judges "+
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
	if d.version, d.position, d.readiness, err = conversationIn(ctx, tx, changeID); err != nil {
		return conversationDecision{}, err
	}
	switch {
	case w.Revise != nil:
		// THE HOME'S OWN, naming no asker: its rules are revise.go's.
		if d, err = decideRevise(ctx, tx, w, seats.Roster, repo.grantRef(repoID), row, named, d); err != nil {
			return d, err
		}
		return routeConversation(ctx, tx, w, seats.Roster, repo.name, row, d)
	case w.Mergeability != nil:
		// THE HOME'S OWN too: its rules are mergeability.go's.
		if d, err = decideMergeability(ctx, tx, w, seats.Roster, seats.Epoch, repo.grantRef(repoID), row, named,
			d); err != nil {
			return d, err
		}
		return routeConversation(ctx, tx, w, seats.Roster, repo.name, row, d)
	case w.Asks != nil:
		// THE HOME'S OWN as well, judged by no chart and routed to nobody:
		// the close, merge or reopen it follows told its concerned already.
		return decideAsks(ctx, tx, w, row, named, d)
	}
	asker, kind := w.Asker.party()
	// EVERY ROW NAMES ITS WRITER BY SEAT, so a credential bound to none writes
	// nothing.
	if kind == "" {
		return d.refuse(ConversationRefusalNotPermitted, "this credential is bound to no seat: a change is "+
			"discussed by a person, through a credential bound to their human seat, or by a seat; a credential "+
			"bound to nobody reads only")
	}
	// THE GRANT BEFORE EVERY RELATION: authoring, raising a finding and
	// starting a thread are relations to the CHANGE that outlive a seat's
	// standing on its repository; a seat the chart no longer lets read it
	// (it left the chart, or a `visibility: unit` no longer reaches it) reads
	// nothing of the change, so writes nothing of it. A person always passes.
	grant := GrantOf(w.Asker, repo.grantRef(repoID), seats.Roster)
	if !grant.MayRead() {
		return d.refuse(ConversationRefusalNotPermitted, grant.ReadRefusal())
	}
	switch {
	case w.Reviewers != nil:
		d, err = decideReviewers(ctx, tx, w, seats.Roster, repo.grantRef(repoID), row, named, grant, noGitlet, d)
	case w.Comment != nil:
		d, err = decideComment(ctx, tx, w, row, named, d)
	case w.CommentEdit != nil:
		d, err = decideCommentEdit(ctx, tx, w, asker, kind, row, named, d)
	case w.CommentDelete != nil:
		d, err = decideCommentDelete(ctx, tx, w, asker, kind, row, named, grant, d)
	case w.Review != nil:
		d, err = decideReview(ctx, tx, w, asker, kind, row, named, grant, d)
	case w.FindingResolve != nil:
		d, err = decideFindingResolve(ctx, tx, w, asker, kind, row, named, grant, d)
	default:
		d, err = decideThreadResolve(ctx, tx, w, asker, kind, row, named, grant, d)
	}
	if err != nil || d.refusal != "" {
		return d, err
	}
	// AND THEN FOR WHOEVER THE SEAT'S TURN WORKS FOR, once the seat itself
	// may: a colleague held to [ConversationWrite.CeilingNeeds], nobody else
	// to anything but the seat's own work; a refusal here forms nothing, an
	// answer unchanged included.
	what := "write " + named + "'s conversation"
	if w.Review != nil {
		what = "review " + named
	}
	if why := w.Ceiling.changeRefusal(w.Asker, repo.grantRef(repoID), ownChange(w.Asker, row), w.CeilingNeeds(),
		what, seats.Roster); why != "" {
		refused := conversationDecision{version: d.version, position: d.position, readiness: d.readiness}
		return refused.refuse(ConversationRefusalNotPermitted, why)
	}
	if w.CommentEdit != nil || w.CommentDelete != nil {
		// AN EDIT OR A DELETE TELLS NOBODY: no notice, no wake.
		return d, nil
	}
	return routeConversation(ctx, tx, w, seats.Roster, repo.name, row, d)
}

// CeilingNeeds is the least access whoever a seat's turn works for must hold
// on the repository for the seat to make w for them
// ([ConversationWrite.Ceiling]): review for a review, the one write whose
// access alone admits it, and read for every other — internal/agent/builtin's
// changeToolNeeds, held to it by its test.
func (w ConversationWrite) CeilingNeeds() Access {
	if w.Review != nil {
		return AccessReview
	}
	return AccessRead
}

// decideReview is a review's rules over row (rules D21 and D22): a LIVE change
// (change_closed, change_merged); a reviewer who did not write the change,
// since an author's review, its approval above all, counts for nothing
// (own_change), and who reviews the repository (not_permitted); a revision the
// change has reached (unknown_revision); every carried finding its reviewer's
// own (not_raiser) and still standing (finding_gone); and at most
// [MaxStandingFindings] of its reviewer's left standing once it supersedes the
// earlier revisions' it does not carry (too_many_findings), which bounds what
// one review's record rewrites.
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
		return d.refuse(ConversationRefusalOwnChange, fmt.Sprintf("%s is an author of %s, and an author's "+
			"review of their own change counts for nothing, so none is taken: ask a reviewer", grant.who(), named))
	case grant.Access < AccessReview:
		return d.refuse(ConversationRefusalNotPermitted, fmt.Sprintf("%s holds %s on %s's repository, and a "+
			"review needs review: %s", grant.who(), grant.Access, named, reviewRemedy(grant)))
	case r.Revision > row.revision:
		return d.refuse(ConversationRefusalUnknownRevision, fmt.Sprintf("%s is at revision %d, and the review "+
			"judges revision %d: read the change again", named, row.revision, r.Revision))
	}
	// THE REVIEWER'S STANDING FINDINGS, in one walk, are all a review may
	// carry: only a carried id outside them is read by its key, to word its
	// refusal — never MaxStandingFindings point reads a round.
	all, err := standingFindings(ctx, tx, row.id, asker, kind)
	if err != nil {
		return conversationDecision{}, err
	}
	own := make(map[string]bool, len(all))
	for _, f := range all {
		own[f.id] = true
	}
	for _, id := range r.Carries {
		if own[id] {
			continue
		}
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
		return conversationDecision{}, fmt.Errorf("gitlet: finding %s stands, %s's own, on %s, and %s's standing "+
			"findings left it out: standingStates and FindingState.Stands disagree", id, asker, row.id, asker)
	}
	superseded := supersededAmong(all, r)
	if after := len(all) - len(superseded) + len(r.Findings); after > MaxStandingFindings {
		return d.refuse(ConversationRefusalTooManyFindings, fmt.Sprintf("the review would leave %s %d standing "+
			"findings on %s, past the %d one reviewer keeps (MaxStandingFindings): dismiss some, or review a "+
			"later revision, which supersedes those it does not carry", grant.who(), after, named,
			MaxStandingFindings))
	}
	r.ReviewID, r.By = ReviewID(w.OpID), w.Asker
	d.concern = concerning{authors: true, verdict: r.Verdict, mentions: r.Mentions}
	r.Findings = slices.Clone(r.Findings)
	for i := range r.Findings {
		r.Findings[i].ID = FindingID(w.OpID, i)
	}
	d.payload = r
	// THE REVIEW, EVERY FINDING AND ITS TEXT, every finding it supersedes,
	// its reviewer as a participant, the ask it answers, and the
	// conversation's row.
	d.rows = 1 + 2*len(r.Findings) + len(superseded) + 3
	return d, nil
}

// decideFindingResolve is an answer to findings' rules over row (rule D21):
// every finding it names standing on the change (finding_gone), since nothing
// moves a dismissed or superseded one again; resolved, won't fix and disputed
// an author's to give (not_author), and a dismissal its raiser's, a
// maintainer's or a person's (not_raiser) — an author's answer leaves a
// blocker standing for its raiser to judge (S40); unchanged where every
// finding is already in the state asked.
func decideFindingResolve(ctx context.Context, tx *sql.Tx, w ConversationWrite, asker string, kind PartyKind,
	row changeRow, named string, grant Grant, d conversationDecision) (conversationDecision, error) {

	fr := *w.FindingResolve
	authors, err := authorsAmong(ctx, tx, row.id, []string{asker})
	if err != nil {
		return conversationDecision{}, err
	}
	// EVERY FINDING IT NAMES READ IN ONE STATEMENT, driven from their list.
	ids := make([]string, len(fr.Findings))
	for i, a := range fr.Findings {
		ids[i] = a.FindingID
	}
	held, err := findingsAmong(ctx, tx, ids)
	if err != nil {
		return conversationDecision{}, err
	}
	moved := 0
	for _, a := range fr.Findings {
		f, found := held[a.FindingID]
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
			// THE RAISER IS TOLD its finding was answered — once however
			// many of theirs the answer moves.
			d.concern.raisers = append(d.concern.raisers, Reviewer{Handle: f.raiser, Kind: f.raiserKind})
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
// which of them review supersedes: those of an earlier revision it does not
// carry — two reviews of one revision are one reviewer's two passes, so
// supersede nothing. The decide and the apply both read by this rule.
func supersededBy(ctx context.Context, tx queryer, changeID, raiser string, kind PartyKind, r Review) (int,
	[]standingFinding, error) {

	all, err := standingFindings(ctx, tx, changeID, raiser, kind)
	if err != nil {
		return 0, nil, err
	}
	return len(all), supersededAmong(all, r), nil
}

// supersededAmong is which of a raiser's standing findings all review
// supersedes ([supersededBy]).
func supersededAmong(all []standingFinding, r Review) []standingFinding {
	// A SET: up to MaxStandingFindings carried against as many standing
	// would be a million comparisons scanned as a list.
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
	return out
}

// standingFinding is one finding of a raiser's still standing, as a review's
// supersession reads it.
type standingFinding struct {
	id       string
	revision int64
	severity Severity
	state    FindingState
}

// standingFindings is every finding raiser keeps standing on changeID, one
// seek of code_findings_raiser_idx a standing state: at most
// [MaxStandingFindings] rows, since a review adding past it is refused.
func standingFindings(ctx context.Context, tx queryer, changeID, raiser string, kind PartyKind) (
	[]standingFinding, error) {

	rows, err := tx.QueryContext(ctx, standingFindingsSQL, changeID, raiser, string(kind), standingStates)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's standing findings on %s: %w", raiser, changeID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []standingFinding
	for rows.Next() {
		var f standingFinding
		var severity, state string
		if err := rows.Scan(&f.id, &f.revision, &severity, &state); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's standing findings on %s: %w", raiser, changeID, err)
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

// findingsAmong reads the findings ids names, in one statement driven from
// the list; an id no finding has is absent.
func findingsAmong(ctx context.Context, tx queryer, ids []string) (map[string]findingRow, error) {
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, findingsAmongSQL, list)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %d findings: %w", len(ids), err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]findingRow, len(ids))
	for rows.Next() {
		var f findingRow
		var kind, severity, state string
		if err := rows.Scan(&f.id, &f.changeID, &f.raiser, &kind, &f.revision, &severity, &state); err != nil {
			return nil, fmt.Errorf("gitlet: read %d findings: %w", len(ids), err)
		}
		f.raiserKind, f.severity, f.state = PartyKind(kind), Severity(severity), FindingState(state)
		out[f.id] = f
	}
	return out, rows.Err()
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
		return findingRow{}, false, fmt.Errorf("gitlet: read finding %s: %w", id, err)
	}
	f.raiserKind, f.severity, f.state = PartyKind(kind), Severity(severity), FindingState(state)
	return f, true, nil
}

// decideReviewers is an ask for review's rules over row (rule D21): a LIVE
// change (change_closed, change_merged), asked by an author, a principal that
// reviews the repository or a person (not_permitted). Each reviewer is judged
// on the deciding chart whoever asks: a handle no seat holds is no_reviewer; a
// person is asked only if noGitlet does not name them, since an ask discloses
// the change's code (ADR-0110); a seat must review the repository and not be
// an author, whose review counts for nothing (not_permitted). Unchanged where
// every reviewer is asked already and none withdrawn still is; past
// [MaxReviewersPerChange] asked at once is too_many_reviewers.
func decideReviewers(ctx context.Context, tx *sql.Tx, w ConversationWrite, roster Roster, repo RepoRef,
	row changeRow, named string, grant Grant, noGitlet map[string]bool,
	d conversationDecision) (conversationDecision, error) {

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
			// A PERSON READS A CHANGE ONLY THROUGH GITLET, and an ask
			// discloses its whole diff, so one without it is asked nothing
			// (ADR-0110).
			if noGitlet[rv.Handle] {
				return d.refuse(ConversationRefusalNotPermitted, fmt.Sprintf("%q has no Gitlet permission, so "+
					"they cannot read %s and are asked to review none of it: an administrator gives them Gitlet "+
					"first", rv.Handle, named))
			}
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
			// ASKED BY THIS RECORD, and so told; one asked already was
			// told then.
			d.concern.requested = append(d.concern.requested, rv)
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

// decideAsks is a settling of a change's asks with its lifecycle over row: an
// end takes a closed or merged change and an ask again a live one, each moving
// the asks the other left (requested to ended, ended to requested), at most
// [MaxAsksSettled] by handle, stamped on the record. A change that moved after
// its home read it, or holds no such ask, is SUPERSEDED, recorded nowhere, for
// the home's next pass.
func decideAsks(ctx context.Context, tx *sql.Tx, w ConversationWrite, row changeRow, named string,
	d conversationDecision) (conversationDecision, error) {

	a := *w.Asks
	from, ends := ReviewerEnded, a.State == ReviewerEnded
	if ends {
		from = ReviewerRequested
	}
	switch live := row.state.Live(); {
	case ends && live:
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s is %s, and its asks are owed while it is "+
			"live", named, row.state))
	case !ends && !live:
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s is %s, and the asks its end left are "+
			"asked again only once it is reopened", named, row.state))
	}
	held, err := asksIn(ctx, tx, row.id, from, MaxAsksSettled)
	if err != nil {
		return conversationDecision{}, err
	}
	if len(held) == 0 {
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s holds no ask %s", named, from))
	}
	a.Reviewers = held
	d.payload = a
	// EVERY ASK IT MOVES, and the conversation's row.
	d.rows = len(held) + 1
	return d, nil
}

// asksIn is the handles of changeID's asks in state, by handle, at most limit:
// one range of the reviewers' key, which leads with the change.
func asksIn(ctx context.Context, tx *sql.Tx, changeID string, state ReviewerState, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, asksInStateSQL, changeID, string(state), limit)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read change %s's asks %s: %w", changeID, state, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var handle string
		if err := rows.Scan(&handle); err != nil {
			return nil, fmt.Errorf("gitlet: read change %s's asks %s: %w", changeID, state, err)
		}
		out = append(out, handle)
	}
	return out, rows.Err()
}

// reviewRemedy is how a seat below review on a repository comes to review it:
// its tier, where its relation already passes a read and the tier alone caps
// it; else ask whoever leads the repository's owner, since a seat related by a
// read alone reviews nothing whatever its tier.
func reviewRemedy(g Grant) string {
	if g.Relation >= AccessReview {
		return "a person can set its role.code.access to review or full_access"
	}
	return "ask " + g.lead()
}

// decideComment is a comment's rules over row: a change in any state, since a
// change is discussed after it merges; a reply names a thread's ROOT on this
// change, keeping threads one level deep and read in order, or a finding on
// it, whose thread the finding names (thread_gone); an anchor is on a revision
// the change has reached (unknown_revision).
func decideComment(ctx context.Context, tx *sql.Tx, w ConversationWrite, row changeRow, named string,
	d conversationDecision) (conversationDecision, error) {

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
		// A FINDING'S THREAD IS NAMED BY THE FINDING, whatever its state: a
		// dismissed or superseded one is still talked about.
		finding, found, err := findingIn(ctx, tx, c.FindingID)
		if err != nil {
			return conversationDecision{}, err
		}
		if !found || finding.changeID != row.id {
			return d.refuse(ConversationRefusalThreadGone, fmt.Sprintf("%s holds no finding %s to reply to",
				named, c.FindingID))
		}
		c.ThreadID = c.FindingID
		d.concern.raisers = []Reviewer{{Handle: finding.raiser, Kind: finding.raiserKind}}
	}
	if c.Anchor != nil && c.Anchor.Revision > row.revision {
		return d.refuse(ConversationRefusalUnknownRevision, fmt.Sprintf("%s is at revision %d, and the comment "+
			"is anchored to revision %d: read the change again", named, row.revision, c.Anchor.Revision))
	}
	d.concern.thread, d.concern.mentions = c.ThreadID, c.Mentions
	d.payload = c
	// THE COMMENT, ITS AUTHOR AS A PARTICIPANT, and the conversation's row —
	// and a participant row each mention, counted as the routing names them.
	d.rows = 3
	return d, nil
}

// decideThreadResolve is a thread's resolution's rules over row: the root of a
// thread on this change (thread_gone; a finding's thread is answered by the
// finding's own answer), resolved by its starter, an author of the change, a
// maintainer or a person (not_permitted); unchanged where it already is.
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

// decideCommentEdit is a comment's edit's rules over row: a comment of this
// change not deleted (comment_gone), edited by its author alone
// (not_permitted); unchanged where its words are the ones it holds.
func decideCommentEdit(ctx context.Context, tx *sql.Tx, w ConversationWrite, asker string, kind PartyKind,
	row changeRow, named string, d conversationDecision) (conversationDecision, error) {

	e := *w.CommentEdit
	c, found, err := commentStateIn(ctx, tx, e.CommentID)
	if err != nil {
		return conversationDecision{}, err
	}
	switch {
	case !found || c.changeID != row.id:
		return d.refuse(ConversationRefusalCommentGone, fmt.Sprintf("%s holds no comment %s", named,
			e.CommentID))
	case c.deleted:
		return d.refuse(ConversationRefusalCommentGone, fmt.Sprintf("comment %s on %s is deleted, and a "+
			"deleted comment takes no edit", e.CommentID, named))
	case c.author != asker || c.authorKind != kind:
		return d.refuse(ConversationRefusalNotPermitted, fmt.Sprintf("comment %s on %s is %s's: a comment is "+
			"edited by its author alone", e.CommentID, named, c.author))
	case c.body == e.Body:
		d.unchanged = true
		return d, nil
	}
	e.By = w.Asker
	d.payload = e
	// THE COMMENT, and the conversation's row.
	d.rows = 2
	return d, nil
}

// decideCommentDelete is a comment's delete's rules over row: a comment of
// this change (comment_gone), deleted by its author, a principal that
// maintains the repository or a person (not_permitted); unchanged where it is
// deleted already.
func decideCommentDelete(ctx context.Context, tx *sql.Tx, w ConversationWrite, asker string, kind PartyKind,
	row changeRow, named string, grant Grant, d conversationDecision) (conversationDecision, error) {

	del := *w.CommentDelete
	c, found, err := commentStateIn(ctx, tx, del.CommentID)
	if err != nil {
		return conversationDecision{}, err
	}
	switch {
	case !found || c.changeID != row.id:
		return d.refuse(ConversationRefusalCommentGone, fmt.Sprintf("%s holds no comment %s", named,
			del.CommentID))
	case c.deleted:
		d.unchanged = true
		return d, nil
	case kind == PartyPerson, c.author == asker && c.authorKind == kind, grant.Access >= AccessMaintain:
	default:
		return d.refuse(ConversationRefusalNotPermitted, fmt.Sprintf("%s did not write comment %s on %s and "+
			"holds %s on its repository: a comment is deleted by its author, a principal that maintains the "+
			"repository, or a person", grant.who(), del.CommentID, named, grant.Access))
	}
	del.By = w.Asker
	d.payload = del
	// THE COMMENT, and the conversation's row; the notices quoting it are
	// this node's own (code_notifications is divergent).
	d.rows = 2
	return d, nil
}

// commentState is what an edit's and a delete's decide read of one comment:
// its change, its author, whether it is deleted and its words.
type commentState struct {
	changeID, author string
	authorKind       PartyKind
	deleted          bool
	body             string
}

// commentStateIn reads the comment id names, and false where no comment has
// it. Apart from [commentIn]: its columns sit past the body, which a thread's
// resolution and a reply need not read.
func commentStateIn(ctx context.Context, tx *sql.Tx, id string) (commentState, bool, error) {
	var (
		c       commentState
		kind    string
		deleted int64
	)
	err := tx.QueryRowContext(ctx, commentStateSQL, id).Scan(&c.changeID, &c.author, &kind, &deleted, &c.body)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return commentState{}, false, nil
	case err != nil:
		return commentState{}, false, fmt.Errorf("gitlet: read comment %s: %w", id, err)
	}
	c.authorKind, c.deleted = PartyKind(kind), deleted != 0
	return c, true, nil
}

// withinConversation refuses a decision past a record's bytes or rows, which
// [ConversationWrite.Validate]'s bounds keep from forming: it makes the
// invariant the record's own, as [withinChange] does a change's.
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
// position of its last record and its readiness inputs' version — zeros
// where the change has none yet.
func conversationIn(ctx context.Context, tx *sql.Tx, changeID string) (version, position, readiness int64,
	err error) {

	err = tx.QueryRowContext(ctx, conversationSQL, changeID).Scan(&version, &position, &readiness)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, 0, 0, nil
	case err != nil:
		return 0, 0, 0, fmt.Errorf("gitlet: read change %s's conversation: %w", changeID, err)
	}
	return version, position, readiness, nil
}

// commentRow is what a decide and an apply read of one comment.
type commentRow struct {
	id, changeID, threadID, parentID string
	author                           string
	authorKind                       PartyKind
	resolved                         bool
}

// root reports a thread's first comment, whose thread is named by its own id;
// a reply's is its root's, and a reply to a finding's the finding's.
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
		return commentRow{}, false, fmt.Errorf("gitlet: read comment %s: %w", id, err)
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
		return nil, fmt.Errorf("gitlet: read change %s's authors: %w", changeID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var author, kind string
		if err := rows.Scan(&author, &kind); err != nil {
			return nil, fmt.Errorf("gitlet: read change %s's authors: %w", changeID, err)
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
		return nil, 0, fmt.Errorf("gitlet: read change %s's reviewers: %w", changeID, err)
	}
	for rows.Next() {
		var handle, state string
		if err := rows.Scan(&handle, &state); err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("gitlet: read change %s's reviewers: %w", changeID, err)
		}
		out[handle] = ReviewerState(state)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, fmt.Errorf("gitlet: read change %s's reviewers: %w", changeID, err)
	}
	_ = rows.Close()
	var requested int
	if err := tx.QueryRowContext(ctx, reviewersAskedSQL, changeID, MaxReviewersPerChange+1).Scan(
		&requested); err != nil {
		return nil, 0, fmt.Errorf("gitlet: count change %s's reviewers: %w", changeID, err)
	}
	return out, requested, nil
}

// The conversation's decide reads, each registered in plan_test.go. Authors
// and reviewers are driven from the list of handles, one seek a handle, the
// reviewers' key NAMED ([reviewersKey] says why); the asks are counted at most
// MaxReviewersPerChange + 1 deep, since withdrawn asks are rows too and the
// count need only say whether one more fits.
const (
	conversationSQL = `SELECT version, position, readiness_version FROM code_conversations WHERE change_id = ?`
	commentSQL      = `SELECT id, change_id, thread_id, parent_id, author, author_kind, resolved
		FROM code_comments WHERE id = ?`
	commentStateSQL = `SELECT change_id, author, author_kind, deleted_at, body FROM code_comments WHERE id = ?`
	authorsAmongSQL = `SELECT a.author, a.author_kind FROM json_each(?2) AS j CROSS JOIN code_change_authors AS a
		WHERE a.change_id = ?1 AND a.author = CAST(j.value AS TEXT)`
	reviewersNamedSQL = `SELECT r.reviewer, r.state FROM json_each(?2) AS j
		CROSS JOIN code_reviewers AS r INDEXED BY ` + reviewersKey + `
		WHERE r.change_id = ?1 AND r.reviewer = CAST(j.value AS TEXT)`
	reviewersAskedSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_reviewers
		WHERE change_id = ? AND state = 'requested' LIMIT ?)`
	// asksInStateSQL is a change's asks in one state, by handle — the
	// reviewers' key NAMED, for reviewersNamedSQL's reason.
	asksInStateSQL = `SELECT reviewer FROM code_reviewers INDEXED BY ` + reviewersKey + `
		WHERE change_id = ? AND state = ? ORDER BY reviewer LIMIT ?`
	// standingFindingsSQL is driven from the list of standing states
	// ([standingStates]), one seek of code_findings_raiser_idx a state —
	// never `state IN (…)`, which the pinned driver seeks no deeper than the
	// raiser, reading every finding they ever raised on the change.
	findingSQL = `SELECT id, change_id, raiser, raiser_kind, revision, severity, state
		FROM code_findings WHERE id = ?`
	findingsAmongSQL = `SELECT f.id, f.change_id, f.raiser, f.raiser_kind, f.revision, f.severity, f.state
		FROM json_each(?) AS j CROSS JOIN code_findings AS f WHERE f.id = CAST(j.value AS TEXT)`
	standingFindingsSQL = `SELECT f.id, f.revision, f.severity, f.state
		FROM json_each(?4) AS j CROSS JOIN code_findings AS f
		WHERE f.change_id = ?1 AND f.raiser = ?2 AND f.state = CAST(j.value AS TEXT) AND f.raiser_kind = ?3`
	// standingStates is [FindingState.Stands]'s states as a JSON list; a test
	// holds the two to each other.
	standingStates = `["open","resolved","wont_fix","disputed"]`
)

// conversationAgain is what a conversation write's asker does after a refusal
// that decided nothing.
const conversationAgain = "send the write again"
