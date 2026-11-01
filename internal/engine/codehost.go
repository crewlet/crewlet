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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/checkrun"
	codecompact "github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	codehome "github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/code/mirror"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/code/readaudit"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracing"
)

// THE CODE HOST ON A GIT NODE: the cache of the company's repositories this
// node serves from, the git pool every process runs in, and the host — smart
// HTTP for persons — built over them, on a node with the git role whose company
// sets code.native. The committer and the lifecycle store are every data
// node's (code.go); the host is only a git node's, and it reaches the code
// log through the estate's router like every other surface, so the node it
// asks is whichever serves the operation.

// Chart is the CURRENT epoch's organization, nil before a company: a FUNCTION
// a surface reads per call rather than a value it captures, because an apply
// replaces the organization wholesale — the scheduler's org reads it so. Who a
// person pushing to the code host is comes from the guard's own principal
// (the identity directory's binding), never from this.
func (e *Engine) Chart() *org.Organization {
	if c := e.Company(); c != nil {
		return c.Org
	}
	return nil
}

// CodeHost is this node's code host: present where this node has the git role
// and the runtime it runs was built for a company that sets code.native. Asked
// per request by whoever serves its routes, never captured: the runtime may be
// built at an apply after the routes are mounted (a company applied after
// boot), and it is the published runtime that says.
func (e *Engine) CodeHost() (*host.Server, bool) {
	n := e.native.Load()
	if n == nil || n.codeHost == nil {
		return nil, false
	}
	return n.codeHost, true
}

// Why this node serves none of the code host now ([Engine.CodeUnserved]).
var (
	// ErrCodeNotHosted: the company this node serves does not set
	// code.native, and nor does the configuration the fleet activated, as
	// this node last read it.
	ErrCodeNotHosted = errors.New("engine: this company does not host code (code.native is not set)")
	// ErrCodeApplying: this node is behind the configuration the fleet
	// activated and is applying it — a first apply building the native
	// runtime among them. Clears in seconds, by itself.
	ErrCodeApplying = errors.New("engine: this node is applying the configuration the fleet activated")
	// ErrCodeApplyFailed: the configuration the fleet activated sets
	// code.native, as this node read it, and this node's apply of it
	// failed (config_apply_failed): a code host that could not be built, a
	// revision this node refused. Clears only when an apply succeeds, which
	// a corrected revision's activation is what usually brings.
	ErrCodeApplyFailed = errors.New("engine: this node could not apply the configuration the fleet " +
		"activated, which hosts code (config_apply_failed)")
	// ErrCodeUnknown: this node's apply of the configuration the fleet
	// activated failed BEFORE it could read what that configuration says
	// (config_apply_failed): the revision could not be read from this
	// node's store, fetched from the coordination store, opened with this
	// node's key or parsed. So this node cannot say whether the company
	// hosts code at all, and says that — never [ErrCodeApplyFailed]'s
	// "which hosts code", which would tell a company that never set
	// code.native that it had. Clears when an attempt reads the revision:
	// the next tick's retry where the failure was a blip, a corrected
	// revision's activation where the revision itself is unreadable here.
	ErrCodeUnknown = errors.New("engine: this node could not read the configuration the fleet " +
		"activated, so cannot say whether this company hosts code (config_apply_failed)")
	// ErrCodeNeedsRestart: the company this node serves sets code.native,
	// and this node's native runtime was already running without the code
	// host when it was set; it serves it once restarted
	// (native_backend_needs_restart).
	ErrCodeNeedsRestart = errors.New("engine: this node's native runtime was started without the code " +
		"host, and serves it once restarted (native_backend_needs_restart)")
)

