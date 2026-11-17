package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/checkrun"
	gitletcompact "github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	gitlethome "github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/gitlet/mirror"
	"github.com/crewlet/crewlet/internal/gitlet/query"
	"github.com/crewlet/crewlet/internal/gitlet/readaudit"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracing"
)

// GITLET ON A GITLET NODE: the cache of the company's repositories, the git
// pool every process runs in, and the host (smart HTTP for persons) over them,
// on a node with the gitlet role whose company sets code.backend: gitlet. The
// committer and lifecycle store are every data node's (code.go); the host
// reaches the code log through the estate's router like every other surface.

// Chart is the CURRENT epoch's organization, nil before a company: read per
// call, never captured, because an apply replaces the organization wholesale.
// Who a person pushing to Gitlet is comes from the guard's own principal,
// never from this.
func (e *Engine) Chart() *org.Organization {
	if c := e.Company(); c != nil {
		return c.Org
	}
	return nil
}

// GitletHost is Gitlet on this node: present where this node has the gitlet role
// and its runtime was built for a company that sets code.backend: gitlet. Asked
// per request, never captured: the runtime may be built at an apply after the
// routes are mounted.
func (e *Engine) GitletHost() (*host.Server, bool) {
	n := e.native.Load()
	if n == nil || n.gitletHost == nil {
		return nil, false
	}
	return n.gitletHost, true
}

// Why this node serves no part of Gitlet now ([Engine.CodeUnserved]).
var (
	// ErrCodeNotHosted: the company this node serves does not set
	// code.backend: gitlet, and nor does the configuration the fleet activated, as
	// this node last read it.
	ErrCodeNotHosted = errors.New("engine: this company does not host code (code.backend is not gitlet)")
	// ErrCodeApplying: this node is behind the configuration the fleet
	// activated and is applying it — a first apply building the native
	// runtime among them. Clears in seconds, by itself.
	ErrCodeApplying = errors.New("engine: this node is applying the configuration the fleet activated")
	// ErrCodeApplyFailed: the configuration the fleet activated sets
	// code.backend: gitlet, as this node read it, and this node's apply of it
	// failed (config_apply_failed). Clears only when an apply succeeds,
	// usually a corrected revision's.
	ErrCodeApplyFailed = errors.New("engine: this node could not apply the configuration the fleet " +
		"activated, which hosts code (config_apply_failed)")
	// ErrCodeUnknown: this node's apply of the activated configuration failed
	// BEFORE it could read it (config_apply_failed): not read, fetched, opened
	// with this node's key or parsed. So it never claims [ErrCodeApplyFailed]'s
	// "which hosts code" to a company that may host none. Clears when an
	// attempt reads the revision.
	ErrCodeUnknown = errors.New("engine: this node could not read the configuration the fleet " +
		"activated, so cannot say whether this company hosts code (config_apply_failed)")
	// ErrCodeNeedsRestart: the company this node serves sets code.backend: gitlet,
	// and this node's native runtime was already running without
	// Gitlet when it was set; it serves it once restarted
	// (native_backend_needs_restart).
	ErrCodeNeedsRestart = errors.New("engine: this node's native runtime was started without Gitlet, " +
		"and serves it once restarted (native_backend_needs_restart)")
)

// CodeUnserved says why this node serves no part of Gitlet (neither
// [Engine.GitletHost] nor [OperatorCode]), as one of five sentinels: not hosted
// is a 404, the other four a 503, and only an apply in flight clears by waiting.
// Asked only where Gitlet was found absent; the caller asks again after it, as
// the runtime may have come up in between (api.EngineCode).
//
// Decided from what the fleet ACTIVATED as this node's reconciler last read it,
// never the published epoch alone: an apply publishes only once its runtime is
// built, so a node building Gitlet, or whose apply failed, still serves an
// epoch saying no code is hosted. Nothing undecoded is claimed: a failed apply
// is [ErrCodeApplyFailed] only where it decoded code.backend: gitlet, else
// [ErrCodeUnknown]; one in flight is [ErrCodeApplying].
func (e *Engine) CodeUnserved() error {
	if c := e.Company(); c != nil && c.Config != nil && c.Config.CodeBackendFor() == config.CodeGitlet {
		// The served epoch hosts code, so this runtime was built before Gitlet
		// was turned on: an apply starts Gitlet before it publishes the epoch.
		return ErrCodeNeedsRestart
	}
	r := e.reconciler.Load()
	if r == nil {
		return ErrCodeNotHosted
	}
	p := r.snapshot()
	switch {
	case p.target == 0 || p.target == p.applied:
		return ErrCodeNotHosted
	case p.targetRead && !p.targetCode:
		return ErrCodeNotHosted
	case p.attempts > 0 && p.attemptStarted.IsZero() && !p.targetRead:
		return fmt.Errorf("%w (epoch %d)", ErrCodeUnknown, p.target)
	case p.attempts > 0 && p.attemptStarted.IsZero():
		return fmt.Errorf("%w (epoch %d)", ErrCodeApplyFailed, p.target)
	}
	return fmt.Errorf("%w (epoch %d)", ErrCodeApplying, p.target)
}

// auditGit records one push through the operator surface's audit, whose
// envelope's source is the operator's and whose publish outlives the request.
func (e *Engine) auditGit(ctx context.Context, r types.OperatorActed) {
	operator.Audit(ctx, e.backends.Queue, types.NewOperatorActed(r))
}

