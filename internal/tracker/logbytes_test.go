package tracker_test

import (
	"cmp"
	"encoding"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The reference company's day, the corpus the storage forecast in
// docs/guides/retention.md is stated for: 100 000 tasks a year, 300 000
// comments, 1 000 edits a day and 3 000 turns a day — and the read barriers
// beside them, its 100 agent seats' linearizable reads at
// statelog.LinearizableReadsPerSeatDay and the object collector's 25.
const (
	referenceTasksADay    = 100_000.0 / 365
	referenceCommentsADay = 300_000.0 / 365
	referenceEditsADay    = 1_000.0
	referenceTurnsADay    = 3_000.0
	referenceSeatReads    = 100 * statelog.LinearizableReadsPerSeatDay
	referenceBarriersADay = referenceSeatReads + 25

	// referenceKeyGrowth is how many bytes longer a key is at a five-year-old
	// project of the reference company's than at the new one the corpus
	// files into — two digits to five — and referenceKeysADay how many times
	// a day one is written: three on a create (the counter's value, the
	// task's key, its snapshot's), once on a comment or an edit.
	referenceKeyGrowth = 3
	referenceKeysADay  = 3*referenceTasksADay + referenceCommentsADay + referenceEditsADay
)

// THE CODE LOG'S DAY, which config sizes the code log's ceiling against this
// log's by (DerivedCodeLogNumerator's reason, and code's CodeLogMaxBytes): at
// the ratio of the two days a blocked trim fills both logs in the same time,
// so that ratio is a figure derived from this log's published day. A push is
// a SEAT'S, as every one of the reference company's is — a hundred seats
// pushing thirty times a day — alone in its record, and priced as its CODING
// BOX'S, the most a seat's push takes of the log: internal/code's measured
// figure for a person's push (BenchmarkPushLogBytes, its package doc's "What
// a push costs, measured") and what a box's principal — the seat, the run
// behind it and the coding run that pushed — its way and the company day its
// quota counted it on take of the log beyond it (its push=box arm). A seat's
// push the engine builds for a tool takes 74 bytes less (its push=agent arm),
// and which of a company's seats push from their boxes and which through
// their tools is no figure the reference company has, so the day prices every
// push at the box's, at its least as at its most — as it prices the
// compactions at the most the reference company's pushes cost them. The
// COMPACTIONS that merge those pushes' packs take the log a push too, and so
// do the code-retention duty's SWEEPS of the rows they retired, a day later:
// internal/code/compact's measured figures
// (BenchmarkCompact's steady/pushes=1200/rate=1/min/min-merge=16 arm, its
// record-B/push and sweep-B/push, in its package doc's "Measured"), a
// repository pushed once a minute, so every merge takes the floor's
// code.CompactMinMerge packs and is about a quarter hour from the next —
// every merge's retirements swept by a record of their own. That is the
// reference company's shape: 3 000 pushes a day over its repositories are a
// few a minute, which never leave a pass more than the floor to merge. A
// merge of more costs less a push (43 bytes where a minute's merge takes
// three hundred), and so do a quarter hour's merges swept together, so these
// two figures are the most the reference company's pushes cost the log, and
// the day prices both terms at them, at its least as at its most. They are a
// MERGE's figures and nothing else's: the steady arm's compactor publishes no
// layer, so no compaction it makes carries one. A PUBLICATION — the
// compaction a repository's maintainer appends to publish a multi-pack-index
// layer, naming every pack the layer covers — is a term of its own, at
// internal/code's measured figures (codeChange… below). The barriers are its
// git clients' linearizable reads: a seat's push asks the log three times
// (its advertisement, its receive's sync and the fetch before it), thirty
// people fetch every three minutes of an eight-hour day, and a CI polls ten
// repositories a minute where one runs.
//
// A push's box names the work item its run was launched for by its identity
// (code.RunProvenance.WorkID, a native task's UUID), which the push=box arm
// carries: 224 bytes beside a person's push, 49 of them that identity.
const (
	referencePushesADay = 3_000
	codePushRecordBytes = 1_604
	// codeEnginePushRecordBytes is a seat's push the engine builds for a
	// tool, alone in its record (BenchmarkPushLogBytes's push=agent arm):
	// no term of the day, which prices every push at the box's, and printed
	// beside it.
	codeEnginePushRecordBytes = 1_481
	codeCompactionBytes       = 108
	codeSweepBytes            = 44
	referencePushReads        = 3 * referencePushesADay
	referencePeopleReads      = 30 * 8 * 60 / 3
	referenceCIReads          = 10 * 60 * 24
)

// A CHANGE'S RECORDS, each what internal/code's
// TestAChangesRecordsCostTheLogWhatTheCodeLogsDayPricesThemAt holds it to
// (its changeRecordBytes, which states the same figures): every record of a
// change's life as this build's writers form it at the reference company's
// text — a 2 KiB description, a 1 KiB summary, comment and finding body —
// alone in its record, by the log's growth. A publication is a compaction,
// measured there beside them because the committer's compact path forms it.
const (
	codeChangeOpenBytes     = 3_430
	codeChangeEditBytes     = 3_100
	codeReviewersBytes      = 1_190
	codeReviseBytes         = 1_640
	codeMergeabilityBytes   = 1_340
	codeReviewFindingsBytes = 5_030
	codeReviewBytes         = 2_340
	codeAnswerBytes         = 1_290
	codeCommentBytes        = 2_280
	codeThreadResolveBytes  = 1_160
	codeMergeBytes          = 1_990
	codeEnqueueBytes        = 980
	// A queued change as its home's queue worker takes it on: its train of one
	// candidate, a check of the candidate's tip queued (its claim, its box and
	// its ending carry a head's keys, priced at theirs) and its landing.
	codeCandidateBytes            = 1_300
	codeCandidateCheckQueuedBytes = 1_020
	codeLandBytes                 = 1_530
	codeCheckQueuedBytes          = 970
	codeCheckDispatchedBytes      = 970
	codeCheckStartedBytes         = 780
	codeCheckNamedBytes           = 840
	codeCheckFinishedBytes        = 840
	codeTipCheckQueuedBytes       = 960
	codeGreenBytes                = 800
	codeBrokenBytes               = 1_040
	codePublicationTailBytes      = 3_830
	codePublicationOneBytes       = 1_110
)

// THE REFERENCE COMPANY'S CHANGES, a STATED JUDGEMENT rather than a
// measurement, since no deployment has reported a day of them: every push is
// a revision of the change its coding run works on, ten pushes a change, so
// its 3 000 pushes a day are 300 changes. Each change is opened by a seat's
// code_open_change, asks one reviewer, takes a review asking for changes with
// two findings, an answer resolving them, two comments and a thread resolved,
// then an approving review, and is merged deleting its source — and its home
// records every revision's facts and its merge into the target, and the
// readiness it stores again after the ask, the two reviews and the answer
// (code.Mergeability.Refresh). That is the day at its least: no checks file,
// no queue, and a target that never moves under a live change. At its most,
// every change is queued first, its home rechecks it at every pass while its
// target moves (internal/code/home's MergeabilityRefresh, ten minutes, over a
// working day of eight hours), and its target's checks file defines three
// checks, each of every revision queued, claimed, its box named and ended,
// beside the head's dispatch. Every queued change is then taken on by its
// home's queue worker: recorded in a train of its own — one candidate, the
// change merged onto its target's tip, as a change reaching its target a few
// an hour is — its three checks, required, queued on the candidate's tip
// (purpose candidate, no dispatch record), each claimed, its box named and
// ended, and the train landed in a land record. One train a change is the
// most: a burst queued faster than its checks shares a train, and a landing,
// and costs less. And at the most every change's landing lies above its
// target's green row — landed ahead of its checks, as a person's override
// merge or push into a branch whose gate requires them lands — so its home's
// tip work checks it: the three checks queued on the landed commit (purpose
// tip, no dispatch record), each claimed, its box named and ended, and a
// green moving the target's green row onto it. One a change is the most,
// since each landing is the newest when its tip is judged; a burst landing
// faster than its checks coalesces onto its newest and costs less. The most
// is each shape's own, never one change's path through them: it prices a
// change's merge, its queue's landing and its landing's tip checks alike. A
// broken is a red tip's, at the rate a merged result fails a check both heads
// passed, and an ejection a red or conflicting candidate's, neither of which
// the reference company's day has (codeLogNegligible).
//
// A PUBLICATION names every pack its layer covers. At the least the
// reference company's packs are each named once, in a busy repository's tail
// of compact.PublishPacks (64); at the most, beside that, every push is
// published on its own, as a young repository's tail is once it holds a
// share of its objects — a repository published at most every
// compact.PublishMinInterval (ten minutes), which its pushes, a few an hour,
// never outrun.
const (
	referenceChangesADay      = referencePushesADay / 10
	referenceRevisionsADay    = referencePushesADay
	referenceRefreshesAChange = 4
	referenceRechecksAChange  = 8 * 60 / 10
	referenceChecksAHead      = 3
	referenceChecksACandidate = referenceChecksAHead
	referenceChecksATip       = referenceChecksAHead
	referencePublishPacks     = 64
)

// codeLogTerm is one term of the code log's day: the bytes a day one SHAPE of
// one op's records takes at the reference company — the records its figure
// was measured on — at the least of its day and at the most: the barriers
// without a CI and with one, every other term the same at both.
//
// A term is a shape's and not its op's, because one op is appended by more
// than one writer, at sizes and rates no one figure covers: a publication is
// a compaction, and a merge's 108 bytes a push say nothing of the layer one
// names. So an op's records of another shape are a term of their own beside
// its first, never folded into a figure measured without them.
type codeLogTerm struct {
	op code.OpKind
	// shape is which of the op's records the term prices: the ones its
	// figure was measured on.
	shape       string
	least, most float64
}

// codeLogDay is the code log's day, a term for each shape of record it
// prices: a seat's push, priced as its coding box's, a merge of the pushes'
// packs — a plain merge, since the steady arm its figure is measured on
// publishes nothing — and a publication of their layers, the sweep of what
// the merges retired, its git clients' barriers at code.BarrierRecordBytes,
// and every record of the reference company's changes: their lifecycle,
// their conversation, their home's facts, merges and readiness, their merges,
// and at the most their queue — its entry, its train, its candidate's checks
// and its landing — their checks, and their landings' tip checks and green.
// What the day is summed from, so a term priced here is a term of every
// figure TestTheDocsPrintTheMeasuredDay holds.
func codeLogDay() []codeLogTerm {
	changes, revisions := float64(referenceChangesADay), float64(referenceRevisionsADay)
	checks := revisions * referenceChecksAHead
	candidates := changes * referenceChecksACandidate
	tips := changes * referenceChecksATip
	git := float64((referencePushReads + referencePeopleReads) * code.BarrierRecordBytes)
	day := []codeLogTerm{{op: code.OpBarrier, shape: "a git client's linearizable read",
		least: git, most: git + float64(referenceCIReads*code.BarrierRecordBytes)}}
	// Every other term is a count of records a day at a measured size: at
	// the least and the most alike, or (onlyMost) at the most alone.
	for _, t := range []struct {
		op       code.OpKind
		shape    string
		records  float64
		bytes    float64
		onlyMost bool
	}{
		{code.OpPush, "a seat's coding box's push alone in its record, received by git and naming its " +
			"coding run and its work item's identity beside the turn — the most a seat's push takes, at " +
			"which the day prices every one of the reference company's — as BenchmarkPushLogBytes's " +
			"push=box arm measures it", referencePushesADay, codePushRecordBytes, false},
		{code.OpCompact, "a merge: a compaction carrying no layer, as every one BenchmarkCompact's " +
			"steady arm makes", referencePushesADay, codeCompactionBytes, false},
		{code.OpCompact, "a publication of a busy repository's tail, its layer naming " +
			"compact.PublishPacks packs: every pack the pushes add named once",
			referencePushesADay / float64(referencePublishPacks), codePublicationTailBytes, false},
		{code.OpCompact, "a publication of a young repository's one pack, one a push",
			referencePushesADay, codePublicationOneBytes, true},
		{code.OpRetireSweep, "a sweep of the pack rows the merges retired, a day later",
			referencePushesADay, codeSweepBytes, false},
		{code.OpChangeOpen, "a seat's code_open_change at a 2 KiB description, one a change", changes,
			codeChangeOpenBytes, false},
		{code.OpReviewers, "an ask of one reviewer, one a change", changes, codeReviewersBytes, false},
		{code.OpReview, "a seat's review asking for changes with two findings, one a change", changes,
			codeReviewFindingsBytes, false},
		{code.OpReview, "a seat's review approving, one a change", changes, codeReviewBytes, false},
		{code.OpFindingResolve, "the owner's answer resolving a review's findings, one a change", changes,
			codeAnswerBytes, false},
		{code.OpComment, "a seat's 1 KiB comment, two a change", 2 * changes, codeCommentBytes, false},
		{code.OpThreadResolve, "a thread resolved, one a change", changes, codeThreadResolveBytes, false},
		{code.OpMerge, "a merge of the change deleting its source, one a change", changes, codeMergeBytes,
			false},
		{code.OpRevise, "a revision's facts, one a push", revisions, codeReviseBytes, false},
		{code.OpMergeability, "a revision's merge into its target, one a push", revisions,
			codeMergeabilityBytes, false},
		{code.OpMergeability, "the stored readiness refreshed after an ask, a review or an answer, four a " +
			"change", referenceRefreshesAChange * changes, codeMergeabilityBytes, false},
		{code.OpMergeability, "a recheck of a moved target at every pass of its home over a working day",
			referenceRechecksAChange * changes, codeMergeabilityBytes, true},
		{code.OpEnqueue, "a change queued into its target, one a change", changes, codeEnqueueBytes, true},
		{code.OpCandidate, "a train of one candidate recorded by its home's queue worker, the change merged " +
			"onto its target's tip with the pack its merge wrote, one a change", changes, codeCandidateBytes, true},
		{code.OpCheckQueued, "a check of a queue's candidate queued by its home's queue worker, naming the " +
			"candidate and the base it was built onto, three a change", candidates,
			codeCandidateCheckQueuedBytes, true},
		{code.OpCheckStarted, "a candidate check claimed by its runner, three a change — a head's claim's " +
			"keys", candidates, codeCheckStartedBytes, true},
		{code.OpCheckStarted, "a candidate check's box named by its runner, three a change — a head's box's " +
			"keys", candidates, codeCheckNamedBytes, true},
		{code.OpCheckFinished, "a candidate check ended, three a change — a head's ending's keys", candidates,
			codeCheckFinishedBytes, true},
		{code.OpLand, "a train of one queued change landed by its home's queue worker, its source deleted, " +
			"one a change", changes, codeLandBytes, true},
		{code.OpCheckQueued, "a check of a revision queued, three a push", checks, codeCheckQueuedBytes, true},
		{code.OpCheckDispatched, "a head's dispatch, one a push", revisions, codeCheckDispatchedBytes, true},
		{code.OpCheckStarted, "a check claimed by its runner, three a push", checks, codeCheckStartedBytes,
			true},
		{code.OpCheckStarted, "a check's box named by its runner, three a push", checks,
			codeCheckNamedBytes, true},
		{code.OpCheckFinished, "a check ended, three a push", checks, codeCheckFinishedBytes, true},
		{code.OpCheckQueued, "a check of a landing's tip queued by its home's tip work, naming the base " +
			"its checks file is read at, three a change", tips, codeTipCheckQueuedBytes, true},
		{code.OpCheckStarted, "a tip check claimed by its runner, three a change — a head's claim's keys",
			tips, codeCheckStartedBytes, true},
		{code.OpCheckStarted, "a tip check's box named by its runner, three a change — a head's box's keys",
			tips, codeCheckNamedBytes, true},
		{code.OpCheckFinished, "a tip check ended, three a change — a head's ending's keys", tips,
			codeCheckFinishedBytes, true},
		{code.OpGreen, "a target's green row moved onto a change's landing once its tip checks passed, " +
			"one a change", changes, codeGreenBytes, true},
	} {
		bytes := t.records * t.bytes
		least := bytes
		if t.onlyMost {
			least = 0
		}
		day = append(day, codeLogTerm{op: t.op, shape: t.shape, least: least, most: bytes})
	}
	return day
}

// codeLogOp is the day's terms of op summed: what op's records take a day,
// whatever their shapes.
func codeLogOp(day []codeLogTerm, op code.OpKind) codeLogTerm {
	sum := codeLogTerm{op: op}
	for _, term := range day {
		if term.op == op {
			sum.least += term.least
			sum.most += term.most
		}
	}
	return sum
}

// codeHoldRecordBytes is the most one hold record of a scratch repository
// takes the code log as the engine writes one — a turn's run id, an operation
// id at statelog.MaxCallerOpIDBytes and a seat handle at iam.MaxLogin —
// measured as the day measures a push, by the log's growth: internal/code's
// TestAHoldRecordCostsTheLogOneSmallRecord measures 865 bytes and holds a hold
// at most this (its holdRecordBytes, which says what the rest covers). The
// hold's reason in codeLogNegligible weighs the turns' holds by it.
const codeHoldRecordBytes = 896

// codeLogNegligible is every op of the code domain whose records the code
// log's day leaves out, and why: each is appended at the rate a person edits
// the company's document or runs a command, at the rate the company makes,
// renames and retires repositories (each manager's rate argued in
// codeLogManagers), or as the trim and the code-retention duty sweep what the
// log no longer needs — a few records a day beside 3 000 pushes and the tens
// of thousands of reads around them — or, a live turn's hold, at a rate the
// reference company's day has none of, argued with the figures
// TestTheHoldsReasonWeighsEveryTurnsHoldAgainstTheDay holds.
var codeLogNegligible = map[code.OpKind]string{
	code.OpCreate: "a repository is created when an activation of the company's document " +
		"declares it, or a person's or a seat's tool makes one: once a repository",
	code.OpSettings: "a repository's settings change when an activation of the company's " +
		"document changes them, or a tool changes, archives, unarchives or protects it",
	code.OpDelete: "a repository is deleted when an activation of the company's document " +
		"drops it, a tool deletes it, or the code-retention duty expires a scratch repository " +
		"idle code.scratch.idle_days: once a repository until a restore",
	code.OpRestore: "a deleted repository returns when an activation of the company's " +
		"document declares it again, or a tool restores it",
	code.OpRename: "a tool's rename of a repository a tool made: one record a rename, on the " +
		"name it claims",
	code.OpReleaseName: "the code-retention duty's release of a renamed repository's old name " +
		"once its redirect ends (code.RedirectWindow, a month): once a rename",
	code.OpHold: "a live turn's hold on a scratch repository idle code.HoldRecordAfter (22 h) " +
		"or more, which is no deliberate act but a turn's: one record for each TURN that names " +
		"such a repository, at its first touch of it — and since neither a read nor a hold moves " +
		"the idle clock, a seat re-reading an idle experiment records one in every turn that " +
		"names it, on every day from the repository's 22nd idle hour until a push moves its clock " +
		"or code.scratch.idle_days (7 unless set) expires it: at most a seat's turns a day times " +
		"the idle scratch repositories each names. The reference company's day makes no scratch " +
		"repository — its 3 000 pushes a day go to repositories it already has (codeLogManagers' " +
		"tools) — so it records none. That is the whole case for leaving it out, and what rides on " +
		"it is not small: were each of its 3 000 turns a day to name one, at the most a hold record " +
		"takes as the engine writes one (896 bytes, codeHoldRecordBytes), they would append 2.69 " +
		"MB a day, 11 % of the day's least of 24.7 MB — a company whose seats work so reads it in " +
		"its code log's crewlet.statelog.log.bytes_per_day. The window is that long because " +
		"code.HoldRecordAfter is idle_days' floor, not the idle_days in force; pricing the rate " +
		"needs a figure from a running company's tool calls, which nobody has, as codeLogManagers " +
		"says of a seat's scratch creates. The op's other record is a live turn's hold on a SEAT'S " +
		"BRANCH no push has set for code.RefHoldRecordAfter (334 h) or more: one for each turn that " +
		"names such a branch, at its first touch of it — a turn coming back to work left two weeks — " +
		"its frame, with the longest ref, at most internal/code's refHoldRecordBytes. The reference " +
		"company's day, whose pushes keep the branches its runs work on moving, records none of " +
		"these either, and they are left out on the same case",
	code.OpPurgePart: "a deleted repository's rows go in bounded parts once it is past its " +
		"retention: once a repository",
	code.OpPurge:    "a deleted repository's last part: once a repository",
	code.OpActivate: "the activation fence: one record an activation, fleet-wide",
	code.OpEviction: "an operator's eviction of a node from the log, or its readmission",
	code.OpGeneration: "a reanchor's record, once an operator recreates or restores the " +
		"log",
	code.OpTrimmed: "the trim's sweep of the anchors under its floor, one record per " +
		"statelog.AnchorsPerTrimmedRecord, and the log keeps an anchor a repository and a " +
		"name, never one a push or a read",
	code.OpExpungeRequest: "a person's purge of a repository's unreachable objects: a command " +
		"a person runs, one record each, a later one widening the purge already open",
	// AN EXPUNGE'S TWO RECORDS, appended by the repository's maintainer
	// (internal/code/compact's expunge loop, polling code.ReadExpungeDue),
	// come at the rate of the purge above, which is the one thing this build
	// expunges for: a schedule of the maintainer's own — an expunge keeping
	// a grace, ExpungeBegin.Grace not 0 — would append them at a rate of its
	// own, which the day must price once it exists.
	code.OpExpungeBegin: "the begin of an expunge serving a person's purge: its repository's " +
		"maintainer begins a cycle for each purge asked, as many as its live packs need — one or " +
		"two for a repository at the default ceiling (code.MaxExpungeOutputs) — each begin naming " +
		"at most code.MaxExpungeInputs packs it retires and code.MaxExpungeOutputs it writes",
	code.OpExpungeCommit: "the commit that ends an expunge its begin opened: one a begin, " +
		"retiring its dropping packs and adding their rescue, each at most code.MaxExpungeOutputs",
	// A PERSON'S MARK is their own gesture, never a seat's: code_mark_inbox
	// is human-only and the operator catalogue's alone, so its rate is the
	// presses of a person's Inbox buttons and their assistant's calls — and the
	// reference company's day, whose changes are its seats', reaches its few
	// persons as notices, not as records of theirs.
	code.OpMarks: "a person's gesture over their own code notices — a notice read, unread, " +
		"snoozed or unsnoozed, or everything read through a position — one record a press of " +
		"an Inbox button or an assistant's call, persons only: a person marking their day's " +
		"notices one at a time writes tens of them, each a gesture and the positions of the " +
		"notices it names, a few hundred bytes for the one or two a press names",
	// A GESTURE ON A CHANGE beside its open, its reviews and its merge: the
	// reference company's changes are opened ready, on the target they merge
	// into, and merged, so its day makes none of them. Each is one record
	// of code_update_change or code_close_change, the largest an edit that
	// rewrites the whole description (codeChangeEditBytes, measured beside
	// the change's other records), and a change edited once a day would
	// append under a twentieth of the day's least
	// (TestTheCodeLogsDayPricesEveryRecordItTakes holds that bound).
	code.OpChangeEdit: "a change's title, description or work item set by code_update_change: a few " +
		"times a change at most, each at most an edit rewriting the description (codeChangeEditBytes, " +
		"measured) — under a twentieth of the day's least were every change edited once",
	code.OpChangeDraft: "a change made a draft or ready by code_update_change: a few times a change at " +
		"most, each a record naming the change and the flag, smaller than an edit",
	code.OpChangeRetarget: "a change retargeted by code_update_change: once a change at most, a " +
		"record naming the change and its new target, smaller than an edit",
	code.OpChangeClose: "a change closed by code_close_change, in place of the merge a change merged " +
		"takes: once a change at most, a record naming the change and a reason, smaller than an edit",
	code.OpChangeReopen: "a closed change reopened by code_close_change: once a close at most, a " +
		"record naming the change, smaller than an edit",
	// A CHANGE'S ASKS SETTLED with its lifecycle: a review settles its own
	// reviewer's ask on its record, so only a change that closes or merges
	// with an ask still unanswered — or is reopened holding asks its end
	// left — takes one, and the reference company's changes are each
	// reviewed by the reviewer they ask before they merge.
	code.OpAsks: "a change's asks for review settled by its repository's home git node through the " +
		"estate's code.conversation — ended once it closed or merged with asks still waiting, asked again " +
		"once it is reopened holding asks its end left: at most once a close, a merge or a reopen of a " +
		"change holding an ask, a few hundred bytes each; the reference company's changes are each " +
		"reviewed by the reviewer they ask before they merge, so none ends with an ask waiting",
	code.OpChangeLanded: "a change whose target came to hold its head outside a merge record — a " +
		"person's own push of it — landed by its repository's home git node: once a change at most, " +
		"in place of the merge record such a change never takes, and smaller than it (no pack and no " +
		"principal); the reference company's changes are merged",
	code.OpChecksSweep: "the code-retention pass's sweep of an idle repository's check attempts past " +
		"code.CheckHistory once they have outlived it by code.CheckSweepGrace: a record an idle " +
		"repository holding them, a day at most, as a pack sweep's",
	// A BROKEN is a red tip's: a merged result failing a required check its
	// changes' heads each passed, which the reference company's day has none
	// of, and which a revert or a fix forward then moves past. Every change's
	// landing broken once would append under a twentieth of the day's least
	// (TestTheCodeLogsDayPricesEveryRecordItTakes holds that bound).
	code.OpBroken: "a landing above its target's green row marked broken by its home's tip work, a " +
		"required check having failed on it: once a red tip at most, each a record naming the landing " +
		"and its failed checks (codeBrokenBytes, measured) — under a twentieth of the day's least were " +
		"every change's landing broken once. The bisect that names it probes the landings between the " +
		"last green and the red tip, each probe the failed checks queued, claimed and ended, at the rate " +
		"of the red tips too; the reference company's landings are each judged before the next lands, so " +
		"its red tip is its culprit, probed by nothing",
	code.OpQueueSweep: "the code-retention pass's sweep of an idle repository's ended queue entries " +
		"and its landings below their green rows past code.QueueHistory once they have outlived it by " +
		"code.QueueSweepGrace: a record an idle repository holding them, a day at most, as a pack " +
		"sweep's",
	// A QUEUE ENTRY'S END OTHER THAN ITS LANDING: an entry ends once, so
	// each comes at most once an enqueue, and the reference company's queued
	// changes each land (codeLogDay at the most).
	code.OpDequeue: "an entry taken out of its queue before it lands by code_dequeue_change, a person's or " +
		"a seat's gesture: at most once an enqueue, a record naming the entry, who asked and its change's " +
		"routing, smaller than an edit; the reference company's queued changes each land",
	code.OpEject: "entries put out of their queue by their repository's home git node — a required check " +
		"failed on what they were tested in, their merge conflicted, their gate no longer held, their head " +
		"moved or a fast-forward was none: at most once an enqueue, since an ejected entry has ended and " +
		"only another enqueue queues its change again, each a record naming the entries, why — the failed " +
		"checks or the conflicted paths — and each change's routing; the bisect that finds a red " +
		"candidate's culprit probes its batch at that rate too. The reference company's queued changes " +
		"merge cleanly onto their targets and pass their checks, so its day ejects none",
	// AN IMPORT AND A PULL MIRROR'S SYNCS fill a repository from another
	// host, which the reference company's day never does: its repositories
	// are its own, and its pushes go to those it already has
	// (codeLogManagers).
	code.OpImportChunk: "an import's chunk, landed by its repository's home polling imports_due: an import " +
		"is named by a person's or the company's document's create of the repository " +
		"(code.CreateRepo.Import), or is a pull mirror's first fill, once a repository, in chunks of at " +
		"most code.MaxImportPacks packs or code.MaxImportRefs refs — at the rate the company makes " +
		"repositories, of which the reference company's day makes none",
	code.OpImportDone: "an import's end, landed by its repository's home: one record an import, once a " +
		"repository",
	code.OpMirrorSync: "a pull mirror's sync, landed by its repository's home polling mirrors_due: one a " +
		"mirror every code.MirrorInterval (fifteen minutes) in which its source moved — ninety-six a day " +
		"for a source that never rests — each the source's new packs and moved refs as push rows on the " +
		"engine's own account, a record for every code.MaxImportPacks packs or code.MaxImportRefs refs. " +
		"Only a person or the company's document makes a repository a mirror " +
		"(code.RepoSettings.MirrorFrom), and the reference company mirrors none, so its day syncs nothing; " +
		"a company mirroring busy sources reads their syncs in its code log's " +
		"crewlet.statelog.log.bytes_per_day",
}

// codeLogUnwritten is every op of the code domain that no node of this build
// appends, with the one name of the code domain its writer must spell, and
// how: the day leaves the op out while no file the crewlet binary is built
// from spells that name (codeLogSpelled), and
// TestTheCodeLogsDayPricesEveryRecordItTakes fails the day one does. A writer
// is one of two things, and each has its name:
//
//   - a LOOP, which polls a LISTING for its work: a code.ReadKind the code
//     domain declares for that loop, and serves before the loop exists
//     (listing);
//   - an ASKER — a tool, a route, the engine's own work — which builds the
//     PAYLOAD the op's records carry and asks the repository's committer to
//     decide it: the type the code domain decodes those records into
//     (payload), which the estate carries to the committer inside its
//     proposal without ever naming it.
//
// KEYED BY THE NAME, NOT BY A FILE: the plan names a writer's files, and an
// implementer names them after the code they land beside — the compactor's
// loop and its decide, planned in compact.go and decide_compact.go, landed in
// compactor.go, poll.go and compaction.go — so a file the plan names may never
// exist while its writer runs, and a tripwire on it would never fire. Every
// loop polls its listing and every asker builds its payload, whatever its
// files are called, and both are the code domain's own declarations. The
// compactor's poll names code.ReadCompactionDue, and the gate holds that it
// finds it there, so a walk that would find no writer at all fails rather
// than passes.
//
// EMPTY while every op the code domain declares has its writer in this build:
// the home's import and mirror loop polls imports_due and mirrors_due,
// code_dequeue_change asks for a dequeue, and the home's queue worker builds
// trains, ejects and lands — each priced into the day or argued out of it
// above. An op the domain declares before its writer exists goes here, with
// the name that writer will spell.
var codeLogUnwritten = map[code.OpKind]struct {
	listing code.ReadKind
	payload reflect.Type
	how     string
}{}

// codeLogPushVias is every way a push's pack is built (code.PushVias), and
// how the day counts the pushes built that way. A WAY IS A SHAPE OF PUSH the
// code tells apart — who builds the pack, for what, carrying how many refs,
// at what rate — and a way added (a ref expiry's, a merge queue's, a
// mirror's) is a writer of pushes the push term was not measured on, so
// TestTheCodeLogsDayPricesEveryRecordItTakes fails on it until its pushes are
// priced into the day or argued out of it here.
var codeLogPushVias = map[code.PushVia]string{
	code.ViaEngine: "a write the engine builds for a tool: a seat's takes the log less than its " +
		"coding box's push (1 481 bytes alone in its record against 1 604, BenchmarkPushLogBytes), " +
		"at which the day prices every one of the reference company's 3 000 a day, and a " +
		"person's takes it less still, naming no run and no day",
	code.ViaGit: "received by git: a seat's coding box's push, the one seat's push git may receive " +
		"(code.Proposal.Validate), naming the coding run that pushed, its coding agent and its work " +
		"item's identity beside the turn — the push term's records (codeLogDay), at which the day " +
		"prices every one of the reference company's 3 000; a person's push, which takes the log less " +
		"(1 380 bytes alone in its record against a box's 1 604), and none of the reference company's " +
		"3 000 is one; and an import's batches (crewlet code import), up to code.MaxCommandsPerPush " +
		"refs a record, once a repository when a person imports it",
	code.ViaExpiry: "the ref lifecycle's deletion of seats' branches idle past code.RefExpiry, on the " +
		"engine's own account: at most ONE deletion of each seat branch some priced push created, never " +
		"more, and up to code.MaxCommandsPerPush of them a record with no pack, no run and no day — a " +
		"deletion command takes its record under a tenth of a seat's push (a ref and two ids against " +
		"1 604 bytes alone in its record) — so the day's expiries cost the log less than the creations a " +
		"fortnight earlier did, which the push term already prices",
	code.ViaMerge: "the push a merge record lands, written by the merge's own apply and never a push " +
		"record: its bytes are the merge record's (code.OpMerge), which the day prices one a change " +
		"(codeLogDay)",
	code.ViaImport: "the push an import chunk lands, written by the chunk's own apply and never a push " +
		"record: its bytes are the chunk's (code.OpImportChunk), once a repository imported, which the " +
		"reference company's day imports none of (codeLogNegligible)",
	code.ViaMirror: "the push a pull mirror's sync lands, written by the sync's own apply and never a push " +
		"record: its bytes are the sync's (code.OpMirrorSync), which the reference company, mirroring " +
		"nothing, never appends (codeLogNegligible)",
	code.ViaQueue: "the pushes a land record lands, one a change, written by the landing's own apply and " +
		"never a push record: their bytes are the land record's (code.OpLand), which the day prices one " +
		"a change at the most (codeLogDay)",
}

// codeLogManagers is everything that may manage a repository
// (code.ManagedBys), and how the day counts the creates, settings changes,
// deletions, restores and purges of the repositories each manages. A MANAGER
// IS A SHAPE OF LIFECYCLE the code tells apart: one added — the code tools'
// own repositories, a seat's scratch ones among them — creates and deletes
// repositories at its own rate, which no reason of codeLogNegligible states,
// so TestTheCodeLogsDayPricesEveryRecordItTakes fails on it until those
// records are priced into the day or argued out of it here.
var codeLogManagers = map[code.ManagedBy]string{
	code.ManagedConfig: "a repository code.repos declares: an activation of the company's " +
		"document creates, settles, deletes and restores it, and its purge follows its " +
		"deletion — codeLogNegligible's reasons",
	code.ManagedTools: "a repository a person's or a seat's tool makes: its create and every " +
		"rename, settings change, deletion and restore is a deliberate act of one record, about " +
		"a push's own in size — under a kibibyte, two with the longest description " +
		"(code.TestAScratchCreateCostsTheLogOneSmallRecord) — and the reference company's 3 000 " +
		"pushes a day go to repositories it already has. The caps bound how many exist, not how " +
		"fast: a seat holds at most code.scratch.max_per_seat (50) scratch repositories, each " +
		"counted until its purge a week after its deletion, so one making and deleting them at " +
		"the cap makes about seven a day, and every one of the reference company's hundred " +
		"seats doing so would append under 0.73 MB of creates a day, under a twentieth of the " +
		"day's least, before their deletions and purges",
}

// routingReasons is why a record of the day's leaves out each key of a
// change's routing (code.ChangeRouting) it does not carry: every one is a
// list of handles, or a flag, that only some records' reasons reach, each a
// few dozen bytes a handle where one does.
var routingReasons = map[string]string{
	"change_id": "the change's id, which every routed record names", "change_ref": "the change's name",
	"repo_name": "the repository's name", "title": "the change's title",
	"actor": "who wrote the record, where a person or a seat did: a home's own records name nobody",
	"owner": "the change's owner", "owner_kind": "whether its owner is a person",
	"work_item": "the change's work item",
	"mentions":  "the handles a text mentions (at most code.MaxMentions, 32), and the reference text mentions nobody",
	"requested": "the reviewers an ask names, which only an ask's record routes",
	"path_owners": "the owners of the paths a revision touches, and the reference company's repositories " +
		"declare none",
	"authors": "the change's authors, which a merge, a review or its findings' answer routes; the reference " +
		"change's author is its owner",
	"verdict":    "a review's verdict, which only a review's record routes",
	"conflicted": "who hears that a change conflicts with its target, and the reference change merges clean",
	"merged":     "who hears that a change merged, routed on the record that lands it",
	"raisers":    "the reviewers whose findings an answer resolves, routed on the answer's record",
	"ready_for":  "who hears that a change became ready, once its readiness moves to ready",
	"re_review": "the reviewers a revision after a review asks again — a handle each on the revisions that " +
		"follow a review",
	"thread_participants": "who a reply or a resolve reaches beyond its author, and the reference thread is " +
		"its author's alone",
	"watchers":      "the change's watchers, and the reference change has none",
	"lead_fallback": "the lead an unreviewed change falls back to, and the reference change asks its reviewer",
	"persons":       "which handles reached are persons, and the reference company's changes are its seats'",
	"checks_failed": "who hears that a check of a change's head failed, routed on the failing ending's record, " +
		"and the reference checks pass",
	"broken": "who hears that a change's landing broke its target, routed on a red tip's broken record " +
		"(codeLogNegligible)",
	"dequeued": "who hears that a change left its target's queue, routed on a dequeue's or an ejection's " +
		"record, and the reference change lands",
	"enqueuer": "who queued the change, which only the queue's records about its entry route",
	"lead":     "the effective lead of the repository's owner, which only a broken record routes",
}

// routingAbsent is every key of a change's routing under prefix that a term's
// records do not carry — carried named without the prefix — with why.
func routingAbsent(prefix string, carried ...string) map[string]string {
	absent := map[string]string{}
	for key, why := range routingReasons {
		if !slices.Contains(carried, key) {
			absent[prefix+"."+key] = why
		}
	}
	return absent
}

// principalAbsent is every key of a principal under prefix that a term's
// records do not carry — carried named without the prefix — with why: a
// seat's records name no person's credential, and a run is named whole,
// in part or not at all.
func principalAbsent(prefix string, carried ...string) map[string]string {
	absent := map[string]string{prefix + ".operator_id": "a person's credential, and the measured record is a seat's"}
	if !slices.ContainsFunc(carried, func(key string) bool { return under(key, "run") }) {
		absent[prefix+".run"] = "the run behind a seat's push or merge, which this record's writer never " +
			"names on its principal: a seat's turn, where the record has one, is a field of its own"
		return absent
	}
	for key, why := range map[string]string{
		"run.run_id":       "the coding run that pushed, and the measured record is a tool's",
		"run.coding_agent": "the coding agent a box ran, and the measured record is a tool's",
		"run.work_id":      "the work item's identity, which a box's admission alone stamps",
	} {
		if !slices.Contains(carried, key) {
			absent[prefix+"."+key] = why
		}
	}
	return absent
}

// absent is the union of reasons.
func absent(reasons ...map[string]string) map[string]string {
	all := map[string]string{}
	for _, r := range reasons {
		maps.Copy(all, r)
	}
	return all
}

// codeLogFields is every key the payload of an op the day prices declares, as
// the day's terms for its records were measured: the keys the measured records
// CARRY, and every other one with why the terms' records never carry it — an
// absent key naming a whole subtree where they carry none of it. A KEY IS A
// SHAPE OF RECORD the payload tells apart: one declared since the term was
// measured — a box's push naming its work item's identity, as it was until the
// push term was measured with it — is a record of that op the term was not
// measured on, larger by what the key spells in every record that carries it,
// so TestTheCodeLogsDayPricesEveryRecordItTakes fails on it until the term is
// measured again and restated, or the key is listed here with why the term's
// records never carry it.
//
// The ENVELOPE every record carries is no op's: a barrier is nothing else,
// and internal/code's census test holds what one costs.
var codeLogFields = map[code.OpKind]struct {
	payload reflect.Type
	carried []string
	absent  map[string]string
}{
	code.OpPush: {
		payload: reflect.TypeFor[code.PushBatch](),
		carried: []string{"repo_id", "pushes.push_id", "pushes.principal.kind",
			"pushes.principal.seat", "pushes.principal.run.turn_id",
			"pushes.principal.run.work_backend", "pushes.principal.run.work_key",
			"pushes.principal.run.work_id", "pushes.principal.run.model", "pushes.principal.run.run_id",
			"pushes.principal.run.coding_agent", "pushes.via", "pushes.node", "pushes.commands.ref",
			"pushes.commands.old", "pushes.commands.new", "pushes.commands.result",
			"pushes.pack.pack_id", "pushes.pack.object", "pushes.pack.hash", "pushes.pack.size",
			"pushes.pack.index_bytes", "pushes.pack.reverse_bytes", "pushes.pack.git_checksum",
			"pushes.pack.objects", "pushes.introduced", "pushes.introduced_count",
			"pushes.received_at", "pushes.day"},
		absent: map[string]string{
			"pushes.principal.operator_id": "a person's credential, and a seat acts as itself",
			"pushes.atomic": "--atomic, a client's flag for a push of several refs, and the measured " +
				"push moves one: the box's branch",
			"pushes.commands.forced": "a forced update, and the measured push moves the box's " +
				"branch on from its tip",
			"pushes.commands.reason": "a refused command's code, and the measured push lands",
			"pushes.commands.detail": "an answer's sentence, which no record carries",
			"pushes.change": "the change a push opens from its options (git push -o change): only a push git " +
				"received carries one — never the engine's writes — and only the push that OPENS a change, " +
				"once a change; the measured push, a coding box's moving its branch on, opens none. A push " +
				"that opens one carries what its options spell — a target, a title and a description — in " +
				"place of the code_open_change record the day prices every one of the reference company's " +
				"changes at (codeChangeOpenBytes), and its home then records the change's revisions, " +
				"mergeability and checks as it does an opened one's, which the day prices too",
			"pushes.routing": "whom a push concerns (code.PushEntry.Routing): an entry carries a routing " +
				"only for a change its commands MERGE, by setting the change's target to its head, and for " +
				"the one it OPENS from its options — each the change's id, name, repository, title, work " +
				"item and owner, the pusher as its actor, and the handles its reason reaches, the change's " +
				"authors for a merge (at most code.MaxRoutedParticipants, 64) and the open's mentions (at " +
				"most code.MaxMentions, 32), with which of them are persons — and none where it merges and " +
				"opens nothing, since a head move concerns nobody; the measured push, a coding box's moving " +
				"its branch on, does neither. The reference company's changes are opened by a tool and " +
				"merged by a merge record, which carry their own routings (codeLogDay), held with the " +
				"record's bytes by the push's decide (internal/code's routingRoom)",
			"pushes.landings": "the landings a push makes on a protected branch (code.PushEntry.Landings): " +
				"only a person's own push under direct_push persons moves one — no protection takes a " +
				"seat's direct push — each its command's place and whether it lands green; the measured " +
				"push, a coding box's moving its branch on, moves none. The queue's history it makes is " +
				"swept with an idle repository's queue (codeLogNegligible)",
		},
	},
	code.OpCompact: {
		payload: reflect.TypeFor[code.Compaction](),
		carried: []string{"repo_id", "compaction_id", "node", "seen", "retire", "add.pack_id",
			"add.object", "add.hash", "add.size", "add.index_bytes", "add.reverse_bytes",
			"add.git_checksum", "add.objects", "layer.layer_id", "layer.packs", "layer.parts.object",
			"layer.parts.hash", "layer.parts.size", "layer.midx_bytes", "layer.bitmap_bytes",
			"layer.objects", "layer.commits", "layer.graph_bytes", "layer.git", "layer.took_ms"},
		absent: map[string]string{
			"layer.base_id": "the layer a tail is published over, an object id, and the measured layers " +
				"are their repositories' first: about fifty bytes on a later one",
			"layer.replaces": "the layers a consolidation replaces, which the maintainer folds once a " +
				"chain grows past its bound: an object id each, beside the packs it already names",
			"layer.graph_id": "a commit-graph layer published beside the index, where a repository's " +
				"history asks for one: an object id",
		},
	},
	code.OpRetireSweep: {
		payload: reflect.TypeFor[code.RetireSweep](),
		carried: []string{"repo_id", "through"},
	},
	code.OpChangeOpen: {
		payload: reflect.TypeFor[code.ChangeOpen](),
		carried: []string{"repo_id", "change_id", "source_ref", "target_ref", "head", "title", "body",
			"owner.kind", "owner.seat", "work_item", "turn_id", "routing.change_id", "routing.change_ref",
			"routing.repo_name", "routing.title", "routing.actor", "routing.owner", "routing.owner_kind",
			"routing.work_item"},
		absent: absent(principalAbsent("owner", "kind", "seat"),
			routingAbsent("routing", "change_id", "change_ref", "repo_name", "title", "actor", "owner",
				"owner_kind", "work_item"),
			map[string]string{
				"draft": "a draft's open, and the reference change opens ready",
				"run_id": "the coding run a change is opened for by the engine (internal/engine's run " +
					"changes) rather than by a tool: one UUID more",
				"mentions": "the handles a description mentions (at most code.MaxMentions, 32), and the " +
					"reference text mentions nobody",
			}),
	},
	code.OpReviewers: {
		payload: reflect.TypeFor[code.ReviewerRequest](),
		carried: []string{"repo_id", "change_id", "by.kind", "by.seat", "request.handle", "request.kind",
			"note", "routing.change_id", "routing.change_ref", "routing.repo_name", "routing.title",
			"routing.actor", "routing.owner", "routing.owner_kind", "routing.work_item", "routing.requested"},
		absent: absent(principalAbsent("by", "kind", "seat"),
			routingAbsent("routing", "change_id", "change_ref", "repo_name", "title", "actor", "owner",
				"owner_kind", "work_item", "requested"),
			map[string]string{"withdraw": "an ask withdrawn, and the reference ask stands"}),
	},
	code.OpRevise: {
		payload: reflect.TypeFor[code.Revise](),
		carried: []string{"repo_id", "change_id", "n", "head_oid", "target_ref", "target_oid",
			"target_position", "base_oid", "base_tree", "head_tree", "patch_id", "touched", "node",
			"owners_digest", "routing.change_id", "routing.change_ref", "routing.repo_name", "routing.title",
			"routing.owner", "routing.owner_kind", "routing.work_item"},
		absent: absent(routingAbsent("routing", "change_id", "change_ref", "repo_name", "title", "owner",
			"owner_kind", "work_item"),
			map[string]string{
				"patch_id_omitted": "a patch too large to identify, and the reference revision's is small",
				"touches_all": "a revision touching more paths than code.MaxTouchedBytes holds, which " +
					"names none, and the reference revision touches five",
				"classes": "what kind of change a revision is — whitespace or comments only — set only " +
					"where it is one, and the reference revision changes code",
				"requested_owners": "the path owners a revision asks, and the reference company's " +
					"repositories declare none",
				"asked":          "the reviewers a revision's owners ask, likewise",
				"skipped_owners": "the path owners a revision could not ask, likewise",
				"re_review": "the reviewers a revision after a review asks again — a handle each on the " +
					"revisions that follow a review",
				"lead_fallback": "the lead an unreviewed change falls back to, and the reference change " +
					"asks its reviewer",
			}),
	},
	code.OpMergeability: {
		payload: reflect.TypeFor[code.Mergeability](),
		carried: []string{"repo_id", "change_id", "revision", "head_oid", "target_ref", "target_oid",
			"target_position", "state", "node", "change_version", "readiness_version", "routing.change_id",
			"routing.change_ref", "routing.repo_name", "routing.title", "routing.owner", "routing.owner_kind",
			"routing.work_item"},
		absent: absent(routingAbsent("routing", "change_id", "change_ref", "repo_name", "title", "owner",
			"owner_kind", "work_item"),
			map[string]string{
				"conflicts": "the conflicted paths of a merge that conflicts (at most code.MaxConflictBytes), " +
					"and the reference change merges clean",
				"conflicts_omitted": "a conflict past that bound, likewise",
				"refresh": "a refresh of the stored readiness, a flag on a record that names no merge — " +
					"the same frame less a merge's facts, which the day prices at a merge's",
				"ready":        "the readiness flag, set once a change is ready: a few bytes",
				"waits_person": "a readiness that waits on a person, a flag, and the reference reviews are seats'",
				"checks_position": "the position of the checks a readiness waits on, where a checks file " +
					"defines some: a few dozen bytes on the day's most",
			}),
	},
	code.OpReview: {
		payload: reflect.TypeFor[code.Review](),
		carried: []string{"repo_id", "change_id", "review_id", "by.kind", "by.seat", "revision", "verdict",
			"summary", "findings.id", "findings.path", "findings.side", "findings.line_start",
			"findings.line_end", "findings.commit_oid", "findings.blob_oid", "findings.severity",
			"findings.category", "findings.title", "findings.body", "turn_id", "model", "routing.change_id",
			"routing.change_ref", "routing.repo_name", "routing.title", "routing.actor", "routing.owner",
			"routing.owner_kind", "routing.work_item", "routing.authors", "routing.verdict"},
		absent: absent(principalAbsent("by", "kind", "seat"),
			routingAbsent("routing", "change_id", "change_ref", "repo_name", "title", "actor", "owner",
				"owner_kind", "work_item", "authors", "verdict"),
			map[string]string{
				"findings.suggestion": "a finding's suggested replacement, and the reference findings " +
					"suggest none: at most its finding's own lines again",
				"carries": "the earlier findings a review carries forward, an id each, and the reference " +
					"reviews raise their own",
				"mentions": "the handles a summary mentions, and the reference text mentions nobody",
			}),
	},
	code.OpFindingResolve: {
		payload: reflect.TypeFor[code.FindingResolve](),
		carried: []string{"repo_id", "change_id", "by.kind", "by.seat", "findings.finding_id",
			"findings.resolution", "routing.change_id", "routing.change_ref", "routing.repo_name",
			"routing.title", "routing.actor", "routing.owner", "routing.owner_kind", "routing.work_item",
			"routing.raisers"},
		absent: absent(principalAbsent("by", "kind", "seat"),
			routingAbsent("routing", "change_id", "change_ref", "repo_name", "title", "actor", "owner",
				"owner_kind", "work_item", "raisers"),
			map[string]string{
				"findings.note":   "an answer's note, and the reference answer resolves without one",
				"findings.commit": "the commit an answer names as its fix, an object id where it names one",
			}),
	},
	code.OpComment: {
		payload: reflect.TypeFor[code.Comment](),
		carried: []string{"repo_id", "change_id", "comment_id", "thread_id", "by.kind", "by.seat", "body",
			"turn_id", "routing.change_id", "routing.change_ref", "routing.repo_name", "routing.title",
			"routing.actor", "routing.owner", "routing.owner_kind", "routing.work_item"},
		absent: absent(principalAbsent("by", "kind", "seat"),
			routingAbsent("routing", "change_id", "change_ref", "repo_name", "title", "actor", "owner",
				"owner_kind", "work_item"),
			map[string]string{
				"parent_id":  "a reply's parent, and the reference comments open their threads",
				"finding_id": "the finding a comment discusses, an id where it does",
				"anchor": "a comment on a line of a revision — its revision, path, side, line and blob, " +
					"about a hundred bytes — and the reference comments are on the change",
				"mentions": "the handles a comment mentions, and the reference text mentions nobody",
			}),
	},
	code.OpThreadResolve: {
		payload: reflect.TypeFor[code.ThreadResolve](),
		carried: []string{"repo_id", "change_id", "thread_id", "resolved", "by.kind", "by.seat",
			"routing.change_id", "routing.change_ref", "routing.repo_name", "routing.title", "routing.actor",
			"routing.owner", "routing.owner_kind", "routing.work_item"},
		absent: absent(principalAbsent("by", "kind", "seat"),
			routingAbsent("routing", "change_id", "change_ref", "repo_name", "title", "actor", "owner",
				"owner_kind", "work_item")),
	},
	code.OpMerge: {
		payload: reflect.TypeFor[code.Merge](),
		carried: []string{"repo_id", "merge_id", "change_id", "method", "target_ref", "base_oid", "head_oid",
			"commit_oid", "pack.pack_id", "pack.object", "pack.hash", "pack.size", "pack.index_bytes",
			"pack.reverse_bytes", "pack.git_checksum", "pack.objects", "delete_source", "source_ref", "node",
			"received_at", "principal.kind", "principal.seat", "principal.run.turn_id",
			"principal.run.work_backend", "principal.run.work_key", "principal.run.model", "protected", "green",
			"routing.change_id", "routing.change_ref", "routing.repo_name", "routing.title", "routing.actor",
			"routing.owner", "routing.owner_kind", "routing.work_item", "routing.authors", "routing.merged"},
		absent: absent(principalAbsent("principal", "kind", "seat", "run.turn_id", "run.work_backend",
			"run.work_key", "run.model"),
			routingAbsent("routing", "change_id", "change_ref", "repo_name", "title", "actor", "owner",
				"owner_kind", "work_item", "authors", "merged"),
			map[string]string{"override": "an administrator's override of the gate, and the reference " +
				"merge meets its gate"}),
	},
	code.OpEnqueue: {
		payload: reflect.TypeFor[code.Enqueue](),
		carried: []string{"repo_id", "entry_id", "change_id", "target_ref", "head_oid", "method",
			"delete_source", "principal.kind", "principal.seat"},
		absent: absent(principalAbsent("principal", "kind", "seat"),
			map[string]string{
				"close_task": "a queued change closing its work item as it lands, a flag",
			}),
	},
	code.OpCandidate: {
		payload: reflect.TypeFor[code.Candidate](),
		carried: []string{"repo_id", "target_ref", "train", "base_oid", "candidates.candidate_id",
			"candidates.ordinal", "candidates.entries.entry_id", "candidates.entries.commit",
			"candidates.tip_oid", "pack.pack_id", "pack.object", "pack.hash", "pack.size", "pack.index_bytes",
			"pack.reverse_bytes", "pack.git_checksum", "pack.objects"},
	},
	code.OpLand: {
		payload: reflect.TypeFor[code.Land](),
		carried: []string{"repo_id", "land_id", "target_ref", "base_oid", "tip_oid", "train",
			"entries.entry_id", "entries.change_id", "entries.commit", "entries.delete_source", "node",
			"received_at", "routing.change_id", "routing.change_ref", "routing.repo_name", "routing.title",
			"routing.actor", "routing.owner", "routing.owner_kind", "routing.work_item", "routing.authors",
			"routing.merged"},
		absent: absent(routingAbsent("routing", "change_id", "change_ref", "repo_name", "title", "actor",
			"owner", "owner_kind", "work_item", "authors", "merged"),
			map[string]string{
				"routing.enqueuer": "the change's enqueuer, whom a landing never tells: the queue lands the " +
					"change as them",
			}),
	},
	code.OpCheckQueued: {
		payload: reflect.TypeFor[code.CheckQueued](),
		carried: []string{"repo_id", "oid", "target", "name", "attempt", "purpose", "change_id", "revision",
			"candidate_id", "base", "defs_oid", "def.name", "def.run", "def.timeout_seconds", "required",
			"reason", "queued_at"},
		absent: absent(
			map[string]string{
				"by": "who queued an attempt again by hand, and the reference checks are queued by " +
					"their home",
				"entry_id": "the entry a queue's bisect probe names, which only the bisect of a red " +
					"candidate queues, at the rate of the ejection it ends in (codeLogNegligible)",
				"def.paths": "the paths a check runs for, where its definition names some: a few dozen " +
					"bytes a pattern",
				"def.report":   "the reports a check writes, where its definition names some",
				"def.run_in":   "where a check runs, where its definition says",
				"def.checkout": "how a check checks out its commit, where its definition says",
				"def.env":      "a check's environment, where its definition sets one",
				"skipped":      "a check its paths skip, a flag",
				"invalid":      "a checks file that does not run, its sentence",
			}),
	},
	code.OpGreen: {
		payload: reflect.TypeFor[code.Green](),
		carried: []string{"repo_id", "target_ref", "oid", "through"},
	},
	code.OpCheckDispatched: {
		payload: reflect.TypeFor[code.CheckDispatched](),
		carried: []string{"repo_id", "target", "tip", "tip_position", "defs_oid", "heads.oid",
			"heads.change_id", "heads.revision", "dispatched_at"},
	},
	code.OpCheckStarted: {
		payload: reflect.TypeFor[code.CheckStarted](),
		carried: []string{"repo_id", "oid", "target", "name", "attempt", "runner", "placement", "box",
			"started_at"},
		absent: map[string]string{
			"borrowed": "a check run on a borrowed cache (D-BORROW), a flag",
		},
	},
	code.OpCheckFinished: {
		payload: reflect.TypeFor[code.CheckFinished](),
		carried: []string{"repo_id", "oid", "target", "name", "attempt", "state", "reason", "exit_code",
			"duration_ms", "summary", "finished_at"},
		absent: absent(
			map[string]string{
				"reporter": "who reported an attempt's ending from outside its runner, and the reference " +
					"checks are ended by the runner that ran them",
				"tail":             "the end of a failing check's output, and the reference checks pass",
				"failures":         "the failures a failing check's report names, likewise",
				"failures_omitted": "likewise",
				"log":              "the object a runner stored a check's whole log in, where it stored one: a key and a hash",
				"url":              "a link to a check's run outside the engine, where its reporter gave one",
				"routing": "whom an ending that fails a change's head concerns (code.CheckFinished.Routing): " +
					"the change's routing, its authors and the head's pusher — some hundreds of bytes, as " +
					"a merge's routing is — and the reference checks pass, which concern nobody",
			}),
	},
}

// codeLogKinds is every kind of object the code log's records are on, and how
// its day counts the records on it.
var codeLogKinds = map[code.ObjectKind]string{
	code.KindRepo: "a push (codeLogDay, by the way its pack was built: codeLogPushVias), a " +
		"merge of the pushes' packs, a publication of their layers and the sweep of the rows a merge " +
		"retired (codeLogDay) are on their repository; so are its settings, deletion, restore and purge " +
		"(codeLogNegligible, by its manager: codeLogManagers), a live turn's hold on it, a " +
		"person's purge of its unreachable objects and the expunge's begin and commit that serve " +
		"it (codeLogNegligible), a change's open and its merge (codeLogDay), its other gestures and " +
		"its landing outside a merge (codeLogNegligible), its queue entry and the train and the landing " +
		"its queue worker records (codeLogDay at the most), its entry's dequeue or ejection " +
		"(codeLogNegligible), the green its landing's tip checks move (codeLogDay at the most) and a red " +
		"tip's broken (codeLogNegligible), and an import's chunks and end and a pull mirror's syncs " +
		"(codeLogNegligible)",
	code.KindBarrier: "a git client's read (codeLogDay)",
	code.KindName: "a repository's create and a tool's rename of it, each on the name it " +
		"claims (codeLogNegligible, by its manager: codeLogManagers), and an old name's release " +
		"(codeLogNegligible)",
	code.KindEviction:   "an operator's eviction or readmission (codeLogNegligible)",
	code.KindGeneration: "a reanchor (codeLogNegligible)",
	code.KindCompany:    "the activation fence (codeLogNegligible)",
	code.KindTrimmed:    "the trim's sweep of the anchors (codeLogNegligible)",
	code.KindChange: "a change's conversation — its asks for review, its comments, its threads' " +
		"resolutions, its reviews and the answers to their findings — and its revisions' facts and its " +
		"mergeability, on the change (codeLogDay)",
	code.KindChecks: "a repository's checks: an attempt queued, a head's dispatch, an attempt's claim and " +
		"its box named, and its ending, of a head's checks, of a queue's candidate's checks and of a " +
		"landing's tip checks (codeLogDay at the most), a bisect's probes (codeLogNegligible, with the " +
		"broken or the ejection they name), and the sweep of an idle repository's past the history " +
		"(codeLogNegligible)",
	code.KindPerson: "a person's marks over their own code notices (codeLogNegligible)",
}

// The reference company's chart, as a seat's tools hold it. Its work is filed
// into ENG, which the chart gives to the Engineering unit — a unit with no
// `id`, as the shipped example's are, so its key is its name — and the unit's
// lead is the project's lead, because a project a unit declares is owned by
// that unit's lead ([org.Organization.LeadsBy]). Both are on every routing
// snapshot a seat's write takes, and the unit on every task it files.
const (
	referenceUnit = "Engineering"
	referenceLead = "eng-lead"
)

// referenceChart is [tracker.Leads] for that chart, the seam a seat's tools
// and the engine's writer each hold (engine's liveLeads).
type referenceChart struct{}

func (referenceChart) ProjectLead(string) string { return referenceLead }
func (referenceChart) UnitLead(string) string    { return referenceLead }

// THE FIGURES THE DOCS PRINT: what each gesture of the reference company's
// day costs the mutation log, in bytes, as the tracker's package doc ("What a
// day costs the log, measured") and docs/guides/retention.md's storage
// forecast print them, and as config's and code's arithmetic for the code
// log's share of the broker reads the day they make.
//
// The text the records carry is a STATED ASSUMPTION, not a measurement: how
// long a description or a comment is belongs to the company, and the
// measurement is of everything else a record carries — its ids, its envelope,
// the routing snapshot its notification takes, and what the broker stores
// beside it. Agents write at length, so the corpus takes a description of two
// kibibytes (about three hundred words), a comment of one, and a turn's
// summary at its MaxTurnSummary cap, which the engine rewrites a longer
// account to fit; and half that text is printed beside it, because the text
// is the term a company moves.
//
// THE KEYS ARE A NEW PROJECT'S, one or two digits, which is the one
// identifier here shorter than a company's: a project of the reference
// company's (100 000 tasks a year over 50 projects) reaches four digits in its
// first year and five by its fifth, three bytes more by then wherever a key is
// written — the counter's value, the task's key and its snapshot's on a
// create, the snapshot's on a comment or an edit — about 8 kB of the day, under
// a tenth of one percent, which the docs state rather than this measuring a
// counter ten thousand creates deep.
//
// Each is HELD to what the broker counts within publishedTolerance
// (TestTheMutationLogsCeilingIsFiveYearsOfAMeasuredDay), and every page that
// prints one, or a figure derived from them, to these
// (TestTheDocsPrintTheMeasuredDay): a change to what a record carries fails
// naming the figure to restate rather than leaving the forecast behind its
// code.
const (
	publishedCreateBytes  = 5_266
	publishedCommentBytes = 3_037
	publishedEditBytes    = 1_157
	publishedTurnBytes    = 1_608

	// AT HALF THE TEXT, a 1 KiB description and a 512-byte comment. An edit
	// and a turn carry neither, so they cost what they cost at the whole.
	publishedHalfTextCreateBytes  = 4_242
	publishedHalfTextCommentBytes = 2_436

	// publishedTolerance is how far a measured gesture may sit from the
	// figure printed for it: a hundredth. The measurement is of
	// fixed-length ids and fixed text, so it repeats to the byte, and a
	// hundredth is the most a printed figure may be wrong by — a dozen
	// bytes added to every record already move an edit past it, which is a
	// change the figures are to be restated for.
	publishedTolerance = 0.01
)

// THE BARRIER FIGURES THE DOCS PRINT: what one linearizable read's barrier
// costs the mutation log at the log's first generation, whose envelope carries
// none; from its second generation to its tenth, which is more reanchors than
// a log meets in its life and the figure the forecast prices every barrier at
// (code.BarrierRecordBytes, which the pages say it costs "too"); and at the
// last a generation can be, ten digits a log never reaches. Each is a signed
// frame (ADR-0031) under this suite's two-byte key id, which every record
// carries (code.BarrierRecordBytes says what a longer one adds). Each is HELD to
// what the broker counts within publishedTolerance
// (TestABarrierCostsTheMutationLogWhatTheForecastPricesItAt), and the pages to
// these (TestTheDocsPrintTheMeasuredDay).
const (
	publishedFirstBarrierBytes = 168
	publishedBarrierBytes      = 176
	publishedLastBarrierBytes  = 185
)

// A corpus is the text the reference company's records carry, what each
// gesture costs the log at it, and the published figure each cost is.
type corpus struct {
	name                string
	description, remark int
	cost                gestures
	constant            [4]string
}

// The two corpora the docs print: the forecast's, and half its text.
var (
	referenceCorpus = corpus{
		name: "the reference text", description: 2 << 10, remark: 1 << 10,
		cost: gestures{
			create: publishedCreateBytes, comment: publishedCommentBytes,
			edit: publishedEditBytes, turn: publishedTurnBytes,
		},
		constant: [4]string{"publishedCreateBytes", "publishedCommentBytes",
			"publishedEditBytes", "publishedTurnBytes"},
	}
	halfTextCorpus = corpus{
		name: "half the text", description: 1 << 10, remark: 512,
		cost: gestures{
			create: publishedHalfTextCreateBytes, comment: publishedHalfTextCommentBytes,
			edit: publishedEditBytes, turn: publishedTurnBytes,
		},
		constant: [4]string{"publishedHalfTextCreateBytes", "publishedHalfTextCommentBytes",
			"publishedEditBytes", "publishedTurnBytes"},
	}
)

// gestures is what each gesture of the corpus's day costs the log, in bytes.
type gestures struct{ create, comment, edit, turn float64 }

// day is the bytes of records the reference company's day writes at these
// costs.
func (g gestures) day() float64 {
	return referenceTasksADay*g.create + referenceCommentsADay*g.comment +
		referenceEditsADay*g.edit + referenceTurnsADay*g.turn
}

// THE MUTATION LOG'S CEILING IS FIVE YEARS OF THE REFERENCE COMPANY'S RECORDS,
// MEASURED.
//
// tracker.TrackerLogMaxBytes says a completely blocked trim reaches it in years
// rather than months, and the storage forecast derives the log's healthy size
// from the same day: each claim is a rate of bytes a day times a number of
// days, and the rate was the ceiling's own five years divided back out until
// something measured it. So each gesture of the corpus's day is written here
// the way the engine writes it — a create through the project's counter into
// the unit the chart gave the project, a comment carrying the commenter's
// watch, a status change, a turn charged to its task — each with the
// notification a seat's tools attach, routed through the company's chart (a
// project's lead and its unit's on every snapshot), and the identifiers
// production mints, through the real writer onto a real broker, and the
// stream's own growth is what each costs.
//
// Each gesture must cost what the docs print for it within publishedTolerance,
// at both corpora. And the day the reference text makes must lie within a
// tenth of the rate the ceiling is sized at, either way: past it the ceiling
// fills sooner than its reason says and the forecast understates the log;
// under it the forecast overstates the log, and the code log's share of the
// broker — a ratio to this day (config.DerivedCodeLogNumerator) — is set too
// small.
func TestTheMutationLogsCeilingIsFiveYearsOfAMeasuredDay(t *testing.T) {
	t.Parallel()
	for _, c := range []corpus{referenceCorpus, halfTextCorpus} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			measured := measureADay(t, c)
			for _, g := range []struct {
				measured, printed  float64
				printedAs, printer string
			}{
				{measured.create, c.cost.create, "a task filed", c.constant[0]},
				{measured.comment, c.cost.comment, "a comment", c.constant[1]},
				{measured.edit, c.cost.edit, "an edit", c.constant[2]},
				{measured.turn, c.cost.turn, "a turn charged to its task", c.constant[3]},
			} {
				if math.Abs(g.measured-g.printed) > publishedTolerance*g.printed {
					t.Errorf("at %s, %s costs the mutation log %.0f bytes and the docs print %.0f "+
						"(%s): restate it, and every figure derived from it, where "+
						"TestTheDocsPrintTheMeasuredDay names them", c.name, g.printedAs,
						g.measured, g.printed, g.printer)
				}
			}
			day := measured.day()
			t.Logf("at %s the reference company's day takes %.2f MB of records", c.name, day/1e6)
			if c.name != referenceCorpus.name {
				return
			}
			sized := float64(tracker.TrackerLogMaxBytes) / (5 * 365)
			t.Logf("the ceiling is sized at %.2f MB a day", sized/1e6)
			if day < 0.9*sized || day > 1.1*sized {
				t.Errorf("the reference company's day takes %.2f MB of the mutation log (%.0f bytes a "+
					"create, %.0f a comment, %.0f an edit, %.0f a turn), and TrackerLogMaxBytes is "+
					"sized at five years of %.2f MB: the ceiling's reason, the storage forecast in "+
					"docs/guides/retention.md and the package doc's measured section all derive "+
					"from that day, so re-derive them from this one", day/1e6, measured.create,
					measured.comment, measured.edit, measured.turn, sized/1e6)
			}
		})
	}
}

