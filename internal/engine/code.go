package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The code host, wired: its settings in force, the seam the API's routes and
// the host reach it through, its committer's stop, the census term of its git
// clients, the reconcile of the repositories code.repos declares, and the
// code-retention duty that sweeps the packs retired past their grace and
// purges what was deleted.
//
// # Every data node commits
//
// A company that sets `code.native` gets a committer and a lifecycle store on
// every data node ([Engine.startNative]), served to the estate router beside
// the code log's reader ([Engine.estateBackend], [code.Served]); a push is
// asked of its repository's committer — the first node rendezvous ranks for
// it — whichever node received it (internal/estate). There is no committer
// election and no per-repository lease: every node computes the same order,
// and two nodes with different views of the fleet still commit safely, since
// the repository's subject lets one record land per round.
//
// # Every data node reconciles
//
// The repositories code.repos declares are made to exist, settled and
// retired by EVERY data node at EVERY boot and EVERY activation
// ([Engine.applyCodeRepos]), each deciding the same declaration: a create
// arbitrates on the name's own subject, so two nodes creating one name land
// one repository, and the reconcile raises the code log's ACTIVATION FENCE
// first ([code.Store.Activate]) — one record per activation fleet-wide — so a
// node applying an older activation late is refused on every node rather
// than walking a newer one back, and an activation that changes nothing
// writes that one record and nothing else.

// codeInForce is the code host's settings IN FORCE at the current epoch
// ([Company.codeInForce]) — none set where no company is current yet. Read per
// call: by the committer's decide and the reader's repository detail (the
// limits), by the code-retention duty's every pass (the retention), and by the
// committer's every batch (the retention again, for the repo_deleted sentence,
// [code.RepoGone], which names when a deleted repository's purge may begin —
// the same reading the duty purges by, so under one revision the two name
// one instant). So a revision's settings hold from its activation on, with no
// restart: they are an epoch's, never the runtime's — and therefore each
// node's, while a revision is applied node by node on each node's own clock.
func (e *Engine) codeInForce() config.Code { return codeInForceAt(e.Company()) }

// codeInForceAt is the code host's settings in force at epoch c: for a
// reader that takes something else off the same epoch beside them — the
// engine's commit signer reads a seat's role and the identity domain off ONE
// epoch, so a revision that renames the seat and moves the domain together
// signs a commit by both or by neither.
func codeInForceAt(c *Company) config.Code {
	switch {
	case c == nil || c.Config == nil:
		return config.Code{}
	case c.codeInForce != nil:
		return *c.codeInForce
	}
	return c.Config.Code
}

// codeLimits is the code host's limits in force at the current epoch
// ([Engine.codeInForce]), the defaults applied.
func (e *Engine) codeLimits() code.Limits { return code.LimitsOf(e.codeInForce()) }

// codeSeatRules is the chart and the calendar the committer judges a SEAT'S
// push by ([code.CommitterOptions.SeatRules]) — the current epoch's roster,
// the one a seat's code read is judged by ([Engine.codeRoster]), and the
// company's clock its day's quota is counted on — both off ONE epoch, so a
// revision that moves a seat and the clock together is judged by both or by
// neither, and false where no epoch is installed yet: no chart, on which the
// committer hands a seat's push to the repository's next committer rather
// than judge it. Read once a batch that carries a seat's push.
func (e *Engine) codeSeatRules() (code.SeatRules, bool) {
	c := e.Company()
	if c == nil || c.roster == nil {
		return code.SeatRules{}, false
	}
	return code.SeatRules{Roster: c.roster(), Zone: c.Config.Location()}, true
}

// codeDeletedRetention is how long a deleted repository waits for its purge at
// the current epoch ([Engine.codeInForce]), the default applied. Two readers,
// one reading: the code-retention pass's [code.Retention.Retention], read at
// every pass, and the committer's [code.CommitterOptions.Retention], read at
// every batch for the repo_deleted sentence ([code.RepoGone]) — so under one
// revision a push to a deleted repository is told the instant the duty purges
// from, and a revision that moves code.deleted_retention_days moves what a
// push is told with it. Not the same instant at every moment: the duty runs on
// its lease holder and the committer on the repository's rendezvous-first
// data node, each reading its own node's epoch at its own moment (a pass, a
// batch), so while such a revision is applied node by node the two may each
// answer from a different one — which is why the sentence names the instant
// as the earliest the purge may begin on the deciding node's epoch, never as
// a promise.
func (e *Engine) codeDeletedRetention() time.Duration {
	return e.codeInForce().DeletedRetention()
}

// codeLifecyclePolicy is what a tool's create is held to, and how long a
// seat's scratch repository may sit idle, at the current epoch
// ([Engine.codeInForce]), the defaults applied through the code domain's one
// conversion ([code.ScratchOf]). Two readers, one reading: the lifecycle
// store's decide of a create ([code.StoreOptions.Policy]) and the
// code-retention pass's scratch expiry ([code.Retention.Policy]) — so a
// revision that moves code.scratch holds from its activation on, with no
// restart, on each node's own epoch.
func (e *Engine) codeLifecyclePolicy() code.LifecyclePolicy {
	return code.ScratchOf(e.codeInForce())
}

// carriedCode is the code host's settings in force at next, installed over
// prev ([Engine.installEpoch]): next's own `code` block where it sets
// `code.native`, and otherwise prev's in force.
//
// CARRIED, because the revision that turns the host off cannot hold its
// settings — `code.limits` and `code.deleted_retention_days` without
// `code.native` are refused, since after a restart they would read as live
// while governing nothing — and a node whose runtime was built with the host
// keeps serving it until that restart ([backendSwitches]). Read off that
// revision, every push the still-running committer decided was held to the
// DEFAULTS: a company that had capped its repositories at 2 GiB and its pushes
// at 64 MiB got 20 GiB and 1 GiB on a host it had just asked to wind down. And
// the still-running retention duty would purge by the default seven days a
// repository the company had chosen to keep restorable for a year — a purge
// has no undo. A revision that hosts code again brings its own.
func carriedCode(next, prev *Company) *config.Code {
	if next.Config != nil && next.Config.Code.Native {
		in := next.Config.Code
		return &in
	}
	if prev == nil {
		return nil
	}
	return prev.codeInForce
}

// OperatorCode is the code host's estate seam as the API's routes and the host
// reach it — the router's, on every node, for [OperatorWork]'s reason — or
// false where this node's RUNNING runtime was not built with `code.native`: a
// company that does not host code, one that turned it on and has not
// restarted, and a node that runs no native runtime yet. Which halves a node
// runs is [Engine.nativeHalves]' one answer, so the code host is handed out
// exactly where a data node serves it.
func OperatorCode(e *Engine) (estate.CodeSeam, bool) {
	if e.router == nil || !e.nativeHalves().code {
		return estate.CodeSeam{}, false
	}
	return e.router.Code(), true
}