// CodeUnserved says why this node serves none of the code host — neither
// [Engine.CodeHost] nor [OperatorCode] — as one of five sentinels, which the
// API answers each in its own way: not hosted is a 404, the other four a
// 503, and only an apply in flight clears by waiting.
//
// DECIDED FROM WHAT THE FLEET ACTIVATED, as this node's reconciler last read
// it, and never from this node's published epoch alone: an apply publishes
// its epoch only once the runtime it needs is built, so a node building its
// first company's code host, or one whose apply of it failed, still serves
// the previous epoch — and that epoch says the company hosts no code, which
// is exactly what a person who just turned it on must not be told. Between an
// activation and the tick that reads it — milliseconds on the nudge the
// activation broadcasts, a reconcile interval at most — a node answers by the
// activation it last read.
//
// WHAT THIS NODE HAS NOT READ IT DOES NOT CLAIM. A failed apply is
// [ErrCodeApplyFailed] — "which hosts code" — only where the attempt decoded
// the revision and found code.native set; one that failed before it could
// decode it is [ErrCodeUnknown], since a revision that could not be read here
// may host no code at all. An apply in flight, decoded or not, is
// [ErrCodeApplying], whose sentence says only that this node is applying.
//
// Asked only where the code host was found absent, and answering as of now:
// a caller that found it absent asks again after this answer, since the
// runtime may have come up between the two (api.EngineCode).
func (e *Engine) CodeUnserved() error {
	if c := e.Company(); c != nil && c.Config != nil && c.Config.Code.Native {
		// The epoch this node serves hosts code, so the runtime that serves
		// none of it was built before code.native was set: an apply starts a
		// runtime with the code host before it publishes the epoch.
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

// auditGit records one push onto this node's own queue, through the operator
// surface's audit — one construction of the record's envelope, whose source is
// the operator's, and a publish that outlives the request.
func (e *Engine) auditGit(ctx context.Context, r types.OperatorActed) {
	operator.Audit(ctx, e.backends.Queue, types.NewOperatorActed(r))
}

// recordCodeRead files one read of a `visibility: unit` repository — a fetch
// the code host served, a read the git reads answered — in this node's reads
// ledger ([Engine.codeReads], internal/code/readaudit), stamped with the
// instant it was answered, the company day that falls in on the CURRENT
// epoch's clock ([Engine.Zone]) and the trace it was served under, and
// returns. The ledger publishes every reader's reads of a flush as one
// `code_fetched` event, which the event store writes into this node's log —
// every git node holds data — and from which the usage publisher derives who
// read the repository (internal/usage, kind repo_read).
//
// NEVER A PUBLISH OF ITS OWN, and that is the point: a read's answer — a
// fetch's response, a server-side read's slot on the git lane, and every read
// waiting on the page it cuts — waited for the broker's replicated, fsynced
// acknowledgement and the event store's commit, up to the client's five
// seconds while the event stream had no leader, and lost its record for good
// where the broker refused it. Filed, it costs the read a fold under a lock;
// the ledger's flush publishes off every read's path, keeps a refused record
// for the next flush, and outlives the request, so a client that hung up once
// it had its pack is still recorded.
//
// NEVER THROUGH THE OPERATOR AUDIT ([Engine.auditGit]): that record's source is
// the runtime audit's, whose types the dashboard's audit log reads as people's
// calls, and a read is neither a call nor necessarily a person's.
func (e *Engine) recordCodeRead(ctx context.Context, read types.CodeFetched) {
	at := time.Now().UTC()
	e.codeReads.Add(readaudit.Read{Fetched: read, At: at,
		Day: period.At(period.Day, at, e.Zone()).Label, Trace: tracing.TraceOf(ctx)})
}

// roleNeeded is [seat.Config.RoleNeeded]: the CURRENT epoch's company's
// answer ([config.Company.NeedsRole] — git exactly where it sets code.native,
// every other role always), and no company's before one is applied. An atomic
// load of the epoch — the sweep's must-not-block rule.
func (e *Engine) roleNeeded(role placement.NodeRole) bool {
	var cfg *config.Company
	if c := e.Company(); c != nil {
		cfg = c.Config
	}
	return cfg.NeedsRole(role)
}

// hostsGit reports whether a runtime built with built serves git on this node:
// the company hosts code and this node has the git role. A git role on a
// company that hosts none serves nothing, and code.native on a node without
// the role is served by the fleet's git nodes.
func (e *Engine) hostsGit(built builtWith) bool {
	return built.runsCode() && e.profile.Roles.Has(placement.RoleGit)
}

// gitHook is the argv the cache's proc-receive hook execs: [Options.GitHook]
// where it is given, and otherwise THIS binary as host.HookCommand names it —
// `crewlet code hook proc-receive` — with its symbolic links resolved, since
// git runs the hook with the pool's PATH and a link replaced by an upgrade
// would start a different binary mid-push.
func (e *Engine) gitHook() ([]string, error) {
	if len(e.gitHookArgv) > 0 {
		return e.gitHookArgv, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("engine: the code host's hook is this binary, and os.Executable "+
			"could not name it: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, fmt.Errorf("engine: the code host's hook is this binary, and its path "+
			"could not be resolved: %w", err)
	}
	return host.HookCommand(exe), nil
}

// buildCodeHost builds this node's code host for the runtime being started —
// the cache's directory, the git pool, the cache's budget, the cache, the git
// reads and the host, in the order each needs the last — starts it, and makes
// this node an answerer of the host's status (codestatus.go), answering the
// host, the git reads, the cache they serve from, the budget it sized that
// cache to and what withdraws the status answerer. Called by
// startNative only where [Engine.hostsGit]. An error fails the boot, or the
// apply that reached [Engine.startNativeFor], which is retried like every
// start of the runtime: a company that asked for native code on a node that
// serves git cannot be served by it.
//
// A FAILURE GIVES THE DIRECTORY BACK. Prepare claims the cache's directory for
// this process, so a retried apply in the same process would otherwise find it
// held by its own failed attempt: before Open the layout is released, after it
// the cache is stopped.
//
// nodeID is the core runtime's resolved id ([core.nodeID]), which the native
// half no longer holds a copy of: every proposal the host makes names it as the
// node that received the push ([host.Options.NodeID]). codeLog is this node's
// own code log, whose records the home's nudges watch.
func (e *Engine) buildCodeHost(ctx context.Context, nodeID string, boot *config.Bootstrap,
	codeLog *runningLog) (codeHostParts, error) {

	// 1. THE DIRECTORY, prepared BEFORE git's HOME exists, since it names it.
	// Absolute: a store path written relative resolves against the working
	// directory the store itself is opened from.
	dir, err := filepath.Abs(boot.Git.CacheDirFor(boot.Store.Path))
	if err != nil {
		return codeHostParts{}, fmt.Errorf("engine: git.cache_dir: %w", err)
	}
	layout, err := cache.Prepare(ctx, dir)
	if err != nil {
		return codeHostParts{}, fmt.Errorf("engine: git.cache_dir %s: %w", dir, err)
	}
	defer layout.Release()

	// 2. GIT, the node's own, its lanes resolved once by the pool — which
	// logs what it found (code_git_ready) — sized by the memory it reads of
	// the node, [Options.gitMemory] in its place in this package's tests.
	pool, err := gitproc.New(ctx, gitproc.Options{
		MaxProcesses: boot.Git.MaxProcesses, MaxTransfers: boot.Git.MaxTransfers,
		Memory: e.gitMemory, Home: layout.Home, Metrics: e.metrics,
	})
	if err != nil {
		return codeHostParts{}, fmt.Errorf("engine: this node has the %q role and the "+
			"company sets code.native, so it serves git: %w", placement.RoleGit, err)
	}

	// 3. THE BUDGET, from the room the cache can use — after the pool, whose
	// transfer lane sizes the request spools that share the cache's volume
	// outside its budget.
	size, err := codeCacheBudget(boot, layout, pool.Slots(gitproc.ClassTransfer), e.cacheVolume())
	if err != nil {
		return codeHostParts{}, err
	}
	size.log(ctx, layout.Dir)

	// 4. THE OBJECTS a push's pack bundle is stored in and a sync reads.
	objects := e.Objects()
	if objects == nil {
		return codeHostParts{}, fmt.Errorf("engine: code.native needs the object store " +
			"(store.objects): this node has none")
	}

	// 5. THE HOOK every push's receive-pack execs.
	hook, err := e.gitHook()
	if err != nil {
		return codeHostParts{}, err
	}

	// 6. THE CACHE, which takes over the layout's claim — its rows read
	// through the router, and its indexes' backfill paced by the fleet's
	// live git and data nodes as the router's placements count them. Its
	// code indexes are kept for the repositories this node is among the
	// first code.IndexHolders of, in the router's git order (M6-D12).
	seam := e.router.Code()
	c, err := cache.Open(ctx, cache.Options{
		Layout: layout, MaxBytes: size.budget, Git: pool, Objects: objects, Fetch: seam, Fleet: e.codeFleet,
		Placement: &cache.Placement{Node: nodeID, Order: e.router.GitOrder},
		Limits:    e.codeLimits, HookCommand: hook, Metrics: e.metrics, Now: time.Now,
	})
	if err != nil {
		return codeHostParts{}, fmt.Errorf("engine: open the code cache at %s: %w", layout.Dir, err)
	}

	// 7. THE GIT READS, the server-side reads of a repository by object id a
	// seat asks of this node as a repository's home (ADR-0059): over the
	// cache and the pool, under the node's ONE seat git-work share of the
	// pool's read lane — the engine writes' builds draw on the same (step 8),
	// never a second share, which would let seats' reads and builds between
	// them hold the whole lane — judging a seat's grant by the current
	// epoch's chart and refusing one while this node is behind the company's
	// configuration — both read per read, never captured. It holds no
	// goroutine of its own, so nothing stops it but the cache's stop.
	// THE RE-SYNC BUCKETS beside the share, ONE set a node for the same
	// reason, which the query server and the code host both draw on: a
	// seat's git read's second look at the log's head and an engine write's
	// re-judgement there each pay its turn's bucket on its seat, a coding
	// box's re-syncs its run's on the same seat, and a check's box its
	// attempt's whether its transfer or its /box/code/ read asks
	// (code.NewResyncs). One set, because the census gives a seat one term on
	// the log: a seat cannot spend it through one server, or through its
	// boxes, while another surface's buckets stay full.
	share := code.NewGitShare(nodeID, pool.Slots(gitproc.ClassRead), e.metrics)
	resyncs := code.NewResyncs(time.Now, e.metrics)
	reads, err := query.New(query.Options{
		NodeID: nodeID, Cache: c, Git: pool, Share: share, Resyncs: resyncs,
		Roster: e.codeRoster, Current: e.admits, Fetched: e.recordCodeRead,
	})
	if err != nil {
		c.Stop(context.WithoutCancel(ctx))
		return codeHostParts{}, fmt.Errorf("engine: build the git reads: %w", err)
	}
	// AND ITS MERGE PREVIEWS beside them (internal/code/home's Previews): a
	// comparison's preview of merging one commit into another, answered by
	// the merge a repository's home records a change's mergeability by — a
	// principal's read, judged, shared and recorded as the git reads are,
	// over the same cache, pool and share, its merge written into a scratch
	// of its own. No goroutine of its own either.
	previews, err := codehome.NewPreviews(codehome.PreviewOptions{
		NodeID: nodeID, Cache: c, Git: pool, Share: share,
		Roster: e.codeRoster, Current: e.admits, Fetched: e.recordCodeRead, Metrics: e.metrics,
	})
	if err != nil {
		c.Stop(context.WithoutCancel(ctx))
		return codeHostParts{}, fmt.Errorf("engine: build the merge previews: %w", err)
	}

	// 8. THE HOST, over the engine's own seams: the router for the rows and
	// the committer, the operator audit for every push, and the current
	// epoch's limits and retention. Who is pushing is the API guard's answer
	// on each request, the seat the identity directory binds its credential
	// to included. An ENGINE WRITE (ADR-0062) is judged by the current
	// epoch's chart and refused a seat while this node is behind the
	// company's configuration, as a read is; signed from the current epoch
	// (codeSigner); and built under the git reads' own share. A CODING BOX
	// is admitted by its run's token, verified under the fleet's keyring
	// (boxTokens), and by its run's row on the fleet's coordination store,
	// which every node reads whichever node launched the run (codeBoxRuns) —
	// its push refused while a person's pause has stopped its seat
	// (stoppedBy, which the run's bridge session refuses its bridged writes
	// by too).
	// A CHECK'S BOX by its check's token (checkTokens) and its attempt's row,
	// which the host reads through the router's code seam like every row.
	// And the deployment's address a person reaches it at (externalBase),
	// on which the line git prints for a push's change links that change's
	// page: this node serves git on its API listener, which
	// `api.external_url` is required beside.
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
		return codeHostParts{}, fmt.Errorf("engine: build the code host: %w", err)
	}
	// ITS OWN LIFETIME from here: the relay and the cache's background work
	// run until the host is stopped, however long this call's caller lives.
	if err := h.Start(ctx); err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return codeHostParts{}, fmt.Errorf("engine: start the code host: %w", err)
	}

	// 9. THE COMPACTOR, a repository's MAINTAINER's three loops — the merge of
	// its packs (ADR-0060), the publication of its layers (ADR-0061) and the
	// expunge a person's purge asks for — over the cache and the store the host
	// uses, listing and proposing through the router's code seam — whose
	// Compact raises this node's floors past what it landed — and asking the
	// router itself who maintains each repository. BUILT here, after the host
	// whose cache it builds in, and STARTED only with the native half's duties
	// ([Engine.startNativeDuties]), once the runtime this half joins is
	// published and only where the node's mode publishes: its loops pass at
	// once once this node is listed, and everything a pass reads goes
	// through that runtime — this node's own copy answers its due listings
	// from it — so a pass started here found its listing unserved, and the
	// alarm a node that comes up beside a repository at the pack bound
	// raises waited a minute for the next pass; and here it started on a
	// node a capacity window restarted to write nothing, whose compactions
	// stored their outputs all the same. Stopped before the host, in the one
	// bounded stop ([native.stopCodeHost]); a stop of one never started ends
	// nothing. The cache's installer of what is published runs under the
	// host's own Start.
	var compactions codecompact.Estate = seam
	if e.compactorEstate != nil {
		compactions = e.compactorEstate(compactions)
	}
	compactor, err := codecompact.New(codecompact.Options{
		Cache: c, Objects: objects, Estate: compactions, Maintainers: e.router, NodeID: nodeID,
		Limits: e.codeLimits, Metrics: e.metrics,
	})
	if err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return codeHostParts{}, fmt.Errorf("engine: build the compactor: %w", err)
	}

	// 10. THE HOME'S CHANGE WORK (internal/code/home): every live change's
	// facts read and recorded, its merge into its target recorded, or the
	// change landed, and its head's checks queued from its target's checks
	// file, and its merge queue's trains built (on the host itself, h),
	// tested and landed — each landed change's work item closed as its
	// enqueuer (codequeue.go) — by the repository's HOME git node — listing
	// and asking through the router's code seam, whose writes raise this
	// node's floors past what they landed, asking the router who is home,
	// nudged by a watch
	// on this node's own code log, each record's position read as this node's
	// applier numbers the log, and merging over this node's cache in a scratch
	// beside it, in the pool's maintenance lane. BUILT here beside the
	// compactor and STARTED with it, by the native half's duties
	// ([Engine.startNativeDuties]), for the compactor's two reasons: its first
	// pass reads through the published runtime, and it asks records of the
	// repositories' writers, which a node restarted to write nothing must not.
	// Stopped BEFORE the compactor, first in the one bounded stop
	// ([native.stopCodeHost]): its facts reads are answered by this node's git
	// reads, and its merges read the cache, which that stop ends after it.
	var (
		homeEstate codehome.Estate = seam
		chains     codehome.Chains = h
	)
	if e.homeSeams != nil {
		homeEstate, chains = e.homeSeams(homeEstate, chains)
	}
	homeLoop, err := codehome.New(codehome.Options{
		Estate: homeEstate, Placement: e.router, Nudges: codeNudges{log: codeLog.log, runner: codeLog.runner},
		Cache: c, Git: pool, Chains: chains, Tasks: e.queueTasks(), Share: share, NodeID: nodeID, Metrics: e.metrics,
	})
	if err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return codeHostParts{}, fmt.Errorf("engine: build the home's change work: %w", err)
	}

	// 10b. THE IMPORTS AND PULL MIRRORS (internal/code/mirror), a worker of
	// the loop just built (codemirrors.go): a repository's home fetching it
	// from the host its rows name, its chunks and syncs landed through its
	// committer. Started by the loop's turns, stopped after the loop in the
	// one bounded stop.
	mirrors, err := e.buildMirrors(nodeID, boot, c, pool, objects, homeLoop)
	if err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return codeHostParts{}, fmt.Errorf("engine: build the imports and mirrors: %w", err)
	}

	// 11. THE CHECKS (internal/code/checkrun): this node's runner of the
	// company's queued checks, and its home's sweep of the checks whose
	// runner is gone, a worker of the loop just built (codechecks.go). BUILT
	// here and STARTED with the home's loop, by the native half's duties, for
	// its reasons; stopped FIRST in the one bounded stop, so each check it runs
	// records its ending and closes its box while the router and the code host
	// its boxes clone from still serve.
	checks, err := e.buildChecks(nodeID, boot, homeLoop, codeLog)
	if err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return codeHostParts{}, fmt.Errorf("engine: build the check runner: %w", err)
	}

	// 12. ITS STATUS, answered for the fleet (codestatus.go) from the moment
	// the host serves, and withdrawn before it stops ([native.stopCodeHost]):
	// under the host's lifetime rather than this call's, as the host runs.
	withdraw, err := e.serveCodeStatus(context.WithoutCancel(ctx), h)
	if err != nil {
		homeLoop.Stop(context.WithoutCancel(ctx))
		compactor.Stop(context.WithoutCancel(ctx))
		h.Stop(context.WithoutCancel(ctx))
		return codeHostParts{}, fmt.Errorf("engine: serve the code host's status on %s: %w", topics.CodeStatus, err)
	}

	// 13. ITS SEARCHES, answered for the fleet (codesearch.go) through the
	// git reads above, each as the asker its signed request names — and
	// withdrawn before the host stops, with the status answerer.
	withdrawSearch, err := e.serveCodeSearch(context.WithoutCancel(ctx), nodeID, reads)
	if err != nil {
		if werr := withdraw(context.WithoutCancel(ctx)); werr != nil {
			log.WarnContext(ctx, "code_status_answerer_not_withdrawn", "error", werr)
		}
		homeLoop.Stop(context.WithoutCancel(ctx))
		compactor.Stop(context.WithoutCancel(ctx))
		h.Stop(context.WithoutCancel(ctx))
		return codeHostParts{}, fmt.Errorf("engine: serve code searches on %s: %w", topics.CodeSearch, err)
	}
	return codeHostParts{host: h, reads: reads, previews: previews, cache: c, size: size, compactor: compactor,
		home: homeLoop, mirrors: mirrors, checks: checks, withdrawStatus: withdraw, withdrawSearch: withdrawSearch}, nil
}

