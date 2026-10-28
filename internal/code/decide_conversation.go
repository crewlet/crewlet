package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S CONVERSATION, DECIDED: an ask for review, a comment and a thread's
// resolution are each a [ConversationWrite] its asker hands the repository's
// committing data node ([ChangeWriter.Converse]), which decides it in ONE
// snapshot as a record of its own op ([ConversationOps]) on the CHANGE's subject.
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
// grant's own sentence). A reply names the ROOT of a thread on this change
// (thread_gone otherwise): a thread is answered at its root, so a thread is one
// level deep and its replies are read in order. An anchored comment's revision
// is one the change has reached (unknown_revision past its head).
//
// A THREAD'S RESOLUTION names the root of a thread on this change
// (thread_gone), and is made by the comment's own author, an author of the
// change, a principal that maintains the repository or a person
// (not_permitted).
//
// A write that asks for what the conversation already says — every reviewer
// it asks already asked and nobody it withdraws still asked, a thread's state
// it is in — is ANSWERED UNCHANGED from the rows, with no record. A comment is
// never unchanged: two comments saying the same are two comments, and one
// asked again under its own operation id is collapsed by the ledger.
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
// but exactly one of the three records; an operation id this engine does not
// accept (statelog.CheckCallerOpID); an asker that is no principal, or that
// names a run; a Seen that is no position on the code log; a repository id
// that is no UUID in its canonical spelling; a change id that is no bare
// UUIDv7; a field the writer stamps — who wrote it, a reviewer's kind, a
// comment's id or thread — set by the asker; an ask that names nobody, a handle
// twice or one that is no seat's handle, past [MaxReviewersPerChange] in either
// list, or a note its bound refuses; a comment its bound refuses, an anchor on
// a reply or one [anchorProblem] refuses, or a turn its bound refuses; a thread
// named by no comment id; and a record past what one write may carry
// ([EntryRoom]). What the repository and the change hold is the decide's to
// judge, from the rows.
func (w ConversationWrite) Validate() error {
	_, err := w.measure()
	return err
}

// measure validates w and answers its record's payload's bytes as the writer
// stamps it, held under [EntryRoom].
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
	// on every reviewer, and a comment's id and thread at a UUID's length.
	stamped := len(encoded) + principalBytes(w.Asker)
	switch {
	case w.Reviewers != nil:
		stamped += len(w.Reviewers.Request) * len(`,"kind":"person"`)
	case w.Comment != nil:
		stamped += 2 * len(`,"comment_id":"00000000-0000-7000-8000-000000000000"`)
	}
	if stamped > EntryRoom {
		return 0, fmt.Errorf("%w: %s: %d bytes, past the %d one write may take of its record",
			ErrInvalidProposal, op, stamped, EntryRoom)
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
	}
	return ""
}

// JudgesChart reports a write whose decide reads the deciding node's chart: a
// SEAT's — its grant on the repository — and every ask of reviewers, whoever
// asks, since each reviewer is judged by the chart. ONE ANSWER, which the
// estate declares the operation's grant by and the writer refuses a node that
// cannot judge one by, so the two never disagree about which writes a node
// behind the company's configuration may decide.
func (w ConversationWrite) JudgesChart() bool {
	return w.Asker.Kind.readsChart() || (w.Reviewers != nil && len(w.Reviewers.Request) > 0)
}

// validFields is every rule of [ConversationWrite.Validate] but the record's
// bytes.
func (w ConversationWrite) validFields() error {
	set := 0
	for _, isSet := range []bool{w.Reviewers != nil, w.Comment != nil, w.ThreadResolve != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%w: a write of a change's conversation is exactly one of an ask for review, a "+
			"comment and a thread's resolution, and this one sets %d", ErrInvalidProposal, set)
	}
	if err := statelog.CheckCallerOpID(w.OpID); err != nil {
		return badField("op_id", "%v", err)
	}
	switch why := w.Asker.invalid(); {
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
	}
	if err := bareUUIDv7("thread_resolve.thread_id", w.ThreadResolve.ThreadID); err != nil {
		return err
	}
	return stampedBy("thread_resolve", w.ThreadResolve.By)
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
	if c.ParentID != "" {
		if err := bareUUIDv7("comment.parent_id", c.ParentID); err != nil {
			return err
		}
		if c.Anchor != nil {
			return badField("comment.anchor", "a reply is about its thread, whose first comment carries the "+
				"anchor; a reply carries none")
		}
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
	}
	return decideThreadResolve(ctx, tx, w, asker, kind, row, named, grant, d)
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
	if c.ParentID != "" {
		parent, found, err := commentIn(ctx, tx, c.ParentID)
		if err != nil {
			return conversationDecision{}, err
		}
		switch {
		case !found || parent.changeID != row.id:
			return d.refuse(ConversationRefusalThreadGone, fmt.Sprintf("%s holds no comment %s to reply to",
				named, c.ParentID))
		case parent.parentID != "":
			return d.refuse(ConversationRefusalThreadGone, fmt.Sprintf("comment %s is a reply in thread %s: a "+
				"thread is answered at its root", c.ParentID, parent.threadID))
		}
		c.ThreadID = parent.id
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
	if !found || root.changeID != row.id || root.parentID != "" {
		return d.refuse(ConversationRefusalThreadGone, fmt.Sprintf("%s holds no thread %s: a thread is named "+
			"by its first comment's id", named, t.ThreadID))
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
)

// conversationAgain is what a conversation write's asker does after a refusal
// that decided nothing.
const conversationAgain = "send the write again"