// recordCodeRead files one read of a `visibility: unit` repository (a fetch
// Gitlet served, or a git read) in this node's reads ledger ([Engine.codeReads],
// internal/gitlet/readaudit), stamped with its instant, its company day on the
// CURRENT epoch's clock ([Engine.Zone]) and its trace. A flush publishes each
// reader's reads as one `code_fetched` event, from which the usage publisher
// derives who read the repository (internal/usage, kind repo_read).
//
// Never a publish of its own, which made every read's answer wait on the
// broker's replicated ack and lost the record where the broker refused it: the
// flush runs off the read's path, keeps a refused record for the next one and
// outlives the request. Never through [Engine.auditGit]: the dashboard reads
// that source as people's calls, and a read is neither.
func (e *Engine) recordCodeRead(ctx context.Context, read types.CodeFetched) {
	at := time.Now().UTC()
	e.codeReads.Add(readaudit.Read{Fetched: read, At: at,
		Day: period.At(period.Day, at, e.Zone()).Label, Trace: tracing.TraceOf(ctx)})
}

// roleNeeded is [seat.Config.RoleNeeded]: the CURRENT epoch's company's
// answer ([config.Company.NeedsRole]), and no company's before one is applied.
// An atomic load of the epoch, since the sweep must not block.
func (e *Engine) roleNeeded(role placement.NodeRole) bool {
	var cfg *config.Company
	if c := e.Company(); c != nil {
		cfg = c.Config
	}
	return cfg.NeedsRole(role)
}

// hostsGit reports whether a runtime built with built serves git on this node:
// the company hosts code and this node has the gitlet role. A gitlet role on a
// company that hosts none serves nothing, and code.backend: gitlet on a node without
// the role is served by the fleet's Gitlet nodes.
func (e *Engine) hostsGit(built builtWith) bool {
	return built.runsCode() && e.profile.Roles.Has(placement.RoleGitlet)
}

// gitHook is the argv the cache's proc-receive hook execs: [Options.GitHook]
// where it is given, and otherwise THIS binary as host.HookCommand names it —
// `crewlet gitlet hook proc-receive` — with its symbolic links resolved, since
// git runs the hook with the pool's PATH and a link replaced by an upgrade
// would start a different binary mid-push.
func (e *Engine) gitHook() ([]string, error) {
	if len(e.gitHookArgv) > 0 {
		return e.gitHookArgv, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("engine: Gitlet's hook is this binary, and os.Executable "+
			"could not name it: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, fmt.Errorf("engine: Gitlet's hook is this binary, and its path "+
			"could not be resolved: %w", err)
	}
	return host.HookCommand(exe), nil
}

