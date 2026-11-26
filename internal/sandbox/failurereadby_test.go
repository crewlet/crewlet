package sandbox

import (
	"context"
	"testing"
	"time"
)

// A READ'S ERROR STREAM IS GIVEN ONLY WHAT ITS WAIT LEAVES. The host hands its
// collection the instant its error stream's read ends ([RunHandle.FailureReadBy]):
// the read's deadline less what follows the collection ([afterCollect]) and the
// pieces it reads whole ([collectWholeWait]). A read that spends both at their
// bounds still hands its take the take's whole budget, whether its asker
// carried this build's wait or an older build's, which held no room for the
// collection's reads ([CollectReadWait]). A read under no deadline sets none.
//
// Mutations, each red: hand the collection no FailureReadBy; leave the whole
// pieces' room, or what follows the collection, out of it.
func TestAReadsErrorStreamIsGivenOnlyWhatItsWaitLeaves(t *testing.T) {
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
		got, want := handles[len(handles)-1].FailureReadBy, failureReadBy(ctx, run, BoxReadCollect, traits)
		if !got.Equal(want) || want.IsZero() == c.deadline {
			t.Errorf("a read of %s (deadline %v) handed its collection %v to end its error stream's read by, "+
				"want %v", c.turn, c.deadline, got, want)
		}
	}

	budget := BoxReadBudget(run, BoxReadCollect, ThreadTraits{})
	for name, wait := range map[string]time.Duration{
		"this build's wait": budget,
		"an older build's":  budget - time.Duration(float64(CollectReadWait)/BoxReadAnswerShare),
	} {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				rig := newCoordRig(t)
				share := time.Duration(float64(wait) * BoxReadAnswerShare)
				ctx, cancel := context.WithTimeout(t.Context(), share)
				defer cancel()
				by := failureReadBy(ctx, run, BoxReadCollect, ThreadTraits{})
				// THE COLLECTION SPENDS ALL IT MAY: its error stream to by,
				// then every whole piece at its bound, then the take's reads
				// of the row and of who reads its media.
				until := by.Add(collectWholeWait + RowReadBudget(rig.coordinator.pending, rig.coordinator.recordBudget) +
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
