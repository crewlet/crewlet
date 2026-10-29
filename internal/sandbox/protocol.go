// Package sandbox is the code-work runtime: a sandbox-enabled seat runs its
// executor phase as a coding agent inside an isolated box instead of the native
// tool loop.
//
// The shape that matters is that a coding run is DETACHED. The tool starts it,
// the Execute loop SUSPENDS, and the engine RESUMES the same loop with the
// result spliced in when the run completes — possibly minutes later, possibly
// in a different process after a restart, possibly on a different node. Nothing
// here parks a goroutine on a running job: a goroutine cannot survive a
// restart, and a coding agent legitimately runs for longer than a deployment
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
// LET GO back to the seat's inbox through an outbox on the row
// ([PendingRun.HandBack]): after its attempts are spent, by an ending no turn
// reached, or by the seat's next holder reaping a claim whose node stopped
// before its turn took it ([Coordinator.RecoverSeat]) once the claims of that
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
// ([CoordinatorOptions.Spent]).
//
// AND A LOST RUN IS ANNOUNCED ONCE: the announcement goes out before the record
// is deleted, under the identity its decided ending recorded, so whichever node
// finishes the ending publishes the same event ([Coordinator.announceEnding]).
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
// author, its kind and the credential it came through ([RecordedAnswer.By]) —
// on the row and on every announcement of what became of the answer, never as a
// seat beside an actor.
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
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
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

// Limits are the caps handed to a coding agent.
//
// Zero means UNSET for each field, and the runner falls back to the agent's own
// default. Note what is not here: a run deadline. The engine imposes no
// wall-clock limit on a coding job — it runs as long as it needs, and
// completion is detected by tracking the job rather than by a clock. There WAS
// a TimeoutSec here, read by nobody, contradicting the paragraph above it.
//
// MaxTurns comes from the resolved [Spec] — providers.sandbox.default_max_turns
// overlaid with role.sandbox.max_turns — and is the only engine-side bound on
// a runaway coding agent, since a job is deliberately never stopped on a clock.
//
// MAXBUDGETUSD IS SET BY NOBODY, deliberately. The fleet's own token meter
// already post-charges a collected run against the seat's budget, and a second
// cap denominated in the operator's dollars would fight it: two ceilings on one
// spend, disagreeing, with the CLI's the one that silently wins.
type Limits struct {
	MaxTurns     int
	MaxBudgetUSD float64
}

// Spec is what it takes to mint and drive one box.
//
// AN EXTERNAL HOST'S REPOSITORY IS NOT HERE. Which repository on GitHub or
// GitLab to work in is task context the executor puts in the brief, and the
// coding agent clones it with the token the config injects — a spec field for
// it would make one repository per role a structural fact when it is a
// per-turn one. A repository on the engine's OWN code host is here
// ([Spec.Repos]), because naming one is a GRANT: the run row carries it and
// the box's credential is judged by it, which a brief cannot be.
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
	// role.sandbox.run_in over providers.sandbox.default_run_in, and carried
	// here rather than chosen by the caller so the launch, the pending row
	// and the reconnect all name the same cell.
	Placement Placement

	// CodingAgent names the runner: "claude-code", "opencode", …
	CodingAgent string

	// TimeoutSec is NOT a run deadline: it is the box's initial TTL, which
	// the waiter refreshes every tick, so a running job is never killed by
	// the clock. It is the ORPHAN-RECLAIM GRACE — how long a box outlives
	// an engine that has stopped heart-beating.
	TimeoutSec float64

	// PauseTTLSec bounds a PAUSED box: how long its snapshot is held before
	// the reaper reclaims it. 0 means never pause — always re-seed from the
	// pushed branch instead.
	PauseTTLSec float64

	// MaxTurns caps the agentic rounds a coding run may take. 0 is
	// uncapped, and is the default.
	//
	// Resolved here rather than passed alongside, because it is settled the
	// same way TimeoutSec and PauseTTLSec are — a provider default with a
	// per-seat override — and a second resolution path for the same shape
	// of knob is how the two come to disagree. [Launch] turns it into the
	// runner's [Limits].
	MaxTurns int

	// Env is the run environment: LLM credentials, the generic agent
	// identity (CREWLET_AGENT_*), the setup steps' env, and role.sandbox.env
	// — which is where an operator DECLARES an external token. The engine
	// never names a tool-specific variable itself. A launch hands the box a
	// COPY with what it minted for the job added ([LaunchRequest.RunEnv]):
	// what the box is made with and every setup step runs with, the run's
	// telemetry added at the job's start.
	Env map[string]string

	// CredentialFiles is a subscription-CLI login: a path relative to the
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

	// Repos are the repositories on the engine's own code host this run is
	// given, each resolved at launch ([RunRepo]), at most [MaxRunRepos]. Empty
	// for a run that clones nothing of the engine's own — a repository on
	// another host is still named in the brief and reached with the
	// credentials the seat's configuration declares. [Coordinator.Launch]
	// writes them on the run's row ([PendingRun.Repos]), which is what the
	// box's credential is judged against for as long as the job runs.
	Repos []RunRepo
}

