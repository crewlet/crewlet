package artifactspike

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/queue"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/signedask"
)

// SPIKE S6: presence fan-out, over the real hubs on a three-member fleet.
//
// Each node runs the shipped presence hub on its own member's core NATS (the
// queue's Broadcast and Listen) and the shipped socket hub, with the room's
// sockets registered and joined as /ws/stream registers and joins them, and
// every presence fan-out pushed to the room exactly as the socket service
// pushes it. A socket here is everything but the websocket write: its frames
// are the encoded bytes a browser would receive, counted as they leave the
// hub's queue. Viewers are spread evenly over the three nodes and each moves
// its focus at a set rate; the spike reports what the busiest node sends its
// sockets, what crosses the broker, the CPU the whole fleet spent, how long a
// join takes to show on every node, and how long a focus change takes to
// reach a socket on another node (the presence objectives' two events).
//
// Run: go test -run '^$' -bench S6 -benchtime 1x ./internal/artifactspike

const (
	// presenceWindow is how long a run measures, after every viewer has
	// joined and every node shows the whole room: twenty probes.
	presenceWindow = 20 * time.Second

	// presenceProbeEvery is how often the latency probe moves its focus.
	//
	// LONGER THAN THE WHOLE PATH CAN TAKE: a focus change waits out the
	// sending node's coalescing window and then the receiving node's (each
	// a second over [presence.CoalesceRate]) before a socket sees
	// it, and a probe moved again inside that path is coalesced away and
	// never shown at all, which would read as a lost frame rather than a
	// slow one.
	presenceProbeEvery = time.Second
)

// BenchmarkS6PresenceFanOut runs a room of 30, 50 and 200 viewers, each
// moving its focus once a second (an active reader) and ten times a second
// (the per-socket ceiling, [presence.ClientRate]).
func BenchmarkS6PresenceFanOut(b *testing.B) {
	for _, viewers := range []int{30, 50, 200} {
		for _, rate := range []int{1, presence.ClientRate} {
			b.Run(fmt.Sprintf("viewers=%d/moves-per-s=%d", viewers, rate), func(b *testing.B) {
				for range b.N {
					fanOut(b, viewers, rate)
				}
			})
		}
	}
}

// countingQueue is a node's broker as its presence hub uses it, counting the
// frames and bytes it broadcasts and hears.
type countingQueue struct {
	q                       *js.Queue
	sentFrames, sentBytes   atomic.Int64
	heardFrames, heardBytes atomic.Int64
}

func (c *countingQueue) Broadcast(ctx context.Context, subject string, payload []byte) error {
	c.sentFrames.Add(1)
	c.sentBytes.Add(int64(len(payload)))
	return c.q.Broadcast(ctx, subject, payload)
}

func (c *countingQueue) Listen(ctx context.Context, pattern string, h queue.ListenFunc) (queue.Unsubscribe, error) {
	return c.q.Listen(ctx, pattern, func(subject string, payload []byte) {
		c.heardFrames.Add(1)
		c.heardBytes.Add(int64(len(payload)))
		h(subject, payload)
	})
}

func (c *countingQueue) AskEach(ctx context.Context, subject string, request []byte, each func([]byte) bool) error {
	return c.q.AskEach(ctx, subject, request, each)
}

func (c *countingQueue) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	return c.q.Serve(ctx, subject, h)
}

// presenceNode is one node's presence: its hub, its socket hub, and the room
// as its sockets were shown it.
type presenceNode struct {
	queue   *countingQueue
	hub     *presence.Hub
	sockets *stream.Hub
	screen  *roomScreen

	pushFrames, pushBytes atomic.Int64
	clients               int
}

// roomScreen is the room as one node's sockets show it, every fan-out applied
// in order, and the instant a watched focus region first appeared on it.
type roomScreen struct {
	mu      sync.Mutex
	members map[string]presence.Member
	counts  *presence.Counts
	seen    map[string]time.Time
}

func (s *roomScreen) apply(c presence.Change, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range c.Upsert {
		s.members[m.Session] = m
		if _, ok := s.seen[m.Focus.Region]; !ok && m.Focus.Region != "" {
			s.seen[m.Focus.Region] = at
		}
	}
	for _, session := range c.Leave {
		delete(s.members, session)
	}
	s.counts = c.Counts
}

