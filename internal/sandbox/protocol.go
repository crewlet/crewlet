// Package sandbox is the executor runtime: every agent seat's executor is its
// harness's own run, inside a box placed in one of the catalogue's cells.
//
// The shape that matters is that a run is DETACHED. The launch starts it, the
// turn SUSPENDS, and the engine RESUMES the same turn with the run's report
// when the run completes — possibly minutes later, possibly in a different
// process after a restart, possibly on a different node. Nothing
// here parks a goroutine on a running job: a goroutine cannot survive a
// restart, and a harness legitimately runs for longer than a deployment
// window.
//
// A run that stops to ask a person something is that same shape one step
// further: the seat goes FREE while the question waits, and the answer is
// matched back to the run by the CONVERSATION it was asked in rather than by
// the inbox batch the question arrived in. A person answers where they are
// talking, and the engine's own chat prompt routinely puts their reply in a
// finer partition than the question was asked from — so a match on the batch
// lost the answer outright. The rule is [ConversationRef.Answers], and
// [ConversationRef.Best] chooses between the runs it admits.
//
// AND BY WHEN IT WAS WRITTEN, against when the question was asked
// ([PendingRun.AskedAt]): the first reply posted after the question is the
// answer, and it is RECORDED on the run ([StatusAnswered]) before anything is
// done with it. Resuming with it is then this package's job, retried on its
// own schedule with the seat's inbox held behind it, never the inbox's — a
// person's message handed back to the broker returns behind their next one,
// which then took the question. See answerowed.go.
//
// AND IT REACHES THE SEAT ONCE, whatever becomes of the run. A recorded reply
// is either TAKEN by a resumed turn — written on the row at the last moment
// before the turn runs ([RecordedAnswer.TakenAt], [ResumeRequest.Begin]) — or
// LET GO back to the seat the way it came, through an outbox on the row
// ([PendingRun.HandBack]) — a chat reply to the seat's inbox, an answer by turn
// to its control topic ([handBackSubject]): after its attempts are spent, by an
// ending no turn reached, or by the seat's next holder reaping a claim whose
// node stopped before its turn took it ([Coordinator.RecoverSeat]) once the claims of that
// answer have died too often or for too long. Short of that, such a claim is
// REVIVED — the answer given back to the run and the run resumed with it
// ([PendingStore.ReviveAnswer], [Coordinator.revivable]) — because a claim that
// never reached its turn used nothing. The take is exclusive with both, in the
// store's own compare-and-set: an ending is DECIDED on the row before any of
// it is done ([RecordedEnding]), no turn takes an answer from a row whose
// ending is decided ([ErrRunEnding]) or from a revived run, which holds no
// claim, and a take that landed first is seen by the decision or the revival
// and the reply goes with the run as used — whichever lands first wins,
// whatever any lease says. And no ending deletes a row still
// holding a reply it owes the seat ([ErrAnswerOwed]). The reply's delivery is
// recorded as worked when the reply leaves the run, either way
// ([CoordinatorOptions.Spent]) — but a refused reply's only once a turn that
// took its refusal keeps it, since the delivery itself is what goes back to the
// seat when the refusal is let go of ([AnswerRefused]).
//
// AND A LOST RUN IS ANNOUNCED ONCE: the announcement goes out before the record
// is deleted, under the identity its decided ending recorded, so whichever node
// finishes the ending publishes the same event ([Coordinator.announceEnding]).
// So is a STOPPED one — a person's pause with `stop_running` ends the seat's
// launching and running runs at once ([Coordinator.StopSeat]) — as a stop and
// never as a loss, with who stopped it recorded in the deciding write
// ([PendingRun.Stop]), its job ENDED and its spend read off the box and charged
// before the box goes, and the turn parked on it closed as stopped
// ([CoordinatorOptions.CloseStopped]).
//
// AND A STOPPED OR LOST JOB IS ENDED BEFORE ITS SESSION IS TAKEN, and so is
// one still working when its turn's cap has passed, since the cap is an end:
// the read sends its script a request to end it — SIGUSR1 to the pid the script
// recorded in its box ([Sandbox.EndJob], [JobEnder]) — which stops its harness
// and packs its session as its own end does, and kills what is left past the
// wait ([Sandbox.KillJob]), so what is taken is final wherever the job ended.
// See endjob.go.
//
// The reap fences the row to its lease before it revives or lets go of
// anything — and the revival stamps that lease on the row itself — and a take
// under an older lease — or under none — is refused, which holds because every
// claim stamps the CLAIMANT's lease on the row ([PendingStore.ClaimForResume])
// and every launch the lease that launched it ([CoordinatorOptions.Lease]).
//
// EVERY SUBJECT A RUN IS REACHED ON IS NAMED BY THE ROW'S SEAT ID — its
// completion and start on the seat's control subject, an answer by turn and a
// reply handed back on its inbox — through one parse of the id the row carries
// as text ([controlSubject], [inboxSubject]), because a seat's mailbox is named
// by its id, derived from its immutable handle (ADR-0013). The handle the row
// carries beside it is a label and this node's bookkeeping key, never an
// address: the coordinator's per-seat counts, holds and retries are keyed by it,
// and whatever maps one of them onto a lease or a mailbox maps the handle to
// the id first. An answer by turn is stamped with that same id
// ([AnswerDesk.Deliver]) and dropped by any seat whose run does not carry it
// ([Coordinator.AnswerByTurn]).
//
// WHO ANSWERED is recorded as iam.ActorFor makes it of a principal — the
// author, its kind and the credential it came through ([AnswerWords]) —
// on the row and on every announcement of what became of the answer, never as a
// seat beside an actor.
//
// AND THE ANSWER IS SEALED: its words and who gave them are on the row only
// inside a seal under the fleet keyring, bound to the run's turn, the question
// it answers and its deliveries, like every other instruction channel
// (ADR-0031), and every reader opens it before anything of it is used. An answer
// whose seal does not open is never handed to a turn or attributed to anybody.
// See answerseal.go.
//
// AND A REPLY TOO LARGE TO BE THE ANSWER IS THE QUESTION'S REFUSAL (ADR-0114):
// past what the row carries as an answer ([MaxAnswerBytes]), decided against the
// run as the store finds it still waiting on the question
// ([PendingRun.takesAnswer]), a chat reply is not taken; what the row records as
// the question's outcome instead is its refusal — its deliveries by id, when it
// was posted and the figures, sealed, and none of its words
// ([PendingStore.RecordRefusal], [PendingRun.AnswerRefusal]) — and announced
// as `executor_answered` too_large under one identity per run, question and
// delivery ([tooLargeRecord]). The refusal is then resumed exactly as an
// answer is: the run is told, in the engine's words and through the same
// channel an answer reaches it ([AskedRun.Refused]), that the person replied
// and the reply was too large to hand it, and either asks again with a question
// of its own, whose own [PendingRun.AskedAt] leaves everything written before
// it the ordinary message it is, or carries on without it. While the refusal
// is the run's the question waits no more, so nothing the person writes
// meanwhile is taken for its answer. The reply itself stays where it was: in
// the conversation, and in its delivery, kept on the seat's inbox until the
// refusal leaves the run ([AnswerRefused]) — spent once a turn that took the
// refusal keeps it ([Coordinator.spendRefused]), and back to the seat as the
// ordinary message it is when the refusal is let go of. A refusal let go of —
// declined after its attempts, or a seal that does not open — puts the
// question back to waiting under the anchor it was asked at, as an answer let
// go of does, and the next qualifying reply answers it; the let-go is announced
// as `executor_answered` declined ([Coordinator.announceRefusalLetGo]). A
// reply within the share is the run's answer word for word, and an answer by
// turn is held to [MaxAnswerTextBytes] at both its ends, on one string.
//
// AND A CALL TO A TOOL AT NEEDS APPROVAL PARKS ITS RUN IN THE SHAPE A RUN'S ASK
// DOES (ADR-0077): the job's question is the call ([PendingRun.HeldCall]), its
// arguments sealed beside it ([CallSeal]); only its approval route answers it,
// never a chat reply ([ConversationRef.Answers]) or an answer by turn that does
// not name it ([Coordinator.AnswerByTurn]); and the resume that makes the call
// marks it first, so no attempt at the same answer makes it twice
// ([PendingStore.MarkHeldCall]). See heldcall.go.
//
// AND A RUN'S VALUES ARE HELD TO A BYTE BUDGET: one value in the coordination
// store is one broker message, so the run's row and the log beside it are each
// held to [RowBytes], in the bytes their encoder writes, and every field has a
// share of its value's. The log's calls have theirs ([BridgeLogBytes]), each
// call held to its own and the calls it counts without listing kept as
// identities ([ElidedCall]); on the row, the parked state has its own
// ([ExecuteStateBytes]), which its park condenses to fit and the store refuses
// past, and the rest is every other field held by the write that puts it there
// — a person's reply, a held call's sealed arguments and a box's word on its
// session included — with room stated for what a later build adds. Every call
// carries the identity its run's call log admitted it under
// ([BridgeCall.Digest]), but for one whose arguments are withheld
// ([BridgeCall.Withheld]), so neither a cut argument nor a dropped call moves an
// operation id a resumed segment derives; and the id its harness gave it, where
// the box sent one the log keeps whole ([BridgeCall.CallID]), which is no part
// of that identity. See rowbytes.go.
//
// AND A BOX IN A `direct` OR `container` CELL IS TOUCHED ONLY ON ITS LAUNCHING
// NODE'S HOST ([PendingRun.LaunchNode], ADR-0065): that node's own waiter polls,
// keeps alive, reaps and reclaims it, a seat's holder elsewhere asks the host to
// read it ([BoxReader]), and a launching node whose presence stays lapsed for
// the box timeout has its running jobs lost with it. See boxhost.go.
//
// AND A JOB LOST WITH ITS HOST IS CARRIED ON, NEVER COLLECTED: one its
// engine's restart killed — which the box tells from one that crashed by the
// engine process that last saw it running ([JobLost], local_witness.go) — one
// whose box is gone, and one whose node left. The waiter that drives the box
// records the loss on the row ([PendingRun.Lost]), and the seat's holder reads
// what the job spent where its box is left and resumes the turn to relaunch the
// same round, at most [MaxRoundLosses] times a round. See lost.go.
//
// AND WHAT A RUN PUSHED TO GITLET is the push record's
// account, never the run's: a collection of a run given a repository there to
// push to reads the rows Gitlet wrote under its launch ([RunPushes],
// [Result.Pushed]) and the run's report states what each ref became, the run
// having been told its own report need not name them. A record that cannot be
// read is SAID to be unknown rather than retried — a collection held until
// Gitlet answers holds a finished run's seat for as long as the host is
// down — and never left out, since silence reads as "nothing pushed".
//
// AND A RUN'S HARNESS SESSION CAN LEAVE ITS BOX: a run names its session before
// its harness starts ([RunHandle.SessionID]), says whether it continued the one it
// was asked to, and why not where it did not ([Result.Continued],
// [Result.FellBack]), and packs it when the harness exits
// ([Result.Captured]); a run handed a session packed in another box
// ([RunRequest.Carried]) restores it before its harness takes it up, and a box
// takes the archive streamed rather than whole ([Sandbox.WriteStream]). See
// carry.go.
//
// See docs/concepts/code-sandbox.md.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/words"
)

