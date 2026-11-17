package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Gitlet, wired: its settings in force, the seam the API's routes and the host
// reach it through, its committer's stop, its git clients' census term, the
// reconcile of code.repos and the code-retention duty.
//
// EVERY DATA NODE COMMITS: `code.backend: gitlet` gives every data node a
// committer and a lifecycle store ([Engine.startNative]), served to the estate
// router ([Engine.estateBackend], [gitlet.Served]), and a push is asked of its
// repository's rendezvous-first committer whichever node received it. No
// election and no per-repository lease: the repository's subject lets one
// record land per round, so nodes with differing fleet views commit safely.
// Every data node also reconciles code.repos ([Engine.applyCodeRepos]).

// codeInForce is Gitlet's settings IN FORCE at the current epoch
// ([Company.codeInForce]), none before a company. Read per call (the
// committer's decide and batches, the reader's repository detail, every
// retention pass), so a revision's settings hold from its activation with no
// restart, on each node's own epoch; the committer's repo_deleted sentence
// ([gitlet.RepoGone]) and the duty's purge read the same value, so under one
// revision they name one instant.
func (e *Engine) codeInForce() config.Code { return codeInForceAt(e.Company()) }

// codeInForceAt is Gitlet's settings in force at epoch c, for a reader that
// takes something else off the same epoch: the commit signer reads a seat's
// role and the identity domain off ONE epoch, so a revision moving both signs
// a commit by both or by neither.
func codeInForceAt(c *Company) config.Code {
	switch {
	case c == nil || c.Config == nil:
		return config.Code{}
	case c.codeInForce != nil:
		return *c.codeInForce
	}
	return c.Config.Code
}

// codeLimits is Gitlet's limits in force at the current epoch
// ([Engine.codeInForce]), the defaults applied.
func (e *Engine) codeLimits() gitlet.Limits { return gitlet.LimitsOf(e.codeInForce()) }

// codeSeatRules is what the committer judges a SEAT'S push by
// ([gitlet.CommitterOptions.SeatRules]), read once a batch that carries one:
// the current epoch's roster (the one [Engine.codeRoster] judges reads by),
// the company's clock for the day's quota, and the chart's activation that a
// mergeability stamps its readiness with ([gitlet.SeatRules.Epoch]), all off
// ONE epoch, so a revision moving a seat and the clock is judged by both or by
// neither. False before an epoch: the committer then hands a seat's push to
// the repository's next committer rather than judge it.
func (e *Engine) codeSeatRules() (gitlet.SeatRules, bool) {
	c := e.Company()
	if c == nil || c.roster == nil {
		return gitlet.SeatRules{}, false
	}
	return gitlet.SeatRules{Roster: c.roster(), Zone: c.Config.Location(), Epoch: chartEpochOf(c)}, true
}

// codeChartEpoch is the activation of the chart this node holds now
// ([chartEpochOf]), 0 while it holds none: what the home's change work names
// in its listings ([gitlethome.Options.Chart]), so a chart apply makes every
// change its records judged by an earlier chart due a refresh.
func (e *Engine) codeChartEpoch() int64 {
	c := e.Company()
	if c == nil || c.roster == nil {
		return 0
	}
	return chartEpochOf(c)
}

// chartEpochOf is the activation c's chart is of (configplane.ActivationStamp
// of the instant its revision was activated), 0 for an epoch built from no
// activation — a stamp of the zero instant is a negative millisecond, which
// would read as earlier than every chart rather than as none.
func chartEpochOf(c *Company) int64 {
	if c.ActivatedAt.IsZero() {
		return 0
	}
	return configplane.ActivationStamp(c.ActivatedAt)
}

