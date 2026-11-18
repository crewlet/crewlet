package estate

import (
	"context"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Session is THIS NODE's read-your-writes floor: the furthest position any
// write it made, or anything it was told to wait for, reached per log stream.
// Every request carries the floor on its operation's own domain's log, and
// whichever data node answers, this one included, waits to have applied it.
//
// It is the NODE's, shared by every seat on it: conservative, never short of a
// seat's own writes. A read this node answers ITSELF consults it too, since
// its seats may have written through another data node while this copy was
// out of service (package doc, "Read-your-writes: ONE floor table per node,
// consulted for local reads too").
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
// waited for HERE, since whichever data node answers the next read waits.
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

// Forget drops floors no data node will ever reach, positions on a generation
// their log abandoned as a node reported ([reply.Obsolete]), which would
// otherwise refuse every read of that log.
//
// BY POSITION, NOT BY STREAM, only while the session holds exactly the floor
// reported: a write observed since the request went out raised the floor onto
// the new generation, which a node CAN reach, and dropping the stream would
// lose that write from the next read.
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