// codeEvents is the event stream this node's code host announces what its
// records applied on — the checks writer an applied ending, the committer an
// applied merge and an opened change, the change writer a review — its
// queue's, the one every event of this node is published to; and nil, never a
// typed nil, where it holds none: a node with no queue records no event of any
// kind.
func (e *Engine) codeEvents() code.EventPublisher {
	if e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	return e.backends.Queue
}

// codeAgentID is the agent id a seat's day is counted under, off the LIVE
// epoch at the moment an opened change or a review is announced
// ([code.CommitterOptions.AgentIDs]) — the same resolution the event log files
// a participant's handle by ([store.DB.SetEventSeats]), so the event's
// `agent_id` and its party rows name one seat. False for a human seat, a
// handle the chart does not hold, and a node with no company active.
func (e *Engine) codeAgentID(handle string) (string, bool) { return seatAgentID(e.Company(), handle) }

// stopCommitter ends this node's committer: a proposal queued on it is
// answered "not serving", which every asker reads as a node that ran nothing
// and takes to the repository's runner-up with the same push id, and a batch
// already publishing finishes — or, past the bound, is cut and answers its own
// outcome (unknown, naming its operation).
//
// IN THE TEARDOWN BEFORE THE ESTATE SUBJECT STOPS ANSWERING
// ([Engine.stopServingEstate]), because that withdrawal CANCELS the answers in
// flight before it waits for them (queue.Queue.Serve's contract), and a
// proposal still queued here is one of them: its context ended, the committer
// answers it with that cancellation, and the estate carries the failure back as
// the push's answer — final, so its asker reports `context canceled` to the git
// client, for a push its batch may yet have landed, rather than moving on.
// Stopped first, the committer answers every queued proposal "not serving" and
// finishes the batch in flight before anything is cancelled, so every asker
// either has its outcome or takes the push to the runner-up under the same id.
// The order saves no wait: once the subject is withdrawn, an ask finds no
// responder at once and moves on as fast as a refusal. Nil-safe, which is a
// node with no runtime or a company that hosts no code.
//
// AND BEFORE THE ADMISSION IS WITHDRAWN ([Engine.withdraw]), with every other
// state-log publisher: every batch it commits is a record on the code log,
// and one appended once the admission is gone is a write a capacity
// coordinator was told nobody was making.
func (e *Engine) stopCommitter(ctx context.Context) { e.native.Load().stopCommitter(ctx) }

// stopCommitter is [Engine.stopCommitter] on this runtime, and the stop its
// own failure path takes ([native.shutdown]). [code.Committer.Stop] is
// idempotent, so the teardown's stop and the shutdown's after it are one.
//
// BOUNDED by [statelog.DefaultResolveBudget]: a batch in flight resolves
// within it when the broker answers — it is the bound every write's own wait
// for its applier takes — and when the broker does not, the push ids the batch
// carried collapse on whatever landed when their clients push again.
//
// THE CHANGE WRITER AND THE CHECKS WRITER STOP WITH IT, for the same two
// reasons: a write of a change's conversation or of a repository's checks
// waiting its turn is answered "not serving" and taken to the runner-up, and a
// publish in flight finishes before the estate subject cancels its answer and
// before the admission is withdrawn. One bound for all three, the writers'
// stops after the committer's.
func (n *native) stopCommitter(ctx context.Context) {
	if n == nil || n.committer == nil {
		return
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), statelog.DefaultResolveBudget)
	defer cancel()
	n.committer.Stop(bounded)
	if n.changeWriter != nil {
		n.changeWriter.Stop(bounded)
	}
	if n.checksWriter != nil {
		n.checksWriter.Stop(bounded)
	}
}

// clientTerm is one log's census term of readers that are neither seats nor
// the engine's own passes ([statelog.Census.Clients]), as a function of the
// byte ceiling the log's stream HOLDS and the trim's min_age.
type clientTerm func(holds int64, minAge time.Duration) int

// clientBarriers is the census term of a log's readers that are neither seats
// nor the engine's own passes: the code log's git clients —
// [code.GitReadsPerDay] over the ceiling the log's stream holds, so a resize
// of the log (`crewlet retention set-capacity`) moves the census with it. Nil
// for every other log.
//
// A TERM OF THE LOG, WHATEVER THIS NODE HOSTS. The code log runs, and keeps
// the ceiling the term is a share of, in every register, whether or not the
// company hosts code; and the rate the census is held against is the barriers
// EVERY node applied over the alarm's day ([retention.census]) — the fleet's
// reads, never this node's. Keyed on this node's runtime instead, the term
// judged a fleet's git reads against the seats' term alone in two places: on
// every node restarted after a revision turned `code.native` off, while up to
// a day of git barriers from before the switch were still in the window; and,
// during the rolling restart after a revision turned it on, on every node not
// yet restarted, while its peers served git. A healthy host fired
// census_drift for up to a day either way. On a company that has never hosted
// code nothing appends a git barrier, so the term allows for reads that never
// come.
func clientBarriers(domain string) clientTerm {
	if domain != (code.Domain{}).Name() {
		return nil
	}
	return code.GitReadsPerDay
}

// applyCodeRepos asks for the reconcile of code.repos at this activation
// ([reconcileCodeRepos]): the repositories this company declares are made to
// exist on the code log with the settings it declares, and every
// configuration-managed repository it no longer declares is deleted — softly,
// restorably for `code.deleted_retention_days`.
//
// # Asked for, never waited on
//
// The apply returns once it has asked, and [codeReconciler] decides beside it:
// a reconcile costs a write for every repository that changed — 16–19 s on
// one node at the ten thousand a document may declare, and more in a fleet
// whose nodes race on each — while a node behind on an epoch for 45 s that a
// peer has applied is shed, and how many repositories a company declares must
// never decide that (the reconciler says more). A newer activation's request
// ends an older one's reconcile still running, since the newer decides every
// repository again. And because the apply has reported the revision applied by
// then, a reconcile that leaves something undone is RUN AGAIN by the
// reconciler, on a growing backoff, until it applies: nothing else would.
//
// # On every data node at every boot and every activation
//
// Idempotent, and never the apply's failure — [Engine.applyContainers]'
// posture, for its reason: this is one clause of an apply and the rest stands
// without it, and it is cheap where nothing changed. Every node deciding the same declaration
// is SAFE rather than merely harmless: a create arbitrates on the name's own
// subject, so two creates of one name land one repository, and the fence
// refuses an older activation's records wherever they land. Nodes applying
// one activation at the same moment race on every subject it changes, and
// the broker lets one record land on each: the others re-decide from it and
// write nothing.
//
// NOT ON A REVISION THAT TURNED THE HOST OFF: on a runtime built with the host
// that revision changes nothing until a restart (the restart line says so),
// and nothing deletes a repository because the HOST was switched off. Nor on a
// node in a mode that does not publish, whose lifecycle store is withheld
// ([Engine.writeSide]), nor for a company no activation has named — its first
// activation does it, as [Engine.applyChart] explains.
func (e *Engine) applyCodeRepos(ctx context.Context, c *Company, activatedAt time.Time) {
	if _, _, runs := e.codeReposSides(c, activatedAt); !runs {
		return
	}
	e.codeRepos.ask(ctx, c, activatedAt)
}