// codeDeletedRetention is how long a deleted repository waits for its purge at
// the current epoch, the default applied. The retention pass
// ([gitlet.Retention.Retention]) and the committer's repo_deleted sentence
// ([gitlet.CommitterOptions.Retention], [gitlet.RepoGone]) both read it, so
// under one revision a push is told the instant the duty purges from. While a
// revision is applied node by node, the duty's lease holder and the
// repository's committer may read different epochs, which is why the sentence
// names the earliest the purge may begin on the deciding node's epoch, never a
// promise.
func (e *Engine) codeDeletedRetention() time.Duration {
	return e.codeInForce().DeletedRetention()
}

// codeLifecyclePolicy is code.scratch at the current epoch through the code
// domain's one conversion ([gitlet.ScratchOf]): what a tool's create is held
// to, the credentials a mirror or import may name, and how long a seat's
// scratch repository may sit idle. Read by the lifecycle store's create
// ([gitlet.StoreOptions.Policy]) and the scratch expiry
// ([gitlet.Retention.Policy]), so a revision holds from its activation.
func (e *Engine) codeLifecyclePolicy() gitlet.LifecyclePolicy {
	return gitlet.ScratchOf(e.codeInForce())
}

// codeRemoteCredentials is code.remote_credentials at the current epoch: the
// variables a mirror's or an import's dial may resolve, each for its declared
// hosts. Read at every dial (mirrorDial), so a revision that withdraws one
// stops its value leaving at the next attempt. The lifecycle decide reads the
// same table through [gitlet.ScratchOf].
func (e *Engine) codeRemoteCredentials() config.CodeRemoteCredentials {
	return e.codeInForce().RemoteCredentials
}

// carriedCode is Gitlet's settings in force at next, installed over prev
// ([Engine.installEpoch]): next's own `code` block where it sets
// `code.backend: gitlet`, otherwise prev's in force.
//
// CARRIED, because the revision that turns the host off cannot hold its
// settings (validation refuses them without the backend), yet a runtime built
// with the host keeps serving it until a restart ([backendSwitches]). Read off
// that revision, the running committer would hold pushes to the DEFAULT
// limits, and the retention duty would purge by the default seven days a
// repository kept restorable for a year: a purge has no undo.
func carriedCode(next, prev *Company) *config.Code {
	if next.Config != nil && next.Config.CodeBackendFor() == config.CodeGitlet {
		in := next.Config.Code
		return &in
	}
	if prev == nil {
		return nil
	}
	return prev.codeInForce
}

// OperatorCode is Gitlet's estate seam as the API's routes and the host reach
// it, the router's on every node for [OperatorWork]'s reason; false where this
// node's RUNNING runtime was not built with `code.backend: gitlet` (no hosted
// code, turned on but not restarted, or no native runtime yet). Which halves a
// node runs is [Engine.nativeHalves]' one answer.
func OperatorCode(e *Engine) (estate.CodeSeam, bool) {
	if e.router == nil || !e.nativeHalves().code {
		return estate.CodeSeam{}, false
	}
	return e.router.Code(), true
}

// codeEvents is the queue this node's Gitlet announces applied records on
// (the checks writer's endings, the committer's merges and opened changes, the
// change writer's reviews); nil, never a typed nil, where the node holds no
// queue, which records no event of any kind.
func (e *Engine) codeEvents() gitlet.EventPublisher {
	if e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	return e.backends.Queue
}

// codeAgentID is the agent id a seat's day is counted under, off the LIVE
// epoch when an opened change or a review is announced
// ([gitlet.CommitterOptions.AgentIDs]): the resolution [store.DB.SetEventSeats]
// files a participant's handle by, so the event's `agent_id` and its party
// rows name one seat. False for a human seat, a handle the chart does not
// hold, and a node with no company active.
func (e *Engine) codeAgentID(handle string) (string, bool) { return seatAgentID(e.Company(), handle) }

