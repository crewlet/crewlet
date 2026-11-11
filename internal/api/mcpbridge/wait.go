package mcpbridge

import (
	"time"

	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
)

// HOW LONG A BOX WAITS FOR A CALL.
//
// A call's answer is kept going while its tool runs (answerEvery), which every
// silence check a harness keeps on a call reads as a call at work: Claude
// Code's idle check starts again at every progress, its transport-loss check
// is disarmed by one, and OpenCode keeps none. So once the answer is kept going
// only the harness's own tool timeout bounds how long a box waits for a call,
// and a tool that hangs keeps its run waiting that long: Claude Code's default
// is 1e8 ms and OpenCode's twelve hours (2.1.310 and 2.1.4, by reading their
// bundles). Both time a call absolutely, never starting again at a progress
// (measured: OpenCode 2.1.4 and Claude Code 2.1.310 each cut a call kept going
// at its timeout, and cancelled it).
//
// So the box is told, on every server it is handed, the bridge's own bound for
// a call — the longest a correct call of the opening can take to be answered
// — as each harness's per-server timeout (internal/sandbox's
// MCPServer.CallTimeout). Past it the harness gives the call up and cancels
// its request, and the call is recorded as one nobody waited for, which no
// save point holds.
//
// NO SINGLE BOUND EXISTS, so the bridge adds up its own. A call runs under its
// run's context, stripped of its cancellation, and the bridge sets no deadline
// on it: each tool family bounds itself, a seat's MCP server's call at the
// server's request timeout (internal/mcp), `delegate` at its call cap
// (internal/agent/subagent), and the engine's other builtins at bounds of their
// own, the longest found under four minutes ([callFloor]). Before a call
// reaches the node that runs it, it may wait for a holder (fleet.go). And
// around the tool the bridge does work of its own, its fixed waits seconds
// long ([callMargin]).
//
// THE BRIDGE CUTS NOTHING AT IT. A call past every bound the engine sets is a
// defect or a hang, and a tool half-way through a write is the run's ("a call
// is the run's, not the request's"); answered early while its tool ran on, it
// would be told failed though its effect is still to come. The harness's
// cancel is what ends the wait, and the bridge records it as it records any.

// bounded is a tool that declares the longest one call of it runs: what the
// bridge adds up a box's wait from ([Bridge.CallTimeout]). Declared here, by
// its consumer; a seat's MCP tool and `delegate` declare theirs.
type bounded interface {
	CallBound() time.Duration
}

// callFloor is the bound of a tool that declares none: the engine's one-call
// ceiling ([crewletmcp.DefaultRequestTimeout], five minutes), which a seat's
// MCP server's call is held to unless its entry says otherwise, for the reason
// it gives — a tool must not have two ceilings by a transport the agent cannot
// see. Every engine builtin found bounds itself under it: the longest a code
// write the estate routes to a repository's Gitlet node, asked of its home node
// and then of its runner-up, each attempt up to 112 seconds for a rebase
// (internal/estate's Timing.GitWriteAttemptFor), so 224 seconds in all; then
// SessionReadWithin, two minutes (internal/agent/builtin, a read of a seat's
// kept session).
const callFloor = crewletmcp.DefaultRequestTimeout

// callMargin is what a call's answer can take beyond its tool's own bound:
// the opening's first request held behind the record of which session the
// job's box started (internal/engine's sessionStartRecordBudget, five
// seconds), the append of the call to the run's log and its retries (its
// appendRetryWaits, 1.75 seconds in all, and four attempts), an activation's
// answer held for the box's re-list (internal/toolsession's relistWait, two
// seconds), a relayed answer's frame (internal/httprelay's pushWait, five
// seconds), and after `delegate`'s cap its workers' harnesses ending within
// their grace (internal/providers/llm/harness's termGrace, five seconds). About
// nineteen seconds of fixed waits, which a minute covers three times over, ON A
// COORDINATION STORE THAT ANSWERS: each of the append's four attempts is a
// compare-and-swap no clock here bounds, and a store in an outage holds the
// call's answer for as long as it takes, which no margin would cover. NOTHING
// HOLDS THE SUM TO THE MARGIN: the five live in four other packages, two of
// them the engine's, which this one cannot import, so moving any of them is
// reading this margin again.
const callMargin = time.Minute

// CallTimeout is how long the harness in a box waits for one call to any server
// of an opening of surface before it gives the call up and cancels it: the
// longest this bridge can take to answer a correct call of it.
//
// THE HOLDER'S WAIT ([FleetOptions.HolderWait], two seat-lease lifetimes at
// the engine's default: none on a bridge that joined no fleet) — a call can
// land while no node can answer it — plus THE LONGEST BOUND the surface's
// tools declare, never under [callFloor], plus [callMargin]. Every tool the
// surface's universe holds is counted, offered or not, since a run activates
// tools mid-run, and the box's settings are written once, at its launch. A
// surface that is not a seat's holds the engine's builtins alone.
func (b *Bridge) CallTimeout(surface Surface) time.Duration {
	if b == nil {
		return 0
	}
	longest := callFloor
	if seat := seatOf(surface); seat != nil {
		for _, entry := range seat.Universe().Entries() {
			if tool, ok := entry.Tool.(bounded); ok {
				longest = max(longest, tool.CallBound())
			}
		}
	}
	var holder time.Duration
	if f := b.fleet.Load(); f != nil {
		holder = f.holderWait
	}
	return holder + longest + callMargin
}
