package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// leaseNode is the backends of one embedded node whose leases are the KV's,
// closed with the case, and its lease store as the engine wired it.
func leaseNode(t *testing.T) (*Backends, *kv.Store) {
	t.Helper()
	b := testBootstrap(t)
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	return leaseNodeOf(t, b)
}

// externalLeaseNode is [leaseNode] on an external broker.
func externalLeaseNode(t *testing.T) (*Backends, *kv.Store) {
	t.Helper()
	b := testBootstrap(t)
	b.Stream.Type = config.StreamNATS
	b.Stream.URL = externaltest.Start(t, queue.MaxPayloadBytes).URL()
	b.Stream.Token = externaltest.Token
	return leaseNodeOf(t, b)
}

// leaseNodeOf opens b's backends with the KV's leases, closed with the case,
// and is its lease store as the engine wired it.
func leaseNodeOf(t *testing.T, b config.Bootstrap) (*Backends, *kv.Store) {
	t.Helper()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Coordination.Type = config.CoordinationEmbeddedKV
	SeedStore(t, &b)
	back, err := OpenBackends(t.Context(), &b, nil, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.WithoutCancel(t.Context())) })
	// NO WRAPPER: the engine asserts TTL and Close on the store itself.
	store, ok := back.Coord.(*kv.Store)
	if !ok {
		t.Fatalf("the KV lease store is wired as %T, not the store itself", back.Coord)
	}
	if back.leases == nil || back.leases == back.rides {
		t.Fatal("the node opened no lease connection of its own")
	}
	return back, store
}

// sentOn is how many messages nc sent while call ran.
func sentOn(nc *nats.Conn, call func()) uint64 {
	before := nc.Stats().OutMsgs
	call()
	return nc.Stats().OutMsgs - before
}

// THE LEASE STORE RIDES THE LEASE CONNECTION, on the embedded broker and on an
// external one: a claim and a renewal are sent on it, and Get and a listing are
// not (ADR-0174).
//
// Mutant: attachCoordination opens the lease store on the coordination client
// — the claim and the renewal send nothing on the lease connection.
// Mutant: the reads over the hold client — the lease connection moves on Get.
// Mutant: the external branch hands out the queue's own connection as the
// lease connection — the external row has none of its own.
func TestTheLeaseStoreRidesTheLeaseConnection(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		open func(*testing.T) (*Backends, *kv.Store)
	}{{"embedded", leaseNode}, {"external", externalLeaseNode}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			back, store := c.open(t)
			holdsRideTheLeaseConnection(t, back, store)
		})
	}
}

// holdsRideTheLeaseConnection is [TestTheLeaseStoreRidesTheLeaseConnection]
// on one node.
func holdsRideTheLeaseConnection(t *testing.T, back *Backends, store *kv.Store) {
	t.Helper()
	ctx := t.Context()
	seat := coord.SeatResource(uuid.New())
	var (
		lease *coord.Lease
		err   error
	)
	if n := sentOn(back.leases, func() {
		lease, _, err = store.TryAcquire(ctx, seat, coord.AcquireOptions{Owner: "node:1",
			TTL: store.TTL(), Ungated: true, Protocol: coord.ProtocolVersion})
	}); err != nil || lease == nil {
		t.Fatalf("claim %s = (%v, %v)", seat, lease, err)
	} else if n == 0 {
		t.Error("a claim sent nothing on the lease connection")
	}
	if n := sentOn(back.leases, func() {
		_, err = store.Renew(ctx, seat, "node:1", lease.Epoch, store.TTL())
	}); err != nil {
		t.Fatalf("renew %s: %v", seat, err)
	} else if n == 0 {
		t.Error("a renewal sent nothing on the lease connection")
	}
	if n := sentOn(back.leases, func() {
		_, err = store.Get(ctx, seat)
		if err == nil {
			_, err = store.ListLive(ctx, coord.ClassSeat)
		}
	}); err != nil {
		t.Fatalf("read %s: %v", seat, err)
	} else if n != 0 {
		t.Errorf("Get and a listing sent %d message(s) on the lease connection, want none", n)
	}
}

// THE LEASE CONNECTION IS ONE THE NODE HEARS LOSING: NATS closing it for good
// is a node whose leases cannot be renewed, so it is dialled watched, as the
// coordination connection is (ADR-0001), and not for the node's own Close.
//
// Mutant: the lease connection dialled unwatched — no closed handler.
func TestTheLeaseConnectionIsWatched(t *testing.T) {
	t.Parallel()
	back, _ := leaseNode(t)
	if back.leases.Opts.ClosedCB == nil {
		t.Error("the lease connection has no closed handler, so NATS closing it for good " +
			"leaves the node holding leases it cannot renew")
	}
	if !back.leases.Opts.NoCallbacksAfterClientClose {
		t.Error("the lease connection reports the node's own Close at shutdown as a lost broker")
	}
}

