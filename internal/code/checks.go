package code

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/words"
)

// A REPOSITORY'S CHECKS ARE ROWS ON A SUBJECT OF THEIR OWN: checks.<repo>.
//
// What became of a check — queued on a commit, claimed by a runner, ended — is
// state on the code log like everything a repository holds, written by
// records on its repository's CHECKS subject ([KindChecks], checks.<repo>) and
// by nothing on the repository's own. A check's records share nothing with
// the repository's pushes and its changes' lifecycles, and decided in the
// committer's one queue they would wait behind its batches; and a table is
// written by the records of ONE subject: these keep code_checks and
// code_check_logs, which no other subject's record writes — the repository's
// purge removes them with the repository, as it removes a change's
// conversation.
//
// # Additive, so every apply is a join
//
// A check's records carry no expectation (statelog.PatternAdditive): many
// runners claim, end and report about one repository's checks at once, and an
// arbitrated subject would put every one of them behind every other — one
// record a round, and sixteen lost rounds a refusal — over rows two of them
// seldom share. PatternAdditive is correct only where records COMMUTE
// (statelog/publish.go), so every apply here is a JOIN over the row its
// record is about ([joinCheck]), and two records in either order leave one
// row, byte for byte:
//
//   - its STATE only rises — queued, then running, then an ending — because it
//     is derived from what the row holds: an ending once one arrived, running
//     once a claim did, queued otherwise;
//   - its QUEUE — the purpose, the definition, what it measures — is the
//     queuing record's with the lowest position: one dispatch's, whichever of
//     two a failover published lands first;
//   - its RUN — the runner, its placement, its start — is the claim with the
//     lower (StartedAt, Runner): two claims of one attempt converge on one
//     runner wherever each landed. The box is that claim's, set once by its
//     holder's later record, which repeats the claim's StartedAt and Runner;
//     whether it borrowed a cache only ever turns on;
//   - its ENDING is the measurement over infrastructure's — passed and failed
//     over errored, cancelled and skipped, since a run that measured the
//     commit says more of it than one that did not run — then the earlier
//     (FinishedAt, reporter), then the lower position; and its log is that
//     ending's.
//
// A field arrives with the record that carries it, so a record about a row no
// record has made yet makes it with what it carries, and the rest arrives with
// the rest. REJECTED: the first record in log order wins. The rows would then
// be whatever the apply order made them, and the writers of two records about
// one check — a runner and a failover's second one — cannot choose their
// order.
//
// # The horizon
//
// A check's rows are kept [CheckHistory]: every ending record removes its own
// repository's rows that ended before its broker instant less the history,
// oldest first, at most [CheckHorizonRows] rows and [CheckHorizonBytes] bytes
// of them, so the history keeps pace with the checks that end; and the
// code-retention duty's checks_sweep does the same for a repository whose
// checks have STOPPED ending — an idle one, whose rows nothing would ever
// remove otherwise ([Store.ChecksSweep]). The duty tells it by its rows: a
// repository holding a row that outlived the history by [CheckSweepGrace] is
// one no ending of its own has reached for that long — or one whose endings'
// bounded horizons fell that far behind — and it sweeps such a repository
// whole, back to the history. A repository whose checks keep ending is never
// listed, and none of its rows is removed twice.
//
// # Base format, as a stated condition
//
// Every op, kind and field here is in the base format: no build that carries
// the code domain has run without them — none of the domain is on main, so no
// image and no release holds it — and while that is so, every build that
// meets a record on a checks subject reads it. Once a build without them has
// run, the KIND takes {Name: "Subject.Kind checks", Since: 2, Path:
// []string{"subject", "k"}, Equals: string(KindChecks)} in versionedFields
// and each op its own row ({Name: "MutationRecord.Op <op>", Since: 2, Path:
// []string{"op"}, Equals: "<op>"} — check_queued, check_started,
// check_finished, checks_sweep, check_dispatched), with RecordVersion at 2,
// their lines under "What each version added", a Carrying record in the
// statelogtest candidate and the kind excepted from the stamp test's
// base-format loop — an older build would otherwise FAIL on such a record at
// Apply's unknown-kind arm rather than retain it. None is a gate.

// CheckHistory is how long a check's rows are kept once it has ended: thirty
// days. Past it a check is one nobody reads — a change merged weeks ago, a tip
// the branch has long moved past — and a page that cites a check cites one
// still held.
const CheckHistory = 30 * 24 * time.Hour

// CheckSweepGrace is how long past [CheckHistory] a repository's check rows
// outlive it before the code-retention duty sweeps them: a day. The duty lists
// only a repository holding a row that ended before its clock less the history
// less this grace, and sweeps it back to the history; a repository whose
// checks keep ending removes its own rows with each ending, so its rows never
// outlive the history by the grace, and the duty never appends a record that
// one of its endings would have made anyway.
//
// A DAY, because it bounds both of what it trades:
//   - what the duty appends: a sweep leaves a repository no row past the
//     history, so its next row to outlive it by the grace does so a grace
//     later at the soonest — a repository is swept at most once a grace (a
//     step more for every CheckHorizonRows rows past the first), so
//     CodeMaxRepos repositories make at most CodeMaxRepos sweeps a day, about
//     104 a pass at RetentionInterval (limits.go's RetentionPassBudget prices
//     them). Listed at the history itself, a repository was swept whenever a
//     row of it had crossed the history since its last ending: at the code
//     host's design point, one push an hour a repository, a large share of
//     CodeMaxRepos every pass — thousands of records, each of which the
//     repository's next ending would have made anyway, every data node
//     applying all of them, ahead of the purges on one budget;
//   - what an idle repository keeps: rows at most the grace past the
//     history, a thirtieth of it more, which no reader notices.
//
// So a repository whose checks end more often than once a day — a change's
// head checked on every push, say — keeps its history by itself and is never
// listed, and one checked less often is swept at most once a day.
const CheckSweepGrace = 24 * time.Hour