// stopCommitter ends this node's committer: a queued proposal is answered "not
// serving", which its asker takes to the repository's runner-up under the same
// push id, and a publishing batch finishes or, past the bound, answers unknown.
// Nil-safe.
//
// BEFORE THE ESTATE SUBJECT STOPS ANSWERING ([Engine.stopServingEstate]), which
// cancels answers in flight (queue.Queue.Serve's contract): a still-queued
// proposal would answer `context canceled` to the git client, final, for a push
// its batch may yet land. The order costs no wait. AND BEFORE THE ADMISSION IS
// WITHDRAWN ([Engine.withdraw]): every batch is a code-log record, and one
// appended after it is a write the capacity coordinator was told nobody makes.
func (e *Engine) stopCommitter(ctx context.Context) { e.native.Load().stopCommitter(ctx) }

// stopCommitter is [Engine.stopCommitter] on this runtime, and the stop its
// own failure path takes ([native.shutdown]); [gitlet.Committer.Stop] is
// idempotent. The change writer and the checks writer stop with it for the
// same reasons, all three closed first and drained together ([stopTogether]).
//
// BOUNDED by [statelog.DefaultResolveBudget], the bound every write's wait for
// its applier takes: past it, the batch's push ids collapse on whatever landed
// when their clients push again.
func (n *native) stopCommitter(ctx context.Context) {
	if n == nil || n.committer == nil {
		return
	}
	stopTogether(ctx, statelog.DefaultResolveBudget, n.proposalWriters()...)
}

// proposalWriters is every writer of this node's code half that answers on the
// estate's proposal lane — its committer, and its change writer and checks
// writer where it built them — each a [closingWriter] only where it is set,
// never a nil pointer behind a non-nil interface.
func (n *native) proposalWriters() []closingWriter {
	writers := []closingWriter{n.committer}
	if n.changeWriter != nil {
		writers = append(writers, n.changeWriter)
	}
	if n.checksWriter != nil {
		writers = append(writers, n.checksWriter)
	}
	return writers
}

// closingWriter is a writer as the teardown stops it: Close refuses every new
// write, and every one still waiting for its turn, as a node that ran nothing,
// leaving the publishes in flight to run; Stop closes it and waits for those
// publishes, cancelling them once its context ends. [gitlet.Committer],
// [gitlet.ChangeWriter] and [gitlet.ChecksWriter] satisfy it.
type closingWriter interface {
	Close()
	Stop(ctx context.Context)
}

// stopTogether stops writers under ONE bound, CLOSING EVERY ONE FIRST and only
// then draining them ALL AT ONCE.
//
// CLOSED FIRST, because an open writer keeps admitting and publishing work
// while another's stop waits out the bound: records the estate subject's
// withdrawal and the admission's then cut short. DRAINED TOGETHER, because the
// bound is the teardown's, not each writer's: drained in turn, a writer after
// one that spent the bound gets an ended context and cancels publishes it
// could have finished.
func stopTogether(ctx context.Context, bound time.Duration, writers ...closingWriter) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
	defer cancel()
	for _, w := range writers {
		w.Close()
	}
	var drained sync.WaitGroup
	for _, w := range writers {
		drained.Go(func() { w.Stop(bounded) })
	}
	drained.Wait()
}

// clientTerm is one log's census term of readers that are neither seats nor
// the engine's own passes ([statelog.Census.Clients]), as a function of the
// byte ceiling the log's stream HOLDS and the trim's min_age.
type clientTerm func(holds int64, minAge time.Duration) int

// clientBarriers is the census term of a log's readers that are neither seats
// nor the engine's own passes: for the code log its git clients,
// [gitlet.GitReadsPerDay] over the ceiling its stream holds, so a resize
// (`crewlet retention set-capacity`) moves the census with it; nil otherwise.
//
// A TERM OF THE LOG, WHATEVER THIS NODE HOSTS: the code log keeps its ceiling
// in every register, and the census is held against the barriers EVERY node
// applied over the alarm's day ([retention.census]). Keyed on this node's
// runtime, a healthy host fires census_drift for up to a day around a switch
// of the backend, its fleet's git reads judged against the seats' term alone.
// A company that never hosted code just allows for reads that never come.
func clientBarriers(domain string) clientTerm {
	if domain != (gitlet.Domain{}).Name() {
		return nil
	}
	return gitlet.GitReadsPerDay
}

