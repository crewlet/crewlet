package live

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// The frame log's bounds.
const (
	// FrameLogFrames is the most frames one turn's log retains: a two-minute
	// turn with deltas coalesced at 200 ms writes a few hundred, so 2000 is a
	// turn at its longest timeout with room for its steps and cards.
	FrameLogFrames = 2000
	// FrameLogBytes is the most encoded bytes one turn's log retains, the
	// size of the largest single frame the protocol admits
	// ([wire.FrameMaxBytes]), so a log can always hold at least the frame it
	// was just handed.
	FrameLogBytes = wire.FrameMaxBytes
	// SubscriberFrames is how far a stream may fall behind its turn before
	// it is closed: 512 frames, the depth the dashboard socket's hub queues
	// for a client too. Past it the client resumes from its Last-Event-ID,
	// and the log replays what it missed (see the package doc).
	SubscriberFrames = 512
	// EndedRetention is how long a log whose turn has ended keeps its frames
	// for a client resuming after a network blip: two minutes, longer than
	// any browser's reconnect back-off and far shorter than anybody waits
	// for a drawer, after which the transcript is what answers.
	EndedRetention = 2 * time.Minute
)

// ErrEnded reports a frame appended to a log whose turn segment has ended.
var ErrEnded = errors.New("live: the turn's frame log has ended")

// ErrCaughtUp reports a stream with nothing to send: the turn's segment has
// ended, and the client resumed after its last frame, so it already holds
// every frame the segment told, its `done` included.
//
// NOT AN EMPTY SUCCESS: a stream that closes having said nothing reads to a
// client as a connection that dropped before the turn ended, and it dials
// again, into the same answer, for as long as the log is retained. The HTTP
// surface answers this `204 No Content`, the server-sent events convention for
// "do not reconnect" (internal/api/assistantapi).
var ErrCaughtUp = errors.New("live: nothing follows the frame the stream resumed after, " +
	"and the turn's segment has ended")

// Log is one turn's frames on the node running it: numbered, retained within
// [FrameLogFrames] and [FrameLogBytes], and fanned out to every stream
// following it.
type Log struct {
	owner, conversation, turn uuid.UUID
	now                       func() time.Time
	tap                       Tap

	mu     sync.Mutex
	next   uint64
	frames []retained
	bytes  int
	subs   map[*Subscription]struct{}
	ended  bool
	endAt  time.Time
}

// retained is one frame and its encoded size.
type retained struct {
	frame wire.Frame
	size  int
}

// newLog starts one of owner's logs whose first frame takes seq first.
func newLog(owner, conversation, turn uuid.UUID, first uint64, now func() time.Time, tap Tap) *Log {
	return &Log{owner: owner, conversation: conversation, turn: turn, now: now, tap: tap,
		next: max(first, 1), subs: map[*Subscription]struct{}{}}
}

// Owner is the person whose turn the log numbers: a peer asking for its
// frames on somebody else's behalf is refused on it.
func (l *Log) Owner() uuid.UUID { return l.owner }

// Conversation and Turn name the log's turn.
func (l *Log) Conversation() uuid.UUID { return l.conversation }

// Turn is the turn the log numbers.
func (l *Log) Turn() uuid.UUID { return l.turn }

// Append stamps a frame's head (the protocol version, the turn, the next seq
// and the instant), validates it and its encoded size, retains it and hands it
// to every subscriber. The caller sets the kind and its payload only.
//
// A FRAME THAT DOES NOT VALIDATE IS REFUSED here, before it takes a seq, so a
// fault in the code that built it never leaves a hole in the numbering a
// client would read as a frame it missed.
func (l *Log) Append(f wire.Frame) (wire.Frame, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return wire.Frame{}, ErrEnded
	}
	f.V, f.ConversationID, f.TurnID = wire.ProtocolVersion, l.conversation, l.turn
	f.Seq, f.At = l.next, l.now().UTC()
	if err := f.Validate(); err != nil {
		return wire.Frame{}, err
	}
	encoded, err := json.Marshal(f)
	if err != nil {
		return wire.Frame{}, fmt.Errorf("live: encode a %s frame: %w", f.Kind, err)
	}
	if len(encoded) > wire.FrameMaxBytes {
		return wire.Frame{}, fmt.Errorf("%w: a %s frame encodes to %d bytes, over %d",
			wire.ErrInvalid, f.Kind, len(encoded), wire.FrameMaxBytes)
	}
	l.next++
	l.frames = append(l.frames, retained{frame: f, size: len(encoded)})
	l.bytes += len(encoded)
	for len(l.frames) > FrameLogFrames || l.bytes > FrameLogBytes {
		l.bytes -= l.frames[0].size
		l.frames = l.frames[1:]
	}
	for sub := range l.subs {
		sub.offer(f)
	}
	if l.tap != nil {
		l.tap(f)
	}
	return f, nil
}