// CheckBoxGrace is how long a check's box, and the token it fetches with
// (internal/code/host's CheckTokens), outlive the check's own timeout:
// sandbox.DefaultSetupTimeout, ten minutes — the budget one setup command of
// a box is given, since a box is set up (its tools, its clone) before its
// check's command starts and the timeout runs only from there. A token valid
// for the timeout alone expired under a check that spent its setup cloning and
// then ran the whole of its timeout, failing its last fetch; one valid much
// longer outlives every box it was minted for. The token is the BACKSTOP and
// the attempt's row the authority: a box whose attempt has ended is refused at
// once, whatever its token's expiry.
const CheckBoxGrace = sandbox.DefaultSetupTimeout

// The record's bounds. Each is the payload's own, refused by its Validate
// before a writer forms a record, so the largest record a check takes fits
// the code log's record with room for whatever a later field carries
// (TestTheLargestCheckRecordsFitARecord).
const (
	// MaxCheckAttempts is the most attempts one check of one commit makes:
	// ten, a reader of the attempts sees every rerun anybody asked for of a
	// flaky check and still one row a rerun.
	MaxCheckAttempts = 10

	// AutoRequeueAttempts is the most attempts the home's own requeue takes a
	// check of a commit to — three: an attempt its INFRASTRUCTURE ended (its
	// runner lost, its box or its setup failed: [CheckReason.Requeued]) is
	// queued again by the home while the attempt that ended is below it, so a
	// dead runner's check runs at most three attempts. Past it the engine
	// stops spending boxes on a check its cells keep failing, and only a
	// rerun somebody asks for runs it again, up to MaxCheckAttempts.
	AutoRequeueAttempts = 3

	// CheckSummaryBytes is the longest summary an ending carries: 4 KiB, a
	// paragraph a reader takes in at a glance.
	CheckSummaryBytes = 4 << 10

	// CheckTailBytes is the most of a run's output an ending carries: the
	// last 16 KiB, about two hundred lines — where a failed build says why.
	CheckTailBytes = 16 << 10

	// MaxCheckFailures is the most failures an ending names: 200, past the
	// failing tests a person reads; more are counted, never listed.
	MaxCheckFailures = 200

	// CheckFailureMessageBytes is the longest message a failure carries:
	// 2 KiB, an assertion and its context.
	CheckFailureMessageBytes = 2 << 10

	// MaxCheckFailureBytes is the most an ending's failures weigh together,
	// encoded: 64 KiB, a sixteenth of a record, so an ending carrying every
	// other field at its bound stays far under the record.
	MaxCheckFailureBytes = 64 << 10

	// MaxCheckLogBytes is the largest log a failed run keeps: its last
	// 64 MiB, read a page at a time and never into a row.
	MaxCheckLogBytes = 64 << 20

	// MaxCheckURLBytes is the longest link an external report carries: 2 KiB,
	// the length a browser and the proxies before it keep — a CI run's link
	// is under two hundred bytes.
	MaxCheckURLBytes = 2 << 10

	// CheckHorizonRows is the most check rows one record's horizon removes:
	// an eighth of MaxRecordRows, so a record's own rows and its horizon's —
	// each check row with its log's, two rows at most — stay a quarter of
	// what one record may touch. ITS ROWS, NOT ITS TIME, BIND IT: measured,
	// the widest ending removing this many narrow rows and their logs holds
	// the write lane 14 ms, against 4.8 ms with nothing to remove
	// (BenchmarkApplyACheckRecord, doc.go's measured section) — a sixth of the
	// third of statelog.ApplyTxTimeBudget a purge part is sized to, where the
	// push rows' horizon had to be cut to HorizonDeletesPerRecord because its
	// rows are the domain's widest.
	CheckHorizonRows = MaxRecordRows / 8

	// CheckHorizonBytes is the most bytes of check rows one record's horizon
	// removes — each row weighed by its row_bytes, the first whatever it
	// weighs: MaxRecordBytes, at least what one record adds, since a check
	// record writes one row of at most its own bytes; so the history's
	// removal keeps pace with its growth in bytes as in rows. The widest rows
	// are some 290 KB — a definition of escaped text, some 200 KB, beside a
	// failed run's summary, tail, failures and link — so a horizon of them
	// removes four, a mebibyte of overflow pages: measured, 2 ms beside the
	// ending's own 4.8, the five secondary indexes a check row carries
	// included (BenchmarkApplyACheckRecord), about the 2.4 ms a megabyte the
	// push rows' deletion measured.
	CheckHorizonBytes = MaxRecordBytes
)