// log is the package's own component, bound for the backend-NEUTRAL runtime:
// the coordinator, the waiter, the manager, launch, setup, mcprender and otel.
//
// Each backend binds its own (localLog, e2bLog) so a line names the box it
// came from. This var used to live in local.go bound to "sandbox.local", which
// stamped every backend-neutral event — and every remote-box event — as the
// local backend, so filtering logs by component hid remote runs exactly where
// an operator would look for them. A file added here logs as plain "sandbox"
// unless it is backend-specific, which is the safe default: an over-general
// component is findable, a wrong one is not.
var log = logging.Get("sandbox")

// DefaultHome is where a box that says nothing else keeps its artefacts.
//
// Every run artefact — the result, the done marker, the ask signal, the
// findings — lives under <home>/.crewlet. It is a per-SANDBOX property rather
// than a constant, which it was for as long as a remote VM was the only
// backend: many local boxes share one filesystem, and a shared home would have
// every run reading its neighbour's done marker.
//
// It is [hostbox.ContainerHome], declared there beside [hostbox.InHome],
// which judges a container box's paths against it — for both local boxes and
// for config validation, all of which already import that leaf package: the
// remote templates' one account has it as its home, and a container box
// mounts its own there so a path reads the same in both.
const DefaultHome = hostbox.ContainerHome

// ExecResult is one shell command's outcome inside a box.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Limits are the caps handed to a harness.
//
// Zero means UNSET for each field, and the runner falls back to the agent's own
// default. Note what is not here: a run deadline. The engine imposes no
// wall-clock limit on a launched job — it runs as long as it needs, and
// completion is detected by tracking the job rather than by a clock. There WAS
// a TimeoutSec here, read by nobody, contradicting the paragraph above it.
//
// MaxTurns comes from the resolved [Spec] — providers.sandbox.default_max_turns
// overlaid with role.sandbox.max_turns — and is zero, uncapped, unless the
// company or the seat sets a cap: a job is deliberately never stopped on a
// clock, so this is the one bound on its rounds, and it is the operator's to
// choose.
//
// NO DOLLAR CAP. The fleet's own token meter already post-charges a collected
// run against the seat's budget, and a second cap denominated in the
// operator's dollars would fight it: two ceilings on one spend, disagreeing,
// with the harness's the one that silently wins. A MaxBudgetUSD field stood
// here, set by nobody while the runner turned it into a flag, and went the way
// TimeoutSec did.
type Limits struct {
	MaxTurns int
}

// Spec is what it takes to mint and drive one box.
//
// AN EXTERNAL HOST'S REPOSITORY IS NOT HERE. Which repository on GitHub or
// GitLab to work in is task context the executor puts in the brief, and the
// harness clones it with the token the config injects — a spec field for it
// would make one repository per role a structural fact when it is a per-turn
// one. A repository on Gitlet, the engine's own git host, is here ([Spec.Repos]),
// because naming one is a GRANT: the run row carries it and the box's
// credential is judged by it, which a brief cannot be.
//
// There are deliberately NO CPU, MEMORY OR DISK FIELDS, and no TEMPLATE either.
// A remote box's resources are a property of its template, fixed when the
// template is built, and the create APIs accept no resource arguments; a local
// box is sized by providers.sandbox.local.run_args. Both are configured on the
// BACKEND, once, so a spec field for either would be a second answer to a
// question the catalogue has already settled — and the last one, Template, was
// set by nobody while looking exactly like a wired knob.
type Spec struct {
	// Placement is WHICH configured backend runs this box. Resolved from
	// the run_in of the providers.llm entry the launch picked, over
	// providers.sandbox.default_run_in, and carried here rather than chosen
	// by the caller so the launch, the pending row and the reconnect all
	// name the same cell.
	Placement Placement

	// Harness names the runner: "claude-code", "opencode", …
	Harness string

	// TurnID is the turn whose run the box is made for. A box on the
	// engine's host records it in its own directory, which is how that
	// host's waiter tells a box no run names any more from one a run is
	// using, and reclaims it ([HostBox]); a remote box has no use for it.
	// Set by [Coordinator.Launch], never by a caller.
	TurnID string

	// TimeoutSec is NOT a run deadline: it is the box's initial TTL, which
	// the launch refreshes while it provisions the box and the waiter every
	// tick once the job runs, so neither a long provisioning nor a running
	// job is ever killed by the clock. It is the ORPHAN-RECLAIM GRACE — how
	// long a box outlives an engine that has stopped heart-beating.
	TimeoutSec float64

	// PauseTTLSec bounds a PAUSED box: how long its snapshot is held before
	// the reaper reclaims it. 0 means never pause — always re-seed from the
	// pushed branch instead.
	PauseTTLSec float64

	// MaxTurns caps the agentic rounds a launch may take. 0 is
	// uncapped, and is the default.
	//
	// Resolved here rather than passed alongside, because it is settled the
	// same way TimeoutSec and PauseTTLSec are — a provider default with a
	// per-seat override — and a second resolution path for the same shape
	// of knob is how the two come to disagree. [Launch] turns it into the
	// runner's [Limits].
	MaxTurns int

	// Env is the run environment: the LLM entry's env and sign-in, the
	// generic agent identity (CREWLET_AGENT_*), the setup steps' env, and
	// role.sandbox.env — which is where an operator DECLARES an external
	// token. The engine never names a tool-specific variable itself, but for
	// Gitlet, whose access it gives every launch on a
	// node that serves it ([BoxGit], ADR-0093) — and that through the
	// launch's own environment, never this field. A launch hands the box a
	// COPY with what it minted for the job added ([LaunchRequest.RunEnv]):
	// what the box is made with and every setup step runs with, the run's
	// telemetry added at the job's start.
	Env map[string]string

	// CredentialFiles is a subscription-harness login: a path relative to the
	// box home mapped to an absolute path on the ENGINE HOST.
	//
	// Each provider decides what to do with it, and they decide differently
	// on purpose. A local box seeds the files in and writes one its run
	// refreshed back — only over the very login it was seeded from, so an
	// idle box or a stale refresh never reverts a newer one; a REMOTE one
	// ignores them, because they carry a refresh token
	// whose rotation is shared fleet state, and pushing that onto somebody
	// else's VM is a materially larger trust step than the scoped headless
	// token the run env already exports.
	CredentialFiles map[string]string

	// Repos are the repositories on Gitlet this run is
	// given, each resolved at launch ([RunRepo]), at most [MaxRunRepos]. Empty
	// for a run that clones nothing from Gitlet — a repository on
	// another host is still named in the brief and reached with the
	// credentials the seat's configuration declares. [Coordinator.Launch]
	// writes them on the run's row ([PendingRun.Repos]), which is what the
	// box's credential is judged against for as long as the job runs.
	Repos []RunRepo
}

// MaxRunRepos is the most repositories one executor is given: each costs a
// clone before the harness starts, and a run that needs more is a task
// that should be split.
//
// THE CLONES ADD UP. They run one after another, after the operator's steps
// and the box's git check, each to its own budget ([DefaultSetupTimeout]), so
// eight are up to eighty minutes on top of the rest of the box's provisioning
// — five times the box's TTL ([DefaultBoxTimeout]), which is why the launch
// keeps the box's timer refreshed for as long as it provisions it
// ([keepProvisioning]) rather than leaving the sum to the timer the box was
// made with. What a run waits for before its harness starts is that sum.
//
// ONE AFTER ANOTHER, never at once, which would cut that wait to the slowest
// clone's: a clone's index-pack keeps an entry for every commit and tree it
// receives while it writes their index — 1.37 million of them for a gigabyte
// of history (internal/gitlet/host's BenchmarkBoxClone) — and eight at once
// would share a box its template sized for one harness. In turn, the
// clones cost a run with many repositories time; at once, they would cost
// its box memory.
//
// NEVER PAST gitlet.MaxRunRepos, the most a read of a run's pushes names: a run
// given more would have every collected result's "what it pushed" refused,
// seen only as the sentence that its push record was unread. internal/engine's
// TestARunGivenItsMostRepositoriesHasItsPushesReadWhole holds the two to each
// other.
const MaxRunRepos = 8

// MaxConePaths is the most directories one repository's cone names
// ([RunRepo.Paths]). A cone is a few top-level directories of a monorepo:
// sixty-four is past any build's declared reach, and it is one of the two
// figures that keep the clone step one argument a shell can be given (see
// [MaxConePathBytes]).
const MaxConePaths = 64

