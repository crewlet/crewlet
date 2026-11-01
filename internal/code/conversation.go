package code

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S CONVERSATION: what is said ABOUT a change, as records on the
// change's own subject ([KindChange]) — who is asked to review it, what is said
// on it and the threads it forms, its reviews and the findings they raise, and
// the answers to those. Each is a [ConversationWrite] its asker hands the
// repository's committing data node, whose [ChangeWriter] decides it in ONE
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
// it and each reviewer's standing verdict (code_participants), its reviews
// (code_reviews), their findings (code_findings, code_finding_texts), its
// counters (code_conversations), the facts the repository's home git node
// recorded of its revisions (code_revision_facts, [Revise]) and whether its
// head revision merges into its target (code_mergeability, [Mergeability]).
// Only a repository's purge removes them,
// behind the deletion gate that drops every record about a purged repository
// ([Applier.Gated] reads a change record's repository off its scope).
//
// # Who clears a blocker (S40)
//
// A finding STANDS while it is open, or an author answered it resolved, won't
// fix or disputed: an author's answer is theirs to give and the raiser's to
// judge, so a standing BLOCKER holds its change back whatever its authors
// said. Exactly two things clear one: its DISMISSAL — by its raiser, a
// principal that maintains the repository, or a person — and its raiser's
// REVIEW OF A LATER REVISION that does not carry it, which supersedes it with
// every other standing finding of theirs it leaves out. A review of the same
// revision supersedes nothing: one reviewer's second pass over one revision
// adds to the first. Who may clear a blocker is said by the rows alone, as a
// precondition read in one snapshot, never as a workflow somebody has to
// remember.

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
	// ConversationRefusalNotPermitted is a write its asker may not make: any
	// write by a seat that reads nothing of the repository — whatever it
	// wrote, raised or started on the change while it did — an ask for
	// review by one who neither wrote the change nor reviews the repository,
	// a thread resolved by one who neither started it nor wrote the change
	// nor maintains the repository, nor is a person — and an ask of a
	// reviewer who may not review it: a seat below review on the repository,
	// or an author of the change, whose review counts for nothing.
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
	// is itself a reply — a thread is answered at its root — and a reply to
	// a finding the change does not hold.
	ConversationRefusalThreadGone ConversationRefusal = "thread_gone"
	// ConversationRefusalNotAuthor is a finding resolved, marked won't fix
	// or disputed by one who did not write the change: answering a finding
	// is its authors'.
	ConversationRefusalNotAuthor ConversationRefusal = "not_author"
	// ConversationRefusalNotRaiser is a finding dismissed by one who neither
	// raised it nor maintains the repository, nor is a person — or carried
	// by a review of one who did not raise it.
	ConversationRefusalNotRaiser ConversationRefusal = "not_raiser"
	// ConversationRefusalFindingGone is a finding a write names that does
	// not STAND on the change: none by that id, or one already dismissed or
	// superseded, which no write moves again.
	ConversationRefusalFindingGone ConversationRefusal = "finding_gone"
	// ConversationRefusalTooManyFindings is a review that would leave its
	// reviewer more than [MaxStandingFindings] standing findings on the
	// change.
	ConversationRefusalTooManyFindings ConversationRefusal = "too_many_findings"
	// ConversationRefusalSuperseded is a revision's facts ([Revise]) or a
	// change's mergeability ([Mergeability]) the change no longer takes: of a
	// revision or a head it has moved from, against a branch it no longer
	// merges into, no later than the facts it holds of that revision against
	// that branch, or matched against path owners the repository has changed
	// since; a merge into a tip before the one the change's mergeability is
	// of, or into the same tip once that one is decided; a refresh of a merge
	// the change does not hold, or of a readiness judged at the inputs the
	// snapshot holds already. The home's own refusal, which no principal is
	// ever answered: its next pass reads the rows again.
	ConversationRefusalSuperseded ConversationRefusal = "superseded"

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
		ConversationRefusalThreadGone, ConversationRefusalNotAuthor, ConversationRefusalNotRaiser,
		ConversationRefusalFindingGone, ConversationRefusalTooManyFindings, ConversationRefusalSuperseded,
		ConversationRefusalBusy, ConversationRefusalNotApplied, ConversationRefusalUnavailable,
	}
}