// applyCodeRepos asks for the reconcile of code.repos at this activation
// ([reconcileCodeRepos]): declared repositories exist with their settings, and
// configuration-managed ones no longer declared are deleted, restorably for
// `code.deleted_retention_days`. Never the apply's failure
// ([Engine.applyContainers]' posture) and never waited on: a reconcile costs a
// write per changed repository (16–19 s at ten thousand) while a node 45 s
// behind is shed, so [codeReconciler] decides beside the apply, runs again
// what is left undone on a backoff (nothing else would), and ends an older
// activation's run for a newer one. Every data node runs it at every boot and activation, safely
// ([reconcileCodeRepos]); none runs it for a revision that turned the host off
// (nothing is deleted for that), on a node that does not publish
// ([Engine.writeSide]), or before a first activation ([Engine.applyChart]).
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
func (e *Engine) codeReposSides(c *Company, activatedAt time.Time) (*gitlet.Store, *gitlet.Reader, bool) {
	n := e.native.Load()
	store := e.writeSide().code
	if n == nil || n.codeReader == nil || store == nil || c == nil || c.Config == nil ||
		activatedAt.IsZero() || c.Config.CodeBackendFor() != config.CodeGitlet {
		return nil, nil, false
	}
	return store, n.codeReader, true
}

// stopCodeRepos ends the reconcile of code.repos in flight and refuses every
// request after ([codeReconciler.stop]). Nil-safe. BEFORE THE ADMISSION IS
// WITHDRAWN ([Engine.withdraw]), with every loop that appends to a log: a
// reconcile still publishing after it is a publisher a capacity coordinator
// cannot see.
func (e *Engine) stopCodeRepos() {
	if e.codeRepos != nil {
		e.codeRepos.stop()
	}
}

// codeLifecycle is the lifecycle store as the reconcile of code.repos calls
// it; *gitlet.Store satisfies it.
type codeLifecycle interface {
	Activate(ctx context.Context, activatedAt time.Time) (gitlet.Activation, error)
	EnsureRepos(ctx context.Context, a gitlet.Activation, declared []gitlet.Declared) ([]gitlet.EnsureResult, error)
	DeleteRepos(ctx context.Context, a gitlet.Activation, ids []string) ([]gitlet.DeleteResult, error)
}

// codeLives is this node's code rows as the reconcile walks them;
// *gitlet.Reader satisfies it.
type codeLives interface {
	WalkLives(ctx context.Context, visit func(gitlet.RepoLife) error) error
}

