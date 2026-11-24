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
// Every other bucket expires its keys (that is what a lease, claim or window
// IS). Here a vanished key reads as a fact: a node whose position expired has
// applied NOTHING, and the trim takes a MINIMUM across rows, so the floor
// collapses to zero and the fleet stops trimming, or trims past a merely quiet
// node.
//
// The test opens the store with a one-second TTL and writes a lease and a
// position together: the lease must be gone while the position is not.
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
		Reach:            InProcess,
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
// Many classes share the positions register. A key is
// coord.DocumentKey(class, id), dotted because a key IS a subject token path,
// so the class is a token the broker can match.
//
// Asserts the NARROWING through the walk every class listing takes
// ([FleetStore.eachPositionKey]): each class read sees only its own keys, AND
// the filter the walk asks the broker to watch hands back only its own. The
// walk drops a neighbour's key itself, so rows stay right whether or not the
// broker narrowed; only the filter says whether every class crossed the wire.
// It must be counted through the walk's own filter, not one the test builds.
func TestAPositionClassWalkSeesOnlyItsOwnClass(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("p%d", bucketSeq.Add(1))
	store := openFleetForTest(t, nc, prefix)
	ctx := context.Background()

	// One key per class, all in the one register. All TEN of them, and
	// `maintenance` beside `maintenance-ack` is the adversarial pair: one
	// class name is a STRING PREFIX of the other, so a filter built by
	// concatenation rather than by the grammar would hand every
	// acknowledgement to the capacity walk. A subject wildcard matches per
	// TOKEN, so it does not — and that is the property this pair pins.
	classes := []string{
		coord.PositionClass, coord.FloorClass, coord.HoldClass, coord.BackupPointClass,
		coord.MaintenanceClass, coord.AdmissionClass, coord.MaintenanceAckClass, coord.SeatPauseClass,
		coord.ConversationHoldClass, coord.AtlasWakeClass,
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
		coord.DocumentKey(coord.PositionClass, "n-1"):               false,
		coord.DocumentKey(coord.PositionClass, "n-2", "shard-a"):    false,
		coord.DocumentKey(coord.PositionClass, "n-3", "shard", "b"): false,
	}
	for key := range want {
		if _, err := store.positions.Put(ctx, key, []byte(`{}`)); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	err := store.eachUnder(ctx, store.positions, coord.DocumentFilter(coord.PositionClass), coord.PositionClass,
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
// Two logs' windows can be open at once, and the broker must pick out one
// log's acknowledgements: a listing handed all and dropping the rest still
// answers right, so the count comes from the filter MaintenanceAcks itself
// asks the broker to watch. A log named as a prefix of another is the
// adversarial neighbour for a filter built by concatenation.
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

// holdFor is the hold owner holds in holds, and whether there is one.
func holdFor(holds []coord.TrimHold, owner string) (coord.TrimHold, bool) {
	for _, h := range holds {
		if h.Owner == owner {
			return h, true
		}
	}
	return coord.TrimHold{}, false
}

// putHoldReason writes owner's hold on one stream, saying why.
func putHoldReason(ctx context.Context, t *testing.T, f *FleetStore, owner, reason string) {
	t.Helper()
	if err := f.PutHold(ctx, coord.TrimHold{Owner: owner, Reason: reason,
		Streams: map[string]coord.Position{"S": {Stream: "S", Seq: 1}},
	}); err != nil {
		t.Fatalf("put %s's hold (%s): %v", owner, reason, err)
	}
}

// A TRIM HOLD A MEMBER HANDS BACK STALE IS READ FROM THE LEADER.
//
// The pass reads one random member's copy, and the listing trusts a live Put
// it delivers. The trim ignores a hold whose one write it has watched stand
// unrenewed past its bound, so a member still holding a write the holder has
// since renewed would have it ignore a live pin. Each listed hold is read
// again from the leader: its newest write, with that write's revision.
func TestATrimHoldAMemberHandsBackStaleIsReadFromTheLeader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	f := openFleet(t, nc)
	owner := coord.HoldOwner("n0", "backup")
	putHoldReason(ctx, t, f, owner, "first")
	behind := recordPass(ctx, t, f.positions)
	putHoldReason(ctx, t, f, owner, "second")
	read, err := newLeaderReader(f.js, f.positions)
	if err != nil {
		t.Fatalf("address the leader: %v", err)
	}
	newest, err := read.live(ctx, coord.HoldKey(owner))
	if err != nil {
		t.Fatalf("read the renewal from the leader: %v", err)
	}

	f.positions = replayKV{KeyValue: f.positions, pass: behind}
	holds, err := f.Holds(ctx)
	if err != nil {
		t.Fatalf("Holds: %v", err)
	}
	got, ok := holdFor(holds, owner)
	if !ok || got.Reason != "second" {
		t.Fatalf("the listing holds %+v, want %s's renewal (\"second\"): the "+
			"write a member that is behind handed back is one the holder has "+
			"since replaced", holds, owner)
	}
	if got.Revision != newest.Revision() {
		t.Fatalf("the renewal was listed at revision %d, want the leader's %d: "+
			"the trim names a write by its revision, and an older one reads as "+
			"a pin left unrenewed", got.Revision, newest.Revision())
	}
}

// A TRIM HOLD RELEASED SINCE A MEMBER'S COPY IS NOT LISTED.
//
// A released hold a member still hands back as live is the holder's last
// write standing unrenewed for ever: the trim would watch it and ignore it,
// or pin on it, but either way it reads a pin nobody holds. The leader's
// answer, that the key holds no live value, is the listing's.
func TestATrimHoldReleasedSinceAMembersCopyIsNotListed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	f := openFleet(t, nc)
	owner := coord.HoldOwner("n0", "backup")
	putHoldReason(ctx, t, f, owner, "copying the log")
	behind := recordPass(ctx, t, f.positions)
	if err := f.ReleaseHold(ctx, owner); err != nil {
		t.Fatalf("ReleaseHold: %v", err)
	}

	f.positions = replayKV{KeyValue: f.positions, pass: behind}
	holds, err := f.Holds(ctx)
	if err != nil {
		t.Fatalf("Holds: %v", err)
	}
	if len(holds) != 0 {
		t.Fatalf("the listing holds %+v after %s released its hold: the live "+
			"Put a member that is behind handed back is not the leader's answer",
			holds, owner)
	}
}