// Valid reports a code this build knows.
func (r ConversationRefusal) Valid() bool { return slices.Contains(ConversationRefusals(), r) }

// ReviewerState is where an ask of a reviewer is (code_reviewers.state). An
// ask WAITS — its reviewer's review is owed — only while it is requested:
// what the listing of the changes waiting on a reviewer walks, what a change's
// readiness waits on, and what a change's bound on its asks counts
// ([MaxReviewersPerChange]). Every other state is the ask answered, ended or
// taken back, kept so the change says who was once asked.
type ReviewerState string

const (
	// ReviewerRequested is a reviewer the change waits on for a review.
	ReviewerRequested ReviewerState = "requested"
	// ReviewerWithdrawn is a reviewer whose ask was withdrawn: kept, so the
	// change says who was once asked, and asked again by a request naming
	// them.
	ReviewerWithdrawn ReviewerState = "withdrawn"
	// ReviewerReviewed is a reviewer whose review answered the ask — settled
	// by the review's own record. Asked again by a request naming them, and
	// by a later revision's first facts where their standing verdict asks
	// for changes ([Revise.ReReview]): the change is theirs to look at again.
	//
	// BASE FORMAT, under the condition [KindChange] states: a value only an
	// apply writes, from the records' own ops, so a build that knows the ops
	// knows the value.
	ReviewerReviewed ReviewerState = "reviewed"
	// ReviewerEnded is a reviewer whose ask was still waiting when the change
	// closed or merged — settled by the change's own [AskSettle] record, which
	// asks it again if the change is reopened.
	//
	// BASE FORMAT, under the condition [KindChange] states, as
	// [ReviewerReviewed].
	ReviewerEnded ReviewerState = "ended"
)

// ReviewerStates is the closed set, in this order.
func ReviewerStates() []ReviewerState {
	return []ReviewerState{ReviewerRequested, ReviewerWithdrawn, ReviewerReviewed, ReviewerEnded}
}

// Valid reports a state this build knows.
func (s ReviewerState) Valid() bool { return slices.Contains(ReviewerStates(), s) }

// ReviewerReason is why a reviewer was asked (code_reviewers.reason).
type ReviewerReason string

const (
	// ReviewerAsked is a reviewer somebody asked by name — a request for
	// review.
	ReviewerAsked ReviewerReason = "requested"
	// ReviewerPathOwner is a reviewer a revision's facts asked ([Revise]):
	// the seat a path owner whose globs the revision touches resolves to — a
	// unit's effective lead, or the seat itself. No principal asked, so its
	// row names no asker.
	//
	// BASE FORMAT, under the condition [KindChange] states: a value only a
	// revise's apply writes, from the record's own op, so a build that knows
	// the op knows the value.
	ReviewerPathOwner ReviewerReason = "path_owner"
)

// ReviewerReasons is the closed set.
func ReviewerReasons() []ReviewerReason { return []ReviewerReason{ReviewerAsked, ReviewerPathOwner} }

// Valid reports a reason this build knows.
func (r ReviewerReason) Valid() bool { return slices.Contains(ReviewerReasons(), r) }