// shown is how many members the screen accounts for: those it presents, and
// for a room presenting only its most recent, the ones counted beside them.
func (s *roomScreen) shown() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.members)
	if s.counts != nil {
		n += s.counts.Human + s.counts.Agent
	}
	return n
}

func (s *roomScreen) seenAt(region string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.seen[region]
	return at, ok
}

// startPresence starts one node's hubs on q.
func startPresence(b *testing.B, q *js.Queue, id string) *presenceNode {
	b.Helper()
	wire, err := signedask.NewOneWay[presence.Frame](presence.Channel, spikeRing)
	if err != nil {
		b.Fatalf("%s: build the presence wire: %v", id, err)
	}
	n := &presenceNode{queue: &countingQueue{q: q}, sockets: stream.NewHub(),
		screen: &roomScreen{members: map[string]presence.Member{}, seen: map[string]time.Time{}}}
	// THE FLEET'S READ BUDGET is what the engine hands the join-time ask;
	// two seconds is the API's own read deadline.
	n.hub, err = presence.New(presence.Options{Queue: n.queue, Wire: wire, Node: id,
		AskBudget: 2 * time.Second})
	if err != nil {
		b.Fatalf("%s: build the presence hub: %v", id, err)
	}
	n.hub.OnChange(func(c presence.Change) {
		now := time.Now()
		n.screen.apply(c, now)
		// AS THE SOCKET SERVICE PUSHES IT (stream.Service.PresenceChanged).
		artifact := artifacts.ID(c.Room.ID)
		n.sockets.Broadcast(stream.PushArtifact(stream.KindArtifactPresence, artifact,
			stream.ArtifactPresence{Artifact: artifact, Upsert: c.Upsert, Leave: c.Leave,
				Counts: c.Counts, Truncated: c.Truncated}, now))
	})
	if err := n.hub.Start(b.Context()); err != nil {
		b.Fatalf("%s: start the presence hub: %v", id, err)
	}
	b.Cleanup(func() {
		n.hub.Stop(context.WithoutCancel(b.Context()))
		n.sockets.Close()
	})
	return n
}

// viewer is one socket in the room.
type viewer struct {
	node    *presenceNode
	session string
}

// join registers a socket on node, joins it to the room in both hubs, and
// drains its frames for the life of the run, counting them.
func (n *presenceNode) join(b *testing.B, id artifacts.ID, seat string, done <-chan struct{}) viewer {
	b.Helper()
	c := stream.NewClient(stream.NewAudience(iam.Principal{Login: seat, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAdmin}}, nil))
	n.sockets.Register(c)
	session, _, err := n.hub.Join(presence.ArtifactRoom(id),
		presence.Local{Seat: seat, Kind: iam.ActorHuman})
	if err != nil {
		b.Fatalf("join %s to the room: %v", seat, err)
	}
	if _, _, ok := n.sockets.JoinArtifact(c, id, artifacts.RoleViewer, session); !ok {
		b.Fatalf("join %s's socket to the room: the hub does not hold it", seat)
	}
	n.clients++
	go func() {
		for {
			select {
			case f := <-c.Out():
				if f.Kind() == stream.KindArtifactPresence {
					n.pushFrames.Add(1)
					n.pushBytes.Add(int64(len(f.Raw())))
				}
			case <-done:
				return
			}
		}
	}()
	return viewer{node: n, session: session}
}