// CheckState is where one attempt of a check is: queued, running, or one of
// the five endings. A named string, so a state off a newer row is a value
// rather than a panic.
type CheckState string

const (
	// CheckStateQueued waits for a runner.
	CheckStateQueued CheckState = "queued"
	// CheckStateRunning is claimed by a runner, or reported running by a
	// person's CI.
	CheckStateRunning CheckState = "running"
	// CheckStatePassed ran, and its command exited 0 — a measurement.
	CheckStatePassed CheckState = "passed"
	// CheckStateFailed ran and did not pass: its command exited non-zero or
	// ran past its timeout — a measurement.
	CheckStateFailed CheckState = "failed"
	// CheckStateErrored did not run to a measurement: its runner was lost,
	// its box or its setup failed, or its definition could not be read.
	CheckStateErrored CheckState = "errored"
	// CheckStateCancelled was stopped before it measured anything: its head
	// moved on, its change left the queue, or a person stopped it.
	CheckStateCancelled CheckState = "cancelled"
	// CheckStateSkipped never ran: its change touched none of its paths.
	CheckStateSkipped CheckState = "skipped"
)

// CheckStates is the closed set, in the order a row moves through it.
func CheckStates() []CheckState {
	return []CheckState{CheckStateQueued, CheckStateRunning, CheckStatePassed, CheckStateFailed,
		CheckStateErrored, CheckStateCancelled, CheckStateSkipped}
}

// Valid reports a state this build knows.
func (s CheckState) Valid() bool { return slices.Contains(CheckStates(), s) }

// Ended reports one of the five endings.
func (s CheckState) Ended() bool { return s.Valid() && s != CheckStateQueued && s != CheckStateRunning }

// Measured reports an ending that measured the commit — passed or failed —
// which a join keeps over an infrastructure's ending of the same attempt.
func (s CheckState) Measured() bool { return s == CheckStatePassed || s == CheckStateFailed }

// CheckPurpose is what a check of a commit is for.
type CheckPurpose string

const (
	// PurposeHead checks a change's head revision.
	PurposeHead CheckPurpose = "head"
	// PurposeCandidate checks a merge queue's candidate.
	PurposeCandidate CheckPurpose = "candidate"
	// PurposeTip checks a protected branch's tip.
	PurposeTip CheckPurpose = "tip"
	// PurposeBisect checks a landing between a green tip and a red one.
	PurposeBisect CheckPurpose = "bisect"
)

// CheckPurposes is the closed set, in this order.
func CheckPurposes() []CheckPurpose {
	return []CheckPurpose{PurposeHead, PurposeCandidate, PurposeTip, PurposeBisect}
}

// Valid reports a purpose this build knows.
func (p CheckPurpose) Valid() bool { return slices.Contains(CheckPurposes(), p) }

// QueueReason is why an attempt was queued.
type QueueReason string

const (
	// QueueDispatch is a commit's first attempt for its target, queued by
	// the home — or the next after one an earlier dispatch errored for a
	// checks file that did not run ([CheckReasonDefsInvalid]), which its
	// dispatch under a file that runs queues.
	QueueDispatch QueueReason = "dispatch"
	// QueueRerun is an attempt a seat or a person asked for again.
	QueueRerun QueueReason = "rerun"
	// QueueRequeue is an attempt the home queued again after its runner was
	// lost, its box or its setup failed.
	QueueRequeue QueueReason = "requeue"
	// QueueReport is an attempt a person's own CI reported, which no runner
	// of the engine runs.
	QueueReport QueueReason = "report"
)

// QueueReasons is the closed set, in this order.
func QueueReasons() []QueueReason {
	return []QueueReason{QueueDispatch, QueueRerun, QueueRequeue, QueueReport}
}

// Valid reports a reason this build knows.
func (r QueueReason) Valid() bool { return slices.Contains(QueueReasons(), r) }

// CheckPlacement is where an attempt runs: one of the check cells, or a
// person's own CI, which reports it.
type CheckPlacement string

const (
	// PlacementContainer is a container on a git node.
	PlacementContainer CheckPlacement = CheckPlacement(CellContainer)
	// PlacementE2B is a remote E2B box.
	PlacementE2B CheckPlacement = CheckPlacement(CellE2B)
	// PlacementExternal is a person's own CI, which reports the attempt.
	PlacementExternal CheckPlacement = "external"
)

// CheckPlacements is the closed set, in this order.
func CheckPlacements() []CheckPlacement {
	return []CheckPlacement{PlacementContainer, PlacementE2B, PlacementExternal}
}

// Valid reports a placement this build knows.
func (p CheckPlacement) Valid() bool { return slices.Contains(CheckPlacements(), p) }

// CheckReason is why an attempt ended as it did.
type CheckReason string