// measureADay writes each gesture of the corpus's day onto a log of its own
// and answers what each cost, as the stream's own growth — what the broker
// counts against the ceiling — over forty of it.
func measureADay(t *testing.T, c corpus) gestures {
	t.Helper()
	r := newRoundTripWithoutProject(t)
	// THE CHART THE ENGINE APPLIES, so the project's row names the unit a
	// create is filed into, and THE CHART THE WRITER RESOLVES LEADS
	// THROUGH, as the engine's does.
	r.ledBy(referenceChart{})
	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(100), []tracker.ChartProject{
		{Key: "ENG", Name: referenceUnit, Unit: referenceUnit},
	}); err != nil {
		t.Fatalf("apply the chart: %v", err)
	}
	r.drain()
	cost := func(what string, n int, gesture func(i int)) float64 {
		t.Helper()
		before, err := r.log.Stats(t.Context())
		if err != nil {
			t.Fatalf("read the log: %v", err)
		}
		for i := range n {
			gesture(i)
		}
		after, err := r.log.Stats(t.Context())
		if err != nil {
			t.Fatalf("read the log: %v", err)
		}
		each := float64(after.Bytes-before.Bytes) / float64(n)
		t.Logf("%s: %-8s %.0f bytes a gesture, in %d records", c.name, what, each,
			(after.Messages-before.Messages)/uint64(n))
		return each
	}

	// A SEAT'S WRITER, as the seat's tools hold it: the actor is the seat,
	// and the record carries the turn and the delegation chain behind it.
	at := time.Date(2031, 4, 16, 14, 30, 0, 0, time.UTC)
	seat := r.writer.As("backend-dev", tracker.AuthorAgent, tracker.Provenance{
		TurnID: uuid.NewString(), Chain: []string{"cto", referenceLead}})
	chart := referenceChart{}
	const repeats = 40
	tasks := make([]string, repeats)
	read := func(id string) tracker.Task {
		t.Helper()
		detail, err := r.reader.Task(t.Context(), id, tracker.DetailWants{},
			statelog.Freshness{Level: statelog.ReadSession})
		if err != nil {
			t.Fatalf("read task %s: %v", id, err)
		}
		return detail.Task
	}

	description := text("A description an agent wrote of the work to do. ", c.description)
	var out gestures
	out.create = cost("create", repeats, func(i int) {
		// The task's id is a function of the operation, as the seat's
		// create derives it, and it names no unit: the ordinary create,
		// which the writer files into the project's own.
		op := statelog.DeriveOpID(at, "create-work_item", fmt.Sprint(i))
		task := newTask(uuid.NewSHA1(uuid.NameSpaceOID, []byte(op)).String())
		task.Key = ""
		task.Title = "Make the importer resume after a refused push to the node"
		task.Body = description
		task.Reporter, task.Assignee = referenceLead, "backend-dev"
		task.Watchers = []string{referenceLead, "backend-dev"}
		tasks[i] = task.ID
		if _, err := seat.CreateTask(t.Context(), op, task,
			tracker.Wake{Kind: tracker.ChangeCreated, After: task}.Notify(chart)); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		r.drain()
	})
	if filed := read(tasks[0]); filed.FiledUnit != referenceUnit || filed.RoutingUnit != referenceUnit {
		t.Fatalf("the premise: a create into ENG was filed into %q and routed to %q, want %s for "+
			"both, as the chart gives it", filed.FiledUnit, filed.RoutingUnit, referenceUnit)
	}

	body := text("A comment an agent left on the work item, saying what it found. ", c.remark)
	out.comment = cost("comment", repeats, func(i int) {
		before := read(tasks[i])
		c := &tracker.Comment{ID: uuid.NewString(), Task: tasks[i], Author: "backend-dev",
			AuthorKind: tracker.AuthorAgent, Body: body, CreatedAt: at}
		patch := tracker.TaskPatch{Comment: c,
			Watch: &tracker.WatchIntent{Handle: "backend-dev", Watch: true, Auto: true}}
		if _, err := seat.UpdateTask(t.Context(), statelog.DeriveOpID(at, "comment-"+c.ID),
			tasks[i], "ENG", tracker.NoIfMatch, patch, tracker.ChangeComment,
			tracker.Wake{Kind: tracker.ChangeComment, Before: before, After: before,
				Comment: c}.Notify(chart)); err != nil {
			t.Fatalf("comment %d: %v", i, err)
		}
		r.drain()
	})

	out.edit = cost("edit", repeats, func(i int) {
		before := read(tasks[i])
		status := tracker.StatusInProgress
		after := before
		after.Status = status
		if _, err := seat.UpdateTask(t.Context(), statelog.DeriveOpID(at, "update-"+tasks[i]),
			tasks[i], "ENG", tracker.NoIfMatch, tracker.TaskPatch{Status: &status},
			tracker.ChangeStatus, tracker.Wake{Kind: tracker.ChangeStatus, Before: before,
				After: after}.Notify(chart)); err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
		r.drain()
	})

	summary := text("Added the resume path to the importer, with a test of a refused step. ",
		tracker.MaxTurnSummary)
	out.turn = cost("turn", repeats, func(i int) {
		// THE ENGINE'S CHARGE, written as the seat with the turn it charges
		// and no chain, as the engine's own writer for it is built.
		run := uuid.NewString()
		charge := r.writer.As("backend-dev", tracker.AuthorAgent, tracker.Provenance{TurnID: run})
		if _, err := charge.RecordTurn(t.Context(), "turn/"+run+"/dispatch", tracker.TurnRecord{
			Task: tasks[i], Seat: "backend-dev", TurnID: run, Trigger: "work_item",
			Outcome: "done", Phases: []string{"execute", "review"},
			Spend: tracker.TurnSpend{Turns: 1, Rounds: 9, Input: 48_000, Output: 3_200,
				CacheRead: 210_000, CacheWrite: 12_000, WallMs: 95_000},
			Summary: summary,
			Tools: []tracker.TurnTool{{Name: "get_work_item", Calls: 2},
				{Name: "run_sandbox", Calls: 1}, {Name: "search_knowledge", Calls: 2},
				{Name: "comment_on_work_item", Calls: 1}},
		}); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		r.drain()
	})
	return out
}

