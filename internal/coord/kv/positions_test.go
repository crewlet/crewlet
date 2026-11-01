package kv

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE POSITIONS REGISTER HAS NO TTL, and this is the case that says why.
//
// Every other bucket in this estate expires its keys — that is what a lease,
// a claim, a dedupe window and a rate window ARE. The register is the one bucket
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

	if _, _, err := leases.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
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
	store, err := OpenFleet(context.Background(), jsOf(nc), FleetConfig{
		BucketPrefix:     fmt.Sprintf("p%d", bucketSeq.Add(1)),
		MaxRateWindow:    ttl,
		MaxClaimTTL:      ttl,
		LedgerRetention:  ttl,
		FireRetention:    ttl,
		FollowRetention:  ttl,
		RebaseRetention:  ttl,
		BudgetRetention:  ttl,
		StatusFreshness:  ttl,
		CustodyRetention: ttl,
	})
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	return store
}

// THE BROKER FILTERS THE CLASS, AND THE WALK ONLY SEES ITS OWN.
//
// Eight classes share the positions register, and a walk that read all of them
// to use one was what this replaced — on a read the state-log write fence
// takes on every first write to a subject. A key here is
// coord.DocumentKey(class, id), whose separator is a dot because a key IS a
// subject token path, so the class is a token the broker can match.
//
// What this asserts is the NARROWING, through the walk every class listing
// takes ([FleetStore.eachPositionKey]): with keys of every class in the bucket,
// each class read must see its own and nothing else, AND the filter the walk
// asks the broker to watch must hand back its own key and nothing else. The
// walk's own class test drops a neighbour's key, so its rows stay right
// whether or not the broker narrowed; only the filter it asked for says
// whether every class was moved over the wire for one. Counted through a walk
// the test built itself, with a filter of its own, it held the grammar and
// never the listing.
func TestAPositionClassWalkSeesOnlyItsOwnClass(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("p%d", bucketSeq.Add(1))
	store := openFleetForTest(t, nc, prefix)
	ctx := context.Background()

	// One key per class, all in the one register. All EIGHT of them, and
	// `maintenance` beside `maintenance-ack` is the adversarial pair: one
	// class name is a STRING PREFIX of the other, so a filter built by
	// concatenation rather than by the grammar would hand every
	// acknowledgement to the capacity walk. A subject wildcard matches per
	// TOKEN, so it does not — and that is the property this pair pins.
	classes := []string{
		"node", "floor", "hold", "backup",
		"maintenance", "admitted", "maintenance-ack", coord.SeatPauseClass,
	}
	for _, class := range classes {
		key := coord.DocumentKey(class, "n-1")
		if _, err := store.positions.Put(ctx, key, []byte(`{}`)); err != nil {
			t.Fatalf("seed %s: %v", class, err)
		}
	}
	spy := &watchSpy{KeyValue: store.positions}
	store.positions = spy

	for _, class := range classes {
		var kept int
		err := store.eachPositionKey(ctx, class, class,
			func(key string, _ []byte, _ time.Time) error {
				if segs, ok := coord.DocumentSegments(key); ok && segs[0] == class {
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
		// THE NARROWING ITSELF. Every record the broker hands back for the
		// filter the walk asked for is moved over the wire, so a filter that
		// did not narrow shows up here as the whole bucket even though the
		// rows the walk yields are still correct.
		if reached := spy.handedBack(ctx, t, store); reached != 1 {
			t.Errorf("class %s's walk asks the broker for %d records for the 1 it "+
				"wanted; the broker is not filtering and the walk is moving the "+
				"other classes too", class, reached)
		}
	}
}

// A CLASS FILTER MATCHES A KEY OF ANY DEPTH, which is the whole difference
// between the `>` [coord.DocumentFilter] ends in and the `*` that would look
// equivalent today.
//
// Most key classes in this register are two segments deep, and a class
// filtered with `*` would match those and silently return nothing for the
// classes that compose a third segment: the operator's backup
// acknowledgements, filed under their log, and a log's maintenance
// acknowledgements.
// A walk that finds no rows and a class that has no rows are the same answer
// at every caller, so the day that happens there is no symptom to notice.
func TestAClassFilterMatchesAKeyOfAnyDepth(t *testing.T) {
	t.Parallel()
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

// ONE LOG'S MAINTENANCE ACKNOWLEDGEMENTS ARE NARROWED BY THE BROKER, on the
// log's own segment of the key ([coord.MaintenanceAckKey]).
//
// A window is per log and two can be open at once, so the seal reads one log's
// acknowledgements, and it is the broker that has to pick them out: a listing
// handed every log's and dropping the rest still answers right, which is why
// the count is taken from the filter MaintenanceAcks itself asks the broker to
// watch, never from a filter the test builds. A log whose name begins with
// another's is the adversarial neighbour for a filter built by concatenation
// rather than by the grammar.
//
// Mutation: have the walk filter on the class alone and every log's records
// are handed back to each log's listing, while its rows stay right.
func TestALogsAcknowledgementsAreNarrowedByTheBroker(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("p%d", bucketSeq.Add(1))
	store := openFleetForTest(t, nc, prefix)
	ctx := context.Background()

	logs := []string{"CREWLET_TRACKER_LOG", "CREWLET_TRACKER_LOG_2", "CREWLET_PAGES_LOG"}
	for _, stream := range logs {
		for _, node := range []string{"node-1", "node-2"} {
			if err := store.PutMaintenanceAck(ctx, coord.MaintenanceAck{
				Stream: stream, NodeID: node, Incarnation: node + ":1", Mode: "seal",
			}); err != nil {
				t.Fatalf("PutMaintenanceAck(%s, %s): %v", stream, node, err)
			}
		}
	}
	spy := &watchSpy{KeyValue: store.positions}
	store.positions = spy
	for _, stream := range logs {
		acks, err := store.MaintenanceAcks(ctx, stream)
		if err != nil {
			t.Fatalf("MaintenanceAcks(%s): %v", stream, err)
		}
		if len(acks) != 2 || acks[0].Stream != stream || acks[1].Stream != stream {
			t.Errorf("MaintenanceAcks(%s) = %+v, want its own two", stream, acks)
		}
		if reached := spy.handedBack(ctx, t, store); reached != 2 {
			t.Errorf("MaintenanceAcks(%s) asks the broker for %d records, want "+
				"its own 2: the broker is not narrowing on the log", stream, reached)
		}
	}
}

// watchSpy is the positions register as a listing reaches it, keeping the
// filter each pass asks the broker to watch.
type watchSpy struct {
	jetstream.KeyValue
	mu      sync.Mutex
	watched []string
}

func (s *watchSpy) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (
	jetstream.KeyWatcher, error) {

	s.mu.Lock()
	s.watched = append(s.watched, keys)
	s.mu.Unlock()
	return s.KeyValue.Watch(ctx, keys, opts...)
}

// handedBack is how many records the broker hands back for the one filter the
// listing since the last call watched: a walk of the register over that same
// filter, counted from inside the walk, before any class test of the
// listing's could drop one.
func (s *watchSpy) handedBack(ctx context.Context, t *testing.T, store *FleetStore) int {
	t.Helper()
	s.mu.Lock()
	watched := s.watched
	s.watched = nil
	s.mu.Unlock()
	if len(watched) != 1 {
		t.Fatalf("the listing watched %q, want exactly one filter", watched)
	}
	var reached int
	if err := store.eachUnder(ctx, s.KeyValue, watched[0], "the spy's count",
		func(jetstream.KeyValueEntry) error {
			reached++
			return nil
		}); err != nil {
		t.Fatalf("walk %q: %v", watched[0], err)
	}
	return reached
}
