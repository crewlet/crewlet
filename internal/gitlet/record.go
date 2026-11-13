package gitlet

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// RecordVersion is the record shape THIS BUILD can decode — never the version
// it stamps on what it writes.
//
// A record above it is RETAINED rather than skipped (statelog's deferral
// contract), which is what makes a rolling upgrade a period of reduced
// coverage rather than an outage. What a writer stamps is the LOWEST version
// that reads the record, computed by [Encode] from [versionedFields] — the
// tracker's rule and the knowledge base's, one rule across the strict domains.
// It moves only with the table, and exactly to the table's highest version.
//
// # The base format
//
// Version 1 is what the domain landed on main with: every op, subject kind,
// payload, field and value that crewlet#298 (merge commit 22280fcf93,
// 2026-11-03) brought. Every build that carries the domain reads all of it,
// and a doc here that calls a shape BASE FORMAT says so. Comments written
// before that merge, the headers of the replicated migrations among them, rest
// on "the domain's stated condition" instead: none of the domain was on main
// yet, so no build of main, no image and no release carried it. The merge
// ended the condition. What it guaranteed, that no build carrying the domain
// has run without a shape, holds of every shape the merge brought and of
// nothing added since: every push to main ships as the image of main, so a
// build without a later shape may run beside one that has it.
//
// Anything a record gains from now on is a [versionedFields] row: a field, or
// a new value of a closed set it carries (an op, a subject kind, a principal
// kind, a push's way), which statelog.VersionedField.Equals names. An apply
// that comes to derive rows from what a record already carries needs one too:
// the record says so through the field or value that leads to them, or through
// a field its writer stamps for them, and that takes the row. The row comes
// with RecordVersion moved to its version, its line under "What each version
// added", a Carrying record in the statelogtest candidate and, for a new op,
// the op excepted from the stamp test's base-format loop. Without it, an older
// build would apply the record short of what it gained, or fail on it where it
// must retain it: at DecodeMutation's default arm for a new op, at Apply's
// unknown-kind arm for a new kind. A record every build must read takes none:
// a gate ([GateRecordVersion]), the barrier, the trimmed record and a
// reanchor's generation are pinned at version 1, and [Encode] refuses one that
// carries a versioned field.
//
// A field of an estate request or answer is no record's and takes no row: what
// an older build makes of one is the estate wire's to decide (internal/estate,
// wirerule.go).
//
// # What each version added
//
//   - 1: every shape this domain landed on main with.
//   - 2: whom an agent's write was made for (ADR-0109): beside the actor its
//     routings name ([ChangeRouting.WorkingFor]) — and beside none on the
//     merge queue's ejection and a close it could not make, whom the entry's
//     enqueue was made for (concerns.go's head, "Whom the actor acted for") —
//     and on an open, a reopen, a push's entry, a merge and an enqueue
//     whatever they wake ([WorkingForVersion]).
const RecordVersion = 2

// WorkingForVersion is the version from which a record carries whom an
// agent's write was made for — on a routing beside its actor, or beside none
// on the merge queue's ejection and a close it could not make, and on the
// payloads the engine's later records read it back from (an open, a reopen, a
// push's entry, a merge and an enqueue) — which the applier keeps on the rows
// those writes make (code_revisions, code_pushes and code_queue's
// working_for, the replicated migration
// a_code_row_keeps_whom_an_agents_write_was_made_for). A record below it keeps
// none, whatever it holds ([keptCarrier]).
const WorkingForVersion = 2

// baseRecordVersion is version 1, the base format: what every build there has
// ever been reads, and what a record carrying no versioned field is stamped
// at. THE BARRIER, THE TRIMMED RECORD AND A REANCHOR'S GENERATION ARE PINNED AT
// IT FOR EVER, as every gate is ([GateRecordVersion]), because of what an older
// node does with a record it cannot read: it retains it — one more deferral
// row per linearizable read, every anchor a trimmed record names kept, a
// generation transition never made.
const baseRecordVersion = 1

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER: an eviction, a purge and the activation fence. A gate this build could
// not read would be deferred, and a deferred gate leaves this node applying
// every record the gate drops — with no inverse that repairs it. A field such
// a record needs that it cannot have belongs somewhere else.
const GateRecordVersion = 1