// MaxConePathBytes is the longest directory a cone names: 255 bytes, the bound
// a ref has (gitref.MaxRefNameBytes) and a single file name has on every
// filesystem git runs on (NAME_MAX). A cone names a directory a few
// components below a repository's root, which a person or a model types out;
// none comes near it.
//
// THE BOUND KEEPS THE CLONE STEP WHOLE. A box runs each setup command as ONE
// argument of a shell (`sh -c`, `bash -l -c` through E2B's envd), and Linux
// refuses an argument past 128 KiB (MAX_ARG_STRLEN) before the shell starts,
// with a failure that names nothing the run asked for. [MaxConePaths] of
// these, quoted at their worst — a path of nothing but single quotes is four
// times its length once quoted ([ShellQuote]), and each takes its two quotes
// and a space — come to at most 64 × 1 023 = 65 472 bytes of the step, and the
// rest of it (the names and the commands) to a few KiB more;
// TestTheWorstCloneStepIsOneShellArgument builds that step and holds it under
// the limit. It bounds what a run's grants add to its row
// too, which every node's completion poll reads whole: at most [MaxRunRepos] ×
// 64 × 255 bytes of paths, about 128 KiB before the JSON on the row escapes
// them.
const MaxConePathBytes = 255

// RepoAccess is what a run may do with a repository it was given. A named
// string with Valid, so a newer build's value off the row is a value rather
// than a panic.
type RepoAccess string

const (
	// RepoRead is fetch and clone.
	RepoRead RepoAccess = "read"
	// RepoWrite is those and push — whatever the seat's own grant allows,
	// which every push is decided against again.
	RepoWrite RepoAccess = "write"
)

// RepoAccesses is every [RepoAccess] this build knows.
func RepoAccesses() []RepoAccess { return []RepoAccess{RepoRead, RepoWrite} }

// Valid reports whether a is one of [RepoAccesses].
func (a RepoAccess) Valid() bool { return slices.Contains(RepoAccesses(), a) }

// RunRepo is one repository a box is given, RESOLVED WHEN THE BOX IS: the
// repository's id beside the name it had then, so a rename meanwhile cannot
// move the grant; the commit the box starts from; the branch an executor
// works on; and the cone it checks out.
//
// The json tags are a WIRE FORMAT: an executor's row carries these
// ([PendingRun.Repos]), and a node of another build reads them. A check's
// repository rides no row: its box is made, run and closed by one node.
type RunRepo struct {
	// Repo is the repository's name at launch: the directory the box is
	// given it in, and the name its URL carries.
	Repo string `json:"repo"`
	// RepoID is the repository's id, canonical: what the grant names.
	RepoID string `json:"repo_id"`
	// Base is the whole id of the commit the box starts from: an
	// executor's base, "" for an empty repository, or the one commit a check
	// measures.
	Base string `json:"base"`
	// Branch is the branch an executor works on, its ref
	// refs/heads/<branch>; "" for a check, which works on no branch.
	Branch string `json:"branch"`
	// Access is what the box may do with the repository: a check's box
	// reads it.
	Access RepoAccess `json:"access"`
	// Paths are the directories checked out (a sparse cone), at most
	// [MaxConePaths]; empty is the whole tree.
	Paths []string `json:"paths,omitempty"`
	// Change is the change an executor works on — the live change whose
	// source is Branch, which it starts from the head of — and nil for a run
	// on no change (runchange.go). An executor's alone.
	Change *RunChange `json:"change,omitempty"`
	// Open is the change an executor asked to open from Branch once it
	// finishes — at its collection, once a launch ([OpenChange]) — and nil
	// for none. An executor's alone, and only one given the repository to
	// write: a run that may push nothing delivers nothing to open one from.
	Open *OpenChange `json:"open_change,omitempty"`
}

// ValidateFor holds a RunRepo to the grammar of the box it is given to for
// purpose. EVERY PURPOSE: a repository name of either form
// (gitref.ValidRepoName), a canonical repository id, a base that is "" or a
// whole commit id, a valid access, and at most [MaxConePaths] cone paths, each
// a clean directory inside the repository ([ConePathProblem] says what that
// holds a path to). Then by purpose:
//
//   - [CloneCoding]: a branch name git and Gitlet both take
//     (gitref.ValidBranch), since the run works on one; a change it works on
//     named by its id and its number; and a change it asks to open given
//     write, never beside a change it works on — whose branch heads that
//     change already — and proposing its branch into another
//     ([OpenChange.problem]);
//   - [CloneSingleSHA], a CHECK's: a base — the commit it measures — no
//     branch, since a check is given one commit and works on none, and read
//     access, since its box's credential fetches and never pushes.
//
// A value a purpose does not read is REFUSED rather than ignored: a branch on
// a check's repository, or a change on one, is a caller that meant another
// purpose, and a box made from it would be told one thing and do
// another.
//
// ONE GRAMMAR for every reader: the launch that resolves a run's repositories
// refuses a request with it, and the clone step is written only for a
// RunRepo it passes ([CloneRecipe]), so nothing a box is told to run carries a
// value no RunRepo can.
func (r RunRepo) ValidateFor(purpose ClonePurpose) error {
	switch {
	case !purpose.Valid():
		return fmt.Errorf("sandbox: repository %s: %q is not a clone purpose (one of %s)", r.Repo, purpose,
			words.Or(ClonePurposes()))
	case !gitref.ValidRepoName(r.Repo):
		return fmt.Errorf("sandbox: %q is not a repository name", r.Repo)
	case !canonicalID(r.RepoID):
		return fmt.Errorf("sandbox: repository %s: its id %q is not a UUID in canonical form", r.Repo, r.RepoID)
	case r.Base != "" && (!gitref.ValidOID(r.Base) || r.Base == gitref.ZeroOID):
		return fmt.Errorf("sandbox: repository %s: its base %q is not a whole commit id", r.Repo, r.Base)
	case !r.Access.Valid():
		return fmt.Errorf("sandbox: repository %s: access %q is neither %q nor %q", r.Repo, r.Access,
			RepoRead, RepoWrite)
	case len(r.Paths) > MaxConePaths:
		return fmt.Errorf("sandbox: repository %s: a cone names at most %d directories, not %d", r.Repo,
			MaxConePaths, len(r.Paths))
	}
	for i, p := range r.Paths {
		if why := ConePathProblem(p); why != "" {
			return fmt.Errorf("sandbox: repository %s: paths[%d] %q %s", r.Repo, i, p, why)
		}
	}
	if purpose == CloneCoding {
		switch {
		case !gitref.ValidBranch(r.Branch):
			return fmt.Errorf("sandbox: repository %s: %q is not a branch name git accepts", r.Repo, r.Branch)
		case r.Change != nil && r.Change.problem() != "":
			return fmt.Errorf("sandbox: repository %s: the change it works on %s", r.Repo, r.Change.problem())
		case r.Open != nil && r.Change != nil:
			return fmt.Errorf("sandbox: repository %s: a run on change !%d asks to open another, and its branch "+
				"%s heads that one already", r.Repo, r.Change.Number, r.Branch)
		case r.Open != nil && r.Access != RepoWrite:
			return fmt.Errorf("sandbox: repository %s: a run given it to %s asks to open a change, and delivers "+
				"nothing to open one from", r.Repo, r.Access)
		case r.Open != nil && r.Open.problem(r.Branch) != "":
			return fmt.Errorf("sandbox: repository %s: the change it asks to open %s", r.Repo,
				r.Open.problem(r.Branch))
		}
		return nil
	}
	switch {
	case r.Change != nil || r.Open != nil:
		return fmt.Errorf("sandbox: repository %s: a check is given one commit and works on no change", r.Repo)
	case r.Base == "":
		return fmt.Errorf("sandbox: repository %s: a check is given one commit, and names none", r.Repo)
	case r.Branch != "":
		return fmt.Errorf("sandbox: repository %s: a check is given one commit and works on no branch, "+
			"and names %q", r.Repo, r.Branch)
	case r.Access != RepoRead:
		return fmt.Errorf("sandbox: repository %s: a check's box reads its repository, its credential "+
			"fetching and never pushing, and is given %q", r.Repo, r.Access)
	}
	return nil
}

// ConePathProblem is why p cannot be a directory of a repository's cone — ""
// when it can be one. A cone path is relative to the repository's root and
// already clean: no leading or trailing slash and no empty, "." or ".."
// component, so one directory has one spelling and none reaches outside the
// repository; valid UTF-8, because it crosses JSON on the run's row, which
// replaces an invalid byte, so two paths would arrive as one; no control
// character, since NUL, a newline and a carriage return each break the
// line-based file git keeps a cone in and none of the others is ever meant;
// no space at either end, which git trims from a cone's directory before it
// writes one (sparse-checkout's strbuf_trim), so " src" would check out src;
// and at most [MaxConePathBytes].
//
// EXPORTED FOR THE ONE OTHER PLACE A CONE IS WRITTEN: a repository's
// .crewlet/checks.yaml names the cone its check's box checks out
// (internal/gitlet's ParseChecks), and that cone becomes a box's clone step as a
// run's does, so a file is held to the rule the step is written under rather
// than to a second reading of it — one the box would refuse at setup, after
// the check had taken its box.
func ConePathProblem(p string) string {
	switch {
	case p == "":
		return "is empty"
	case len(p) > MaxConePathBytes:
		return fmt.Sprintf("is %d bytes, past the %d a cone's directory may have", len(p), MaxConePathBytes)
	case !utf8.ValidString(p):
		return "is not valid UTF-8"
	case strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "holds a control character"
	case strings.HasPrefix(p, " ") || strings.HasSuffix(p, " "):
		return "begins or ends with a space, which git trims from a cone's directory, so it would check out " +
			"another one"
	case path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../"):
		return "is not a clean path inside the repository: no leading or trailing slash, and no empty, " +
			"\".\" or \"..\" component"
	}
	return ""
}