// buildGitletHost builds and starts Gitlet on this node for the runtime being
// started (directory, git pool, budget, cache, git reads, host, each needing
// the last) and answers the host's status (codestatus.go). Called by
// startNative only where [Engine.hostsGit]; an error fails the boot or the apply.
//
// A FAILURE GIVES THE DIRECTORY BACK: Prepare claims it for this process, so a
// retried apply would find it held by its own failed attempt. Before Open the
// layout is released, after it the cache is stopped.
//
// nodeID is the core runtime's resolved id ([core.nodeID]), which every proposal
// names as the node that received the push ([host.Options.NodeID]); codeLog is
// this node's own code log, whose records the home's nudges watch.
func (e *Engine) buildGitletHost(ctx context.Context, nodeID string, boot *config.Bootstrap,
	codeLog *runningLog) (gitletHostParts, error) {

	// 0. IN A TEST, GITLET IS SIZED BY ITS CASE, before anything is built
	// for it ([ErrUndeclaredCodeCache]).
	if err := e.undeclaredCodeCache(boot); err != nil {
		return gitletHostParts{}, err
	}

	// 1. THE DIRECTORY, prepared BEFORE git's HOME exists, since it names it.
	// Absolute: a store path written relative resolves against the working
	// directory the store itself is opened from.
	dir, err := filepath.Abs(boot.Gitlet.CacheDirFor(boot.Store.Path))
	if err != nil {
		return gitletHostParts{}, fmt.Errorf("engine: gitlet.cache_dir: %w", err)
	}
	layout, err := cache.Prepare(ctx, dir)
	if err != nil {
		return gitletHostParts{}, fmt.Errorf("engine: gitlet.cache_dir %s: %w", dir, err)
	}
	defer layout.Release()

	// 2. GIT, the node's own: the pool resolves its lanes once (code_git_ready),
	// sized by the node's memory ([Options.gitMemory] in this package's tests).
	pool, err := gitproc.New(ctx, gitproc.Options{
		MaxProcesses: boot.Gitlet.MaxProcesses, MaxTransfers: boot.Gitlet.MaxTransfers,
		Memory: e.gitMemory, Home: layout.Home, Metrics: e.metrics,
	})
	if err != nil {
		return gitletHostParts{}, fmt.Errorf("engine: this node has the %q role and the "+
			"company sets code.backend: gitlet, so it serves git: %w", placement.RoleGitlet, err)
	}

	// 3. THE BUDGET, after the pool: its transfer lane sizes the request spools
	// that share the cache's volume outside the budget.
	size, err := codeCacheBudget(boot, layout, pool.Slots(gitproc.ClassTransfer), e.cacheVolume())
	if err != nil {
		return gitletHostParts{}, err
	}
	size.log(ctx, layout.Dir)

	// 4. THE OBJECTS a push's pack bundle is stored in and a sync reads.
	objects := e.Objects()
	if objects == nil {
		return gitletHostParts{}, fmt.Errorf("engine: code.backend: gitlet needs the object store " +
			"(store.objects): this node has none")
	}

	// 5. THE HOOK every push's receive-pack execs.
	hook, err := e.gitHook()
	if err != nil {
		return gitletHostParts{}, err
	}

	// 6. THE CACHE, taking over the layout's claim: rows read through the
	// router, index backfill paced by the fleet's live git and data nodes as
	// the router's placements count them, and code indexes kept for the
	// repositories this node is among the first gitlet.IndexHolders of, in the
	// router's git order (M6-D12).
	seam := e.router.Code()
	c, err := cache.Open(ctx, cache.Options{
		Layout: layout, MaxBytes: size.budget, Git: pool, Objects: objects, Fetch: seam, Fleet: e.codeFleet,
		Placement: &cache.Placement{Node: nodeID, Order: e.router.GitOrder},
		Limits:    e.codeLimits, HookCommand: hook, Metrics: e.metrics, Now: time.Now,
	})
	if err != nil {
		return gitletHostParts{}, fmt.Errorf("engine: open the code cache at %s: %w", layout.Dir, err)
	}

	// 7. THE GIT READS: server-side reads by object id a seat asks of this
	// node as a repository's home (ADR-0089), a grant judged by the current
	// epoch's chart and refused while this node is behind the configuration,
	// both read per read. They hold the node's ONE seat git-work share of the
	// read lane, which engine writes (step 8) draw on too: two shares would
	// let seats' reads and builds hold the whole lane. No goroutine of their
	// own; the cache's stop ends them. THE RE-SYNC BUCKETS are likewise ONE
	// set a node, shared by the query server and Gitlet's git surface
	// (gitlet.NewResyncs names who pays which): the census gives a seat one
	// term on the log, not one per surface.
	share := gitlet.NewGitShare(nodeID, pool.Slots(gitproc.ClassRead), e.metrics)
	resyncs := gitlet.NewResyncs(time.Now, e.metrics)
	reads, err := query.New(query.Options{
		NodeID: nodeID, Cache: c, Git: pool, Share: share, Resyncs: resyncs,
		Roster: e.codeRoster, Current: e.admits, Fetched: e.recordCodeRead,
	})
	if err != nil {
		c.Stop(context.WithoutCancel(ctx))
		return gitletHostParts{}, fmt.Errorf("engine: build the git reads: %w", err)
	}
	// AND ITS MERGE PREVIEWS (internal/gitlet/home's Previews): a comparison's
	// preview of merging one commit into another by the merge the home records
	// mergeability by, judged, shared and recorded as the git reads are, over
	// the same cache, pool and share, in a scratch of its own. No goroutine.
	previews, err := gitlethome.NewPreviews(gitlethome.PreviewOptions{
		NodeID: nodeID, Cache: c, Git: pool, Share: share,
		Roster: e.codeRoster, Current: e.admits, Fetched: e.recordCodeRead, Metrics: e.metrics,
	})
	if err != nil {
		c.Stop(context.WithoutCancel(ctx))
		return gitletHostParts{}, fmt.Errorf("engine: build the merge previews: %w", err)
	}

	// 8. THE HOST, over the engine's seams: the router for rows and the
	// committer, the operator audit for every push, the current epoch's limits
	// and retention; the pusher is the API guard's answer per request. An
	// ENGINE WRITE (ADR-0092) is judged as a read is, signed from the current
	// epoch (codeSigner) and built under the reads' share. A CODING BOX is
	// admitted by its run's token (boxTokens) and its run's row on the
	// coordination store, read by every node (codeBoxRuns), its push refused
	// while a person's pause stops its seat (stoppedBy). A CHECK'S BOX by its
	// check's token (checkTokens) and its attempt's row via the router.
	// externalBase is where a push's printed change link points: this node
	// serves git on its API listener, which `api.external_url` is required beside.
	h, err := host.New(host.Options{
		Cache: c, Git: pool, Estate: seam, Objects: objects,
		Audit: e.auditGit, Fetched: e.recordCodeRead,
		Limits: e.codeLimits, DeletedRetention: e.codeDeletedRetention,
		Roster: e.codeRoster, Signer: codeSigner{e: e}, Share: share, Resyncs: resyncs, Current: e.admits,
		Boxes:  host.Boxes{Tokens: e.boxTokens, Checks: e.checkTokens, Runs: e.codeBoxRuns(log)},
		NodeID: nodeID, ExternalBase: e.externalBase, Timing: host.Production, Metrics: e.metrics,
	})
	if err != nil {
		c.Stop(context.WithoutCancel(ctx))
		return gitletHostParts{}, fmt.Errorf("engine: build Gitlet: %w", err)
	}
	// ITS OWN LIFETIME from here: the relay and the cache's background work
	// run until the host is stopped, however long this call's caller lives.
	if err = h.Start(ctx); err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return gitletHostParts{}, fmt.Errorf("engine: start Gitlet: %w", err)
	}

	// 9. THE COMPACTOR: a repository MAINTAINER's three loops (pack merges,
	// ADR-0090; layer publication, ADR-0091; a purge's expunge) over the host's
	// cache and store, proposing through the router's code seam (whose Compact
	// raises this node's floors past what it landed) and asking the router who
	// maintains each repository. BUILT here, after the host whose cache it
	// builds in; STARTED only by [Engine.startNativeDuties], once the runtime is
	// published and only where the node's mode publishes: a pass reads through
	// that runtime, and a node a capacity window restarted to write nothing must
	// store no compactions. Stopped before the host in [native.stopGitletHost],
	// where a stop of one never started ends nothing. The cache's installer of
	// what is published runs under the host's own Start.
	var compactions gitletcompact.Estate = seam
	if e.compactorEstate != nil {
		compactions = e.compactorEstate(compactions)
	}
	compactor, err := gitletcompact.New(gitletcompact.Options{
		Cache: c, Objects: objects, Estate: compactions, Maintainers: e.router, NodeID: nodeID,
		Limits: e.codeLimits, Events: e.codeEvents(), Metrics: e.metrics,
	})
	if err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return gitletHostParts{}, fmt.Errorf("engine: build the compactor: %w", err)
	}

	// 10. THE HOME'S CHANGE WORK (internal/gitlet/home), on a repository's
	// HOME Gitlet node: each live change's facts and merge recorded or the
	// change landed, its head's checks queued from its target's checks file,
	// and its merge queue's trains built (on h), tested and landed, each
	// landed change's work item closed as its enqueuer (codequeue.go). It asks
	// through the router's code seam, nudged by a watch on this node's code log
	// (positions as this node's applier numbers it), merging in a scratch
	// beside the cache in the pool's maintenance lane. BUILT and STARTED as the
	// compactor is, for its reasons. Stopped before the compactor in
	// [native.stopGitletHost]: its facts reads are this node's git reads, and
	// its merges read the cache, both stopped after it.
	var (
		homeEstate gitlethome.Estate = seam
		chains     gitlethome.Chains = h
	)
	if e.homeSeams != nil {
		homeEstate, chains = e.homeSeams(homeEstate, chains)
	}
	homeLoop, err := gitlethome.New(gitlethome.Options{
		Estate: homeEstate, Placement: e.router, Nudges: codeNudges{log: codeLog.log, runner: codeLog.runner},
		Cache: c, Git: pool, Chains: chains, Tasks: e.queueTasks(), Share: share, NodeID: nodeID,
		Chart: e.codeChartEpoch, Metrics: e.metrics,
	})
	if err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return gitletHostParts{}, fmt.Errorf("engine: build the home's change work: %w", err)
	}

	// 10b. THE IMPORTS AND PULL MIRRORS (internal/gitlet/mirror), a worker of
	// the loop just built (codemirrors.go): a repository's home fetching it
	// from the host its rows name, landing chunks and syncs through its
	// committer. Started by the loop's turns, stopped after the loop.
	mirrors, err := e.buildMirrors(nodeID, boot, c, pool, objects, homeLoop)
	if err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return gitletHostParts{}, fmt.Errorf("engine: build the imports and mirrors: %w", err)
	}

	// 10c. THE ATLAS'S READ OF A REPOSITORY'S DEPLOY AND DEPENDENCY FILES
	// (internal/atlas/codescan), a worker of the same loop (atlascode.go).
	if err = e.buildAtlasCodeScan(homeLoop); err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return gitletHostParts{}, fmt.Errorf("engine: build the atlas's code scan: %w", err)
	}

	// 11. THE CHECKS (internal/gitlet/checkrun): this node's runner of queued
	// checks and its home's sweep of checks whose runner is gone, a worker of
	// the loop just built (codechecks.go), started with it. Stopped FIRST in
	// the one bounded stop, so each check records its ending and closes its box
	// while the router and Gitlet, which its boxes clone from, still serve.
	checks, err := e.buildChecks(nodeID, boot, homeLoop, codeLog)
	if err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return gitletHostParts{}, fmt.Errorf("engine: build the check runner: %w", err)
	}

	// 12. ITS STATUS, answered for the fleet (codestatus.go) under the host's
	// lifetime rather than this call's, withdrawn before it stops.
	withdraw, err := e.serveCodeStatus(context.WithoutCancel(ctx), nodeID, h)
	if err != nil {
		homeLoop.Stop(context.WithoutCancel(ctx))
		compactor.Stop(context.WithoutCancel(ctx))
		h.Stop(context.WithoutCancel(ctx))
		return gitletHostParts{}, fmt.Errorf("engine: serve Gitlet's status on %s: %w", topics.CodeStatus, err)
	}

	// 13. ITS SEARCHES, answered for the fleet (codesearch.go) through the git
	// reads above as the asker its signed request names, holding all but one
	// unit of their share; withdrawn with the status answerer.
	withdrawSearch, err := e.serveCodeSearch(context.WithoutCancel(ctx), nodeID, reads, share.Units())
	if err != nil {
		if werr := withdraw(context.WithoutCancel(ctx)); werr != nil {
			log.WarnContext(ctx, "code_status_answerer_not_withdrawn", "error", werr)
		}
		homeLoop.Stop(context.WithoutCancel(ctx))
		compactor.Stop(context.WithoutCancel(ctx))
		h.Stop(context.WithoutCancel(ctx))
		return gitletHostParts{}, fmt.Errorf("engine: serve code searches on %s: %w", topics.CodeSearch, err)
	}
	return gitletHostParts{host: h, reads: reads, previews: previews, cache: c, size: size, compactor: compactor,
		home: homeLoop, mirrors: mirrors, checks: checks, withdrawStatus: withdraw, withdrawSearch: withdrawSearch}, nil
}