// A BARRIER COSTS THE MUTATION LOG WHAT THE FORECAST PRICES IT AT.
//
// The storage forecast and config's arithmetic for the code log's share price
// a linearizable read's barrier on this log at code.BarrierRecordBytes, the
// code log's measured figure — a barrier here names a longer subject and
// carries another scope, so it is measured on its own stream at each
// generation the docs print a figure for: the first, whose envelope carries
// none; the second and the tenth, the two ends of the run of one-digit
// generations, which is more reanchors than a log meets in its life; and the
// last a generation can be, whose ten digits grow it by a byte each.
//
// Each must cost what the docs print for it within publishedTolerance, and
// every generation a log meets no more than the forecast prices it at: past
// that, the forecast understates the barriers and every figure derived from
// them, and the census sized from code.BarrierRecordBytes allows more of them
// than the log has room for.
func TestABarrierCostsTheMutationLogWhatTheForecastPricesItAt(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name       string
		generation uint32
		printed    float64
		printer    string
		met        bool
	}{
		{"the first generation", 0, publishedFirstBarrierBytes, "publishedFirstBarrierBytes", true},
		{"the second generation", 1, publishedBarrierBytes, "publishedBarrierBytes", true},
		{"the tenth generation", 9, publishedBarrierBytes, "publishedBarrierBytes", true},
		{"the last generation", math.MaxUint32, publishedLastBarrierBytes, "publishedLastBarrierBytes", false},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			// SIGNED AS EVERY BARRIER A NODE APPENDS IS, since the frame is
			// part of what the log stores and so of what a barrier costs it.
			index, err := statelog.NewReadIndex(tracker.Domain{}, tracker.Domain{}.Stream(),
				r.log, r.log.Members, testSigner(t, tracker.Domain{}), r.reserve, tracker.EncodeBarrier,
				func() uint32 { return arm.generation }, nil)
			if err != nil {
				t.Fatalf("build the read index: %v", err)
			}
			before, err := r.log.Stats(t.Context())
			if err != nil {
				t.Fatalf("read the log: %v", err)
			}
			const barriers = 500
			for range barriers {
				if _, err := index.Read(t.Context()); err != nil {
					t.Fatalf("a linearizable read's barrier: %v", err)
				}
			}
			after, err := r.log.Stats(t.Context())
			if err != nil {
				t.Fatalf("read the log: %v", err)
			}
			if got := after.Messages - before.Messages; got != barriers {
				t.Fatalf("the premise: %d sequential reads appended %d barriers, want one each",
					barriers, got)
			}
			each := float64(after.Bytes-before.Bytes) / barriers
			t.Logf("at %s a barrier grew the mutation log by %.1f bytes", arm.name, each)
			if math.Abs(each-arm.printed) > publishedTolerance*arm.printed {
				t.Errorf("at %s a barrier costs the mutation log %.1f bytes and the docs print "+
					"%.0f (%s): restate it, and every figure derived from it, where "+
					"TestTheDocsPrintTheMeasuredDay names them", arm.name, each, arm.printed,
					arm.printer)
			}
			if arm.met && each > code.BarrierRecordBytes {
				t.Errorf("at %s a barrier costs the mutation log %.1f bytes, and the forecast "+
					"prices every barrier a log meets at %d (code.BarrierRecordBytes): the "+
					"figures derived from it understate this log", arm.name, each,
					code.BarrierRecordBytes)
			}
		})
	}
}

