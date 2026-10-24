package presence

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// judged is an objective meter that keeps what it was handed.
type judged struct {
	mu      sync.Mutex
	joins   []joinEvent
	cursors []time.Time
	other   []objectives.Name
}

type joinEvent struct {
	elapsed  time.Duration
	answered bool
}

func (j *judged) Record(n objectives.Name, elapsed time.Duration, answered bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if n == objectives.PresenceJoin {
		j.joins = append(j.joins, joinEvent{elapsed, answered})
		return
	}
	j.other = append(j.other, n)
}

func (j *judged) RecordSince(n objectives.Name, stamp time.Time, _ bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if n == objectives.PresenceCursor {
		j.cursors = append(j.cursors, stamp)
		return
	}
	j.other = append(j.other, n)
}

func (j *judged) snapshot() ([]joinEvent, []time.Time, []objectives.Name) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.joins), slices.Clone(j.cursors), slices.Clone(j.other)
}

// replying is the broker as a hub uses it, with every reply to an ask held to
// onReply first: how a case moves the asking node's clock between an ask
// going out and its answer landing.
type replying struct {
	Queue
	onReply func()
}

func (r replying) AskEach(ctx context.Context, subject string, request []byte,
	each func(reply []byte) bool) error {
	return r.Queue.AskEach(ctx, subject, request, func(reply []byte) bool {
		r.onReply()
		return each(reply)
	})
}

// broadcasting is the broker as a hub uses it, counting the frames the hub
// has handed it: how a case knows a node's own move went out before another
// node starts listening for it.
type broadcasting struct {
	Queue
	sent *atomic.Int64
}

func (b broadcasting) Broadcast(ctx context.Context, subject string, payload []byte) error {
	err := b.Queue.Broadcast(ctx, subject, payload)
	b.sent.Add(1)
	return err
}

// startJudged is a node whose objectives j records, over q, lapsing a typing
// line after lapse.
func startJudged(t *testing.T, q Queue, name string, c *clock, j *judged,
	lapse time.Duration) *node {
	t.Helper()
	h, err := New(Options{Queue: q, Wire: wireFor(t, fleetRing), Node: name,
		AskBudget: 300 * time.Millisecond, Now: c.Now, Coalesce: testCoalesce,
		TypingLapse: lapse, Objectives: j})
	if err != nil {
		t.Fatal(err)
	}
	n := &node{hub: h, seen: newScreen(), clock: c}
	h.OnChange(n.seen.apply)
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Stop(context.Background()) })
	return n
}

func startedClient(t *testing.T, broker *memory.Broker) *memory.Queue {
	t.Helper()
	q := broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	return q
}

func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A JOIN IS TIMED FROM ITS FIRST SOCKET TO ITS PEERS' MEMBERS SHOWN, ONCE.
//
// A node's first socket in a room makes it listen and ask its peers, and the
// room is whole once the last answer lands — or, where nobody answers, once
// the ask has gone out. That instant against the first socket's is the join,
// measured on the node's own clock. A second socket joins a room the node
// already listens on and is shown everybody at once: no event.
//
// Mutation: time the join from the ask going out rather than from the answer
// applied, and dana's join reads as instant.
func TestAJoinIsTimedFromItsFirstSocketToItsPeersShown(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	clockA, clockB := newClock(), newClock()
	judgeA, judgeB := &judged{}, &judged{}
	a := startJudged(t, startedClient(t, broker), "node-a", clockA, judgeA, testLapse)
	answerTakes := 1200 * time.Millisecond
	b := startJudged(t, replying{Queue: startedClient(t, broker),
		onReply: func() { clockB.Advance(answerTakes) }}, "node-b", clockB, judgeB, testLapse)

	if _, _, err := joined(a.hub, room1, erin); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "node-a's own join timed", func() bool {
		joins, _, _ := judgeA.snapshot()
		return len(joins) == 1
	})
	if joins, _, _ := judgeA.snapshot(); joins[0] != (joinEvent{0, true}) {
		t.Errorf("a join nobody answered was timed %+v, want shown as its ask went out", joins[0])
	}

	if _, _, err := joined(b.hub, room1, dana); err != nil {
		t.Fatal(err)
	}
	b.seen.waitFor(t, "node-b", room1, "dana:viewing", "erin:viewing")
	waitUntil(t, "node-b's join timed", func() bool {
		joins, _, _ := judgeB.snapshot()
		return len(joins) == 1
	})
	if joins, _, _ := judgeB.snapshot(); joins[0] != (joinEvent{answerTakes, true}) {
		t.Errorf("node-b's join was timed %+v, want %s: from its first socket to the "+
			"answer that showed it erin", joins[0], answerTakes)
	}

	if _, _, err := joined(b.hub, room1, Local{Seat: "fay", Kind: dana.Kind}); err != nil {
		t.Fatal(err)
	}
	b.seen.waitFor(t, "node-b", room1, "dana:viewing", "erin:viewing", "fay:viewing")
	settle()
	if joins, _, other := judgeB.snapshot(); len(joins) != 1 || len(other) != 0 {
		t.Errorf("a join into a room node-b listens on was timed: %+v, %v", joins, other)
	}
}