// codeNudges is this node's code log as the home's change work watches it
// (gitlethome.Nudges): each record's sequence, and a gap's, a position in the
// generation this node's applier last committed in. The sequence is the
// broker's, before any applier applied it, so the home's next read waits for
// its copy to hold it. A REANCHOR moves the generation under a node until its
// applier follows: an old-generation position floors nothing in the new, at
// worst a nudge spent, which the backstop's next pass makes up.
type codeNudges struct {
	log    *jetstream.DomainLog
	runner *statelog.Runner
}

// Watch begins the watch, its handler's sequences turned into positions.
func (n codeNudges) Watch(ctx context.Context, filter string, h gitlethome.Nudged) (gitlethome.Watching, error) {
	at := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: topics.CodeLogStream, Generation: n.runner.Committed().Generation, Seq: seq}
	}
	w, err := n.log.Watch(ctx, filter, jetstream.WatchHandler{
		Subject: func(subject string, seq uint64) { h.Subject(subject, at(seq)) },
		Gap:     func(through uint64) { h.Gap(at(through)) },
	})
	if err != nil {
		// NO WATCH, never a nil one inside the interface, which a nil check
		// would take for a running one.
		return nil, err
	}
	return w, nil
}

// codeFleet is how many live nodes serve git and how many hold the estate,
// from the presence view the router's placements read ([presenceRoster]), so a
// cache's index backfill paces itself by the fleet the router asks. A node
// with no view answers none of either.
func (e *Engine) codeFleet() (int, int, error) {
	roster := presenceRoster{view: e.dataView}
	git, err := roster.LiveGitletNodes()
	if err != nil {
		return 0, 0, err
	}
	data, err := roster.LiveDataNodes()
	if err != nil {
		return 0, 0, err
	}
	return len(git), len(data), nil
}

