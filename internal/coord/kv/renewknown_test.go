package kv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
)

// knownTTL is the seat lease bucket's age in these cases, and the TTL every
// lease they renew in one round trip is taken at: the production shape, where
// a seat or presence lease is held at its bucket's whole age.
const knownTTL = time.Minute

// published is how many messages nc sent while call ran: one per request a
// store makes on it, a read or a write, since its replies come back on a
// subscription the connection made once.
func published(nc *nats.Conn, call func()) uint64 {
	before := nc.Stats().OutMsgs
	call()
	return nc.Stats().OutMsgs - before
}

// peerOf is a second store on s's buckets, over a connection of its own to ns:
// another process of the same fleet, whose writes s's connection never sends.
func peerOf(t *testing.T, ns *server.Server, s *Store) *Store {
	t.Helper()
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("connect the peer: %v", err)
	}
	t.Cleanup(nc.Close)
	peer, err := Open(context.Background(), jsOf(nc), Config{TTL: s.TTL(), Reach: InProcess,
		BucketPrefix: strings.TrimSuffix(s.leases.kv.Bucket(), leasesSuffix)})
	if err != nil {
		t.Fatalf("open the peer's store: %v", err)
	}
	return peer
}

// knownSeat is a seat a case holds, by its own number.
func knownSeat(n int) string {
	return coord.SeatResource(uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "known-seat-%d", n)))
}

// claimUngated claims resource for owner at ttl, outside the protocol gate, so
// no gate view runs on the store's connection beside what a case counts.
func claimUngated(ctx context.Context, t *testing.T, s *Store, resource, owner string,
	ttl time.Duration) *coord.Lease {

	t.Helper()
	lease, refused, err := s.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: owner, TTL: ttl, Ungated: true, Protocol: coord.ProtocolVersion,
	})
	if err != nil || lease == nil {
		t.Fatalf("claim %s for %s = (%v, %q, %v)", resource, owner, lease, refused, err)
	}
	return lease
}

// A RENEWAL OF A RECORD THIS STORE LAST WROTE IS ONE ROUND TRIP: one write at
// the revision its own last write was given, with no read before it, counted
// as the messages the store's own connection sends. A peer's write in between
// moves the revision, and the renewal reads and decides as before — renewed
// for a write by the same tenure, false once a peer took the lease — and then
// it is one round trip again, or, for a lease it no longer holds, a read.
//
// Thirty-two renewals at once are thirty-two messages: what a pass sending its
// renewals together costs the connection.
//
// Red on main: every renewal read the record first, two messages.
// Mutation: read before every renewal (2 messages, where 1 is wanted).
func TestARenewOfARecordThisStoreLastWroteIsOneRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns, nc := embeddedServer(t)
	s := openStoreVia(t, jsOf(nc), InProcess, knownTTL)
	peer := peerOf(t, ns, s)
	const owner = "node-a:1"
	seat := knownSeat(0)
	lease := claimUngated(ctx, t, s, seat, owner, knownTTL)

	renew := func(what string, want bool, wantSent uint64) {
		t.Helper()
		var (
			renewed bool
			err     error
		)
		sent := published(nc, func() { renewed, err = s.Renew(ctx, seat, owner, lease.Epoch, knownTTL) })
		if err != nil || renewed != want {
			t.Fatalf("%s: Renew = (%v, %v), want (%v, nil)", what, renewed, err, want)
		}
		if sent != wantSent {
			t.Errorf("%s: the renewal sent %d message(s) on the store's connection, want %d",
				what, sent, wantSent)
		}
	}

	for i := range 3 {
		renew(fmt.Sprintf("renewal %d of the store's own claim", i+1), true, 1)
	}

	// The same tenure renewed through another handle — a second path of
	// this owner's — moves the revision: the write at the known one is
	// refused, the record is read, and the renewal written at what it read.
	if ok, err := peer.Renew(ctx, seat, owner, lease.Epoch, knownTTL); err != nil || !ok {
		t.Fatalf("the peer's renewal of the same tenure = (%v, %v)", ok, err)
	}
	renew("a renewal after another handle's write", true, 3)
	renew("the renewal after that", true, 1)

	// A peer takes the lease: the write at the known revision is refused,
	// the read finds the peer's record, and the answer is false — after
	// which nothing is known of the seat, so the next renewal reads alone.
	if ok, err := peer.Release(ctx, seat, owner, lease.Epoch); err != nil || !ok {
		t.Fatalf("the peer's release = (%v, %v)", ok, err)
	}
	claimUngated(ctx, t, peer, seat, "node-b:1", knownTTL)
	renew("a renewal after a peer took the lease", false, 2)
	renew("a renewal of a lease nothing knows", false, 1)

	// Many at once, as a pass sends them.
	const together = 32
	leases := make([]*coord.Lease, together)
	for i := range leases {
		leases[i] = claimUngated(ctx, t, s, knownSeat(i+1), owner, knownTTL)
	}
	var wg sync.WaitGroup
	errs := make([]error, together)
	sent := published(nc, func() {
		for i, l := range leases {
			wg.Go(func() {
				ok, err := s.Renew(ctx, l.Resource, owner, l.Epoch, knownTTL)
				if err == nil && !ok {
					err = errors.New("answered false")
				}
				errs[i] = err
			})
		}
		wg.Wait()
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("renewal %d of %d at once: %v", i, together, err)
		}
	}
	if sent != together {
		t.Errorf("%d renewals at once sent %d messages, want one each", together, sent)
	}
}

