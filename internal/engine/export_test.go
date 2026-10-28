package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// MailboxTurn hands e one inbox partition for handle's seat as the node's
// mailbox does once it has admitted the turn ([Engine.mailboxTurn]), for a test
// outside the package whose subject is the dispatcher behind it rather than the
// node's admission in front of it.
func MailboxTurn(ctx context.Context, e *Engine, handle string, evs []*events.Event) queue.Result {
	return e.mailboxTurn(ctx, handle, evs)
}

// ControlOutcome is what a seat's control-topic delivery leaves with the broker,
// for a test outside the package that drives the coordinator behind it.
func ControlOutcome(err error) queue.Result { return controlOutcome(err) }

// Reflects reports whether e has built its reflect dispatcher, for a test
// outside the package that drives an engine through its public surface.
func Reflects(e *Engine) bool { return e.reflector.Load() != nil }

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
