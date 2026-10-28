package code

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S CONVERSATION: what is said ABOUT a change, as records on the
// change's own subject ([KindChange]) — who is asked to review it, what is said
// on it and the threads it forms. Each is a [ConversationWrite] its asker hands
// the repository's committing data node, whose [ChangeWriter] decides it in ONE
// snapshot and publishes it as a record of its own op ([ConversationOps]); the
// change's lifecycle stays on the repository's subject, decided by the
// committer ([ChangeProposal]).
//
// # Split by subject
//
// A table is written by the records of ONE subject, so two writers deciding
// from two snapshots never contend on one row: the repository's records keep a
// change's row, its body, its authors and its revisions; the change's keep its
// reviewers (code_reviewers), its comments (code_comments), who takes part in
// it (code_participants) and its counters (code_conversations). Only a
// repository's purge removes them, behind the deletion gate that drops every
// record about a purged repository ([Applier.Gated] reads a change record's
// repository off its payload).

// ConversationRefusal is why a change's writer decided nothing of a write of
// its conversation ([ConversationWrite]) — a code its asker reads as "<code>:
// <sentence>", the sentence the decide's ([ConversationResult.Detail]). A named
// string, so a code this build does not know is a value rather than a panic.
// NONE IS EVER RECORDED: a refused write appends nothing, so no record of the
// code log carries one and none takes a versionedFields row.
type ConversationRefusal string

// The codes, in the order the decide's rules reach them (decide_conversation.go)
// and then the writer's own.
const (
	// ConversationRefusalRepoDeleted is a write to a change of a repository
	// that is deleted, being purged, purged or not there at all — in the
	// sentence of the state it is in ([RepoGone]).
	ConversationRefusalRepoDeleted ConversationRefusal = "repo_deleted"
	// ConversationRefusalRepoArchived is a write to a change of an archived
	// repository, which is read-only: its changes are read and written
	// nothing.
	ConversationRefusalRepoArchived ConversationRefusal = "repo_archived"
	// ConversationRefusalChangeNotFound is a write naming a change the
	// repository does not hold.
	ConversationRefusalChangeNotFound ConversationRefusal = "change_not_found"
	// ConversationRefusalNotPermitted is a write its asker may not make: a
	// comment by one who reads nothing of the repository, an ask for review
	// by one who neither wrote the change nor reviews the repository, a
	// thread resolved by one who neither started it nor wrote the change nor
	// maintains the repository, nor is a person — and an ask of a reviewer
	// who may not review it: a seat below review on the repository, or an
	// author of the change, whose review counts for nothing.
	ConversationRefusalNotPermitted ConversationRefusal = "not_permitted"
	// ConversationRefusalNoReviewer is an ask of a reviewer the chart does
	// not hold: no seat by that handle.
	ConversationRefusalNoReviewer ConversationRefusal = "no_reviewer"
	// ConversationRefusalTooManyReviewers is an ask that would leave the
	// change asking more reviewers at once than [MaxReviewersPerChange].
	ConversationRefusalTooManyReviewers ConversationRefusal = "too_many_reviewers"
	// ConversationRefusalUnknownRevision is a comment anchored to a revision
	// past the change's head as the deciding snapshot holds it: the asker
	// read a revision this node has not applied, or none there is.
	ConversationRefusalUnknownRevision ConversationRefusal = "unknown_revision"
	// ConversationRefusalChangeClosed is a write only a live change takes —
	// an ask for review — to a closed one.
	ConversationRefusalChangeClosed ConversationRefusal = "change_closed"
	// ConversationRefusalChangeMerged is a write only a live change takes to
	// a merged one.
	ConversationRefusalChangeMerged ConversationRefusal = "change_merged"
	// ConversationRefusalThreadGone is a reply to, or a resolution of, a
	// thread the change does not hold: no comment by that id, or one that
	// is itself a reply — a thread is answered at its root.
	ConversationRefusalThreadGone ConversationRefusal = "thread_gone"

	// ConversationRefusalBusy is a write the change's writer did not reach in
	// time — its asker's attempt run short behind the change's other writes,
	// or the change moving under every round the write may take — and
	// decided nothing: it is sent again.
	ConversationRefusalBusy ConversationRefusal = "busy"
	// ConversationRefusalNotApplied is a write whose record was durable on
	// the code log and applied nowhere: a gate drops it on every node.
	ConversationRefusalNotApplied ConversationRefusal = "not_applied"
	// ConversationRefusalUnavailable is the code log refusing the write.
	ConversationRefusalUnavailable ConversationRefusal = "unavailable"
)

