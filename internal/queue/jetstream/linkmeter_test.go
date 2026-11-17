package jetstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
// so a mebibyte takes eight seconds to cross each way — long enough that a
// meter reading four times a second judges spans of seconds inside one
// transfer.
const meterRate = 128 << 10

// meterInflight is what a first-in, first-out link lets each connection
// queue: half a second of [meterRate], well inside the ten seconds a member's
// leaf writes are given ([linktest.SharedFIFO]).
const meterInflight = 64 << 10

// meterSpan is the span the meter's cases ask whether the link is moving
// over.
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

// incompressibleMiB is a mebibyte of random bytes: what a compressing link
// carries whole.
func incompressibleMiB(t *testing.T) []byte {
	t.Helper()
	return incompressible(t, 1<<20)
}

// A LINK'S METER COUNTS WHAT CROSSES IT EACH WAY, at the leaf's own socket
// and never ahead of the link: a mebibyte published from the leaf, then a
// mebibyte delivered to it, across a first-in, first-out link whose relay's
// receive buffer is pinned. At every piece the link carries, decided by the
// link's own count rather than by a clock:
//
//   - what the meter says the member acknowledged is at most what the link
//     has read plus the relay's receive buffer — what a kernel takes and
//     acknowledges before the relay reads it, and all it can;
//   - what the meter says the leaf read is at most what the link has written
//     to the leaf;
//   - once a transfer has run for a span, the link reads as moving over it —
//     the upload by its acknowledged bytes alone, the delivery by its read
//     ones alone — at a floor played at half the link's rate;
//
// and each transfer is counted whole: the upload acknowledged once the member
// has it, the delivery read once the leaf has it.
//
// Mutant: no acknowledgement half (the meter counts reads alone) — the upload
// never moves and is never counted. Mutant: acknowledged = written (no kernel
// count, no send queue) — past what the link read and its buffer.
func TestALinksMeterCountsWhatCrossesItEachWay(t *testing.T) {
	t.Parallel()
	f := newMeterFleet(t, func(t testing.TB, target string) *linktest.Link {
		return linktest.SharedFIFO(t, target, meterRate, meterInflight)
	})
	f.meter.setFloor(meterRate / 2)
	rcv := uint64(f.link.Buffers().Receive)
	if rcv == 0 {
		t.Fatal("the link reports no receive buffer granted")
	}
	payload := incompressibleMiB(t)
	// warm is how far into a transfer the link must be before a span of it
	// can be asked about: the span, two of the meter's readings, and the
	// connection's queue on the link.
	warm := int64(meterRate*(meterSpan+2*meterPeriod)/time.Second) + meterInflight

	var (
		mu    sync.Mutex
		phase linktest.Way
		start int64
		moved int
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
		if c.Way != phase || c.Count.Carried-start < warm {
			return
		}
		moved++
		if !f.meter.Moving(meterSpan) {
			t.Errorf("%s, %d bytes into a transfer at the link's rate, the link reads still over %v",
				c.Way, c.Count.Carried-start, meterSpan)
		}
	})
	transfer := func(way linktest.Way, send func(), arrived <-chan []byte) {
		t.Helper()
		mu.Lock()
		phase, start, moved = way, f.link.Count(server.DEFAULT_GLOBAL_ACCOUNT, way).Carried, 0
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

// A LINK'S METER STANDS STILL ON A BLACKHOLED LINK: once the link reads
// nothing more of the leaf, a mebibyte the leaf publishes is written to its
// socket, and what the meter says the member acknowledged comes to rest at no
// more than the link had read and its receive buffer holds — below what was
// written, the rest held in the leaf's socket, unacknowledged — and over a
// span of readings at rest the link reads as still. Decided at the meter's
// own readings, by their count.
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

// A LEAF'S DATA LINK CROSSES ITS TUNNEL, every time it links: a leaf whose URLs
// name two members links to one of them through the tunnel, and when that
// member stops, links to the other through it again — so whatever member the
// leaf reaches, its meter reads the link. It is the guard on nats-server's
// proxy path for every bump of it, which the meter rests on.
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

// relinkLag is how late the relink case's link delivers every chunk, each
// way, once it has linked: 700 ms, so a leaf's handshake meets a round trip of
// 1.4 s — the member's INFO, the leaf's INFO and the member's again 2.1 s
// after the dial, past nats-server's one-second wait for them, and the leaf's
// CONNECT 2.8 s after the member took the connection, past its two-second
// wait — and stays under the fixed two seconds nats-server gives the
// member's answer to CONNECT, which no option moves ([leafHandshakeTerm]).
const relinkLag = 700 * time.Millisecond

// A LEAF LINKS AGAIN ACROSS A LINK WHOSE QUEUE IS DEEP: once its link drops
// while every byte either way crosses 700 ms late, as behind a bottleneck
// that other flows keep that deep, the leaf dials again through
// its tunnel, the handshake completes, and a request crosses the new link —
// decided by the request's answer, never by a clock. Left at nats-server's
// waits, every attempt timed out at the member's INFO or the leaf's CONNECT,
// and the leaf never linked again while the queue stayed deep.
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
// a queue on an external broker across a link meters every connection it
// dials as one link, so after the server restarts and the client dials again,
// what crosses the new connection is counted as the first's was, each way.
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