const (
	// CheckReasonExit is a run whose command exited: 0 passed, any other
	// code failed.
	CheckReasonExit CheckReason = "exit"
	// CheckReasonTimedOut is a run past its timeout, failed: a measurement,
	// since the timeout is the check's own.
	CheckReasonTimedOut CheckReason = "timed_out"
	// CheckReasonReported is an ending a person's CI reported.
	CheckReasonReported CheckReason = "reported"
	// CheckReasonRunnerLost is a run whose runner left the git order.
	CheckReasonRunnerLost CheckReason = "runner_lost"
	// CheckReasonBoxFailed is a run whose box could not be made or failed.
	CheckReasonBoxFailed CheckReason = "box_failed"
	// CheckReasonSetupFailed is a run whose box was not made ready to run.
	CheckReasonSetupFailed CheckReason = "setup_failed"
	// CheckReasonDefsInvalid is a check its target's checks file no longer
	// defines in a form that runs.
	CheckReasonDefsInvalid CheckReason = "defs_invalid"
	// CheckReasonSuperseded is a check of a head its change has moved on
	// from.
	CheckReasonSuperseded CheckReason = "superseded"
	// CheckReasonDequeued is a candidate's check whose change left the
	// queue.
	CheckReasonDequeued CheckReason = "dequeued"
	// CheckReasonAbandoned is a candidate's check whose train was given up.
	CheckReasonAbandoned CheckReason = "abandoned"
	// CheckReasonByPerson is a check a person stopped.
	CheckReasonByPerson CheckReason = "by_person"
	// CheckReasonPaths is a check skipped because its change touched none of
	// its paths.
	CheckReasonPaths CheckReason = "paths"
)

// CheckReasons is the closed set, in this order.
func CheckReasons() []CheckReason {
	return []CheckReason{CheckReasonExit, CheckReasonTimedOut, CheckReasonReported, CheckReasonRunnerLost,
		CheckReasonBoxFailed, CheckReasonSetupFailed, CheckReasonDefsInvalid, CheckReasonSuperseded,
		CheckReasonDequeued, CheckReasonAbandoned, CheckReasonByPerson, CheckReasonPaths}
}

// Valid reports a reason this build knows.
func (r CheckReason) Valid() bool { return slices.Contains(CheckReasons(), r) }

// ranIn reports an ending only an attempt a runner claimed reaches: a run
// that exited or ran past its timeout, a runner lost, a box or a setup that
// failed — and never a check cancelled or errored before any runner took it.
func (r CheckReason) ranIn() bool {
	return r == CheckReasonExit || r == CheckReasonTimedOut || r.Requeued()
}

// Requeued reports an ending the home queues again on its own, below
// [AutoRequeueAttempts]: one its infrastructure caused — the runner lost, the
// box or its setup failed — and never one the commit or its definition did.
func (r CheckReason) Requeued() bool {
	return r == CheckReasonRunnerLost || r == CheckReasonBoxFailed || r == CheckReasonSetupFailed
}

// CheckRefusal is why a repository's checks writer decided nothing of a write
// of its checks ([ChecksRequest]) — a code its asker reads as "<code>:
// <sentence>", the sentence the decide's ([ChecksResult.Detail]). A named
// string, so a code this build does not know is a value rather than a panic.
// NONE IS EVER RECORDED: a refused write appends nothing, so no record of the
// code log carries one and none takes a versionedFields row.
type CheckRefusal string

// The codes, in the order the decide's rules reach them (checkswriter.go) and
// then the writer's own.
const (
	// CheckRefusalRepoDeleted is a write about a repository that is deleted,
	// being purged, purged or not there at all — in the sentence of the
	// state it is in ([RepoGone]).
	CheckRefusalRepoDeleted CheckRefusal = "repo_deleted"
	// CheckRefusalRepoArchived is an attempt queued on an archived
	// repository, which is read-only: nothing moves in it to be checked.
	CheckRefusalRepoArchived CheckRefusal = "repo_archived"
	// CheckRefusalNoSuchCheck is a write about an attempt the repository does
	// not hold: a claim or an ending of none, a requeue of a check no attempt
	// of which ran, or an attempt that follows none the rows hold.
	CheckRefusalNoSuchCheck CheckRefusal = "no_such_check"
	// CheckRefusalNotFinished is an attempt queued while the one before it
	// has not ended: a check of a commit runs one attempt at a time.
	CheckRefusalNotFinished CheckRefusal = "not_finished"
	// CheckRefusalAttemptsExhausted is an attempt past the last a check of a
	// commit makes ([MaxCheckAttempts]), or a requeue past the last the home
	// takes one to ([AutoRequeueAttempts]).
	CheckRefusalAttemptsExhausted CheckRefusal = "attempts_exhausted"
	// CheckRefusalRerunGenuine is an attempt asked again of a check whose
	// latest attempt the home may not queue again: one that measured the
	// commit — passed or failed — or ended for any reason its infrastructure
	// did not cause ([CheckReason.Requeued]) — and, for its dispatch, for any
	// reason but its dispatch's own error ([CheckReasonDefsInvalid]).
	CheckRefusalRerunGenuine CheckRefusal = "rerun_genuine"
	// CheckRefusalClaimed is a claim of an attempt another runner holds, or
	// one that has ended — naming the runner that holds it
	// ([ChecksResult.Holder]). A runner's refusal, never a tool's: the loser
	// starts no box.
	CheckRefusalClaimed CheckRefusal = "claimed"

	// CheckRefusalBusy is a write the repository's checks writer did not
	// reach in time — its asker's attempt run short behind the repository's
	// other check writes — and decided nothing: it is sent again.
	CheckRefusalBusy CheckRefusal = "busy"
	// CheckRefusalNotApplied is a write whose record was durable on the code
	// log and applied nowhere: a gate drops it on every node.
	CheckRefusalNotApplied CheckRefusal = "not_applied"
	// CheckRefusalUnavailable is the code log refusing the write.
	CheckRefusalUnavailable CheckRefusal = "unavailable"
)