// canonicalID reports whether s is a UUID other than the nil one, spelled as
// uuid.UUID.String spells it — the one spelling a row compares byte for byte.
func canonicalID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

// RunHandle points at a detached job.
//
// PERSISTED, which is the whole point: the command id and pid go into the
// pending-run row so a later turn — or a fresh engine after a restart — can
// reconnect to a job that is still running and collect its result.
type RunHandle struct {
	CommandID string
	PID       int

	// SessionID is the harness session the job was started in, named by the
	// runner BEFORE the harness starts — the one the job continues
	// ([RunRequest.Resume]), or the id it minted for a new one — so a job
	// killed before it reported anything still names its session. The job
	// itself says which it ran ([Result.SessionID], [Result.Continued]): one
	// asked to continue a session its box neither held nor could restore
	// starts a new one instead.
	SessionID string

	// FreshSessionID is that new one: the session of its own a job asked to
	// continue SessionID starts in its place, named by the runner before the
	// harness starts as SessionID is; empty for a job that continues none.
	// With which of the two its box said it started ([LaunchRecord.SessionStart]),
	// what names the session the job ran ([PendingRun.SessionRan]).
	FreshSessionID string
}

// Result is one launch's outcome.
type Result struct {
	Text    string
	Success bool

	// InputTokens is the run's WHOLE prompt count, its cached share
	// included, and CacheReadTokens / CacheWriteTokens are the share of it
	// the provider's prompt cache served and stored — the same convention
	// every engine provider reports on its completion, so a launch's
	// tokens fold into the spend rollup beside the phases the engine ran
	// itself. A breakdown, never an addition: the total is input + output.
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          float64

	// SessionID is the harness session the run worked in, as its harness reported it
	// or, where it reported nothing — or nothing shaped as a harness's session
	// id — as the run's script recorded it before the harness started.
	SessionID string

	// Continued says the run continued the session it was asked to
	// ([RunRequest.Resume]): its box held that session, or restored it from
	// the archive carried into it ([RunRequest.Carried]), and the harness took it
	// up. False for a run that started a session of its own — one asked to,
	// and one whose script found the session neither in its box nor
	// restorable, which starts a new one under another id and says why
	// ([Result.FellBack]) rather than let its harness create an empty session
	// under the old id and answer the brief as if it remembered.
	Continued bool

	// FellBack says why a run asked to continue a session started a new one
	// in its place, in the words its script said it in on its error stream —
	// the session was not in its box and none was carried in, the box's harness
	// is older than the release that continues a carried one, the archive
	// carried in could not be restored — so whoever reads what the run did
	// learns why it began again rather than finding a new session id alone.
	// "" for a run that continued its session, and for one asked to start
	// one. ONE SENTENCE, redacted and cut to [MaxFellBackBytes] where the
	// runner reads it and again as the launch's record is published
	// ([BoundFellBack]): the line it is read from is a file in the box.
	FellBack string

	// HarnessVersion is the release of the harness that ran, as its `--version` gave
	// it ("2.1.310"), read by the run's script in the box; "" where it
	// recorded none this build can read, one longer than the executor log
	// records a release at included.
	HarnessVersion string

	// Captured is the run's session as its box packed it when the harness
	// exited, to be carried into another box ([SessionArchive]); nil where
	// the box holds none — a run killed before its harness exited, a harness that
	// wrote no session, a box without the tools to pack one, which the run's
	// error stream says.
	Captured *SessionArchive

	// HarnessCutOff says the run's harness did not exit cleanly: its exit
	// status was other than 0 — a signal's, its memory limit's, a crash's —
	// or its box recorded none. The run's script packs the session after the
	// harness exits whatever its status (Captured), so a session it packed
	// after a harness that was killed may end mid-record, its last call with
	// no result, as one the engine packs of a job still working does
	// ([SessionCapture.Interrupted]).
	HarnessCutOff bool

	// MemoryKills is how many processes the kernel killed for memory in the
	// job's memory domain while its harness ran, and MemoryBytes the domain's
	// ceiling, both 0 where it killed none or kept no count. OutOfMemory is a
	// run whose harness was one of them: it ended at its box's memory
	// ceiling (types.ExecutorErrorOutOfMemory). Read off the box, so a run
	// that forges it costs only its own conversation, which a person releases.
	MemoryKills int64
	MemoryBytes int64
	OutOfMemory bool

	// JobEnd is how the job was ended on request, as its script recorded it
	// ([JobEnder], internal/sandbox/executor's Paths.JobEnded): a harness told
	// to stop, one killed past its grace, or a request that came before the
	// harness started; "" for a job nobody asked to end, and for a word this
	// build does not know. A run ended so did not finish, whatever it wrote:
	// it never reads as a success, and its failure says first that it was
	// ended.
	JobEnd JobEnd

	// Kept is what the read that collected this result kept of the job's
	// session ([Coordinator.takeCapture]), made on the node whose host the
	// box is on and carried back with the result to whichever node collects,
	// for the launch's own record; nil where nothing was taken. Set by the
	// coordinator, never by a runner.
	Kept *SessionKept

	// HarnessEnd is how the job's harness ended, as the read that collected
	// this result found it ([LaunchRecord.HarnessEnd]): recorded on the job's
	// record by that read, on the node whose host the box is on, and carried
	// back with the result for the claim it collects for, which was read
	// before the record was written ([Coordinator.collectClaimed]). Set by the
	// coordinator, never by a runner; empty from a host of an older build,
	// which is never read as an own run.
	HarnessEnd HarnessEnd

	// Turns is how many model turns the run's harness says its own loop took —
	// each one a provider call the engine neither made nor saw — and zero
	// where the harness reported none. It is what the launch's own record counts
	// as its calls (tokens.LaunchCalls): a run whose harness said nothing counts
	// as one, the one call the engine knows it asked for.
	//
	// AS REPORTED, never estimated: Claude Code states it on its result
	// line (`num_turns`); OpenCode's stream does not carry a count a
	// reader can trust — 2.0 prints a step's `step_start` for some steps
	// of a run and not others — so its runs report none.
	Turns int

	// DeliveredRefs are the branches and pull or merge requests the run
	// reported delivering, in its report's order: the `Delivered:` lines it
	// wrote, or the pull-request URLs its prose holds where it named none.
	// They are DISPLAY and a reminder — the launch's own record and the run's
	// report its reviewer reads carry them — and nothing judges a turn on
	// them: whether a turn reached anybody is the engine's own record of the
	// calls it made ([github.com/crewlet/crewlet/internal/agent/turn]).
	//
	// A runner hands them on AS THE ACCOUNT WROTE THEM, byte for byte: the
	// coordinator holds them to what the record can carry, for every runner
	// alike — redacted, deduplicated, valid UTF-8 and within
	// [MaxDeliveredRefBytes] — and it can only judge the bytes it is given.
	DeliveredRefs []string

	// DeliveredRefsElided is how many more refs the run reported than its
	// record lists: the refs past [MaxDeliveredRefBytes], and any that is not
	// valid UTF-8, which the record's JSON would turn into another ref. Set
	// by the coordinator.
	DeliveredRefsElided int

	// Pushed are the refs the run moved on Gitlet, read
	// from the push rows Gitlet wrote under its launch ([RunPushes]):
	// each repository the run was given to push to in turn, its refs in the
	// order the run first pushed them, held with the run's own refs to
	// [MaxDeliveredRefBytes] ([boundPushed]). PushedElided is how many more
	// the push record names than Pushed lists. PushedUnread is why the
	// record could not be read — set, the run's pushes are UNKNOWN, and an
	// empty Pushed beside it never means "none".
	//
	// The ENGINE'S account of the run's delivery there, which the run's own
	// `Delivered:` lines are not: a box writes those as it likes. Set by the
	// coordinator, never by a runner — whatever a runner put here is
	// replaced at the collection, which reads the record itself.
	Pushed       []PushedRef
	PushedElided int
	PushedUnread string

	// Opened is what became of each change the run asked to open
	// ([RunRepo.Open]), in the order it was given its repositories: opened
	// at this collection, one its branch already headed, or why none was
	// ([OpenedChange]). Set by the coordinator from the engine's account
	// ([RunPushes.Changes]), as Pushed is, and never by a runner.
	Opened []OpenedChange

	Error string

	// ContextExhausted says the run ended because its conversation no longer
	// fits the model's context, as its harness named the cause: Claude Code's
	// terminal reason `prompt_too_long`, OpenCode's ContextOverflowError. It
	// is what tells such a run from every other failed one on its record
	// (types.ExecutorErrorContextExhausted), and no later launch continues the
	// session it ran ([ranOutOfContext]). Read only on a run that did not
	// succeed. The box's word: a run that forges it costs only its own
	// conversation's session, whose transcript it could spoil anyway.
	ContextExhausted bool

	// Event is how the harness said its run ended IN FIELDS, beside the words of
	// Text and Error: its own terminal event, verbatim — Claude Code's
	// result line, whose `api_error_status` names a spent plan however the
	// sentence beside it is worded. It is what an executor's ending is
	// classified by first, as a call's is (harness.Provider.RunEnded), and
	// nothing else reads it. Set on a run that did not succeed, by a harness that
	// prints such an event; empty otherwise, and wherever the line is past
	// [MaxRunEventBytes] ([boundBox]).
	Event string

	// Transcript is the agent's activity log — what it said, each tool it
	// called and the first line of what it ran — rebuilt from its own
	// output, or its stderr where the harness streams nothing better. It is the
	// observability surface for an agent that emits no telemetry of its
	// own. A runner returns it WHOLE and redacted; the coordinator holds it
	// to the record's bound ([Coordinator.fitResult]) and redacts it again
	// at publish.
	Transcript string

	// TranscriptElidedLines and TranscriptElidedBytes are how much of the
	// transcript's middle the record's bound left out: whole lines, and
	// every byte not kept. Zero for a transcript kept whole. Set by the
	// coordinator, never by a runner.
	TranscriptElidedLines int
	TranscriptElidedBytes int

	// Condensed is what condensing this result's report, failure or
	// question to the record's bounds cost the seat's auxiliary model
	// ([Coordinator.fitResult]) — a failed rewrite included, since it was
	// still paid for. Set by the coordinator, never by a runner; the segment
	// that resumes from this collection pays it ([ResumeRequest.Engine]).
	Condensed AuxTokens
}

