package estate

import (
	"context"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Session is THIS NODE's read-your-writes floor: the furthest position any
// write it made — or anything it was told to wait for — reached, per log
// stream. Every request carries the floor on the log of its operation's own
// domain, and whichever data node answers it, this node included, waits to
// have applied it first.
//
// # One table per node, consulted for local reads too
//
// It is the NODE's, shared by every seat on it, which is conservative rather
// than wrong: a read may wait for another seat's write on this node as well as
// its own, and never for less than its own.
//
// And it is consulted by a read this node answers ITSELF, not only by one it
// sends. A node's seats may write through another data node — this node's
// copy was out of service, or behind — and then read here once its copy
// serves again; the write is on the log and not yet in this node's rows. A
// data node that trusted its own copy because "it applies the write itself" is
// one that answers its own seat from before a write it has already been told
// landed.
type Session struct {
	mu sync.Mutex
	// highWater is the furthest position observed on each stream.
	highWater map[string]statelog.Position
}

// NewSession is an empty session: no write observed yet.
func NewSession() *Session {
	return &Session{highWater: map[string]statelog.Position{}}
}

// Observe raises the floor on at's stream to at, where at is further than
// what the session holds — every write result, local or remote, and every
// position a caller is told to wait for. A position with no stream or no
// sequence names nothing to wait for and is ignored.
func (s *Session) Observe(at statelog.Position) {
	if s == nil || at.Stream == "" || at.Seq == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if held, ok := s.highWater[at.Stream]; !ok || at.Packed() > held.Packed() {
		s.highWater[at.Stream] = at
	}
}

// Await is [Session.Observe] in the shape the tool seams wait in: nothing is
// waited for HERE, because what the next read needs is that whichever data
// node answers it has applied this far — and that node waits.
func (s *Session) Await(_ context.Context, at statelog.Position) error {
	s.Observe(at)
	return nil
}

// Floors is the session's floor on each of streams that has one, in the
// order streams names them.
func (s *Session) Floors(streams []string) []statelog.Position {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []statelog.Position
	for _, stream := range streams {
		if at, ok := s.highWater[stream]; ok {
			out = append(out, at)
		}
	}
	return out
}

// Forget drops floors no data node will ever reach — each a position on a
// generation its log has since abandoned, as a node reported
// ([reply.Obsolete]) — so the node stops carrying a floor that refuses every
// read of its log.
//
// BY POSITION, NOT BY STREAM: a floor is dropped only while the session still
// holds exactly the one that was reported. A write observed after the request
// went out raised the floor past it — on the log's new generation, which a
// node CAN reach — and dropping the stream wholesale would lose that write
// from the next read.
func (s *Session) Forget(floors ...statelog.Position) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, floor := range floors {
		if held, ok := s.highWater[floor.Stream]; ok && held == floor {
			delete(s.highWater, floor.Stream)
		}
	}
}

// named is the floors among sent whose stream is one of streams.
func named(sent []statelog.Position, streams []string) []statelog.Position {
	var out []statelog.Position
	for _, at := range sent {
		if slices.Contains(streams, at.Stream) {
			out = append(out, at)
		}
	}
	return out
}
