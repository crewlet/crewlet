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
// The same check [coordtest.RunFleet] runs against one server, carried to the
// one substrate where it can fail. Every round stands on a set-up — a record
// written, read back and removed at the version read, a counter charged and
// reset — and on three members every step of that has a way to go wrong that
// one server never shows, each of which this case has met:
//
//   - A READ SERVED BY A REPLICA behind an acknowledged write: a run just
//     created read back as absent, a charge was counted short, and a reset
//     whose listing missed the counter it was clearing left the race after it
//     counting from one. coord/kv reads every key from the stream leader and
//     closes every listing on it.
//   - A LEADER'S "ANOTHER WRITE IS IN FLIGHT", read as a lost race: a removal
//     at the version its caller had just read was refused while the leader
//     still had that caller's own create in flight, and the race over a record
//     that was never removed had no winner. coord/kv waits that answer out.
//   - A LOSER TOLD THE STORE WAS DOWN: on a replicated stream a share of a
//     create race's losers came back as a refusal the client wrapped in
//     neither of its sentinels, and a create that matched only the sentinel
//     answered "unknown" — a released delivery claim processed twice.
//
// IT IS A RACE AND FAILS LIKE ONE: under load, a share of runs, and on an idle
// machine almost never. Each mechanism has a staged case that fails every time
// — TestAWalkServedByACopyThatIsBehindIsClosedByTheLeader and
// TestAConditionalWriteIsAnsweredByWhatTheLeaderDecided in coord/kv, and
// TestACoordinationReadIsNeverAnsweredByAMemberThatIsBehind here — and this
// one holds them together under the timing that produced the failures.
//
// Mutation, measured with eight CPU burners on four cores: the build before
// coord/kv read through the leader failed 9 runs of 12 ("a run just created
// read back as absent", "8 admitted and 7 counted"); make
// leaderBucket.closeOnLeader return without asking and 6 of 12 fail ("the
// counter just reset reads back as (1, <nil>)"). Handing back the leader's
// first answer from leaderBucket.settle failed 0 of 12 at that load — the
// in-flight answer is rarer than a stale replica — which is why that
// mechanism's guard is the staged case, not this one.
func TestCreatesOverARemovedRecordAreRacesOnAReplicatedFleet(t *testing.T) {
	t.Parallel()
	c := StartCluster(t, 3, js.Config{})
	store, err := coordkv.OpenFleet(t.Context(), c.Client(t, 0).Conn(),
		coordkv.FleetConfig{
			BucketPrefix: "raced", Replicas: len(c.Servers), Clustered: true,
			RateWindow: time.Minute, ClaimTTL: 10 * time.Minute, SetupOnceRetention: 10 * time.Minute,
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