// THE DOCS PRINT THE MEASURED DAY: every page that states what a gesture costs
// the mutation log, or a figure derived from the day those costs make, states
// it from the published figures — so a figure restated for a change in what a
// record carries fails here until every page says it, rather than one page
// moving and four keeping the old day.
//
// The pages are the storage forecast in docs/guides/retention.md, this
// package's doc ("What a day costs the log, measured") and TrackerLogMaxBytes's
// reason, and the reasons that size the code log's share of the broker against
// this day (config's DerivedCodeLogNumerator and its prose in
// docs/guides/replication.md, code's CodeLogMaxBytes and GitReadsShare), the
// count of barriers a day that is the reference company's line for a code log
// at half this log's ceiling among them — and what the code log keeps of the
// reference company's day beside its barriers (internal/code's "What a push
// costs, measured" and the code log's column in docs/concepts/scaling.md's
// table of what every data node holds). Each figure is computed here from the
// published costs, the corpus's rates, the published barriers, the code log's
// day (codeLogDay, every term of it), code.BarrierRecordBytes and
// config.DefaultMinAge, and rounded as the pages round it; the few the pages
// put in words are held to the range the words are true over.
//
// Mutation: change any published figure, a corpus's rate or text,
// MaxTurnSummary, a term of the code log's day, code.BarrierRecordBytes or
// config.DefaultMinAge, and a page no longer says what this package measures.
func TestTheDocsPrintTheMeasuredDay(t *testing.T) {
	t.Parallel()
	whole, half := referenceCorpus.cost, halfTextCorpus.cost
	records, halfRecords := whole.day(), half.day()
	barriers := float64(referenceBarriersADay * code.BarrierRecordBytes)
	seatBarriersAYear := float64(referenceSeatReads*code.BarrierRecordBytes) * 365
	day := records + barriers
	window := config.DefaultMinAge.Hours() / 24
	ceiling := float64(tracker.TrackerLogMaxBytes)
	years := func(capacity, rate float64) float64 { return capacity / rate / 365 }
	keyBytes := referenceKeyGrowth * referenceKeysADay

	// THE CODE LOG'S DAY BESIDE THIS ONE, at its least (no CI, no checks, no
	// queue and no moving target) and its most: every term it prices, summed,
	// and its parts — the pushes and what compacts and sweeps them, the git
	// clients' barriers, and the changes' records.
	terms := codeLogDay()
	pushes := codeLogOp(terms, code.OpPush).most
	compactions := float64(referencePushesADay * codeCompactionBytes)
	sweeps := codeLogOp(terms, code.OpRetireSweep).most
	merges := compactions + sweeps
	publications := codeLogOp(terms, code.OpCompact)
	publications.least -= compactions
	publications.most -= compactions
	readBarriers := codeLogOp(terms, code.OpBarrier)
	gitLeast, gitMost := readBarriers.least, readBarriers.most
	var codeLeastBytes, codeMostBytes, changeLeast, changeMost float64
	for _, term := range terms {
		codeLeastBytes += term.least
		codeMostBytes += term.most
		switch term.op {
		case code.OpPush, code.OpCompact, code.OpRetireSweep, code.OpBarrier:
		default:
			changeLeast += term.least
			changeMost += term.most
		}
	}
	codeLeast, codeMost := codeLeastBytes/day, codeMostBytes/day
	// THE DAY OF A COMPANY WHOSE SEATS ONLY PUSH: every term but the
	// changes', which is the day the share was sized at before the
	// publications and the changes were priced, and where the pages say two
	// thirds still sits.
	pushOnlyLeast := (codeLeastBytes - changeLeast) / day
	pushOnlyMost := (codeMostBytes - changeMost) / day
	share := float64(config.DerivedCodeLogNumerator) / config.DerivedCodeLogDenominator
	// A TENTH OF THE CODE LOG'S CEILING is what its census lets git's barriers
	// hold over min_age ([code.GitReadsShare]).
	tenth := func(ceiling float64) float64 { return ceiling / code.GitReadsShare }
	// A CHANGE'S RECORDS, at the reference company's text, as the pages list
	// them: each one's bytes alone in its record.
	changeFigures := strings.Join([]string{
		fmt.Sprintf("an open %s bytes", spaced(codeChangeOpenBytes)),
		fmt.Sprintf("a review asking for changes with two findings %s", spaced(codeReviewFindingsBytes)),
		fmt.Sprintf("an approving review %s", spaced(codeReviewBytes)),
		fmt.Sprintf("a comment %s", spaced(codeCommentBytes)),
		fmt.Sprintf("a merge %s", spaced(codeMergeBytes)),
		fmt.Sprintf("a revision's facts %s", spaced(codeReviseBytes)),
		fmt.Sprintf("its mergeability %s", spaced(codeMergeabilityBytes)),
	}, ", ")

	// THE BARRIER THE DAY IS PRICED AT IS THE ONE THIS LOG MEASURES: the pages
	// print one figure for both, saying a mutation-log barrier costs
	// code.BarrierRecordBytes "too".
	if publishedBarrierBytes != code.BarrierRecordBytes {
		t.Errorf("the docs price this log's barriers at code.BarrierRecordBytes, %d bytes, and "+
			"say a barrier here costs that too, but one costs %d (publishedBarrierBytes): "+
			"price the day at what this log measures, or reword", code.BarrierRecordBytes,
			publishedBarrierBytes)
	}

	// THE WORDS, each held to the range it is true over.
	for _, w := range []struct {
		words    string
		value    float64
		low, top float64
	}{
		{"nearly four years of the whole day", years(ceiling, day), 3.5, 4},
		{"nearly five years of the records", years(ceiling, records), 4.5, 5},
		{"a log created at 4 GiB fills in about a year", years(4<<30, day), 0.9, 1.2},
		{"at a three-year-old company the ceiling is under a year away", years(ceiling, day) - 3, 0.75, 1},
		{"half of the records are the turns", referenceTurnsADay * whole.turn / records, 0.45, 0.55},
		{"the keys a five-year-old project writes are under a tenth of one percent of the day",
			keyBytes / records, 0, 0.001},
		{"the code log takes about twice this log's day at its least", codeLeast, 1.9, 2.2},
		{"and about seven and a half times it at its most", codeMost, 7.25, 7.75},
		{"two thirds is below that range", share / codeLeast, 0, 1},
		{"so a code log at two thirds of this log's ceiling fills in about a third of this log's " +
			"time at its least", share / codeLeast, 0.29, 0.36},
		{"and in under a tenth of it at its most", share / codeMost, 0, 0.1},
		{"two thirds is at the least of what a company whose seats only push takes", share / pushOnlyLeast,
			0.95, 1.1},
		{"and under the most of it, where a CI polls and every push is published", share / pushOnlyMost,
			0, 1},
		{"the change records alone take more than this log's whole day", changeLeast / day, 1,
			math.Inf(1)},
		{"so a half never suffices for a company whose seats open changes", changeLeast / (day / 2), 1,
			math.Inf(1)},
		{"git's barriers over min_age are under a thirtieth of the tenth of the derived ceiling",
			window * gitMost / tenth(code.CodeLogMaxBytes), 0, 1.0 / 30},
		{"and about a third of the tenth of the 1 GiB floor",
			window * gitMost / tenth(float64(config.CodeLogMaxBytesFloor)), 0.3, 0.36},
	} {
		if w.value < w.low || w.value > w.top {
			t.Errorf("the docs say %s, and the published day makes it %.2f: reword it where "+
				"this test holds it", w.words, w.value)
		}
	}

	mb := func(bytes float64, places int) string { return fmt.Sprintf("%.*f", places, bytes/1e6) }
	count := func(rate float64) string { return spaced(int64(math.Round(rate))) }
	kib := func(n int) string { return fmt.Sprintf("%d KiB", n>>10) }
	tasks, comments := referenceTasksADay*whole.create, referenceCommentsADay*whole.comment
	edits, turns := referenceEditsADay*whole.edit, referenceTurnsADay*whole.turn
	collector := referenceBarriersADay - referenceSeatReads
	// THE BARRIERS' YEAR to the nearest ten megabytes, as the guide rounds a
	// figure it calls "about".
	barrierYear := math.Round(seatBarriersAYear/1e7) * 10
	// THE CODE LOG'S GIT CENSUS at its floor and the default min_age, to the
	// nearest thousand, as the guide rounds it: the term it prices at
	// code.BarrierRecordBytes, so a barrier restated moves the census the
	// guide prints.
	gitCensus := spaced(int64(math.Round(float64(code.GitReadsPerDay(config.CodeLogMaxBytesFloor,
		config.DefaultMinAge))/1e3) * 1e3))

	for file, wants := range map[string][]string{
		"docs/guides/retention.md": {
			fmt.Sprintf("holds **about %s MB**", mb(window*day, 0)),
			fmt.Sprintf("seven days, `min_age`'s default, of the %s MB a day the reference company "+
				"writes to it", mb(day, 1)),
			fmt.Sprintf("The day is %s MB of records and %s MB of read barriers", mb(records, 1),
				mb(barriers, 1)),
			fmt.Sprintf("| %s tasks filed, at a %s description | %s bytes, in two records | %s MB |",
				count(referenceTasksADay), kib(referenceCorpus.description),
				spaced(int64(whole.create)), mb(tasks, 2)),
			fmt.Sprintf("| %s comments, at %s | %s bytes | %s MB |", count(referenceCommentsADay),
				kib(referenceCorpus.remark), spaced(int64(whole.comment)), mb(comments, 2)),
			fmt.Sprintf("| %s edits, each a status change | %s bytes | %s MB |",
				count(referenceEditsADay), spaced(int64(whole.edit)), mb(edits, 2)),
			fmt.Sprintf("| %s turns charged to a task, a summary at its %d-byte cap | %s bytes | %s MB |",
				count(referenceTurnsADay), tracker.MaxTurnSummary, spaced(int64(whole.turn)),
				mb(turns, 2)),
			fmt.Sprintf("| %s read barriers: the seats' %s and the object collector's %d | %d bytes | %s MB |",
				spaced(referenceBarriersADay), spaced(referenceSeatReads), collector,
				code.BarrierRecordBytes, mb(barriers, 2)),
			fmt.Sprintf("with half of it — a %s description, a %d-byte comment — the records come to %s MB",
				kib(halfTextCorpus.description), halfTextCorpus.remark, mb(halfRecords, 1)),
			fmt.Sprintf("about %.0f kB of the day, under a tenth of one percent", keyBytes/1e3),
			fmt.Sprintf("The read barriers add about **%.0f MB a year** to the log's throughput — %s a "+
				"day at %d bytes, %.0f %% of what the log takes", barrierYear,
				spaced(referenceSeatReads), code.BarrierRecordBytes, 100*barriers/day),
			fmt.Sprintf("at %d bytes a barrier — the code log's `code.BarrierRecordBytes`, which a "+
				"mutation-log barrier costs too from the log's second generation to its tenth (%d "+
				"bytes at its first)", publishedBarrierBytes, publishedFirstBarrierBytes),
			fmt.Sprintf("On a fleet that trims they are about %s MB of the %s.", mb(window*barriers, 0),
				mb(window*day, 0)),
			fmt.Sprintf("the log keeps the whole day, about %.1f GB a year, so a log created at 16 GiB "+
				"fills in nearly four years — nearly five on its records alone — and one "+
				"created at 4 GiB in about a year", day*365/1e9),
			fmt.Sprintf("at %d bytes a barrier in its signed frame (about %s a day at the 1 GiB floor "+
				"and the default seven days)", code.BarrierRecordBytes, gitCensus),
		},
		"internal/tracker/subject.go": {
			fmt.Sprintf("a task filed: %s bytes, in two records", spaced(int64(whole.create))),
			fmt.Sprintf("at a %s description", kib(referenceCorpus.description)),
			fmt.Sprintf("a comment: %s bytes at %s", spaced(int64(whole.comment)),
				kib(referenceCorpus.remark)),
			fmt.Sprintf("an edit: %s bytes for a status change", spaced(int64(whole.edit))),
			fmt.Sprintf("a turn charged to its task: %s bytes with its summary at [MaxTurnSummary]",
				spaced(int64(whole.turn))),
			fmt.Sprintf("So the day is %s MB of records — %s MB of tasks, %s of comments, %s of edits "+
				"and %s of turns: half of it is the turns", mb(records, 1), mb(tasks, 2),
				mb(comments, 2), mb(edits, 2), mb(turns, 2)),
			fmt.Sprintf("(a %s description and a %d-byte comment make it %s MB)",
				kib(halfTextCorpus.description), halfTextCorpus.remark, mb(halfRecords, 1)),
			fmt.Sprintf("written, about %.0f kB of the day.", keyBytes/1e3),
			fmt.Sprintf("the seats' %s linearizable reads and the object collector's %d append %s MB of "+
				"barriers, at %d bytes each from a log's second generation to its tenth (%d at its "+
				"first;", spaced(referenceSeatReads), collector, mb(barriers, 1),
				publishedBarrierBytes, publishedFirstBarrierBytes),
			fmt.Sprintf("%s MB a day in all, which the trim's seven-day min_age holds about %s MB of",
				mb(day, 1), mb(window*day, 0)),
			"[TrackerLogMaxBytes] is nearly five years of the records, and nearly four " +
				"years of the whole day, barriers included.",
		},
		"internal/tracker/domain.go": {
			fmt.Sprintf("its day takes %s MB of this log", mb(records, 1)),
			fmt.Sprintf("which fills it in %.1f years", years(ceiling, records)),
			fmt.Sprintf("Its seats' linearizable reads add %s MB a day of barriers, so a completely "+
				"blocked trim reaches it in nearly four years", mb(barriers, 1)),
			"at a three-year-old company whose trim never ran the ceiling is under a year away",
		},
		"internal/config/retention.go": {
			fmt.Sprintf("the mutation log about %s MB a day: %s MB of records", mb(day, 1),
				mb(records, 1)),
			fmt.Sprintf("and %s MB of barriers, its seats' %s reads and the object collector's %d",
				mb(barriers, 1), spaced(referenceSeatReads), collector),
			fmt.Sprintf("the code log %s–%s MB a day: %s pushes at %s bytes",
				mb(codeLeastBytes, 1), mb(codeMostBytes, 1), spaced(referencePushesADay),
				spaced(codePushRecordBytes)),
			fmt.Sprintf("push=box arm beside a person's; %s where the engine builds the push for a tool",
				spaced(codeEnginePushRecordBytes)),
			fmt.Sprintf("%s MB; the compactions that merge those pushes' packs, %s MB at %d bytes a push, "+
				"and the code-retention duty's sweeps of what they retired, %s MB at %d", mb(pushes, 1),
				mb(compactions, 2), codeCompactionBytes, mb(sweeps, 2), codeSweepBytes),
			fmt.Sprintf("the publications of their layers, %s–%s MB", mb(publications.least, 2),
				mb(publications.most, 2)),
			fmt.Sprintf("the barriers of its git clients, %s–%s MB", mb(gitLeast, 1), mb(gitMost, 1)),
			fmt.Sprintf("so a seat's push three times, %s a day, beside thirty people fetching every "+
				"three minutes of a working day (%s) and, where one runs, a CI polling ten "+
				"repositories a minute (%s)", spaced(referencePushReads),
				spaced(referencePeopleReads), spaced(referenceCIReads)),
			fmt.Sprintf("and the records of its %s changes a day, %s–%s MB", spaced(referenceChangesADay),
				mb(changeLeast, 1), mb(changeMost, 1)),
			fmt.Sprintf("So the code log takes %.2f–%.2f of the mutation log's day — %.2f on its push "+
				"records alone (%.2f of the tracker's records)", codeLeast, codeMost, pushes/day,
				pushes/records),
			fmt.Sprintf("fills in %.2f of the mutation log's time at the least and %.2f at the most",
				share/codeLeast, share/codeMost),
			fmt.Sprintf("Even a company whose seats only push takes %.2f–%.2f of the mutation log's day "+
				"here", pushOnlyLeast, pushOnlyMost),
			fmt.Sprintf("two thirds is at the least of that range: such a code log fills in %.2f of the "+
				"mutation log's time at the least and %.2f at the most", share/pushOnlyLeast,
				share/pushOnlyMost),
			fmt.Sprintf("the changes' records alone, %s MB a day at their least, are more than the "+
				"mutation log's whole day", mb(changeLeast, 1)),
		},
		"internal/config/bootstrap.go": {
			"two thirds is the least of what a company whose seats only push writes here, and BELOW " +
				"what a company whose seats work through changes writes",
			"take about twice to about seven and a half times what its tracker writes and its seats' " +
				"reads take a day",
		},
		"internal/code/limits.go": {
			fmt.Sprintf("against the tracker's measured %s MB of records and %s MB of barriers",
				mb(records, 1), mb(barriers, 1)),
			fmt.Sprintf("%s pushes at the MEASURED %s bytes a seat's coding box's push alone in its "+
				"record (BenchmarkPushLogBytes), %s MB, the compactions that merge their packs and the "+
				"sweeps of what those retired at the MEASURED %d and %d bytes a push "+
				"(internal/code/compact's steady arm at the floor), %s MB, the publications of their "+
				"layers, %s–%s MB, its git clients' barriers at BarrierRecordBytes, %s–%s MB, and its "+
				"changes' records, %s–%s MB", spaced(referencePushesADay), spaced(codePushRecordBytes),
				mb(pushes, 1), codeCompactionBytes, codeSweepBytes, mb(merges, 1),
				mb(publications.least, 2), mb(publications.most, 2), mb(gitLeast, 1), mb(gitMost, 1),
				mb(changeLeast, 1), mb(changeMost, 1)),
			fmt.Sprintf("git barriers beside its pushes: %s–%s MB a day, %.0f–%.0f MB over the default "+
				"seven days — under a thirtieth of the tenth of a derived 10⅔ GiB ceiling, and "+
				"about a third of the tenth of even the 1 GiB floor", mb(gitLeast, 1), mb(gitMost, 1),
				window*gitLeast/1e6, window*gitMost/1e6),
		},
		"docs/guides/replication.md": {
			fmt.Sprintf("a company's push records (%s bytes a seat's push from its coding box, measured), "+
				"the records that compact their packs and sweep what each compaction retired (%d bytes "+
				"a push, measured) and publish their layers, the barriers its git clients' reads append "+
				"(%d bytes a read)", spaced(codePushRecordBytes), codeCompactionBytes+codeSweepBytes,
				code.BarrierRecordBytes),
			"take about twice to about seven and a half times what its tracker writes and its seats' " +
				"reads take a day",
			fmt.Sprintf("priced as its coding box's, %s bytes alone in its record, and %s", spaced(
				codePushRecordBytes), spaced(referenceChangesADay)),
			fmt.Sprintf("the code log takes %s–%s MB a day against the %s MB its tracker writes and its "+
				"seats' reads take", mb(codeLeastBytes, 1), mb(codeMostBytes, 1), mb(day, 1)),
			fmt.Sprintf("Its pushes are %s MB of that, the records that "+
				"[compact](../concepts/code-host.md#compaction) their packs, publish their layers and "+
				"sweep what each compaction retired %s–%s MB, its git clients' barriers %s–%s MB, and its "+
				"changes' records %s–%s MB", mb(pushes, 1), mb(merges+publications.least, 1),
				mb(merges+publications.most, 1), mb(gitLeast, 1), mb(gitMost, 1), mb(changeLeast, 1),
				mb(changeMost, 1)),
			fmt.Sprintf("at their measured sizes (%s)", changeFigures),
			fmt.Sprintf("(a train %s bytes, a candidate's check queued %s, a landing %s)",
				spaced(codeCandidateBytes), spaced(codeCandidateCheckQueuedBytes), spaced(codeLandBytes)),
			"in about a third of the mutation log's time where its seats open changes, and in under a " +
				"tenth where its checks run and its targets move",
			fmt.Sprintf("the changes' records alone, %s MB a day at their least, are more than the "+
				"mutation log's whole day", mb(changeLeast, 1)),
		},
		"internal/code/doc.go": {
			fmt.Sprintf("so %s bytes alone in its record", spaced(codePushRecordBytes)),
			fmt.Sprintf("%d bytes where a merge takes the floor's sixteen", codeCompactionBytes),
			fmt.Sprintf("%d bytes a push where every merge's rows are swept apart", codeSweepBytes),
			fmt.Sprintf("hold about %.0f MB of it over the default seven-day min_age, and the records "+
				"that merge their packs and sweep what those merges retired at most about %.1f MB "+
				"beside them — about %.0f MB", window*pushes/1e6, window*merges/1e6,
				window*(pushes+merges)/1e6),
		},
		"docs/concepts/scaling.md": {
			fmt.Sprintf("takes %s bytes for a seat's coding box's push in a record of its own (%s where "+
				"the engine builds it for a tool)", spaced(codePushRecordBytes),
				spaced(codeEnginePushRecordBytes)),
			fmt.Sprintf("43 to %d bytes a push more", codeCompactionBytes),
			fmt.Sprintf("and the sweeps that remove the rows a merge retired, a day later, up to %d more",
				codeSweepBytes),
			fmt.Sprintf("| about %.0f MB |", window*(pushes+merges)/1e6),
		},
	} {
		page := prose(t, file)
		for _, want := range wants {
			if !strings.Contains(page, want) {
				t.Errorf("%s does not say %q", file, want)
			}
		}
	}
}

