package jetstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// meterRate is the pace the meter's cases hold a link to: 128 KiB a second,
// so a mebibyte takes eight seconds each way — spans of seconds inside one
// transfer for a meter reading four times a second.
const meterRate = 128 << 10

// meterInflight is what a first-in, first-out link lets each connection
// queue: half a second of [meterRate], well inside the ten seconds a member's
// leaf writes are given ([linktest.SharedFIFO]).
const meterInflight = 64 << 10

// meterSpan is the span the blackholed case asks whether the link stands still over.
const meterSpan = 2 * time.Second

// meterFleet is a member serving leaves, a link in front of its leaf
// listener, and a leaf joined to it across that link through its tunnel,
// with a connection to each that answers the other: the link's traffic in
// the leaf's global account is the data link's, which the leaf's meter reads.
type meterFleet struct {
	member, leaf testServer
	link         *linktest.Link
	meter        *LinkMeter
	// up is a connection to the leaf, down one to the member.
	up, down *nats.Conn
}

// newMeterFleet starts a meter fleet across the link open builds, and waits
// until a request each way is answered across it.
//
// THE MEMBER ADVERTISES THE LINK, which is opened with no target and then
// named the member's listener: a member advertising its own listener hands
// the leaf a URL it dials first on every redial ([linktest.Link]'s "The link
// is the only way").
func newMeterFleet(t *testing.T, open func(testing.TB, string) *linktest.Link) meterFleet {
	t.Helper()
	link := open(t, "")
	cfg := testTimings(Config{FleetAuth: testFleetAuth()})
	memberCfg := cfg
	memberCfg.ServerName = "member"
	memberCfg.LeafHost, memberCfg.LeafPort, memberCfg.LeafAdvertise = "127.0.0.1", AnyPort, link.Addr()
	memberCfg.StoreDir, memberCfg.StoreMaxBytes = brokertest.TempDir(t), brokertest.StoreMaxBytes
	member := startTestServer(t, memberCfg)
	link.To(fmt.Sprintf("127.0.0.1:%d", member.LeafPort()))

	leafCfg := cfg
	leafCfg.ServerName = "leaf"
	leafCfg.LeafURLs = []string{"nats-leaf://" + link.Addr()}
	leaf := startTestServer(t, leafCfg)
	f := meterFleet{member: member, leaf: leaf, link: link, meter: leaf.embedded.linkMeter()}
	if f.meter == nil {
		t.Fatal("a leaf has no meter of its link")
	}
	var err error
	if f.up, err = leaf.Conn(); err != nil {
		t.Fatalf("connect to the leaf: %v", err)
	}
	t.Cleanup(f.up.Close)
	if f.down, err = member.Conn(); err != nil {
		t.Fatalf("connect to the member: %v", err)
	}
	t.Cleanup(f.down.Close)
	for _, c := range []struct {
		name     string
		answers  *nats.Conn
		asks     *nats.Conn
		on, what string
	}{
		{"a request from the leaf", f.down, f.up, "meter.member.echo", "up"},
		{"a request from the member", f.up, f.down, "meter.leaf.echo", "down"},
	} {
		if _, err := c.answers.Subscribe(c.on, func(m *nats.Msg) { _ = m.Respond(m.Data) }); err != nil {
			t.Fatalf("answer %s: %v", c.on, err)
		}
		if err := c.answers.Flush(); err != nil {
			t.Fatal(err)
		}
		until(t, c.name+" answered across the link", func() bool {
			_, err := c.asks.Request(c.on, []byte(c.what), time.Second)
			return err == nil
		})
	}
	return f
}

// setFloor plays the floor at floor bytes a second.
func (m *LinkMeter) setFloor(floor float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.floor = floor
}

// written is what has been written to the link's sockets that are open.
func (m *LinkMeter) written() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n uint64
	for c := range m.conns {
		n += c.written.Load()
	}
	return n
}

// openedSockets is how many sockets the link has opened.
func (m *LinkMeter) openedSockets() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opened
}

// heldSockets is how many sockets the meter holds open.
func (m *LinkMeter) heldSockets() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.conns)
}

