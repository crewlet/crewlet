package kv

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// sharedLinkRate is the case's leaf link, both ways, shared fairly by every
// connection with bytes to send: 64 KiB a second, about twice the slowest
// allowed, so the upload below holds the data link for some sixteen seconds.
const sharedLinkRate = 64 << 10

// uploadBytes is the upload the data link carries: half a mebibyte, random so
// the link's compression leaves it whole.
const uploadBytes = 512 << 10

// leafOverSharedLink is a member serving leaves behind a fair shared link and
// a leaf joined across it: the leaf's data connection (the global account)
// and its lease connection ([js.LeaseAccount]), and the member's own.
type leafOverSharedLink struct {
	link                *linktest.Link
	data, leases, local *nats.Conn
}

func newLeafOverSharedLink(t *testing.T) leafOverSharedLink {
	t.Helper()
	// A MEMBER THAT SERVES LEAVES PERSISTS, or it is refused.
	member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: brokertest.TempDir(t),
		StoreMaxBytes: brokertest.StoreMaxBytes, FleetAuth: leafFleetAuth()})
	if err != nil {
		t.Fatalf("start a member serving leaves: %v", err)
	}
	t.Cleanup(member.Shutdown)
	link := linktest.SharedEachWay(t, fmt.Sprintf("127.0.0.1:%d", member.LeafPort()), sharedLinkRate)
	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf",
		LeafURLs: []string{"nats-leaf://" + link.Addr()}, FleetAuth: leafFleetAuth()})
	if err != nil {
		t.Fatalf("start the leaf: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	f := leafOverSharedLink{link: link}
	for _, c := range []struct {
		conn **nats.Conn
		dial func() (*nats.Conn, error)
		what string
	}{{&f.data, leaf.Conn, "the leaf's data connection"},
		{&f.leases, leaf.LeaseConn, "the leaf's lease connection"},
		{&f.local, member.Conn, "the member"}} {
		nc, err := c.dial()
		if err != nil {
			t.Fatalf("connect %s: %v", c.what, err)
		}
		t.Cleanup(nc.Close)
		*c.conn = nc
	}
	return f
}

// client is a JetStream client of nc in the embedded fleet's domain.
func (leafOverSharedLink) client(t *testing.T, nc *nats.Conn) jetstream.JetStream {
	t.Helper()
	c, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatalf("a client of the fleet's domain: %v", err)
	}
	return c
}

// upload sends uploadBytes from the leaf's data connection to the member and
// answers on the channel it returns once the member has them, after waiting
// until the link carries the first of them.
func (f leafOverSharedLink) upload(t *testing.T, subject string) <-chan error {
	t.Helper()
	sub, err := f.local.Subscribe(subject, func(m *nats.Msg) { _ = m.Respond(nil) })
	if err != nil {
		t.Fatalf("answer the upload: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := f.local.Flush(); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, "the member's interest in the upload across the link", func() bool {
		_, err := f.data.Request(subject, nil, time.Second)
		return err == nil
	})
	body := make([]byte, uploadBytes)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	before := f.link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToBroker).Carried
	done := make(chan error, 1)
	go func() {
		_, err := f.data.Request(subject, body, time.Minute)
		done <- err
	}()
	pollUntil(t, "the upload on the data link", func() bool {
		return f.link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToBroker).Carried > before+16<<10
	})
	return done
}

// pollUntil waits until cond holds, within a liveness bound.
func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	bound := time.NewTimer(time.Minute) //fixedwait:liveness
	defer bound.Stop()
	tick := time.NewTicker(20 * time.Millisecond) //fixedwait:liveness
	defer tick.Stop()
	for !cond() {
		select {
		case <-bound.C:
			t.Fatalf("waited a minute for %s", what)
		case <-tick.C:
		}
	}
}

// A RENEWAL CROSSES A LEAF'S LINK AHEAD OF AN UPLOAD ON IT (#1035): the
// lease store's holds ride the lease connection, a leaf link of their own, so
// at a fair bottleneck a renewal sent while half a mebibyte crosses the data
// link is answered before the upload has crossed. A store whose hold client is
// the data connection is answered only once the upload's bytes have, since
// its renewal is queued behind them on the one link. Decided by the link's own
// count of what it carried when each renewal was answered, never by a clock.
//
// Mutant: the lease store opened over the data client — the first arm's
// renewals wait for the upload, as the control's do.
func TestARenewalCrossesALeafsLinkAheadOfAnUploadOnIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newLeafOverSharedLink(t)
	prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))
	member, err := Open(ctx, f.client(t, f.local), f.client(t, f.local),
		Config{TTL: knownTTL, BucketPrefix: prefix, Reach: InProcess})
	if err != nil {
		t.Fatalf("open the member's store: %v", err)
	}
	t.Cleanup(member.Close)
	open := func(hold *nats.Conn) *Store {
		s, err := Open(ctx, f.client(t, hold), f.client(t, f.data),
			Config{TTL: knownTTL, BucketPrefix: prefix, Reach: AcrossLink})
		if err != nil {
			t.Fatalf("open the leaf's store: %v", err)
		}
		t.Cleanup(s.Close)
		return s
	}
	carried := func() int64 {
		return f.link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToBroker).Carried
	}

	leases, shared := open(f.leases), open(f.data)
	const owner = "leaf:1"
	onLeases := claimUngated(ctx, t, leases, knownSeat(0), owner, knownTTL)
	onShared := claimUngated(ctx, t, shared, knownSeat(1), owner, knownTTL)

	start := carried()
	done := f.upload(t, "kv.upload.leases")
	for i := range 3 {
		if ok, err := leases.Renew(ctx, knownSeat(0), owner, onLeases.Epoch, knownTTL); err != nil || !ok {
			t.Fatalf("renewal %d over the lease connection = (%v, %v)", i+1, ok, err)
		}
		if got := carried() - start; got >= uploadBytes {
			t.Errorf("renewal %d over the lease connection was answered once the data link had "+
				"carried %d bytes, the whole upload's %d: it waited behind it", i+1, got, uploadBytes)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("the upload: %v", err)
	}

	// THE CONTROL: the same renewal over the data connection, queued behind
	// the upload, is answered only once the upload has crossed, or not at
	// all inside its own budget.
	start = carried()
	done = f.upload(t, "kv.upload.shared")
	ok, err := shared.Renew(ctx, knownSeat(1), owner, onShared.Epoch, knownTTL)
	got := carried() - start
	switch {
	case err != nil && !errors.Is(err, coord.ErrUnavailable):
		t.Fatalf("the renewal over the data connection = %v, want an answer or unknown", err)
	case err == nil && ok && got < uploadBytes:
		t.Errorf("the control's renewal was answered with %d bytes of the %d-byte upload carried: "+
			"the case cannot tell a renewal behind the upload from one beside it", got, uploadBytes)
	}
	if err := <-done; err != nil {
		t.Fatalf("the control's upload: %v", err)
	}
}