// gitHalf is this node's git half as the estate's router and server ask for it
// ([estate.GitServer], through [engineGit]): reads by object id from the query
// server, engine writes from Gitlet's git surface, merge previews from the
// home's merge. [Engine.buildGitletHost] builds the three over one cache, pool
// and seat git-work share, and [native.stopGitletHost] stops them together.
type gitHalf struct {
	reads    *query.Server
	writes   *host.Server
	previews *gitlethome.Previews
}

// Read is the query server's read by object id.
func (g gitHalf) Read(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	return g.reads.Read(ctx, q)
}

// Write is Gitlet's engine write (ADR-0092).
func (g gitHalf) Write(ctx context.Context, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	return g.writes.Write(ctx, w)
}

// MergePreview is the home's merge of one commit into another, a comparison's
// preview.
func (g gitHalf) MergePreview(ctx context.Context, q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
	return g.previews.MergePreview(ctx, q)
}

// gitletHostParts is what [Engine.buildGitletHost] built. The cache is stopped
// by the host with itself; the engine holds it only to READ for what no request
// asks ([Engine.codeReading]). The compactor and the home's change work are
// built NOT started: [Engine.startNativeDuties] starts them past the runtime's
// publication and the mode's gate.
type gitletHostParts struct {
	host           *host.Server
	reads          *query.Server
	previews       *gitlethome.Previews
	cache          *cache.Cache
	size           codeCacheSize
	compactor      *gitletcompact.Compactor
	home           *gitlethome.Loop
	mirrors        *mirror.Mirrors
	checks         *checkrun.Runner
	withdrawStatus queue.Unsubscribe
	withdrawSearch queue.Unsubscribe
}

// codeReading fills the reading's code half at now: the queued checks' backlog,
// the committer's busy refusals (code_commit_busy), the cache's memory refusals
// (code_maintenance_refused), repositories at the pack bound
// (code_compaction_behind), the home's merge rate and failing mirrors
// (code_mirror_failing). Filled on EVERY node, since an alarm describes one; a
// half this node lacks stays zero, read as nothing wrong. The committer is read
// outside the cache's branch: every data node of a code-hosting company runs
// one, cache or not ([native.committer]). Its fields are assigned HERE because
// the producers gate follows a call that hands the reading on
// (TestEveryAlarmReadingFieldHasAProducer). Read from the PUBLISHED runtime each
// evaluation, never captured; after the host stops, the cache's last pass is
// the node's truth until it exits.
func (e *Engine) codeReading(ctx context.Context, now time.Time, out *statelog.Reading) {
	n := e.native.Load()
	if n == nil {
		return
	}
	if n.codeReader != nil {
		// THE QUEUED CHECKS' BACKLOG, from this node's rows whether or not any
		// Gitlet node runs checks: a fleet whose every runner is gone is the
		// backlog the alarm exists for.
		backlog, err := n.codeReader.ChecksBacklog(ctx, now)
		if err != nil {
			log.WarnContext(ctx, "code_checks_backlog_unread", "error", err.Error(),
				"detail", "this evaluation raises no code_checks_backlog; the next reads the rows again")
		}
		out.CodeChecksBacklog = backlog.Behind
		out.CodeChecksBacklogMore = backlog.More
		out.CodeChecksBacklogOldest = backlog.Oldest
		out.CodeChecksBacklogCheck = backlog.Check
	}
	if n.committer != nil {
		busy := n.committer.AlarmReading(now)
		out.CodeCommitBusy = busy.Refused
		out.CodeCommitBusyOfRepo = busy.OfRepo
		out.CodeCommitBusyNode = busy.Node
		out.CodeCommitBusyWaited = busy.Waited
		if busy.Repo != "" {
			// THE NAME AN OPERATOR KNOWS, read only while something was
			// refused (one point read an evaluation), else the id.
			out.CodeCommitBusyRepo = n.codeStore.NameOf(ctx, busy.Repo)
		}
	}
	if n.gitCache == nil {
		return
	}
	n.gitCache.AlarmReading(out)
	if n.compactor != nil {
		n.compactor.AlarmReading(out)
	}
	if n.home != nil {
		// THE MERGE RATE, from the queues this node is home to: only the
		// home builds a queue's trains, so only it times their rounds.
		n.home.AlarmReading(out)
	}
	if n.mirrors != nil {
		// THE MIRRORS THIS NODE IS HOME TO, from its own memory of their
		// attempts: a push to another host is no record, so only the home
		// that pushes knows whether it can (M6-D16).
		n.mirrors.AlarmReading(out)
	}
}

