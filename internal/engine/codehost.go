package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// THE CODE HOST ON A GIT NODE: the cache of the company's repositories this
// node serves from, the git pool every process runs in, and the host — smart
// HTTP for persons — built over them, on a node with the git role whose company
// sets code.native. The committer and the lifecycle store are every data
// node's (code.go); the host is only a git node's, and it reaches the code
// log through the estate's router like every other surface, so the node it
// asks is whichever serves the operation.

// Chart is the CURRENT epoch's organization, nil before a company: the one
// function every surface that asks "who is this token's person" reads per
// call, because an apply replaces it — the operator catalogue, the host's
// Persons and the scheduler's org alike.
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

// boundSeat is the human seat an operator token is bound to by
// `contact.crewlet_operator_id` on the current chart, "" for none — the host's
// Persons, the same answer the operator surface gives about the same caller.
func (e *Engine) boundSeat(operatorID string) string {
	return operator.BoundSeat(e.Chart, e.LookupSecret, operatorID)
}

// auditGit records one push onto this node's own queue, through the operator
// surface's audit — one construction of the record's envelope, whose source is
// the operator's, and a publish that outlives the request.
func (e *Engine) auditGit(ctx context.Context, r types.OperatorActed) {
	operator.Audit(ctx, e.backends.Queue, types.NewOperatorActed(r))
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
// the cache's directory, the git pool, the cache's budget, the cache and the
// host, in the order each needs the last — and starts it, answering the budget
// it sized the cache to beside it. Called by startNative only where
// [Engine.hostsGit]. An error fails the boot, or the apply that reached
// [Engine.startNativeFor], which is retried like every start of the runtime: a
// company that asked for native code on a node that serves git cannot be
// served by it.
//
// A FAILURE GIVES THE DIRECTORY BACK. Prepare claims the cache's directory for
// this process, so a retried apply in the same process would otherwise find it
// held by its own failed attempt: before Open the layout is released, after it
// the cache is stopped.
func (e *Engine) buildCodeHost(ctx context.Context, n *native, boot *config.Bootstrap) (*host.Server, codeCacheSize, error) {
	// 1. THE DIRECTORY, prepared BEFORE git's HOME exists, since it names it.
	// Absolute: a store path written relative resolves against the working
	// directory the store itself is opened from.
	dir, err := filepath.Abs(boot.Git.CacheDirFor(boot.Store.Path))
	if err != nil {
		return nil, codeCacheSize{}, fmt.Errorf("engine: git.cache_dir: %w", err)
	}
	layout, err := cache.Prepare(ctx, dir)
	if err != nil {
		return nil, codeCacheSize{}, fmt.Errorf("engine: git.cache_dir %s: %w", dir, err)
	}
	defer layout.Release()

	// 2. GIT, the node's own, its lanes resolved once by the pool — which
	// logs what it found (code_git_ready).
	pool, err := gitproc.New(ctx, gitproc.Options{
		MaxProcesses: boot.Git.MaxProcesses, MaxTransfers: boot.Git.MaxTransfers,
		Home: layout.Home, Metrics: e.metrics,
	})
	if err != nil {
		return nil, codeCacheSize{}, fmt.Errorf("engine: this node has the %q role and the "+
			"company sets code.native, so it serves git: %w", placement.RoleGit, err)
	}

	// 3. THE BUDGET, from the room the cache can use — after the pool, whose
	// transfer lane sizes the request spools that share the cache's volume
	// outside its budget.
	size, err := codeCacheBudget(boot, layout, pool.Slots(gitproc.ClassTransfer), e.cacheVolume())
	if err != nil {
		return nil, codeCacheSize{}, err
	}
	size.log(ctx, layout.Dir)

	// 4. THE OBJECTS a push's pack bundle is stored in and a sync reads.
	objects := e.Objects()
	if objects == nil {
		return nil, codeCacheSize{}, fmt.Errorf("engine: code.native needs the object store " +
			"(store.objects): this node has none")
	}

	// 5. THE HOOK every push's receive-pack execs.
	hook, err := e.gitHook()
	if err != nil {
		return nil, codeCacheSize{}, err
	}

	// 6. THE CACHE, which takes over the layout's claim.
	seam := e.router.Code()
	c, err := cache.Open(ctx, cache.Options{
		Layout: layout, MaxBytes: size.budget, Git: pool, Objects: objects, Fetch: seam,
		Limits: e.codeLimits, HookCommand: hook, Metrics: e.metrics, Now: time.Now,
	})
	if err != nil {
		return nil, codeCacheSize{}, fmt.Errorf("engine: open the code cache at %s: %w", layout.Dir, err)
	}

	// 7. THE HOST, over the engine's own seams: the router for the rows and
	// the committer, the chart for a token's person, the operator audit for
	// every push, and the current epoch's limits and retention.
	h, err := host.New(host.Options{
		Cache: c, Git: pool, Estate: seam, Objects: objects,
		Persons: e.boundSeat, Audit: e.auditGit,
		Limits: e.codeLimits, DeletedRetention: e.codeDeletedRetention,
		NodeID: n.nodeID, Timing: host.Production, Metrics: e.metrics,
	})
	if err != nil {
		c.Stop(context.WithoutCancel(ctx))
		return nil, codeCacheSize{}, fmt.Errorf("engine: build the code host: %w", err)
	}
	// ITS OWN LIFETIME from here: the relay and the cache's background work
	// run until the host is stopped, however long this call's caller lives.
	if err := h.Start(ctx); err != nil {
		h.Stop(context.WithoutCancel(ctx))
		return nil, codeCacheSize{}, fmt.Errorf("engine: start the code host: %w", err)
	}
	return h, size, nil
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

	// room is the space the cache can use: free plus held, less broker and
	// spools. What a derived budget is a share of, and what an explicit one
	// is compared with.
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
		"transfers", s.transfers, "dedicated", s.dedicated}
	switch {
	case s.derived && s.budget < config.GitCacheMaxBytesFloor:
		log.WarnContext(ctx, "code_cache_sized", append(attrs, "detail",
			fmt.Sprintf("the room on the cache's volume derives a budget under %d bytes, so a "+
				"repository is downloaded again at nearly every fetch: %s",
				config.GitCacheMaxBytesFloor, s.remedies()))...)
	case !s.derived && s.budget > s.room:
		log.WarnContext(ctx, "code_cache_sized", append(attrs, "detail",
			"git.cache_max_bytes is more than the cache's volume has room for beside what "+
				"is already there, the broker's limit and the request spools, so the cache "+
				"fills the volume before it evicts anything: lower git.cache_max_bytes, or "+
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
//     this volume and never in the budget.
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
		transfers: transfers, dedicated: true}
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
	size.room = f + layout.Held - size.broker - size.spools
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
	tenants := fmt.Sprintf("its %d transfers' request spools may hold %d bytes", s.transfers, s.spools)
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
func (e *Engine) stopCodeHost(ctx context.Context) { e.native.Load().stopCodeHost(ctx) }

// stopCodeHost is [Engine.stopCodeHost] on this runtime, and the stop its own
// failure path takes ([native.shutdown]).
//
// ONCE, AND BOUNDED ONCE, by [codeHostStopBudget]: everything the host waits
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
func (n *native) stopCodeHost(ctx context.Context) {
	if n == nil || n.codeHost == nil {
		return
	}
	n.codeHostStop.stop(ctx, n.codeHost.Stop)
}

// codeHostStopBudget bounds a stop's wait for the code host's cancelled work:
// THREE KILL GRACES. A cancelled git process is sent SIGTERM and, gitproc.KillGrace
// later, SIGKILL; a pipe a straggler still holds is closed a further KillGrace
// after that; and the third is for what follows a process's end — a handler's
// answer to a client that has gone, an upload abandoned under its own bound.
const codeHostStopBudget = 3 * gitproc.KillGrace

// boundedStop runs a stop ONCE, its wait bounded by budget, however many
// callers ask for it: every caller returns once that one stop has, and none
// waits a second budget. Its zero value bounds by [codeHostStopBudget].
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
			budget = codeHostStopBudget
		}
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
		defer cancel()
		stop(bounded)
	})
}