// usageFloored is r with every count of what the run spent made non-negative,
// and whether any had to be.
//
// A RUN REPORTS ITS OWN USAGE, parsed out of the harness's last line of
// output — and that line is printed inside a box the run's own agent can run
// any command in. A negative count there is a REFUND: the charge would
// subtract it from the seat's budget counter, and the launch's record from
// every rollup that sums it, so a run could buy itself headroom by printing a
// number. Nothing a run spends is negative, so a negative count is read as
// nothing spent, and the caller says so. Floored where every collected result
// enters the coordinator, so every backend's parser is covered by one rule.
func (r Result) usageFloored() (Result, bool) {
	floored := false
	floor := func(n *int) {
		if *n < 0 {
			*n, floored = 0, true
		}
	}
	floor(&r.InputTokens)
	floor(&r.OutputTokens)
	floor(&r.CacheReadTokens)
	floor(&r.CacheWriteTokens)
	floor(&r.Turns)
	if r.CostUSD < 0 {
		r.CostUSD, floored = 0, true
	}
	return r, floored
}

// Sandbox is one live, isolated execution environment.
type Sandbox interface {
	// ID is the provider's handle for this box, and what a reconnect needs.
	ID() string

	// Home is the absolute path this run's artefacts live under. A property
	// rather than a constant — see [DefaultHome].
	Home() string

	Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)

	// StartBackground launches a detached process and returns its handle
	// immediately. The job keeps running after the caller's turn ends and
	// writes its result to a file a later collect reads.
	StartBackground(ctx context.Context, cmd string, opts ExecOptions) (string, error)

	// Job reports what is known of the background job whose handle
	// StartBackground returned: still running, ended, or lost with the
	// engine process that last saw it running ([JobState]).
	//
	// ASKED OF THE BOX, because only the backend knows what its handle
	// names. Inside a remote VM or a container the handle is a pid in the
	// box's own process namespace, where nothing but the box's own work
	// can hold it. On the engine host it is a host pid, which the kernel
	// hands to any process once the job is gone: a probe that took the pid
	// on trust would read a stranger as the job and hold a dead run open
	// until the stranger exits. And only a box on the engine host can lose
	// its job with the engine: a remote box answers running or ended. An
	// error means the box could not be asked, which is not proof of any
	// answer.
	Job(ctx context.Context, commandID string) (JobState, error)

	// EndJob asks the job commandID names to END: SIGUSR1 to script, ONE
	// process of it — the run's script, by the pid it recorded in the box
	// (internal/sandbox/executor's Paths.JobScript). The script ends its
	// harness and packs its session as its own exit does ([JobEnder],
	// endjob.go). SIGUSR1 and not SIGTERM, which is a teardown's to every
	// process of the job at once, server and exporter with it, where an end
	// needs them alive to pack through.
	//
	// [ErrNotJobProcess] where script is no living process of the job: on the
	// engine host, no member of the job's recorded process group, held to it
	// engine-side ([procgroup.Leader.SignalMember]), since the pid is the job's
	// word and the job runs as the engine's own user; in a container or a VM,
	// no process of that pid in the box's own namespace. A pid below 2 is
	// refused unsent everywhere: 1 is the namespace's init, which in a
	// container forwards a signal to the container's main process. Any other
	// error is a box that could not be asked.
	EndJob(ctx context.Context, commandID string, script int) error

	// KillJob ends every process of the job commandID names and KEEPS the box
	// and its files for the read that follows: SIGTERM, [JobStopGrace] for
	// them to go, SIGKILL, [JobStopGrace] for the kernel to take them
	// ([JobKillWait]). On the engine host that is the job's recorded group,
	// continued first as Close continues it. In a container it is the job's
	// group in its container — recorded on the engine host as the job
	// started, or, for a job an older build started, which recorded none, read
	// off the job's first process where an exec's own session leads it — never
	// every process the box's user may signal, which would take the
	// container's main process and stop the container the read still execs
	// its pack in. In an E2B VM it is every process of the box's user: envd
	// starts a job in envd's own process group, which no signal may reach,
	// and runs as root, out of reach of the user's; and no job runs in the box
	// after it — a stop's and a loss's ending reclaims it right after, and a
	// collection past its turn's cap pauses it for a turn that starts no round
	// past that cap.
	//
	// NEVER A PART OF THE JOB FOR THE WHOLE: a box that can name no process
	// group of the job's own signals nothing of it and answers [ErrNoJobGroup].
	// The job's first process alone is no kill of the job — the run's script
	// and its harness are that process's children, and go on without it — and
	// it is the process a box answers [Sandbox.Job] by, so a kill of it alone
	// had a job still working read as one that had ended.
	//
	// A job that is already over is nothing to kill. Any other error is a box
	// that could not be asked; whether what it killed is over is the job's
	// poll to say.
	KillJob(ctx context.Context, commandID string) error

	// WriteFile puts content at path whole, by REPLACING the file there: the
	// content is staged under a fresh name beside the path and renamed over
	// it, so a process in the box reading the file meanwhile — a git
	// credential helper reading the token file the completion poll keeps
	// current under a running job — finds the old file or the new one,
	// never an empty or part-written one. A write that fails removes what it
	// staged, or says it could not, where a remote box stopped answering.
	// The new file has the mode the backend writes every file with,
	// whatever the replaced one had.
	//
	// A backend that writes the box from the engine host replaces only a
	// regular file where it lies, or puts one where nothing is, and refuses
	// anything else at the path with [ErrNotRegularFile], leaving it as the
	// box made it: the rename writes through no link, and a named pipe is
	// never opened to be waited on. A remote box is a machine of its own, so
	// what is at the path there is replaced, a link included.
	WriteFile(ctx context.Context, path string, content []byte) error

	// WriteStream puts what content yields at path whole, exactly as
	// [Sandbox.WriteFile] puts a byte slice there — staged beside the path
	// and renamed over it, the same refusals on the engine host — for content
	// the engine must not hold in memory: a harness's session archive,
	// carried into a box from the object store, which grows with the session
	// it packs.
	//
	// READ ONCE, A PIECE AT A TIME, and only while ctx lasts: a write whose
	// caller gave up stops at the next piece rather than reading its source
	// to the end. A source that fails part way fails the write, and the file
	// at the path is the one that was there before — the staged one is
	// removed, never moved over it — so an archive cut short in transit is
	// never a file the box reads as whole. The caller keeps content: the
	// write neither closes it nor reads it once it has returned.
	WriteStream(ctx context.Context, path string, content io.Reader) error

	// ReadFile reads a file the engine means to read WHOLE — a report, a
	// question, a marker, a result line — and REFUSES one past
	// [MaxFileBytes] ([ErrFileTooLarge], as a [*FileTooLargeError] naming the
	// cap it refused at) rather than returning its first part, because a
	// clipped report reads as a finished one. Empty on missing: a poll for a
	// marker that is not written yet is not an error.
	//
	// NEVER FOR A MACHINE STREAM. A harness's event log and its stderr
	// grow with the run and have no size a whole read could honestly refuse
	// at — the engine keeps a bounded share of them in the end — so they
	// are read with [Sandbox.OpenFile] or [Sandbox.ReadTail] instead.
	//
	// A path that names something other than a regular file is refused with
	// [ErrNotRegularFile] by a backend that reads the box from the engine
	// host, for all three reads, because opening one there could block.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// OpenFile streams a file front to back, for a MACHINE STREAM decoded
	// once in bounded memory: a harness's event log, read at
	// collection a line at a time. Nothing is refused for its size, since
	// the reader holds one piece at a time and decides itself what to keep.
	// A missing file is a reader that yields nothing, as ReadFile's empty
	// answer is; the caller closes it.
	OpenFile(ctx context.Context, path string) (io.ReadCloser, error)

	// ReadTail reads at most n bytes from the END of a file, with the
	// file's whole size — for a question about a stream's end: what is the
	// job doing now, how far has its stream grown, what did it say last
	// before it failed. A whole read answers each of those at the cost of
	// the whole stream, every poll, for as long as the run lasts. Empty on
	// missing, as the others are.
	ReadTail(ctx context.Context, path string, n int) (FileTail, error)

	// OpenTail streams at most the last n bytes of a file — from where its
	// end less n stood when it was opened — with the file's whole size then,
	// for a stream's END kept whole rather than held: a failed check's log,
	// the output's last bytes put into the object store as they arrive. Its
	// cost is the n bytes, never the file before them: a remote box sends
	// only the range, a box on the engine host seeks. OpenFile with the
	// earlier bytes read and dropped cost a check that wrote gigabytes their
	// whole transfer from the box to keep its last tens of MiB. A missing
	// file is a reader that yields nothing and a size of 0, as the others
	// answer; the caller closes it.
	OpenTail(ctx context.Context, path string, n int64) (io.ReadCloser, int64, error)

	// SetTimeout resets the box's wall-clock TTL to seconds from now.
	//
	// THE KEEPALIVE. A provider reclaims a box that many seconds after the
	// TTL was last set, and the waiter calls this every poll tick — so a
	// running box is bounded only by how long the engine can go WITHOUT a
	// heartbeat, never by a fixed run deadline — as the launch does while it
	// provisions the box, on another goroutine than the one running the
	// box's setup steps, so a backend answers it beside a command in flight.
	// A provider with no settable TTL no-ops.
	SetTimeout(ctx context.Context, seconds float64) error

	// Pause snapshots the box for a later resume, holding a run blocked on
	// a clarification with its working tree as it left it, uncommitted work
	// included, and its harness's own copy of the conversation. Where the
	// turn kept the asking job's session (session.go), the relaunch continues
	// that in this box or any other (restore.go), so the pause holds only
	// the working tree; where it kept none — keeping switched off, the
	// capture refused — the paused box is the one copy of the conversation,
	// and a relaunch after it is reclaimed starts a session of its own. A
	// provider without snapshots no-ops, and the relaunch re-seeds its
	// working tree from the pushed branch instead.
	Pause(ctx context.Context) error

	Close(ctx context.Context) error
}

