package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	codecompact "github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// Reflects reports whether e has built its reflect dispatcher, for a test
// outside the package that drives an engine through its public surface.
func Reflects(e *Engine) bool { return e.reflector.Load() != nil }

// GitReadForTest is a read of a repository by object id through e's own estate
// router — the repository's home git node first, this node at its place — for
// a test outside the package that drives the git walk through a running engine.
func GitReadForTest(ctx context.Context, e *Engine, q code.GitQuery) (code.GitAnswer, error) {
	return e.router.Code().GitRead(ctx, q)
}

// GitWriteForTest is an engine write through e's own estate router — the
// repository's home git node first, this node at its place — for a test
// outside the package that drives the git write's walk through a running
// engine.
func GitWriteForTest(ctx context.Context, e *Engine, w code.EngineWrite) (code.EngineWriteResult, error) {
	return e.router.Code().GitWrite(ctx, w)
}

// CodeReadingForTest is the code half of e's alarm reading now
// ([Engine.codeReading]) — its committer's, its git cache's and its
// compactor's, read the way every evaluation reads them — for a case outside
// the package that judges what a running node's committer or compactor found,
// apart from when the alarm table next evaluates it.
func CodeReadingForTest(ctx context.Context, e *Engine) statelog.Reading {
	var r statelog.Reading
	e.codeReading(ctx, time.Now().UTC(), &r)
	return r
}

// WithCommitterPublisher is opts with wrap standing between a data node's
// committer and the code log's write authority it publishes through
// ([code.CommitterOptions.Publisher]), for a case outside the package whose
// subject is what a running node's committer raises — a repository's queue
// filled behind a batch held at its publish, rather than by a burst raced
// against batches a healthy committer lands in milliseconds.
func WithCommitterPublisher(opts Options, wrap func(code.Publisher) code.Publisher) Options {
	opts.committerPublisher = wrap
	return opts
}

// CommitterOf is the committer e's runtime decides pushes with, nil where it
// built none, for a case outside the package that proposes to it directly.
func CommitterOf(e *Engine) *code.Committer {
	if n := e.native.Load(); n != nil {
		return n.committer
	}
	return nil
}

// ItemRanker is the ranker the tracker's item search is built with, so the
// serving-seams audit outside the package reflects over the fields it reads
// rather than over a description of them a new field would leave behind.
type ItemRanker = itemRanker

// LearningDutyName is the duty every background learning pass claims, so a
// test outside the package reads the key the engine claims rather than a copy
// of it a rename would leave behind.
const LearningDutyName = learningDutyName

// WithUsageFlushEvery is opts with this node's usage publisher ticking every
// every rather than every [usage.FlushInterval], for a test outside the
// package that must see the engine keep a LIVE loop running.
func WithUsageFlushEvery(opts Options, every time.Duration) Options {
	opts.usageEvery = every
	return opts
}

// The helper tool server's environment, named once for both halves of the
// suite: mcpserver_test.go runs the server (its TestMain re-executes the test
// binary as one) and says what each variable does, and a case inside the
// package launches the same server, which it could not if the names were
// declared outside it.
const (
	ToolServerModeEnv          = "CREWLET_ENGINE_TEST_MCP"
	ToolServerToolEnv          = "CREWLET_ENGINE_TEST_TOOL"
	ToolServerEchoEnv          = "CREWLET_ENGINE_TEST_ECHO"
	ToolServerRequireEnv       = "CREWLET_ENGINE_TEST_REQUIRE"
	ToolServerHoldEnv          = "CREWLET_ENGINE_TEST_HOLD"
	ToolServerReadyEnv         = "CREWLET_ENGINE_TEST_READY"
	ToolServerLaunchesEnv      = "CREWLET_ENGINE_TEST_LAUNCHES"
	ToolServerFailLaunchesEnv  = "CREWLET_ENGINE_TEST_FAIL_LAUNCHES"
	ToolServerCrashLaunchesEnv = "CREWLET_ENGINE_TEST_CRASH_LAUNCHES"
	ToolServerCrashCode        = 3
	ToolServerLaunchToken      = "{launch}"
)

// WithMCPRetryBase is opts with the MCP restart schedule's first wait at base
// rather than [mcp.RestartBase], the rest of it scaled to match, for a test
// outside the package that must watch a retry or a restart bring a server up —
// a shared server's or a seat's per-role child's — without waiting fifteen
// seconds for it.
func WithMCPRetryBase(opts Options, base time.Duration) Options {
	opts.mcpRetryBase = base
	return opts
}

// MCPStatus is this node's MCP servers as its heartbeat would publish them
// now, for a test outside the package that reads what an operator is told.
func MCPStatus(e *Engine) []coord.MCPServerStatus { return e.mcpStatus(context.Background()) }

// WithSweepEvery is opts with this node's seat host sweeping placement every
// every rather than every [seat.SweepInterval], for a test outside the package
// that must tell a claim the sweep's tick made from one something else asked
// for.
func WithSweepEvery(opts Options, every time.Duration) Options {
	opts.sweepEvery = every
	return opts
}

// WithCacheDisk is opts with the code cache sized as though its volume had free
// bytes free and an embedded broker storing its streams there could still
// write broker bytes — one volume holding every tenant, as the single-volume
// layout does — for a case outside the package whose subject is the budget a
// disk derives, which the machine running it would otherwise decide.
func WithCacheDisk(opts Options, free, broker int64) Options {
	opts.cacheDisk = &cacheVolume{
		free: func(string) (int64, error) { return free, nil },
		same: func(string, string) bool { return true },
		broker: func() (jetstream.VolumeClaim, bool, error) {
			return jetstream.VolumeClaim{Dir: "stream", Limit: broker}, true, nil
		},
	}
	return opts
}

