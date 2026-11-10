package jetstreamtest

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// forwarder is one member's view of one peer: a listener this member dials,
// which proxies to the peer's real route port until it is stopped.
//
// # Why a forwarder per ORDERED PAIR
//
// A partition is not symmetric to build even when it is symmetric in effect.
// Member 0 reaches member 1 through 0's own forwarder for 1, and member 1
// reaches member 0 through 1's forwarder for 0 — two different listeners. With
// one forwarder per pair, stopping it would cut both directions from a single
// switch, which cannot express the asymmetric case a real network produces (a
// member that can send and not receive), and cannot cut ONE member off from
// the cluster without also cutting the cluster off from itself.
//
// For n = 3 that is six forwarders, which is the number the design note names.
type forwarder struct {
	from, to int
	target   string

	mu       sync.Mutex
	listener net.Listener
	port     int
	// served is closed when the goroutine serving the current listener has
	// returned, which it does once the listener closes: from then on, every
	// connection that listener accepted is counted below.
	served chan struct{}
	// live are the connections currently proxied through this forwarder.
	// They are CLOSED on a partition, which is the half a stopped listener
	// does not cover: NATS holds its routes open, so refusing new
	// connections leaves an established route working indefinitely.
	live   map[net.Conn]struct{}
	closed bool
	// carried is how many connections this forwarder has dialled through to
	// its peer since its listener last opened, and open how many of the
	// connections it accepted have not ended — still dialling the peer, or
	// proxied. They are [Cluster.drain]'s, which accounts for every dial one
	// member makes to another.
	carried, open int
	// emptied is closed when open reaches zero after a drain asked, and nil
	// while none waits.
	emptied chan struct{}
}

// Partition cuts member i off from every peer, in both directions.
//
// It stops the listeners AND closes the live connections. Stopping the
// listeners alone is the mistake worth naming: NATS keeps its routes open once
// established, so a member whose listeners are shut carries on talking over
// the connections it already has — and every fleet-failure test above this one
// passes vacuously, having partitioned nothing.
//
// The member forms no route until [Cluster.Heal].
func (c *Cluster) Partition(t *testing.T, i int) {
	t.Helper()
	c.requireForwarders(t)
	for _, f := range c.forwarders {
		if f.from == i || f.to == i {
			f.stop()
		}
	}
	t.Logf("partitioned member %d", i)
}

// Heal restores member i's routes.
func (c *Cluster) Heal(t *testing.T, i int) {
	t.Helper()
	c.requireForwarders(t)
	for _, f := range c.forwarders {
		if f.from == i || f.to == i {
			if err := f.start(t.Context()); err != nil {
				t.Fatalf("heal %d->%d: %v", f.from, f.to, err)
			}
		}
	}
	t.Logf("healed member %d", i)
}

// drainBound is how long [Cluster.drain] waits for a forwarder's connections
// to end once it takes no new ones.
//
// LIVENESS, NOT A DEADLINE: what ends a connection a forwarder carries is the
// far member refusing it, which takes milliseconds; a member stopping; or the
// forwarder's own dial to a member that is down, which [forwarderDialTimeout]
// gives up on in two seconds. A route the far member admitted is held open,
// and NATS keeps an established route for as long as both ends run, so it
// never ends and the bound only ever fails a drain one of whose routes was
// admitted. Thirty seconds is the wait this package's cases give a refusal to
// be counted.
const drainBound = 30 * time.Second

// drain stops the forwarder from member from to member to taking new
// connections, waits until every connection it carries has ended, and answers
// how many it carried since it last opened: each a dial of member from's that
// reached member to, and every one there was, since nothing reaches member to
// by this path once it returns.
//
// THE HALF OF A PARTITION THAT ENDS NOTHING: [Cluster.Partition] closes the
// connections a forwarder carries, which cuts an established route, and this
// leaves each to end on its own. A route the far member refuses ends at its
// refusal; one it admitted is held open and never does, so a forwarder that
// does not empty fails the case, naming what it still carries.
func (c *Cluster) drain(t *testing.T, from, to int) int {
	t.Helper()
	f := c.forwarderOf(t, from, to)
	carried, emptied := f.drain()
	bound := time.NewTimer(drainBound) //fixedwait:liveness
	defer bound.Stop()
	select {
	case <-emptied:
		return carried
	case <-bound.C:
		f.mu.Lock()
		open := f.open
		f.mu.Unlock()
		t.Fatalf("the forwarder %d->%d still carries %d of the %d connections it took, %s "+
			"after it stopped taking new ones: member %d admitted a route and holds it "+
			"open, where one it refused would have ended at its refusal",
			from, to, open, carried, drainBound, to)
		return 0
	}
}

// reopen opens the forwarder from member from to member to again after
// [Cluster.drain], on the port member from dials, counting from zero.
func (c *Cluster) reopen(t *testing.T, from, to int) {
	t.Helper()
	if err := c.forwarderOf(t, from, to).start(t.Context()); err != nil {
		t.Fatalf("reopen %d->%d: %v", from, to, err)
	}
}