// A CLAIM ITS OWNER STILL HOLDS IS ITS WRITE AND ITS READ-BACK: the presence's
// renewal, which claims rather than renews, starts from the record its store
// last wrote and writes at that revision with no read before it, so it costs
// two round trips — the write, and the read that hands back the store's
// deadline — where it cost three. A peer's write in between moves the
// revision, and the claim reads and decides as before.
//
// Red on main: three messages.
// Mutation: read on the first round too (three, where two are wanted).
func TestAClaimItsOwnerStillHoldsIsItsWriteAndItsReadBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns, nc := embeddedServer(t)
	s := openStoreVia(t, jsOf(nc), InProcess, knownTTL)
	peer := peerOf(t, ns, s)
	const owner = "node-a:1"
	presence := coord.NodeResource("node-a")
	first := claimUngated(ctx, t, s, presence, owner, knownTTL)

	claim := func(what string, wantSent uint64) {
		t.Helper()
		var lease *coord.Lease
		sent := published(nc, func() { lease = claimUngated(ctx, t, s, presence, owner, knownTTL) })
		if lease.Epoch != first.Epoch {
			t.Fatalf("%s: the renewing claim moved the epoch %d -> %d", what, first.Epoch, lease.Epoch)
		}
		if sent != wantSent {
			t.Errorf("%s: the claim sent %d message(s) on the store's connection, want %d",
				what, sent, wantSent)
		}
	}
	claim("the owner's claim of its own presence", 2)
	claim("the next", 2)
	claimUngated(ctx, t, peer, presence, owner, knownTTL)
	claim("a claim after another handle's write", 4)
	claim("the claim after that", 2)
}

// awaitLapsed returns once the store itself judges resource unheld: its Get,
// which compares the record's deadline with the STORE's clock, finds no live
// lease. No clock of the case's decides it.
func awaitLapsed(ctx context.Context, t *testing.T, s *Store, resource string) {
	t.Helper()
	for {
		lease, err := s.Get(ctx, resource)
		if err != nil {
			t.Fatalf("read %s while waiting for it to lapse: %v", resource, err)
		}
		if lease == nil {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("%s never lapsed by the store's clock: %v", resource, ctx.Err())
		}
	}
}

// A KNOWN REVISION NEVER RENEWS A LAPSED DUTY LEASE: a duty's record is judged
// by its deadline against the store's clock, and can lapse while it is still
// there, so a write at its revision — which would land — renews nothing. The
// renewal reads the record and the clock and answers false, as a lapsed lease
// is deliberately not renewable.
//
// A guard on main, where every renewal read first.
// Mutation: know a record whatever its bucket (the duty renewed, true).
func TestAKnownRevisionNeverRenewsALapsedDutyLease(t *testing.T) {
	t.Parallel()
	lapsesBeforeRenewing(t, coord.WorkerResource("scheduler"))
}

// A KNOWN REVISION NEVER RENEWS A SEAT LEASE THAT LAPSED BEFORE ITS BUCKET'S
// AGE: a seat lease taken for less than the bucket's age is judged by its own
// deadline, as a duty is, so it is not known either.
//
// Mutation: know a record whatever its TTL (the seat renewed, true).
func TestAKnownRevisionNeverRenewsASeatLeaseShorterThanItsBucket(t *testing.T) {
	t.Parallel()
	lapsesBeforeRenewing(t, knownSeat(0))
}

// lapsesBeforeRenewing claims resource for a fraction of a second, lets it
// lapse by the store's clock, and holds its renewal to false.
func lapsesBeforeRenewing(t *testing.T, resource string) {
	t.Helper()
	ctx := t.Context()
	s := openStore(t, embeddedNATS(t), knownTTL)
	const (
		owner = "node-a:1"
		// short is far under the bucket's age, so the record is still
		// there when it lapses.
		short = 300 * time.Millisecond
	)
	lease := claimUngated(ctx, t, s, resource, owner, short)
	awaitLapsed(ctx, t, s, resource)
	renewed, err := s.Renew(ctx, resource, owner, lease.Epoch, short)
	if err != nil || renewed {
		t.Fatalf("the renewal of %s after it lapsed = (%v, %v), want (false, nil): a lapsed "+
			"lease is not renewable", resource, renewed, err)
	}
}

// RENEWALS OF KNOWN RECORDS GO TO THE BROKER TOGETHER: nothing in the store
// holds one renewal's write while another's is in flight, so a pass that sends
// its renewals together has them in flight together — which counting their
// messages cannot tell, since renewals sent one at a time send as many. Each
// renewal is held at its write until every one of them has reached it, which
// only renewals running at once can do.
//
// Mutation: hold one lock across a known renewal's write (the first renewal
// waits alone, until the case gives up on the others).
func TestRenewalsOfKnownRecordsAreInFlightTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), knownTTL)
	const owner = "node-a:1"
	const together = 16
	leases := make([]*coord.Lease, together)
	for i := range leases {
		leases[i] = claimUngated(ctx, t, s, knownSeat(i), owner, knownTTL)
	}

	// THE CASE'S PATIENCE, a liveness bound that decides nothing: renewals
	// in flight together all reach their writes at once, and only renewals
	// that cannot leave the first waiting for it to run out.
	giveUp, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var (
		mu      sync.Mutex
		arrived int
		gaveUp  bool
	)
	all := make(chan struct{})
	s.beforeKnownWrite = func(string) {
		mu.Lock()
		if arrived++; arrived == together {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-giveUp.Done():
			mu.Lock()
			gaveUp = true
			mu.Unlock()
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, together)
	for i, l := range leases {
		wg.Go(func() {
			ok, err := s.Renew(ctx, l.Resource, owner, l.Epoch, knownTTL)
			if err == nil && !ok {
				err = errors.New("answered false")
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("renewal %d of %d at once: %v", i, together, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if gaveUp || arrived != together {
		t.Fatalf("of %d renewals of records the store knows, %d reached their writes, and "+
			"never all in flight at once: the store sends them one at a time", together, arrived)
	}
}