// MaxRunRepos is the most repositories one coding run is given: each costs a
// clone before the coding agent starts, inside the setup budget
// ([DefaultSetupTimeout] a step), and a run that needs more is a task that
// should be split.
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
// rest of it (the names, the commands and the commit-trailer hook it writes)
// to a few KiB more; TestTheWorstCloneStepIsOneShellArgument builds that step
// and holds it under the limit. It bounds what a run's grants add to its row
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

// RunRepo is one repository a run is given, RESOLVED AT LAUNCH: the
// repository's id beside the name it had then, so a rename mid-run cannot move
// the grant; the commit the run starts from; the branch it works on; and the
// cone it checks out.
//
// The json tags are a WIRE FORMAT: the run's row carries these
// ([PendingRun.Repos]), and a node of another build reads them.
type RunRepo struct {
	// Repo is the repository's name at launch: the directory the box is
	// given it in, and the name its URL carries.
	Repo string `json:"repo"`
	// RepoID is the repository's id, canonical: what the grant names.
	RepoID string `json:"repo_id"`
	// Base is the whole id of the commit the run starts from, "" for an
	// empty repository.
	Base string `json:"base"`
	// Branch is the branch the run works on, its ref refs/heads/<branch>.
	Branch string `json:"branch"`
	// Access is what the run may do with the repository.
	Access RepoAccess `json:"access"`
	// Paths are the directories checked out (a sparse cone), at most
	// [MaxConePaths]; empty is the whole tree.
	Paths []string `json:"paths,omitempty"`
}

