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

// RecordVersion is the record shape THIS BUILD can decode, never the version it
// stamps: [Encode] stamps the LOWEST version that reads a record, from
// [versionedFields], and RecordVersion is always that table's highest. A record
// above it is RETAINED, not skipped (statelog's deferral contract), so a rolling
// upgrade reduces coverage rather than causing an outage.
//
// Version 1, the BASE FORMAT, is every shape crewlet#298 landed on main with,
// read by every build that carries the domain; a migration citing "the
// domain's stated condition" predates that merge, which ended it. Since then
// every push to main ships as main's image, so a later shape may meet a build
// without it: what a record gains is a [versionedFields] row, and each version
// past 1 is a constant below naming what it added.
const RecordVersion = CommentEditsVersion

// WorkingForVersion is the version from which a record carries whom an agent's
// write was made for (ADR-0109; concerns.go, "Whom the actor acted for"): on a
// routing beside its actor, beside none on the merge queue's ejection and a
// close it could not make, and on an open, a reopen, a push's entry, a merge and
// an enqueue. The applier keeps it as working_for on code_revisions,
// code_pushes and code_queue; a record below it keeps none, whatever it holds
// ([keptCarrier]).
const WorkingForVersion = 2

// OwnedPathsVersion is the version from which a revise carries when its home measured the facts
// and the globs each matched path owner and each ask of one is for (code_revision_facts.measured_at,
// code_reviewers.owned_paths); a record below it keeps neither.
const OwnedPathsVersion = 3

// ViewedVersion is the version from which a code record may be a person's viewed marks ([OpViewed])
// or the key a repository's are digested under ([OpViewedKey]).
const ViewedVersion = 4

// OfferedVersion is the version from which a push's attempt carries how many commits it offered
// (code_pushes.offered); a record below it keeps no count.
const OfferedVersion = 5

// CheckCountsVersion is the version from which a check's ending carries its test counts and its
// output's lines (code_checks.tests_*, log_lines, tail_cut); an ending below it keeps none.
const CheckCountsVersion = 6

// TestedInVersion is the version from which a failed check's ejection names the train it failed on
// (code_queue.ejected's tested_in); an ejection below it names none.
const TestedInVersion = 7

// GateChecksVersion is the version from which a person's CI's report says whether the target's gate
// requires its check (code_checks.required of a row no queue made), and an ejection may name a
// required check the train's base defines in no form that runs ([EjectChecksUndefined]).
const GateChecksVersion = 8

// RevertsVersion is the version from which a push opening a change names the change it reverts
// (code_changes.reverts), as a revert_change's push does; a record below it names none.
const RevertsVersion = 9

// CommentEditsVersion is the version from which a change's record may edit or delete one of its comments
// ([OpCommentEdit], [OpCommentDelete]).
const CommentEditsVersion = 10

// baseRecordVersion is version 1, the base format: what every build reads, and
// what a record carrying no versioned field is stamped at. The barrier, the
// trimmed record and a reanchor's generation are PINNED at it for ever, as every
// gate is ([GateRecordVersion]), because an older node retains a record it
// cannot read: a deferral row per linearizable read, every anchor a trimmed
// record names kept, a generation transition never made.
const baseRecordVersion = 1

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER: an eviction, a purge and the activation fence. A gate this build could
// not read would be deferred, and a deferred gate leaves this node applying
// every record the gate drops — with no inverse that repairs it. A field such
// a record needs that it cannot have belongs somewhere else.
const GateRecordVersion = 1

