package presence

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fleetRing is the keyring every node of a test fleet holds.
var fleetRing = statelog.OneKey("k1", "the presence suite's fleet key material")

// clock is a settable clock a hub stamps and expires by. Fan-outs are still
// timed on the real clock, briefly ([testCoalesce]).
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testCoalesce is the fan-out interval a suite runs at: short, so a case waits
// milliseconds for a room to fan out rather than the production fifth of a
// second.
const testCoalesce = 10 * time.Millisecond

func wireFor(t *testing.T, ring statelog.Keyring) *Wire {
	t.Helper()
	w, err := signedask.NewOneWay[Frame](Channel, ring)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// node is one hub on a shared in-memory broker, and what its sockets were told.
type node struct {
	hub   *Hub
	queue *memory.Queue
	seen  *screen
	clock *clock
}

// startNode starts a hub with the engine's own ask budget. Not a shorter one:
// a room fills from its peers on that ask alone until the next rebroadcast,
// which is later than a case waits, and an answer applies the moment it
// arrives, so a longer budget slows nothing that passes.
func startNode(t *testing.T, broker *memory.Broker, name string, c *clock) *node {
	t.Helper()
	return startNodeAsking(t, broker, name, c, eventfan.FleetReadBudget)
}

// startNodeAsking is [startNode] with the join-time ask given askBudget.
func startNodeAsking(t *testing.T, broker *memory.Broker, name string, c *clock, askBudget time.Duration) *node {
	t.Helper()
	q := broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	h, err := New(Options{Queue: q, Wire: wireFor(t, fleetRing), Node: name,
		AskBudget: askBudget, Now: c.Now, Coalesce: testCoalesce})
	if err != nil {
		t.Fatal(err)
	}
	n := &node{hub: h, queue: q, seen: newScreen(), clock: c}
	h.OnChange(n.seen.apply)
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Stop(context.Background()) })
	return n
}

// screen is what one node's sockets show of each room: every fan-out applied
// in order, as a dashboard applies them.
type screen struct {
	mu      sync.Mutex
	rooms   map[Room]map[string]Member
	counts  map[Room]*Counts
	pushes  int
	changed chan struct{}
}

func newScreen() *screen {
	return &screen{rooms: map[Room]map[string]Member{},
		counts: map[Room]*Counts{}, changed: make(chan struct{}, 1)}
}

func (s *screen) apply(c Change) {
	s.mu.Lock()
	room := s.rooms[c.Room]
	if room == nil {
		room = map[string]Member{}
		s.rooms[c.Room] = room
	}
	for _, m := range c.Upsert {
		room[m.Session] = m
	}
	for _, session := range c.Leave {
		delete(room, session)
	}
	s.counts[c.Room] = c.Counts
	s.pushes++
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// members is the room as shown: each member's seat and activity, sorted.
func (s *screen) members(id Room) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.rooms[id] {
		out = append(out, m.Seat+":"+string(m.Activity))
	}
	slices.Sort(out)
	return out
}

func (s *screen) member(id Room, seat string) (Member, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.rooms[id] {
		if m.Seat == seat {
			return m, true
		}
	}
	return Member{}, false
}

func (s *screen) pushCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pushes
}

// waitFor waits until the room shows exactly want.
func (s *screen) waitFor(t *testing.T, who string, id Room, want ...string) {
	t.Helper()
	slices.Sort(want)
	if want == nil {
		want = []string{}
	}
	deadline := time.After(10 * time.Second)
	for {
		got := s.members(id)
		if got == nil {
			got = []string{}
		}
		if slices.Equal(got, want) {
			return
		}
		select {
		case <-s.changed:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("%s shows %v in the room, want %v", who, got, want)
		}
	}
}

// settle waits long enough for a fan-out that must NOT happen to have had its
// chance: many coalescing intervals on the real clock.
func settle() { time.Sleep(30 * testCoalesce) }

var (
	room1 = ArtifactRoom(artifacts.IDFor("presence-suite-room-1"))
	dana  = Local{Seat: "dana", Kind: iam.ActorHuman}
	erin  = Local{Seat: "erin", Kind: iam.ActorHuman}
)

// sealedFrom is a frame from a node that is not a hub here, sealed under ring:
// how a case speaks as a peer, honest or not.
func sealedFrom(t *testing.T, ring statelog.Keyring, f Frame) []byte {
	t.Helper()
	sealed, err := wireFor(t, ring).Seal(f)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// subject is a room's presence subject.
func subject(id Room) string { return id.subject() }