// WithGitMemory is opts with the git pool sizing its lanes, and the code cache
// budgeting its background work, from a node of memory bytes rather than from
// the memory of the machine running the case ([gitproc.Options.Memory]), for a
// case outside the package whose subject is what a node's memory leaves its
// git cache.
func WithGitMemory(opts Options, memory int64) Options {
	opts.gitMemory = memory
	return opts
}

// WithCompactorEstate is opts with wrap standing between a git node's
// compactor and the router's code seam it lists and proposes through
// ([codecompact.Options.Estate]), for a case outside the package whose subject
// is what a running node's compactor raises — a repository shown at the pack
// bound without two thousand packs pushed into it.
func WithCompactorEstate(opts Options, wrap func(codecompact.Estate) codecompact.Estate) Options {
	opts.compactorEstate = wrap
	return opts
}

// TestGitTransfers is the transfer lane [PinGitLanes] gives a case's node:
// four, 256 MiB of request spool.
const TestGitTransfers = 4

// PinGitLanes sets b's git.max_transfers to [TestGitTransfers], for every
// case booting a node that may build a code host. The transfer lane sizes the
// request spools the cache's room is taken out of (host.SpoolAllowance), and
// unset it follows the machine's cores — four a core, as memory allows. Beside
// a broker on the same temporary volume, which claims three quarters of its
// free space, a sixteen-core machine's 4 GiB of spools left a node no room
// under 16 GiB free, and its node refused to host code: a property of the
// machine running the suite, never of the case. Pinned, the room is a quarter
// of the free space less 256 MiB, which every volume a node boots on has.
func PinGitLanes(b *config.Bootstrap) { b.Git.MaxTransfers = TestGitTransfers }

// RequireNodeGit holds the git the suite runs to the node floor
// ([gittest.RequireNode]) where a boot over b for company builds a code host —
// the company hosts code and b's node has the git role, which is
// [Engine.hostsGit] read off the two documents — so a machine below the floor
// reads the contributor's sentence rather than the node's refusal, and a boot
// that builds no pool asks nothing of the machine's git. A case whose company
// hosts code only from a later activation calls [gittest.RequireNode] itself.
func RequireNodeGit(t testing.TB, b *config.Bootstrap, company *config.Company) {
	t.Helper()
	if company.NeedsRole(placement.RoleGit) && b.Profile("").Roles.Has(placement.RoleGit) {
		gittest.RequireNode(t)
	}
}

// SettleSeatServers waits until no seat this node holds has a pass over its
// per-role children in flight, for a test outside the package that reads what
// an apply's reconcile of those children did: the apply hands each seat's
// pass to the seat's runner and returns without waiting for it, so a read
// straight after the apply sees the children as the apply found them. A
// runner is chosen before the apply returns, so every pass it asked for has
// ended — its surface filed — when this does.
func SettleSeatServers(e *Engine) {
	e.mcpMu.Lock()
	held := make([]*seatChildren, 0, len(e.seatMCP))
	for _, s := range e.seatMCP {
		held = append(held, s)
	}
	e.mcpMu.Unlock()
	for _, s := range held {
		s.mu.Lock()
		for s.running {
			s.idle.Wait()
		}
		s.mu.Unlock()
	}
}

// SeedStore writes the migrated store image ([storetest.Seed]) where b's store
// opens, for a case booting a node on a path of its own: the node's file
// unless the store is scratch — discarded at open, so a seed would only be
// deleted — and the replicated file beside it only when the node holds the
// estate ([HoldsEstate]), since a fresh node without `data` has no such file
// and a backup or an estate check would find one. A file already there is
// left as it is, so a case booting twice on one path reopens its own.
//
// Call it once b is final — its roles and paths set — and before the first
// open. NOT for a case whose subject is the fresh file itself: the boot that
// creates and migrates the estates, or a store that cannot open.
func SeedStore(t testing.TB, b *config.Bootstrap) {
	t.Helper()
	if b.Store.Scratch {
		return
	}
	storetest.Seed(t, store.EstateNode, b.Store.Path)
	if HoldsEstate(b) {
		storetest.Seed(t, store.EstateReplicated,
			store.ReplicatedPath(b.Store.Path, b.Store.ReplicatedPath))
	}
}

// SeatBoxSetupForTest is the provisioning a seat's box is given at launch, as
// this node would hand it over: the provider-wide steps passed in, then the
// seat's own from the running company, every file's body read through this
// node's resolver.
//
// EXPORTED FOR A TEST ONLY: in production it is one step of a launch that
// needs a manager, a pending-run store and a budget, none of which is what a
// case about how a file reaches the box is about.
func SeatBoxSetupForTest(e *Engine, defaults []sandbox.SetupStep, handle string) []sandbox.SetupStep {
	c := e.Company()
	return e.runEnvironment(context.Background(), c, c.Org.AgentSeatByHandle(handle), seatSandbox(c, handle), defaults).setup
}

// ForgetPersonBlinderForTest drops this node's resolved blinder, so the next
// use resolves the company's key again.
//
// EXPORTED FOR A TEST ONLY: in production the cache is right for the life of
// the process, because the key never changes while a company runs. What a
// test needs is the state a restarted node is in after somebody deleted the
// key — and an engine cannot be restarted over the same store in one process.
func ForgetPersonBlinderForTest(e *Engine) {
	e.personBlinds.mu.Lock()
	defer e.personBlinds.mu.Unlock()
	e.personBlinds.held = nil
}