// A MOVE IS TIMED FROM ITS OWN NODE'S STAMP TO THE FRAME HEARD, AND A ROSTER
// RESTATED IS NO MOVE.
//
// A peer's frame names the members that moved, each stamped by the node that
// holds it at the move — the stamp a lapse takes too, so a typing line that
// lapsed is timed from the lapse rather than from the keystroke six seconds
// before it. A rebroadcast restates every member and moves none.
//
// Mutation: time a full roster as moves, or leave the lapse unstamped, and the
// stamps heard are not the moves'.
func TestAMoveIsTimedFromItsStampAndARosterRestatedIsNone(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	clockA, clockB := newClock(), newClock()
	judgeB := &judged{}
	// A LAPSE LONG ENOUGH for the case to move node-a's clock between the
	// keystroke reaching node-b and the lapse, on a loaded runner too.
	lapse := 1500 * time.Millisecond
	sentA := new(atomic.Int64)
	a := startJudged(t, broadcasting{Queue: startedClient(t, broker), sent: sentA},
		"node-a", clockA, &judged{}, lapse)
	b := startJudged(t, startedClient(t, broker), "node-b", clockB, judgeB, lapse)
	chat := ChatRoom(uuid.NewSHA1(uuid.NameSpaceURL, []byte("presence-objectives-chat")))

	sessionA, _, err := joined(a.hub, chat, erin)
	if err != nil {
		t.Fatal(err)
	}
	// ERIN'S JOIN GOES OUT BEFORE NODE-B LISTENS. Sent after, node-b hears it
	// after its ask's answer has already shown erin, and times that join as
	// one more move of erin's than the case counts: a loaded runner held
	// node-a's frame past node-b's ask and failed the case on exactly that.
	waitUntil(t, "node-a's join of erin sent", func() bool { return sentA.Load() > 0 })
	if _, _, err := joined(b.hub, chat, dana); err != nil {
		t.Fatal(err)
	}
	b.seen.waitFor(t, "node-b", chat, "dana:viewing", "erin:viewing")
	_, before, _ := judgeB.snapshot()

	clockA.Advance(time.Minute)
	typed := clockA.Now()
	if err := a.hub.Update(chat, sessionA, Editing, Focus{}); err != nil {
		t.Fatal(err)
	}
	b.seen.waitFor(t, "node-b", chat, "dana:viewing", "erin:editing")
	clockA.Advance(5 * time.Second)
	lapsedAt := clockA.Now()
	b.seen.waitFor(t, "node-b", chat, "dana:viewing", "erin:viewing")

	_, cursors, _ := judgeB.snapshot()
	if got := cursors[len(before):]; !slices.Equal(got, []time.Time{typed, lapsedAt}) {
		t.Errorf("node-b timed erin's moves from %v, want the keystroke's %v and the "+
			"lapse's %v", got, typed, lapsedAt)
	}

	a.hub.tick(t.Context(), clockA.Now())
	settle()
	if _, after, _ := judgeB.snapshot(); len(after) != len(cursors) {
		t.Errorf("a rebroadcast was timed as %d moves", len(after)-len(cursors))
	}
}
