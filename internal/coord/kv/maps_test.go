package kv

import (
	"context"
	"fmt"
	"testing"
	"time"

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
		FollowRetention: time.Minute,
		CooldownMax:     time.Minute, StatusFreshness: time.Minute,
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
