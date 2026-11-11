package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CALL'S RECORD IS ONE ENTRY, UPDATED BY ITS IDENTITY, end to end: through the
// real launcher, bridge, ledger and run store. While the call's tool runs, the
// run's log holds its entry in flight — its identity, written before the tool
// ran — and no finished call; once the tool returns, the log holds the call
// once, finished, with what it answered, at the instant its entry named, and no
// entry: the update took the entry out in the same write it listed the call.
//
// Mutation: list the call without taking its entry out, and the log holds it
// twice once it returns; write no entry ahead of it, and the log holds nothing
// while it runs.
func TestACallsRecordIsUpdatedOnceByItsIdentity(t *testing.T) {
	t.Parallel()
	const turnID = "t-one-record"
	delegate := newHeldTool("delegate")
	run := launchBridged(t, turnID, delegate)
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		run.call(t, "delegate")
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case <-delegate.entered:
	case <-ctx.Done():
		t.Fatal("the bridged call never reached its tool")
	}
	held := run.calls(t, turnID)
	if len(held.InFlight) != 1 || held.InFlight[0].Name != "delegate" || held.InFlight[0].Seq != 1 ||
		len(held.Calls) != 0 {
		t.Fatalf("while its tool runs the log holds %+v in flight and %+v finished, want the call's entry alone",
			held.InFlight, held.Calls)
	}
	close(delegate.release)
	<-answered
	done := run.calls(t, turnID)
	if len(done.InFlight) != 0 || len(done.Calls) != 1 || done.Calls[0].Name != "delegate" ||
		done.Calls[0].Output != "delegate done" || !done.Calls[0].StartedAt.Equal(held.InFlight[0].StartedAt) {
		t.Errorf("once its tool returned the log holds %+v in flight and %+v finished, want the call once, "+
			"finished at its entry's identity", done.InFlight, done.Calls)
	}
}

// A RESUMED TURN THAT SHOWS NO REPLY BESIDE A CALL WHOSE OUTCOME IS UNKNOWN IS
// NOT KNOWN TO HAVE FAILED TO REPLY. A call the round's log held in flight as its
// collection read it — or a closed round's record holds so — may be the reply,
// so the turn's end says whether it replied is unknown, never that it did not;
// a turn whose record shows its reply, or holds no such call, is read as it was.
//
// Mutation: read a resumed turn's delivery off its record alone, and a reply
// still running is said not to have gone.
func TestAResumedTurnsDeliveryIsUnknownBesideACallWhoseOutcomeIsUnknown(t *testing.T) {
	t.Parallel()
	reply := turn.Reply{Kind: turn.ReplyTool, Surface: "vendor"}
	inFlight := resumeInput{Bridged: sandbox.BridgeLog{InFlight: []sandbox.BridgeCall{{Name: "vendor_post", Seq: 1}}}}
	closed := resumeInput{}
	closed.State.Iterations = []ledger.Iteration{{Iteration: 1,
		Calls: []ledger.Call{{Name: "vendor_post", Failed: true, Unfinished: true}}}}
	for name, tc := range map[string]struct {
		in        resumeInput
		delivered bool
		unknown   bool
	}{
		"a call in flight as its log was read": {in: inFlight, unknown: true},
		"a closed round's call so read":        {in: closed, unknown: true},
		"no call whose outcome is unknown":     {in: resumeInput{}},
		"a reply the record shows":             {in: inFlight, delivered: true},
	} {
		ending := resumedEnding(tc.in, turn.Result{Delivered: tc.delivered}, reply, colleagueAnswer{})
		if ending.Delivered != tc.delivered || ending.DeliveryUnknown != tc.unknown {
			t.Errorf("%s: the turn ended delivered %v, unknown %v; want %v, %v", name, ending.Delivered,
				ending.DeliveryUnknown, tc.delivered, tc.unknown)
		}
	}
}