// FileTail is the end of a file, read by [Sandbox.ReadTail].
type FileTail struct {
	// Data is the file's last bytes — all of it when the file is no longer
	// than what was asked for. It begins wherever the byte count put it, so
	// a reader of lines drops a partial first one ([FileTail.Lines]).
	Data []byte

	// Size is the whole file's size as the read found it, so a caller can
	// say how much came before Data rather than present the end as the
	// whole.
	Size int64
}

// Whole reports whether Data is the entire file.
func (t FileTail) Whole() bool { return int64(len(t.Data)) >= t.Size }

// Before is how many bytes of the file came before Data and were not read.
func (t FileTail) Before() int64 { return max(t.Size-int64(len(t.Data)), 0) }

// Lines is Data without a first line the window began inside, and how many
// bytes that partial line held. A tail that is the whole file begins at a
// line, so nothing is dropped from it; one that began mid-file begins
// wherever the byte count landed, and the bytes before its first line break
// are the end of a line nobody can read whole from here.
func (t FileTail) Lines() ([]byte, int) {
	if t.Whole() {
		return t.Data, 0
	}
	i := bytes.IndexByte(t.Data, '\n')
	if i < 0 {
		return nil, len(t.Data)
	}
	return t.Data[i+1:], i + 1
}

// tailOf is the end of a file held in memory, in the shape every backend's
// [Sandbox.ReadTail] answers.
func tailOf(content []byte, n int) FileTail {
	n = max(n, 0)
	if len(content) <= n {
		return FileTail{Data: content, Size: int64(len(content))}
	}
	return FileTail{Data: content[len(content)-n:], Size: int64(len(content))}
}

// ExecOptions are the per-command knobs both exec shapes take.
type ExecOptions struct {
	Env        map[string]string
	Cwd        string
	TimeoutSec float64
}

// JobState is what is known of a detached job ([Sandbox.Job], [Runner.Poll]).
//
// THREE ANSWERS, because "not running" is two facts that a run is carried
// forward from differently. A job that ENDED finished, or died while an engine
// that had seen it running was still there to see it go: what it left is the
// round's result, a crash included, and it is collected. A job that was LOST
// went with the engine process that last saw it running, before any engine
// could see it end: a `direct` box's process tree is the engine host's, and
// the restart that replaces the engine — its container restarted, its systemd
// unit stopped with every process in its control group, its host rebooted —
// kills the job in the same instant, mid-step, with nothing written to say so.
// What it left is not a result, and its round is carried on rather than
// collected ([PendingRun.Lost]).
type JobState string

const (
	// JobRunning is a job still working — or hung, which nothing tells apart
	// from working without a clock the engine deliberately does not impose.
	JobRunning JobState = "running"
	// JobEnded is a job that is over and was seen to end: it wrote its done
	// marker, or its process went while an engine that had watched it run was
	// still there.
	JobEnded JobState = "ended"
	// JobLost is a job that is over and that nothing alive saw end: it went
	// with the engine process that last saw it running, or after it, before
	// the engine that replaced it could look.
	JobLost JobState = "lost"
)

// JobStates are the three.
var JobStates = []JobState{JobRunning, JobEnded, JobLost}

// Valid reports whether this build knows s.
func (s JobState) Valid() bool { return slices.Contains(JobStates, s) }

// Over reports whether the job is no longer running, whichever way it went.
func (s JobState) Over() bool { return s == JobEnded || s == JobLost }

// ErrBoxGone is a [Provider.Connect] that found the box DEFINITIVELY not
// there — reclaimed by its provider, reaped, its directory removed — as
// against one that merely could not be reached. The difference decides
// whether asking again can help: a collection that cannot reach a box is
// retried, and one whose box is gone is settled at once.
var ErrBoxGone = errors.New("sandbox: the box is gone")

// boxGone is a backend's own sentence saying a box no longer exists, which is
// also [ErrBoxGone] — the sentence is what a person reads, the sentinel what
// a caller acts on.
type boxGone struct{ err error }

func (g boxGone) Error() string   { return g.err.Error() }
func (g boxGone) Unwrap() []error { return []error{g.err, ErrBoxGone} }

// ErrBoxPaused is a [Provider.Attach] that found the box PAUSED where its
// backend cannot read a paused box without waking it — an E2B snapshot, which
// has no envd to answer, or a paused container, which takes no exec. The box
// is there; reading it would mean resuming it, which is the one thing the
// caller asked not to do.
var ErrBoxPaused = errors.New("sandbox: the box is paused")

// boxPaused is a backend's own sentence saying a box is paused, which is also
// [ErrBoxPaused].
type boxPaused struct{ err error }

func (p boxPaused) Error() string   { return p.err.Error() }
func (p boxPaused) Unwrap() []error { return []error{p.err, ErrBoxPaused} }

// Provider mints sandboxes. Configured under providers.sandbox and swapped
// wholesale on an apply, mirroring the LLM providers beside it.
type Provider interface {
	// Kind names the backend, for logs and the operator surface.
	Kind() string

	Create(ctx context.Context, spec Spec) (Sandbox, error)

	// Connect reattaches to an existing box by id, live or paused.
	//
	// The detached lifecycle rests on this: the completion turn — possibly
	// in a fresh engine after a restart — reattaches to the box that ran the
	// job, collects its result and tears it down. A PAUSED box auto-resumes
	// on connect, which is why the reaper must not use it, and why neither
	// may a reader ([Provider.Attach]).
	Connect(ctx context.Context, sandboxID string) (Sandbox, error)

	// Attach reattaches to an existing box by id WITHOUT RESUMING IT, for a
	// caller that only reads: a peek at a running job's output, and the
	// completion poll.
	//
	// THE READ MUST CHANGE NOTHING, and through Connect it could not: a
	// reader that took a running record, then reached the box after the
	// collection had read it and paused it, woke the box back up — and on
	// E2B, where nothing renews a box once its record stops running, the
	// woken box was killed at its TTL and the parked run lost the snapshot
	// it had been paused to keep. A box this backend cannot read while it
	// is paused is refused with [ErrBoxPaused]; one whose files the engine
	// host reads directly is handed back as it is, paused or not. A box
	// that is gone is [ErrBoxGone], as for Connect. Nothing about the box's
	// timer or its state moves.
	Attach(ctx context.Context, sandboxID string) (Sandbox, error)

	// Kill terminates a box by id WITHOUT resuming it.
	//
	// The primitive the pause reaper needs. Connect auto-resumes, so
	// reclaiming a paused snapshot through it would boot the VM back up
	// purely to kill it — paying for a resume to pay for a shutdown.
	// Best-effort: a box that is already gone is not an error.
	Kill(ctx context.Context, sandboxID string) error
}

// Runner runs one harness inside a box against a brief.
//
// TWO SHAPES, ONE RUNNER: the inline Run (start, block, result — for tests and
// short jobs) and the detached Start/Poll/Collect triple the engine drives
// across turns. Run is Start followed by Collect on the same handle.
type Runner interface {
	Name() string

	// Install puts the agent in the box. Separate from Create because a
	// reused box already has it, and re-installing on every turn is the
	// slowest thing in a coding turn's critical path.
	Install(ctx context.Context, box Sandbox) error

	Start(ctx context.Context, box Sandbox, req RunRequest) (RunHandle, error)

	// Poll reports what is known of the background job: running, ended —
	// finished, or seen to die — or lost with the engine that last saw it
	// running ([JobState]).
	Poll(ctx context.Context, box Sandbox, handle RunHandle) (JobState, error)

	// Collect reads the finished job's result out of the box.
	Collect(ctx context.Context, box Sandbox, handle RunHandle) (Result, error)

	// Follow begins a LIVE READING of what a job says about itself, for a
	// person watching the run ([LiveReading]): advanced a read at a time,
	// each read adding what the job has written since, settled and
	// redacted, without ending, pausing or otherwise touching the job. Safe
	// on a job in any state: one that has finished says so, and one that
	// has said nothing yet reads as nothing rather than as an error.
	Follow(handle RunHandle) LiveReading
}

// CallIdentifier is a [Runner] whose harness hands each tool call it makes over
// the bridge an id of its own, in the request's `_meta`: CallIDMeta is the key
// it sends the id under, the one the bridge reads ([BridgeCall.CallID],
// [OpenedJob.CallIDMeta]) — `claudecode/toolUseId` for Claude Code — and ""
// for a harness that sends none.
//
// OPTIONAL, as [SessionTaker] is: a runner that declares no key — a harness
// whose `_meta` carries no id of the call, OpenCode's, or a test's — has every
// call of its runs recorded with none, each read by its place as every call an
// older build recorded is. EXPORTED so a runner asserts it implements it, as
// internal/sandbox/executor's does: a method renamed out from under it would
// read as a harness that declares nothing, and every id would be dropped
// without a word.
type CallIdentifier interface{ CallIDMeta() string }