// codeNudges is this node's own code log as the home's change work watches it
// (codehome.Nudges): the log's watch (jetstream.DomainLog.Watch), each
// record's sequence — and a gap's — a position on the code log in the
// generation this node's applier last committed in. A sequence the watch
// tells is the broker's, before any applier has applied it; read at the
// position, the home's next read of the rows waits for its copy to hold it.
// A REANCHOR, which an operator runs when the log is recreated, moves the
// generation under a node until its applier follows: a position of the old
// generation floors nothing in the new, and the home reads the rows as they
// stand — at worst a nudge spent, which the backstop's next pass makes up.
type codeNudges struct {
	log    *jetstream.DomainLog
	runner *statelog.Runner
}

// Watch begins the watch, its handler's sequences turned into positions.
func (n codeNudges) Watch(ctx context.Context, filter string, h codehome.Nudged) (codehome.Watching, error) {
	at := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: topics.CodeLogStream, Generation: n.runner.Committed().Generation, Seq: seq}
	}
	w, err := n.log.Watch(ctx, filter, jetstream.WatchHandler{
		Subject: func(subject string, seq uint64) { h.Subject(subject, at(seq)) },
		Gap:     func(through uint64) { h.Gap(at(through)) },
	})
	if err != nil {
		// NO WATCH, never a nil one inside the interface, which a caller
		// testing the watch against nil would take for a running one.
		return nil, err
	}
	return w, nil
}