// gitBackground is what this node advertises its git cache gives its
// background work, as the seat host writes it on every beat
// ([coord.GitBackgroundKey]): [Engine.gitBackgroundBudget] on a node with the
// gitlet role, and nil on any other, whose presence row then carries no such key
// — it serves no git, so no Gitlet node weighs it.
func (e *Engine) gitBackground() func() int64 {
	if !e.profile.Roles.Has(placement.RoleGitlet) {
		return nil
	}
	return e.gitBackgroundBudget
}

// gitBackgroundBudget is what this node's git cache gives its background work
// now ([cache.Cache.BackgroundBudget], the figure its admissions hold every run
// to), which a Gitlet node advertises every beat ([Engine.gitBackground]) so a
// repository's maintenance is placed where admissions would hold it. Zero
// where this node holds no cache: nothing to give. Not the DISK budget
// ([Engine.codeCacheBudget]).
//
// Read from the PUBLISHED runtime each beat, never captured: Gitlet is built at
// boot or the first apply hosting code, after the heartbeat starts. A load and
// a locked read in memory, so the beat can take it unbounded.
func (e *Engine) gitBackgroundBudget() int64 {
	n := e.native.Load()
	if n == nil || n.gitCache == nil {
		return 0
	}
	return n.gitCache.BackgroundBudget()
}

// cacheVolume is what sizing the code cache reads of this node's disk: the
// free space on a directory's volume, whether two directories draw on one pool
// of space, and the embedded broker's claim on the volume it stores its
// streams on — through the queue where it is the JetStream one, and none where
// it keeps no streams on this node's disk. [Options.cacheDisk] in its place
// where a case set one.
func (e *Engine) cacheVolume() cacheVolume {
	if e.cacheDisk != nil {
		return *e.cacheDisk
	}
	return cacheVolume{free: volumeFree, same: sameVolume, broker: func() (jetstream.VolumeClaim, bool, error) {
		if b, ok := e.backends.Queue.(brokerVolume); ok {
			return b.VolumeClaim()
		}
		return jetstream.VolumeClaim{}, false, nil
	}}
}

// brokerVolume is the embedded broker's claim on its store directory's volume,
// as sizing a neighbour of it needs it — declared here, by its caller, and
// satisfied by the JetStream queue ([jetstream.Queue.VolumeClaim]).
type brokerVolume interface {
	VolumeClaim() (jetstream.VolumeClaim, bool, error)
}

// cacheVolume is [Engine.cacheVolume]'s three readings, as seams a test
// replaces.
type cacheVolume struct {
	free   func(dir string) (int64, error)
	same   func(a, b string) bool
	broker func() (jetstream.VolumeClaim, bool, error)
}

// codeCacheSize is the budget a cache is held to, where it came from, and
// every reading it was decided from — the fields code_cache_sized logs.
type codeCacheSize struct {
	budget  int64
	derived bool

	// room is the space the cache can use: free plus held, less broker,
	// spools and patches. What a derived budget is a share of, and what an
	// explicit one is compared with.
	room int64

	free int64
	held int64

	// broker is what the embedded broker may still write to the cache's
	// volume — its store limit less what it stores — where it shares the
	// volume, and 0 where it does not. brokerLimit is the limit it is held
	// to there ([jetstream.VolumeClaim.Limit], -1 for none), which a refusal
	// names.
	broker      int64
	brokerLimit int64

	// spools is Gitlet's request-spool allowance, which lives on the
	// cache's volume and never in its budget: [host.SpoolAllowance] of the
	// pool's transfer lane, which gitlet.max_transfers sizes.
	spools    int64
	transfers int

	// patches is what the revision facts' patch spools may hold on the
	// cache's volume, outside its budget too: [query.PatchSpoolAllowance],
	// a fixed count of patches at their bound, which no setting moves.
	patches int64

	dedicated bool
}

// cacheBudget is the budget as [config.NodeTierWarnings] compares it with the
// company's repository limit.
func (s codeCacheSize) cacheBudget() config.CacheBudget {
	return config.CacheBudget{Bytes: s.budget, Derived: s.derived}
}