// RunRequest is one launch's inputs.
//
// NEVER PERSISTED: a run is started once, by the node that launched it, and
// everything a later collection or resume needs is on the run's row and in the
// box. So a field added here changes no record a peer of another build reads.
type RunRequest struct {
	// System is the run's standing instructions, which the runner hands its
	// harness on the channel that ADDS to the harness's own system prompt rather than
	// replacing it: the vendor's prompt is what teaches the harness this run
	// exists to use. A runner adds its own standing instruction to it — the
	// report the run writes before it stops, at the box's path for it.
	System string

	// Brief is the task: the prompt the harness runs, and nothing standing.
	Brief string

	// Resume is the harness's own session the run CONTINUES, as an earlier run in
	// the same box reported it ([Result.SessionID]): the conversation, its
	// tool results and its working files are where that run left them, and
	// Brief is its next message — Claude Code's `--resume <id>`, OpenCode's
	// `--session <id>`. A seat's job sets it on every relaunch of its turn that
	// continues the turn's session — the turn's kept capture, in whatever box
	// the job gets ([LaunchRequest.Session]), or the session the asking job's
	// box holds ([LaunchRequest.Continue]); the assistant's deep work, where
	// its run parked on a proposal and the approval has come. Empty starts a
	// new session.
	//
	// THE HARNESS'S SESSION AND NOT A SUMMARY OF IT, because the run that asked
	// knew what it had read, tried and decided, and a fresh conversation
	// told only the question and the answer starts that work again. It holds
	// in the box the session lives in, or in a box the session is carried
	// into ([RunRequest.Carried]). A runner whose harness cannot continue a
	// session refuses a run that asks it to, rather than start a new
	// conversation its caller believes is the old one; and a run whose box
	// turns out to hold the session neither way starts a new one and says so
	// ([Result.Continued]).
	Resume string

	// Carried is the session Resume names, packed in another box
	// ([SessionArchive]), for this run's box to restore before its harness takes
	// it up: written into the box as the run starts, after the previous
	// run's artefacts are cleared, and restored by the run's own script — or
	// not, in a box whose own copy of the session already holds this archive
	// (one the box packed, or restored before), which is never written over:
	// that copy is the archive's or newer — unless the archive is one its
	// harness was cut off in ([CarriedSession.Replace]), which replaces the
	// copy whatever it holds. A box holding a copy that does not hold the
	// archive has it replaced by the archive. Nil for a run that
	// continues a session its box already holds, or none. Its session must
	// be Resume's, and its harness and format the runner's, or the run is
	// refused.
	Carried *CarriedSession

	// FreshBrief is the brief a run asked to continue a session is started
	// with INSTEAD of Brief where its script finds the session neither held
	// by its box nor restorable from Carried — an archive that does not
	// unpack, an import its harness refused, a release that cannot continue
	// it — and starts a new session in its place ([Result.Continued]): Brief
	// is then a continuation written for a model that remembers, which a new
	// session would answer as if it did. So the run carries both, and its
	// script hands its harness the one its restore's outcome chose. Empty
	// where Resume is: a run that starts a session is handed Brief.
	FreshBrief string

	Env    map[string]string
	Limits Limits

	// Model is the model the run works under, written in the grammar of the
	// harness the runner drives: the seat's providers.llm entry's model exactly
	// as its runs on the engine host hand it to that same harness, which is the
	// only harness the engine lets reach this runner. Empty leaves the agent's
	// own choice.
	//
	// NO ENDPOINT AND NO KEY: an entry talks to its harness's own vendor, and its
	// sign-in rides the run env, so nothing secret is written into a file
	// inside the box.
	Model string

	// MCPServers are the MCP servers the engine hands the run, keyed by
	// server name: its tool bridge, and the seat's servers that run in its
	// box. SERVER-LEVEL ONLY — there is no per-tool allowlist, because the
	// agent inside the box negotiates its own tool list with the server and
	// an allowlist the engine could not enforce would be a claim rather than
	// a control.
	MCPServers map[string]MCPServer

	// OnlyMCPServers holds the run to MCPServers alone: no server its
	// harness's own configuration, a repository's or a plugin's declares,
	// and no connector of the account it signs in with, starts beside them
	// (Claude Code's --strict-mcp-config).
	//
	// The assistant's deep work sets it: it acts for a person, confined on
	// the engine host, and reaches exactly the tools it is handed. A seat's
	// executor leaves it false, and its harness starts what it would start
	// for a person running it — the box's own configuration's servers, the
	// repository's, a plugin's, the signed-in account's connectors — beside
	// the ones the engine hands it: what those reach is the run's cell's to
	// bound, never the engine's to decide. A runner whose harness has no way
	// to be held to the servers it is handed refuses a run that sets it.
	OnlyMCPServers bool

	// Steer is how the run's harness reaches a person's notes to its turn while
	// it works — see [SteerWiring]. The zero value takes no note inside the
	// run: its notes wait for whatever reads the turn next, which is what a
	// run with no tool bridge — the assistant's deep work — has. NEVER
	// PERSISTED: it names the job's own endpoint, and a resumed turn's next
	// launch is handed its own.
	Steer SteerWiring

	// ReportedRepos are the repositories on Gitlet this
	// run may push to ([Pushable]), by the names it was given them under.
	// What it pushes there the engine reads from the push record and reports
	// for it ([Result.Pushed]), so a runner tells its agent to write a
	// `Delivered:` line only for what it delivered somewhere else: a line
	// for a push the record already names is the same delivery said twice.
	ReportedRepos []string

	// Skills are skills the run's harness is handed as its OWN — see [Skill]:
	// written where the harness reads skills of its own, listed to its model
	// beside the ones the box and the checkout hold, and loaded with the
	// harness's own skill tool. Nil hands none. A runner refuses a run handed
	// skills that its harness cannot take, or with nowhere to report a load
	// ([RunRequest.SkillLoads]).
	Skills []Skill

	// SkillLoads is where the run's harness reports loading one of Skills —
	// see [SkillWiring]. NEVER PERSISTED, for the reason Steer is not: it
	// names the job's own endpoint.
	SkillLoads SkillWiring

	// FreshWiring is how the run's harness reaches the seat's tools where its
	// script starts a session of its own in place of the one Resume names:
	// MCPServers, Steer and SkillLoads as they are but under the job's OTHER
	// bridge token, the one the bridge reads as the box having started a
	// session of its own (internal/api/mcpbridge). The run's script installs
	// exactly one of the two once its restore has come to whatever it came
	// to, and removes the other from the box before its harness starts, so the
	// token its harness dials IS the box's word on which session it started —
	// one the run's model can neither forge nor take back, holding only the
	// token of the session it is in. Nil where Resume is empty, which a
	// runner refuses a run that sets it for, and where the run has no bridge
	// to tell (the assistant's deep work). NEVER PERSISTED, for the reason
	// Steer is not.
	FreshWiring *Wiring

	// Attribution is the credit every commit the run authors as itself
	// carries — see [CommitAttribution]. Nil credits nothing: the run's git
	// is left exactly as the box and the repository configure it.
	Attribution *CommitAttribution
}

// SkillNamespace is what a run's harness lists every skill of
// [RunRequest.Skills] under: `crewlet:<name>` ([Skill.Listed]).
//
// ONE NAME ON EVERY HARNESS. Claude Code names a plugin's skill
// `<plugin>:<skill>`, and the plugin a runner hands it its skills in is named
// this; OpenCode names a skill by its directory, and a runner names each
// directory in full. So the engine can tell a run's model which skill to load
// without knowing which harness runs it, and a load the harness reports is
// read back by the name it listed. And NOT A NAME A REPOSITORY'S OR AN IMAGE'S
// OWN SKILL TAKES by accident, which, sharing one, the harness would run in
// the company's place or the company's in its.
const SkillNamespace = "crewlet"

// Skill is one skill a run's harness is handed as its own.
type Skill struct {
	// Name is the skill's own name ([ValidSkillName]), which the harness
	// lists it under in [SkillNamespace] ([Skill.Listed]).
	Name string

	// Description is what the harness lists beside the name, for its model
	// to choose by: the harness hands a model a skill's body only when the
	// model loads it.
	Description string

	// Body is the skill's text, which the harness hands its model once the
	// model loads the skill — AS THE HARNESS READS A SKILL OF ITS OWN, since
	// that is what it is: Claude Code reads a skill's file as a template, in
	// which a line's `` !`command` `` runs as the skill loads and
	// `$ARGUMENTS` is a placeholder, where OpenCode hands its model the text
	// as written.
	Body string

	// Required says the run's calls to the tools this skill covers are
	// refused until the harness reports loading it. A runner refuses a run
	// with a required skill on a harness release that cannot report a load:
	// it could never call those tools.
	Required bool
}

// Listed is the name the run's harness lists the skill under, and its model
// loads it by.
func (s Skill) Listed() string { return SkillNamespace + ":" + s.Name }

// maxSkillName is the longest [Skill.Name]: the Agent Skills specification's
// bound on a skill's name, which both harnesses read a skill's name by.
const maxSkillName = 64

// ValidSkillName reports whether name can be a [Skill.Name]: one to
// [maxSkillName] lowercase ASCII letters and digits, in runs joined by single
// hyphens — the Agent Skills specification's grammar. It is a directory's
// name in the box on either harness, and part of what the harness lists; a
// name outside it is one a harness may refuse to load, or one that names a
// path rather than a skill.
func ValidSkillName(name string) bool {
	if name == "" || len(name) > maxSkillName || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && name[i-1] != '-':
		default:
			return false
		}
	}
	return true
}

// SkillWiring is where a run's harness reports loading one of its skills
// ([RunRequest.Skills]): one tool of the MCP server the run's tool bridge is
// ([RunRequest.MCPServers]), which a hook of the harness's calls once its skill
// tool has answered (Claude Code), and the URL of the bridge's skill route,
// which a plugin of the harness's own posts each load to (OpenCode). The
// engine fills it from the same names its bridge serves, so the box and the
// bridge cannot disagree about them (internal/api/mcpbridge).
type SkillWiring struct {
	// Server is the name the bridge is listed under in the run's MCP
	// configuration, and Tool the tool a hook calls on it — never listed,
	// so no model is offered it, and no model can unlock a skill it never
	// read.
	Server string
	Tool   string

	// URL is the job's skill route: the bridge endpoint the box already
	// dials, with the route's suffix.
	URL string
}

