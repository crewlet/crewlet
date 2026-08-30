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

	// shutdownGrace is one rung of the transport's shutdown ladder: how long
	// it waits after closing the child's stdin before SIGTERM, and again
	// before SIGKILL. MEASURED-motivated; the SDK's own default is 5s.
	//
	// It is 2s because the ladder is NOT only walked on shutdown. The SDK
	// closes the session from inside Connect's own error paths, so a server
	// that launches and never speaks pays this on top of the startup deadline
	// before the engine can move on — and that is on the seat-acquisition
	// path. TestStartupDeadlineOnAMuteServer measured a 300ms startup budget
	// costing 5.30s wall-clock at the SDK default; the whole of the overshoot
	// was this constant.
	//
	// What the window buys is a server flushing state on a clean shutdown,
	// and a server that has not exited 2s after its stdin closed is not
	// flushing — it is not watching stdin, which is the case the SIGTERM rung
	// exists for. A well-behaved server exits in milliseconds and never
	// touches this number at all.
	shutdownGrace = 2 * time.Second

	// stderrDrainTimeout bounds the wait for the stderr pump to see EOF once
	// the parent's write end is closed. REASONED.
	//
	// EOF arrives the moment every holder of that descriptor has closed it,
	// which for a well-behaved tree is process exit. So this window is not
	// really a timeout on shutdown work — it is the grace given to a
	// GRANDCHILD (one uvx or npx spawned) that inherited stderr and outlived
	// its parent, to let go by itself before the tree is signalled by process
	// group.
	//
	// A longer grace here would be a GIVE-UP with nothing behind it — the
	// reader simply abandoned. This one has a kill behind it, so waiting
	// longer buys nothing and costs shutdown latency on every
	// server that has such a grandchild. Two seconds is orders of magnitude
	// more than closing a descriptor takes; past it the holder is stuck or
	// deliberate, and neither gets better with time. See stderrRelay.reap and
	// the measurement in TestMeasuredStdioTimings.
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

	// maxToolPages bounds a tools/list pagination walk. REASONED.
	//
	// Real servers expose at most a few hundred tools and never split a
	// listing this finely, so a walk this long means the server's pagination
	// is broken — e.g. it ignores the cursor request param and re-serves page
	// one for ever.
	maxToolPages = 100
)
