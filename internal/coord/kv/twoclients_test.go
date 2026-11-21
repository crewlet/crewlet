package kv

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// twoClients opens a store whose hold client rides hold and whose read client
// rides a connection of its own to ns, as the engine opens it (package doc,
// "Two clients"), and answers it with that second connection.
func twoClients(t *testing.T, hold *nats.Conn, connect func() (*nats.Conn, error),
	cfg Config) (*Store, *nats.Conn) {

	t.Helper()
	read, err := connect()
	if err != nil {
		t.Fatalf("connect the read client: %v", err)
	}
	t.Cleanup(read.Close)
	if cfg.BucketPrefix == "" {
		cfg.BucketPrefix = fmt.Sprintf("t%d", bucketSeq.Add(1))
	}
	cfg.Reach = InProcess
	s, err := Open(context.Background(), jsOf(hold), jsOf(read), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s, read
}

// THE CONTRACT ACROSS THE TWO CLIENTS: every case, with each store's holds on
// one connection and its reads on another, so a renewal acknowledged on the
// hold client is what Get and the listings read on the other ("a backend
// serves its own writes per resource").
func TestContractAcrossTheTwoClients(t *testing.T) {
	t.Parallel()
	ns, hold := embeddedServer(t)
	connect := func() (*nats.Conn, error) { return nats.Connect("", nats.InProcessServer(ns)) }
	coordtest.Run(t, func(t *testing.T) coord.Backend {
		s, _ := twoClients(t, hold, connect, Config{TTL: coordtest.LongTTL})
		return s
	})
}

// A HOLD RIDES THE HOLD CLIENT AND EVERY FLEET READ THE READ CLIENT: a claim,
// a renewal and a release send nothing on the read client's connection, and
// Get, the listings, the placement hints, the gate's view and a watch send
// nothing on the hold client's. The lease link then carries what this node
// holds and nothing that grows with the fleet.
//
// Mutations: the scans, Get or the gate view over the hold client (the read
// arms see the hold connection move); a write over the read client (the hold
// arms see the read connection move).
func TestAHoldRidesTheHoldClientAndEveryFleetReadTheReadClient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns, hold := embeddedServer(t)
	s, read := twoClients(t, hold, func() (*nats.Conn, error) {
		return nats.Connect("", nats.InProcessServer(ns))
	}, Config{TTL: knownTTL})
	const owner = "node-a:1"
	seat, other := knownSeat(0), knownSeat(1)

	sent := func(what string, wantHold, wantRead bool, call func() error) {
		t.Helper()
		var err error
		onRead := published(read, func() {
			if onHold := published(hold, func() { err = call() }); (onHold > 0) != wantHold {
				t.Errorf("%s sent %d message(s) on the hold client's connection, want any: %v",
					what, onHold, wantHold)
			}
		})
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if (onRead > 0) != wantRead {
			t.Errorf("%s sent %d message(s) on the read client's connection, want any: %v",
				what, onRead, wantRead)
		}
	}

	var lease *coord.Lease
	sent("an ungated claim", true, false, func() error {
		lease = claimUngated(ctx, t, s, seat, owner, knownTTL)
		return nil
	})
	sent("a renewal", true, false, func() error {
		_, err := s.Renew(ctx, seat, owner, lease.Epoch, knownTTL)
		return err
	})
	sent("Get", false, true, func() error {
		_, err := s.Get(ctx, seat)
		return err
	})
	sent("ListLive", false, true, func() error {
		_, err := s.ListLive(ctx, coord.ClassSeat)
		return err
	})
	sent("ListOwned", false, true, func() error {
		_, err := s.ListOwned(ctx, owner)
		return err
	})
	sent("PreferredResources", false, true, func() error {
		_, err := s.PreferredResources(ctx, coord.ClassSeat, "node-a")
		return err
	})
	sent("FleetProtocolFloor", false, true, func() error {
		_, _, err := s.FleetProtocolFloor(ctx)
		return err
	})
	// The gate's view now runs on the read client, so from here only the
	// hold client's silence is asked.
	held := published(hold, func() {
		if _, err := s.Release(ctx, seat, owner, lease.Epoch); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
	if held == 0 {
		t.Error("a release sent nothing on the hold client's connection")
	}
	watchCtx, stop := context.WithCancel(ctx)
	defer stop()
	if onHold := published(hold, func() {
		if _, err := s.WatchLive(watchCtx, coord.ClassSeat); err != nil {
			t.Fatalf("WatchLive: %v", err)
		}
	}); onHold != 0 {
		t.Errorf("WatchLive sent %d message(s) on the hold client's connection, want none", onHold)
	}
	// A gated claim's writes are a hold's and its gate a read's.
	if onHold := published(hold, func() {
		if _, refused, err := s.TryAcquire(ctx, other, coord.AcquireOptions{
			Owner: owner, TTL: knownTTL, Protocol: coord.ProtocolVersion}); err != nil || refused != "" {
			t.Fatalf("a gated claim = (%q, %v)", refused, err)
		}
	}); onHold == 0 {
		t.Error("a gated claim sent nothing on the hold client's connection")
	}
}

// errPathDown is the carrier's refusal in these cases.
var errPathDown = errors.New("the data path is down")

// A CARRIER REFUSES A HOLD BEFORE ANYTHING IS SENT: while it fails, a claim and
// a renewal answer unknown wrapping its error, sending nothing, and the held
// record is left at its revision; a release and every read pass, since giving
// a hold up is always right.
//
// Mutations: the carrier not asked by Renew or TryAcquire (each answers, and
// sends); the carrier asked by Release (refused, so a drain on a dead path
// holds its leases to the TTL).
func TestACarrierRefusesAHoldBeforeAnythingIsSent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, nc := embeddedServer(t)
	var down atomic.Bool
	s, err := Open(ctx, jsOf(nc), jsOf(nc), Config{TTL: knownTTL, Reach: InProcess,
		BucketPrefix: fmt.Sprintf("t%d", bucketSeq.Add(1)),
		Carrier: func() error {
			if down.Load() {
				return errPathDown
			}
			return nil
		}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	const owner = "node-a:1"
	seat, other := knownSeat(0), knownSeat(1)
	lease := claimUngated(ctx, t, s, seat, owner, knownTTL)
	before, err := s.leases.read.last(ctx, encodeResource(seat))
	if err != nil {
		t.Fatalf("read the held record: %v", err)
	}

	down.Store(true)
	refused := func(what string, err error, sent uint64) {
		t.Helper()
		if !errors.Is(err, coord.ErrUnavailable) || !errors.Is(err, errPathDown) {
			t.Errorf("%s with the carrier failing answered %v, want ErrUnavailable wrapping %v",
				what, err, errPathDown)
		}
		if sent != 0 {
			t.Errorf("%s with the carrier failing sent %d message(s), want none", what, sent)
		}
	}
	var claimErr, renewErr error
	sent := published(nc, func() {
		_, _, claimErr = s.TryAcquire(ctx, other, coord.AcquireOptions{
			Owner: owner, TTL: knownTTL, Ungated: true, Protocol: coord.ProtocolVersion})
	})
	refused("a claim", claimErr, sent)
	sent = published(nc, func() { _, renewErr = s.Renew(ctx, seat, owner, lease.Epoch, knownTTL) })
	refused("a renewal", renewErr, sent)
	after, err := s.leases.read.last(ctx, encodeResource(seat))
	if err != nil {
		t.Fatalf("read the held record again: %v", err)
	}
	if after.Revision() != before.Revision() {
		t.Errorf("the held record moved from revision %d to %d while the carrier failed",
			before.Revision(), after.Revision())
	}

	if got, err := s.Get(ctx, seat); err != nil || got == nil {
		t.Errorf("Get with the carrier failing = (%v, %v), want the lease", got, err)
	}
	if _, err := s.ListLive(ctx, coord.ClassSeat); err != nil {
		t.Errorf("ListLive with the carrier failing: %v", err)
	}
	if ok, err := s.Release(ctx, seat, owner, lease.Epoch); err != nil || !ok {
		t.Errorf("Release with the carrier failing = (%v, %v), want (true, nil)", ok, err)
	}
}
