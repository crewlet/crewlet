package jetstreamtest

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/coordtest"
	coordkv "github.com/crewlet/crewlet/internal/coord/kv"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A CREATE RACING OTHERS OVER A REMOVED RECORD IS A RACE ON A REPLICATED FLEET
// TOO, AND NEVER AN OUTAGE.
//
// A removal leaves a marker, and the client creates over one with a second
// conditional publish whose refusal it hands back unmapped — which a SOLO
// stream words with the code the client's "key exists" sentinel matches, and a
// REPLICATED one with a code it matches nothing. So the contract suite, which
// runs the KV backend on one server, passed a backend whose every create
// matching only the sentinel told a share of a race's losers that the store
// was down: on a clustered fleet a failed sign-in racing another node's to a
// just-flushed record went unrecorded and left its node's throttle on its own
// curve for half a minute, and a released delivery claim answered "unknown"
// and was processed twice. The same check [coordtest.RunFleet] runs, carried
// to the one substrate where it can fail.
//
// Mutation: match the failed attempt's create against the sentinel alone and
// Fail answers an error here while the single-server suite stays green.
func TestCreatesOverARemovedRecordAreRacesOnAReplicatedFleet(t *testing.T) {
	t.Parallel()
	c := StartCluster(t, 3, js.Config{})
	store, err := coordkv.OpenFleet(t.Context(), c.Client(t, 0).Conn(),
		coordkv.FleetConfig{
			BucketPrefix: "raced", Replicas: len(c.Servers), Clustered: true,
			RateWindow: time.Minute, ClaimTTL: 10 * time.Minute,
			SetupOnceRetention: 10 * time.Minute, AttemptWindow: 10 * time.Minute,
			LedgerRetention: 10 * time.Minute, FireRetention: 10 * time.Minute,
			FollowRetention: 10 * time.Minute, CooldownMax: time.Hour,
			StatusFreshness: 10 * time.Minute,
		})
	if err != nil {
		t.Fatalf("open a replicated fleet store: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, err := range coordtest.CheckCreatesOverARemovedRecordAreRaces(ctx, store,
		time.Now().UTC()) {
		t.Error(err)
	}
}