// versionedFields is every field a code record has gained since the base
// format, and the version a reader must be at to apply a record carrying it.
// Adding one is adding a row here, moving [RecordVersion] to its version, and
// giving the statelogtest candidate a record that carries it
// ([statelog.VersionedField]).
//
// WHOM A WRITE WAS MADE FOR is version 2, on every op it rides: a routing's
// beside its actor — or beside none on the merge queue's ejection and a close
// it could not make, under the same row — which every payload about a change
// carries at `mutation.routing` (a list of them, a queue's, walked element by
// element) and a push's entries at `mutation.pushes.routing`; and the
// payload's own, an open's, a reopen's, a merge's and an enqueue's at
// `mutation.working_for` and a push entry's at `mutation.pushes.working_for`.
// The routing's row and the payload's own are on EVERY OP rather than the
// ones that carry one today, because every payload's working_for means one
// thing, and a later payload that carries it is stamped by its row too.
var versionedFields = statelog.RecordFields{
	{Name: "ChangeRouting.WorkingFor", Since: WorkingForVersion,
		Path: []string{"mutation", "routing", "working_for"}},
	{Name: "PushEntry.Routing.WorkingFor", Since: WorkingForVersion, Op: string(OpPush),
		Path: []string{"mutation", "pushes", "routing", "working_for"}},
	{Name: "Mutation.WorkingFor", Since: WorkingForVersion, Path: []string{"mutation", "working_for"}},
	{Name: "PushEntry.WorkingFor", Since: WorkingForVersion, Op: string(OpPush),
		Path: []string{"mutation", "pushes", "working_for"}},
}

// VersionedFields is the table, for the conformance suite.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// OpKind is what a record does.
type OpKind string