// THE CODE LOG'S DAY PRICES EVERY RECORD THE LOG TAKES, or says why one is not
// in it.
//
// The day sizes the code log's share (config's DerivedCodeLogNumerator) and
// says where that share sits against what the log takes. A kind of record
// the log takes and the day leaves out makes it quietly short, and no figure
// TestTheDocsPrintTheMeasuredDay reads moves when it joins the log: the
// change, review, merge, queue and check records this build's tools, its home
// git nodes and its check runners append would come on top of a day that has
// no room for them — as they did until they were priced here.
//
// So every op the code domain declares is exactly one of three: priced by the
// day (codeLogDay, a term for each shape of its records it prices),
// negligible beside it for the reason given (codeLogNegligible), or appended
// by no node of this build (codeLogUnwritten) — and that only while no file
// the crewlet binary is built from names the listing its loop would poll or
// the payload its asker would build. Every kind of object the log's records
// are on says how the day counts them (codeLogKinds).
//
// AN OP IS CLASSIFIED ONCE, AND ITS RECORDS CHANGE SHAPE UNDER IT: a later
// writer appends the same op's records at a size or a rate its term was not
// measured on, or its reason does not describe, and nothing above moves. So
// the shapes are held too, each by what the code itself tells it apart by:
// every way a push's pack is built and everything that may manage a
// repository, each closed set classified whole (codeLogPushVias,
// codeLogManagers); and every key a priced op's payload declares, against the
// keys its terms were measured on (codeLogFields).
//
// A new op, kind, way, manager or payload key, or the writer of an unwritten
// op, fails here until its records are priced into the day — and the share's
// place restated on the pages TestTheDocsPrintTheMeasuredDay holds — or argued
// out of it. The one negligible reason that prints a bound on a measured
// figure, a change's edits, is held to it here.
//
// Mutation: declare an op, a kind, a way or a manager in the code domain, add
// a key to a priced op's payload, or drop the land's term from codeLogDay, and
// this fails naming it; take the compactor's poll off code.ReadCompactionDue
// and it fails saying the walk found no writer.
func TestTheCodeLogsDayPricesEveryRecordItTakes(t *testing.T) {
	t.Parallel()
	const remedy = "price its records into codeLogDay — and restate the code log's share on the " +
		"pages TestTheDocsPrintTheMeasuredDay holds — or say in codeLogNegligible why they are no " +
		"rate beside the day"
	const shapeRemedy = "price them into codeLogDay as a term of their own beside their op's, " +
		"measured — and restate the code log's share on the pages TestTheDocsPrintTheMeasuredDay " +
		"holds — or argue them out of the day with the measured figure that keeps them out; then say " +
		"so where this test reads it"
	day := codeLogDay()
	// A CHANGE'S EDITS stay out of the day on a bound: every one of the
	// reference company's changes edited once, its whole description
	// rewritten, appends under a twentieth of the day's least.
	var least float64
	for _, term := range day {
		least += term.least
	}
	if edits := float64(referenceChangesADay * codeChangeEditBytes); edits >= least/20 {
		t.Errorf("codeLogNegligible leaves a change's edits out of the day on a bound of a twentieth of "+
			"its least, and an edit a change at %d bytes (codeChangeEditBytes) appends %.2f MB a day "+
			"against a least of %.1f MB: price them into codeLogDay, or restate the bound",
			codeChangeEditBytes, edits/1e6, least/1e6)
	}
	// A RED TIP'S BROKEN stays out of the day on the same bound: every one of
	// the reference company's changes' landings broken once appends under a
	// twentieth of the day's least.
	if brokens := float64(referenceChangesADay * codeBrokenBytes); brokens >= least/20 {
		t.Errorf("codeLogNegligible leaves a red tip's broken out of the day on a bound of a twentieth "+
			"of its least, and a broken a change at %d bytes (codeBrokenBytes) appends %.2f MB a day "+
			"against a least of %.1f MB: price them into codeLogDay, or restate the bound",
			codeBrokenBytes, brokens/1e6, least/1e6)
	}
	// A CHANGE'S RECORDS ARE PRICED AT WHAT internal/code MEASURES THEM AT:
	// its changeRecordBytes is the figure each record is held to there, and
	// this package cannot import a test file's map, so it reads the map's
	// lines. A record restated there and not here is a day the measurement no
	// longer backs.
	measured := prose(t, "internal/code/changelog_test.go")
	for name, bytes := range map[string]int64{
		"open": codeChangeOpenBytes, "edit": codeChangeEditBytes, "reviewers": codeReviewersBytes,
		"revise": codeReviseBytes, "mergeability": codeMergeabilityBytes,
		"review_findings": codeReviewFindingsBytes, "review": codeReviewBytes, "answer": codeAnswerBytes,
		"comment": codeCommentBytes, "thread_resolve": codeThreadResolveBytes, "merge": codeMergeBytes,
		"enqueue": codeEnqueueBytes, "check_queued": codeCheckQueuedBytes,
		"check_dispatched": codeCheckDispatchedBytes, "check_started": codeCheckStartedBytes,
		"check_named": codeCheckNamedBytes, "check_finished": codeCheckFinishedBytes,
		"publication_tail": codePublicationTailBytes, "publication_one": codePublicationOneBytes,
		"tip_check_queued": codeTipCheckQueuedBytes, "green": codeGreenBytes, "broken": codeBrokenBytes,
		"candidate": codeCandidateBytes, "candidate_check_queued": codeCandidateCheckQueuedBytes,
		"land": codeLandBytes,
	} {
		line := fmt.Sprintf("%q: %s,", name, strings.ReplaceAll(spaced(bytes), " ", "_"))
		if !strings.Contains(measured, line) {
			t.Errorf("the day prices a change's %s at %d bytes, and internal/code's changeRecordBytes, "+
				"which holds that record to what the log takes, has no line %q: state the figure it "+
				"measures in both, and restate the pages TestTheDocsPrintTheMeasuredDay holds",
				name, bytes, line)
		}
	}
	// AN OP IS PRICED BY ITS SHAPES' TERMS, each one shape of its records, once.
	priced := map[code.OpKind]bool{}
	shapes := map[[2]string]bool{}
	for _, term := range day {
		priced[term.op] = true
		key := [2]string{string(term.op), term.shape}
		switch {
		case strings.TrimSpace(term.shape) == "":
			t.Errorf("codeLogDay prices %q records without saying which: a term is the shape of "+
				"its op's records its figure was measured on", term.op)
		case shapes[key]:
			t.Errorf("codeLogDay prices %q records of the shape %q twice", term.op, term.shape)
		}
		shapes[key] = true
	}
	for _, op := range code.OpKinds {
		var in []string
		if priced[op] {
			in = append(in, "codeLogDay")
		}
		if _, found := codeLogNegligible[op]; found {
			in = append(in, "codeLogNegligible")
		}
		if _, found := codeLogUnwritten[op]; found {
			in = append(in, "codeLogUnwritten")
		}
		switch len(in) {
		case 0:
			t.Errorf("the code log takes %q records, which its day neither prices nor leaves out "+
				"for a reason: %s", op, remedy)
		case 1:
		default:
			t.Errorf("%q is in %s: an op is priced, negligible or unwritten, one of the three",
				op, strings.Join(in, " and "))
		}
	}
	for _, listed := range []struct {
		name string
		ops  []code.OpKind
	}{
		{"codeLogDay", slices.Collect(maps.Keys(priced))},
		{"codeLogNegligible", slices.Collect(maps.Keys(codeLogNegligible))},
		{"codeLogUnwritten", slices.Collect(maps.Keys(codeLogUnwritten))},
		{"codeLogFields", slices.Collect(maps.Keys(codeLogFields))},
	} {
		for _, op := range listed.ops {
			if !op.Valid() {
				t.Errorf("%s names %q, which is no op the code domain declares", listed.name, op)
			}
		}
	}
	for op, why := range codeLogNegligible {
		if strings.TrimSpace(why) == "" {
			t.Errorf("codeLogNegligible leaves %q out of the day and says nothing of why", op)
		}
	}

	// THE WRITERS, each by the name it spells — a loop by the listing it polls,
	// an asker by the payload it builds: an unwritten op or shape stays
	// unwritten while no file the crewlet binary is built from spells its
	// writer's name. The walk that looks must find the compactor's listing,
	// whose merges the day prices, or it is a walk that would find no writer
	// at all.
	var payloads []reflect.Type
	for op, unwritten := range codeLogUnwritten {
		if unwritten.payload == nil {
			continue
		}
		if decoded := codeLogPayload(op); unwritten.payload != decoded {
			t.Errorf("codeLogUnwritten holds %q records unwritten until their asker builds %v, "+
				"and the code domain decodes them into %v: name the payload the op's records "+
				"carry, which is what its asker builds", op, unwritten.payload, decoded)
			continue
		}
		payloads = append(payloads, unwritten.payload)
	}
	polled, built := codeLogSpelled(t, payloads)
	if !polled[code.ReadCompactionDue] {
		t.Errorf("no file the crewlet binary is built from names the compactor's listing (%q), "+
			"which its poll reads for the merges codeLogDay prices: either the compactor is gone, "+
			"and its term with it, or codeLogSpelled reads no writer and would miss every one this "+
			"test holds unwritten", code.ReadCompactionDue)
	}
	for op, unwritten := range codeLogUnwritten {
		switch {
		case (unwritten.listing == "") == (unwritten.payload == nil) ||
			strings.TrimSpace(unwritten.how) == "":
			t.Errorf("codeLogUnwritten holds %q records unwritten on the listing %q and the "+
				"payload %v: an unwritten op names the one thing its writer spells — the listing "+
				"its loop polls or the payload its asker builds — and says how its records would "+
				"be appended", op, unwritten.listing, unwritten.payload)
		case unwritten.payload == nil && !unwritten.listing.Valid():
			t.Errorf("codeLogUnwritten holds %q records unwritten on the listing %q, which is no "+
				"listing the code domain declares", op, unwritten.listing)
		case polled[unwritten.listing]:
			t.Errorf("a file the crewlet binary is built from names the listing %q, which the "+
				"loop that appends %q records polls, so a node appends them — %s: %s",
				unwritten.listing, op, unwritten.how, remedy)
		case built[unwritten.payload]:
			t.Errorf("a file the crewlet binary is built from names %v, the payload the asker "+
				"that appends %q records builds, so a node appends them — %s: %s",
				unwritten.payload, op, unwritten.how, remedy)
		}
	}
	// THE WAYS AND THE MANAGERS, each closed set classified whole.
	classifiedWhole(t, "codeLogPushVias", "way of building a push's pack", code.PushVias(),
		codeLogPushVias, shapeRemedy)
	classifiedWhole(t, "codeLogManagers", "manager of a repository", code.ManagedBys(),
		codeLogManagers, shapeRemedy)

	// THE KEYS of every priced op's payload, against those its term was
	// measured on. A barrier carries none: its record is the envelope alone.
	for op := range priced {
		if _, listed := codeLogFields[op]; !listed && op != code.OpBarrier {
			t.Errorf("codeLogDay prices %q records, and codeLogFields does not list the keys of "+
				"their payload its term was measured on", op)
		}
	}
	for op, fields := range codeLogFields {
		if !priced[op] {
			t.Errorf("codeLogFields lists the keys of %q records, which the day does not price", op)
		}
		declared := payloadKeys(t, fields.payload)
		carried := map[string]bool{}
		for _, key := range fields.carried {
			switch {
			case carried[key]:
				t.Errorf("codeLogFields lists %q as carried by %q records twice", key, op)
			case !slices.Contains(declared, key):
				t.Errorf("codeLogFields lists %q as carried by %q records, and their payload "+
					"declares no such key: the term was measured on records this build no "+
					"longer writes — measure it again and restate the figures", key, op)
			}
			carried[key] = true
		}
		for key, why := range fields.absent {
			switch {
			case carried[key]:
				t.Errorf("codeLogFields lists %q as both carried by %q records and absent from "+
					"them", key, op)
			case strings.TrimSpace(why) == "":
				t.Errorf("codeLogFields lists %q as absent from %q records and says nothing of "+
					"why", key, op)
			case !slices.ContainsFunc(declared, func(d string) bool { return under(d, key) }):
				t.Errorf("codeLogFields lists %q as absent from %q records, and their payload "+
					"declares no such key", key, op)
			}
		}
		for _, key := range declared {
			absent := slices.ContainsFunc(slices.Collect(maps.Keys(fields.absent)),
				func(a string) bool { return under(key, a) })
			if !carried[key] && !absent {
				t.Errorf("%q records declare the key %q, which their term in the day was not "+
					"measured with: measure the term again with records that carry it and restate "+
					"the figures — and the code log's share and when a half suffices on the pages "+
					"TestTheDocsPrintTheMeasuredDay holds — then list it in codeLogFields as "+
					"carried, or list it there as absent with why the term's records never carry it",
					op, key)
			}
		}
	}

	for _, kind := range code.ObjectKinds {
		if strings.TrimSpace(codeLogKinds[kind]) == "" {
			t.Errorf("the code log's records are on %q objects, and codeLogKinds does not say how "+
				"its day counts them: %s", kind, remedy)
		}
	}
	for kind := range codeLogKinds {
		if !kind.Valid() {
			t.Errorf("codeLogKinds names %q, which is no kind of object the code domain declares", kind)
		}
	}
}