// versionedFields is every field a code record has gained since the base
// format, and the version a reader must be at to apply a record carrying it. A
// new value of a closed set (an op, a subject kind, a principal kind, a push's
// way; statelog.VersionedField.Equals) is one too, as is whatever leads an apply
// to derive new rows. Without its row an older build applies the record short,
// or fails where it must retain it (DecodeMutation's default arm, Apply's
// unknown-kind arm). A row comes with [RecordVersion] moved to it, its version
// constant, a Carrying record in the statelogtest candidate and, for a new op,
// the op excepted from the stamp test's base-format loop. No row for a record
// every build reads ([RecordEnvelope.readByEveryBuild]), an estate wire field
// (internal/estate, wirerule.go), or rows re-derived from rows every data node
// holds alike ([DerivationVersion]), kept out of the rows a horizon is given.
var versionedFields = statelog.RecordFields{
	// The routing's and the payload's own working_for name no op: it means one
	// thing on every payload, so a later payload carrying it is stamped too.
	{Name: "ChangeRouting.WorkingFor", Since: WorkingForVersion,
		Path: []string{"mutation", "routing", "working_for"}},
	{Name: "PushEntry.Routing.WorkingFor", Since: WorkingForVersion, Op: string(OpPush),
		Path: []string{"mutation", "pushes", "routing", "working_for"}},
	{Name: "Mutation.WorkingFor", Since: WorkingForVersion, Path: []string{"mutation", "working_for"}},
	{Name: "PushEntry.WorkingFor", Since: WorkingForVersion, Op: string(OpPush),
		Path: []string{"mutation", "pushes", "working_for"}},
	{Name: "Revise.MeasuredAt", Since: OwnedPathsVersion, Op: string(OpRevise),
		Path: []string{"mutation", "measured_at"}},
	{Name: "MatchedOwner.Paths", Since: OwnedPathsVersion, Op: string(OpRevise),
		Path: []string{"mutation", "requested_owners", "paths"}},
	{Name: "OwnerAsk.Paths", Since: OwnedPathsVersion, Op: string(OpRevise),
		Path: []string{"mutation", "asked", "paths"}},
	{Name: "OwnerAsk.PathsOmitted", Since: OwnedPathsVersion, Op: string(OpRevise),
		Path: []string{"mutation", "asked", "paths_omitted"}},
	{Name: "RecordEnvelope.Op viewed", Since: ViewedVersion, Path: []string{"op"}, Equals: string(OpViewed)},
	{Name: "RecordEnvelope.Op viewed_key", Since: ViewedVersion, Path: []string{"op"}, Equals: string(OpViewedKey)},
	{Name: "PushEntry.Offered", Since: OfferedVersion, Op: string(OpPush),
		Path: []string{"mutation", "pushes", "offered"}},
	{Name: "CheckFinished.Tests", Since: CheckCountsVersion, Op: string(OpCheckFinished),
		Path: []string{"mutation", "tests"}},
	{Name: "CheckFinished.LogLines", Since: CheckCountsVersion, Op: string(OpCheckFinished),
		Path: []string{"mutation", "log_lines"}},
	{Name: "CheckFinished.TailCut", Since: CheckCountsVersion, Op: string(OpCheckFinished),
		Path: []string{"mutation", "tail_cut"}},
	{Name: "Ejection.TestedIn", Since: TestedInVersion, Op: string(OpEject),
		Path: []string{"mutation", "entries", "tested_in"}},
	{Name: "CheckStarted.Required", Since: GateChecksVersion, Op: string(OpCheckStarted),
		Path: []string{"mutation", "required"}},
	{Name: "CheckFinished.Required", Since: GateChecksVersion, Op: string(OpCheckFinished),
		Path: []string{"mutation", "required"}},
	{Name: "Ejection.Reason checks_undefined", Since: GateChecksVersion, Op: string(OpEject),
		Path: []string{"mutation", "entries", "reason"}, Equals: string(EjectChecksUndefined)},
	{Name: "ChangeOption.Reverts", Since: RevertsVersion, Op: string(OpPush),
		Path: []string{"mutation", "pushes", "change", "reverts"}},
	{Name: "RecordEnvelope.Op comment_edit", Since: CommentEditsVersion, Path: []string{"op"},
		Equals: string(OpCommentEdit)},
	{Name: "RecordEnvelope.Op comment_delete", Since: CommentEditsVersion, Path: []string{"op"},
		Equals: string(OpCommentDelete)},
}

// VersionedFields is the table, for the conformance suite.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// OpKind is what a record does.
type OpKind string

