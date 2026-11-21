package jetstreamtest

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	coordkv "github.com/crewlet/crewlet/internal/coord/kv"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A CREATE RACING OTHERS OVER A REMOVED RECORD IS A RACE ON A REPLICATED FLEET
// TOO, AND NEVER AN OUTAGE.
//
// [coordtest.RunFleet]'s check where it can fail; three members add:
//   - A READ SERVED BY A REPLICA behind an acknowledged write (coord/kv reads
//     from the stream leader).
//   - A LEADER'S "ANOTHER WRITE IS IN FLIGHT" read as a lost race (coord/kv
//     waits it out, internal/jsinflight).
//   - A LOSER TOLD THE STORE WAS DOWN: a delivery claim processed twice.
//
// IT FAILS LIKE A RACE, under load; each mechanism's staged case fails every
// time. No round lists: a windowed counter is rolled inside its charge (ADR-0019).
func TestCreatesOverARemovedRecordAreRacesOnAReplicatedFleet(t *testing.T) {
	t.Parallel()
	c := StartCluster(t, 3, js.Config{})
	store, err := coordkv.OpenFleet(t.Context(), c.Client(t, 0).JetStream(),
		coordkv.FleetConfig{
			BucketPrefix: "raced", Replicas: len(c.Servers), Clustered: true, Reach: coordkv.InProcess,
			MaxRateWindow: time.Minute, MaxClaimTTL: 10 * time.Minute,
			LedgerRetention: 10 * time.Minute, FireRetention: 10 * time.Minute,
			FollowRetention: 10 * time.Minute, RebaseRetention: 10 * time.Minute,
			BudgetRetention: coord.BudgetRetention,
			StatusFreshness: 10 * time.Minute, CustodyRetention: 10 * time.Minute,
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
