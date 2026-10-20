package presence

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
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

	focus := Focus{Collection: "review", Doc: "row-12", Field: "status"}
	if err := a.hub.Update(room1, danaSession, Editing, focus); err != nil {
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

// A JOIN SHOWS THE PEERS' MEMBERS AS THEY ANSWER, not once the ask has run
// out its budget: a scatter has no roster, so the ask always runs its whole
// budget, and applying the answers only then showed a join everywhere after two
// seconds in spike S6, past the second presence allows.
//
// Mutation: collect the answers and apply them when the ask returns, and the
// newcomer waits the whole five seconds.
func TestAJoinShowsThePeersMembersAsTheyAnswer(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	a := startNode(t, broker, "node-a", c)
	if _, _, err := a.hub.Join(room1, dana); err != nil {
		t.Fatal(err)
	}
	a.seen.waitFor(t, "node-a", room1, "dana:viewing")
	settle()
	// A PEER THAT NEVER ANSWERS, as a slow or partitioned node does: the ask
	// then runs its whole budget, as it always does on the broker, where no
	// asker knows how many peers there are to wait for.
	silent := broker.Client()
	if err := silent.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Stop(context.Background()) })
	if _, err := silent.Serve(t.Context(), topics.PresenceAsk,
		func(ctx context.Context, _ []byte) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}); err != nil {
		t.Fatal(err)
	}
	b := startNodeAsking(t, broker, "node-b", c, 5*time.Second)
	started := time.Now()
	if _, _, err := b.hub.Join(room1, erin); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(Rebroadcast)
	for {
		if _, shown := b.seen.member(room1, "dana"); shown {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the newcomer's node never showed the peer's member")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("the peer's member was shown %s after the join, against the second a "+
			"join is allowed: the answer waited for the ask's five-second budget", took)
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

	mallory := Member{Session: "mallory-session", Seat: "mallory",
		Kind: iam.ActorHuman, Activity: Editing, At: c.Now()}
	forged := Frame{V: frameVersion, Kind: FrameState, Node: "node-x", Room: room1,
		Upsert: []Member{mallory}, Until: c.Now().Add(time.Minute)}
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
	malformed.Upsert = []Member{{Session: "m", Seat: "mallory",
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
// nothing has been heard for [Expiry] it is gone, with
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

	ghost := Member{Session: "ghost-session", Seat: "ghost",
		Kind: iam.ActorHuman, Activity: Viewing, At: c.Now()}
	heartbeat := func() {
		t.Helper()
		f := Frame{V: frameVersion, Kind: FrameState, Node: "node-gone", Room: room1,
			Upsert: []Member{ghost}, Full: true,
			Until: c.Now().Add(FrameValidity)}
		if err := b.queue.Broadcast(t.Context(), subject(room1), sealedFrom(t, fleetRing, f)); err != nil {
			t.Fatal(err)
		}
	}
	heartbeat()
	b.seen.waitFor(t, "node-b", room1, "erin:viewing", "ghost:viewing")

	// A REBROADCAST RENEWS IT: heard again at fifteen seconds.
	c.Advance(Rebroadcast)
	heartbeat()
	settle()
	c.Advance(Expiry - time.Second)
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
		focus := Focus{Region: "section-" + strconv.Itoa(i)}
		if err := a.hub.Update(room1, session, Editing, focus); err != nil {
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
	b.hub.Touch(room1, Agent{Seat: "agent-cto", Turn: "run-1", Activity: Editing, Focus: Focus{Collection: "review"}})
	a.seen.waitFor(t, "node-a", room1, "dana:viewing", "erin:viewing", "agent-cto:editing")
	if err := a.hub.Update(room1, session, Idle, Focus{}); err != nil {
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
	waitCounts(t, a.seen, room1, &Counts{Human: 9, Agent: 1})
	// THE FIFTY ARRIVE OVER SEVERAL FAN-OUTS, at most
	// [FrameMembers] members each.
	waitMembers(t, a.seen, room1, RoomTop)
	if _, shown := a.seen.member(room1, "seat-00"); shown {
		t.Fatal("the least recent member is presented in a room past the threshold")
	}
	if _, shown := a.seen.member(room1, "seat-59"); !shown {
		t.Fatal("the most recent member is not presented")
	}
	// A JOINER IS HANDED WHAT THE NODE'S SOCKETS ARE SHOWN — the fifty and
	// their counts — and its own member beside them, until the next fan-out
	// takes the least recent out as it does for every socket here.
	lateSession, roster, err := a.hub.Join(room1, Local{Seat: "late", Kind: iam.ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	holdsItself := slices.ContainsFunc(roster.Members, func(m Member) bool {
		return m.Session == lateSession
	})
	if !roster.Truncated || roster.Counts == nil || len(roster.Members) != RoomTop+1 ||
		!holdsItself {
		t.Fatalf("a joiner of a large room was handed %d members (itself among them: %v), "+
			"counts %+v, truncated %v", len(roster.Members), holdsItself, roster.Counts,
			roster.Truncated)
	}

	// DOWN TO 45: still the large-room shape, counting nobody hidden.
	for _, s := range sessions[:16] {
		a.hub.Leave(room1, s)
	}
	waitCounts(t, a.seen, room1, &Counts{})
	// AND BELOW 40: everybody again, with no counts at all.
	for _, s := range sessions[16:22] {
		a.hub.Leave(room1, s)
	}
	waitCounts(t, a.seen, room1, nil)
	waitMembers(t, a.seen, room1, 39)
}

// A FAN-OUT RE-PRESENTS AT MOST [FrameMembers] MEMBERS, the
// most recently active first, and the rest follow in the next fan-outs: a large
// room whose members all move does not send each socket every one of them in
// every frame.
//
// Mutation: drop the cap in room.diff, and the first fan-out after the burst
// carries all thirty.
func TestAFanOutRePresentsAtMostItsBoundOfMembers(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	a := startNode(t, broker, "node-a", c)
	var sessions []string
	for i := range 30 {
		s, _, err := a.hub.Join(room1, Local{Seat: fmt.Sprintf("seat-%02d", i), Kind: iam.ActorHuman})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	waitMembers(t, a.seen, room1, 30)
	var largest int
	a.hub.OnChange(func(ch Change) {
		a.seen.mu.Lock()
		largest = max(largest, len(ch.Upsert))
		a.seen.mu.Unlock()
	})
	c.Advance(time.Second)
	for i, s := range sessions {
		if err := a.hub.Update(room1, s, Editing,
			Focus{Region: fmt.Sprintf("r%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(10 * time.Second)
	for {
		edited := 0
		for _, m := range a.seen.members(room1) {
			if strings.HasSuffix(m, ":editing") {
				edited++
			}
		}
		if edited == len(sessions) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("%d of %d moves were shown", edited, len(sessions))
		case <-time.After(5 * time.Millisecond):
		}
	}
	a.seen.mu.Lock()
	defer a.seen.mu.Unlock()
	if largest > FrameMembers {
		t.Fatalf("one fan-out re-presented %d members, past the bound of %d", largest,
			FrameMembers)
	}
}

// A MEMBER HELD BACK IS TOLD WITHIN A FEW FAN-OUTS however many others keep
// moving: the change that has waited longest goes first. Ordered by recency
// alone, a viewer who moved once in a room where more than a frame's worth of
// others move all the time is never shown (spike S6's probe, missed on most of
// its moves after the frame bound landed).
//
// Mutation: order the held-back members by recency alone, and the probe's move
// is never shown.
func TestAHeldBackMemberIsToldWithinAFewFanOuts(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	a := startNode(t, broker, "node-a", c)
	var busy []string
	for i := range 2 * FrameMembers {
		s, _, err := a.hub.Join(room1, Local{Seat: fmt.Sprintf("busy-%02d", i), Kind: iam.ActorHuman})
		if err != nil {
			t.Fatal(err)
		}
		busy = append(busy, s)
	}
	probe, _, err := a.hub.Join(room1, Local{Seat: "probe", Kind: iam.ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	waitMembers(t, a.seen, room1, len(busy)+1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c.Advance(time.Millisecond)
			for _, s := range busy {
				_ = a.hub.Update(room1, s, Editing, Focus{Region: strconv.Itoa(i)})
			}
			time.Sleep(time.Millisecond)
		}
	}()
	defer func() { close(stop); <-done }()
	time.Sleep(5 * testCoalesce)
	if err := a.hub.Update(room1, probe, Editing, Focus{Region: "here"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(100 * testCoalesce)
	for {
		if m, ok := a.seen.member(room1, "probe"); ok && m.Focus.Region == "here" {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the probe's one move was never shown while the room kept moving")
		case <-time.After(time.Millisecond):
		}
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

	focus := Focus{Collection: "review", Doc: "row-3"}
	worker.hub.Touch(room1, Agent{Seat: "agent-cto", Turn: "run-1", Activity: Editing, Focus: focus})
	viewer.seen.waitFor(t, "the viewer's node", room1, "agent-cto:editing", "erin:viewing")
	if m, _ := viewer.seen.member(room1, "agent-cto"); m.Kind != iam.ActorAgent || m.Focus != focus ||
		m.Turn != "run-1" {
		t.Fatalf("the agent is shown as %+v, want editing row-3 in turn run-1", m)
	}
	worker.hub.mu.Lock()
	listening := len(worker.hub.listening)
	worker.hub.mu.Unlock()
	if listening != 0 {
		t.Fatal("a node whose only member is an agent listens on the room")
	}

	c.Advance(agentLinger - time.Second)
	worker.hub.Touch(room1, Agent{Seat: "agent-cto", Turn: "run-1", Activity: Viewing, Focus: focus})
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
		if err := h.Update(room1, session, Editing,
			Focus{Region: strconv.Itoa(i)}); err != nil {
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

// A MOVE CROSSES NODES IN ONE SHORT WINDOW ON EACH SIDE, at the production
// rates: the mover's node tells the fleet within a broadcast window and the
// viewer's node shows it within a fan-out window, however busy the viewer's
// room is. One window of a fifth of a second on each side measured a median
// near 240 ms in spike S6, against the 150 ms presence allows.
//
// Mutation: send the fleet frame on the sockets' fan-out window again, or set
// both rates back to five a second, and the median move crosses past 150 ms.
func TestAMoveCrossesNodesInOneShortWindowOnEachSide(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	hub := func(name string) (*Hub, *screen) {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
		h, err := New(Options{Queue: q, Wire: wireFor(t, fleetRing), Node: name,
			AskBudget: eventfan.FleetReadBudget, Now: time.Now})
		if err != nil {
			t.Fatal(err)
		}
		seen := newScreen()
		h.OnChange(seen.apply)
		if err := h.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Stop(context.Background()) })
		return h, seen
	}
	mover, _ := hub("node-a")
	viewer, viewed := hub("node-b")
	moving, _, err := mover.Join(room1, dana)
	if err != nil {
		t.Fatal(err)
	}
	busy, _, err := viewer.Join(room1, erin)
	if err != nil {
		t.Fatal(err)
	}
	waitListening(t, viewer, room1)
	waitMembers(t, viewed, room1, 2)
	// THE VIEWER'S ROOM IS BUSY: its own member moves all the time, so its
	// fan-out window is always running when the probe arrives.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(7 * time.Millisecond):
			}
			_ = viewer.Update(room1, busy, Editing, Focus{Region: strconv.Itoa(i)})
		}
	}()
	defer func() { close(stop); <-done }()
	// AND THE MOVER'S IS TOO, so its broadcast window is always running
	// when the probe moves.
	other, _, err := mover.Join(room1, Local{Seat: "frank", Kind: iam.ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	stopOther := make(chan struct{})
	doneOther := make(chan struct{})
	go func() {
		defer close(doneOther)
		for i := 0; ; i++ {
			select {
			case <-stopOther:
				return
			case <-time.After(7 * time.Millisecond):
			}
			_ = mover.Update(room1, other, Editing, Focus{Region: strconv.Itoa(i)})
		}
	}()
	defer func() { close(stopOther); <-doneOther }()
	var crossings []time.Duration
	for probe := range 16 {
		region := "probe-" + strconv.Itoa(probe)
		started := time.Now()
		if err := mover.Update(room1, moving, Viewing,
			Focus{Region: region}); err != nil {
			t.Fatal(err)
		}
		for {
			if m, ok := viewed.member(room1, "dana"); ok && m.Focus.Region == region {
				break
			}
			if time.Since(started) > 5*time.Second {
				t.Fatalf("probe %d never crossed", probe)
			}
			time.Sleep(time.Millisecond)
		}
		crossings = append(crossings, time.Since(started))
		time.Sleep(20 * time.Millisecond)
	}
	slices.Sort(crossings)
	if median := crossings[len(crossings)/2]; median > 150*time.Millisecond {
		t.Fatalf("the median of sixteen moves crossed in %s (all: %v): a broadcast window "+
			"and a fan-out window are 150 ms between them at their longest", median, crossings)
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
	if err := a.hub.Update(room1, "not-a-session", Editing, Focus{}); !errors.Is(err, ErrNotJoined) {
		t.Fatalf("a frame for a session never joined answered %v, want ErrNotJoined", err)
	}
	if err := a.hub.Update(ArtifactRoom(artifacts.IDFor("elsewhere")), session, Viewing, Focus{}); !errors.Is(err, ErrNotJoined) {
		t.Fatalf("a frame for another room answered %v, want ErrNotJoined", err)
	}
	if err := a.hub.Update(room1, session, "dancing", Focus{}); err == nil {
		t.Fatal("an activity outside the vocabulary was taken")
	}
	if err := a.hub.Update(room1, session, Viewing,
		Focus{Region: strings.Repeat("r", maxRegionBytes+1)}); err == nil {
		t.Fatal("a focus region past its bound was taken")
	}
	// A FRAME SAYING WHAT THE ROOM ALREADY SHOWS fans nothing out.
	a.seen.waitFor(t, "node-a", room1, "dana:viewing")
	settle()
	before := a.seen.pushCount()
	for range 5 {
		if err := a.hub.Update(room1, session, Viewing, Focus{}); err != nil {
			t.Fatal(err)
		}
	}
	settle()
	if got := a.seen.pushCount(); got != before {
		t.Fatalf("frames that changed nothing fanned the room out %d more times", got-before)
	}
	a.hub.Leave(room1, session)
	a.hub.Leave(room1, session)
	if err := a.hub.Update(room1, session, Viewing, Focus{}); !errors.Is(err, ErrNotJoined) {
		t.Fatalf("a frame after leaving answered %v, want ErrNotJoined", err)
	}
}

// waitListening waits until the hub's reconciler listens on the room.
func waitListening(t *testing.T, h *Hub, id Room) {
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

// waitMembers waits until the room shows n members.
func waitMembers(t *testing.T, s *screen, id Room, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		got := len(s.members(id))
		if got == n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the room shows %d members, want %d", got, n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// waitCounts waits until the room's last fan-out carried want.
func waitCounts(t *testing.T, s *screen, id Room, want *Counts) {
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

// A ROOM'S FAN-OUTS REACH ITS SINKS IN THE ORDER THEY WERE TAKEN.
//
// Each fan-out is the difference from the one before it, and two of a room are
// two timers: the second can fire while the first is still in its sinks. Taken
// then and handed over first, a member's leave reaches the sink ahead of the
// upsert taken before it, and what the sink shows ends with the member in the
// room — for good, since nothing says they left again.
//
// Mutation: drop [Hub.delivering] from flush, and the screen ends showing dana
// beside erin.
func TestARoomsFanOutsReachItsSinksInTheOrderTheyWereTaken(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	n := startNode(t, broker, "node-a", c)
	shown := newScreen()
	inSink, release, out := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	n.hub.OnChange(func(ch Change) {
		if ch.Room != room1 {
			return
		}
		if first.CompareAndSwap(false, true) {
			// THE FIRST FAN-OUT IS HELD in this sink, as a slow socket
			// hand-off would hold it, while the room moves on.
			close(inSink)
			<-release
			defer close(out)
		}
		shown.apply(ch)
	})

	danaSession, _, err := n.hub.Join(room1, dana)
	if err != nil {
		t.Fatal(err)
	}
	<-inSink
	if _, _, err := n.hub.Join(room1, erin); err != nil {
		t.Fatal(err)
	}
	n.hub.Leave(room1, danaSession)
	// LONG ENOUGH FOR THE NEXT FAN-OUT TO HAVE BEEN TAKEN AND HANDED OVER,
	// were it not waiting for the one in the sink.
	settle()
	close(release)
	<-out
	shown.waitFor(t, "the held sink", room1, "erin:viewing")
}

// A JOINER IS HANDED WHAT EVERY FAN-OUT AFTER ITS JOIN IS TAKEN FROM: what this
// node's sockets were last shown, and itself.
//
// A fan-out is the difference from what the sockets were last shown, so a
// joiner handed the room as it presented at the join holds a screen the next
// fan-out does not apply to: erin, who arrived after the last fan-out and left
// before the next, was in its roster and in no fan-out's leave, and stayed on
// its screen for good.
//
// Mutation: hand the joiner r.present() rather than the baseline, and the
// joiner's screen ends showing erin.
func TestAJoinersScreenIsRightFromItsRoster(t *testing.T) {
	t.Parallel()
	broker, c := memory.NewBroker(), newClock()
	n := startNode(t, broker, "node-a", c)
	joiner := newScreen()
	var joined atomic.Bool
	inSink, release, out := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	n.hub.OnChange(func(ch Change) {
		if ch.Room != room1 {
			return
		}
		if first.CompareAndSwap(false, true) {
			// THE FIRST FAN-OUT IS HELD in this sink, so nothing is fanned
			// out between erin's arrival and her leave.
			close(inSink)
			<-release
			defer close(out)
		}
		if joined.Load() {
			joiner.apply(ch)
		}
	})

	if _, _, err := n.hub.Join(room1, dana); err != nil {
		t.Fatal(err)
	}
	<-inSink
	erinSession, _, err := n.hub.Join(room1, erin)
	if err != nil {
		t.Fatal(err)
	}
	_, roster, err := n.hub.Join(room1, Local{Seat: "late", Kind: iam.ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	joiner.apply(Change{Room: room1, Upsert: roster.Members, Leave: []string{},
		Counts: roster.Counts, Truncated: roster.Truncated})
	joined.Store(true)
	n.hub.Leave(room1, erinSession)
	close(release)
	<-out

	want := []string{"dana:viewing", "late:viewing"}
	joiner.waitFor(t, "the joiner", room1, want...)
	settle()
	if got := joiner.members(room1); !slices.Equal(got, want) {
		t.Fatalf("the joiner shows %v once the room settled, want %v", got, want)
	}
}
