package mcpbridge

import (
	"time"

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

// HoldPollsFor shortens how long the bridge holds a poll for notes, for a case
// that waits one out.
func HoldPollsFor(b *Bridge, d time.Duration) { b.hold = d }

// SteerHold is how long a poll for notes is held when none waits.
const SteerHold = steerHold

// SteerTakeBudget bounds the inbox's answer to one hook.
const SteerTakeBudget = steerTakeBudget