// Last is the seq of the newest frame, zero before the first.
func (l *Log) Last() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.next - 1
}

// Subscribe is every retained frame after seq after, and a subscription to
// every frame appended from now on, taken in one step so no frame falls
// between the two. gap is true when frames after `after` are no longer
// retained: the caller answers a snapshot rather than a partial replay.
//
// A subscription to an ended log is closed at once, after the replay.
func (l *Log) Subscribe(after uint64) (replay []wire.Frame, sub *Subscription, gap bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	replay, gap = l.sinceLocked(after)
	sub = &Subscription{ch: make(chan wire.Frame, SubscriberFrames), log: l}
	if l.ended {
		sub.closeLocked()
		return replay, sub, gap
	}
	l.subs[sub] = struct{}{}
	return replay, sub, gap
}

// Since is every retained frame after seq after, whether frames after it are
// no longer retained (gap), and whether the segment has ended, read in one
// step: what a peer is answered when it asks for the frames its client missed.
func (l *Log) Since(after uint64) (frames []wire.Frame, gap, ended bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	frames, gap = l.sinceLocked(after)
	return frames, gap, l.ended
}

// sinceLocked is [Log.Since]'s frames and gap, under l.mu.
func (l *Log) sinceLocked(after uint64) (frames []wire.Frame, gap bool) {
	if len(l.frames) > 0 {
		gap = after+1 < l.frames[0].frame.Seq
	} else {
		gap = after+1 < l.next
	}
	for _, r := range l.frames {
		if r.frame.Seq > after {
			frames = append(frames, r.frame)
		}
	}
	return frames, gap
}

// End closes the segment: no frame is appended after it, and every
// subscription is closed once it has been handed what it holds.
func (l *Log) End() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return
	}
	l.ended, l.endAt = true, l.now()
	for sub := range l.subs {
		sub.closeLocked()
	}
	clear(l.subs)
}

// Ended reports whether the segment has ended.
func (l *Log) Ended() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ended
}

// expired reports whether an ended log has outlived [EndedRetention].
func (l *Log) expired(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ended && now.Sub(l.endAt) >= EndedRetention
}

// Subscription is one stream following a log.
type Subscription struct {
	ch  chan wire.Frame
	log *Log

	// overflowed and closed are guarded by log.mu.
	overflowed bool
	closed     bool
}

// C delivers the log's frames in order, and is closed when the segment ends,
// when the subscription falls [SubscriberFrames] behind, or when it is
// cancelled.
func (s *Subscription) C() <-chan wire.Frame { return s.ch }

// Overflowed reports whether the subscription was closed for falling behind:
// its client resumes from the last frame it drew rather than being handed a
// stream with a frame missing.
func (s *Subscription) Overflowed() bool {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	return s.overflowed
}

// Cancel stops the subscription. Safe to call more than once and after the
// log closed it.
func (s *Subscription) Cancel() {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	delete(s.log.subs, s)
	s.closeLocked()
}

// offer hands one frame over, under log.mu: a full buffer closes the
// subscription rather than dropping the frame, because a transcript rebuilt
// from deltas cannot survive a hole.
func (s *Subscription) offer(f wire.Frame) {
	if s.closed {
		return
	}
	select {
	case s.ch <- f:
	default:
		s.overflowed = true
		delete(s.log.subs, s)
		s.closeLocked()
	}
}

// closeLocked closes the channel once, under log.mu.
func (s *Subscription) closeLocked() {
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}