// THE HOLD'S REASON WEIGHS EVERY TURN'S HOLD AGAINST THE DAY. A hold record is
// no deliberate act but a turn's — one for each turn that names a scratch
// repository idle code.HoldRecordAfter or more, recurring in every turn while
// neither a read nor a hold moves the idle clock — so codeLogNegligible keeps
// it out of the day only on a stated bound: what the reference company's
// 3 000 turns a day would append, each naming one such repository, at the most
// a hold record takes as the engine writes one (codeHoldRecordBytes), beside
// the day's least. This holds the figures the reason prints to that
// arithmetic, so a restated frame or day re-opens the judgement rather than
// leaving a figure nothing computes.
//
// Mutation: change codeHoldRecordBytes, referenceTurnsADay or a term of the
// day — the reason no longer prints what they make, red.
func TestTheHoldsReasonWeighsEveryTurnsHoldAgainstTheDay(t *testing.T) {
	t.Parallel()
	var least float64
	for _, term := range codeLogDay() {
		least += term.least
	}
	holds := referenceTurnsADay * codeHoldRecordBytes
	reason := strings.Join(strings.Fields(codeLogNegligible[code.OpHold]), " ")
	for _, want := range []string{
		fmt.Sprintf("were each of its %s turns a day to name one", spaced(referenceTurnsADay)),
		fmt.Sprintf("(%s bytes, codeHoldRecordBytes)", spaced(codeHoldRecordBytes)),
		fmt.Sprintf("append %.2f MB a day, %.0f %% of the day's least of %.1f MB", holds/1e6,
			100*holds/least, least/1e6),
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("codeLogNegligible's reason for %q records does not say %q, which is what the "+
				"reference company's turns, a hold record's measured frame and the code log's day make "+
				"it: restate the figures, and judge again whether the hold stays out of the day",
				code.OpHold, want)
		}
	}
}

