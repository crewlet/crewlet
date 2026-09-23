package kv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE POSITIONS REGISTER HAS NO TTL, and this is the case that says why.
//
// Every other bucket in this estate expires its keys — that is what a lease,
// a claim, a dedupe window and a cooldown ARE. The register is the one bucket
// where a key's disappearance would be read as a fact rather than as silence:
// a node whose position key expired reads as a node that has applied NOTHING,
// and the trim takes a MINIMUM across those rows. So an expiry here does not
// lose information, it produces the wrong answer — the floor collapses to
// zero and the fleet stops trimming, or, with the inequality the other way,
// trims past a node that was merely quiet.
//
// The other buckets' short TTL is what makes this checkable in a test rather
// than in a year: the store is opened with a one-second TTL, a lease and a
// position are written together, and the lease is gone while the position is
// not.
func TestThePositionRegisterSurvivesASweepThatExpiresEverythingElse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	const ttl = time.Second
	leases := openStore(t, nc, ttl)
	s := openFleetWithTTL(t, nc, ttl)

	if _, err := leases.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
		Owner: "node-a:1", TTL: ttl,
	}); err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	want := coord.NodePositions{
		NodeID: "node-a",
		Domains: map[string]coord.DomainPosition{
			"tracker": {Seq: 4096, Generation: 2, AppliedThrough: 4096},
		},
	}
	if err := s.PutPositions(ctx, want); err != nil {
		t.Fatalf("PutPositions: %v", err)
	}

	// Long enough that every bucket with this store's TTL has reaped.
	time.Sleep(ttl + 500*time.Millisecond)

	if got, err := leases.Get(ctx, "seat:ceo"); err != nil || got != nil {
		t.Fatalf("the lease survived its TTL, so this run proves nothing about "+
			"what did NOT expire: (%v, %v)", got, err)
	}

	rows, err := s.Positions(ctx)
	if err != nil {
		t.Fatalf("Positions: %v", err)
	}
	row, found := rowFor(rows, "node-a")
	if !found {
		t.Fatalf("the position row expired with the leases: %v — a node that "+
			"went quiet for a second now reads as a node that has applied "+
			"nothing, and the trim takes a minimum across these rows", rows)
	}
	if got := row.Domains["tracker"].Seq; got != 4096 {
		t.Errorf("the row came back with seq %d, want 4096", got)
	}
	if row.At.IsZero() {
		t.Error("the row carries no timestamp, so nothing can tell a fresh " +
			"position from a year-old one")
	}
}

// AND A ROW IS FORGOTTEN ONLY WHEN A NODE IS, which is the other half: the
// register has no TTL, so the only thing that removes a row is the eviction
// that removes the node — and a row nothing removes is a node the trim waits
// for for ever.
func TestForgettingANodeRemovesItsRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openFleetWithTTL(t, embeddedNATS(t), time.Minute)

	for _, id := range []string{"node-a", "node-b"} {
		if err := s.PutPositions(ctx, coord.NodePositions{
			NodeID:  id,
			Domains: map[string]coord.DomainPosition{"tracker": {Seq: 10}},
		}); err != nil {
			t.Fatalf("PutPositions(%s): %v", id, err)
		}
	}
	if err := s.ForgetPositions(ctx, "node-a"); err != nil {
		t.Fatalf("ForgetPositions: %v", err)
	}
	rows, err := s.Positions(ctx)
	if err != nil {
		t.Fatalf("Positions: %v", err)
	}
	if _, found := rowFor(rows, "node-a"); found {
		t.Error("the evicted node's row survived, so the trim still waits for it")
	}
	if _, found := rowFor(rows, "node-b"); !found {
		t.Error("forgetting one node removed another's row")
	}
}

// rowFor picks one node's row out of the register's listing.
func rowFor(rows []coord.NodePositions, node string) (coord.NodePositions, bool) {
	for _, row := range rows {
		if row.NodeID == node {
			return row, true
		}
	}
	return coord.NodePositions{}, false
}

// openFleetWithTTL opens a fleet store whose expiring buckets all reap at ttl,
// so a case can prove what did NOT expire beside something that did.
func openFleetWithTTL(t *testing.T, nc *nats.Conn, ttl time.Duration) *FleetStore {
	t.Helper()
	store, err := OpenFleet(context.Background(), nc, FleetConfig{
		BucketPrefix:    fmt.Sprintf("p%d", bucketSeq.Add(1)),
		RateWindow:      ttl,
		ClaimTTL:        ttl,
		LedgerRetention: ttl,
		FireRetention:   ttl,
		FollowRetention: ttl,
		CooldownMax:     ttl,
		BudgetRetention: ttl,
		StatusFreshness: ttl,
	})
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	return store
}