// THE RECORDS OF A CHANGE'S CONVERSATION, one payload an op ([ConversationOps]),
// each on the change's own subject and written by the repository's committing
// data node ([ChangeWriter]). Each carries its repository and its change and
// the fields its asker chose; the writer STAMPS what no asker sends — who wrote
// it, from the write's asker; a reviewer's kind, from the chart; a comment's
// id and the thread it is in — so a record says what it did with no lookup.
//
// Every field is in the base format, under the condition [KindChange] states —
// Mentions and Routing among them, under the condition concerns.go states for
// a routing.

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
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// WRITER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
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
	// FindingID is the finding this replies to — its thread is the
	// finding's, named by the finding's id — and "" for a comment that
	// replies to none; never beside a ParentID.
	//
	// BASE FORMAT, under the condition [KindChange] states; a field a
	// comment gained after its op, so once a build without it has run it
	// takes {Name: "Comment.FindingID", Since: 2, Op: "comment", Path:
	// []string{"mutation", "finding_id"}}, which an older build defers the
	// record by rather than writing the reply into a thread of its own.
	FindingID string `json:"finding_id,omitempty"`
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
	// Mentions is the handles its body names, as its asker resolved them
	// against the chart it acts in, at most [MaxMentions]; the writer holds
	// them to its own chart, and each is told ([ReasonMention]) and joins
	// the change's participants.
	Mentions []string `json:"mentions,omitempty"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// WRITER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
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
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// WRITER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// Verdict is what a review says of the revision it judges.
type Verdict string

const (
	// VerdictApprove approves the revision: an approval that counts where
	// its reviewer may review the repository and did not write the change.
	VerdictApprove Verdict = "approve"
	// VerdictRequestChanges asks for changes before the change merges.
	VerdictRequestChanges Verdict = "request_changes"
	// VerdictComment says something and judges nothing.
	VerdictComment Verdict = "comment"
)

// Verdicts is the closed set, in this order.
func Verdicts() []Verdict { return []Verdict{VerdictApprove, VerdictRequestChanges, VerdictComment} }

// Valid reports a verdict this build knows.
func (v Verdict) Valid() bool { return slices.Contains(Verdicts(), v) }

// Severity is how much a finding weighs. A BLOCKER is the one that holds a
// change back until it is cleared (S40): only its dismissal — by its raiser, a
// maintainer or a person — or its raiser's review of a later revision that does
// not carry it clears one; resolved, won't fix and disputed keep it standing.
type Severity string

const (
	SeverityBlocker Severity = "blocker"
	SeverityMajor   Severity = "major"
	SeverityMinor   Severity = "minor"
	SeverityNit     Severity = "nit"
)

// Severities is the closed set, heaviest first.
func Severities() []Severity {
	return []Severity{SeverityBlocker, SeverityMajor, SeverityMinor, SeverityNit}
}

// Valid reports a severity this build knows.
func (s Severity) Valid() bool { return slices.Contains(Severities(), s) }

// Category is what a finding is about.
type Category string

const (
	CategoryCorrectness Category = "correctness"
	CategorySecurity    Category = "security"
	CategoryPerformance Category = "performance"
	CategoryTests       Category = "tests"
	CategoryDesign      Category = "design"
	CategoryStyle       Category = "style"
	CategoryDocs        Category = "docs"
)

// Categories is the closed set, in this order.
func Categories() []Category {
	return []Category{CategoryCorrectness, CategorySecurity, CategoryPerformance, CategoryTests, CategoryDesign,
		CategoryStyle, CategoryDocs}
}

// Valid reports a category this build knows.
func (c Category) Valid() bool { return slices.Contains(Categories(), c) }

// FindingState is where a finding is (code_findings.state). The first four
// STAND — the finding is still what its raiser said, whatever its authors
// answered — and the last two do not.
type FindingState string

const (
	// FindingOpen is a finding nobody has answered.
	FindingOpen FindingState = "open"
	// FindingResolved is one an author says the change now answers.
	FindingResolved FindingState = "resolved"
	// FindingWontFix is one an author says the change will not answer.
	FindingWontFix FindingState = "wont_fix"
	// FindingDisputed is one an author says is wrong.
	FindingDisputed FindingState = "disputed"
	// FindingDismissed is one its raiser, a maintainer or a person
	// withdrew: it no longer stands, and no write moves it again.
	FindingDismissed FindingState = "dismissed"
	// FindingSuperseded is one its raiser's review of a later revision did
	// not carry: it no longer stands, and no write moves it again.
	FindingSuperseded FindingState = "superseded"
)

// FindingStates is the closed set, the standing states first.
func FindingStates() []FindingState {
	return []FindingState{FindingOpen, FindingResolved, FindingWontFix, FindingDisputed, FindingDismissed,
		FindingSuperseded}
}

// Valid reports a state this build knows.
func (s FindingState) Valid() bool { return slices.Contains(FindingStates(), s) }

// Stands reports a state in which a finding is still what its raiser said:
// open, resolved, won't fix or disputed. A standing BLOCKER holds its change
// back; resolved, won't fix and disputed are an author's answers, which the
// raiser judges — by dismissing it, or by not carrying it into a review of a
// later revision.
func (s FindingState) Stands() bool {
	return s == FindingOpen || s == FindingResolved || s == FindingWontFix || s == FindingDisputed
}

// Resolution reports a state a write may move a finding to
// ([FindingResolution.Resolution]): an author's resolved, won't fix and
// disputed, and a dismissal. Superseded is a later review's alone.
func (s FindingState) Resolution() bool {
	return s == FindingResolved || s == FindingWontFix || s == FindingDisputed || s == FindingDismissed
}

// Finding is one thing a review says about lines of a change: where — a file,
// a side of the revision's diff, its lines, the commit and the blob they were
// read in — how much it weighs, what it is about, and what it says, with the
// lines it would put in their place where it suggests some. Its id is STAMPED
// BY THE WRITER ([FindingID]); an asker sends none.
type Finding struct {
	ID        string   `json:"id,omitempty"`
	Path      string   `json:"path"`
	Side      Side     `json:"side"`
	LineStart int      `json:"line_start"`
	LineEnd   int      `json:"line_end"`
	CommitOID string   `json:"commit_oid"`
	BlobOID   string   `json:"blob_oid"`
	Severity  Severity `json:"severity"`
	Category  Category `json:"category"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	// Suggestion is the lines the finding would put in place of the ones it
	// is anchored to; nil for none, and "" a suggestion to delete them.
	Suggestion *string `json:"suggestion,omitempty"`
}

