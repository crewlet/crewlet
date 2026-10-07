package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/workkey"
)

// A LAUNCH THE ENGINE MAKES IS STAMPED WITH THE LEASE IT HOLDS THE SEAT UNDER,
// through the coordinator it builds — the wiring between the seat host that
// knows the lease and the coordinator that stamps the row, which a coordinator
// test with a fixed lease cannot see. It is what lets the seat's next holder
// fence this node off the run: a row left at the zero epoch is fenced by
// nothing.
func TestTheEngineStampsALaunchWithItsSeatLease(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxDoc(""))
	waitHeld(t, e, "swe")
	rt := e.sandbox.Load()
	manager := rt.coordinator.Manager()
	if _, err := rt.coordinator.Launch(t.Context(), manager, sandbox.LaunchRequest{
		Turn:  sandbox.TurnRef{TurnID: "t-stamped", AgentHandle: "swe", Role: "SWE"},
		Brief: "fix the flake",
		Spec: manager.BuildSpec(sandbox.SpecInput{
			Placement: sandbox.Direct, CodingAgent: "claude-code",
		}),
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	run, found, err := rt.pending.Get(t.Context(), "t-stamped")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	host := e.node.Host()
	epoch, held := host.EpochFor("swe")
	if !held || epoch == 0 {
		t.Fatalf("the premise: the seat is held under a lease (%d, %v)", epoch, held)
	}
	if run.Owner != host.Owner() || run.OwnerEpoch != epoch {
		t.Fatalf("the run is owned by %q at %d, want the seat's lease %q at %d",
			run.Owner, run.OwnerEpoch, host.Owner(), epoch)
	}
}

// A CLAIM THAT DIED BEFORE ITS TURN TOOK THE REPLY IS REVIVED BY THE ENGINE'S
// RECOVERY, under the lease the engine holds the seat under, and the run is
// resumed with the reply once. The seat host is what hands the recovery its
// owner and epoch, which a coordinator test with a fixed lease cannot see: a
// revival stamped with anything else is one the claim's stalled node could
// still take the answer under.
func TestTheEngineRevivesAClaimThatDiedBeforeItsTurn(t *testing.T) {
	t.Setenv("K", "sk-ant-test")
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxWithModelDoc)
	waitHeld(t, e, "swe")
	host := e.node.Host()
	rt := e.sandbox.Load()
	retries := &capturedRetries{}
	resumer := &resumeSpy{}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Audience: noAudience{}, Queue: e.backends.Queue, Pending: rt.pending,
		Manager: rt.coordinator.Manager(), Resume: resumer,
		Hold: seatHold{engine: e}, Admit: e.mayResumeAnswer, After: retries.after,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	e.sandbox.Store(&sandboxRuntime{pending: rt.pending, coordinator: coordinator})

	reply := events.New(types.ExternalNotification{
		NotificationSource: "slack", SourceEventType: "message",
		Sender: "ana", Body: "use main",
	}, events.TraceContext{})
	seedClaimedAnswer(t, e, "t-revived", reply)
	if !host.Release(t.Context(), "swe", seat.ReasonPlacement) {
		t.Fatal("the seat was not released")
	}
	host.Sweep(t.Context())
	waitHeld(t, e, "swe")
	epoch, held := host.EpochFor("swe")
	if !held {
		t.Fatal("the seat is not held after its sweep")
	}
	run, found, err := rt.pending.Get(t.Context(), "t-revived")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if run.Status != sandbox.StatusAnswered || run.Owner != host.Owner() || run.OwnerEpoch != epoch ||
		run.Answer == nil || run.Answer.LostClaims != 1 {
		t.Fatalf("run %q owned by %q at %d with %+v, want it answered again under the seat's "+
			"lease %q at %d, its lost claim counted", run.Status, run.Owner, run.OwnerEpoch,
			run.Answer, host.Owner(), epoch)
	}
	eventually(t, "the revived run to be resumed", func() bool {
		retries.fire()
		return resumer.count() == 1
	})
}

// A REPLY THE ENGINE'S REAP HANDS BACK IS SPENT IN ITS COMPLETION LEDGER FIRST.
// A run whose claim died before its turn took the person's reply, and whose
// answer has already lost every claim it may ([sandbox.MaxAnswerRevivals]), is
// reaped by the seat's next holder — this node, as it takes the seat — and the
// reply goes back to the inbox as a copy. Its original delivery is recorded as
// worked in the fleet's completion ledger, through the dispatcher the engine
// builds, so a node that recorded the answer and stopped before acknowledging
// it cannot have the original come round afterwards as a second message.
func TestTheEngineSpendsTheReplyItsReapHandsBack(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	reply := events.New(types.ExternalNotification{
		NotificationSource: "slack", SourceEventType: "message",
		Sender: "ana", Body: "use main",
	}, events.TraceContext{})
	seedClaimedAnswer(t, e, "t-reaped", reply)
	pastRevivals(t, e, "t-reaped")

	applyOK(t, e, sandboxDoc(""))
	waitHeld(t, e, "swe")
	key := workkey.Derive([]string{reply.ID.String()})
	eventually(t, "the reaped reply to be spent in the completion ledger", func() bool {
		return e.dispatch.Completions.Worked(t.Context(), "swe", []string{key})[key]
	})
	eventually(t, "the reaped run to be ended", func() bool {
		_, found, err := e.sandbox.Load().pending.Get(t.Context(), "t-reaped")
		return err == nil && !found
	})
}