// codeFleet is how many live nodes serve git and how many hold the estate,
// from the same watched presence view the router's two placements read
// ([presenceRoster]) — so the backfill of a git cache's indexes paces itself by
// the fleet the router asks, never another count of it. A node with no view
// has no fleet, and answers none of either.
func (e *Engine) codeFleet() (int, int, error) {
	roster := presenceRoster{view: e.dataView}
	git, err := roster.LiveGitNodes()
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
// ([estate.GitServer], through [engineGit]): a repository's reads by object id
// answered by the query server, its engine writes built and landed by the
// code host, and its merge previews answered by the home's merge — three
// servers [Engine.buildCodeHost] builds together, over one cache, one git pool
// and one seat git-work share, and stops together ([native.stopCodeHost]).
type gitHalf struct {
	reads    *query.Server
	writes   *host.Server
	previews *codehome.Previews
}

// Read is the query server's read by object id.
func (g gitHalf) Read(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	return g.reads.Read(ctx, q)
}

// Write is the code host's engine write (ADR-0062).
func (g gitHalf) Write(ctx context.Context, w code.EngineWrite) (code.EngineWriteResult, error) {
	return g.writes.Write(ctx, w)
}

// MergePreview is the home's merge of one commit into another, a comparison's
// preview.
func (g gitHalf) MergePreview(ctx context.Context, q code.MergePreviewQuery) (code.MergePreviewAnswer, error) {
	return g.previews.MergePreview(ctx, q)
}

// codeHostParts is what [Engine.buildCodeHost] built: the host, the git reads
// and the merge previews beside it over the same cache and pool, the cache
// they serve from — which the host stops with itself, and which the engine
// holds only to READ, for what no request asks (the alarm reading's code half,
// [Engine.codeReading]) — the budget it sized that cache to, the compactor
// that merges the packs of the repositories this node maintains, publishes
// their layers and expunges what a person's purge drops, and the home's change
// work, each built and NOT started (the native half's duties start the
// compactor's three loops and the home's, past the runtime's publication and
// the mode's gate: [Engine.startNativeDuties]), and what withdraws the host's
// status answerer ([Engine.serveCodeStatus]).
type codeHostParts struct {
	host           *host.Server
	reads          *query.Server
	previews       *codehome.Previews
	cache          *cache.Cache
	size           codeCacheSize
	compactor      *codecompact.Compactor
	home           *codehome.Loop
	mirrors        *mirror.Mirrors
	checks         *checkrun.Runner
	withdrawStatus queue.Unsubscribe
	withdrawSearch queue.Unsubscribe
}

// codeReading fills the reading's code half at now from this node's
// committer: how many proposals it refused busy for a backlog over the window
// code_commit_busy stands for, the repository with the most of them — named
// by this node's own rows, else by its id — how many met the node's whole
// bound, and how many waited in a queue until their asker could wait no
// longer ([code.Committer.AlarmReading]). And from this node's git cache: how
// many repositories it cannot write a bitmap of for memory — its own, or the
// one a publication of a repository it maintains writes — the largest of those
// charges and the budget they are past ([cache.Cache.AlarmReading]), which
// raise code_maintenance_refused — and from its compactor: how many
// repositories this node maintains sit at the pack bound, every push carrying
// objects into them refused until a compaction lands
// ([codecompact.Compactor.AlarmReading]), which raise code_compaction_behind on
// the node that compacts them.
//
// THE CACHE JUDGED, LOGGED, COUNTED AND GAUGED EVERY REFUSAL, AND NOTHING READ
// IT INTO A READING, so the alarm the cache's docs, the alarm reference and
// the concept page all promised could never fire on a running node: a node
// with a repository past its memory said so on a line and a gauge, and its
// alarm table — the one surface that reaches the log, the collector and the
// health envelope at once — stayed silent. Filled on EVERY node's evaluation,
// as every half is (an alarm describes one node), so a git node's refusals
// fire on that git node; a node without the git role, or one whose company
// hosts no code, holds no cache and leaves the fields zero, which the table
// reads as nothing wrong here.
//
// THE COMMITTER BESIDE THE CACHE, NEVER INSIDE ITS BRANCH: a committer runs on
// every data node of a company that hosts code ([native.committer]), the git
// role or not, and a data node without it holds no cache — so a reading that
// asked the committer only where a cache stood was silent on exactly the node
// whose queue was full. Its five fields are ASSIGNED HERE, from the concrete
// committer's reading, because the reading's producers gate follows a call
// that hands the reading on, never one whose result is the committer's own
// shape (TestEveryAlarmReadingFieldHasAProducer).
//
// Read from the PUBLISHED runtime at each evaluation, never captured, as every
// surface asks for the code host ([Engine.CodeHost]); a reading taken after
// the host stopped reads the cache's last pass, which is the truth about the
// node until it exits.
func (e *Engine) codeReading(ctx context.Context, now time.Time, out *statelog.Reading) {
	n := e.native.Load()
	if n == nil {
		return
	}
	if n.codeReader != nil {
		// THE QUEUED CHECKS' BACKLOG, from this node's own rows on every node
		// that holds them, whether or not any git node runs checks: a fleet
		// whose every runner is gone is the backlog the alarm exists for.
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
			// THE NAME AN OPERATOR KNOWS, read from this node's own rows
			// only while something was refused: one point read an
			// evaluation, the id where the rows hold none.
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
}

// gitBackground is what this node advertises its git cache gives its
// background work, as the seat host writes it on every beat
// ([coord.GitBackgroundKey]): [Engine.gitBackgroundBudget] on a node with the
// git role, and nil on any other, whose presence row then carries no such key
// — it serves no git, so no git node weighs it.
func (e *Engine) gitBackground() func() int64 {
	if !e.profile.Roles.Has(placement.RoleGit) {
		return nil
	}
	return e.gitBackgroundBudget
}

// gitBackgroundBudget is what this node's git cache gives its background work
// now — its maintenance budget ([cache.Cache.BackgroundBudget], the figure its
// admissions hold every run to) — which a git node advertises on every beat
// beside its status ([Engine.gitBackground]), so the git node a repository's
// maintenance is placed on is one whose own admissions would hold it. Zero
// where this node holds no cache — a company that hosts no code, or a code
// host not built yet — which is the node saying it has nothing to give. (Not
// the cache's DISK budget, [Engine.codeCacheBudget].)
//
// Read from the PUBLISHED runtime at each beat, never captured, as
// [Engine.codeReading] reads it: the code host is built at boot or at the first
// apply that hosts code, after the heartbeat starts. A load and a locked read
// of a figure in memory, so the beat can take it unbounded.
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

	// spools is the code host's request-spool allowance, which lives on the
	// cache's volume and never in its budget: [host.SpoolAllowance] of the
	// pool's transfer lane, which git.max_transfers sizes.
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
// derived share under [config.GitCacheMaxBytesFloor], the smallest budget a
// node is ever given explicitly, so that nearly every repository is a cold sync
// at nearly every fetch; or an explicit budget the room cannot hold beside the
// volume's other tenants, which the cache fills before it evicts anything.
func (s codeCacheSize) log(ctx context.Context, dir string) {
	attrs := []any{"dir", dir, "bytes", s.budget, "derived", s.derived, "room", s.room,
		"free", s.free, "held", s.held, "broker", s.broker, "spools", s.spools,
		"transfers", s.transfers, "patches", s.patches, "dedicated", s.dedicated}
	switch {
	case s.derived && s.budget < config.GitCacheMaxBytesFloor:
		log.WarnContext(ctx, "code_cache_sized", append(attrs, "detail",
			fmt.Sprintf("the room on the cache's volume derives a budget under %d bytes, so a "+
				"repository is downloaded again at nearly every fetch: %s",
				config.GitCacheMaxBytesFloor, s.remedies()))...)
	case !s.derived && s.budget > s.room:
		log.WarnContext(ctx, "code_cache_sized", append(attrs, "detail",
			"git.cache_max_bytes is more than the cache's volume has room for beside what "+
				"is already there, the broker's limit, the request spools and the revision "+
				"facts' patch spools, so the cache fills the volume before it evicts "+
				"anything: lower git.cache_max_bytes, or "+
				"give git.cache_dir a volume of its own")...)
	default:
		log.InfoContext(ctx, "code_cache_sized", attrs...)
	}
}

// codeCacheBudget is the budget of the cache prepared at layout, beside a host
// whose transfer lane of transfers slots may spool requests on its volume:
// git.cache_max_bytes where it is set, and otherwise derived from the ROOM the
// cache can use ([config.Git.CacheBudget]) —
//
//   - the volume's FREE space, PLUS
//   - what the cache already HOLDS (layout.Held), since free space excludes the
//     cache's own files and a budget derived from it alone shrank at every
//     restart of a warm node, whose first janitor pass then evicted the
//     difference, LESS
//   - what an embedded BROKER storing its streams on the same volume may still
//     write there: its store limit less what it stores. nats-server holds what
//     its streams store to that limit, and an unset stream.store_max_bytes
//     derives it as three quarters of the same free space — so a budget taken
//     from the free space beside it claimed bytes the broker had already
//     claimed, and the two together could fill the volume the store lives on.
//     Bytes the broker STORES are not free and so not counted twice; LESS
//   - the request SPOOLS' allowance ([host.SpoolAllowance]), which lives on
//     this volume and never in the budget; LESS
//   - the revision facts' PATCH SPOOLS ([query.PatchSpoolAllowance]): the
//     query server spools at most query.MaxPatchSpools patches of at most
//     query.MaxPatchIDDiffBytes each on this volume, outside the budget as
//     every spool is — bounded per node rather than per read, since per read
//     they grew with the seat share, half the read lane, and took the store's
//     headroom on the shared layout.
//
// DEDICATED where the cache's volume holds none of this node's other disk — the
// store, the replicated estate, the snapshots, the broker's streams — which
// [sameVolume] decides. A tenant that cannot be compared reads as sharing, the
// smaller budget.
//
// A BROKER WHOSE CLAIM CANNOT BE READ FAILS THE BUILD where it might share the
// volume: its limit is a struct field in this process, so a failure is a broker
// in no state to serve anything, and a budget guessed without it is the one
// this function exists not to make.
//
// A ROOM WITH NO SHARE IN IT FAILS THE BUILD TOO, by name
// ([codeCacheSize.noRoom]). Where the broker's claim and the spools take
// everything the volume has, any derived budget would be bytes the volume does
// not have, and its share is nothing — which the cache would refuse as no
// budget at all, in words naming neither the volume nor what claims it. A share
// that is merely SMALL is served and warned about ([codeCacheSize.log]): the
// cache works at any size, evicting what it is not serving, and a node is not
// refused over a cold sync. But a room of nothing is not a small cache, it is
// an overcommitted volume — the one the store and its snapshots live on, on the
// shared layout — and only the operator can say which tenant gives way. An
// explicit git.cache_max_bytes is that answer, and is never refused here: on a
// volume with no room it is warned about as more than the room.
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
	size.budget, size.derived = boot.Git.CacheBudget(size.room, size.dedicated)
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
	return fmt.Errorf("engine: git.cache_dir %s has no room for the code cache: its volume has "+
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
	out := "give git.cache_dir a volume of its own — its own filesystem with its own " +
		"space — or set git.cache_max_bytes to the budget the cache may fill"
	if s.dedicated {
		out = "give git.cache_dir a larger volume, or set git.cache_max_bytes to the " +
			"budget the cache may fill"
	}
	if s.broker > 0 {
		out += ", or set stream.store_max_bytes lower, since the broker beside the cache " +
			"on this volume may fill what it allows"
	}
	return out + fmt.Sprintf(", or lower git.max_transfers, whose every transfer may spool "+
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

// stopCodeHost ends this node's code host: it refuses every new git request,
// cancels the ones in flight — a receive-pack is killed through its context,
// its hook's connection with it — closes the hook relay and stops the cache.
// Nil-safe, which is a node that serves no git.
//
// IN THE TEARDOWN BEFORE THE COMMITTER ([Engine.stopCommitter]) AND BEFORE THE
// ADMISSION IS WITHDRAWN: a push in flight proposes through the estate, and a
// sync's linearizable read appends a barrier to the code log — an append once
// the admission is gone is a write a capacity coordinator was told nobody was
// making. A push cancelled here that a committer had already appended is
// answered `unknown` to a client that is gone; its row lands or not by the
// log, never by the host. By then the API's listener is closed, and every git
// request it let through has finished (cmd/crewlet's shutdown order).
//
// AND BEFORE THE USAGE PUBLISHER'S LAST FLUSH ([Engine.stopUsage]), with every
// other writer of a row the node's day counts: a peer's git read is answered on
// this node's git lane until the cache stops, and every read of a `visibility:
// unit` repository it answers is one ([types.CodeFetched]). Stopped after that
// flush, the reads in between reached a published day only at a next start,
// which a node that never starts again does not have.
func (e *Engine) stopCodeHost(ctx context.Context) { e.native.Load().stopCodeHost(ctx) }

// stopCodeHost is [Engine.stopCodeHost] on this runtime, and the stop its own
// failure path takes ([native.shutdown]).
//
// ONCE, AND BOUNDED ONCE, by [host.StopBudget]: everything the host waits
// for has already been cancelled by the time it waits, so what is left is
// git's own exit and the handlers' answers to clients that are gone. Both the
// teardown and the runtime's shutdown ask for it, and [host.Server.Stop] is
// idempotent only in EFFECT — a second call waits again for the same work — so
// a stop cut short by its bound would have spent a second budget of the
// process's termination grace on the shutdown's call. The second caller
// returns once the first has: a wait cut short leaves the cache's claim held
// until the last of that work ends, so no other process can open the directory
// while something of this one still writes in it, and waiting for it again
// changes nothing.
//
// THE CHECK RUNNER FIRST, inside the same one stop: each check it runs records
// its ending — `errored runner_lost` where its command had not ended, so its
// home queues it again on a git node still running — and closes its box, while
// the router its endings go through and the code host its boxes clone from
// still serve. THEN THE HOME'S CHANGE WORK: its turns read facts
// of this node's git reads, merge in scratches over its cache and ask records
// of the repositories' writers, so it is ended — a facts read cut, a merge's
// git ended and its scratch removed, a record's ask cancelled, which lands or
// does not on its own — before the git reads and the cache it reads are. THEN
// THE COMPACTOR: it proposes through the router and holds a maintenance slot
// of the cache the host stops with itself, so it is ended — its build killed,
// its uploads abandoned, an unproposed compaction's outputs left to the
// collector — before the cache is. THEN THE STATUS ANSWERER: a node that has
// decided to stop serving git stops answering for its host before the host
// and the cache its answers read stop, so no peer is told of a cache that is
// going. Its withdrawal waits only for answers already in flight, each read
// from memory.
//
// THEN THE GIT READS AND THE MERGE PREVIEWS, for the same reason and one more:
// a peer's read or preview is answered on this node's git lane by the query
// server or the previews, which the host does not count among its requests, so
// one in flight outlived the host's stop and was answered, and recorded as a
// read of a unit's repository, after it — behind the usage day's last flush
// the teardown orders after this stop. Each stop refuses every read after it,
// cuts the reads in flight — each answered unready, about this node, so its
// walk asks the next git node — and waits for them, before the host and the
// cache they read stop ([query.Server.Stop], [codehome.Previews.Stop]). A
// peer's ENGINE WRITE, answered on the git writes' lane, needs no step of its
// own: the host counts it among its requests, so the host's stop refuses
// every write after it, cuts the writes in flight — one cut before its
// landing ran nothing, and is answered not serving, about this node, so its
// walk asks the next git node the same push — and waits for them.
// All seven share the one budget, and a second caller waits for the first.
func (n *native) stopCodeHost(ctx context.Context) {
	if n == nil || n.codeHost == nil {
		return
	}
	n.codeHostStop.stop(ctx, func(ctx context.Context) {
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
		n.codeHost.Stop(ctx)
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