// classifiedWhole fails t for every member of one of the code domain's closed
// sets that classified does not say how the code log's day counts the records
// of, and for every member classified names that the set does not hold.
func classifiedWhole[K ~string](t *testing.T, name, what string, set []K, classified map[K]string,
	remedy string) {
	t.Helper()
	for _, member := range set {
		if strings.TrimSpace(classified[member]) == "" {
			t.Errorf("the code domain declares the %s %q, and %s does not say how the code log's "+
				"day counts its records: %s", what, member, name, remedy)
		}
	}
	for member := range classified {
		if !slices.Contains(set, member) {
			t.Errorf("%s names %q, which is no %s the code domain declares", name, member, what)
		}
	}
}

// under reports whether key is the key parent or one nested in it.
func under(key, parent string) bool {
	return key == parent || strings.HasPrefix(key, parent+".")
}

// payloadKeys is every key a payload of type rt declares, as its encoding
// spells it, a nested key after its parent's and a dot: a struct the code
// domain declares is walked into — through a pointer or a slice, and inlined
// where the encoder inlines it — and every other value, a string, a number,
// an instant, another package's type or anything that encodes itself, is one
// key. Read off the type and not a record, so a key a record leaves out
// (omitempty) is a key all the same: what the payload can carry, not what one
// sample did.
func payloadKeys(t *testing.T, rt reflect.Type) []string {
	t.Helper()
	domain := reflect.TypeFor[code.PushBatch]().PkgPath()
	var keys []string
	var walk func(prefix string, rt reflect.Type)
	walk = func(prefix string, rt reflect.Type) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || rt.PkgPath() != domain || encodesItself(rt) {
			keys = append(keys, prefix)
			return
		}
		for i := range rt.NumField() {
			field := rt.Field(i)
			tagged, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			switch {
			case tagged == "-":
			case field.Anonymous && tagged == "" && embedded.Kind() == reflect.Struct:
				walk(prefix, field.Type)
			case !field.IsExported():
			default:
				walk(strings.TrimPrefix(prefix+"."+cmp.Or(tagged, field.Name), "."), field.Type)
			}
		}
	}
	walk("", rt)
	if len(keys) == 0 || slices.Contains(keys, "") {
		t.Fatalf("%s is no payload of keys: it encodes as one value", rt)
	}
	return keys
}