const (
	// OpCreate makes a repository: its name claim, its row and its
	// protection rules, in one record on the NAME's subject.
	OpCreate OpKind = "create"

	// OpPush applies a batch of pushes to one repository: refs, packs and
	// push rows, in one record on the repository's subject.
	OpPush OpKind = "push"

	// OpSettings replaces a repository's settings — owner, default branch,
	// description, archived, protection rules — whole.
	OpSettings OpKind = "settings"

	// OpDelete marks a repository deleted, restorable until its purge.
	OpDelete OpKind = "delete"

	// OpRestore brings a deleted repository back with the settings it
	// returns with.
	OpRestore OpKind = "restore"

	// OpPurgePart deletes one bounded part of a deleted repository's rows.
	OpPurgePart OpKind = "purge_part"

	// OpPurge removes what remains of a repository and writes its deletion
	// marker — A GATE by its op, on an ordinary repository subject.
	OpPurge OpKind = "purge"

	// OpActivate raises the activation fence to a configuration activation —
	// A GATE by its kind ([KindCompany]).
	OpActivate OpKind = "activate"

	// OpEviction is a node's eviction from this log, or its readmission.
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's record.
	OpGeneration OpKind = "generation"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"

	// OpTrimmed is the trim's payload-free record that removes the anchors
	// below a floor (ADR-0080).
	OpTrimmed OpKind = "trimmed"

	// OpCompact merges some of a repository's packs into fewer: the outputs'
	// rows join code_packs and the inputs' leave it for code_packs_retired,
	// on the repository's own subject ([Compaction]), decided by the
	// repository's committer alone.
	//
	// BASE FORMAT ([RecordVersion]): the domain landed on main with this op,
	// so every build that meets a compact record reads it. It is no gate: it
	// removes no repository and drops no record.
	OpCompact OpKind = "compact"

	// OpRetireSweep removes a repository's retired pack rows at or below a
	// position the code-retention duty's clock put past RetireGrace, on the
	// repository's own subject ([RetireSweep]).
	//
	// BASE FORMAT ([RecordVersion]): the domain landed on main with this op,
	// so every build that meets a retire_sweep record reads it. It is no
	// gate.
	OpRetireSweep OpKind = "retire_sweep"

	// OpExpungeRequest asks for an expunge of a repository's unreachable
	// objects — a person's purge — and records the request and nothing else,
	// on the repository's own subject ([ExpungeRequest]): the repository's
	// maintainer serves it with as many begins and commits as its live packs
	// need.
	//
	// BASE FORMAT ([RecordVersion]): the domain landed on main with this op,
	// so every build that meets an expunge_request record reads it. It is no
	// gate.
	OpExpungeRequest OpKind = "expunge_request"

	// OpExpungeBegin begins an expunge: some of a repository's live packs
	// leave code_packs for code_packs_retired, and the packs that replace
	// them join it — those holding what a ref reaches live, those holding the
	// rest DROPPING — while the repository's drop epoch moves and its open
	// expunge is named, on the repository's own subject ([ExpungeBegin]),
	// decided by the repository's committer alone.
	//
	// BASE FORMAT, as [OpExpungeRequest] is. It is no gate.
	OpExpungeBegin OpKind = "expunge_begin"

	// OpExpungeCommit ends an expunge: the repository's dropping packs leave
	// code_packs for code_packs_retired, and its rescue — what of them a push
	// that landed before the begin made reachable again — joins it live, on
	// the repository's own subject ([ExpungeCommit]), decided by the
	// repository's committer alone.
	//
	// BASE FORMAT, as [OpExpungeRequest] is. It is no gate.
	OpExpungeCommit OpKind = "expunge_commit"

	// OpExpungeScan records that a scan on a repository's maintainer's own
	// schedule found NO expunge due ([ExpungeScan]): the repository leaves
	// the `expunge_due` listing's weekly arm for ExpungeInterval on every
	// node, on the repository's own subject, decided by the repository's
	// committer alone. At most one a repository an ExpungeInterval.
	//
	// BASE FORMAT, as [OpExpungeRequest] is. It is no gate.
	OpExpungeScan OpKind = "expunge_scan"

	// OpImportChunk lands one chunk of a repository's import — packs fetched
	// from the host it is imported from and the refs they let it set — as a
	// push row `via import`, on the repository's own subject
	// ([ImportChunk]), decided by its committer alone, in its turn.
	//
	// BASE FORMAT, as [OpExpungeRequest] is. It is no gate.
	OpImportChunk OpKind = "import_chunk"

	// OpImportDone ends a repository's import, making it active
	// ([ImportDone]), on the repository's own subject, decided by its
	// committer alone.
	//
	// BASE FORMAT, as [OpExpungeRequest] is. It is no gate.
	OpImportDone OpKind = "import_done"

	// OpMirrorSync moves a pull mirror onto its source as its home fetched
	// it, as a push row `via mirror` ([MirrorSync]), on the repository's own
	// subject, decided by its committer alone — or, carrying no pack and no
	// command, records only that its home found the source level with it.
	//
	// BASE FORMAT, as [OpExpungeRequest] is. It is no gate.
	OpMirrorSync OpKind = "mirror_sync"

	// OpRename claims a new name for a tool-managed repository on that name's
	// subject, leaving the old one redirecting to it ([Rename]).
	//
	// BASE FORMAT, as [Rename] is. It is no gate.
	OpRename OpKind = "rename"

	// OpReleaseName frees a renamed repository's old name once its redirect
	// has ended, on that name's subject ([ReleaseName]).
	//
	// BASE FORMAT, as [ReleaseName] is. It is no gate.
	OpReleaseName OpKind = "release_name"

	// OpHold is a live turn's hold on a scratch repository, or on one seat's
	// branch of any repository, on the repository's own subject, ordered
	// against a deletion of what it holds ([Hold]).
	//
	// BASE FORMAT, as [Hold] is. It is no gate.
	OpHold OpKind = "hold"

	// OpChangeOpen opens a change (web!42) on a repository: its row, its
	// owner as its first author and its first revision, the source branch's
	// head — numbered by the apply from the repository's next_change, which
	// the same apply advances — on the repository's own subject
	// ([ChangeOpen]), decided by the repository's committer alone
	// ([Committer.ProposeChange]).
	//
	// BASE FORMAT ([RecordVersion]): the domain landed on main with this op,
	// so every build that meets a change_open record reads it. It is no gate.
	OpChangeOpen OpKind = "change_open"

	// OpChangeEdit sets a live change's title, its body or the work item it
	// carries ([ChangeEdit]), on the repository's own subject, decided by its
	// committer alone.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpChangeEdit OpKind = "change_edit"

	// OpChangeDraft makes a live change a draft, or makes a draft ask to be
	// merged ([ChangeDraft]), on the repository's own subject, decided by
	// its committer alone.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpChangeDraft OpKind = "change_draft"

	// OpChangeRetarget moves a live change's target to another live branch
	// ([ChangeRetarget]), on the repository's own subject, decided by its
	// committer alone.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpChangeRetarget OpKind = "change_retarget"

	// OpChangeClose closes a live change without merging it ([ChangeClose]),
	// on the repository's own subject, decided by its committer alone.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpChangeClose OpKind = "change_close"

	// OpChangeReopen opens a closed change again ([ChangeReopen]) — with a
	// revision of its own where its source branch moved while it was closed
	// — on the repository's own subject, decided by its committer alone.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpChangeReopen OpKind = "change_reopen"

	// OpChangeLanded records a live change MERGED because its target came to
	// hold its head — moved past it, by a push that did not set it to exactly
	// that head — as the repository's home Gitlet node found it ([ChangeLanded]),
	// on the repository's own subject, decided by its committer alone against
	// the change's head and the target row the home read.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpChangeLanded OpKind = "change_landed"

	// OpReviewers asks reviewers of a live change, or withdraws the ask
	// ([ReviewerRequest]), on the CHANGE's own subject ([KindChange]),
	// written by the repository's committing data node ([ChangeWriter]).
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpReviewers OpKind = "reviewers"

	// OpComment says something on a change — a thread of its own, anchored
	// to a line of one of its revisions or not, or a reply to a thread
	// ([Comment]) — on the change's own subject, written by the repository's
	// committing data node.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpComment OpKind = "comment"

	// OpThreadResolve marks a thread of a change's comments resolved, or
	// open again ([ThreadResolve]), on the change's own subject, written by
	// the repository's committing data node.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpThreadResolve OpKind = "thread_resolve"

	// OpReview is a review of one revision of a change — a verdict, a
	// summary and the findings it raises, and the reviewer's own standing
	// findings it carries ([Review]) — on the change's own subject, written
	// by the repository's committing data node, ONE RECORD with all its
	// findings; a review of a later revision than a finding of its
	// reviewer's supersedes that finding unless it carries it.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpReview OpKind = "review"

	// OpFindingResolve answers findings of a change — resolved, won't fix
	// and disputed by an author, dismissed by its raiser, a maintainer or a
	// person ([FindingResolve]) — on the change's own subject, written by
	// the repository's committing data node.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpFindingResolve OpKind = "finding_resolve"

	// OpRevise records one revision's facts on its change — what its head is
	// against its target's tip, as the repository's home Gitlet node read them,
	// and the path owners whose globs its touched paths match, with the
	// reviews that asks ([Revise]) — on the change's own subject, written by
	// the repository's committing data node for the home, which asks it and
	// publishes nothing itself.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpRevise OpKind = "revise"

	// OpMergeability records whether a change's head revision merges cleanly
	// into its target's tip — clean, in conflict naming the paths, or
	// undecided within its home's deadline — as the repository's home Gitlet
	// node merged the two commits in a scratch of its own ([Mergeability]),
	// on the change's own subject, written by the repository's committing
	// data node for the home, which asks it and publishes nothing itself.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpMergeability OpKind = "mergeability"

	// OpMergeabilityKept records a re-evaluation of a change's mergeability
	// that found the outcome its row holds — a recheck of a moved target, a
	// refresh of its readiness — by what it moved alone: the tip a recheck
	// merged into, and the versions and the checks position a readiness was
	// judged at ([MergeabilityKept]). Never asked for: the change writer
	// forms it in place of the [OpMergeability] its home sent, on the
	// change's own subject.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpMergeabilityKept OpKind = "mergeability_kept"

	// OpAsks settles a change's asks for review with its lifecycle — ending
	// the asks still waiting on a change that closed or merged, and asking
	// again those its end left on one reopened ([AskSettle]) — on the
	// change's own subject, written by the repository's committing data node
	// for the home, which asks it and publishes nothing itself.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpAsks OpKind = "asks"

	// OpCheckQueued queues one attempt of one check of one commit, or records
	// it skipped ([CheckQueued]), on the repository's CHECKS subject
	// ([KindChecks]), its apply a join (checks.go).
	//
	// BASE FORMAT, as the checks subject is (checks.go). It is no gate.
	OpCheckQueued OpKind = "check_queued"

	// OpCheckStarted claims an attempt for a runner, or names the box its
	// holder made ([CheckStarted]), on the repository's checks subject.
	//
	// BASE FORMAT, as [OpCheckQueued] is. It is no gate.
	OpCheckStarted OpKind = "check_started"

	// OpCheckFinished ends an attempt ([CheckFinished]), on the repository's
	// checks subject, and removes the repository's rows past [CheckHistory].
	//
	// BASE FORMAT, as [OpCheckQueued] is. It is no gate.
	OpCheckFinished OpKind = "check_finished"

	// OpChecksSweep removes a repository's check rows past [CheckHistory]
	// where no check of its own ends to ([ChecksSweep]), on its checks
	// subject — the code-retention duty's.
	//
	// BASE FORMAT, as [OpCheckQueued] is. It is no gate.
	OpChecksSweep OpKind = "checks_sweep"

	// OpCheckDispatched records a repository's home's reading of a target's
	// checks file at its tip and the change heads it dispatched under it,
	// with the attempts that dispatch queues at them ([CheckDispatched]), on
	// the repository's checks subject: what the home's due listing converges
	// on, and code_checks' rows the attempts make, each as a check_queued
	// record's would (checksdispatch.go).
	//
	// BASE FORMAT, as [OpCheckQueued] is. It is no gate.
	OpCheckDispatched OpKind = "check_dispatched"

	// OpMerge merges a change into its target ([Merge]), on the repository's
	// own subject, decided by its committer alone through the change's merge
	// gate from one snapshot ([Committer.ProposeMerge], ADR-0096): ONE
	// RECORD merges the change, moves the target onto the merge's commit and
	// lands that move as a push.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpMerge OpKind = "merge"

	// OpEnqueue puts a change in its target's merge queue ([Enqueue]), on the
	// repository's own subject, decided by its committer alone through the
	// change's merge gate ([Committer.ProposeQueue]).
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpEnqueue OpKind = "enqueue"

	// OpDequeue takes a live entry out of its target's merge queue at a
	// principal's ask ([Dequeue]), on the repository's own subject, decided by
	// its committer alone.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpDequeue OpKind = "dequeue"

	// OpMarks moves one person's marks over their code notices by one
	// gesture ([PersonMarks], marks.go) — read, unread, snoozed, read
	// through a position on this log, their primary reasons — on the
	// person's own subject ([KindPerson]), written by whichever data node
	// the mark is asked of ([Marks]).
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpMarks OpKind = "marks"

	// OpCandidate records a queue's TRAIN ([Candidate]): the
	// candidates its home built of a target's live entries onto the target's
	// tip, each a merge of a batch of them on the one before, and the pack
	// their objects are stored in — on the repository's own subject, decided
	// by its committer alone ([Committer.ProposeQueue]).
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpCandidate OpKind = "candidate"

	// OpEject puts live entries out of their queue ([Eject]) — a check failed
	// on what they were tested in, their merge conflicted, their gate no
	// longer holds — on the repository's own subject, decided by its
	// committer alone.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpEject OpKind = "eject"

	// OpLand lands a queue's entries on their target ([Land]) — a train's
	// tested prefix — merging each change and moving the target in ONE
	// record whose decide proves every entry's gate again with its enqueuer
	// as the merger, on the repository's own subject, decided by its
	// committer alone.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpLand OpKind = "land"

	// OpGreen moves a protected target's green row onto a landing whose
	// required checks passed ([Green]), on the repository's own subject.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpGreen OpKind = "green"

	// OpBroken marks a landing above its target's green row broken, a
	// required check of it having failed ([Broken]), on the repository's own
	// subject.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpBroken OpKind = "broken"

	// OpQueueSweep removes a repository's ended queue entries and its
	// landings below their target's green row once they outlive
	// [QueueHistory], where no record of its queues' own does ([QueueSweep])
	// — the code-retention duty's, on the repository's own subject.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpQueueSweep OpKind = "queue_sweep"

	// OpCloseSettled settles the closes landed queue entries owe their
	// changes' work items, each closed or definitively not, every close a
	// turn of their home ended in one record ([CloseSettled]) — made as each
	// entry's enqueuer, recorded on the repository's own subject.
	//
	// BASE FORMAT, as [OpChangeOpen] is. It is no gate.
	OpCloseSettled OpKind = "close_settled"
)