// ConversationRefusals is every code, in this order.
func ConversationRefusals() []ConversationRefusal {
	return []ConversationRefusal{
		ConversationRefusalRepoDeleted, ConversationRefusalRepoArchived, ConversationRefusalChangeNotFound,
		ConversationRefusalNotPermitted, ConversationRefusalNoReviewer, ConversationRefusalTooManyReviewers,
		ConversationRefusalUnknownRevision, ConversationRefusalChangeClosed, ConversationRefusalChangeMerged,
		ConversationRefusalThreadGone, ConversationRefusalBusy, ConversationRefusalNotApplied,
		ConversationRefusalUnavailable,
	}
}

// Valid reports a code this build knows.
func (r ConversationRefusal) Valid() bool { return slices.Contains(ConversationRefusals(), r) }

// ReviewerState is where an ask of a reviewer is (code_reviewers.state).
type ReviewerState string

const (
	// ReviewerRequested is a reviewer the change asks for a review.
	ReviewerRequested ReviewerState = "requested"
	// ReviewerWithdrawn is a reviewer whose ask was withdrawn: kept, so the
	// change says who was once asked, and asked again by a request naming
	// them.
	ReviewerWithdrawn ReviewerState = "withdrawn"
)

// ReviewerStates is the closed set, in this order.
func ReviewerStates() []ReviewerState { return []ReviewerState{ReviewerRequested, ReviewerWithdrawn} }

// Valid reports a state this build knows.
func (s ReviewerState) Valid() bool { return slices.Contains(ReviewerStates(), s) }

// ReviewerReason is why a reviewer was asked (code_reviewers.reason).
type ReviewerReason string

// ReviewerAsked is a reviewer somebody asked by name — a request for review.
const ReviewerAsked ReviewerReason = "requested"

// ReviewerReasons is the closed set.
func ReviewerReasons() []ReviewerReason { return []ReviewerReason{ReviewerAsked} }

// Valid reports a reason this build knows.
func (r ReviewerReason) Valid() bool { return slices.Contains(ReviewerReasons(), r) }

// THE RECORDS OF A CHANGE'S CONVERSATION, one payload an op ([ConversationOps]),
// each on the change's own subject and written by the repository's committing
// data node ([ChangeWriter]). Each carries its repository and its change and
// the fields its asker chose; the writer STAMPS what no asker sends — who wrote
// it, from the write's asker; a reviewer's kind, from the chart; a comment's
// id and the thread it is in — so a record says what it did with no lookup.
//
// Every field is in the base format, under the condition [KindChange] states.

// Reviewer is one reviewer a request names: a seat by its handle, or a person
// by the human seat they hold. Its Kind is STAMPED BY THE WRITER from the
// deciding node's chart; an asker names the handle alone.
type Reviewer struct {
	Handle string    `json:"handle"`
	Kind   PartyKind `json:"kind,omitempty"`
}

// ReviewerRequest asks reviewers of a live change, withdraws the ask of
// others, or both — each handle at most once across the two lists.
type ReviewerRequest struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// By is who asked. STAMPED BY THE WRITER from the write's asker; an
	// asker sends none.
	By Principal `json:"by,omitzero"`
	// Request is the reviewers asked, at most [MaxReviewersPerChange].
	Request []Reviewer `json:"request,omitempty"`
	// Withdraw is the handles whose ask is withdrawn, at most
	// [MaxReviewersPerChange].
	Withdraw []string `json:"withdraw,omitempty"`
	// Note is what the ask says, at most [MaxReviewNoteBytes]; "" for none.
	Note string `json:"note,omitempty"`
}