// Validate holds a RunRepo to its grammar: a repository name of either form
// (gitref.ValidRepoName), a canonical repository id, a base that is "" or a
// whole commit id, a branch name git and the code host both take
// (gitref.ValidBranch), a valid access, and at most [MaxConePaths] cone paths,
// each a clean directory inside the repository ([ConePathProblem] says what
// that holds a path to).
//
// ONE GRAMMAR for every reader: the launch that resolves a run's repositories
// refuses a request with it, the clone step is written only for a RunRepo it
// passes ([CloneRecipe]), so nothing a box is told to run carries a value no
// RunRepo can.
func (r RunRepo) Validate() error {
	switch {
	case !gitref.ValidRepoName(r.Repo):
		return fmt.Errorf("sandbox: %q is not a repository name", r.Repo)
	case !canonicalID(r.RepoID):
		return fmt.Errorf("sandbox: repository %s: its id %q is not a UUID in canonical form", r.Repo, r.RepoID)
	case r.Base != "" && (!gitref.ValidOID(r.Base) || r.Base == gitref.ZeroOID):
		return fmt.Errorf("sandbox: repository %s: its base %q is not a whole commit id", r.Repo, r.Base)
	case !gitref.ValidBranch(r.Branch):
		return fmt.Errorf("sandbox: repository %s: %q is not a branch name git accepts", r.Repo, r.Branch)
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
// (internal/code's ParseChecks), and that cone becomes a box's clone step as a
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
	SessionID string
}

// Result is one coding run's outcome.
type Result struct {
	Text    string
	Success bool

	// InputTokens is the run's WHOLE prompt count, its cached share
	// included, and CacheReadTokens / CacheWriteTokens are the share of it
	// the provider's prompt cache served and stored — the same convention
	// every engine provider reports on its completion, so a coding run's
	// tokens fold into the spend rollup beside the phases the engine ran
	// itself. A breakdown, never an addition: the total is input + output.
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          float64
	SessionID        string

	// NeedsInput means the agent asked a question and stopped. Question and
	// AskTo say what it asked and who should answer: "requester", "team",
	// "manager", or a name.
	NeedsInput bool
	Question   string
	AskTo      string

	// WIPBranch is the branch the run pushed before it asked, as the ask
	// recorded it: the name the agent gave, or the one git reported where
	// it asked. It is what a run re-seeded on a fresh machine is told to
	// check out ([PendingRun.Branch]), so it is carried as its own fact —
	// never inferred from DeliveredRefs, which name pull requests as often
	// as branches and are empty for the branch a run pushes and opens
	// nothing for. Empty when the ask named none and git could not say,
	// and on a run that did not ask.
	WIPBranch string

	// DeliveredRefs are the branches and pull or merge requests the run
	// reported delivering, in its report's order: the `Delivered:` lines it
	// wrote, or the pull-request URLs its prose holds where it named none.
	// They are DISPLAY and a reminder — the run's phase record and the
	// resumed executor's text carry them — and nothing judges a turn on
	// them: whether a turn reached anybody is the tool loop's own record of
	// what ran ([github.com/crewlet/crewlet/internal/agent/turn]).
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

	Error string

	// Transcript is the agent's activity log — what it said, each tool it
	// called and the first line of what it ran — rebuilt from its own
	// output, or its stderr where the CLI streams nothing better. It is the
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
// A RUN REPORTS ITS OWN USAGE, parsed out of the coding CLI's last line of
// output — and that line is printed inside a box the run's own agent can run
// any command in. A negative count there is a REFUND: the charge would
// subtract it from the seat's budget counter, and the phase record from every
// rollup that sums it, so a run could buy itself headroom by printing a
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

	// JobRunning reports whether the background job whose handle
	// StartBackground returned is still running.
	//
	// ASKED OF THE BOX, because only the backend knows what its handle
	// names. Inside a remote VM or a container the handle is a pid in the
	// box's own process namespace, where nothing but the box's own work
	// can hold it. On the engine host it is a host pid, which the kernel
	// hands to any process once the job is gone: a probe that took the pid
	// on trust would read a stranger as the job and hold a dead run open
	// until the stranger exits. An error means the box could not be asked,
	// which is not proof of either answer.
	JobRunning(ctx context.Context, commandID string) (bool, error)

	// WriteFile puts content at path whole. A backend that writes the box
	// from the engine host writes only a regular file where it lies, and
	// refuses anything else at the path with [ErrNotRegularFile] — a link
	// there would be followed out of the box, and a named pipe waited on
	// for good.
	WriteFile(ctx context.Context, path string, content []byte) error

	// ReadFile reads a file the engine means to read WHOLE — a report, a
	// question, a marker, a result line — and REFUSES one past
	// [MaxFileBytes] ([ErrFileTooLarge], as a [*FileTooLargeError] naming the
	// cap it refused at) rather than returning its first part, because a
	// clipped report reads as a finished one. Empty on missing: a poll for a
	// marker that is not written yet is not an error.
	//
	// NEVER FOR A MACHINE STREAM. A coding agent's event log and its stderr
	// grow with the run and have no size a whole read could honestly refuse
	// at — the engine keeps a bounded share of them in the end — so they
	// are read with [Sandbox.OpenFile] or [Sandbox.ReadTail] instead.
	//
	// A path that names something other than a regular file is refused with
	// [ErrNotRegularFile] by a backend that reads the box from the engine
	// host, for all three reads, because opening one there could block.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// OpenFile streams a file front to back, for a MACHINE STREAM decoded
	// once in bounded memory: a coding agent's event log, read at
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

	// SetTimeout resets the box's wall-clock TTL to seconds from now.
	//
	// THE KEEPALIVE. A provider reclaims a box that many seconds after the
	// TTL was last set, and the waiter calls this every poll tick — so a
	// running box is bounded only by how long the engine can go WITHOUT a
	// heartbeat, never by a fixed run deadline. A provider with no settable
	// TTL no-ops.
	SetTimeout(ctx context.Context, seconds float64) error

	// Pause snapshots the box for a later resume, holding a run blocked on
	// a clarification with exact conversational continuity. A provider
	// without snapshots no-ops, and the engine re-seeds from the pushed
	// branch instead.
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

// Runner runs one coding agent inside a box against a brief.
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

	// Poll reports whether the background job has finished.
	Poll(ctx context.Context, box Sandbox, handle RunHandle) (bool, error)

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

// RunRequest is one coding run's inputs.
//
// NEVER PERSISTED: a run is started once, by the node that launched it, and
// everything a later collection or resume needs is on the run's row and in the
// box. So a field added here changes no record a peer of another build reads.
type RunRequest struct {
	// System is the run's standing instructions, which the runner hands its
	// CLI on the channel that ADDS to the CLI's own system prompt rather than
	// replacing it: the vendor's prompt is what teaches the harness this run
	// exists to use. A runner adds its own standing instruction to it — the
	// report the run writes before it stops, at the box's path for it.
	System string

	// Brief is the task: the prompt the CLI runs, and nothing standing.
	Brief string

	Env    map[string]string
	Limits Limits

	// Model is the model the run works under, written in the grammar of the
	// CLI the runner drives: the seat's providers.llm entry's model exactly
	// as its text calls hand it to that same CLI, which is the only CLI the
	// engine lets reach this runner. Empty leaves the agent's own choice.
	//
	// NO ENDPOINT AND NO KEY: an entry talks to its CLI's own vendor, and its
	// sign-in rides the run env, so nothing secret is written into a file
	// inside the box.
	Model string

	// MCPServers is the scoped MCP surface, keyed by server name.
	// SERVER-LEVEL SCOPING ONLY — there is no per-tool allowlist, because
	// the agent inside the box negotiates its own tool list with the server
	// and an allowlist the engine could not enforce would be a claim rather
	// than a control.
	MCPServers map[string]MCPServer
}

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
		return r.Poll(ctx, box, handle)
	}); err != nil {
		return Result{}, err
	}
	return r.Collect(ctx, box, handle)
}

// WaitFunc blocks until a predicate holds or the context ends. Injected so the
// inline shape's polling cadence is the caller's choice — a test wants none.
type WaitFunc func(ctx context.Context, done func(context.Context) (bool, error)) error

// probeByKill answers [Sandbox.JobRunning] for a box whose handle is a pid in
// the box's own process namespace, by running `kill -0` inside it.
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