// A REPLY A TURN TOOK IS SPENT BY THE ENGINE'S REAP, for a node that stopped
// between its take and the spend that goes with it. The turn took the person's
// reply — it may already have answered them — and its node stopped mid-turn,
// with the reply's delivery unacknowledged if it was taken inline. The seat's
// next holder reaps the claim and hands nothing back: the reply is used. What
// it does write is the delivery's completion record, through the dispatcher the
// engine builds, so the original coming round to it is dropped by the ledger
// rather than run as a second turn — nothing on the reaped run recognises it.
func TestTheEngineSpendsAReplyATurnTookBeforeItsNodeStopped(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	reply := events.New(types.ExternalNotification{
		NotificationSource: "slack", SourceEventType: "message",
		Sender: "ana", Body: "use main",
	}, events.TraceContext{})
	seedClaimedAnswer(t, e, "t-taken", reply)
	store := sandbox.NewCoordStore(e.backends.Fleet)
	run, found, err := store.Get(t.Context(), "t-taken")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if ok, err := store.TakeAnswer(t.Context(), "t-taken", run.LaunchID,
		sandbox.Fence{Owner: run.Owner, Epoch: run.OwnerEpoch}); err != nil || !ok {
		t.Fatalf("TakeAnswer = %v, %v", ok, err)
	}

	applyOK(t, e, sandboxDoc(""))
	waitHeld(t, e, "swe")
	key := workkey.Derive([]string{reply.ID.String()})
	eventually(t, "the taken reply to be spent in the completion ledger", func() bool {
		return e.dispatch.Completions.Worked(t.Context(), "swe", []string{key})[key]
	})
	eventually(t, "the reaped run to be ended", func() bool {
		_, found, err := e.sandbox.Load().pending.Get(t.Context(), "t-taken")
		return err == nil && !found
	})
}

// pastRevivals marks a seeded claim's answer as having lost every claim it may
// ([sandbox.MaxAnswerRevivals]): the seat's next holder reaps the claim and
// hands its reply back rather than reviving it. Written as that many revivals
// would have — each claim given back to the answer and the next one dying —
// under no lease, so that whatever epoch this node takes the seat at outranks
// them, as the next holder's always does.
func pastRevivals(t *testing.T, e *Engine, turnID string) {
	t.Helper()
	store := sandbox.NewCoordStore(e.backends.Fleet)
	ctx := t.Context()
	for i := range sandbox.MaxAnswerRevivals {
		run, found, err := store.Get(ctx, turnID)
		if err != nil || !found {
			t.Fatalf("Get = %v, %v", found, err)
		}
		if _, ok, err := store.ReviveAnswer(ctx, turnID, sandbox.Revival{
			Launch: run.LaunchID, Answer: run.Answer.EventIDs,
		}); err != nil || !ok {
			t.Fatalf("revival %d = %v, %v", i+1, ok, err)
		}
		if _, ok, err := store.ClaimForResume(ctx, turnID, sandbox.RecordedAnswerTail(run.LaunchID),
			sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("claim %d = %v, %v", i+2, ok, err)
		}
	}
}

// seedClaimedAnswer records a run on the swe seat that parked on a question,
// was answered with reply, and was CLAIMED for the answer's resume by a node
// that stopped before its turn took the answer.
func seedClaimedAnswer(t *testing.T, e *Engine, turnID string, reply *events.Event) {
	t.Helper()
	seedRunningRun(t, e, turnID, "box-"+turnID)
	store := sandbox.NewCoordStore(e.backends.Fleet)
	ctx := t.Context()
	asked := time.Now().UTC().Add(-time.Hour)
	if err := store.MarkAwaiting(ctx, turnID, sandbox.Clarification{
		Question: "which branch?", AskedAt: asked,
	}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("encode the reply: %v", err)
	}
	run, found, err := store.Get(ctx, turnID)
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if _, ok, err := store.RecordAnswer(ctx, turnID, run.LaunchID, sandbox.RecordedAnswer{
		Text: "use main", Via: types.AnswerViaChat, EventIDs: []string{reply.ID.String()},
		Events: []json.RawMessage{raw}, PostedAt: asked.Add(time.Minute),
	}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := store.ClaimForResume(ctx, turnID, sandbox.RecordedAnswerTail(run.LaunchID),
		sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
}