// decideCodeRepos is the reconciler's run: one reconcile of the epoch w
// asked for, through this node's lifecycle store and over its rows as they are
// when it BEGINS, answering what it left undone that running it again can do
// ([reconcileCodeRepos]) — a node whose lifecycle store was withheld since the
// request decides nothing, and leaves nothing it could do.
func (e *Engine) decideCodeRepos(ctx context.Context, w codeReposWanted) error {
	store, lives, runs := e.codeReposSides(w.company, w.at)
	if !runs {
		return nil
	}
	return reconcileCodeRepos(ctx, log, store, lives, codeOwners(w.company), w.company.Config.Code.Repos, w.at)
}

// codeReposSides is the lifecycle store a reconcile of the epoch c at the
// activation activatedAt writes through and the rows it walks, and false where
// none runs — the cases [Engine.applyCodeRepos] names.
func (e *Engine) codeReposSides(c *Company, activatedAt time.Time) (*code.Store, *code.Reader, bool) {
	n := e.native.Load()
	store := e.writeSide().code
	if n == nil || n.codeReader == nil || store == nil || c == nil || c.Config == nil ||
		activatedAt.IsZero() || !c.Config.Code.Native {
		return nil, nil, false
	}
	return store, n.codeReader, true
}

// stopCodeRepos ends the reconcile of code.repos in flight and refuses every
// request after ([codeReconciler.stop]). Nil-safe.
//
// BEFORE THE ADMISSION IS WITHDRAWN ([Engine.withdraw]), with every other
// loop that appends to a log: a reconcile publishes a record for every
// repository that changed, and one still running once the admission is gone
// is a publisher a capacity coordinator cannot see.
func (e *Engine) stopCodeRepos() {
	if e.codeRepos != nil {
		e.codeRepos.stop()
	}
}

// codeLifecycle is the lifecycle store as the reconcile of code.repos calls
// it; *code.Store satisfies it.
type codeLifecycle interface {
	Activate(ctx context.Context, activatedAt time.Time) (code.Activation, error)
	EnsureRepos(ctx context.Context, a code.Activation, declared []code.Declared) ([]code.EnsureResult, error)
	DeleteRepos(ctx context.Context, a code.Activation, ids []string) ([]code.DeleteResult, error)
}

// codeLives is this node's code rows as the reconcile walks them;
// *code.Reader satisfies it.
type codeLives interface {
	WalkLives(ctx context.Context, visit func(code.RepoLife) error) error
}