// Set reports whether the wiring names anything a harness could reach.
func (w SkillWiring) Set() bool { return w.Server != "" && w.Tool != "" && w.URL != "" }

// CommitAttribution asks a run's runner to credit Crewlet on every commit the
// run authors as itself: `Co-authored-by: Crewlet <noreply@crewlet.ai>`, the
// one trailer GitHub renders as a participant in the commit; where Seat is
// set, the seat that made the commit and the work item its turn is charged
// to; and, where Run is set, the run that wrote it, which leads a reader of
// the history back to the run's page.
//
// GIT ADDS IT, NOT THE MODEL. The runner installs a hook in the box that git
// runs on every commit the run makes (internal/sandbox/executor's
// attribution.go), so the credit is there whether or not the agent remembered
// an instruction — and only on a commit whose author is the identity the run
// commits as, so a person's commit the run amends, cherry-picks or rebases is
// never credited to Crewlet.
//
// A CLAIM, NEVER EVIDENCE: the box can delete the hook or write any trailer
// it likes, so nothing in the engine reads a pushed commit's trailers — a box
// push's provenance is stamped from the run's own row at its admission. The
// trailers carry that provenance where the row does not reach: a mirror of
// the repository, and a commit older than Gitlet's push horizon.
type CommitAttribution struct {
	// Run is what the run's commits name it by, the identifier its page is
	// addressed by: a seat's job is its turn's, so every job of one turn —
	// a relaunch included — names the one run. Empty names no run — a run
	// with no page of its own — and the commit names none. One line of
	// printable text, since it becomes a trailer's value.
	Run string

	// Seat is the handle of the seat the run is the executor of, and
	// WorkItem the item its turn is charged to (nil for none, which the
	// commit says as "none" rather than leaving out, since a rewritten
	// commit's trailer is replaced, never kept). Empty for a run that is no
	// seat's — deep work for a person — which names neither; a WorkItem
	// without a Seat, or one with no backend, is the engine's own bug and is
	// refused.
	Seat     string
	WorkItem *types.WorkItem
}

// Wiring is how a run's harness reaches the seat's tools and the engine's: its
// MCP servers, the tool bridge among them, where it asks for its turn's notes,
// and where it reports a skill it loaded — the three parts of a run's request
// that name the job's bridge endpoint ([RunRequest.FreshWiring]).
type Wiring struct {
	MCPServers map[string]MCPServer
	Steer      SteerWiring
	SkillLoads SkillWiring
}

// SteerWiring is where a run's harness asks for its turn's notes: one tool of the
// MCP server the run's tool bridge is ([RunRequest.MCPServers]), which a hook
// of the harness's calls between its steps, and the URL of the bridge's notes
// route, which a harness that admits notes from a plugin of its own polls. The
// engine fills it from the same names its bridge serves, so the box and the
// bridge cannot disagree about them (internal/api/mcpbridge).
type SteerWiring struct {
	// Server is the name the bridge is listed under in the run's MCP
	// configuration, and Tool the tool a hook calls on it — never listed,
	// so no model is offered it.
	Server string
	Tool   string

	// URL is the job's notes route: the bridge endpoint the box already
	// dials, with the route's suffix.
	URL string
}

// Set reports whether the wiring names anything a harness could reach.
func (w SteerWiring) Set() bool { return w.Server != "" && w.Tool != "" && w.URL != "" }

// Run is the inline shape: start, wait, collect.
//
// A helper rather than an interface method, so a Runner implements three
// primitives instead of four and the two shapes cannot drift apart — the
// blocking one IS the detached one with a wait in the middle.
func Run(ctx context.Context, r Runner, box Sandbox, req RunRequest, wait WaitFunc) (Result, error) {
	handle, err := r.Start(ctx, box, req)
	if err != nil {
		return Result{}, err
	}
	if err := wait(ctx, func(ctx context.Context) (bool, error) {
		state, err := r.Poll(ctx, box, handle)
		return state.Over(), err
	}); err != nil {
		return Result{}, err
	}
	return r.Collect(ctx, box, handle)
}

// WaitFunc blocks until a predicate holds or the context ends. Injected so the
// inline shape's polling cadence is the caller's choice — a test wants none.
type WaitFunc func(ctx context.Context, done func(context.Context) (bool, error)) error

// probeByKill answers whether a job is running for a box whose handle is a pid
// in the box's own process namespace, by running `kill -0` inside it.
//
// The handle is parsed rather than interpolated: it is read back from the run's
// record in the coordination store, and a shell command is the wrong place to
// find out it was not a number.
func probeByKill(ctx context.Context, box Sandbox, commandID string) (bool, error) {
	pid, err := strconv.Atoi(commandID)
	if err != nil || pid <= 0 {
		return false, fmt.Errorf("sandbox: job handle %q is not a process id", commandID)
	}
	res, err := box.Exec(ctx, "kill -0 "+strconv.Itoa(pid)+" 2>/dev/null", ExecOptions{})
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// ErrNotJobProcess is an end request ([Sandbox.EndJob]) refused unsent: the pid
// it names is no living process of the job.
var ErrNotJobProcess = errors.New("sandbox: the pid is no living process of the job")

// ErrNoJobGroup is a kill ([Sandbox.KillJob]) that signalled nothing, because
// its box can name no process group that is the job's own: a container job
// whose start recorded none, and whose first process is gone or in a group no
// exec's session of its own leads. What is left of such a job runs on until
// its box is reclaimed, and whether it is over is its poll's to say.
var ErrNoJobGroup = errors.New("sandbox: the box names no process group of the job's own, " +
	"so nothing of it was signalled")

// The words [endScript] answers in, one for each of its two outcomes, so an
// exec that never ran the script — a container runtime that is down, a box
// whose agent dropped the stream — is told from a pid that is not there.
const (
	endSent = "crewlet-end-sent"
	endGone = "crewlet-end-gone"
)

// endScript is the shell line that sends pid the end request (SIGUSR1) in a box
// whose pids are its own namespace's, and says which way it went.
//
// THE `-SIG` SPELLING, `kill -USR1 <pid>`, which dash, bash and busybox ash
// all read the same way (end-job-scratch/exp-kill.sh).
func endScript(pid int) string {
	return "if kill -USR1 " + strconv.Itoa(pid) + " 2> /dev/null; then echo " + endSent +
		"; else echo " + endGone + "; fi"
}

// endByKill answers [Sandbox.EndJob] for a box whose pids are its own
// namespace's — a container and an E2B VM: script is signalled in the box,
// where nothing but the box's own processes can hold its pid, so a pid that
// names nothing there is [ErrNotJobProcess]. A pid below 2 is refused before
// the box is asked. Run from the box's home, a directory the job cannot take
// away as it can its checkout, which an exec's working directory must exist in.
func endByKill(ctx context.Context, box Sandbox, script int) error {
	if script < 2 {
		return fmt.Errorf("%w: pid %d is no process an end request is sent to", ErrNotJobProcess, script)
	}
	res, err := box.Exec(ctx, endScript(script), ExecOptions{Cwd: box.Home()})
	if err != nil {
		return fmt.Errorf("sandbox: asking pid %d in box %s to end its job: %w", script, box.ID(), err)
	}
	switch lastLine(res.Stdout) {
	case endSent:
		return nil
	case endGone:
		return fmt.Errorf("%w: box %s has no pid %d to signal", ErrNotJobProcess, box.ID(), script)
	}
	return fmt.Errorf("sandbox: box %s did not run its end request (exit %d): %s", box.ID(),
		res.ExitCode, strings.TrimSpace(res.Stderr))
}

// lastLine is the last non-empty line of out, trimmed.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// killScript is the shell script that ends every process target names — a
// pid, or `-<pgid>` for a process group — in the shape [Sandbox.KillJob]
// promises: SIGTERM, up to [JobStopGrace] for them to go, SIGKILL, and up to
// [JobStopGrace] more for the kernel to take them, each wait over as soon as
// `kill -0` finds nothing of target left. A target with nothing left to
// signal is done at once.
//
// THE `-SIG` SPELLING, which reads a negative operand as a pid in dash, bash
// and busybox ash alike; dash takes `kill -s TERM -<pgid>` for an option it
// does not know, and wants a `--` before the pid that busybox ash need not
// read the same way (end-job-scratch/exp-kill.sh).
func killScript(target string) string {
	return killAndAwait(target, "kill -0 "+target+" 2> /dev/null")
}

// killAndAwait is a kill in [killScript]'s shape whose waits last while the
// shell condition alive holds — what is left of target — and no longer than
// a grace each. Its output goes nowhere: a box's agent that stops listening
// when its own process ends would otherwise end the script on its next write.
func killAndAwait(target, alive string) string {
	await := fmt.Sprintf(`i=0; while %s && [ "$i" -lt %d ]; do sleep 0.1; i=$((i+1)); done`,
		alive, int(JobStopGrace/(100*time.Millisecond)))
	return fmt.Sprintf("exec > /dev/null 2>&1\nkill -TERM %s || exit 0\n%s\nkill -KILL %s\n%s\n",
		target, await, target, await)
}

// remoteJob answers [Sandbox.Job] for a box whose job runs in a machine of its
// own, which the engine's restart does not reach — an E2B VM, and the fake that
// stands in for one: running while `kill -0` there says so ([probeByKill]),
// and ended otherwise. Such a job is never lost with the engine: it outlives
// every engine process, and whichever one finds it gone found it gone.
func remoteJob(ctx context.Context, box Sandbox, commandID string) (JobState, error) {
	running, err := probeByKill(ctx, box, commandID)
	switch {
	case err != nil:
		return "", err
	case running:
		return JobRunning, nil
	}
	return JobEnded, nil
}