// incompressibleMiB is a mebibyte of random bytes: what a compressing link
// carries whole.
func incompressibleMiB(t *testing.T) []byte {
	t.Helper()
	return incompressible(t, 1<<20)
}

// A LINK'S METER COUNTS WHAT CROSSES IT EACH WAY, at the leaf's socket and
// never ahead of the link: a mebibyte up, then one down, across a FIFO link
// with a pinned receive buffer. At every piece the link carries: acknowledged
// <= its read plus that buffer; read <= what it wrote to the leaf; and past a
// span's warm-up the meter's counts, read on the link's clock (its carried
// bytes at its rate, which a loaded relay falls behind), clear the meter's
// line at a floor of half the rate: up by acknowledged bytes alone, down by
// read ones, once the leaf has read what it was handed. Each is counted whole.
//
// Mutant: no acknowledgement half — the upload never moves nor is counted.
// Mutant: acknowledged = written — past what the link read and its buffer.
func TestALinksMeterCountsWhatCrossesItEachWay(t *testing.T) {
	t.Parallel()
	f := newMeterFleet(t, func(t testing.TB, target string) *linktest.Link {
		return linktest.SharedFIFO(t, target, meterRate, meterInflight)
	})
	const floor = meterRate / 2
	rcv := uint64(f.link.Buffers().Receive)
	if rcv == 0 {
		t.Fatal("the link reports no receive buffer granted")
	}
	payload := incompressibleMiB(t)
	// ahead is how far the counts can stand from the link's: acknowledged up
	// to rcv past its read, read up to its inflight past what it carried, and
	// a window read but not yet acknowledged. span keeps the line clear of it
	// with a second to spare; warm also lets the connection's queue fill.
	ahead := 2*float64(rcv) + meterInflight
	span := time.Duration(ahead/(meterRate*(1-movingShare*floor/meterRate))*float64(time.Second)) + time.Second
	warm := int64(meterRate*span/time.Second) + meterInflight

	var (
		mu       sync.Mutex
		phase    linktest.Way
		start    int64
		base     uint64
		readings []reading
		moved    int
	)
	f.link.Watch(func(c linktest.Crossing) {
		if c.Account != server.DEFAULT_GLOBAL_ACCOUNT {
			return
		}
		switch c.Way {
		case linktest.ToBroker:
			acked, _ := f.meter.Carried()
			if read := f.link.Count(c.Account, c.Way).Read; acked > uint64(read)+rcv {
				t.Errorf("the meter reads %d bytes acknowledged where the link has read %d and its "+
					"receive buffer holds %d", acked, read, rcv)
			}
		case linktest.ToNode:
			_, read := f.meter.Carried()
			if carried := f.link.Count(c.Account, c.Way).Carried; read > uint64(carried) {
				t.Errorf("the meter reads %d bytes read where the link has written %d to the leaf",
					read, carried)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if c.Way != phase {
			return
		}
		into := c.Count.Carried - start
		if c.Way == linktest.ToNode && !leafRead(t.Context(), f.meter, base+uint64(into)) {
			return
		}
		acked, read := f.meter.Carried()
		readings = append(readings, reading{at: time.Unix(0, 0).Add(time.Duration(into) * time.Second / meterRate),
			acked: acked, read: read})
		if into < warm {
			return
		}
		moved++
		if !movingOver(readings, span, floor) {
			t.Errorf("%s, %d bytes into a transfer at the link's rate, the link reads still over %v of it",
				c.Way, into, span)
		}
	})
	transfer := func(way linktest.Way, send func(), arrived <-chan []byte) {
		t.Helper()
		mu.Lock()
		_, base = f.meter.Carried()
		phase, start, readings, moved = way, f.link.Count(server.DEFAULT_GLOBAL_ACCOUNT, way).Carried, nil, 0
		mu.Unlock()
		send()
		bound := time.NewTimer(2 * time.Minute) //fixedwait:liveness
		defer bound.Stop()
		select {
		case got := <-arrived:
			if !bytes.Equal(got, payload) {
				t.Fatalf("%s: %d bytes arrived, not the mebibyte sent", way, len(got))
			}
		case <-bound.C:
			t.Fatalf("%s: the mebibyte never arrived", way)
		}
		mu.Lock()
		defer mu.Unlock()
		if moved == 0 {
			t.Fatalf("%s: no piece crossed past the span's warm-up, so moving was never asked", way)
		}
		phase = ""
	}

	// UP: the leaf publishes, the member hears.
	acked0, _ := f.meter.Carried()
	upArrived := make(chan []byte, 1)
	if _, err := f.down.Subscribe("meter.up", func(m *nats.Msg) { upArrived <- m.Data }); err != nil {
		t.Fatal(err)
	}
	if err := f.down.Flush(); err != nil {
		t.Fatal(err)
	}
	until(t, "the member's interest across the link", func() bool {
		_, err := f.up.Request("meter.member.echo", nil, time.Second)
		return err == nil
	})
	transfer(linktest.ToBroker, func() {
		if err := f.up.Publish("meter.up", payload); err != nil {
			t.Fatal(err)
		}
	}, upArrived)
	until(t, "the member's acknowledgement of the whole upload", func() bool {
		acked, _ := f.meter.Carried()
		return acked-acked0 >= uint64(len(payload))
	})

	// DOWN: the member publishes, the leaf hears.
	_, read0 := f.meter.Carried()
	downArrived := make(chan []byte, 1)
	if _, err := f.up.Subscribe("meter.down", func(m *nats.Msg) { downArrived <- m.Data }); err != nil {
		t.Fatal(err)
	}
	if err := f.up.Flush(); err != nil {
		t.Fatal(err)
	}
	until(t, "the leaf's interest across the link", func() bool {
		_, err := f.down.Request("meter.leaf.echo", nil, time.Second)
		return err == nil
	})
	transfer(linktest.ToNode, func() {
		if err := f.down.Publish("meter.down", payload); err != nil {
			t.Fatal(err)
		}
	}, downArrived)
	if _, read := f.meter.Carried(); read-read0 < uint64(len(payload)) {
		t.Errorf("the leaf heard a mebibyte and the meter read %d bytes off its link", read-read0)
	}
	// No piece the teardown carries is asked about.
	f.link.Watch(nil)
}

// leafRead reports whether the meter has read want bytes off the leaf's link,
// asking until it has or ctx ends.
func leafRead(ctx context.Context, m *LinkMeter, want uint64) bool {
	if _, read := m.Carried(); read >= want {
		return true
	}
	poll := time.NewTicker(5 * time.Millisecond) //fixedwait:liveness
	defer poll.Stop()
	for {
		select {
		case <-poll.C:
		case <-ctx.Done():
			return false
		}
		if _, read := m.Carried(); read >= want {
			return true
		}
	}
}

// A LINK'S METER STANDS STILL ON A BLACKHOLED LINK: a mebibyte the leaf
// publishes once the link reads nothing more is written to its socket, the
// acknowledged count comes to rest at no more than the link had read plus its
// receive buffer, below what was written, and over a span of readings at rest
// the link reads still. Decided at the meter's own readings, by their count.
//
// Mutant: acknowledged = written — as much as was written, past the link's
// read and its buffer.
func TestALinksMeterStandsStillOnABlackholedLink(t *testing.T) {
	t.Parallel()
	f := newMeterFleet(t, func(t testing.TB, target string) *linktest.Link {
		return linktest.SharedFIFO(t, target, meterRate, meterInflight)
	})
	f.meter.setFloor(meterRate / 2)
	rcv := uint64(f.link.Buffers().Receive)
	// THE MEMBER LISTENS, so the leaf forwards the publish across the link
	// at all: a leaf sends a hub only what somebody there wants.
	if _, err := f.down.Subscribe("meter.blackholed", func(*nats.Msg) {}); err != nil {
		t.Fatal(err)
	}
	if err := f.down.Flush(); err != nil {
		t.Fatal(err)
	}
	until(t, "the member's interest across the link", func() bool {
		_, err := f.up.Request("meter.member.echo", nil, time.Second)
		return err == nil
	})
	f.link.Blackhole(server.DEFAULT_GLOBAL_ACCOUNT)
	read := uint64(f.link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToBroker).Read)
	payload := incompressibleMiB(t)
	if err := f.up.Publish("meter.blackholed", payload); err != nil {
		t.Fatal(err)
	}

	// AT REST: the acknowledged count unchanged over a span's readings, with
	// more written than acknowledged — the publish reached the socket.
	still := int(meterSpan/meterPeriod) + 1
	bound := time.NewTimer(2 * time.Minute) //fixedwait:liveness
	defer bound.Stop()
	var acked uint64
	for same := 0; same < still; {
		next := f.meter.nextReading()
		select {
		case <-next:
		case <-bound.C:
			t.Fatalf("the meter's acknowledged count never came to rest on a blackholed link "+
				"(last %d, written %d)", acked, f.meter.written())
		}
		now, _ := f.meter.Carried()
		if now == acked && f.meter.written() > now {
			same++
		} else {
			same = 0
		}
		acked = now
	}
	if acked > read+rcv {
		t.Errorf("the meter reads %d bytes acknowledged across a link that read %d before it went "+
			"dark and whose receive buffer holds %d", acked, read, rcv)
	}
	if f.meter.Moving(meterSpan) {
		t.Errorf("a link whose meter stood still for %d readings reads moving over %v", still, meterSpan)
	}
}

// A LEAF'S DATA LINK CROSSES ITS TUNNEL, every time it links: a leaf naming two
// members links to one through the tunnel and, when that one stops, to the
// other through it again, so its meter reads whichever it reaches. The guard
// on the nats-server proxy path the meter rests on, for every bump of it.
//
// Mutant: no Proxy on the leaf's remote — the leaf links, and its meter never
// opens a socket.
func TestALeafsDataLinkCrossesItsTunnel(t *testing.T) {
	t.Parallel()
	cfg := testTimings(Config{FleetAuth: testFleetAuth()})
	members := make([]testServer, 2)
	var urls []string
	for i := range members {
		memberCfg := cfg
		memberCfg.ServerName = fmt.Sprintf("member-%d", i)
		memberCfg.LeafHost, memberCfg.LeafPort = "127.0.0.1", AnyPort
		memberCfg.StoreDir, memberCfg.StoreMaxBytes = brokertest.TempDir(t), brokertest.StoreMaxBytes
		members[i] = startTestServer(t, memberCfg)
		urls = append(urls, fmt.Sprintf("nats-leaf://127.0.0.1:%d", members[i].LeafPort()))
	}
	leafCfg := cfg
	leafCfg.ServerName = "leaf"
	leafCfg.LeafURLs = urls
	leaf := startTestServer(t, leafCfg)
	meter := leaf.embedded.linkMeter()

	on := func() int {
		for i, m := range members {
			if m.embedded != nil && m.embedded.ns.NumLeafNodes() > 0 {
				return i
			}
		}
		return -1
	}
	until(t, "the leaf's link to a member", func() bool { return on() >= 0 })
	first := on()
	opened := meter.openedSockets()
	if opened == 0 {
		t.Fatalf("the leaf linked to member %d and its tunnel opened no socket to it", first)
	}
	members[first].stop()
	other := 1 - first
	until(t, "the leaf's link to the other member, through its tunnel", func() bool {
		return members[other].embedded.ns.NumLeafNodes() > 0 && meter.openedSockets() > opened
	})
}

// relinkLag is how late the relink case's link delivers every chunk each way:
// 700 ms, a 1.4 s round trip, so the INFO exchange ends 2.1 s after the dial
// (past nats-server's one-second wait) and the leaf's CONNECT lands 2.8 s
// after the accept (past its two-second wait), yet under the fixed two seconds
// given the member's answer to CONNECT, which no option moves
// ([leafHandshakeTerm]).
const relinkLag = 700 * time.Millisecond

// A LEAF LINKS AGAIN ACROSS A LINK WHOSE QUEUE IS DEEP: once its link drops
// while every byte crosses 700 ms late each way, the leaf dials again through
// its tunnel, the handshake completes, and a request crosses the new link —
// decided by the answer, never a clock. At nats-server's default waits every
// attempt times out at the member's INFO or the leaf's CONNECT.
//
// Mutant: the leaf's wait for the member's INFO left at its one second.
// Mutant: the member's wait for the leaf's CONNECT left at its two seconds.
func TestALeafLinksAgainAcrossALinkWhoseQueueIsDeep(t *testing.T) {
	t.Parallel()
	f := newMeterFleet(t, linktest.Lagging)
	opened := f.meter.openedSockets()
	f.link.Lag(relinkLag)
	f.link.Drop(server.DEFAULT_GLOBAL_ACCOUNT)

	bound := time.NewTimer(90 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	for {
		// A REQUEST'S WAIT HOLDS ITS ROUND TRIP AND ONE MORE, so an answer
		// that crosses is never lost to it.
		_, err := f.up.Request("meter.member.echo", []byte("again"), 4*relinkLag)
		if err == nil && f.meter.openedSockets() > opened {
			return
		}
		select {
		case <-bound.C:
			t.Fatalf("the leaf never linked again across a link lagging %v each way: the last "+
				"request said %v, and the tunnel opened %d sockets to the members (%d before the drop)",
				relinkLag, err, f.meter.openedSockets(), opened)
		default:
		}
	}
}

// A LEAF WHOSE TUNNEL STOPS ACCEPTING IS A DEAD BROKER: the tunnel declares
// the death the broker's own, which the engine ends the node on
// ([Server.Death]), rather than leave a leaf that can never link again.
//
// Mutant: the tunnel handed a death of its own — the broker's never closes.
func TestALeafWhoseTunnelStopsAcceptingIsADeadBroker(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	nowhere := ln.Addr().String()
	_ = ln.Close()
	cfg := testTimings(Config{FleetAuth: testFleetAuth()})
	cfg.ServerName = "leaf"
	cfg.LeafURLs = []string{"nats-leaf://" + nowhere}
	leaf := startTestServer(t, cfg)
	_ = leaf.embedded.tunnel.ln.Close()
	bound := time.NewTimer(10 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	select {
	case <-leaf.Death().Done():
	case <-bound.C:
		t.Fatal("a leaf whose tunnel stopped accepting is not dead")
	}
	if err := leaf.Death().Err(); !errors.Is(err, ErrBrokerDied) || !strings.Contains(err.Error(), "tunnel") {
		t.Errorf("the broker's death says %v, want ErrBrokerDied naming the tunnel", err)
	}
}

// AN EXTERNAL DIAL'S METER COUNTS WHAT CROSSES IT, its reconnects included:
// every connection a queue on an external broker dials is metered as one
// link, so after a server restart the new connection is counted each way too.
//
// Mutant: a reconnect handed to the client unmetered.
func TestAnExternalDialsMeterCountsWhatCrossesIt(t *testing.T) {
	t.Parallel()
	srv := externaltest.Start(t, queue.MaxPayloadBytes)
	link := linktest.SharedFIFO(t, srv.HostPort(), 1<<20, meterInflight)
	q, err := Open(t.Context(), Config{URL: "nats://" + link.Addr(), Token: externaltest.Token})
	if err != nil {
		t.Fatalf("a queue on an external broker across a link did not open: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	meter := q.LinkMeter()
	if meter == nil {
		t.Fatal("a queue on an external broker has no meter of its link")
	}
	peer, err := nats.Connect(srv.URL(), nats.Token(externaltest.Token))
	if err != nil {
		t.Fatalf("connect to the external server: %v", err)
	}
	t.Cleanup(peer.Close)
	const size = 256 << 10
	payload := incompressible(t, size)

	crossing := func(round int) {
		t.Helper()
		acked0, read0 := meter.Carried()
		up := fmt.Sprintf("meter.ext.up.%d", round)
		heard, err := peer.SubscribeSync(up)
		if err != nil {
			t.Fatal(err)
		}
		if err := peer.Flush(); err != nil {
			t.Fatal(err)
		}
		if err := q.Conn().Publish(up, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := heard.NextMsg(time.Minute); err != nil {
			t.Fatalf("round %d: the peer never heard the queue's publish: %v", round, err)
		}
		until(t, fmt.Sprintf("round %d: the publish acknowledged at the queue's socket", round), func() bool {
			acked, _ := meter.Carried()
			return acked-acked0 >= size
		})
		down := fmt.Sprintf("meter.ext.down.%d", round)
		mine, err := q.Conn().SubscribeSync(down)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Conn().Flush(); err != nil {
			t.Fatal(err)
		}
		if err := peer.Publish(down, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := mine.NextMsg(time.Minute); err != nil {
			t.Fatalf("round %d: the queue never heard the peer's publish: %v", round, err)
		}
		if _, read := meter.Carried(); read-read0 < size {
			t.Errorf("round %d: the queue heard %d bytes and its meter read %d", round, size, read-read0)
		}
	}
	crossing(0)
	// THE SERVER RESTARTED under the queue, which dials it again across the
	// link: a connection the client opened for itself, not the dial's.
	opened := meter.openedSockets()
	srv.Restart(queue.MaxPayloadBytes, q.Conn())
	until(t, "the queue's connection dialled again and the peer's", func() bool {
		return meter.openedSockets() > opened && peer.IsConnected()
	})
	crossing(1)
}

// AN EXTERNAL DIAL CLOSES THE SOCKET ITS CLIENT GAVE UP: nats.go's reconnect
// loop moves past a failed handshake without closing its socket, which a
// metered dialer would hold for ever — a descriptor, and one more socket read
// at every reading, per failed attempt. Three dials through one dialer: the
// two handed out before are closed at their far end, and the meter holds one.
//
// Mutant: the dialer keeps what it handed out before — the first two stay
// open, and the meter holds three.
func TestAnExternalDialClosesTheSocketItsClientGaveUp(t *testing.T) {
	t.Parallel()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 3)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
			accepted <- c
		}
	}()
	meter := newLinkMeter()
	t.Cleanup(meter.close)
	d := newInfoDialer(externalHandshake, meter)
	servers := make([]net.Conn, 3)
	for i := range servers {
		c, err := d.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		servers[i] = <-accepted
	}
	for i, s := range servers[:2] {
		closedWithin(t, s, fmt.Sprintf("the socket of dial %d, which the client gave up", i))
	}
	if n := meter.heldSockets(); n != 1 {
		t.Errorf("the meter holds %d sockets after three dials, want the one the client has", n)
	}
}

// A METERED WRITE IS COUNTED AS ITS SOCKET TAKES IT, never only once it
// returns: nats.go flushes a large message as one Write, and where the kernel
// counts no acknowledged bytes (darwin) the meter reads written less what the
// socket holds, so a Write counted whole reads as nothing acknowledged while
// it crosses. 64 MiB to a peer that reads nothing blocks and is counted while
// it does; once the peer reads it all, the Write is counted whole.
//
// Mutant: the Write handed to the socket whole — nothing is counted while it
// blocks.
func TestAMeteredWriteIsCountedAsItsSocketTakesIt(t *testing.T) {
	t.Parallel()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer, ok := <-accepted
	if !ok {
		t.Fatal("the peer's socket was never accepted")
	}
	t.Cleanup(func() { _ = peer.Close() })
	meter := newLinkMeter()
	t.Cleanup(meter.close)
	c := meter.wrap(client)
	t.Cleanup(func() { _ = c.Close() })

	const size = 64 << 20
	wrote := make(chan error, 1)
	go func() {
		_, err := c.Write(make([]byte, size))
		wrote <- err
	}()
	until(t, "the blocked write counted as its socket took it", func() bool {
		select {
		case err := <-wrote:
			t.Fatalf("a write of %d bytes to a peer that reads nothing returned (%v)", size, err)
		default:
		}
		return meter.written() > 0
	})
	if n, err := io.CopyN(io.Discard, peer, size); err != nil {
		t.Fatalf("the peer read %d of %d bytes: %v", n, size, err)
	}
	if err := <-wrote; err != nil {
		t.Fatalf("the write: %v", err)
	}
	if n := meter.written(); n != size {
		t.Errorf("the meter counted %d bytes of a %d-byte write", n, size)
	}
}