// reconcileCodeRepos is one reconcile of code.repos — the declaration repos,
// its owners resolved on the activation's chart — at the activation
// activatedAt, logged to logger.
//
// # The fence first
//
// It raises the code log's ACTIVATION FENCE to this activation before it
// decides any repository ([code.Store.Activate]), and every decision after it
// waits for this node to have applied that fence (the session floor its
// [code.Activation] carries). So every decision reads a snapshot in which each
// older activation's EARLIER records have applied — a difference one of them
// made is settled back — while each older activation's LATER record is refused
// by the fence, identically on every node, because the fence is derived from
// the log (internal/code's package doc). That is what lets an unchanged
// repository cost nothing: an activation that changes nothing writes its one
// fence record fleet-wide, never a settings record a repository a node, and
// the unchanged declarations are read in one batch ([code.Store.EnsureRepos]).
//
// # What changed, side by side
//
// The repositories that changed — created, settled, restored, and the
// configuration-managed ones the document no longer declares, deleted — are
// decided several at a time ([code.Store.EnsureRepos],
// [code.Store.DeleteRepos]), each its own subject, so their records reach the
// applier together and are committed as one batch. One at a time, a create
// was a publish, an append and a wait for this node to apply it — 6.7 ms, and
// a deletion 7.0 ms — so the last of the ten thousand repositories a document
// may declare existed over a minute after its first activation; side by side,
// 16–19 s (internal/code's doc.go, measured section).
//
// # A failure every repository shares is said once
//
// A failure the whole reconcile meets — a later activation's fence, this
// node's own refusal by the code log, an estate it cannot read, its context
// ending — is logged ONCE, and no deletion follows it: ten thousand
// lines on every data node at every activation say nothing the first did not,
// and a deletion met the same failure after a wait of its own. A refusal for
// one repository is logged and the rest are still decided. It logs only what
// it wrote.
//
// # What it answers: what running it again can do
//
// It answers the first failure it left undone that a second run can apply —
// a fence the log did not raise, a failure every repository shared, a
// repository whose outcome the log did not answer or whose decision this node
// could not make yet, a deletion that failed, the deletions a failed walk of
// the rows skipped — and nil where nothing of that kind is left, which is what
// [codeReconciler] runs it again for. The design working is no such failure: a
// later activation's fence or row (that activation's reconcile owns them), a
// context ended by a newer request or the engine stopping (the newer
// reconcile, or the next boot, decides everything again), a purge to wait out
// (the code-retention duty declares the name when it completes). Nor is what
// no second run can change: a name another manager holds, and a declaration
// the code log refuses as written — each waits for a revision that changes it,
// and is said at every run until then.
func reconcileCodeRepos(ctx context.Context, logger *slog.Logger, store codeLifecycle,
	lives codeLives, owners *org.Index, repos []config.CodeRepo, activatedAt time.Time) error {

	var undone error
	leave := func(err error) {
		if undone == nil {
			undone = err
		}
	}
	a, err := store.Activate(ctx, activatedAt)
	if err != nil {
		return codeReposNotApplied(ctx, logger, failureOf(ctx, err), "no declared repository was "+
			"created, settled or deleted at this activation")
	}
	declared := make(map[string]bool, len(repos))
	decided := make([]code.Declared, 0, len(repos))
	for _, r := range repos {
		// DECLARED WHETHER OR NOT IT CAN BE RECORDED: a declaration this
		// node cannot build must never read as a removal.
		declared[r.Name] = true
		d, refused := declaredRepo(owners, r)
		if refused != nil {
			leave(codeRepoNotApplied(ctx, logger, r.Name, refused))
			continue
		}
		decided = append(decided, d)
	}
	// THE ROWS BEFORE THE DECISIONS, at the stale level and in the
	// reconcile's own narrow walk: what each declared name was — none, live
	// or deleted — which names what a write did, and which
	// configuration-managed repositories the document no longer declares.
	// Stale is enough, because every deletion decides inside its own
	// snapshot after the fence: a row this walk missed is caught at the next
	// apply, and one it showed wrongly is decided away. Held as it is read,
	// a map of the declared names and a list of what goes — never the rows.
	was := make(map[string]code.RepoStatus, len(declared))
	var gone []code.RepoLife
	listErr := lives.WalkLives(ctx, func(l code.RepoLife) error {
		switch {
		case declared[l.Name]:
			was[l.Name] = l.Status
		case retiredByOmission(l):
			gone = append(gone, l)
		}
		return nil
	})
	if listErr != nil {
		logger.WarnContext(ctx, "code_repos_not_listed", "error", listErr.Error(),
			"detail", "a repository code.repos no longer declares is not deleted at this "+
				"activation; "+codeReposRunAgain)
		leave(fmt.Errorf("engine: walk the code log's repositories, without which no "+
			"removal from code.repos is deleted: %w", listErr))
	}
	if listErr == nil {
		importsIgnored(ctx, logger, decided, was)
	}
	var wrote codeReposWritten
	defer wrote.log(ctx, logger)
	results, err := store.EnsureRepos(ctx, a, decided)
	for i, res := range results {
		switch {
		//nolint:errorlint // the IDENTICAL value is EnsureRepos' contract for a declaration its failure left undecided
		case err != nil && res.Err == err:
			// THE CALL'S OWN FAILURE, which left this one undecided:
			// said once, below.
		case res.Err != nil:
			leave(codeRepoNotApplied(ctx, logger, decided[i].Name, res.Err))
		case res.Changed:
			wrote.ensured(decided[i].Name, was, listErr == nil)
		}
	}
	if err != nil {
		// THE SHARED FAILURE IS THE ANSWER, whatever a repository's own was
		// before it: a run again decides that repository with the rest once
		// the shared one clears, and where it is the design working — a
		// later activation owns the repositories, or the engine stopped —
		// no run of THIS activation has anything left to do.
		return codeReposNotApplied(ctx, logger, failureOf(ctx, err), "no further "+
			"repository was created, settled or deleted at this activation")
	}
	if listErr != nil || len(gone) == 0 {
		return undone
	}
	ids := make([]string, len(gone))
	for i, l := range gone {
		ids[i] = l.ID
	}
	deletions, err := store.DeleteRepos(ctx, a, ids)
	for i, res := range deletions {
		switch {
		//nolint:errorlint // the IDENTICAL value is DeleteRepos' contract for a repository its failure left undecided
		case err != nil && res.Err == err:
		case res.Err != nil:
			leave(codeRepoNotApplied(ctx, logger, gone[i].Name, res.Err))
		case res.Deleted:
			wrote.deleted = append(wrote.deleted, gone[i].Name)
		}
	}
	if err != nil {
		return codeReposNotApplied(ctx, logger, failureOf(ctx, err), "no further "+
			"repository code.repos no longer declares was deleted at this activation")
	}
	return undone
}

// codeReposWritten is what one reconcile of code.repos wrote, by what it did.
type codeReposWritten struct {
	created, changed, restored, deleted []string
	// written is a declaration written while what it was is unknown: the
	// walk of the rows before the decisions failed.
	written []string
}

// ensured files a name whose declaration was written, by what it was before:
// none is a create, deleted a restore, live a settings change.
func (w *codeReposWritten) ensured(name string, was map[string]code.RepoStatus, listed bool) {
	switch status, found := was[name]; {
	case !listed:
		w.written = append(w.written, name)
	case !found:
		w.created = append(w.created, name)
	case status == code.RepoStatusDeleted:
		w.restored = append(w.restored, name)
	default:
		w.changed = append(w.changed, name)
	}
}