// OpKinds are the fifty-six, in the order they are documented.
var OpKinds = []OpKind{
	OpCreate, OpPush, OpSettings, OpDelete, OpRestore, OpPurgePart, OpPurge,
	OpActivate, OpEviction, OpGeneration, OpBarrier, OpTrimmed, OpCompact, OpRetireSweep,
	OpExpungeRequest, OpExpungeBegin, OpExpungeCommit, OpExpungeScan, OpImportChunk, OpImportDone, OpMirrorSync,
	OpRename,
	OpReleaseName, OpHold,
	OpChangeOpen, OpChangeEdit, OpChangeDraft, OpChangeRetarget, OpChangeClose, OpChangeReopen, OpChangeLanded,
	OpReviewers, OpComment, OpThreadResolve, OpReview, OpFindingResolve, OpRevise, OpMergeability,
	OpMergeabilityKept, OpAsks,
	OpCheckQueued, OpCheckStarted, OpCheckFinished, OpChecksSweep, OpCheckDispatched, OpMerge, OpEnqueue, OpDequeue,
	OpMarks, OpCandidate, OpEject, OpLand, OpGreen, OpBroken, OpQueueSweep, OpCloseSettled,
}

// CheckOps are the ops of a repository's CHECKS, in [OpKinds]' order: every
// record on its checks subject ([KindChecks]).
func CheckOps() []OpKind {
	return []OpKind{OpCheckQueued, OpCheckStarted, OpCheckFinished, OpChecksSweep, OpCheckDispatched}
}

