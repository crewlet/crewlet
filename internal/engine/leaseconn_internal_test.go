package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// leaseNode is the backends of one embedded node whose leases are the KV's,
// closed with the case, and its lease store as the engine wired it.
func leaseNode(t *testing.T) (*Backends, *kv.Store) {
	t.Helper()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
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

// THE LEASE STORE RIDES THE LEASE CONNECTION: a claim and a renewal are sent
// on it, and Get and a listing are not (ADR-0169).
//
// Mutant: attachCoordination opens the lease store on the coordination client
// — the claim and the renewal send nothing on the lease connection.
// Mutant: the reads over the hold client — the lease connection moves on Get.
func TestTheLeaseStoreRidesTheLeaseConnection(t *testing.T) {
	t.Parallel()
	back, store := leaseNode(t)
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