// encodesItself reports a type that writes its own encoding, which is one key
// whatever its fields are.
func encodesItself(rt reflect.Type) bool {
	for _, encoder := range []reflect.Type{reflect.TypeFor[json.Marshaler](),
		reflect.TypeFor[encoding.TextMarshaler]()} {
		if rt.Implements(encoder) || reflect.PointerTo(rt).Implements(encoder) {
			return true
		}
	}
	return false
}

// codeLogSpelled is what the code domain's writers spell that a file the
// crewlet binary is built from names: every listing of the domain (a
// code.ReadKind), which a loop polls, and each of payloads, which an asker
// builds. A file is a non-test file, read as go/build reads it for this
// platform, of a package cmd/crewlet reaches through its imports — so no test,
// and no package only tests import (the suites' harnesses, codetest among
// them, poll a listing and build a payload to check them) — outside the code
// domain's own package, which declares and serves them. Each is found by the
// domain's own identifier for it, read off its declaration rather than spelled
// here — a listing's constant, a payload's type — so one renamed is the one
// the walk looks for.
func codeLogSpelled(t *testing.T, payloads []reflect.Type) (polled map[code.ReadKind]bool,
	built map[reflect.Type]bool) {
	t.Helper()
	root := sourcetree.Root(t)
	domain := reflect.TypeFor[code.ReadKind]().PkgPath()
	module, found := strings.CutSuffix(domain, "/internal/code")
	if !found {
		t.Fatalf("the code domain's import path is %s, not its module's internal/code", domain)
	}
	fset := token.NewFileSet()

	// THE DOMAIN'S NAME FOR EACH LISTING: every constant of type ReadKind its
	// package declares, by the listing it spells.
	domainDir := filepath.Join(root, "internal", "code")
	domainPkg, err := build.Default.ImportDir(domainDir, 0)
	if err != nil {
		t.Fatalf("read the code domain's package: %v", err)
	}
	listings := map[string]code.ReadKind{}
	for _, file := range domainPkg.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(domainDir, file), nil,
			parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse the code domain's %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				if typ, ok := value.Type.(*ast.Ident); !ok || typ.Name != "ReadKind" {
					continue
				}
				for i, name := range value.Names {
					var lit *ast.BasicLit
					if i < len(value.Values) {
						lit, _ = value.Values[i].(*ast.BasicLit)
					}
					if lit == nil || lit.Kind != token.STRING {
						t.Fatalf("the code domain's %s declares the listing %s with no string "+
							"literal, which this walk reads the listing it names from", file,
							name.Name)
					}
					spelled, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("the code domain's %s spells the listing %s as %s: %v", file,
							name.Name, lit.Value, err)
					}
					listings[name.Name] = code.ReadKind(spelled)
				}
			}
		}
	}
	for _, listing := range code.ReadKinds() {
		if !slices.Contains(slices.Collect(maps.Values(listings)), listing) {
			t.Fatalf("no constant of type ReadKind in the code domain spells the listing %q, so "+
				"this walk has no name to find it by", listing)
		}
	}

	// THE DOMAIN'S NAME FOR EACH PAYLOAD: its type's, which no constant shares.
	types := map[string]reflect.Type{}
	for _, payload := range payloads {
		if payload.PkgPath() != domain || payload.Name() == "" {
			t.Fatalf("%v is no type the code domain declares by a name, so this walk has no "+
				"name to find it by", payload)
		}
		types[payload.Name()] = payload
	}

	ids := sourcetree.MustIdentifiers(slices.Concat(slices.Collect(maps.Keys(listings)),
		slices.Collect(maps.Keys(types)))...)
	polled, built = map[code.ReadKind]bool{}, map[reflect.Type]bool{}
	read := map[string]bool{}
	for queue := []string{"cmd/crewlet"}; len(queue) > 0; queue = queue[1:] {
		pkgPath := queue[0]
		if read[pkgPath] {
			continue
		}
		read[pkgPath] = true
		dir := filepath.Join(root, filepath.FromSlash(pkgPath))
		pkg, err := build.Default.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("read %s, which the crewlet binary is built from: %v", pkgPath, err)
		}
		for _, imported := range pkg.Imports {
			if inModule, ok := strings.CutPrefix(imported, module+"/"); ok {
				queue = append(queue, inModule)
			}
		}
		if dir == domainDir {
			continue
		}
		for _, file := range slices.Concat(pkg.GoFiles, pkg.CgoFiles) {
			path := filepath.Join(dir, file)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !ids.In(src) {
				continue
			}
			f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			local := ""
			for _, imported := range f.Imports {
				if spelled, _ := strconv.Unquote(imported.Path.Value); spelled != domain {
					continue
				}
				local = domainPkg.Name
				if imported.Name != nil {
					local = imported.Name.Name
				}
			}
			if local == "." {
				t.Fatalf("%s imports the code domain into its own scope, so this walk cannot "+
					"tell the domain's names from its own", path)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && local != "" && x.Name == local {
						if listing, ok := listings[sel.Sel.Name]; ok {
							polled[listing] = true
						}
						if payload, ok := types[sel.Sel.Name]; ok {
							built[payload] = true
						}
					}
				}
				return true
			})
		}
	}
	return polled, built
}

// codeLogPayload is the type the code domain decodes op's records into, on
// whichever kind of object they are on — read off the domain's own decode, so
// a payload named for an op is the one its records carry — or nil where it
// decodes them on none.
func codeLogPayload(op code.OpKind) reflect.Type {
	for _, kind := range code.ObjectKinds {
		decoded, err := code.DecodeMutation(code.MutationRecord{
			RecordEnvelope: code.RecordEnvelope{Subject: code.Subject{Kind: kind}, Op: op},
			Mutation:       json.RawMessage("{}"),
		})
		if err == nil {
			return reflect.TypeOf(decoded)
		}
	}
	return nil
}

// prose is a file of this tree as a reader reads its words: a comment's
// markers taken off each line and every run of white space one space, so a
// sentence is found however a page or a comment wraps it.
func prose(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(strings.TrimSpace(line), "//")
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// spaced is n as the docs write a number: its thousands set apart by a space
// (1 568, 12 525).
func spaced(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}

// text is n bytes of sentence repeated, cut at n.
func text(sentence string, n int) string {
	return strings.Repeat(sentence, n/len(sentence)+1)[:n]
}
