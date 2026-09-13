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
	// live are the connections currently proxied through this forwarder.
	// They are CLOSED on a partition, which is the half a stopped listener
	// does not cover: NATS holds its routes open, so refusing new
	// connections leaves an established route working indefinitely.
	live   map[net.Conn]struct{}
	closed bool
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
	f.mu.Unlock()

	go f.serve(ctx, ln)
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

// serve accepts and proxies until the listener closes.
func (f *forwarder) serve(ctx context.Context, ln net.Listener) {
	dialer := net.Dialer{Timeout: forwarderDialTimeout}
	for {
		in, err := ln.Accept()
		if err != nil {
			return
		}
		out, err := dialer.DialContext(ctx, "tcp", f.target)
		if err != nil {
			_ = in.Close()
			continue
		}
		f.track(in)
		f.track(out)
		go pipe(in, out)
		go pipe(out, in)
	}
}

// track registers a connection so a partition can close it, closing it at once
// if the partition already happened.
func (f *forwarder) track(c net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.live == nil {
		_ = c.Close()
		return
	}
	f.live[c] = struct{}{}
}

func pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}