// CheckRefusals is every code, in this order.
func CheckRefusals() []CheckRefusal {
	return []CheckRefusal{
		CheckRefusalRepoDeleted, CheckRefusalRepoArchived, CheckRefusalNoSuchCheck, CheckRefusalNotFinished,
		CheckRefusalAttemptsExhausted, CheckRefusalRerunGenuine, CheckRefusalClaimed,
		CheckRefusalBusy, CheckRefusalNotApplied, CheckRefusalUnavailable,
	}
}

// Valid reports a code this build knows.
func (r CheckRefusal) Valid() bool { return slices.Contains(CheckRefusals(), r) }

// Ends reports whether r is a reason an attempt ends in state s — the one
// pairing a writer forms and [CheckFinished.Validate] holds it to: a run that
// exited passed or failed by its code, one past its timeout failed, a
// person's CI any ending it reports, infrastructure's reasons errored,
// a stop's reasons cancelled.
func (r CheckReason) Ends(s CheckState) bool {
	switch r {
	case CheckReasonExit:
		return s == CheckStatePassed || s == CheckStateFailed
	case CheckReasonTimedOut:
		return s == CheckStateFailed
	case CheckReasonReported:
		return s == CheckStatePassed || s == CheckStateFailed || s == CheckStateErrored ||
			s == CheckStateCancelled
	case CheckReasonRunnerLost, CheckReasonBoxFailed, CheckReasonSetupFailed, CheckReasonDefsInvalid:
		return s == CheckStateErrored
	case CheckReasonSuperseded, CheckReasonDequeued, CheckReasonAbandoned, CheckReasonByPerson:
		return s == CheckStateCancelled
	case CheckReasonPaths:
		return s == CheckStateSkipped
	}
	return false
}

// CheckKey names one attempt of one check of one commit FOR ONE TARGET BRANCH
// — a check row's key.
//
// THE TARGET IS THE KEY'S: what a check of a commit measures is the
// definition its target's checks file gives it — the file read at the
// target's tip, never the commit's own, so no change narrows the checks that
// judge it — and its target's protection is what requires it. One commit is
// checked for two targets as two checks: a head another change into another
// branch shares, a tip of one branch that a change into another names as its
// head. Keyed by the commit alone, the second target's dispatch found the
// first's attempts and queued nothing — its own file's definition never ran,
// and its change's checks read empty — and a change into a branch whose file
// defined a check trivially could pass, for every change sharing its head,
// the check a stricter branch requires.
type CheckKey struct {
	RepoID string `json:"repo_id"`
	// OID is the commit checked, in 40 hex digits.
	OID string `json:"oid"`
	// Target is the branch the commit is checked for, as a ref.
	Target string `json:"target"`
	// Name is the check's name in its file ([config.ValidCodeCheckName]).
	Name string `json:"name"`
	// Attempt counts from 1.
	Attempt int `json:"attempt"`
}

// Validate refuses a key no attempt is named by ([ErrInvalid], naming the
// field): a repository id that is not a UUID in its canonical spelling, a
// commit that is not one's whole id, a target that is not a branch's ref, a
// name no check takes, an attempt outside 1 to [MaxCheckAttempts]. EXPORTED
// for the one key read off the
// wire outside a record: the one a check's box's token names
// (internal/code/host's CheckTokens), held to the rule every record's key is.
func (k CheckKey) Validate() error { return k.validateAs("") }

// validateAs is [CheckKey.Validate] with each field named under prefix — a
// key inside a request names its fields where the request carries it
// (`check_box.oid`).
func (k CheckKey) validateAs(prefix string) error {
	switch {
	case !canonicalUUID(k.RepoID):
		return checkField(prefix+"repo_id", "%q is not a UUID in its canonical lowercase spelling", k.RepoID)
	case !gitref.ValidOID(k.OID) || k.OID == ZeroOID:
		return checkField(prefix+"oid", "%q is not a commit's whole id", k.OID)
	case !strings.HasPrefix(k.Target, "refs/heads/") || !ValidRef(k.Target):
		return checkField(prefix+"target", "%q is not a branch's ref", k.Target)
	case !config.ValidCodeCheckName(k.Name):
		return checkField(prefix+"name", "%q is not a check name", k.Name)
	case k.Attempt < 1 || k.Attempt > MaxCheckAttempts:
		return checkField(prefix+"attempt", "%d is not an attempt: 1 to %d (code.MaxCheckAttempts)", k.Attempt,
			MaxCheckAttempts)
	}
	return nil
}

// checkField is a check record's field refused, as [ErrInvalid].
func checkField(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, fmt.Sprintf(format, args...))
}

