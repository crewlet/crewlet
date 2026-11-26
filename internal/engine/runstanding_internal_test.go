package engine

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// standingLinkRate is the pace the case holds a leaf's link to, each way: 160
// KiB a second, about five times the slowest a link is allowed, so a run's
// record at its bound takes about thirty-eight seconds to cross raw.
const standingLinkRate = 160 << 10

// A LEAF BOOTS BESIDE RUNS AT THEIR BOUND. Every boot floors each seat's
// fencing counter above its runs' epochs before claiming a seat, inside the
// boot's one ceiling. Read off the records, three at their bound took a leaf
// at the slowest pace allowed past that ceiling, and the node never started;
// off their standing the floor moves headers. The ceiling here is the boot's,
// scaled from the slowest pace to this link's, and the link's count is the
// signal that no record crossed.
//
// Mutation: floor off the listing of records, or let a run's change state no
// standing, and the floor outlasts its ceiling and the records cross.
func TestALeafBootsBesideRunsAtTheirBound(t *testing.T) {
	t.Parallel()
	pace := time.Second * (1 << 20) / standingLinkRate
	// In milliseconds, since two durations multiplied overflow.
	ceiling := jsprovision.SequenceBudget(true) * (pace / time.Millisecond) / (objstore.MiBPace / time.Millisecond)
	across := kvtest.SlowLeaf(t, standingLinkRate)
	member := sandbox.NewCoordStore(across.Member)
	seat, epoch := uuid.New(), int64(41)
	for i := range 3 {
		turn := fmt.Sprintf("t%d", i+1)
		row := fmt.Sprintf(`{"turn_id":%q,"agent_id":%q,"agent_handle":"swe","status":"running",`+
			`"launch_id":"l1","launch_seq":1}`, turn, seat)
		if made, err := across.Member.CreateSandboxRun(t.Context(), turn, []byte(row), nil); err != nil || !made {
			t.Fatalf("seed run %s = (%v, %v)", turn, made, err)
		}
		// THE CLAIM'S WRITE, a change of the row, is what states it here.
		if won, err := member.ClaimOwnership(t.Context(), turn, "n1", epoch-int64(i)); err != nil || !won {
			t.Fatalf("stamp run %s with a lease = (%v, %v)", turn, won, err)
		}
		padToTheBound(t, across, turn)
	}
	nc, err := across.LeafBroker.Conn()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	leaf, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatal(err)
	}
	epochs, err := kv.OpenEpochs(t.Context(), leaf, leaf, kv.EpochsConfig{BucketPrefix: "slowleaf",
		Reach: kv.AcrossLink})
	if err != nil {
		t.Fatalf("open the epochs counter across the link: %v", err)
	}

	before := across.Link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried
	ctx, cancel := context.WithTimeout(t.Context(), ceiling)
	defer cancel()
	if err := floorSeatEpochs(ctx, epochs, across.Leaf, "on the members this leaf joins"); err != nil {
		t.Fatalf("the floor across a %v-a-MiB link, given the boot's ceiling at that pace (%v) = %v",
			pace, ceiling, err)
	}
	if carried := across.Link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried - before; carried >= 64<<10 {
		t.Errorf("the floor carried %d bytes to the leaf: a run's record crossed", carried)
	}
	next, err := epochs.Next(t.Context(), coord.SeatResource(seat))
	if err != nil || next != epoch+1 {
		t.Errorf("the seat's next epoch = (%d, %v), want %d: the counter stands at its runs' highest", next,
			err, epoch+1)
	}
}

// padToTheBound grows turn's claimed record to its bound in one change that
// states what the claim's write stated (the listing of standings), so the
// record at its bound stands as the claim left it.
//
// THE CLAIM IS TAKEN SMALL, the pad after: a claim is a read and a write of the
// record, and on a member's own connection the read is asked again every two
// seconds while nobody answers (internal/coord/kv's runs class). The leader
// builds the reply of a record at its bound again for every ask, seconds each
// under the race detector beside the package's other cases, until the claim
// runs out its bound before the boot this case is about.
func padToTheBound(t *testing.T, across kvtest.Across, turn string) {
	t.Helper()
	stood, err := across.Member.SandboxRunStandings(t.Context())
	if err != nil {
		t.Fatalf("list the runs' standing: %v", err)
	}
	i := slices.IndexFunc(stood, func(r coord.StoodRun) bool { return r.TurnID == turn })
	if i < 0 {
		t.Fatalf("run %s is not among the runs' standing %v", turn, stood)
	}
	if _, changed, err := across.Member.MutateSandboxRun(t.Context(), turn,
		func(read coord.Record) (coord.SandboxRunChange, error) {
			return coord.SandboxRunChange{Put: kvtest.AtTheBound(t, read.Value, 8<<10),
				Standing: stood[i].Standing}, nil
		}); err != nil || !changed {
		t.Fatalf("pad run %s to its bound = (%v, %v)", turn, changed, err)
	}
}