// Every op is BASE FORMAT ([RecordVersion]) but [OpViewed] and [OpViewedKey]
// ([ViewedVersion]) and [OpCommentEdit] and [OpCommentDelete]
// ([CommentEditsVersion]). None installs a gate but [OpPurge], and an eviction
// and the activation fence by their kind ([RecordEnvelope.InstallsGate]).
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
	// repository's committer alone. It removes no repository and drops no record.
	OpCompact OpKind = "compact"

	// OpRetireSweep removes a repository's retired pack rows at or below a
	// position the code-retention duty's clock put past RetireGrace, on the
	// repository's own subject ([RetireSweep]).
	OpRetireSweep OpKind = "retire_sweep"

	// OpExpungeRequest asks for an expunge of a repository's unreachable
	// objects — a person's purge — and records the request and nothing else,
	// on the repository's own subject ([ExpungeRequest]): the repository's
	// maintainer serves it with as many begins and commits as its live packs
	// need.
	OpExpungeRequest OpKind = "expunge_request"

	// OpExpungeBegin begins an expunge: some of a repository's live packs
	// leave code_packs for code_packs_retired, and the packs that replace
	// them join it — those holding what a ref reaches live, those holding the
	// rest DROPPING — while the repository's drop epoch moves and its open
	// expunge is named, on the repository's own subject ([ExpungeBegin]),
	// decided by the repository's committer alone.
	OpExpungeBegin OpKind = "expunge_begin"

	// OpExpungeCommit ends an expunge: the repository's dropping packs leave
	// code_packs for code_packs_retired, and its rescue — what of them a push
	// that landed before the begin made reachable again — joins it live, on
	// the repository's own subject ([ExpungeCommit]), decided by the
	// repository's committer alone.
	OpExpungeCommit OpKind = "expunge_commit"

	// OpExpungeScan records that a scan on a repository's maintainer's own
	// schedule found NO expunge due ([ExpungeScan]): the repository leaves
	// the `expunge_due` listing's weekly arm for ExpungeInterval on every
	// node, on the repository's own subject, decided by the repository's
	// committer alone. At most one a repository an ExpungeInterval.
	OpExpungeScan OpKind = "expunge_scan"

	// OpImportChunk lands one chunk of a repository's import — packs fetched
	// from the host it is imported from and the refs they let it set — as a
	// push row `via import`, on the repository's own subject
	// ([ImportChunk]), decided by its committer alone, in its turn.
	OpImportChunk OpKind = "import_chunk"

	// OpImportDone ends a repository's import, making it active
	// ([ImportDone]), on the repository's own subject, decided by its
	// committer alone.
	OpImportDone OpKind = "import_done"

	// OpMirrorSync moves a pull mirror onto its source as its home fetched
	// it, as a push row `via mirror` ([MirrorSync]), on the repository's own
	// subject, decided by its committer alone — or, carrying no pack and no
	// command, records only that its home found the source level with it.
	OpMirrorSync OpKind = "mirror_sync"

	// OpRename claims a new name for a tool-managed repository on that name's
	// subject, leaving the old one redirecting to it ([Rename]).
	OpRename OpKind = "rename"

	// OpReleaseName frees a renamed repository's old name once its redirect
	// has ended, on that name's subject ([ReleaseName]).
	OpReleaseName OpKind = "release_name"

	// OpHold is a live turn's hold on a scratch repository, or on one seat's
	// branch of any repository, on the repository's own subject, ordered
	// against a deletion of what it holds ([Hold]).
	OpHold OpKind = "hold"

	// OpChangeOpen opens a change (web!42) on a repository: its row, its
	// owner as its first author and its first revision, the source branch's
	// head — numbered by the apply from the repository's next_change, which
	// the same apply advances — on the repository's own subject
	// ([ChangeOpen]), decided by the repository's committer alone
	// ([Committer.ProposeChange]).
	OpChangeOpen OpKind = "change_open"

	// OpChangeEdit sets a live change's title, its body or the work item it
	// carries ([ChangeEdit]), on the repository's own subject, decided by its
	// committer alone.
	OpChangeEdit OpKind = "change_edit"

	// OpChangeDraft makes a live change a draft, or makes a draft ask to be
	// merged ([ChangeDraft]), on the repository's own subject, decided by
	// its committer alone.
	OpChangeDraft OpKind = "change_draft"

	// OpChangeRetarget moves a live change's target to another live branch
	// ([ChangeRetarget]), on the repository's own subject, decided by its
	// committer alone.
	OpChangeRetarget OpKind = "change_retarget"

	// OpChangeClose closes a live change without merging it ([ChangeClose]),
	// on the repository's own subject, decided by its committer alone.
	OpChangeClose OpKind = "change_close"

	// OpChangeReopen opens a closed change again ([ChangeReopen]) — with a
	// revision of its own where its source branch moved while it was closed
	// — on the repository's own subject, decided by its committer alone.
	OpChangeReopen OpKind = "change_reopen"

	// OpChangeLanded records a live change MERGED because its target came to
	// hold its head — moved past it, by a push that did not set it to exactly
	// that head — as the repository's home Gitlet node found it ([ChangeLanded]),
	// on the repository's own subject, decided by its committer alone against
	// the change's head and the target row the home read.
	OpChangeLanded OpKind = "change_landed"

	// OpReviewers asks reviewers of a live change, or withdraws the ask
	// ([ReviewerRequest]), on the CHANGE's own subject ([KindChange]),
	// written by the repository's committing data node ([ChangeWriter]).
	OpReviewers OpKind = "reviewers"

	// OpComment says something on a change — a thread of its own, anchored
	// to a line of one of its revisions or not, or a reply to a thread
	// ([Comment]) — on the change's own subject, written by the repository's
	// committing data node.
	OpComment OpKind = "comment"

	// OpThreadResolve marks a thread of a change's comments resolved, or
	// open again ([ThreadResolve]), on the change's own subject, written by
	// the repository's committing data node.
	OpThreadResolve OpKind = "thread_resolve"

	// OpCommentEdit replaces the words of one of a change's comments, by its
	// author ([CommentEdit]), on the change's own subject.
	OpCommentEdit OpKind = "comment_edit"

	// OpCommentDelete deletes one of a change's comments ([CommentDelete]):
	// its row stays in its thread with its words cleared, on the change's own
	// subject.
	OpCommentDelete OpKind = "comment_delete"

	// OpReview is a review of one revision of a change — a verdict, a
	// summary and the findings it raises, and the reviewer's own standing
	// findings it carries ([Review]) — on the change's own subject, written
	// by the repository's committing data node, ONE RECORD with all its
	// findings; a review of a later revision than a finding of its
	// reviewer's supersedes that finding unless it carries it.
	OpReview OpKind = "review"

	// OpFindingResolve answers findings of a change — resolved, won't fix
	// and disputed by an author, dismissed by its raiser, a maintainer or a
	// person ([FindingResolve]) — on the change's own subject, written by
	// the repository's committing data node.
	OpFindingResolve OpKind = "finding_resolve"

	// OpRevise records one revision's facts on its change — what its head is
	// against its target's tip, as the repository's home Gitlet node read them,
	// and the path owners whose globs its touched paths match, with the
	// reviews that asks ([Revise]) — on the change's own subject, written by
	// the repository's committing data node for the home, which asks it and
	// publishes nothing itself.
	OpRevise OpKind = "revise"

	// OpMergeability records whether a change's head revision merges cleanly
	// into its target's tip — clean, in conflict naming the paths, or
	// undecided within its home's deadline — as the repository's home Gitlet
	// node merged the two commits in a scratch of its own ([Mergeability]),
	// on the change's own subject, written by the repository's committing
	// data node for the home, which asks it and publishes nothing itself.
	OpMergeability OpKind = "mergeability"

	// OpMergeabilityKept records a re-evaluation of a change's mergeability
	// that found the outcome its row holds — a recheck of a moved target, a
	// refresh of its readiness — by what it moved alone: the tip a recheck
	// merged into, and the versions and the checks position a readiness was
	// judged at ([MergeabilityKept]). Never asked for: the change writer
	// forms it in place of the [OpMergeability] its home sent, on the
	// change's own subject.
	OpMergeabilityKept OpKind = "mergeability_kept"

	// OpAsks settles a change's asks for review with its lifecycle — ending
	// the asks still waiting on a change that closed or merged, and asking
	// again those its end left on one reopened ([AskSettle]) — on the
	// change's own subject, written by the repository's committing data node
	// for the home, which asks it and publishes nothing itself.
	OpAsks OpKind = "asks"

	// OpCheckQueued queues one attempt of one check of one commit, or records
	// it skipped ([CheckQueued]), on the repository's CHECKS subject
	// ([KindChecks]), its apply a join (checks.go).
	OpCheckQueued OpKind = "check_queued"

	// OpCheckStarted claims an attempt for a runner, or names the box its
	// holder made ([CheckStarted]), on the repository's checks subject.
	OpCheckStarted OpKind = "check_started"

	// OpCheckFinished ends an attempt ([CheckFinished]), on the repository's
	// checks subject, and removes the repository's rows past [CheckHistory].
	OpCheckFinished OpKind = "check_finished"

	// OpChecksSweep removes a repository's check rows past [CheckHistory]
	// where no check of its own ends to ([ChecksSweep]), on its checks
	// subject — the code-retention duty's.
	OpChecksSweep OpKind = "checks_sweep"

	// OpCheckDispatched records a repository's home's reading of a target's
	// checks file at its tip and the change heads it dispatched under it,
	// with the attempts that dispatch queues at them ([CheckDispatched]), on
	// the repository's checks subject: what the home's due listing converges
	// on, and code_checks' rows the attempts make, each as a check_queued
	// record's would (checksdispatch.go).
	OpCheckDispatched OpKind = "check_dispatched"

	// OpMerge merges a change into its target ([Merge]), on the repository's
	// own subject, decided by its committer alone through the change's merge
	// gate from one snapshot ([Committer.ProposeMerge], ADR-0096): ONE
	// RECORD merges the change, moves the target onto the merge's commit and
	// lands that move as a push.
	OpMerge OpKind = "merge"

	// OpEnqueue puts a change in its target's merge queue ([Enqueue]), on the
	// repository's own subject, decided by its committer alone through the
	// change's merge gate ([Committer.ProposeQueue]).
	OpEnqueue OpKind = "enqueue"

	// OpDequeue takes a live entry out of its target's merge queue at a
	// principal's ask ([Dequeue]), on the repository's own subject, decided by
	// its committer alone.
	OpDequeue OpKind = "dequeue"

	// OpMarks moves one person's marks over their code notices by one
	// gesture ([PersonMarks], marks.go) — read, unread, snoozed, read
	// through a position on this log, their primary reasons — on the
	// person's own subject ([KindPerson]), written by whichever data node
	// the mark is asked of ([Marks]).
	OpMarks OpKind = "marks"

	// OpViewed moves one person's viewed marks on one change's files by one gesture
	// ([ViewedMarks]) on their own subject, under [ViewedPath], forgetting their marks on ended changes.
	OpViewed OpKind = "viewed"

	// OpViewedKey records the key a repository's viewed marks are digested under ([ViewedKeyRecord]),
	// on its own subject, once: before the first mark of any of its files.
	OpViewedKey OpKind = "viewed_key"

	// OpCandidate records a queue's TRAIN ([Candidate]): the
	// candidates its home built of a target's live entries onto the target's
	// tip, each a merge of a batch of them on the one before, and the pack
	// their objects are stored in — on the repository's own subject, decided
	// by its committer alone ([Committer.ProposeQueue]).
	OpCandidate OpKind = "candidate"

	// OpEject puts live entries out of their queue ([Eject]) — a check failed
	// on what they were tested in, their merge conflicted, their gate no
	// longer holds — on the repository's own subject, decided by its
	// committer alone.
	OpEject OpKind = "eject"

	// OpLand lands a queue's entries on their target ([Land]) — a train's
	// tested prefix — merging each change and moving the target in ONE
	// record whose decide proves every entry's gate again with its enqueuer
	// as the merger, on the repository's own subject, decided by its
	// committer alone.
	OpLand OpKind = "land"

	// OpGreen moves a protected target's green row onto a landing whose
	// required checks passed ([Green]), on the repository's own subject.
	OpGreen OpKind = "green"

	// OpBroken marks a landing above its target's green row broken, a
	// required check of it having failed ([Broken]), on the repository's own
	// subject.
	OpBroken OpKind = "broken"

	// OpQueueSweep removes a repository's ended queue entries and its
	// landings below their target's green row once they outlive
	// [QueueHistory], where no record of its queues' own does ([QueueSweep])
	// — the code-retention duty's, on the repository's own subject.
	OpQueueSweep OpKind = "queue_sweep"

	// OpCloseSettled settles the closes landed queue entries owe their
	// changes' work items, each closed or definitively not, every close a
	// turn of their home ended in one record ([CloseSettled]) — made as each
	// entry's enqueuer, recorded on the repository's own subject.
	OpCloseSettled OpKind = "close_settled"
)