// CheckQueued queues one attempt of one check of one commit — or records it
// skipped, its change touching none of its paths — on the repository's checks
// subject ([OpCheckQueued]). Every field is in the base format, under the
// condition the subject states.
type CheckQueued struct {
	CheckKey
	Purpose CheckPurpose `json:"purpose"`
	// ChangeID and Revision are the change and its revision a head check
	// measures; empty and 0 for any other purpose.
	ChangeID string `json:"change_id,omitempty"`
	Revision int    `json:"revision,omitempty"`
	// CandidateID and EntryID are the queue's candidate and entry a
	// candidate's check measures.
	CandidateID string `json:"candidate_id,omitempty"`
	EntryID     string `json:"entry_id,omitempty"`
	// Base is the commit the check's diff is read against — what an
	// API-first check asks the API about — "" for none.
	Base string `json:"base,omitempty"`
	// DefsOID is the blob of the checks file the definition was read from.
	DefsOID string `json:"defs_oid,omitempty"`
	// Def is the definition the attempt runs under, carried whole; nil only
	// for an attempt a person's CI reports, which no runner of the engine
	// runs.
	Def *CheckDef `json:"def,omitempty"`
	// Required reports a check the target's protection requires.
	Required bool        `json:"required,omitempty"`
	Reason   QueueReason `json:"reason"`
	// By is who asked for the attempt: the seat or the person who asked for
	// a rerun, the person whose CI reports it — and nobody for the engine's
	// own dispatch and requeue, whose record names its node as its actor.
	By *Principal `json:"by,omitempty"`
	// Skipped records the attempt skipped rather than queued: its change
	// touched none of its paths ([CheckReasonPaths]).
	Skipped bool `json:"skipped,omitempty"`
	// Invalid records the attempt ERRORED at its dispatch rather than queued
	// — a check the target's gate requires, which its checks file defines
	// in no form that runs ([CheckReasonDefsInvalid]): the file unparsable,
	// or something at its path that is no file — and says why, at most
	// CheckSummaryBytes, which the row keeps as its summary. Such an attempt
	// carries no definition, there being none that runs.
	//
	// BASE FORMAT, under the condition the subject states: once a build
	// without it has run it takes {Name: "CheckQueued.Invalid", Since: 2, Op:
	// "check_queued", Path: []string{"mutation", "invalid"}}, which an older
	// build defers the record by rather than queue an attempt with no
	// definition.
	Invalid string `json:"invalid,omitempty"`
	// QueuedAt is the queuing node's instant.
	QueuedAt time.Time `json:"queued_at"`
}

// Validate refuses a queued attempt no writer forms ([ErrInvalid], naming the
// field).
func (q CheckQueued) Validate() error {
	if err := q.CheckKey.Validate(); err != nil {
		return err
	}
	switch {
	case !q.Purpose.Valid():
		return checkField("purpose", "%q is not one of %s", q.Purpose, words.Or(CheckPurposes()))
	case !q.Reason.Valid():
		return checkField("reason", "%q is not one of %s", q.Reason, words.Or(QueueReasons()))
	case (q.Purpose == PurposeHead) != (q.ChangeID != ""):
		return checkField("change_id", "a head check names the change it measures, and no other check names one")
	case q.ChangeID != "" && (!canonicalUUID(q.ChangeID) || q.Revision < 1):
		return checkField("change_id", "%q, revision %d: a head check names a change by its id and one of "+
			"its revisions", q.ChangeID, q.Revision)
	case q.ChangeID == "" && q.Revision != 0:
		return checkField("revision", "a revision is a head check's alone")
	case len(q.CandidateID) > MaxProvenanceBytes || len(q.EntryID) > MaxProvenanceBytes:
		return checkField("candidate_id", "a queue's ids are at most %d bytes", MaxProvenanceBytes)
	case q.Base != "" && (!gitref.ValidOID(q.Base) || q.Base == ZeroOID):
		return checkField("base", "%q is not a commit's whole id", q.Base)
	case q.DefsOID != "" && (!gitref.ValidOID(q.DefsOID) || q.DefsOID == ZeroOID):
		return checkField("defs_oid", "%q is not a blob's whole id", q.DefsOID)
	case (q.Def == nil) != (q.Reason == QueueReport || q.Invalid != ""):
		return checkField("def", "every attempt the engine runs carries its definition, and one a "+
			"person's CI reports, or one errored at its dispatch for want of one, carries none")
	case q.Invalid != "" && (q.Purpose != PurposeHead || q.Reason != QueueDispatch || q.Skipped || !q.Required):
		return checkField("invalid", "only a change head's required check is errored at its dispatch, for a "+
			"checks file defining it in no form that runs")
	case len(q.Invalid) > CheckSummaryBytes || !utf8.ValidString(q.Invalid):
		return checkField("invalid", "%d bytes: at most %d of valid UTF-8 (code.CheckSummaryBytes)",
			len(q.Invalid), CheckSummaryBytes)
	case q.Def != nil && q.Def.Name != q.Name:
		return checkField("def.name", "%q is not the check's name, %q", q.Def.Name, q.Name)
	case q.Skipped && (q.Purpose != PurposeHead || q.Reason != QueueDispatch):
		return checkField("skipped", "only a change's head is skipped, at its dispatch, for the paths it "+
			"did not touch")
	case q.QueuedAt.IsZero():
		return checkField("queued_at", "an attempt names the instant it was queued")
	case (q.By != nil) != (q.Reason == QueueRerun || q.Reason == QueueReport):
		return checkField("by", "a rerun names who asked for it and a report the person who sent it, and "+
			"the engine's own dispatch and requeue name nobody")
	case q.Reason == QueueReport && (q.By.Kind != PrincipalOperator || q.By.Seat == ""):
		return checkField("by", "a check is reported by a person: a credential bound to the human seat "+
			"they hold")
	}
	if q.Def != nil {
		if err := q.Def.Validate(); err != nil {
			return fmt.Errorf("def: %w", err)
		}
	}
	if q.By != nil {
		if err := q.By.Validate(); err != nil {
			return fmt.Errorf("by: %w", err)
		}
	}
	return nil
}

