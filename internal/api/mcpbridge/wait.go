package mcpbridge

import (
	"time"

	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
)

// HOW LONG A BOX WAITS FOR A CALL: the package doc's section of that name.
// Both harnesses time a call absolutely, whatever progress flows, so the box is
// told the bridge's bound as each one's per-server timeout (internal/sandbox's
// MCPServer.CallTimeout). No single bound exists, so the bridge adds one up: a
// call runs under its run's context with no deadline of the bridge's, each tool
// family bounds itself ([bounded], [callFloor]), it may first wait for a holder
// (fleet.go), and the bridge's own fixed waits come on top ([callMargin]). The
// bridge cuts nothing at the sum: the harness's cancel ends the wait.

// bounded is a tool that declares the longest one call of it runs
// ([Bridge.CallTimeout]); a seat's MCP tool and `delegate` implement it.
type bounded interface {
	CallBound() time.Duration
}

// callFloor is the bound of a tool that declares none: the engine's one-call
// ceiling ([crewletmcp.DefaultRequestTimeout], five minutes), so a tool has one
// ceiling whatever the transport. Every engine builtin bounds itself under it,
// the longest a Gitlet code write: two attempts of up to 112 seconds each
// (internal/estate's Timing.GitWriteAttemptFor), 224 seconds.
const callFloor = crewletmcp.DefaultRequestTimeout

// callMargin covers the bridge's fixed waits around a tool, about nineteen
// seconds, three times over: internal/engine's beginBudget (5 s) and
// appendRetryWaits (1.75 s), internal/toolsession's relistWait (2 s),
// internal/httprelay's pushWait (5 s), and internal/providers/llm/harness's
// termGrace (5 s) after `delegate`'s cap. It assumes a store that answers: no
// margin covers an outage. [Session.Started] waits on no store (internal/engine's
// bridgeLedger.started writes off the request's path). Nothing ties the sum to
// these waits, which live in packages this one cannot import, so changing one
// means rereading this. It is part of every call's [Begun.Until] ([callBound]).
const callMargin = time.Minute

// callBound is the longest one call of the tool named runs on surface: its
// declared bound ([bounded]), never under [callFloor], which a tool the surface
// lacks or a non-seat surface gets. A collection waits up to it plus
// [callMargin] past the call's start for its record ([Begun.Until]).
func callBound(surface Surface, name string) time.Duration {
	longest := callFloor
	if seat := seatOf(surface); seat != nil {
		if entry, ok := seat.Universe().Lookup(name); ok {
			if tool, ok := entry.Tool.(bounded); ok {
				longest = max(longest, tool.CallBound())
			}
		}
	}
	return longest
}

// CallTimeout is how long the harness in a box waits for one call to any server
// of an opening of surface before it gives the call up and cancels it: the
// longest this bridge can take to answer a correct call of it.
//
// It is [FleetOptions.HolderWait] (none outside a fleet), since a call can land
// while no node can answer, plus the longest bound of any tool in the surface's
// universe, offered or not (a run activates tools mid-run, and the box's settings
// are written once, at launch), never under [callFloor], plus [callMargin].
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