// A HOLD IS REFUSED WHILE THE DATA PATH IS DEAD: the engine gates the KV lease
// store on its queue's data path, so once the queue's own connection is gone
// a renewal is unknown, though the lease connection would carry it (ADR-0173).
//
// Mutant: attachCoordination sets no Carrier — the renewal lands.
func TestAHoldIsRefusedWhileTheDataPathIsDead(t *testing.T) {
	t.Parallel()
	back, store := leaseNode(t)
	ctx := t.Context()
	seat := coord.SeatResource(uuid.New())
	lease, _, err := store.TryAcquire(ctx, seat, coord.AcquireOptions{Owner: "node:1",
		TTL: store.TTL(), Ungated: true, Protocol: coord.ProtocolVersion})
	if err != nil || lease == nil {
		t.Fatalf("claim %s = (%v, %v)", seat, lease, err)
	}
	if err := back.Queue.Stop(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("stop the queue: %v", err)
	}
	renewed, err := store.Renew(ctx, seat, "node:1", lease.Epoch, store.TTL())
	if !errors.Is(err, coord.ErrUnavailable) || renewed {
		t.Errorf("a renewal with the queue's connection gone = (%v, %v), want unknown", renewed, err)
	}
	if got, err := store.Get(ctx, seat); err != nil || got == nil || got.Epoch != lease.Epoch {
		t.Errorf("the refused renewal's seat reads (%v, %v), want still held at epoch %d",
			got, err, lease.Epoch)
	}
}

// pathsOf is a queue whose data path and lease link answer as set.
type pathsOf struct {
	queue.EventQueue
	data, lease error
}

func (p pathsOf) DataPath() error  { return p.data }
func (p pathsOf) LeaseLink() error { return p.lease }

// BROKERLINK NAMES THE PATH THAT IS DOWN: the data path first, which every
// mailbox rides, then the lease link, which the node's holds ride; nil only
// while both answer.
//
// Mutant: BrokerLink asks the data path alone — a dead lease link reads ready.
func TestBrokerLinkNamesTheLeaseLinkAndTheDataPath(t *testing.T) {
	t.Parallel()
	down := errors.New("down")
	for _, c := range []struct {
		name        string
		data, lease error
		want        string
	}{
		{"both up", nil, nil, ""},
		{"the data path down", down, nil, "the data path"},
		{"the lease link down", nil, down, "the lease link"},
		{"both down", down, down, "the data path"},
	} {
		e := &Engine{backends: &Backends{Queue: pathsOf{data: c.data, lease: c.lease}}}
		err := e.BrokerLink()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: BrokerLink = %v, want nil", c.name, err)
		case c.want != "" && (err == nil || !strings.HasPrefix(err.Error(), c.want) || !errors.Is(err, down)):
			t.Errorf("%s: BrokerLink = %v, want it to name %s", c.name, err, c.want)
		}
	}
}