// forwarderOf is the forwarder member from reaches member to through.
func (c *Cluster) forwarderOf(t *testing.T, from, to int) *forwarder {
	t.Helper()
	c.requireForwarders(t)
	for _, f := range c.forwarders {
		if f.from == from && f.to == to {
			return f
		}
	}
	t.Fatalf("no forwarder from member %d to member %d", from, to)
	return nil
}

// requireForwarders fails a test that partitions a cluster started without
// them, rather than silently doing nothing.
//
// The failure mode this closes is the worst kind: a Partition call that
// returns cleanly having cut nothing, under a test whose assertions then hold
// for the wrong reason.
func (c *Cluster) requireForwarders(t *testing.T) {
	t.Helper()
	if len(c.forwarders) == 0 {
		t.Fatal("this cluster was started without forwarders, so Partition and " +
			"Heal would cut nothing and every assertion under them would hold " +
			"vacuously: start it with StartPartitionableCluster")
	}
}

// forwarderDialTimeout bounds the relay's own dial to the peer's real route
// port.
//
// Loopback, so the honest outcomes are microseconds or an immediate refusal
// from a peer that is not up yet — which is the ordinary case here, since the
// relays are started BEFORE the members. What the timeout covers is the one
// shape that neither answers nor refuses: a listener whose accept backlog is
// full, on a machine already running dozens of embedded brokers. Two seconds
// is far inside the minute a member waits for its cluster to become current,
// so a route that loses this dial is retried by NATS and surfaces as a slow
// cluster rather than as a harness that hung.
const forwarderDialTimeout = 2 * time.Second

// start opens (or reopens) the listener and serves it.
//
// The context is the TEST's: it bounds the listen and every dial the relay
// makes, so a forwarder cannot outlive the test that built it by holding a
// connect attempt open past the end of the run.
func (f *forwarder) start(ctx context.Context) error {
	f.mu.Lock()
	if f.listener != nil {
		f.mu.Unlock()
		return nil
	}
	// THE SAME PORT EVERY TIME. The member was started with this address
	// in its routes and NATS re-dials that address on its own schedule, so
	// a healed forwarder has to answer where the peer is already looking.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", f.port))
	if err != nil {
		f.mu.Unlock()
		return err
	}
	f.listener = ln
	f.closed = false
	f.live = map[net.Conn]struct{}{}
	f.carried = 0
	served := make(chan struct{})
	f.served = served
	f.mu.Unlock()

	go f.serve(ctx, ln, served)
	return nil
}

// stop closes the listener and every live connection through it.
func (f *forwarder) stop() {
	f.mu.Lock()
	ln, live := f.listener, f.live
	f.listener, f.live, f.closed = nil, nil, true
	f.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for conn := range live {
		_ = conn.Close()
	}
}

// drain closes the listener and leaves every connection it carries open. Once
// the goroutine serving it has returned, so every connection it accepted is
// counted, it answers how many it carried since it opened and a channel that
// closes when the last of them has ended.
func (f *forwarder) drain() (carried int, emptied <-chan struct{}) {
	f.mu.Lock()
	ln, served := f.listener, f.served
	f.listener = nil
	f.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	// BOUNDED BY THE DIAL IN HAND: serve returns at its next accept, after
	// at most one dial of forwarderDialTimeout's.
	if served != nil {
		<-served
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan struct{})
	f.emptied = ch
	f.settle()
	return f.carried, ch
}

// serve accepts and proxies until the listener closes, and closes served when
// it returns.
//
// AN ACCEPTED CONNECTION IS COUNTED OPEN BEFORE THE NEXT ACCEPT, so once this
// returns every connection the listener took is counted, and carried once its
// dial to the peer went through: [forwarder.drain] waits for exactly that.
func (f *forwarder) serve(ctx context.Context, ln net.Listener, served chan<- struct{}) {
	defer close(served)
	dialer := net.Dialer{Timeout: forwarderDialTimeout}
	for {
		in, err := ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.open++
		f.mu.Unlock()
		out, err := dialer.DialContext(ctx, "tcp", f.target)
		if err != nil {
			_ = in.Close()
			f.ended()
			continue
		}
		f.track(in, out)
		go f.proxy(in, out)
	}
}

// track registers both halves of a connection dialled through to the peer, so
// a partition can close them — closing them at once if the partition already
// happened — and counts the connection carried.
func (f *forwarder) track(in, out net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.carried++
	if f.closed || f.live == nil {
		_ = in.Close()
		_ = out.Close()
		return
	}
	f.live[in] = struct{}{}
	f.live[out] = struct{}{}
}

// proxy copies a carried connection both ways until either side ends, which
// closes both, and then counts it ended.
func (f *forwarder) proxy(in, out net.Conn) {
	toPeer := make(chan struct{})
	go func() {
		pipe(out, in)
		close(toPeer)
	}()
	pipe(in, out)
	<-toPeer
	f.mu.Lock()
	delete(f.live, in)
	delete(f.live, out)
	f.mu.Unlock()
	f.ended()
}

// ended counts one accepted connection ended, and wakes a drain waiting for
// the last.
func (f *forwarder) ended() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open--
	f.settle()
}

// settle closes emptied once nothing is open. f.mu is held.
func (f *forwarder) settle() {
	if f.open == 0 && f.emptied != nil {
		close(f.emptied)
		f.emptied = nil
	}
}

// pipe copies src to dst until either ends, then closes both.
func pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}