// log says how the cache was sized, at code_cache_sized: at INFO where the
// budget fits the room, and at WARN with what to change where it does not — a
// derived share under [config.GitletCacheMaxBytesFloor], the smallest budget a
// node is ever given explicitly, so that nearly every repository is a cold sync
// at nearly every fetch; or an explicit budget the room cannot hold beside the
// volume's other tenants, which the cache fills before it evicts anything.
func (s codeCacheSize) log(ctx context.Context, dir string) {
	attrs := []any{"dir", dir, "bytes", s.budget, "derived", s.derived, "room", s.room,
		"free", s.free, "held", s.held, "broker", s.broker, "spools", s.spools,
		"transfers", s.transfers, "patches", s.patches, "dedicated", s.dedicated}
	switch {
	case s.derived && s.budget < config.GitletCacheMaxBytesFloor:
		log.WarnContext(ctx, "code_cache_sized", append(attrs, "detail",
			fmt.Sprintf("the room on the cache's volume derives a budget under %d bytes, so a "+
				"repository is downloaded again at nearly every fetch: %s",
				config.GitletCacheMaxBytesFloor, s.remedies()))...)
	case !s.derived && s.budget > s.room:
		log.WarnContext(ctx, "code_cache_sized", append(attrs, "detail",
			"gitlet.cache_max_bytes is more than the cache's volume has room for beside what "+
				"is already there, the broker's limit, the request spools and the revision "+
				"facts' patch spools, so the cache fills the volume before it evicts "+
				"anything: lower gitlet.cache_max_bytes, or "+
				"give gitlet.cache_dir a volume of its own")...)
	default:
		log.InfoContext(ctx, "code_cache_sized", attrs...)
	}
}

// codeCacheBudget sizes the cache prepared at layout: gitlet.cache_max_bytes
// where set, else a share ([config.Gitlet.CacheBudget]) of the ROOM: free space
// PLUS layout.Held (free space excludes the cache, so a warm node's budget would
// shrink each restart) LESS what a broker on the volume may still write (an
// unset stream.store_max_bytes derives from the same free space), the request
// spools of transfers slots ([host.SpoolAllowance]) and the patch spools
// ([query.PatchSpoolAllowance]). DEDICATED where no tenant shares the volume
// ([sameVolume]; one that cannot be compared shares). An unreadable broker
// claim fails the build: that broker can serve nothing, and a guessed budget is
// what this exists not to make. So does a derived room of nothing, an
// overcommitted volume only the operator can resolve ([codeCacheSize.noRoom]);
// a small share is only warned of, and an explicit budget never refused.
func codeCacheBudget(boot *config.Bootstrap, layout cache.Layout, transfers int, v cacheVolume) (codeCacheSize, error) {
	f, err := v.free(layout.Dir)
	if err != nil {
		return codeCacheSize{}, fmt.Errorf("engine: size the code cache at %s: %w", layout.Dir, err)
	}
	size := codeCacheSize{free: f, held: layout.Held, spools: host.SpoolAllowance(transfers),
		transfers: transfers, patches: query.PatchSpoolAllowance, dedicated: true}
	for _, tenant := range cacheVolumeTenants(boot) {
		if v.same(layout.Dir, tenant) {
			size.dedicated = false
			break
		}
	}
	claim, stores, err := v.broker()
	if err != nil {
		return codeCacheSize{}, fmt.Errorf("engine: size the code cache at %s: read what the "+
			"embedded broker may still store on its volume: %w", layout.Dir, err)
	}
	if stores && v.same(layout.Dir, claim.Dir) {
		size.dedicated = false
		size.broker, size.brokerLimit = claim.Unclaimed(), claim.Limit
		if size.broker < 0 {
			// A SERVER HOLDING TO NO LIMIT may take every free byte.
			size.broker = f
		}
	}
	size.room = f + layout.Held - size.broker - size.spools - size.patches
	size.budget, size.derived = boot.Gitlet.CacheBudget(size.room, size.dedicated)
	if size.derived && size.budget <= 0 {
		return codeCacheSize{}, size.noRoom(layout.Dir, boot.Stream.StoreMaxBytes)
	}
	return size, nil
}

// noRoom is the refusal of a derived budget of nothing: every figure the room
// was made of, and each setting that changes one — a volume of the cache's own
// (whose room is its free space less the spools), an explicit budget, the
// broker's limit where a broker shares the volume, and the transfer lane that
// sizes the spools. declared is stream.store_max_bytes as written, 0 where the
// broker derived its own limit.
func (s codeCacheSize) noRoom(dir string, declared int64) error {
	tenants := fmt.Sprintf("its %d transfers' request spools may hold %d bytes and its revision facts' "+
		"patch spools %d", s.transfers, s.spools, s.patches)
	if s.broker > 0 {
		var limit string
		switch {
		case s.brokerLimit < 0:
			limit = "no limit at all, so every free byte"
		case declared > 0:
			limit = fmt.Sprintf("stream.store_max_bytes, %d", declared)
		default:
			limit = fmt.Sprintf("its own limit of %d, derived from the volume's free space "+
				"when it started since stream.store_max_bytes is unset", s.brokerLimit)
		}
		tenants = fmt.Sprintf("the embedded broker storing its streams there may still write "+
			"%d bytes under %s, and %s", s.broker, limit, tenants)
	}
	remedies := s.remedies()
	return fmt.Errorf("engine: gitlet.cache_dir %s has no room for the code cache: its volume has "+
		"%d bytes free and the cache holds %d, %s, which leaves a room of %d bytes and no "+
		"share of it to derive a budget from. %s%s", dir, s.free, s.held, tenants, s.room,
		strings.ToUpper(remedies[:1]), remedies[1:])
}