// CheckStarted claims an attempt for a runner — and, as the holder's later
// record repeating its claim's StartedAt and Runner, names the box it made —
// on the repository's checks subject ([OpCheckStarted]). Every field is in
// the base format, under the condition the subject states.
type CheckStarted struct {
	CheckKey
	// Runner is the claiming node's id; empty for an attempt a person's CI
	// reports running.
	Runner    string         `json:"runner,omitempty"`
	Placement CheckPlacement `json:"placement"`
	// Box is the box's handle: empty on the claim, named by the holder's
	// later record once it made one.
	Box string `json:"box,omitempty"`
	// Borrowed reports a box given its node's cache to read from.
	Borrowed bool `json:"borrowed,omitempty"`
	// StartedAt is the claiming node's instant: the claim's key, with
	// Runner, so two claims of one attempt converge on the lower.
	StartedAt time.Time `json:"started_at"`
}

// Validate refuses a claim no writer forms.
func (s CheckStarted) Validate() error {
	if err := s.CheckKey.Validate(); err != nil {
		return err
	}
	switch {
	case !s.Placement.Valid():
		return checkField("placement", "%q is not one of %s", s.Placement, words.Or(CheckPlacements()))
	case (s.Placement == PlacementExternal) != (s.Runner == ""):
		return checkField("runner", "a runner of the engine names itself, and a person's CI is no runner")
	case len(s.Runner) > MaxProvenanceBytes || len(s.Box) > MaxProvenanceBytes:
		return checkField("box", "a runner's id and a box's handle are at most %d bytes", MaxProvenanceBytes)
	case s.Placement == PlacementExternal && (s.Box != "" || s.Borrowed):
		return checkField("box", "a person's CI runs no box of the engine's")
	case s.StartedAt.IsZero():
		return checkField("started_at", "a claim names the instant it was made")
	}
	return nil
}

// CheckFailure is one failure an ending names: where, which test, and what
// it said.
type CheckFailure struct {
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	TestID string `json:"test_id,omitempty"`
	// Message is what the run's report said: the check's command's words,
	// which run the change's own code (ADR-0040).
	Message string `json:"message" untrusted:"fence"`
}

// CheckLog is a failed run's log, stored as one object a code_check_logs row
// names ([CheckLogReferences]).
type CheckLog struct {
	Object objstore.Key  `json:"object"`
	Hash   objstore.Hash `json:"hash"`
	Size   int64         `json:"size"`
}

// Content is the object a read of the log is checked against.
func (l CheckLog) Content() objstore.Object {
	return objstore.Object{Key: l.Object, Hash: l.Hash, Size: l.Size}
}

// CheckFinished ends an attempt, on the repository's checks subject
// ([OpCheckFinished]): what it measured — or why it measured nothing — what
// it said, and its log where a failed run kept one. Every field is in the
// base format, under the condition the subject states.
type CheckFinished struct {
	CheckKey
	State  CheckState  `json:"state"`
	Reason CheckReason `json:"reason"`
	// ExitCode is the command's exit status: required of a run that exited.
	ExitCode   *int  `json:"exit_code,omitempty"`
	DurationMs int64 `json:"duration_ms,omitempty"`
	// Summary is what a reader is told first, at most CheckSummaryBytes.
	Summary string `json:"summary,omitempty" untrusted:"fence"`
	// Tail is the run's last output, at most CheckTailBytes; a passed run
	// carries none — the change's own code's words, as Summary is
	// (ADR-0040).
	Tail string `json:"tail,omitempty" untrusted:"fence"`
	// Failures are what the run's reports named, at most MaxCheckFailures
	// weighing at most MaxCheckFailureBytes, and FailuresOmitted how many
	// more there were; a passed run carries none.
	Failures        []CheckFailure `json:"failures,omitempty"`
	FailuresOmitted int            `json:"failures_omitted,omitempty"`
	// Log is the failed run's kept log; a passed run keeps none.
	Log *CheckLog `json:"log,omitempty"`
	// Reporter is the person whose CI reported the ending; nil for a runner
	// of the engine's.
	Reporter *Principal `json:"reporter,omitempty"`
	// URL is the reported run's own page, at most MaxCheckURLBytes.
	URL string `json:"url,omitempty"`
	// FinishedAt is the instant the attempt ended, by its runner's clock or
	// its reporter's writer's.
	FinishedAt time.Time `json:"finished_at"`
}