// CommentAnchor is where a comment points: a line of one file, in one
// revision of the change, on one side of its diff — the base's file (old) or
// the head's (new) — by the blob it read there, so a later revision can carry
// it to where those lines went.
type CommentAnchor struct {
	// Revision is the change's revision the line was read in, 1 or above.
	Revision int64 `json:"revision"`
	// Path is the file, a clean relative path ([MaxGitPathBytes]).
	Path string `json:"path"`
	// Side is the side of the revision's diff: old or new.
	Side Side `json:"side"`
	// Line is the line, from 1.
	Line int `json:"line"`
	// BlobOID is the file's blob on that side, a whole object id.
	BlobOID string `json:"blob_oid"`
}

// Comment says something on a change: a thread of its own, or a reply to one.
type Comment struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// CommentID is [CommentID] of the write's operation id, and ThreadID the
	// thread it is in — its own id for a thread it starts, the thread's root
	// for a reply. BOTH STAMPED BY THE WRITER; an asker sends neither.
	CommentID string `json:"comment_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	// ParentID is the root of the thread this replies to; "" for a comment
	// that starts a thread of its own. A thread is answered at its root, so
	// a reply never names another reply.
	ParentID string `json:"parent_id,omitempty"`
	// By is who said it. STAMPED BY THE WRITER from the write's asker; an
	// asker sends none.
	By Principal `json:"by,omitzero"`
	// Body is what it says, 1 to [MaxCommentBytes] bytes.
	Body string `json:"body"`
	// Anchor is the line a thread's first comment points at; nil for none,
	// and always nil on a reply, which is about its thread.
	Anchor *CommentAnchor `json:"anchor,omitempty"`
	// TurnID is the turn that said it; "" for none — a person's comment has
	// none.
	TurnID string `json:"turn_id,omitempty"`
}

// ThreadResolve marks a thread of a change's comments resolved, or open
// again.
type ThreadResolve struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// ThreadID is the thread, by its root's id.
	ThreadID string `json:"thread_id"`
	// Resolved is the state it leaves the thread in.
	Resolved bool `json:"resolved"`
	// By is who resolved it. STAMPED BY THE WRITER from the write's asker;
	// an asker sends none.
	By Principal `json:"by,omitzero"`
}

// CommentID is the id of the comment a write under opID says: DERIVED from the
// write's operation id, a bare UUIDv7, as [OpenChangeID] derives an open's
// change — so the comment asked again under its own id, of the repository's
// runner-up after its writer went silent or by an asker whose answer was lost,
// is the same comment, which the operation ledger collapses onto the record
// that landed, and no retry says it twice.
func CommentID(opID string) string {
	at, _ := statelog.OpMintedAt(opID)
	return statelog.DeriveOpID(at, "", "code.comment", opID)
}

// commentProblem is why s is no comment, or "": 1 to [MaxCommentBytes] bytes
// of valid UTF-8 holding no NUL — a line break and a tab are the text's own.
func commentProblem(s string) string {
	if strings.TrimSpace(s) == "" {
		return "a comment says something"
	}
	return textProblem(s, MaxCommentBytes, "comment")
}

// anchorProblem is why a is no anchor a comment may carry, or "": a revision
// from 1, a clean relative path, a side of a diff, a line from 1 and a whole
// object id for the blob read there.
func anchorProblem(a CommentAnchor) string {
	switch {
	case a.Revision < 1:
		return fmt.Sprintf("revision: %d; a change's revisions count from 1", a.Revision)
	case !a.Side.Valid():
		return fmt.Sprintf("side: %q is not one of %v", a.Side, Sides())
	case a.Line < 1:
		return fmt.Sprintf("line: %d; a file's lines count from 1", a.Line)
	case !gitref.ValidOID(a.BlobOID):
		return fmt.Sprintf("blob_oid: %q is not a whole object id", a.BlobOID)
	case !utf8.ValidString(a.Path):
		return "path: not valid UTF-8"
	}
	if why := validGitPath(a.Path, false); why != "" {
		return "path: " + why
	}
	return ""
}

// handleProblem is why h is no handle a reviewer request may name, or "".
func handleProblem(h string) string {
	if !org.ValidHandle(h) {
		return fmt.Sprintf("%q is not a seat's handle", h)
	}
	return ""
}