// OpKinds are the sixty, in the order they are documented.
var OpKinds = []OpKind{
	OpCreate, OpPush, OpSettings, OpDelete, OpRestore, OpPurgePart, OpPurge,
	OpActivate, OpEviction, OpGeneration, OpBarrier, OpTrimmed, OpCompact, OpRetireSweep,
	OpExpungeRequest, OpExpungeBegin, OpExpungeCommit, OpExpungeScan, OpImportChunk, OpImportDone, OpMirrorSync,
	OpRename,
	OpReleaseName, OpHold,
	OpChangeOpen, OpChangeEdit, OpChangeDraft, OpChangeRetarget, OpChangeClose, OpChangeReopen, OpChangeLanded,
	OpReviewers, OpComment, OpThreadResolve, OpCommentEdit, OpCommentDelete, OpReview, OpFindingResolve, OpRevise,
	OpMergeability, OpMergeabilityKept, OpAsks,
	OpCheckQueued, OpCheckStarted, OpCheckFinished, OpChecksSweep, OpCheckDispatched, OpMerge, OpEnqueue, OpDequeue,
	OpMarks, OpViewed, OpViewedKey, OpCandidate, OpEject, OpLand, OpGreen, OpBroken, OpQueueSweep, OpCloseSettled,
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
	return []OpKind{OpReviewers, OpComment, OpThreadResolve, OpCommentEdit, OpCommentDelete, OpReview,
		OpFindingResolve, OpRevise, OpMergeability, OpMergeabilityKept, OpAsks}
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
	// it creates one (code_repos.created_by_kind). BASE FORMAT ([RecordVersion]).
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
	// ID is who decided it, BARE: a name as iam.ActorFor gives it (a person's
	// human seat, an unbound credential's login, a seat's handle) or the
	// deciding node's id. NO PREFIX: the kind is a column of its own, and no
	// other row in the audit feed calls that party `operator:<name>`.
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
		return refusedf("%w: actor.kind: %q is not one of %s", ErrInvalid, a.Kind, words.Or(ActorKinds()))
	case strings.TrimSpace(a.ID) == "":
		return refusedf("%w: actor.id: a record names who decided it", ErrInvalid)
	case a.Kind == ActorOperator && strings.TrimSpace(a.OperatorID) == "":
		return refusedf("%w: actor.operator_id: an operator's record names the "+
			"credential it was made with", ErrInvalid)
	}
	return nil
}

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted. Its EIGHT
// keys are RESERVED at the format's top level for its life: a later version may
// add fields beside them, never repurpose one, as the event envelope splits a
// known header from an opaque body, since a rolling upgrade puts records this
// build cannot read on the wire and dropping them would be an outage.
type RecordEnvelope struct {
	// V is the record version, refused for the PAYLOAD when unknown and
	// never for this struct.
	V int `json:"v"`

	// OpID is the idempotency key, the message id and the ops table's key.
	// EMPTY on a barrier and a trimmed record ([EncodeBarrier], [EncodeTrimmed]).
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record arbitrates on.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is the AUTHORED instant, the writer's own clock: provenance
	// only, NEVER ORDERED ON and never written to a row; every instant this
	// domain stores is the broker's.
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

// The BROKER'S instant is deliberately NOT in the envelope: it reaches the
// applier as [statelog.ApplyOptions.StoredAt], because a field a writer could
// set is one a writer could lie about.

// ErrFutureVersion reports a record a newer build wrote. It carries the
// SUBJECT the applier's deferral arm files the record under.
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
		// No scope key never reaches the scope's own decoder, so its widening
		// is made here: absent is the widest claim, never "touches nothing".
		env.Scope = Scope{DomainPath()}
	}
	return env, nil
}