// Review judges one revision of a live change: a verdict, a summary, the
// findings it raises — every one in this one record — and the reviewer's own
// standing findings it CARRIES, which a review of a later revision supersedes
// every other one of.
type Review struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// ReviewID is [ReviewID] of the write's operation id. STAMPED BY THE
	// WRITER; an asker sends none.
	ReviewID string `json:"review_id,omitempty"`
	// By is the reviewer. STAMPED BY THE WRITER from the write's asker; an
	// asker sends none.
	By Principal `json:"by,omitzero"`
	// Revision is the change's revision judged, 1 or above, and at most its
	// head as the asker read it.
	Revision int64   `json:"revision"`
	Verdict  Verdict `json:"verdict"`
	// Summary is what the review says, at most [MaxReviewSummaryBytes].
	Summary  string    `json:"summary,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
	// Carries is the ids of the reviewer's own standing findings that still
	// stand as it judges the revision, at most [MaxStandingFindings].
	Carries []string `json:"carries,omitempty"`
	// TurnID and Model are the turn and the model that reviewed; "" for none
	// — a person's review has neither.
	TurnID string `json:"turn_id,omitempty"`
	Model  string `json:"model,omitempty"`
	// Mentions is the handles its summary and its findings name, as
	// [Comment.Mentions] is.
	Mentions []string `json:"mentions,omitempty"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// WRITER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// FindingResolution is one finding's answer.
type FindingResolution struct {
	FindingID string `json:"finding_id"`
	// Resolution is what the answer makes of it: resolved, wont_fix and
	// disputed — an author's — or dismissed.
	Resolution FindingState `json:"resolution"`
	// Note is why, at most [MaxReviewNoteBytes]; "" for none.
	Note string `json:"note,omitempty"`
	// Commit is the commit an author says resolves it, a whole object id,
	// and "" for none.
	Commit string `json:"commit,omitempty"`
}