// remedies is every setting that gives the cache more room, any one of them
// enough — what a refusal of no room and a warning of little both end with:
// a volume of the cache's own (a larger one where it already has one), an
// explicit budget, the broker's limit where a broker shares the volume, and
// the transfer lane, which sizes the spools.
func (s codeCacheSize) remedies() string {
	out := "give gitlet.cache_dir a volume of its own — its own filesystem with its own " +
		"space — or set gitlet.cache_max_bytes to the budget the cache may fill"
	if s.dedicated {
		out = "give gitlet.cache_dir a larger volume, or set gitlet.cache_max_bytes to the " +
			"budget the cache may fill"
	}
	if s.broker > 0 {
		out += ", or set stream.store_max_bytes lower, since the broker beside the cache " +
			"on this volume may fill what it allows"
	}
	return out + fmt.Sprintf(", or lower gitlet.max_transfers, whose every transfer may spool "+
		"%d MiB of request on this volume", host.SpoolPerTransfer>>20)
}

// cacheVolumeTenants is every directory of this node's other disk the cache's
// volume may share, beside the broker's store directory, which its claim
// names ([codeCacheBudget]): the store's, the replicated estate's where it
// lives apart, and the snapshots'.
func cacheVolumeTenants(boot *config.Bootstrap) []string {
	dirs := []string{filepath.Dir(boot.Store.Path), boot.Store.SnapshotDirFor()}
	if p := strings.TrimSpace(boot.Store.ReplicatedPath); p != "" {
		dirs = append(dirs, filepath.Dir(p))
	}
	return dirs
}

// stopGitletHost ends Gitlet on this node: it refuses new git requests, cancels
// those in flight (a receive-pack through its context, its hook's connection
// with it), closes the hook relay and stops the cache. Nil-safe.
//
// BEFORE THE COMMITTER ([Engine.stopCommitter]) AND THE ADMISSION'S WITHDRAWAL:
// a push proposes through the estate and a sync's read appends a barrier to the
// code log, which after the withdrawal is a write a capacity coordinator was
// told nobody makes; a cancelled push a committer had appended lands or not by
// the log. AND BEFORE THE USAGE PUBLISHER'S LAST FLUSH ([Engine.stopUsage]):
// every `visibility: unit` read answered until the cache stops is a record
// ([types.CodeFetched]) that flush must carry. The API's listener has closed
// first (cmd/crewlet's shutdown order).
func (e *Engine) stopGitletHost(ctx context.Context) { e.native.Load().stopGitletHost(ctx) }

// stopGitletHost is [Engine.stopGitletHost] on this runtime, and the stop its
// own failure path takes ([native.shutdown]): ONCE, bounded once by
// [host.StopBudget] ([boundedStop]): both ask, a second [host.Server.Stop]
// would spend a second budget, and a cut-short wait keeps the cache's claim
// until the last work ends, so waiting again changes nothing. Each part stops
// before what it reads: the CHECK RUNNER (each check records its ending,
// `errored runner_lost` to be queued again, and closes its box while the
// router and Gitlet serve); the HOME'S CHANGE WORK, MIRRORS and COMPACTOR; the
// STATUS and SEARCH ANSWERERS (no peer is told of a going cache); the GIT READS
// and MERGE PREVIEWS, uncounted by the host, so one in flight is cut (answered
// unready, its walk moving on) before the usage day's last flush; then the
// host, whose stop cuts engine writes in flight alike.
func (n *native) stopGitletHost(ctx context.Context) {
	if n == nil || n.gitletHost == nil {
		return
	}
	n.gitletHostStop.stop(ctx, func(ctx context.Context) {
		if n.checkRunner != nil {
			n.checkRunner.Stop(ctx)
		}
		if n.home != nil {
			n.home.Stop(ctx)
		}
		if n.mirrors != nil {
			n.mirrors.Stop(ctx)
		}
		if n.compactor != nil {
			n.compactor.Stop(ctx)
		}
		if n.withdrawCodeStatus != nil {
			if err := n.withdrawCodeStatus(ctx); err != nil {
				log.WarnContext(ctx, "code_status_answerer_not_withdrawn", "error", err)
			}
		}
		if n.withdrawCodeSearch != nil {
			if err := n.withdrawCodeSearch(ctx); err != nil {
				log.WarnContext(ctx, "code_search_answerer_not_withdrawn", "error", err)
			}
		}
		if n.mergePreviews != nil {
			n.mergePreviews.Stop(ctx)
		}
		if n.gitReads != nil {
			n.gitReads.Stop(ctx)
		}
		n.gitletHost.Stop(ctx)
	})
}

// boundedStop runs a stop ONCE, its wait bounded by budget, however many
// callers ask for it: every caller returns once that one stop has, and none
// waits a second budget. Its zero value bounds by [host.StopBudget].
type boundedStop struct {
	once   sync.Once
	budget time.Duration
}

// stop runs stop under the bound, the first time it is asked; a later call
// waits for that one to return and runs nothing. The bound is detached from
// ctx's cancellation for [Engine.stopCommitter]'s reason: the stop is often
// asked for because ctx ended.
func (b *boundedStop) stop(ctx context.Context, stop func(context.Context)) {
	b.once.Do(func() {
		budget := b.budget
		if budget <= 0 {
			budget = host.StopBudget
		}
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
		defer cancel()
		stop(bounded)
	})
}
