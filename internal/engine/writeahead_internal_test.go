package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// A CALL'S RECORD IS ONE ENTRY, UPDATED BY ITS IDENTITY, end to end through the
// real launcher, bridge, ledger and run store. While the tool runs, the run's log
// holds the call's entry in flight (written before the tool ran) and no finished
// call; once it returns, the log holds the call once, finished at the instant its
// entry named, and no entry: one write lists the call and takes the entry out.
//
// Mutation: list the call without taking its entry out, and it is held twice;
// write no entry ahead of it, and the log holds nothing while it runs.
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

// A RESUMED TURN BESIDE A CALL WHOSE OUTCOME IS UNKNOWN IS NOT KNOWN TO HAVE
// FAILED TO REPLY. Such a call (in flight as collection read the round's log, or
// so recorded by a closed round, listed or counted in a folded tally) may be the
// reply, so the turn's end says delivery is unknown, never that it did not reply;
// a record showing the reply, or holding no such call, is read as it was.
//
// Mutation: read delivery off the record alone, and a reply still running is
// said not to have gone; read a closed round's listed calls alone, and a park
// that folded them says so too.
func TestAResumedTurnsDeliveryIsUnknownBesideACallWhoseOutcomeIsUnknown(t *testing.T) {
	t.Parallel()
	reply := turn.Reply{Kind: turn.ReplyTool, Surface: "vendor"}
	inFlight := resumeInput{Bridged: sandbox.BridgeLog{InFlight: []sandbox.BridgeCall{{Name: "vendor_post", Seq: 1}}}}
	closed := resumeInput{}
	closed.State.Iterations = []ledger.Iteration{{Iteration: 1,
		Calls: []ledger.Call{{Name: "vendor_post", Failed: true, Unfinished: true}}}}
	tallied := resumeInput{}
	tallied.State.Iterations = []ledger.Iteration{{Iteration: 1,
		Elided: []ledger.Elided{{Name: "vendor_post", Calls: 3, Failed: 1, Unfinished: 1}}}}
	failedOnly := resumeInput{}
	failedOnly.State.Iterations = []ledger.Iteration{{Iteration: 1,
		Elided: []ledger.Elided{{Name: "vendor_post", Calls: 3, Failed: 1}}}}
	for name, tc := range map[string]struct {
		in        resumeInput
		delivered bool
		unknown   bool
	}{
		"a call in flight as its log was read":   {in: inFlight, unknown: true},
		"a closed round's call so read":          {in: closed, unknown: true},
		"a closed round's call so read, tallied": {in: tallied, unknown: true},
		"a tally of failures alone":              {in: failedOnly},
		"no call whose outcome is unknown":       {in: resumeInput{}},
		"a reply the record shows":               {in: inFlight, delivered: true},
	} {
		ending := resumedEnding(tc.in, turn.Result{Delivered: tc.delivered}, reply, colleagueAnswer{})
		if ending.Delivered != tc.delivered || ending.DeliveryUnknown != tc.unknown {
			t.Errorf("%s: the turn ended delivered %v, unknown %v; want %v, %v", name, ending.Delivered,
				ending.DeliveryUnknown, tc.delivered, tc.unknown)
		}
	}
}

// submitTool is a seat's submit_work: the call that hands the round's work in.
type submitTool struct{}

func (submitTool) Name() string               { return runner.SubmitWorkTool }
func (submitTool) Description() string        { return "hands the round's work in" }
func (submitTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (submitTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "submitted"}, nil
}

// heldRecords is a run store whose calls' updates fail while it is down, as a
// coordination store's outage longer than the ledger's own attempts does. It
// counts the updates it refused and closes landed once one lands after it is back.
type heldRecords struct {
	sandbox.PendingStore
	down    atomic.Bool
	refused atomic.Int32
	once    sync.Once
	landed  chan struct{}
}

func (s *heldRecords) AppendBridgeCall(ctx context.Context, turnID string, a sandbox.BridgeAppend) (bool, error) {
	if a.Step != sandbox.AppendRecord {
		return s.PendingStore.AppendBridgeCall(ctx, turnID, a)
	}
	if s.down.Load() {
		s.refused.Add(1)
		return false, errors.New("the coordination store is electing a leader")
	}
	recorded, err := s.PendingStore.AppendBridgeCall(ctx, turnID, a)
	if recorded {
		s.once.Do(func() { close(s.landed) })
	}
	return recorded, err
}

// A SUBMISSION WHOSE UPDATE THE STORE REFUSED LANDS ONCE THE STORE IS BACK, end
// to end through the real launcher, bridge, ledger and run store. While the store
// refuses updates past the ledger's own attempts, the box is answered with the
// call's result, the log holds the call's entry, and the bridge asks for the
// update again once the answer is out. Once the store is back the update lands,
// so collection reads the submission and the resume replays it as the round's
// submission, not a call whose outcome is unknown.
//
// Mutation: answer a refused update as an error the bridge does not ask again
// (refused-update-not-owed), or have the bridge ask for none
// (owed-update-never-asked-again), and the update never lands.
func TestASubmissionWhoseUpdateTheStoreRefusedLandsOnceTheStoreIsBack(t *testing.T) {
	t.Parallel()
	const turnID = "t-owed-update"
	rig := bridgedRig(t, turnID, submitTool{})
	rt := rig.engine.sandbox.Load()
	store := &heldRecords{PendingStore: rt.pending, landed: make(chan struct{})}
	store.down.Store(true)
	rig.engine.useSandbox(store, rt.coordinator)
	rig.pending = store
	run := rig.launch(t, nil)

	run.call(t, runner.SubmitWorkTool)
	if n := int(store.refused.Load()); n < appendAttempts {
		t.Fatalf("the store refused %d updates before the box was answered, want the ledger's %d attempts",
			n, appendAttempts)
	}
	held := run.calls(t, turnID)
	if len(held.InFlight) != 1 || held.InFlight[0].Name != runner.SubmitWorkTool || len(held.Calls) != 0 {
		t.Fatalf("with the store out, the log holds %+v in flight and lists %+v, want the submission's entry "+
			"alone", held.InFlight, held.Calls)
	}

	store.down.Store(false)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	select {
	case <-store.landed:
	case <-ctx.Done():
		t.Fatal("the submission's update never landed once the store was back: nothing asked for it again")
	}
	logged := run.calls(t, turnID)
	if len(logged.InFlight) != 0 || len(logged.Calls) != 1 || logged.Calls[0].Name != runner.SubmitWorkTool ||
		logged.Calls[0].Output != "submitted" || logged.Calls[0].Failed {
		t.Fatalf("once the store was back the log holds %+v in flight and lists %+v, want the submission "+
			"listed with what it answered", logged.InFlight, logged.Calls)
	}
	resumed := runnerResume(resumeInput{Run: run.row(t, turnID), Bridged: logged}, nil, nil, nil)
	if len(resumed.Unfinished) != 0 || !slices.ContainsFunc(resumed.Bridged, func(c runner.BridgedCall) bool {
		return c.Name == runner.SubmitWorkTool && !c.Failed && c.Result == "submitted"
	}) {
		t.Errorf("the resume reads %+v (unfinished %+v), want the submission it replays", resumed.Bridged,
			resumed.Unfinished)
	}
}