// reconcileCodeRepos is one reconcile of code.repos (owners resolved on the
// activation's chart) at activatedAt. It raises the code log's ACTIVATION FENCE
// first ([gitlet.Store.Activate]) and every decision waits for this node to
// apply it, so older activations' earlier records are settled back and their
// later ones refused on every node (internal/gitlet's package doc); an
// unchanged activation writes that one record. Changed repositories are decided
// side by side, racing nodes landing one record a subject
// ([gitlet.Store.EnsureRepos], [gitlet.Store.DeleteRepos]). A shared failure is
// logged ONCE and no deletion follows; one repository's is logged and the rest
// decided. It answers the first failure a second run can apply, for
// [codeReconciler]; nil for the design working (a later activation, a stop, a
// purge [Engine.redeclareCode] waits out) or what only a revision changes.
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
	decided := make([]gitlet.Declared, 0, len(repos))
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
	// THE ROWS BEFORE THE DECISIONS, stale and narrow: what each declared
	// name was (naming what a write did) and which configuration-managed
	// repositories go. Stale is enough: every deletion decides in its own
	// snapshot after the fence. Held as a name map and a list, never the rows.
	was := make(map[string]gitlet.RepoStatus, len(declared))
	var gone []gitlet.RepoLife
	listErr := lives.WalkLives(ctx, func(l gitlet.RepoLife) error {
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
		// THE SHARED FAILURE IS THE ANSWER, whatever a repository's own was:
		// a run again decides that repository with the rest once it clears.
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
func (w *codeReposWritten) ensured(name string, was map[string]gitlet.RepoStatus, listed bool) {
	switch status, found := was[name]; {
	case !listed:
		w.written = append(w.written, name)
	case !found:
		w.created = append(w.created, name)
	case status == gitlet.RepoStatusDeleted:
		w.restored = append(w.restored, name)
	default:
		w.changed = append(w.changed, name)
	}
}

// importsIgnored says, in ONE line a run, which declarations name an import
// for a repository that already existed (was: live or deleted) and is not
// being imported: an import is read by its repository's create alone
// ([gitlet.Declared.Import]), so the line tells a person nothing will be
// fetched, or that a finished import can go. Its count and first
// [codeReposLoggedNames] names; nothing where there are none.
func importsIgnored(ctx context.Context, logger *slog.Logger, decided []gitlet.Declared,
	was map[string]gitlet.RepoStatus) {

	var names []string
	for _, d := range decided {
		if status, existed := was[d.Name]; existed && status != gitlet.RepoStatusImporting && d.Import != nil {
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
// kind of write, beside the count: 32 names of at most 63 bytes are 2 KB, where
// ten thousand pass the 16 KB at which a container runtime's json log splits a
// line, and `crewlet gitlet status` lists them all anyway.
const codeReposLoggedNames = 32

// log says what was written in one line, and nothing where nothing was: it
// runs at every boot and activation on every data node, and a line each time
// would report a company nobody edited as one that changed. Each kind is its
// COUNT and its first [codeReposLoggedNames] names.
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
// newer activation's request, [gitlet.ErrSuperseded], or the engine stopping —
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
	case errors.Is(err, gitlet.ErrSuperseded):
		// A LATER ACTIVATION OWNS THE REPOSITORIES (fenced by the fleet or
		// this node): every decision this one made is refused everywhere.
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
	case errors.Is(err, gitlet.ErrPurging):
		logger.InfoContext(ctx, "code_repo_awaits_purge", "name", name,
			"detail", "the purge of the repository that last held this name completes "+
				"first; the code-retention duty declares it again when it does")
		return nil
	case errors.Is(err, gitlet.ErrSuperseded):
		logger.InfoContext(ctx, "code_repo_superseded", "name", name,
			"detail", "a later configuration's reconcile owns this repository now")
		return nil
	case errors.Is(err, gitlet.ErrNotConfigManaged):
		// SOMEBODY ELSE'S REPOSITORY HOLDS THE NAME, and a reconcile never
		// takes one over: said at each run until a revision renames it.
		logger.ErrorContext(ctx, "code_repo_not_applied", "name", name, "error", err.Error(),
			"detail", "a repository code.repos does not manage holds this name, and the "+
				"configuration never takes one over: rename the declaration; the rest of "+
				"code.repos is still applied")
		return nil
	case errors.Is(err, gitlet.ErrInvalid):
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
// absent from code.repos: a LIVE one (importing, active or archived) that the
// CONFIGURATION manages. ONLY THE CONFIGURATION'S: another manager's, a later
// build's value among them, is no declaration's to retire, and this build may
// be a newer one's OLDER PEER in a rolling upgrade. [gitlet.Store.DeleteRepo]
// checks the manager again in its own snapshot; this is the reconcile not
// asking.
func retiredByOmission(l gitlet.RepoLife) bool {
	live := l.Status == gitlet.RepoStatusImporting || l.Status == gitlet.RepoStatusActive ||
		l.Status == gitlet.RepoStatusArchived
	return live && l.ManagedBy == gitlet.ManagedConfig
}

// declaredRepo is one code.repos entry as the domain records it, on the
// activation's chart indexed once ([codeOwners]): owners resolved to a unit's
// key or a seat's handle, protection rules as namespaced patterns SORTED,
// gates and path owners in their one form ([gitlet.CanonicalGate],
// [gitlet.CanonicalPathOwners]), so every node forms byte-identical records
// ([gitlet.RepoSettings.Validate] refuses any other order); branch, visibility
// and search defaulted, search to indexed. Mirrors and import as written: a
// credential stays its ${VAR} reference, resolved only at the dial.
//
// AN ERROR, [gitlet.ErrInvalid], only for what validation refuses (an owner
// the chart does not resolve): named rather than recorded with an empty owner,
// and only a revision corrects it.
func declaredRepo(o *org.Index, r config.CodeRepo) (gitlet.Declared, error) {
	if o == nil {
		return gitlet.Declared{}, fmt.Errorf("%w: the epoch has no chart to resolve the "+
			"owner on", gitlet.ErrInvalid)
	}
	owner, err := declaredOwner(o, "owner", r.Owner.Unit, r.Owner.Seat)
	if err != nil {
		return gitlet.Declared{}, err
	}
	protections := make([]gitlet.Protection, 0, len(r.Protect))
	for _, rule := range r.Protect {
		p := gitlet.Protection{PushRule: gitlet.PushRule{
			Pattern: rule.Pattern(), AllowForce: rule.AllowForce, AllowDelete: rule.AllowDelete,
			DirectPush: gitlet.DirectPush(rule.DirectPush.Resolved()),
		}}
		if g := rule.Gate(); g != nil {
			gate := gitlet.CanonicalGate(declaredGate(*g))
			p.Gate = &gate
		}
		protections = append(protections, p)
	}
	slices.SortFunc(protections, func(a, b gitlet.Protection) int { return cmp.Compare(a.Pattern, b.Pattern) })
	owners := make([]gitlet.PathOwner, 0, len(r.PathOwners))
	for i, po := range r.PathOwners {
		who, err := declaredOwner(o, fmt.Sprintf("path_owners[%d]", i), po.Unit, po.Seat)
		if err != nil {
			return gitlet.Declared{}, err
		}
		owners = append(owners, gitlet.PathOwner{Owner: who, Paths: po.Paths})
	}
	d := gitlet.Declared{Name: r.Name, Settings: gitlet.RepoSettings{
		ManagedBy: gitlet.ManagedConfig, Owner: owner, Visibility: gitlet.Visibility(r.Visibility.Resolved()),
		Search: gitlet.SearchMode(r.Search.Resolved()), DefaultBranch: r.ResolvedDefaultBranch(),
		Description: r.Description, Archived: r.Archived, Protections: protections,
		PathOwners: gitlet.CanonicalPathOwners(owners),
	}}
	if m := r.MirrorFrom; m != nil {
		d.Settings.MirrorFrom = &gitlet.MirrorSource{URL: m.URL, Credential: m.Credential}
	}
	if m := r.MirrorTo; m != nil {
		d.Settings.MirrorTo = &gitlet.MirrorTarget{URL: m.URL, Credential: m.Credential,
			As: strings.TrimSpace(m.As), Refs: slices.Clone(m.Refs)}
	}
	if i := r.Import; i != nil {
		d.Import = &gitlet.ImportSource{URL: i.URL, Credential: i.Credential}
	}
	return d, nil
}

// declaredGate is a rule's resolved merge gate ([config.CodeProtection.Gate])
// as the code domain records it: each value of config's closed sets the
// domain's of the same spelling (TestEveryDeclaredMergeByIsOneTheDomainRecords
// holds the sets to each other), its lists in the document's order —
// [gitlet.CanonicalGate] forms the record's — and nil where the document names
// none, so a gate that names no human path records none.
func declaredGate(g config.CodeGate) gitlet.MergeGate {
	out := gitlet.MergeGate{
		MergeBy: gitlet.MergeBy(g.MergeBy), Approvals: gitlet.Approvals(g.Approvals),
		ApprovalsUnless: declaredClasses(g.ApprovalsUnless), Checks: g.Checks,
		Landing:       gitlet.Landing{Depth: g.Landing.Depth, Batch: g.Landing.Batch},
		OwnersApprove: g.OwnersApprove, DeleteSource: gitlet.DeleteSource(g.DeleteSource),
		AgentsEditChecks: g.AgentsEditChecks,
	}
	if g.HumanPaths != nil {
		out.HumanPaths = make([]gitlet.HumanPath, len(g.HumanPaths))
		for i, h := range g.HumanPaths {
			out.HumanPaths[i] = gitlet.HumanPath{Glob: h.Glob, Unless: declaredClasses(h.Unless)}
		}
	}
	return out
}

// declaredClasses is a list of config's change classes as the domain's: nil
// for none.
func declaredClasses(classes []config.CodeChangeClass) []gitlet.ChangeClass {
	if classes == nil {
		return nil
	}
	out := make([]gitlet.ChangeClass, len(classes))
	for i, c := range classes {
		out[i] = gitlet.ChangeClass(c)
	}
	return out
}

// declaredOwner is the owner a declaration names at field — exactly one of a
// unit, resolved to its KEY whichever spelling the declaration used, or a seat,
// by its handle — or [gitlet.ErrInvalid] for one the configuration's validation
// refuses.
func declaredOwner(o *org.Index, field, unit, seat string) (gitlet.Owner, error) {
	switch unit, seat = strings.TrimSpace(unit), strings.TrimSpace(seat); {
	case unit != "" && seat == "":
		u := o.UnitByRef(unit)
		if u == nil {
			return gitlet.Owner{}, fmt.Errorf("%w: %s.unit: %q is no unit of the chart, which the "+
				"configuration's validation refuses", gitlet.ErrInvalid, field, unit)
		}
		return gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: u.Key()}, nil
	case seat != "" && unit == "":
		if o.SeatByHandle(seat) == nil {
			return gitlet.Owner{}, fmt.Errorf("%w: %s.seat: %q is no seat's handle, which the "+
				"configuration's validation refuses", gitlet.ErrInvalid, field, seat)
		}
		return gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: seat}, nil
	}
	return gitlet.Owner{}, fmt.Errorf("%w: %s: a declaration names exactly one of a unit and a seat, "+
		"which the configuration's validation requires", gitlet.ErrInvalid, field)
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
	// retired past gitlet.RetireGrace and purges the repositories deleted past
	// their retention.
	codeRetentionDutyName = "code-retention"

	// codeRetentionDutyTTL is how long the duty outlives a holder that
	// stopped renewing it: three ticks ([gitlet.RetentionInterval]), every
	// singleton's rule, against a purge days late by design. A pass renews
	// it as it runs ([dutyRenewal.keep]).
	codeRetentionDutyTTL = 3 * gitlet.RetentionInterval
)

// codeRetentionDuty is the code-retention duty as one node runs it: the pass
// ([gitlet.Retention]), and the claim, the renewal, the decline and the give-back
// every fleet duty that decides from a domain's rows goes through.
type codeRetentionDuty struct {
	pass *gitlet.Retention

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
func (e *Engine) newCodeRetentionDuty(store *gitlet.Store, reader *gitlet.Reader, nodeID string) *codeRetentionDuty {
	d := &codeRetentionDuty{
		pass: &gitlet.Retention{
			Store: store, Reader: reader,
			// THE CURRENT EPOCH'S, read at every pass: a revision that
			// lengthens the retention saves a repository the next pass
			// would have purged.
			Retention: e.codeDeletedRetention,
			OnPurged:  e.redeclareCode,
			// AND ITS code.scratch, for the scratch expiry, which deletes
			// nothing a live turn holds ([Engine.codeHolds]).
			Policy: e.codeLifecyclePolicy,
			Holds:  e.codeHolds,
			// AND ITS REF EXPIRY, last: seats' idle branches deleted through
			// the router's committer, which fails over, keeping every branch
			// the same holds name.
			Propose: e.router.Code().Propose,
			Node:    nodeID,
			Logger:  logging.Get("gitlet"),
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
// Nil-safe. BEFORE THE ADMISSION IS WITHDRAWN ([Engine.withdraw]), with every
// loop that appends to a log (a pass publishes purge steps and
// re-declarations), and so before [Engine.releaseDuties]: a loop running past
// the release takes the duty back, and the node exits holding it a whole TTL.
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
func (d *codeRetentionDuty) decidesFrom() []string { return []string{gitlet.Domain{}.Name()} }

// declined is whether this node declines the duty now, said on a change
// ([dutyDecline.record]), asked before every claim and every renewal
// ([dutyRenewal.declining]). A purge has no undo, so it declines while:
//
//   - its code log copy is out of service ([Engine.copyOutOfService]): peers
//     may not agree a purge it decides was due;
//   - it does not serve the fleet's activated revision
//     ([Engine.servedFleetRevision]), stricter than the posture on purpose: a
//     holder that failed a revision lengthening code.deleted_retention_days
//     would purge by the one it replaced. Cheap: a purge is days late by design.
//
// The pack sweeps decline with it, deciding from the same copy.
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
// [gitlet.RetentionInterval], or sooner where the turn declined for a reason
// that clears by itself ([dutyDecline.again]) — the boot's first turn, which
// runs before `crewlet run` has built the reconciler, and a turn that meets an
// activation this node is still applying.
func (d *codeRetentionDuty) wait() time.Duration {
	return cmp.Or(d.declines.again(), gitlet.RetentionInterval)
}

// turn is one turn of the duty: claimed, kept while its pass runs, and the
// pass's report logged. It reports the pass's report and whether a pass ran.
//
// DECLINED FIRST, before the claim ([codeRetentionDuty.declined]), and the
// duty given back if this node holds it, so a node that may purge takes it at
// its own next tick rather than once the lease lapses.
func (d *codeRetentionDuty) turn(ctx context.Context) (gitlet.RetentionReport, bool) {
	if d.declined(ctx) {
		d.declines.giveBack(ctx, codeRetentionDecline, d.giveBack)
		return gitlet.RetentionReport{}, false
	}
	asked := d.renewal.clock.Now() // BEFORE the claim: what it confirms runs from here
	if d.claim != nil {
		switch held, err := d.claim(ctx); {
		case err != nil:
			if ctx.Err() == nil {
				log.WarnContext(ctx, "code_retention_duty_unclaimed", "error", err)
			}
			return gitlet.RetentionReport{}, false
		case !held:
			return gitlet.RetentionReport{}, false
		}
	}
	// FROM HERE THE PASS KEEPS THE DUTY, ended the moment it cannot say it
	// holds it or this node declines ([dutyRenewal.keep]), so no pass purges
	// beside another holder's. It stops between steps; the next resumes from
	// the rows.
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

// logCodeRetention says what one pass did: at info where it did, kept, held or
// failed anything or stopped at its budget, at debug where nothing was due. A
// pass ENDED (the engine stopping, the duty lost or declined mid-pass, which
// the renewal has said) is no failure; any other error is a warning. There is
// no /fleet view of a pass: this line and the scratch expiry's
// code_scratch_expiry_skipped warning are where it is read.
func logCodeRetention(ctx context.Context, logger *slog.Logger, report gitlet.RetentionReport, err error,
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

// redeclareCode is the retention pass's [gitlet.Retention.OnPurged]: where the
// CURRENT epoch declares the name a purge freed (the reconcile could only wait
// the purge out, code_repo_awaits_purge), it is created now, since nothing
// schedules the next activation. Fenced under the epoch's own activation as
// every reconcile is; an error (a later activation's fence, a write that did
// not land) keeps the name for the next pass.
func (e *Engine) redeclareCode(ctx context.Context, name string) error {
	c := e.Company()
	store := e.writeSide().code
	if c == nil || c.Config == nil || c.ActivatedAt.IsZero() || c.Config.CodeBackendFor() != config.CodeGitlet || store == nil {
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