// A LEAF'S HOLD IS REFUSED ONCE ITS DATA LINK FALLS SILENT, its lease link up:
// the carrier is the queue's end-to-end proof, not its first hop, which reads
// up across a link that carries nothing until the leaf's pings give up
// minutes later. The renewal is refused as unknown, naming the data path, and
// the seat stays held for its TTL to run out, as the member reads it (ADR-0173).
//
// Mutant: the carrier wired to the queue's first hop (Queue.Link) — never
// refused while the link reads up. Mutant: the carrier hands out DataPath
// without starting its probe — DataPath answers from the first hop alone.
func TestALeafsHoldIsRefusedOnceItsDataLinkFallsSilent(t *testing.T) {
	t.Parallel()
	member := testBootstrap(t)
	member.Node.ID = "lease-member"
	brokertest.StoreIn(&member.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	member.Stream.Leaf.Host = "127.0.0.1"
	var onMember *Backends
	var memberStore *kv.Store
	for attempt := 1; onMember == nil; attempt++ {
		// A PORT LOST BETWEEN THE PROBE AND THE BIND says nothing about the case.
		member.Stream.Leaf.Port = freeLoopbackPort(t)
		of := &bootstrapOf{b: &member}
		onMember, memberStore = openLeaseNode(t, of)
		if onMember == nil && (!errors.Is(of.err, jetstream.ErrLeafPortTaken) || attempt == 4) {
			t.Fatalf("open the member: %v", of.err)
		}
	}
	link := linktest.Lagging(t, fmt.Sprintf("127.0.0.1:%d", member.Stream.Leaf.Port))

	leaf := testBootstrap(t)
	leaf.Node.ID = "lease-leaf"
	leaf.Secrets = member.Secrets
	leaf.Stream.Leaf.URLs = []string{"nats-leaf://" + link.Addr()}
	of := &bootstrapOf{b: &leaf}
	_, store := openLeaseNode(t, of)
	if store == nil {
		t.Fatalf("open the leaf: %v", of.err)
	}

	ctx := t.Context()
	seat := coord.SeatResource(uuid.New())
	lease, _, err := store.TryAcquire(ctx, seat, coord.AcquireOptions{Owner: "node:leaf",
		TTL: store.TTL(), Ungated: true, Protocol: coord.ProtocolVersion})
	if err != nil || lease == nil {
		t.Fatalf("claim %s = (%v, %v)", seat, lease, err)
	}
	link.Blackhole(server.DEFAULT_GLOBAL_ACCOUNT)
	t.Cleanup(func() { link.Mend(server.DEFAULT_GLOBAL_ACCOUNT) })

	bound := time.NewTimer(60 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	poll := time.NewTicker(100 * time.Millisecond) //fixedwait:liveness
	defer poll.Stop()
	for {
		renewed, refused := store.Renew(ctx, seat, "node:leaf", lease.Epoch, store.TTL())
		if refused != nil {
			if !errors.Is(refused, coord.ErrUnavailable) || !errors.Is(refused, jetstream.ErrDataPathUnproven) ||
				renewed {
				t.Fatalf("a renewal across a silent data link = (%v, %v), want unknown for the "+
					"unproven data path", renewed, refused)
			}
			break
		}
		select {
		case <-bound.C:
			t.Fatal("a renewal still lands a minute after the leaf's data link fell silent")
		case <-poll.C:
		}
	}
	got, err := memberStore.Get(ctx, seat)
	if err != nil || got == nil || got.Epoch != lease.Epoch || got.Owner != "node:leaf" {
		t.Errorf("the refused renewal's seat reads (%v, %v) on the member, want still the leaf's "+
			"at epoch %d", got, err, lease.Epoch)
	}
}

// openLeaseNode opens of's node with the KV's leases, closed with the case,
// and its lease store; nil, with of.err set, where it does not open.
func openLeaseNode(t *testing.T, of *bootstrapOf) (*Backends, *kv.Store) {
	t.Helper()
	of.b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	of.b.Coordination.Type = config.CoordinationEmbeddedKV
	SeedStore(t, of.b)
	back, err := OpenBackends(t.Context(), of.b, nil, nil)
	if err != nil {
		of.err = err
		return nil, nil
	}
	t.Cleanup(func() { back.Close(context.WithoutCancel(t.Context())) })
	store, ok := back.Coord.(*kv.Store)
	if !ok {
		t.Fatalf("the KV lease store is wired as %T, not the store itself", back.Coord)
	}
	return back, store
}

// THE LEASE TTL IN FORCE IS THE BUCKET'S, THROUGH THE STORE AS WIRED: a node
// configured with a longer TTL than the bucket a peer made claims and renews
// with the bucket's, which it reads off the store the engine wired, opened
// over two clients (ADR-0174).
//
// Mutant: the lease store wired behind a wrapper with no TTL — the node
// claims with its own 60 s, which the bucket's 30 s refuses.
func TestTheLeaseTTLInForceIsTheBucketsThroughTheWiredStore(t *testing.T) {
	t.Parallel()
	url := externaltest.Start(t, queue.MaxPayloadBytes).URL()
	onBroker := func(ttl float64) config.Bootstrap {
		b := testBootstrap(t)
		b.Stream.Type = config.StreamNATS
		b.Stream.URL = url
		b.Stream.Token = externaltest.Token
		b.Coordination.LeaseTTLSeconds = ttl
		return b
	}
	first, second := onBroker(30), onBroker(60)
	first.Node.ID, second.Node.ID = "ttl-first", "ttl-second"
	// THE FLEET'S KEYRING, which every node holds.
	second.Secrets = first.Secrets
	leaseNodeOf(t, first)
	back, _ := leaseNodeOf(t, second)
	if got := effectiveLeaseTTL(&second, back.Coord); got != 30*time.Second {
		t.Errorf("a node configured with 60s on a bucket made at 30s holds with %v, want the bucket's", got)
	}
}
