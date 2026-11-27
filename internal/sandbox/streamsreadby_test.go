package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// A READ'S STREAMS ARE GIVEN ONLY WHAT ITS WAIT LEAVES. The host hands its
// collection the instant its reads of the run's streams end
// ([RunHandle.StreamsReadBy]): the read's deadline less what follows the
// collection ([afterCollect]) and the pieces it reads whole
// ([CollectReadWait]). A read that spends both at their bounds still hands its
// take the take's whole budget, whether its asker carried this build's wait or
// an older build's, which held no room for the whole pieces. A read under no
// deadline sets none.
//
// Mutations, each red: hand the collection no StreamsReadBy; leave the whole
// pieces' room, or what follows the collection, out of it.
func TestAReadsStreamsAreGivenOnlyWhatItsWaitLeaves(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	host := rig.hostOn(t, "node-a", rig.pending)
	traits := ThreadTraitsOf(rig.runner)
	var run PendingRun
	for _, c := range []struct {
		turn     string
		deadline bool
	}{{"t1", true}, {"t2", false}} {
		run = rig.launchedBy(c.turn, "node-a")
		if _, won, err := rig.pending.ClaimForResume(t.Context(), c.turn, CompletionTail(run.LaunchID),
			rigLease); err != nil || !won {
			t.Fatalf("ClaimForResume = %v, %v", won, err)
		}
		before := len(rig.runner.Collected())
		ctx, cancel := context.WithCancel(t.Context())
		if c.deadline {
			ctx, cancel = context.WithTimeout(t.Context(), time.Hour)
		}
		AnswerBoxRead(ctx, host, BoxReadRequest{Version: BoxReadWireVersion, ID: "read-" + c.turn, Node: "node-a",
			TurnID: c.turn, LaunchID: run.LaunchID, SandboxID: run.SandboxID, Read: BoxReadCollect})
		cancel()
		handles := rig.runner.Collected()
		if len(handles) != before+1 {
			t.Fatalf("the host's read of %s collected nothing; the case needs one that did", c.turn)
		}
		got, want := handles[len(handles)-1].StreamsReadBy, streamsReadBy(ctx, run, BoxReadCollect, traits)
		if !got.Equal(want) || want.IsZero() == c.deadline {
			t.Errorf("a read of %s (deadline %v) handed its collection %v to end its streams' reads by, "+
				"want %v", c.turn, c.deadline, got, want)
		}
	}

	budget := BoxReadBudget(run, BoxReadCollect, ThreadTraits{})
	// A VARIABLE, so the quotient is no constant a conversion refuses.
	collect := CollectReadWait
	for name, wait := range map[string]time.Duration{
		"this build's wait": budget,
		"an older build's":  budget - time.Duration(float64(collect)/BoxReadAnswerShare),
	} {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				// A STORE AS SLOW AS ANY NODE'S, so the take's records need
				// all the room the budget gives them.
				rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
					o.Pending = &crossingWrites{PendingStore: o.Pending, crossing: coord.MaxValueCrossing,
						readCrossing: coord.MaxValueReadCrossing, given: map[string][]handed{}}
				})
				share := time.Duration(float64(wait) * BoxReadAnswerShare)
				ctx, cancel := context.WithTimeout(t.Context(), share)
				defer cancel()
				by := streamsReadBy(ctx, run, BoxReadCollect, ThreadTraits{})
				// THE COLLECTION SPENDS ALL IT MAY: its error stream to by,
				// then every whole piece at its bound, then the take's reads
				// of the row and of who reads its media.
				until := by.Add(CollectReadWait + RowReadBudget(rig.coordinator.pending, rig.coordinator.recordBudget) +
					MediaReadersRoom)
				if wait := time.Until(until); wait > 0 {
					time.Sleep(wait)
				}
				take, cancelTake, _ := rig.coordinator.takeContext(ctx, atKeptBound)
				defer cancelTake()
				deadline, _ := take.Deadline()
				whole := SessionTakeBudget + rig.coordinator.reserveRoom()
				if left := time.Until(deadline); left < whole-time.Nanosecond {
					t.Errorf("a take after a collection at its bounds is given %v, want its whole %v", left, whole)
				}
			})
		})
	}
}

// A READ OF A BOX COUNTS WHAT IT WITHHOLDS TO THE NODE'S METER: the read hands
// its collection a context carrying [CoordinatorOptions.Collects], which the
// runner counts each piece it refuses or does not show to ([MeterOf]); a node
// given none hands none.
//
// Mutation: read the box under the context it was asked under.
func TestAReadOfABoxCountsWhatItWithholdsToTheNodesMeter(t *testing.T) {
	t.Parallel()
	meter := &countedMeter{}
	for name, given := range map[string]CollectMeter{"given one": meter, "given none": nil} {
		rig := newCoordRigWith(t, func(o *CoordinatorOptions) { o.Collects = given })
		var handed CollectMeter
		rig.runner.CollectFunc = func(ctx context.Context) error {
			handed = MeterOf(ctx)
			return nil
		}
		run := rig.launchedBy("t1", "")
		if _, _, err := rig.coordinator.readBox(t.Context(), run, BoxReadCollect); err != nil {
			t.Fatalf("%s: readBox: %v", name, err)
		}
		if handed != given {
			t.Errorf("a node %s handed its collection the meter %v, want %v", name, handed, given)
		}
	}
}

// countedMeter is a meter a case hands a node.
type countedMeter struct{}

func (*countedMeter) Withheld(Withholding) {}
