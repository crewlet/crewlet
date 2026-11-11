package mcpbridge

import (
	"time"

	"github.com/crewlet/crewlet/internal/agent/subagent"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// Dispatcher is a session's current opening as its MCP server sees it, for a
// case that has to reach a call past the route's own gate — one already on its
// way in when the run ended.
func Dispatcher(s *Session) toolsession.Dispatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

// EndOpening ends a session's live opening again, as its own end does, for a
// case that ends one opening twice: no path of the bridge does — each takes
// the opening out of the bridge's tables before it ends it — and the opening's
// end has to survive one that would ([binding.end]).
func EndOpening(s *Session) {
	s.mu.Lock()
	opened := s.live
	s.mu.Unlock()
	opened.end()
}

// HoldPollsFor shortens how long the bridge holds a poll for notes, for a case
// that waits one out.
func HoldPollsFor(b *Bridge, d time.Duration) { b.hold = d }

// SteerHold is how long a poll for notes is held when none waits.
const SteerHold = steerHold

// SteerTakeBudget bounds the inbox's answer to one hook.
const SteerTakeBudget = steerTakeBudget

// CallMargin is what a call's answer can take beyond its tool's own bound
// ([Bridge.CallTimeout]).
const CallMargin = callMargin

// THE TOOLS WHOSE BOUND A BOX'S WAIT IS ADDED UP FROM declare it in the shape
// the bridge reads ([bounded]): a rename on either side would leave a box's
// wait at the floor, with nothing to say so.
var (
	_ bounded = (*crewletmcp.Tool)(nil)
	_ bounded = (*subagent.Tool)(nil)
)