// ChangeOps are the seven ops of a change's LIFECYCLE, in [OpKinds]' order:
// what a [ChangeProposal] may be, each decided by the repository's committer
// as a record of its own — a landing, which the repository's home Gitlet node
// asks, among them.
func ChangeOps() []OpKind {
	return []OpKind{OpChangeOpen, OpChangeEdit, OpChangeDraft, OpChangeRetarget, OpChangeClose, OpChangeReopen,
		OpChangeLanded}
}

// ConversationOps are the ops of a change's CONVERSATION, in [OpKinds]' order:
// what a [ConversationWrite] may be, each a record of its own on the change's
// subject ([KindChange]), written by the repository's committing data node
// ([ChangeWriter]) — a revision's facts and a change's mergeability, which the
// repository's home Gitlet node asks, among them, the record the writer keeps
// such a mergeability by where its row holds the outcome already, and its
// settling of the change's asks with the change's lifecycle.
func ConversationOps() []OpKind {
	return []OpKind{OpReviewers, OpComment, OpThreadResolve, OpReview, OpFindingResolve, OpRevise, OpMergeability,
		OpMergeabilityKept, OpAsks}
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// ActorKind is who decided a record — never who made the pushes a batch
// carries, which each entry's own principal says.
type ActorKind string

const (
	// ActorOperator is a person or a credential acting on the company's
	// behalf — an eviction or a readmission they ran, a reanchor of this log,
	// a person's lifecycle request through a tool — recorded under the name
	// iam.ActorFor gives them (a person's human seat, an unbound credential's
	// login) with the credential they acted through beside it, as every other
	// log's records name them.
	ActorOperator ActorKind = "operator"

	// ActorConfig is the reconcile of code.repos at a configuration
	// activation: a create, a settings change, a delete, a restore and the
	// activation fence itself.
	ActorConfig ActorKind = "config"

	// ActorSystem is the engine on its own account: the committer's push
	// batches, compactions, expunge records and change records — a person's
	// purge request and every write of a change's lifecycle among them, whose
	// principal its payload names — the change writer's records of a change's
	// conversation, whose principal its payload names too, the code-retention
	// pass's sweeps and purges, and a reanchor nobody named, recorded under the
	// node that ran it.
	ActorSystem ActorKind = "system"

	// ActorAgent is a seat of the chart, by its handle: a repository's
	// lifecycle request a seat made through its tools — a create, a rename, a
	// settings change, an archive or unarchive, a delete, a restore, a
	// protection, a live turn's hold ([Store.Lifecycle]) — and a reanchor run
	// through a seat's credential. Recorded as the repository's creator where
	// it creates one (code_repos.created_by_kind).
	//
	// BASE FORMAT ([RecordVersion]): the domain landed on main with this
	// kind, so every build that meets a record a seat decided reads it.
	ActorAgent ActorKind = "agent"
)

// ActorKinds is the closed set, in this order.
func ActorKinds() []ActorKind {
	return []ActorKind{ActorOperator, ActorConfig, ActorSystem, ActorAgent}
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ActorKind) Valid() bool { return slices.Contains(ActorKinds(), k) }

// Actor is who decided a record.
type Actor struct {
	// ID is who decided it, BARE: an operator's or a seat's name as
	// iam.ActorFor gives it — a person's human seat, an unbound credential's
	// login, a seat's handle — or the deciding node's id. NO PREFIX: the
	// kind is a column of its own, and a name spelled `operator:<name>` is
	// one no other row in the audit feed calls that party by.
	ID   string
	Kind ActorKind
	// OperatorID is the credential the record was made through: REQUIRED of
	// an operator's record this domain decides, which [Actor.validate] holds.
	// A generation record is the reanchor's, not a decision of this domain's,
	// and carries exactly the credential the reanchor's facts name, beside
	// any kind ([generationActor]).
	OperatorID string
}

// validate refuses an actor no record may name.
func (a Actor) validate() error {
	switch {
	case !a.Kind.Valid():
		return fmt.Errorf("%w: actor.kind: %q is not one of %s", ErrInvalid, a.Kind, words.Or(ActorKinds()))
	case strings.TrimSpace(a.ID) == "":
		return fmt.Errorf("%w: actor.id: a record names who decided it", ErrInvalid)
	case a.Kind == ActorOperator && strings.TrimSpace(a.OperatorID) == "":
		return fmt.Errorf("%w: actor.operator_id: an operator's record names the "+
			"credential it was made with", ErrInvalid)
	}
	return nil
}

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted.
//
// Its EIGHT keys are RESERVED at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one — the
// split the event envelope makes between a known header and an opaque body,
// for the same reason: a rolling upgrade puts a record this build cannot read
// on the wire, and dropping it would make every upgrade an outage.
type RecordEnvelope struct {
	// V is the record version, refused for the PAYLOAD when unknown and
	// never for this struct.
	V int `json:"v"`

	// OpID is the idempotency key, the message id and the ops table's key.
	// EMPTY on a barrier and on a trimmed record: an op id becomes a message
	// id, and a duplicate ack is served with no quorum round trip at all.
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record arbitrates on.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is the AUTHORED instant, the writer's own clock: provenance
	// for an operator reading the log, NEVER ORDERED ON and never written to
	// a row — every instant this domain stores is the broker's.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation the record was decided in, which makes a
	// position a pure function of the record.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node's id, read by the eviction gate before
	// any kind rule, on a node that may not decode the payload at all.
	Writer string `json:"writer,omitempty"`

	// Scope is every object this record's apply may write and every object
	// its decide read (scope.go).
	Scope Scope `json:"scope"`
}

// InstallsGate reports whether this record installs an apply gate, answered
// from the envelope alone because a node that cannot decode the payload has
// to: an eviction and the activation fence by their KIND, a purge by its OP.
// Reading only the kind would let a purge this build cannot decode be deferred
// — and a deferred purge is a node serving a repository every peer removed.
func (e RecordEnvelope) InstallsGate() bool {
	return e.Subject.Kind.InstallsGate() || e.Op == OpPurge
}

// readByEveryBuild reports a record every build there will ever be must read:
// a gate, the barrier, the trimmed record and a reanchor's generation — each
// pinned at the base version, so none may carry a versioned field ([Encode]
// refuses one that does).
func (e RecordEnvelope) readByEveryBuild() bool {
	return e.InstallsGate() || e.Op == OpBarrier || e.Op == OpTrimmed || e.Op == OpGeneration
}

// MutationRecord is one committed mutation: the envelope plus everything a
// build at this version may read.
type MutationRecord struct {
	RecordEnvelope

	// Expect is the version this mutation was decided against. PROVENANCE
	// ONLY — the broker enforced it.
	Expect uint64 `json:"expect,omitempty"`

	// Mutation is the typed payload for (Subject.Kind, Op), left as opaque
	// bytes when the version is above this build's.
	Mutation json.RawMessage `json:"mutation,omitempty"`

	// Actor, ActorKind and OperatorID are who decided this record.
	Actor      string    `json:"actor,omitempty"`
	ActorKind  ActorKind `json:"actor_kind,omitempty"`
	OperatorID string    `json:"operator_id,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// What is NOT in the envelope, deliberately: the BROKER'S instant. It reaches
// the applier as an argument ([statelog.ApplyOptions.StoredAt]) rather than as
// a key in the format, because a field a writer could set is a field a writer
// could lie about. Stated here so nobody "completes" the envelope by adding it.

// ErrFutureVersion reports a record a newer build wrote. It carries the
// SUBJECT because the caller is the applier's deferral arm, which has to file
// the record under something.
type ErrFutureVersion struct {
	Got     int
	Want    int
	Subject Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("gitlet: the record on %s is version %d and this build "+
		"reads %d — it is RETAINED at its position rather than skipped, and "+
		"reprocessed by a build that knows the shape", e.Subject, e.Got, e.Want)
}

// DecodeEnvelope is the FIRST pass, and it never fails on version.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("gitlet: decode the record envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("gitlet: a record carries version %d — "+
			"every record states its version, and one that does not cannot be "+
			"told apart from a newer build's", env.V)
	}
	if err := env.Subject.Validate(); err != nil {
		return RecordEnvelope{}, err
	}
	if len(env.Scope) == 0 {
		// A RECORD WITH NO SCOPE KEY AT ALL never reaches the scope's own
		// decoder, so the widening it makes is made here: absent is the
		// widest claim, never "touches nothing".
		env.Scope = Scope{DomainPath()}
	}
	return env, nil
}

// Decode is the SECOND pass, and it is the one that may refuse on version. It
// returns the envelope alongside the error whenever the envelope decoded,
// because that is the case the applier retains.
func Decode(payload []byte) (MutationRecord, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return MutationRecord{}, err
	}
	if env.V > RecordVersion {
		return MutationRecord{RecordEnvelope: env}, &ErrFutureVersion{
			Got: env.V, Want: RecordVersion, Subject: env.Subject,
		}
	}
	var rec MutationRecord
	extra, err := decodeInto(payload, &rec, recordFields)
	if err != nil {
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("gitlet: decode "+
			"the record on %s: %w", env.Subject, err)
	}
	rec.Scope = env.Scope
	rec.Extra = extra
	return rec, nil
}

// Encode renders a record, carrying back whatever a newer build wrote, and
// stamps it with the lowest version that reads it.
//
// LOSSLESS IN BOTH DIRECTIONS: a node that read a record it only half
// understood and republished it must not strip the half it did not. A carried
// field LOSES to a known one ([encode]). A ZERO VERSION IS "STAMP IT": every
// writer leaves V unset; a set version is kept and refused when it is below
// what the record carries; and a record every build must read is refused
// whenever it carries a versioned field at all.
func Encode(rec MutationRecord) ([]byte, error) { return encodeWith(rec, versionedFields) }

// encodeWith is [Encode] under a named field table — the seam the stamping
// rule is tested through.
func encodeWith(rec MutationRecord, fields statelog.RecordFields) ([]byte, error) {
	stamp := rec.V == 0
	if stamp {
		rec.V = baseRecordVersion
	}
	data, err := encode(rec, rec.Extra)
	if err != nil {
		return nil, fmt.Errorf("gitlet: encode the record on %s: %w", rec.Subject, err)
	}
	minimum, err := fields.Minimum(string(rec.Op), data)
	if err != nil {
		return nil, fmt.Errorf("gitlet: stamp the record on %s: %w", rec.Subject, err)
	}
	switch {
	case rec.readByEveryBuild() && minimum > baseRecordVersion:
		return nil, fmt.Errorf("gitlet: the %s record on %s must be readable by "+
			"every build for ever — an older node defers a gate, and retains a "+
			"barrier, a trimmed record or a generation, that it cannot read — and "+
			"it carries %s, so a field it needs belongs somewhere else", rec.Op,
			rec.Subject, strings.Join(fields.Carried(string(rec.Op), data), ", "))
	case rec.V >= minimum:
	case stamp:
		rec.V = minimum
		if data, err = encode(rec, rec.Extra); err != nil {
			return nil, fmt.Errorf("gitlet: encode the record on %s: %w", rec.Subject, err)
		}
	default:
		return nil, fmt.Errorf("gitlet: the %s record on %s is stamped version %d "+
			"and carries %s — a build reading %d would decode it, drop what it has "+
			"no field for and apply the rest; leave the version unset and the "+
			"encoder stamps the lowest one that reads it", rec.Op, rec.Subject,
			rec.V, strings.Join(fields.Carried(string(rec.Op), data), ", "), rec.V)
	}
	return data, nil
}

// recordFields is every top-level name this build writes. The omitempty names
// are listed because a zero value does not marshal them, and a name missing
// here would be decoded into the struct AND carried as unknown — so the next
// encode would write the stale carried copy back over what the caller set.
var recordFields = fieldSet(MutationRecord{}, "op_id", "created_at", "gen",
	"writer", "expect", "mutation", "actor", "actor_kind", "operator_id")

// recordFor encodes one record decided inside a snapshot — the ONE place a
// writer of this domain stamps a record, so the framework's stamp (Writer,
// Gen), the actor and the scope check are applied once. The publisher refuses
// a record without the stamp; this refuses a scope a writer cannot mean.
// payload == nil writes no mutation.
func recordFor(stamp statelog.Stamp, subject Subject, op OpKind, scope []string,
	opID string, actor Actor, payload any, at time.Time) (statelog.Decision, error) {

	if err := validScope(scope); err != nil {
		return statelog.Decision{}, err
	}
	if err := actor.validate(); err != nil {
		return statelog.Decision{}, err
	}
	record := MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// NO VERSION: the encoder stamps the lowest one that reads
			// what this record carries.
			OpID: opID, Subject: subject, Op: op, CreatedAt: at,
			Gen: stamp.Gen, Writer: stamp.Writer, Scope: Scope(scope),
		},
		Actor: actor.ID, ActorKind: actor.Kind, OperatorID: actor.OperatorID,
	}
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return statelog.Decision{}, fmt.Errorf("gitlet: encode the %s payload "+
				"for %s: %w", op, subject, err)
		}
		record.Mutation = body
	}
	encoded, err := Encode(record)
	if err != nil {
		return statelog.Decision{}, err
	}
	return statelog.Decision{Payload: encoded}, nil
}

// EncodeBarrier renders the read index's payload-free append — the framework
// owns WHEN a barrier goes out and what its acknowledgement proves, and this
// domain owns what a record on its log looks like.
//
// AN OP ID IS REFUSED: it becomes a message id, and a repeat inside the
// duplicate window is answered from the dedupe cache with no quorum round trip
// — a position nothing confirmed, which is the claim a barrier must never fake.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("gitlet: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, errors.New("gitlet: a barrier carries no op id and this one " +
			"has one — an op id becomes a message id, and a duplicate ack is " +
			"served with no quorum round trip at all")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// PINNED: see [baseRecordVersion].
			V: baseRecordVersion, Subject: BarrierSubject(), Op: OpBarrier,
			Scope: Scope{statelog.FrameworkScope}, Gen: env.Gen,
		},
	})
}

// EncodeTrimmed renders the trim's trimmed record ([statelog.TrimmedEnvelope],
// ADR-0080) as a record this domain's applier decodes — the barrier's split:
// the framework decides when one is appended, what it removes and the floor
// its subject names, and this domain what a record on its log looks like.
//
// THE WRITER TRAVELS, unlike a barrier's: a trimmed record removes rows, so the
// eviction gate must be able to drop an evicted node's. NO OP ID, asserted. And
// the floor is checked here, at the write, rather than in [Subject.Validate],
// which the read path runs too.
func EncodeTrimmed(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.TrimmedKind || env.Subject.Kind != statelog.TrimmedKind {
		return nil, fmt.Errorf("gitlet: a %q envelope on %q is not a trimmed record",
			env.Kind, env.Subject)
	}
	if env.OpID != "" {
		return nil, errors.New("gitlet: a trimmed record carries no op id and this " +
			"one has one — an op id becomes a message id and a ledger row nothing " +
			"resolves")
	}
	floor, err := statelog.TrimmedFloor(Domain{}.Stream().Name, env.Subject)
	if err != nil {
		return nil, fmt.Errorf("gitlet: %w", err)
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// PINNED, for the barrier's reason: see [baseRecordVersion].
			V: baseRecordVersion, Subject: TrimmedSubject(floor), Op: OpTrimmed,
			Scope: Scope{statelog.FrameworkScope}, Gen: env.Gen, Writer: env.Writer,
		},
	})
}