// Decode is the SECOND pass, the one that may refuse on version. It returns
// the envelope beside the error whenever the envelope decoded: the applier
// retains that case.
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
// LOSSLESS both ways: a node republishing a record it half understood must not
// strip the half it did not, and a carried field LOSES to a known one
// ([encode]). A ZERO V means "stamp it", and every writer leaves it unset; a
// set version below what the record carries is refused, and so is any
// versioned field on a record every build must read.
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

// recordFields is every top-level name this build writes, the omitempty ones
// included: a name missing here would decode into the struct AND be carried as
// unknown, so the next encode would write the stale copy over what was set.
var recordFields = fieldSet(MutationRecord{}, "op_id", "created_at", "gen",
	"writer", "expect", "mutation", "actor", "actor_kind", "operator_id")

// recordFor encodes one record decided inside a snapshot: the ONE place this
// domain's writers stamp a record (Writer, Gen), name its actor and check its
// scope. payload == nil writes no mutation.
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
			// NO VERSION: [Encode] stamps it.
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

// EncodeBarrier renders the read index's payload-free append: the framework
// owns when a barrier goes out and what its ack proves, this domain what a
// record on its log looks like. AN OP ID IS REFUSED: a message id repeated in
// the duplicate window is answered from the dedupe cache with no quorum round
// trip, a position nothing confirmed, the one claim a barrier must never fake.
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
// ADR-0080) with the barrier's split: the framework decides when one is
// appended, what it removes and its floor, this domain what it looks like.
// THE WRITER TRAVELS, unlike a barrier's: a trimmed record removes rows, so the
// eviction gate must be able to drop an evicted node's. NO OP ID. The floor is
// checked here at the write, not in [Subject.Validate], which reads run too.
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
