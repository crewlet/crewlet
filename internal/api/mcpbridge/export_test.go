package mcpbridge

import "github.com/crewlet/crewlet/internal/toolsession"

// Dispatcher is a session's current opening as its MCP server sees it, for a
// case that has to reach a call past the route's own gate — one already on its
// way in when the run ended.
func Dispatcher(s *Session) toolsession.Dispatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}
