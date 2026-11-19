package mcp

import "time"

// The deadlines and bounds this package enforces.
//
// They live together because two copies of a deadline is how a transport ends
// up with a ceiling its config says it does not have. Each says where its
// value came from: MEASURED means a test in this package produced the number,
// REASONED means it was argued from the surrounding behaviour and nothing was
// measured — the distinction matters more than the digits.
const (
	// DefaultStartupTimeout bounds connect + handshake, and separately bounds
	// the first tools/list. REASONED, not measured.
	//
	// Sized for the slow HEALTHY case rather than the fast one: a uvx / npx
	// server whose package is not in the local cache downloads it on first
	// launch, which on a modest link is tens of seconds for a Python package
	// with compiled dependencies. Two minutes clears that comfortably while
	// still being a deadline a booting operator will actually see, instead of
	// a boot that appears to have finished and quietly never did.
	DefaultStartupTimeout = 120 * time.Second

	// DefaultRequestTimeout bounds one tools/call. REASONED, not measured.
	//
	// The number was originally picked to match an MCP SDK's SSE-friendly HTTP
	// read default, so a tool behaved the same over stdio and over HTTP. The Go
	// SDK imposes no such default — its streamable transport uses the
	// http.Client it is given — so the *matching* argument no longer holds
	// mechanically. The number is kept anyway, for the half of the argument
	// that still does: the same tool must not have two different ceilings
	// depending on a transport the agent calling it cannot see.
	DefaultRequestTimeout = 300 * time.Second

	// shutdownGrace is one rung of the stdio transport's shutdown ladder
	// (childInput.Close): how long it waits after closing the child's stdin
	// before SIGTERM, and again before SIGKILL. The SDK's own default is 5s.
	//
	// 2s because the ladder also runs from Connect's error paths: a server that
	// launches and never speaks pays it on top of the startup deadline, on the
	// seat-acquisition path (TestStartupDeadlineOnAMuteServer: a 300ms budget
	// cost 5.30s at the SDK default). The window lets a server flush on a clean
	// shutdown; one still running 2s after its stdin closed is not watching
	// stdin, which is what the SIGTERM rung is for.
	shutdownGrace = 2 * time.Second

	// stderrDrainTimeout bounds the wait for the stderr pump to see EOF once
	// the parent's write end is closed. REASONED.
	//
	// EOF arrives once every holder has closed the descriptor, which for a
	// well-behaved tree is process exit. So this is the grace for a grandchild
	// (one uvx or npx spawned) that inherited stderr and outlived its parent to
	// let go before the tree is signalled by process group. A kill stands
	// behind it, so waiting longer only costs shutdown latency: 2s is orders of
	// magnitude more than closing a descriptor takes, and past it the holder is
	// stuck or deliberate. See stderrRelay.reap and TestMeasuredStdioTimings.
	stderrDrainTimeout = 2 * time.Second

	// stderrReapGrace is the second, shorter wait after the process group has
	// been signalled. REASONED: a SIGKILLed process closes its descriptors in
	// the kernel, so this covers scheduling, not shutdown work. Past it the
	// read end is closed under the pump, which ends it unconditionally.
	stderrReapGrace = time.Second

	// tailLines bounds the crash tail kept per server. REASONED.
	// Enough for a Python traceback plus a startup banner (a few KB), small
	// enough to hold for every spawned server at once.
	tailLines = 50

	// reapGrace is how long the shutdown ladder of a session that has
	// already ended waits for its stdio child to be reaped before it closes
	// the child's stdin (childInput.Close): long enough that a child exiting
	// on its own is recorded as having done so, rather than as ending at the
	// close that would otherwise race its exit. REASONED: a process's exit
	// closes its stdout in the same step that makes it reapable, so the
	// reaper — a goroutine already blocked in the wait — follows the end of
	// the session by a scheduling delay; a quarter of a second is far past
	// that under the race detector, and costs a server that hung up its
	// output but kept running nothing a restart would notice.
	reapGrace = 250 * time.Millisecond

	// restartJitter spreads every wait the restart schedule takes by a fifth
	// either way, so the servers of a fleet that booted together, all
	// refused by one vendor, do not ask it again in lockstep for the life of
	// the deployment. Spread rather than clamped at the ceiling: a clamp
	// would put half of them back on the same instant there.
	restartJitter = 0.2

	// maxToolPages bounds a tools/list pagination walk. REASONED.
	//
	// Real servers expose at most a few hundred tools and never split a
	// listing this finely, so a walk this long means the server's pagination
	// is broken — e.g. it ignores the cursor request param and re-serves page
	// one for ever.
	maxToolPages = 100
)

// The restart schedule: the bridge's wait before each attempt at a down server
// (supervise.go), and the seat runner's before restarting a per-role child
// whose start failed ([RestartWait]). EXPORTED so the engine can hold them to
// the seat cadence they are anchored to, which this package cannot import.
//
// A first death, or one after RestartCeiling of uptime, begins a series whose
// first attempt waits NOTHING: the child was healthy and the vendor likely
// still is. Every other attempt waits RestartBase, doubling to RestartCeiling,
// each spread by restartJitter: 15 s, 30 s, 1 min, 2 min, 4 min, then 5 min
// (± a fifth). A failed start's retries are their own series, ending when the
// server comes up (server.up). The ceiling is also what counts as stable, so a
// server crashing every few minutes is never restarted faster. REASONED.
const (
	// RestartBase is one seat heartbeat (internal/seat's HeartbeatInterval),
	// the cadence at which a node re-proves it holds a seat at all. Sooner
	// than that, a retry mostly re-asks a vendor that has just refused it,
	// forking a process each time; and a per-role child held back because
	// its node could not prove the seat (Supervision.MayStart) is asked
	// again one beat later, which is when the proof can next change.
	RestartBase = 15 * time.Second

	// RestartCeiling is the seat host's undead alarm interval (internal/
	// seat's UndeadAlarmInterval, five minutes), the interval at which
	// a seat's other persistent failure is said again. Every failed attempt
	// logs once, so this is how often a server that will not come back
	// repeats itself, and it bounds how long a vendor that has recovered
	// waits to be noticed. A deployment has no honest reason to want either
	// differently, so neither is a knob.
	RestartCeiling = 5 * time.Minute
)