// importsIgnored says, in ONE line a run, which declarations name an import
// for a repository that existed before the run — live or deleted, as the walk
// of the rows found it (was) — and is not being imported: an import is read
// by its repository's create alone ([code.Declared.Import]), so each of these
// is passed by, and the line is what tells the person who added one to a
// repository that already holds history that nothing will be fetched — and
// the one who left one in place after its import that it can go. One still
// being imported is its create's import running, and is not named. Its count,
// and its first [codeReposLoggedNames] names; nothing where there are none.
func importsIgnored(ctx context.Context, logger *slog.Logger, decided []code.Declared,
	was map[string]code.RepoStatus) {

	var names []string
	for _, d := range decided {
		if status, existed := was[d.Name]; existed && status != code.RepoStatusImporting && d.Import != nil {
			names = append(names, d.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	logger.InfoContext(ctx, "code_import_ignored", "count", len(names),
		"names", names[:min(len(names), codeReposLoggedNames)],
		"detail", "import is read only when its repository is created, and these existed already, so "+
			"nothing is fetched for them: remove import from each declaration — a pull mirror's "+
			"mirror_from is what follows another host")
}

// codeReposLoggedNames is the most names the reconcile's line lists of each
// kind of write, beside the count of them all: thirty-two names of at most 63
// bytes are 2 KB, where the ten thousand one first activation can create are
// over 300 KB in one line — past the line a log pipeline carries whole (a
// container runtime's json log splits a line at 16 KB) — and name nothing a
// person reads that `crewlet code status` does not list.
const codeReposLoggedNames = 32

// log says what was written in one line, and nothing where nothing was: this
// runs at every boot and every activation on every data node, and a line
// naming every repository each time would report a company nobody edited as
// one that changed. Each kind is its COUNT, and its first
// [codeReposLoggedNames] names.
func (w *codeReposWritten) log(ctx context.Context, logger *slog.Logger) {
	var attrs []any
	for _, f := range []struct {
		key   string
		names []string
	}{
		{"created", w.created}, {"changed", w.changed}, {"restored", w.restored},
		{"deleted", w.deleted}, {"written", w.written},
	} {
		if len(f.names) > 0 {
			attrs = append(attrs, f.key, len(f.names),
				f.key+"_names", f.names[:min(len(f.names), codeReposLoggedNames)])
		}
	}
	if len(attrs) > 0 {
		logger.InfoContext(ctx, "code_repos_applied", attrs...)
	}
}

// failureOf is err as a reconcile reports it: where the reconcile's own
// context ended it, the reason that context was ended ([codeReconciler]) — a
// newer activation's request, [code.ErrSuperseded], or the engine stopping —
// rather than the bare cancellation every decision in flight answers.
func failureOf(ctx context.Context, err error) error {
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
	}
	return err
}

// codeReposRunAgain is what a line about something a reconcile left undone
// says comes next: the apply that asked has reported its revision applied, so
// this node's reconciler is what runs it again ([codeReconciler]).
const codeReposRunAgain = "this node runs the reconcile of code.repos again until it " +
	"applies (code_repos_retrying says when)"

// codeReposNotApplied logs, ONCE, a failure the whole reconcile met, which
// left what detail says undone, and answers it where running the reconcile
// again can apply what it left: a later activation that owns the repositories
// and an engine stopping are the design working, and answer nil.
func codeReposNotApplied(ctx context.Context, logger *slog.Logger, err error, detail string) error {
	switch {
	case errors.Is(err, code.ErrSuperseded):
		// A LATER ACTIVATION OWNS THE REPOSITORIES: this node applies an
		// older configuration than one the fleet has already fenced, or
		// has applied a later one itself, so every decision this one made
		// would be refused on every node.
		logger.InfoContext(ctx, "code_repos_superseded", "error", err.Error(),
			"detail", "a later configuration's reconcile owns the repositories, so this "+
				"activation decides no further one; this node reconciles the later one")
		return nil
	case errors.Is(err, errCodeReconcileStopped):
		logger.InfoContext(ctx, "code_repos_interrupted", "error", err.Error(),
			"detail", detail+"; the next boot reconciles code.repos again")
		return nil
	}
	logger.ErrorContext(ctx, "code_repos_not_applied", "error", err.Error(),
		"detail", detail+"; "+codeReposRunAgain)
	return err
}

// codeRepoNotApplied logs one repository the reconcile did not apply, at the
// level what stopped it deserves, and answers its failure where running the
// reconcile again can apply it: a purge that has to finish first and a later
// activation that owns the repository are the design working, a name another
// manager holds and a declaration the code log refuses as written are no
// reconcile's to change, and each answers nil.
func codeRepoNotApplied(ctx context.Context, logger *slog.Logger, name string, err error) error {
	switch {
	case errors.Is(err, code.ErrPurging):
		logger.InfoContext(ctx, "code_repo_awaits_purge", "name", name,
			"detail", "the purge of the repository that last held this name completes "+
				"first; the code-retention duty declares it again when it does")
		return nil
	case errors.Is(err, code.ErrSuperseded):
		logger.InfoContext(ctx, "code_repo_superseded", "name", name,
			"detail", "a later configuration's reconcile owns this repository now")
		return nil
	case errors.Is(err, code.ErrNotConfigManaged):
		// SOMEBODY ELSE'S REPOSITORY HOLDS THE NAME, and a reconcile never
		// takes over a repository the configuration does not manage — so
		// no run of it changes this, and it is said at each one until a
		// revision renames the declaration.
		logger.ErrorContext(ctx, "code_repo_not_applied", "name", name, "error", err.Error(),
			"detail", "a repository code.repos does not manage holds this name, and the "+
				"configuration never takes one over: rename the declaration; the rest of "+
				"code.repos is still applied")
		return nil
	case errors.Is(err, code.ErrInvalid):
		logger.ErrorContext(ctx, "code_repo_not_applied", "name", name, "error", err.Error(),
			"detail", "the code log refuses this declaration as written, which the "+
				"configuration's validation should have refused: the revision that corrects "+
				"it applies it; the rest of code.repos is still applied")
		return nil
	}
	logger.ErrorContext(ctx, "code_repo_not_applied", "name", name, "error", err.Error(),
		"detail", "the rest of code.repos is still applied; "+codeReposRunAgain)
	return fmt.Errorf("engine: repository %q of code.repos: %w", name, err)
}

// retiredByOmission reports a repository the reconcile deletes for being
// absent from code.repos — called for one the document does not declare: a
// LIVE repository, active or archived, that the CONFIGURATION manages.
//
// ONLY THE CONFIGURATION'S. A repository some other manager made — a later
// build's own value for one a person or a lead created — is no declaration's
// to retire, and this build is a newer build's OLDER PEER in a rolling
// upgrade: deleting such a repository for being absent from code.repos would
// throw away somebody's repository on every node this build runs on.
// [code.Store.DeleteRepo] checks the manager again inside its own snapshot,
// so this is the reconcile not asking rather than the only guard.
func retiredByOmission(l code.RepoLife) bool {
	live := l.Status == code.RepoStatusImporting || l.Status == code.RepoStatusActive ||
		l.Status == code.RepoStatusArchived
	return live && l.ManagedBy == code.ManagedConfig
}

// declaredRepo is one code.repos entry as the domain records it, against the
// chart of the activation being applied, indexed once for the whole list
// ([codeOwners]): the owner and every path owner resolved to a unit's key (or
// a seat's handle), every protection rule spelled as its namespaced pattern
// ([config.CodeProtection.Pattern]) and SORTED, each one's merge gate resolved
// ([config.CodeProtection.Gate]) and in its one form ([code.CanonicalGate]),
// and the path owners in theirs ([code.CanonicalPathOwners]) — the record
// carries one canonical order, so every node deciding one declaration forms
// byte-identical records, and [code.RepoSettings.Validate] refuses any other —
// and the default branch, the visibility and the search mode defaulted, the
// last to indexed: a repository the company declares is searched across. Its
// mirrors and its import are recorded as written — each credential the ${VAR}
// reference itself, never what it resolves to, which is read only when the
// remote is dialed — a push mirror's seat by its handle, trimmed as an owner's
// is, and its refs in the document's order, unset for the default.
//
// AN ERROR only for a revision validation refuses — an owner the chart does
// not resolve: a runnable revision never reaches it, and one that does is
// named rather than recorded with an empty owner. It is [code.ErrInvalid],
// since the declaration as written is what is wrong: no run of the reconcile
// changes it, and only a revision that corrects it does.
func declaredRepo(o *org.Index, r config.CodeRepo) (code.Declared, error) {
	if o == nil {
		return code.Declared{}, fmt.Errorf("%w: the epoch has no chart to resolve the "+
			"owner on", code.ErrInvalid)
	}
	owner, err := declaredOwner(o, "owner", r.Owner.Unit, r.Owner.Seat)
	if err != nil {
		return code.Declared{}, err
	}
	protections := make([]code.Protection, 0, len(r.Protect))
	for _, rule := range r.Protect {
		p := code.Protection{PushRule: code.PushRule{
			Pattern: rule.Pattern(), AllowForce: rule.AllowForce, AllowDelete: rule.AllowDelete,
			DirectPush: code.DirectPush(rule.DirectPush.Resolved()),
		}}
		if g := rule.Gate(); g != nil {
			gate := code.CanonicalGate(declaredGate(*g))
			p.Gate = &gate
		}
		protections = append(protections, p)
	}
	slices.SortFunc(protections, func(a, b code.Protection) int { return cmp.Compare(a.Pattern, b.Pattern) })
	owners := make([]code.PathOwner, 0, len(r.PathOwners))
	for i, po := range r.PathOwners {
		who, err := declaredOwner(o, fmt.Sprintf("path_owners[%d]", i), po.Unit, po.Seat)
		if err != nil {
			return code.Declared{}, err
		}
		owners = append(owners, code.PathOwner{Owner: who, Paths: po.Paths})
	}
	d := code.Declared{Name: r.Name, Settings: code.RepoSettings{
		ManagedBy: code.ManagedConfig, Owner: owner, Visibility: code.Visibility(r.Visibility.Resolved()),
		Search: code.SearchMode(r.Search.Resolved()), DefaultBranch: r.ResolvedDefaultBranch(),
		Description: r.Description, Archived: r.Archived, Protections: protections,
		PathOwners: code.CanonicalPathOwners(owners),
	}}
	if m := r.MirrorFrom; m != nil {
		d.Settings.MirrorFrom = &code.MirrorSource{URL: m.URL, Credential: m.Credential}
	}
	if m := r.MirrorTo; m != nil {
		d.Settings.MirrorTo = &code.MirrorTarget{URL: m.URL, Credential: m.Credential,
			As: strings.TrimSpace(m.As), Refs: slices.Clone(m.Refs)}
	}
	if i := r.Import; i != nil {
		d.Import = &code.ImportSource{URL: i.URL, Credential: i.Credential}
	}
	return d, nil
}

// declaredGate is a rule's resolved merge gate ([config.CodeProtection.Gate])
// as the code domain records it: each value of config's closed sets the
// domain's of the same spelling (TestEveryDeclaredMergeByIsOneTheDomainRecords
// holds the sets to each other), its lists in the document's order —
// [code.CanonicalGate] forms the record's — and nil where the document names
// none, so a gate that names no human path records none.
func declaredGate(g config.CodeGate) code.MergeGate {
	out := code.MergeGate{
		MergeBy: code.MergeBy(g.MergeBy), Approvals: code.Approvals(g.Approvals),
		ApprovalsUnless: declaredClasses(g.ApprovalsUnless), Checks: g.Checks,
		Landing:       code.Landing{Depth: g.Landing.Depth, Batch: g.Landing.Batch},
		OwnersApprove: g.OwnersApprove, DeleteSource: code.DeleteSource(g.DeleteSource),
		AgentsEditChecks: g.AgentsEditChecks,
	}
	if g.HumanPaths != nil {
		out.HumanPaths = make([]code.HumanPath, len(g.HumanPaths))
		for i, h := range g.HumanPaths {
			out.HumanPaths[i] = code.HumanPath{Glob: h.Glob, Unless: declaredClasses(h.Unless)}
		}
	}
	return out
}

// declaredClasses is a list of config's change classes as the domain's: nil
// for none.
func declaredClasses(classes []config.CodeChangeClass) []code.ChangeClass {
	if classes == nil {
		return nil
	}
	out := make([]code.ChangeClass, len(classes))
	for i, c := range classes {
		out[i] = code.ChangeClass(c)
	}
	return out
}

// declaredOwner is the owner a declaration names at field — exactly one of a
// unit, resolved to its KEY whichever spelling the declaration used, or a seat,
// by its handle — or [code.ErrInvalid] for one the configuration's validation
// refuses.
func declaredOwner(o *org.Index, field, unit, seat string) (code.Owner, error) {
	switch unit, seat = strings.TrimSpace(unit), strings.TrimSpace(seat); {
	case unit != "" && seat == "":
		u := o.UnitByRef(unit)
		if u == nil {
			return code.Owner{}, fmt.Errorf("%w: %s.unit: %q is no unit of the chart, which the "+
				"configuration's validation refuses", code.ErrInvalid, field, unit)
		}
		return code.Owner{Kind: code.OwnerUnit, Ref: u.Key()}, nil
	case seat != "" && unit == "":
		if o.SeatByHandle(seat) == nil {
			return code.Owner{}, fmt.Errorf("%w: %s.seat: %q is no seat's handle, which the "+
				"configuration's validation refuses", code.ErrInvalid, field, seat)
		}
		return code.Owner{Kind: code.OwnerSeat, Ref: seat}, nil
	}
	return code.Owner{}, fmt.Errorf("%w: %s: a declaration names exactly one of a unit and a seat, "+
		"which the configuration's validation requires", code.ErrInvalid, field)
}

// codeOwners is the epoch c's chart indexed for declaredRepo — built ONCE a
// reconcile, because code.repos lists up to ten thousand owners and the
// chart's one-off lookups walk every unit or every seat for each (internal/org's
// [org.Index]) — or nil where the epoch has no chart.
func codeOwners(c *Company) *org.Index {
	if c == nil || c.Org == nil {
		return nil
	}
	return c.Org.Index()
}

const (
	// codeRetentionDutyName is the fleet singleton that sweeps the packs
	// retired past code.RetireGrace and purges the repositories deleted past
	// their retention.
	codeRetentionDutyName = "code-retention"

	// codeRetentionDutyTTL is how long the duty outlives a holder that
	// stopped renewing it: three ticks ([code.RetentionInterval]), every
	// other singleton's rule — a holder that died is replaced within three
	// quarters of an hour, against a purge that is days late by design. A
	// pass in flight renews it as it runs ([dutyRenewal.keep]), so no pass
	// outlives its holder's claim.
	codeRetentionDutyTTL = 3 * code.RetentionInterval
)

// codeRetentionDuty is the code-retention duty as one node runs it: the pass
// ([code.Retention]), and the claim, the renewal, the decline and the give-back
// every fleet duty that decides from a domain's rows goes through.
type codeRetentionDuty struct {
	pass *code.Retention

	// claim answers whether this node holds the duty this turn
	// ([Engine.workerDuty]); nil is a node with nobody to claim from, which
	// runs it alone.
	claim schedule.DutyFunc
	// giveBack gives the duty back where this node holds it, on a turn that
	// declines it ([Engine.workerGiveBack]); nil holds no lease to give.
	giveBack func(context.Context) (bool, error)
	// copies names the first of the domains given whose copy on this node is
	// out of service ([Engine.copyOutOfService]). Nil judges no copy.
	copies func(ctx context.Context, domains ...string) (string, bool)
	// served answers whether this node serves the revision the fleet's
	// activation pointer names ([Engine.servedFleetRevision]): a purge is
	// judged by that revision's code.deleted_retention_days and has no undo.
	// Nil judges no revision.
	served func(ctx context.Context) (*Company, error)
	// declines remembers why the duty is declining, if it is.
	declines dutyDecline
	// renewal keeps the duty while a pass runs, and ends the pass when it
	// cannot.
	renewal dutyRenewal
	// log is where each pass's line goes ([logCodeRetention]); nil is the
	// engine's.
	log *slog.Logger

	loop *loop
}

// newCodeRetentionDuty is the duty over this node's lifecycle store and code
// reader, its ref expiry's pushes naming nodeID (the core's resolved id).
func (e *Engine) newCodeRetentionDuty(store *code.Store, reader *code.Reader, nodeID string) *codeRetentionDuty {
	d := &codeRetentionDuty{
		pass: &code.Retention{
			Store: store, Reader: reader,
			// THE CURRENT EPOCH'S, read at every pass: a revision that
			// lengthens the retention saves a repository the next pass
			// would have purged.
			Retention: e.codeDeletedRetention,
			OnPurged:  e.redeclareCode,
			// AND ITS code.scratch, read at every pass's scratch expiry —
			// which deletes nothing a live turn holds: every run row's holds
			// and every seat holder's running turns', asked once a page of
			// due repositories ([Engine.codeHolds]).
			Policy: e.codeLifecyclePolicy,
			Holds:  e.codeHolds,
			// AND ITS REF EXPIRY, last, which deletes seats' idle branches
			// through each repository's committer — the estate router's,
			// which asks the one rendezvous ranks first and fails over —
			// keeping every branch the same holds name.
			Propose: e.router.Code().Propose,
			Node:    nodeID,
			Logger:  logging.Get("code"),
		},
		claim:    e.workerDuty(codeRetentionDutyName, codeRetentionDutyTTL),
		giveBack: e.workerGiveBack(codeRetentionDutyName),
		copies:   e.copyOutOfService,
		served:   e.servedFleetRevision,
	}
	d.renewal = newDutyRenewal(codeRetentionDutyName, codeRetentionDutyTTL).declining(d.declined)
	return d
}

// startCodeRetention arms the code-retention duty on a node whose runtime hosts
// code and that publishes — the only nodes whose lifecycle store may write
// ([Engine.writeSide]). One loop a node, claiming the duty each turn.
func (e *Engine) startCodeRetention(ctx context.Context, nodeID string, n *native) {
	store := e.writeSide().code
	if n == nil || n.codeReader == nil || store == nil {
		return
	}
	d := e.newCodeRetentionDuty(store, n.codeReader, nodeID)
	d.loop = startLoop(ctx, sleep, d.tick)
	e.codeRetention = d
}

// stopCodeRetention ends the duty's loop, waiting out a pass in flight.
// Nil-safe.
//
// BEFORE THE ADMISSION IS WITHDRAWN ([Engine.withdraw]), with every other
// loop that appends to a log: a pass publishes purge steps and
// re-declarations, and one still running once the admission is gone is a
// publisher a capacity coordinator cannot see. And so before
// [Engine.releaseDuties], which follows the withdrawal, as every loop that
// claims a duty is: one still running past the release takes the duty
// straight back, and the node exits holding it for its whole TTL.
func (e *Engine) stopCodeRetention() {
	d := e.codeRetention
	if d == nil {
		return
	}
	d.loop.stop()
	e.codeRetention = nil
}

// decidesFrom is the one domain the duty decides from: the code log's rows,
// whose deleted repositories it purges.
func (d *codeRetentionDuty) decidesFrom() []string { return []string{code.Domain{}.Name()} }

// declined is whether this node declines the duty now, said on a change
// ([dutyDecline.record]) — asked before every turn's claim, and before every
// renewal of a pass in flight ([dutyRenewal.declining]). A purge removes rows
// on every node and the bytes of every pack the repository named, and has no
// undo, so this node declines while either of two things is true:
//
//   - its copy of the code log is out of service ([Engine.copyOutOfService]):
//     a purge decided from it is one the node's peers may not agree was due;
//   - it does not serve the revision the fleet's activation pointer names
//     ([Engine.servedFleetRevision]), nothing activated included: the pass
//     purges by this node's code.deleted_retention_days, and a holder that
//     failed to apply a revision lengthening it — to save a deleted
//     repository — would purge that repository by the retention the company
//     replaced. Stronger than the posture on purpose, and cheap: a node
//     merely applying the revision, or starting, asks again a reconcile
//     interval later ([codeRetentionDuty.wait]), a node that failed it a
//     [code.RetentionInterval] later, and a purge is days late by design.
//
// The pass's sweeps of retired packs are declined with its purges: they
// decide from the same copy, and a sweep that waits for a node that serves
// only keeps an object from the collector a while longer.
func (d *codeRetentionDuty) declined(ctx context.Context) bool {
	reason, _ := copyDeclineReason(ctx, codeRetentionDecline, d.copies, d.decidesFrom())
	if reason.key == "" {
		reason = revisionDeclineReason(ctx, codeRetentionDecline, d.served)
	}
	return d.declines.record(ctx, codeRetentionDecline, reason)
}

// tick is one turn of the duty's loop, and how long the loop waits before the
// next ([codeRetentionDuty.wait]).
func (d *codeRetentionDuty) tick(ctx context.Context) time.Duration {
	d.turn(ctx)
	return d.wait()
}

// wait is how long the loop waits after the turn that just ran:
// [code.RetentionInterval], or sooner where the turn declined for a reason
// that clears by itself ([dutyDecline.again]) — the boot's first turn, which
// runs before `crewlet run` has built the reconciler, and a turn that meets an
// activation this node is still applying.
func (d *codeRetentionDuty) wait() time.Duration {
	return cmp.Or(d.declines.again(), code.RetentionInterval)
}

// turn is one turn of the duty: claimed, kept while its pass runs, and the
// pass's report logged. It reports the pass's report and whether a pass ran.
//
// DECLINED FIRST, before the claim ([codeRetentionDuty.declined]), and the
// duty given back if this node holds it, so a node that may purge takes it at
// its own next tick rather than once the lease lapses.
func (d *codeRetentionDuty) turn(ctx context.Context) (code.RetentionReport, bool) {
	if d.declined(ctx) {
		d.declines.giveBack(ctx, codeRetentionDecline, d.giveBack)
		return code.RetentionReport{}, false
	}
	asked := d.renewal.clock.Now() // BEFORE the claim: what it confirms runs from here
	if d.claim != nil {
		switch held, err := d.claim(ctx); {
		case err != nil:
			if ctx.Err() == nil {
				log.WarnContext(ctx, "code_retention_duty_unclaimed", "error", err)
			}
			return code.RetentionReport{}, false
		case !held:
			return code.RetentionReport{}, false
		}
	}
	// FROM HERE THE PASS KEEPS THE DUTY AS IT RUNS, and is ended the moment
	// it cannot say it still holds it or this node comes to decline it
	// ([dutyRenewal.keep]) — so no pass purges beside another holder's, nor
	// on past a copy gone wrong or a revision this node has not applied. A
	// pass stops between steps when its context ends, and the next resumes
	// from the rows.
	work, kept := d.renewal.keep(ctx, d.claim, asked)
	report, err := d.pass.Pass(work)
	ended := work.Err() != nil
	if _, stopErr := kept.Stop(); errors.Is(stopErr, errDutyDeclined) {
		// GIVEN BACK ONCE THE PASS HAS RETURNED, never from the renewal
		// that ended it: a step already on its way would otherwise land
		// after another node had taken the duty.
		d.declines.giveBack(ctx, codeRetentionDecline, d.giveBack)
	}
	logger := d.log
	if logger == nil {
		logger = log
	}
	logCodeRetention(ctx, logger, report, err, ended)
	return report, true
}

// logCodeRetention says what one pass did: at info where it swept, purged,
// released an old name, expired a scratch repository or kept one a live turn
// holds, expired, kept or held a seat's idle branch, failed (an expiry's too),
// held a name, could not read the live turns' holds or stopped at its budget,
// at debug where it found nothing due. A pass ENDED — the
// engine stopping, the duty lost or declined mid-pass, which the renewal has
// said — is no failure; any other error is a warning. There is no /fleet view
// of a pass: this line, and the scratch expiry's own
// code_scratch_expiry_skipped warning, are where a pass is read.
func logCodeRetention(ctx context.Context, logger *slog.Logger, report code.RetentionReport, err error,
	ended bool) {
	attrs := []any{"sweeps", report.Sweeps, "swept_rows", report.SweptRows,
		"checks_sweeps", report.ChecksSweeps, "checks_swept_rows", report.ChecksSweptRows,
		"queue_sweeps", report.QueueSweeps, "queue_swept_rows", report.QueueSweptRows, "purged", report.Purged,
		"steps", report.Steps, "remain", report.Remain, "released", report.Released, "expired", report.Expired,
		"scratch_held", report.ScratchHeld, "refs_expired", report.RefsExpired, "refs_kept", report.RefsKept,
		"refs_held", report.RefsHeld}
	if len(report.Failed) > 0 {
		attrs = append(attrs, "failed", report.Failed)
	}
	if len(report.Unhanded) > 0 {
		attrs = append(attrs, "unhanded", report.Unhanded)
	}
	if report.ScratchExpiryUnread != "" {
		attrs = append(attrs, "scratch_expiry_unread", report.ScratchExpiryUnread)
	}
	if len(report.ExpiryFailed) > 0 {
		attrs = append(attrs, "expiry_failed", report.ExpiryFailed)
	}
	if report.ExpiryUnread != "" {
		attrs = append(attrs, "expiry_unread", report.ExpiryUnread)
	}
	switch {
	case err != nil && !ended:
		logger.WarnContext(ctx, "code_retention_pass_failed", append(attrs, "error", err.Error(),
			"detail", "the next pass, a tick later, lists what is due again")...)
	case report.Sweeps > 0 || report.ChecksSweeps > 0 || report.QueueSweeps > 0 || len(report.Purged) > 0 ||
		len(report.Failed) > 0 || len(report.Unhanded) > 0 || report.Remain || report.Released > 0 ||
		report.Expired > 0 || report.ScratchHeld > 0 || report.ScratchExpiryUnread != "" ||
		report.RefsExpired > 0 || report.RefsKept > 0 || report.RefsHeld > 0 || len(report.ExpiryFailed) > 0 ||
		report.ExpiryUnread != "":
		logger.InfoContext(ctx, "code_retention_pass", attrs...)
	default:
		logger.DebugContext(ctx, "code_retention_pass", attrs...)
	}
}

// redeclareCode is the retention pass's [code.Retention.OnPurged]: the purge
// of a repository freed its name, and where the CURRENT epoch declares that
// name again — a person re-added it while it purged, and the reconcile could
// only wait the purge out (code_repo_awaits_purge) — it is created now rather
// than at the next activation, which nothing schedules.
//
// Under the current epoch's own activation, fenced first as every reconcile
// is. An error keeps the name for the next pass: a fence a later activation
// has raised — this node applies an older configuration — or a write that did
// not land. A name the epoch does not declare is simply free.
func (e *Engine) redeclareCode(ctx context.Context, name string) error {
	c := e.Company()
	store := e.writeSide().code
	if c == nil || c.Config == nil || c.ActivatedAt.IsZero() || !c.Config.Code.Native || store == nil {
		return nil
	}
	i := slices.IndexFunc(c.Config.Code.Repos, func(r config.CodeRepo) bool { return r.Name == name })
	if i < 0 {
		return nil
	}
	d, err := declaredRepo(codeOwners(c), c.Config.Code.Repos[i])
	if err != nil {
		return err
	}
	a, err := store.Activate(ctx, c.ActivatedAt)
	if err != nil {
		return fmt.Errorf("engine: declare %q again: %w", name, err)
	}
	ensured, err := store.EnsureRepo(ctx, a, d)
	if err != nil {
		return fmt.Errorf("engine: declare %q again: %w", name, err)
	}
	if ensured.Changed {
		// THE RECONCILE'S OWN LINE, in its own shape ([codeReposWritten.log]).
		log.InfoContext(ctx, "code_repos_applied", "created", 1, "created_names", []string{name},
			"detail", "the purge of the repository that last held this name freed it")
	}
	return nil
}
