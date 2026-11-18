package mcp

import (
	"context"
	"fmt"
	"time"
)

// handoff is a server being replaced (package doc, "Replacing a server").
type handoff struct {
	idle chan struct{} // closed once no call is in flight through the old server
	done chan struct{} // closed once the replacement concluded, next and why set
	next *server       // the replacement, nil where none came up
	why  string        // why none came up
}

// replaceBound is the longest a call waits for the server's replacement: the
// calls in flight, the old process's shutdown ladder, the new one's start.
func (s Spec) replaceBound() time.Duration {
	return s.requestTimeout() + 2*shutdownGrace + stderrDrainTimeout + stderrReapGrace +
		2*s.startupTimeout()
}

// enter admits a call: the live session, counted in flight until leave; else
// the hand-off to wait on while the server is replaced; else neither (refused).
func (s *server) enter() (*client, *handoff) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.retiring != nil:
		return nil, s.retiring
	case s.client == nil || s.client.gone():
		return nil, nil
	}
	s.inFlight++
	return s.client, nil
}

// leave ends a call enter admitted.
func (s *server) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight--
	if s.inFlight == 0 && s.retiring != nil {
		close(s.retiring.idle)
	}
}

// retire hands every later call to the replacement and waits for those in
// flight, within the request timeout. Only the owner concludes the hand-off.
func (s *server) retire(ctx context.Context) (h *handoff, owner bool) {
	s.mu.Lock()
	if s.retiring != nil {
		h = s.retiring
		s.mu.Unlock()
		return h, false
	}
	h = &handoff{idle: make(chan struct{}), done: make(chan struct{})}
	s.retiring = h
	inFlight := s.inFlight
	if inFlight == 0 {
		close(h.idle)
	}
	s.mu.Unlock()
	if inFlight == 0 {
		return h, true
	}

	bound := s.spec().requestTimeout()
	s.b.log.Info("server_replacement_waiting", "server", s.spec().Name, "calls_in_flight", inFlight,
		"detail", "the restart waits for the calls in flight to answer, at most "+bound.String())
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-h.idle:
	case <-t.C:
	case <-ctx.Done():
	}
	return h, true
}

// conclude releases the calls waiting on h to next, or refuses them with why
// where next is nil.
func (h *handoff) conclude(next *server, why string) {
	h.next, h.why = next, why
	close(h.done)
}

// handedOn makes a call that reached t's server while it was replaced on the
// replacement, where it lists the same tool ([Tool.handsOnTo]): never sent, so once.
func (t *Tool) handedOn(ctx context.Context, args map[string]any, h *handoff) (Result, error) {
	select {
	case <-h.done:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	name := fmt.Sprintf("%s/%s", t.srv.spec().Name, t.raw)
	if h.next == nil {
		return Result{
			Output: fmt.Sprintf("MCP tool %s cannot be called: the %q server was being restarted "+
				"and did not come back (%s). The call was not sent; the engine retries the server "+
				"on its own, so carry on without it or try again later.", name, t.Server(), h.why),
			Failed: true,
			Cause:  RefusalUnavailable,
		}, nil
	}
	next := t.srv.b.toolOf(h.next, t.raw)
	if next == nil || !t.handsOnTo(next) {
		return Result{
			Output: fmt.Sprintf("MCP tool %s was not called: the %q server restarted and now lists "+
				"this tool differently, or not at all. The call was not sent; look the tool up again "+
				"before calling it.", name, t.Server()),
			Failed: true,
			Cause:  RefusalStaleVersion,
		}, nil
	}
	return next.Call(ctx, args)
}

// handsOnTo reports whether next may make a call admitted to t: the same tool
// in every respect, at a level no stricter, so the call skips no question.
func (t *Tool) handsOnTo(next *Tool) bool {
	same := *next
	same.level = t.level
	return same.sameAs(t) && strictness(next.level) <= strictness(t.level)
}

// strictness orders levels from the one that asks nothing.
func strictness(l Level) int {
	switch l {
	case LevelAllow:
		return 0
	case LevelApprove:
		return 1
	}
	return 2
}

// toolOf is the tool s files under the server's own name raw, nil for none.
func (b *Bridge) toolOf(s *server, raw string) *Tool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, t := range s.tools {
		if t.raw == raw {
			return t
		}
	}
	return nil
}
