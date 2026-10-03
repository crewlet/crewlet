package kv

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// openMapsFleet is a fleet store on the embedded broker for the placement
// maps' own cases.
func openMapsFleet(t *testing.T) *FleetStore {
	t.Helper()
	nc := embeddedNATS(t)
	store, err := OpenFleet(context.Background(), jsOf(nc), FleetConfig{
		BucketPrefix: fmt.Sprintf("m%d", bucketSeq.Add(1)),
		RateWindow:   time.Minute, ClaimTTL: time.Minute,
		LedgerRetention: time.Minute, FireRetention: time.Minute,
		FollowRetention: time.Minute, BudgetRetention: time.Minute, RebaseRetention: time.Minute,
		CooldownMax: time.Minute, StatusFreshness: time.Minute,
	})
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	return store
}

// A REMOVED ESTATE MAP ENDS EVERY WATCH OF IT. Nothing in the engine removes
// the map, so a removal is an operator's hand on the bucket — and a watch that
// went on delivering nothing would leave a node routing by a map that no
// longer exists, where a closed one sends it to read the store again and be
// told there is none. The contract suite cannot stage it: the contract has no
// removal verb, because the engine never needs one.
func TestARemovedEstateMapEndsTheWatch(t *testing.T) {
	store := openMapsFleet(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	if _, ok, err := store.CreateEstateMap(ctx, []byte(`{"epoch":1}`)); err != nil || !ok {
		t.Fatalf("CreateEstateMap = (%v, %v)", ok, err)
	}
	ch, err := store.WatchEstateMap(ctx)
	if err != nil {
		t.Fatalf("WatchEstateMap: %v", err)
	}
	if rec := <-ch; string(rec.Value) != `{"epoch":1}` {
		t.Fatalf("the watch opened on %s", rec.Value)
	}
	if err := store.estate.Purge(ctx, mapKey); err != nil {
		t.Fatalf("purging the map: %v", err)
	}
	select {
	case rec, ok := <-ch:
		if ok {
			t.Fatalf("the watch delivered %s at %d after the map was removed, "+
				"where it should have ended", rec.Value, rec.Version)
		}
	case <-ctx.Done():
		t.Fatal("the watch was still open after the map was removed")
	}
	if _, found, err := store.EstateMap(ctx); err != nil || found {
		t.Fatalf("EstateMap after the removal = (found %v, %v)", found, err)
	}

	// And a watch opened NOW finds no map: the removal is the current
	// value, which is the absence of one, so nothing is delivered until a
	// map is written again — and then that map is.
	again, err := store.WatchEstateMap(ctx)
	if err != nil {
		t.Fatalf("WatchEstateMap: %v", err)
	}
	created, ok, err := store.CreateEstateMap(ctx, []byte(`{"epoch":2}`))
	if err != nil || !ok {
		t.Fatalf("CreateEstateMap over the removal = (%v, %v)", ok, err)
	}
	select {
	case rec, ok := <-again:
		if !ok || string(rec.Value) != `{"epoch":2}` || rec.Version != created.Version {
			t.Fatalf("the watch opened over the removal delivered (%s at %d, open %v), "+
				"want the new map at %d", rec.Value, rec.Version, ok, created.Version)
		}
	case <-ctx.Done():
		t.Fatal("the watch opened over the removal never delivered the new map")
	}
}

var _ coord.EstateMaps = (*FleetStore)(nil)

// A WATCH ON A COPY BEHIND THE LEADER NEVER HANDS OVER AN OLDER MAP. The
// broker places a watch's consumer on whichever replica it picks, and that
// replica may not yet have the version the leader holds — so a node that had
// just read or written version V would be handed V-1 first and act on a map
// the fleet had replaced. The map is read from the leader and handed over
// first, and what the watch offers after it is forwarded only when it is newer
// than everything already accounted for: the leader's map, or a removal the
// leader says the map is past.
//
// Staged on the forwarding itself, because the lag cannot be staged on a
// broker: which replica serves the consumer is the broker's random pick, and a
// case that waited for it to pick a lagging one would prove nothing on the
// runs where it did not.
func TestAWatchOnACopyBehindTheLeaderNeverHandsOverAnOlderMap(t *testing.T) {
	t.Parallel()
	put := func(v uint64) jetstream.KeyValueEntry {
		return leaderEntry{bucket: "estate", key: mapKey, value: fmt.Appendf(nil, "v%d", v),
			revision: v, op: jetstream.KeyValuePut}
	}
	del := func(v uint64) jetstream.KeyValueEntry {
		return leaderEntry{bucket: "estate", key: mapKey, revision: v, op: jetstream.KeyValueDelete}
	}
	for name, c := range map[string]struct {
		current jetstream.KeyValueEntry // the leader's newest message, nil for none
		offered []jetstream.KeyValueEntry
		want    []uint64
	}{
		"the replica is behind the leader's map": {
			current: put(5),
			offered: []jetstream.KeyValueEntry{put(4), nil, put(5), put(6)},
			want:    []uint64{5, 6},
		},
		"the leader has no map": {
			offered: []jetstream.KeyValueEntry{nil, put(3)},
			want:    []uint64{3},
		},
		"the leader's newest is a removal the replica has not seen": {
			current: del(7),
			offered: []jetstream.KeyValueEntry{put(6), nil, put(8)},
			want:    []uint64{8},
		},
		"a removal after a map was handed over ends the watch": {
			current: put(1),
			offered: []jetstream.KeyValueEntry{del(2), put(3)},
			want:    []uint64{1},
		},
		"a removal before any map was handed over is its absence": {
			offered: []jetstream.KeyValueEntry{del(2), put(3)},
			want:    []uint64{3},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			updates := make(chan jetstream.KeyValueEntry, len(c.offered))
			for _, e := range c.offered {
				updates <- e
			}
			close(updates)
			out := make(chan coord.EstateMapRecord)
			go func() {
				defer close(out)
				forwardEstateMap(t.Context(), c.current, updates, out, "estate")
			}()
			var got []uint64
			for rec := range out {
				if want := fmt.Sprintf("v%d", rec.Version); string(rec.Value) != want {
					t.Fatalf("version %d handed over as %q, want %q", rec.Version, rec.Value, want)
				}
				got = append(got, rec.Version)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("the watch handed over %v, want %v", got, c.want)
			}
		})
	}
}
