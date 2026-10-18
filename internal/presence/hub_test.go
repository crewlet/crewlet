package presence

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A ROOM CROSSES THE FLEET: two nodes, a socket on each, and each node's
// sockets come to show both people, what each is doing, and the one who left.
// The second node learns the first's member from its join-time ask and every
// change after from the broadcast; nothing is forwarded.
func TestARoomCrossesTheFleet(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	a := startNode(t, broker, "node-a", c)
	b := startNode(t, broker, "node-b", c)

	danaSession, roster, err := a.hub.Join(room1, dana)
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Members) != 1 || roster.Members[0].Session != danaSession ||
		roster.Members[0].Seat != "dana" || roster.Members[0].Kind != iam.ActorHuman {
		t.Fatalf("the joiner was handed %+v, want a roster holding itself", roster)
	}
	a.seen.waitFor(t, "node-a", room1, "dana:viewing")

	if _, _, err := b.hub.Join(room1, erin); err != nil {
		t.Fatal(err)
	}
	b.seen.waitFor(t, "node-b", room1, "dana:viewing", "erin:viewing")
	a.seen.waitFor(t, "node-a", room1, "dana:viewing", "erin:viewing")

	focus := artifacts.Focus{Collection: "review", Doc: "row-12", Field: "status"}
	if err := a.hub.Update(room1, danaSession, artifacts.PresenceEditing, focus); err != nil {
		t.Fatal(err)
	}
	b.seen.waitFor(t, "node-b", room1, "dana:editing", "erin:viewing")
	if m, _ := b.seen.member(room1, "dana"); m.Focus != focus {
		t.Fatalf("node-b shows dana focused on %+v, want %+v", m.Focus, focus)
	}

	a.hub.Leave(room1, danaSession)
	b.seen.waitFor(t, "node-b", room1, "erin:viewing")
	// AND NODE-A, WITH NOBODY LEFT IN THE ROOM, HOLDS IT NO LONGER: what
	// its peers say there is heard by nobody here.
	deadline := time.After(10 * time.Second)
	for {
		a.hub.mu.Lock()
		_, held := a.hub.rooms[room1]
		_, listening := a.hub.listening[room1]
		a.hub.mu.Unlock()
		if !held && !listening {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("node-a still holds the room (%v) or listens on it (%v) with nobody in it",
				held, listening)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A NODE THAT STOPS TELLS THE FLEET ITS MEMBERS LEFT, so a peer drops them at
// once rather than showing them for the whole expiry.
func TestAStoppingNodeTakesItsMembersWithIt(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	a := startNode(t, broker, "node-a", c)
	b := startNode(t, broker, "node-b", c)
	if _, _, err := a.hub.Join(room1, dana); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.hub.Join(room1, erin); err != nil {
		t.Fatal(err)
	}
	b.seen.waitFor(t, "node-b", room1, "dana:viewing", "erin:viewing")
	a.hub.Stop(context.Background())
	b.seen.waitFor(t, "node-b", room1, "erin:viewing")
	if _, _, err := a.hub.Join(room1, dana); !errors.Is(err, ErrStopped) {
		t.Fatalf("a join on a stopped hub answered %v, want ErrStopped", err)
	}
}

// A PRESENCE FRAME THE FLEET DID NOT SEAL SHOWS NOBODY: not a frame at all,
// one sealed under another fleet's key, one sealed in another key domain, one
// past its expiry and one whose member is malformed each leave the room as it
// was, while the same frame sealed by the fleet shows its member (which is
// what proves the case could fail).
func TestAPresenceFrameTheFleetDidNotSealShowsNobody(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	a := startNode(t, broker, "node-a", c)
	if _, _, err := a.hub.Join(room1, dana); err != nil {
		t.Fatal(err)
	}
	a.seen.waitFor(t, "node-a", room1, "dana:viewing")
	// THE LISTEN IS THE RECONCILER'S, so wait for it before speaking.
	waitListening(t, a.hub, room1)

	mallory := artifacts.PresenceMember{Session: "mallory-session", Seat: "mallory",
		Kind: iam.ActorHuman, Activity: artifacts.PresenceEditing, At: c.Now()}
	forged := Frame{V: frameVersion, Kind: FrameState, Node: "node-x", Artifact: room1,
		Upsert: []artifacts.PresenceMember{mallory}, Until: c.Now().Add(time.Minute)}
	elsewhere, err := signedask.NewOneWay[Frame]("artifacts.other", fleetRing)
	if err != nil {
		t.Fatal(err)
	}
	otherDomain, err := elsewhere.Seal(forged)
	if err != nil {
		t.Fatal(err)
	}
	expired := forged
	expired.Until = c.Now().Add(-time.Second)
	malformed := forged
	malformed.Upsert = []artifacts.PresenceMember{{Session: "m", Seat: "mallory",
		Kind: iam.ActorHuman, Activity: "dancing"}}
	unsealed := []byte(`{"v":1,"kind":"state","node":"node-x","artifact":"` + room1.String() +
		`","upsert":[{"session":"mallory-session","seat":"mallory","kind":"human",` +
		`"activity":"editing"}],"until":"2099-01-01T00:00:00Z"}`)

	sender := broker.Client()
	if err := sender.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sender.Stop(context.Background()) }()
	for name, payload := range map[string][]byte{
		"not sealed at all":          unsealed,
		"another fleet's key":        sealedFrom(t, statelog.OneKey("k1", "not this fleet's"), forged),
		"another channel's domain":   otherDomain,
		"past its expiry":            sealedFrom(t, fleetRing, expired),
		"a member no build can show": sealedFrom(t, fleetRing, malformed),
	} {
		if err := sender.Broadcast(t.Context(), subject(room1), payload); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	settle()
	if got := a.seen.members(room1); len(got) != 1 || got[0] != "dana:viewing" {
		t.Fatalf("frames the fleet did not seal changed the room to %v, want only dana", got)
	}

	// AND THE SAME FRAME SEALED BY THE FLEET IS SHOWN: the case can fail.
	if err := sender.Broadcast(t.Context(), subject(room1), sealedFrom(t, fleetRing, forged)); err != nil {
		t.Fatal(err)
	}
	a.seen.waitFor(t, "node-a", room1, "dana:viewing", "mallory:editing")
}

// PRESENCE EXPIRES AFTER THREE MISSED HEARTBEATS: a peer's member heard once
// is still shown at forty-four seconds, a rebroadcast renews it, and once
// nothing has been heard for [artifacts.PresenceExpiry] it is gone, with
// nothing to clean up anywhere. A node's own members never expire this way:
// they leave when their socket does.
func TestPresenceExpiresAfterThreeMissedHeartbeats(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	b := startNode(t, broker, "node-b", c)
	if _, _, err := b.hub.Join(room1, erin); err != nil {
		t.Fatal(err)
	}
	waitListening(t, b.hub, room1)

	ghost := artifacts.PresenceMember{Session: "ghost-session", Seat: "ghost",
		Kind: iam.ActorHuman, Activity: artifacts.PresenceViewing, At: c.Now()}
	heartbeat := func() {
		t.Helper()
		f := Frame{V: frameVersion, Kind: FrameState, Node: "node-gone", Artifact: room1,
			Upsert: []artifacts.PresenceMember{ghost}, Full: true,
			Until: c.Now().Add(artifacts.PresenceFrameValidity)}
		if err := b.queue.Broadcast(t.Context(), subject(room1), sealedFrom(t, fleetRing, f)); err != nil {
			t.Fatal(err)
		}
	}
	heartbeat()
	b.seen.waitFor(t, "node-b", room1, "erin:viewing", "ghost:viewing")

	// A REBROADCAST RENEWS IT: heard again at fifteen seconds.
	c.Advance(artifacts.PresenceRebroadcast)
	heartbeat()
	settle()
	c.Advance(artifacts.PresenceExpiry - time.Second)
	b.hub.tick(t.Context(), c.Now())
	settle()
	b.seen.waitFor(t, "node-b", room1, "erin:viewing", "ghost:viewing")

	// THREE MISSED: forty-five seconds since it was last heard.
	c.Advance(time.Second)
	b.hub.tick(t.Context(), c.Now())
	b.seen.waitFor(t, "node-b", room1, "erin:viewing")
}

// A BROADCAST IS NEVER RETAINED: what a node said while nobody was listening
// is heard by no listener that arrives later, and the broker holds no record
// of it and no subscription for it.
func TestABroadcastIsNeverRetained(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	a := startNode(t, broker, "node-a", c)
	session, _, err := a.hub.Join(room1, dana)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		focus := artifacts.Focus{Region: "section-" + strconv.Itoa(i)}
		if err := a.hub.Update(room1, session, artifacts.PresenceEditing, focus); err != nil {
			t.Fatal(err)
		}
		settle()
	}

	late := broker.Client()
	if err := late.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = late.Stop(context.Background()) }()
	var heard atomic.Int32
	stop, err := late.Listen(t.Context(), subject(room1), func(string, []byte) { heard.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	settle()
	if n := heard.Load(); n != 0 {
		t.Fatalf("a listener that arrived after the room's frames heard %d of them: a "+
			"retained presence frame is a heartbeat replayed after its moment", n)
	}
	if n := len(late.History()); n != 0 {
		t.Fatalf("the broker recorded %d published events for a room's presence", n)
	}
	subs, err := late.ListSubscriptions(t.Context(), ">")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 0 {
		t.Fatalf("presence left durable subscriptions on the broker: %v", subs)
	}
	// AND WHAT IS SAID NOW IS HEARD: the listener was live all along.
	a.hub.tick(t.Context(), c.Now())
	deadline := time.After(10 * time.Second)
	for heard.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the late listener never heard the room's rebroadcast")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// PRESENCE WRITES NO ROW ANYWHERE: a whole room's life across two nodes, an
// agent's included, publishes nothing durable on the broker, and the package
// imports nothing that could write a row, a log record or a coordination key.
func TestPresenceWritesNoRowAnywhere(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	a := startNode(t, broker, "node-a", c)
	b := startNode(t, broker, "node-b", c)
	session, _, err := a.hub.Join(room1, dana)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.hub.Join(room1, erin); err != nil {
		t.Fatal(err)
	}
	b.hub.Touch(room1, "agent-cto", artifacts.PresenceEditing, artifacts.Focus{Collection: "review"})
	a.seen.waitFor(t, "node-a", room1, "dana:viewing", "erin:viewing", "agent-cto:editing")
	if err := a.hub.Update(room1, session, artifacts.PresenceIdle, artifacts.Focus{}); err != nil {
		t.Fatal(err)
	}
	a.hub.tick(t.Context(), c.Now())
	b.hub.tick(t.Context(), c.Now())
	a.hub.Leave(room1, session)
	b.seen.waitFor(t, "node-b", room1, "erin:viewing", "agent-cto:editing")

	if n := len(a.queue.History()); n != 0 {
		t.Fatalf("a room's life published %d durable events", n)
	}
	subs, err := a.queue.ListSubscriptions(t.Context(), ">")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 0 {
		t.Fatalf("a room's life left durable subscriptions: %v", subs)
	}

	// NOTHING DURABLE IS EVEN REACHABLE: the package's own imports name no
	// store, no coordination client, no state log and no event stream.
	forbidden := []string{"internal/store", "internal/coord", "internal/events",
		"internal/estate", `/internal/statelog"`}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		read++
		for _, imp := range file.Imports {
			for _, bad := range forbidden {
				if strings.Contains(imp.Path.Value, bad) {
					t.Errorf("%s imports %s: presence is told, never stored", name, imp.Path.Value)
				}
			}
		}
	}
	if read == 0 {
		t.Fatal("no source file was read, so this gate could not fail")
	}
}

// A LARGE ROOM PRESENTS ITS MOST RECENT FIFTY and counts the rest by kind,
// and presents everybody again only below forty: a room hovering at the
// threshold does not flip on every join.
func TestALargeRoomPresentsTheMostRecentFifty(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	a := startNode(t, broker, "node-a", c)
	var sessions []string
	for i := range 60 {
		c.Advance(time.Second)
		who := Local{Seat: fmt.Sprintf("seat-%02d", i), Kind: iam.ActorHuman}
		if i%10 == 9 {
			who.Kind = iam.ActorAgent
		}
		s, _, err := a.hub.Join(room1, who)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	waitCounts(t, a.seen, room1, &artifacts.PresenceCounts{Human: 9, Agent: 1})
	if got := len(a.seen.members(room1)); got != artifacts.PresenceRoomTop {
		t.Fatalf("a room of 60 presents %d members, want %d", got, artifacts.PresenceRoomTop)
	}
	if _, shown := a.seen.member(room1, "seat-00"); shown {
		t.Fatal("the least recent member is presented in a room past the threshold")
	}
	if _, shown := a.seen.member(room1, "seat-59"); !shown {
		t.Fatal("the most recent member is not presented")
	}
	_, roster, err := a.hub.Join(room1, Local{Seat: "late", Kind: iam.ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	if !roster.Truncated || roster.Counts == nil || len(roster.Members) != artifacts.PresenceRoomTop {
		t.Fatalf("a joiner of a large room was handed %d members, counts %+v, truncated %v",
			len(roster.Members), roster.Counts, roster.Truncated)
	}

	// DOWN TO 45: still the large-room shape, counting nobody hidden.
	for _, s := range sessions[:16] {
		a.hub.Leave(room1, s)
	}
	waitCounts(t, a.seen, room1, &artifacts.PresenceCounts{})
	// AND BELOW 40: everybody again, with no counts at all.
	for _, s := range sessions[16:22] {
		a.hub.Leave(room1, s)
	}
	waitCounts(t, a.seen, room1, nil)
	if got := len(a.seen.members(room1)); got != 39 {
		t.Fatalf("a room of 39 presents %d members, want all of them", got)
	}
}

// AN AGENT IS PRESENT WHILE IT WORKS: its tool call puts its seat in the room
// as an agent, editing where the call wrote, on every node watching; it stays
// through its next call and leaves once it has been quiet for the linger. A
// node holding only agents in a room does not listen on it.
func TestAnAgentIsPresentWhileItWorks(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	worker := startNode(t, broker, "node-seats", c)
	viewer := startNode(t, broker, "node-api", c)
	if _, _, err := viewer.hub.Join(room1, erin); err != nil {
		t.Fatal(err)
	}
	waitListening(t, viewer.hub, room1)

	focus := artifacts.Focus{Collection: "review", Doc: "row-3"}
	worker.hub.Touch(room1, "agent-cto", artifacts.PresenceEditing, focus)
	viewer.seen.waitFor(t, "the viewer's node", room1, "agent-cto:editing", "erin:viewing")
	if m, _ := viewer.seen.member(room1, "agent-cto"); m.Kind != iam.ActorAgent || m.Focus != focus {
		t.Fatalf("the agent is shown as %+v", m)
	}
	worker.hub.mu.Lock()
	listening := len(worker.hub.listening)
	worker.hub.mu.Unlock()
	if listening != 0 {
		t.Fatal("a node whose only member is an agent listens on the room")
	}

	c.Advance(agentLinger - time.Second)
	worker.hub.Touch(room1, "agent-cto", artifacts.PresenceViewing, focus)
	viewer.seen.waitFor(t, "the viewer's node", room1, "agent-cto:viewing", "erin:viewing")
	c.Advance(agentLinger - time.Second)
	worker.hub.tick(t.Context(), c.Now())
	settle()
	if _, shown := viewer.seen.member(room1, "agent-cto"); !shown {
		t.Fatal("an agent touched within the linger left the room")
	}
	c.Advance(time.Second)
	worker.hub.tick(t.Context(), c.Now())
	viewer.seen.waitFor(t, "the viewer's node", room1, "erin:viewing")
}

// A ROOM FANS OUT AT MOST ITS COALESCED RATE however fast its members move.
func TestARoomFansOutAtMostItsCoalescedRate(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	q := broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Stop(context.Background()) }()
	const interval = 200 * time.Millisecond
	h, err := New(Options{Queue: q, Wire: wireFor(t, fleetRing), Node: "node-a",
		AskBudget: time.Second, Now: time.Now, Coalesce: interval})
	if err != nil {
		t.Fatal(err)
	}
	seen := newScreen()
	h.OnChange(seen.apply)
	session, _, err := h.Join(room1, dana)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	for i := 0; time.Since(started) < 3*interval; i++ {
		if err := h.Update(room1, session, artifacts.PresenceEditing,
			artifacts.Focus{Region: strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	elapsed := time.Since(started)
	time.Sleep(2 * interval)
	h.Stop(context.Background())
	if most := int(elapsed/interval) + 2; seen.pushCount() > most {
		t.Fatalf("a room fanned out %d times in %s, more than the %d its rate allows",
			seen.pushCount(), elapsed, most)
	}
}

// A SOCKET STATES ONLY WHAT IT IS DOING AND WHERE, inside the vocabulary's
// bounds, for a session it holds.
func TestAPresenceFrameIsHeldToItsSessionAndBounds(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	a := startNode(t, broker, "node-a", c)
	session, _, err := a.hub.Join(room1, dana)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.hub.Update(room1, "not-a-session", artifacts.PresenceEditing, artifacts.Focus{}); !errors.Is(err, ErrNotJoined) {
		t.Fatalf("a frame for a session never joined answered %v, want ErrNotJoined", err)
	}
	if err := a.hub.Update(artifacts.IDFor("elsewhere"), session, artifacts.PresenceViewing, artifacts.Focus{}); !errors.Is(err, ErrNotJoined) {
		t.Fatalf("a frame for another room answered %v, want ErrNotJoined", err)
	}
	if err := a.hub.Update(room1, session, "dancing", artifacts.Focus{}); err == nil {
		t.Fatal("an activity outside the vocabulary was taken")
	}
	if err := a.hub.Update(room1, session, artifacts.PresenceViewing,
		artifacts.Focus{Region: strings.Repeat("r", maxRegionBytes+1)}); err == nil {
		t.Fatal("a focus region past its bound was taken")
	}
	a.hub.Leave(room1, session)
	a.hub.Leave(room1, session)
	if err := a.hub.Update(room1, session, artifacts.PresenceViewing, artifacts.Focus{}); !errors.Is(err, ErrNotJoined) {
		t.Fatalf("a frame after leaving answered %v, want ErrNotJoined", err)
	}
}

// waitListening waits until the hub's reconciler listens on the room.
func waitListening(t *testing.T, h *Hub, id artifacts.ID) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		h.mu.Lock()
		_, ok := h.listening[id]
		h.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the hub never listened on the room")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// waitCounts waits until the room's last fan-out carried want.
func waitCounts(t *testing.T, s *screen, id artifacts.ID, want *artifacts.PresenceCounts) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		s.mu.Lock()
		got := s.counts[id]
		s.mu.Unlock()
		if (got == nil && want == nil) || (got != nil && want != nil && *got == *want) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the room's counts are %+v, want %+v", got, want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