// Validate refuses an ending no writer forms.
func (f CheckFinished) Validate() error {
	if err := f.CheckKey.Validate(); err != nil {
		return err
	}
	switch {
	case !f.State.Ended() || f.State == CheckStateSkipped:
		return checkField("state", "%q is no ending a run reaches: passed, failed, errored or cancelled "+
			"(a skip is recorded when the attempt is queued)", f.State)
	case !f.Reason.Ends(f.State):
		return checkField("reason", "%q does not end an attempt %s", f.Reason, f.State)
	case f.Reason == CheckReasonExit && f.ExitCode == nil:
		return checkField("exit_code", "a run that exited names its status")
	case f.ExitCode != nil && f.Reason == CheckReasonExit && (*f.ExitCode == 0) != (f.State == CheckStatePassed):
		return checkField("exit_code", "%d: a run passes by exiting 0, and fails by any other status",
			*f.ExitCode)
	case f.DurationMs < 0:
		return checkField("duration_ms", "%d is no duration", f.DurationMs)
	case len(f.Summary) > CheckSummaryBytes || !utf8.ValidString(f.Summary):
		return checkField("summary", "%d bytes: at most %d of valid UTF-8 (code.CheckSummaryBytes)",
			len(f.Summary), CheckSummaryBytes)
	case len(f.Tail) > CheckTailBytes || !utf8.ValidString(f.Tail):
		return checkField("tail", "%d bytes: at most %d of valid UTF-8 (code.CheckTailBytes)", len(f.Tail),
			CheckTailBytes)
	case f.State == CheckStatePassed && (f.Tail != "" || len(f.Failures) > 0 || f.FailuresOmitted > 0 ||
		f.Log != nil):
		return checkField("tail", "a passed run keeps its summary alone: no tail, no failures, no log")
	case len(f.Failures) > MaxCheckFailures:
		return checkField("failures", "%d failures, past the %d an ending names (code.MaxCheckFailures); "+
			"count the rest in failures_omitted", len(f.Failures), MaxCheckFailures)
	case f.FailuresOmitted < 0:
		return checkField("failures_omitted", "%d is no count", f.FailuresOmitted)
	case len(f.URL) > MaxCheckURLBytes || !utf8.ValidString(f.URL):
		return checkField("url", "%d bytes: at most %d of valid UTF-8 (code.MaxCheckURLBytes)", len(f.URL),
			MaxCheckURLBytes)
	case (f.Reporter != nil) != (f.Reason == CheckReasonReported):
		return checkField("reporter", "a reported ending names the person who reported it, and no other "+
			"ending names one")
	case f.Reporter != nil && (f.Reporter.Kind != PrincipalOperator || f.Reporter.Seat == ""):
		return checkField("reporter", "a check is reported by a person: a credential bound to the human "+
			"seat they hold")
	case f.FinishedAt.IsZero():
		return checkField("finished_at", "an ending names the instant it ended")
	}
	if f.Reporter != nil {
		if err := f.Reporter.Validate(); err != nil {
			return fmt.Errorf("reporter: %w", err)
		}
	}
	for i, failure := range f.Failures {
		if len(failure.Message) > CheckFailureMessageBytes || !utf8.ValidString(failure.Message) ||
			!utf8.ValidString(failure.Path) || !utf8.ValidString(failure.TestID) || failure.Line < 0 {
			return checkField(fmt.Sprintf("failures[%d]", i), "a failure's message is at most %d bytes, "+
				"and every field valid UTF-8 (code.CheckFailureMessageBytes)", CheckFailureMessageBytes)
		}
	}
	if len(f.Failures) > 0 {
		encoded, err := json.Marshal(f.Failures)
		if err != nil {
			return fmt.Errorf("code: encode an ending's failures: %w", err)
		}
		if len(encoded) > MaxCheckFailureBytes {
			return checkField("failures", "%d bytes encoded, past the %d an ending's failures weigh "+
				"(code.MaxCheckFailureBytes)", len(encoded), MaxCheckFailureBytes)
		}
	}
	if f.Log != nil {
		if err := f.Log.Content().Validate(); err != nil {
			return checkField("log", "%v", err)
		}
		if f.Log.Size > MaxCheckLogBytes {
			return checkField("log", "%d bytes, past the %d a run keeps (code.MaxCheckLogBytes)", f.Log.Size,
				MaxCheckLogBytes)
		}
	}
	return nil
}

// ChecksSweep removes a repository's check rows that ended before Through,
// oldest first — the code-retention duty's, for a repository whose rows
// outlive [CheckHistory] by [CheckSweepGrace] with no check of its own ending
// to remove them ([OpChecksSweep], [Store.ChecksSweep]). In the base format,
// under the condition the subject states.
type ChecksSweep struct {
	RepoID string `json:"repo_id"`
	// Through is the duty holder's cutoff: its clock less CheckHistory.
	Through time.Time `json:"through"`
}

// Validate refuses a sweep no writer forms.
func (s ChecksSweep) Validate() error {
	switch {
	case !canonicalUUID(s.RepoID):
		return checkField("repo_id", "%q is not a UUID in its canonical lowercase spelling", s.RepoID)
	case s.Through.IsZero():
		return checkField("through", "a sweep names the instant its rows ended before")
	}
	return nil
}