// fanOut is one presence run.
func fanOut(b *testing.B, viewers, rate int) {
	b.Helper()
	b.StopTimer()
	br := startBrokers(b, topoFleet)
	nodes := make([]*presenceNode, len(br.queues))
	for i, q := range br.queues {
		nodes[i] = startPresence(b, q, fmt.Sprintf("node-%d", i))
	}
	id := artifacts.IDFor(fmt.Sprintf("spike presence %d %d", viewers, rate))
	done := make(chan struct{})
	defer close(done)

	// THE JOINS, every viewer at once, and how long until every node shows
	// the whole room: the join objective's event, at its worst.
	joinStart := time.Now()
	room := make([]viewer, viewers)
	for v := range room {
		room[v] = nodes[v%len(nodes)].join(b, id, fmt.Sprintf("member-%02d", v%len(spikeSeats)), done)
	}
	probeNode, watchNode := nodes[0], nodes[len(nodes)-1]
	probe := probeNode.join(b, id, "member-63", done)
	settle := time.Now().Add(time.Minute)
	for _, n := range nodes {
		for n.screen.shown() < viewers+1 {
			if time.Now().After(settle) {
				b.Fatalf("a node shows %d of %d members a minute after the joins",
					n.screen.shown(), viewers+1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	joined := time.Since(joinStart)

	pushFrames0, pushBytes0 := make([]int64, len(nodes)), make([]int64, len(nodes))
	sent0, heard0 := make([]int64, len(nodes)), make([]int64, len(nodes))
	for i, n := range nodes {
		pushFrames0[i], pushBytes0[i] = n.pushFrames.Load(), n.pushBytes.Load()
		sent0[i], heard0[i] = n.queue.sentBytes.Load(), n.queue.heardBytes.Load()
	}
	cpu0 := cpuTime(b)
	b.StartTimer()
	start := time.Now()
	stop := start.Add(presenceWindow)
	var wg sync.WaitGroup
	for v, vw := range room {
		wg.Go(func() {
			rnd := rand.New(rand.NewPCG(uint64(v), 17))
			interval := time.Second / time.Duration(rate)
			// A RANDOM PHASE, so the viewers' moves spread over the
			// interval rather than land together.
			time.Sleep(time.Duration(rnd.Int64N(int64(interval))))
			for time.Now().Before(stop) {
				focus := presence.Focus{Collection: sheetCollection,
					Doc: docID(rnd.IntN(seededRows)), Field: "status"}
				if err := vw.node.hub.Update(presence.ArtifactRoom(id), vw.session, presence.Viewing, focus); err != nil {
					b.Errorf("a viewer's focus: %v", err)
					return
				}
				time.Sleep(interval)
			}
		})
	}
	// THE PROBE: a focus change on the first node, timed to the moment the
	// last node's sockets are shown it.
	var cursor latencies
	missed := 0
	for k := 0; time.Now().Add(presenceProbeEvery).Before(stop); k++ {
		region := fmt.Sprintf("probe-%d", k)
		moved := time.Now()
		if err := probe.node.hub.Update(presence.ArtifactRoom(id), probe.session, presence.Editing,
			presence.Focus{Region: region}); err != nil {
			b.Fatalf("the probe's focus: %v", err)
		}
		time.Sleep(presenceProbeEvery)
		if at, ok := watchNode.screen.seenAt(region); ok {
			cursor.add(at.Sub(moved))
		} else {
			missed++
		}
	}
	wg.Wait()
	elapsed := time.Since(start)
	b.StopTimer()
	cpu := cpuTime(b) - cpu0

	// THE BUSIEST NODE is the one with the most sockets; every node fans
	// out the same changes, so its socket traffic is the most any node sends.
	busiest := 0
	for i, n := range nodes {
		if n.clients > nodes[busiest].clients {
			busiest = i
		}
	}
	bn := nodes[busiest]
	pushFrames := float64(bn.pushFrames.Load()-pushFrames0[busiest]) / elapsed.Seconds()
	pushBytes := float64(bn.pushBytes.Load()-pushBytes0[busiest]) / elapsed.Seconds()
	var sent, heard float64
	for i, n := range nodes {
		sent += float64(n.queue.sentBytes.Load()-sent0[i]) / elapsed.Seconds()
		heard += float64(n.queue.heardBytes.Load()-heard0[i]) / elapsed.Seconds()
	}
	b.ReportMetric(pushFrames/float64(bn.clients), "push-frames/s/socket")
	b.ReportMetric(pushBytes/1024, "busiest-node-socket-KiB/s")
	b.ReportMetric(sent/1024/float64(len(nodes)), "broker-sent-KiB/s/node")
	b.ReportMetric(heard/1024/float64(len(nodes)), "broker-heard-KiB/s/node")
	b.ReportMetric(cpu.Seconds()/elapsed.Seconds(), "fleet-cpu-cores")
	b.ReportMetric(ms(joined), "join-all-ms")
	cursor.report(b, "cursor")
	b.ReportMetric(withinShare(&cursor, objectives.PresenceCursorWithin), "cursor-within-objective")
	b.ReportMetric(float64(missed), "probe-missed")
}