// FindingResolve answers findings of a change, each at most once.
type FindingResolve struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// By is who answers. STAMPED BY THE WRITER from the write's asker; an
	// asker sends none.
	By       Principal           `json:"by,omitzero"`
	Findings []FindingResolution `json:"findings"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// WRITER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// AskSettle settles a change's asks for review with its LIFECYCLE ([OpAsks]):
// every ask still waiting on a change that closed or merged is ENDED — nobody
// owes the review of a change that will not merge — and every ask its end left
// waiting is ASKED AGAIN once the change is reopened. The change's lifecycle is
// its repository's records, which write nothing of its conversation (a table
// is written by the records of one subject), so its home git node finds a
// change whose asks no longer match its state from the rows (the
// `changes_due` read's [ReadAnswer.AskSettles]) and asks the change's writer
// for this record on the change's own subject. The home's own write, which no
// principal asks.
//
// Every field is in the base format, under the condition [OpAsks] states.
type AskSettle struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// State is the state the change's asks move to, as its home read the
	// change: ended, for a closed or merged change's waiting asks;
	// requested, for a reopened change's ended ones. The decide holds it to
	// the change's state in its own snapshot.
	State ReviewerState `json:"state"`
	// Reviewers is the asks the record moves, by handle, at most
	// [MaxAsksSettled]. STAMPED BY THE WRITER from its snapshot; the home
	// sends none.
	Reviewers []string `json:"reviewers,omitempty"`
}

// MaxAsksSettled is the most asks one [AskSettle] moves: every ask a change can
// hold waiting at once — those asked by name or as path owners
// ([MaxReviewersPerChange]) and those a revision's facts ask to look again
// ([MaxReReviewers]). A change holding more — only a reopened change asked
// again beside the asks its end left, can — is settled over more than one
// record, each its home's next, since the change stays due until none is left.
const MaxAsksSettled = MaxReviewersPerChange + MaxReReviewers

// ReviewID is the id of the review a write under opID gives, DERIVED from the
// write's operation id as [CommentID] derives a comment's, so a review asked
// again is the review that landed.
func ReviewID(opID string) string {
	at, _ := statelog.OpMintedAt(opID)
	return statelog.DeriveOpID(at, "", "code.review", opID)
}

// FindingID is the id of the i-th finding a review under opID raises, derived
// from the review's own: a review's findings named before its record exists,
// the same on every node and every retry.
func FindingID(opID string, i int) string {
	at, _ := statelog.OpMintedAt(opID)
	return statelog.DeriveOpID(at, "", ReviewID(opID), strconv.Itoa(i))
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

// findingProblem is why f is no finding a review may raise, or "", naming the
// field: a clean relative path, a side of a diff, its lines from 1 with the
// last at or past the first, whole object ids for the commit and the blob
// they were read in, a severity and a category this build knows, a title of
// one line up to [MaxFindingTitleBytes], a body up to [MaxFindingBodyBytes]
// and a suggestion up to [MaxSuggestionBytes] — and no id, which the writer
// stamps.
func findingProblem(f Finding) string {
	switch {
	case f.ID != "":
		return "id: the writer derives a finding's id from the review's, and an asker sends none"
	case !utf8.ValidString(f.Path):
		return "path: not valid UTF-8"
	case validGitPath(f.Path, false) != "":
		return "path: " + validGitPath(f.Path, false)
	case !f.Side.Valid():
		return fmt.Sprintf("side: %q is not one of %v", f.Side, Sides())
	case f.LineStart < 1 || f.LineEnd < f.LineStart:
		return fmt.Sprintf("lines %d to %d: a finding's lines count from 1, the last at or past the first",
			f.LineStart, f.LineEnd)
	case !gitref.ValidOID(f.CommitOID):
		return fmt.Sprintf("commit_oid: %q is not a whole object id", f.CommitOID)
	case !gitref.ValidOID(f.BlobOID):
		return fmt.Sprintf("blob_oid: %q is not a whole object id", f.BlobOID)
	case !f.Severity.Valid():
		return fmt.Sprintf("severity: %q is not one of %v", f.Severity, Severities())
	case !f.Category.Valid():
		return fmt.Sprintf("category: %q is not one of %v", f.Category, Categories())
	}
	switch {
	case strings.TrimSpace(f.Title) == "":
		return "title: a finding is named by a title"
	case len(f.Title) > MaxFindingTitleBytes:
		return fmt.Sprintf("title: %d bytes, past the %d a finding's title holds", len(f.Title),
			MaxFindingTitleBytes)
	case !utf8.ValidString(f.Title):
		return "title: not valid UTF-8"
	case strings.ContainsFunc(f.Title, unicode.IsControl):
		return "title: holds a control character: a title is one line"
	}
	if why := textProblem(f.Body, MaxFindingBodyBytes, "finding's body"); why != "" {
		return "body: " + why
	}
	if f.Suggestion != nil {
		if why := textProblem(*f.Suggestion, MaxSuggestionBytes, "suggestion"); why != "" {
			return "suggestion: " + why
		}
	}
	return ""
}

// findingTextBytes is what f's text weighs in its conversation's text_bytes:
// its title, its body and its suggestion — the wide row a finding's text is
// kept on.
func findingTextBytes(f Finding) int64 {
	n := len(f.Title) + len(f.Body)
	if f.Suggestion != nil {
		n += len(*f.Suggestion)
	}
	return int64(n)
}

// handleProblem is why h is no handle a reviewer request may name, or "".
func handleProblem(h string) string {
	if !org.ValidHandle(h) {
		return fmt.Sprintf("%q is not a seat's handle", h)
	}
	return ""
}