// THE BROKER FILTERS THE CLASS, AND THE WALK ONLY SEES ITS OWN.
//
// Seven classes share the positions register, and a walk that read all of them
// to use one was what this replaced — on a read the state-log write fence
// takes on every first write to a subject. A key here is
// coord.DocumentKey(class, id), whose separator is a dot because a key IS a
// subject token path, so the class is a token the broker can match.
//
// What this asserts is the OUTCOME rather than the wire: with keys of every
// class in the bucket, each class read must see its own and nothing else. A
// filter that narrowed to the wrong token would return nothing; one that
// narrowed to nothing at all would return everything — and the client-side
// class test that remains would hide the second, so the count is checked from
// the inside of the walk rather than from the rows it produced.
func TestAPositionClassWalkSeesOnlyItsOwnClass(t *testing.T) {
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("p%d", bucketSeq.Add(1))
	store := openFleetForTest(t, nc, prefix)
	ctx := context.Background()

	// One key per class, all in the one register. All SEVEN of them, and
	// `maintenance` beside `maintenance-ack` is the adversarial pair: one
	// class name is a STRING PREFIX of the other, so a filter built by
	// concatenation rather than by the grammar would hand every
	// acknowledgement to the capacity walk. A subject wildcard matches per
	// TOKEN, so it does not — and that is the property this pair pins.
	classes := []string{
		"node", "floor", "hold", "backup",
		"maintenance", "admitted", "maintenance-ack",
	}
	for _, class := range classes {
		key := coord.DocumentKey(class, "n-1")
		if _, err := store.positions.Put(ctx, key, []byte(`{}`)); err != nil {
			t.Fatalf("seed %s: %v", class, err)
		}
	}

	for _, class := range classes {
		var reached, kept int
		err := store.eachUnder(ctx, store.positions, coord.DocumentFilter(class), class,
			func(kve jetstream.KeyValueEntry) error {
				reached++
				if segs, ok := coord.DocumentSegments(kve.Key()); ok && segs[0] == class {
					kept++
				}
				return nil
			})
		if err != nil {
			t.Fatalf("walk %s: %v", class, err)
		}
		if kept != 1 {
			t.Errorf("class %s saw %d of its own keys, want 1", class, kept)
		}
		// THE NARROWING ITSELF. Every record that reaches the walk was moved
		// over the wire, so a filter that did not narrow shows up here as the
		// whole bucket even though the rows it yields are still correct.
		if reached != 1 {
			t.Errorf("class %s was handed %d records for the 1 it wanted; the broker "+
				"is not filtering and the walk is moving the other classes too",
				class, reached)
		}
	}
}

// A CLASS FILTER MATCHES A KEY OF ANY DEPTH, which is the whole difference
// between the `>` [coord.DocumentFilter] ends in and the `*` that would look
// equivalent today.
//
// Every key class in this register is two segments deep right now, so `*`
// matches all of them and the difference is invisible — until the first class
// that composes a third segment, whose listing then silently returns nothing.
// A walk that finds no rows and a class that has no rows are the same answer
// at every caller, so the day that happens there is no symptom to notice.
func TestAClassFilterMatchesAKeyOfAnyDepth(t *testing.T) {
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("p%d", bucketSeq.Add(1))
	store := openFleetForTest(t, nc, prefix)
	ctx := context.Background()

	want := map[string]bool{
		coord.DocumentKey("node", "n-1"):               false,
		coord.DocumentKey("node", "n-2", "shard-a"):    false,
		coord.DocumentKey("node", "n-3", "shard", "b"): false,
	}
	for key := range want {
		if _, err := store.positions.Put(ctx, key, []byte(`{}`)); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	err := store.eachUnder(ctx, store.positions, coord.DocumentFilter("node"), "node",
		func(kve jetstream.KeyValueEntry) error {
			if _, ok := want[kve.Key()]; !ok {
				t.Errorf("the walk was handed %q, which it did not seed", kve.Key())
				return nil
			}
			want[kve.Key()] = true
			return nil
		})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("the class filter did not match %q; a filter that matches only "+
				"one depth stops selecting a class the day it grows a segment, and "+
				"an empty listing is indistinguishable from an empty class", key)
		}
	}
}
