package engine

import (
	"context"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A RUN WAITING FOR AN OPERATOR IS RAISED FROM THE HEARTBEAT OF THE NODE HOLDING
// ITS SEAT, by every data node, whichever node holds it: a satellite evaluates no
// alarm table of its own, and read from each node's own memory alone, the alarm
// was never raised for a seat a satellite held. The reading sums the fleet's runs
// and names the one waiting longest, by its seat, the node holding it, its
// condition and how long. A live node whose beat carried no status keeps its last
// word, a listing that fails stands on the last that answered, and a node whose
// lease is gone says nothing any more.
//
// Mutation: read this node's own memory alone, read a node that said nothing this
// beat as holding no such run, clear the reading on a failed listing, or keep a
// gone node's word, and a step goes red.
func TestARunWaitingForAnOperatorIsRaisedFromItsHoldersHeartbeat(t *testing.T) {
	t.Parallel()
	live := t.Context()
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	backend := coordtest.NewFaulty(coordmem.New())
	beat := func(node string, blocked *coord.ResumesBlocked, saying bool) *coord.Lease {
		t.Helper()
		// THE PLACEMENT HALF ALONE where the node does not say: a beat whose
		// status hook overran its share carries no status at all.
		meta := map[string]any{"roles": []string{"seats"}}
		if saying {
			meta[coord.StatusKey] = coord.NodeStatus{ResumesBlocked: blocked}.Meta()
		}
		lease, _, err := backend.TryAcquire(live, coord.NodeResource(node), coord.AcquireOptions{
			Owner: node + ":1", TTL: time.Hour, Preferred: node, Ungated: true, Meta: meta,
		})
		if err != nil || lease == nil {
			t.Fatalf("%s's beat: %v", node, err)
		}
		return lease
	}
	beat("core-1", nil, true)
	sat1 := beat("sat-1", &coord.ResumesBlocked{Runs: 2, Seat: "swe", Kind: "disk_full",
		Since: now.Add(-40 * time.Minute)}, true)
	beat("sat-2", &coord.ResumesBlocked{Runs: 1, Seat: "ops", Kind: "fleet_keys_differ",
		Since: now.Add(-10 * time.Minute)}, true)
	// A DATA NODE HOLDING NO SUCH RUN ITSELF, as a core node of a fleet whose
	// seats run on satellites does.
	e := &Engine{backends: &Backends{Coord: backend}, testNode: "core-1"}
	read := func(step string, want statelog.Reading) {
		t.Helper()
		var got statelog.Reading
		e.executorReading(live, now, &got)
		if got.ExecutorResumesBlocked != want.ExecutorResumesBlocked ||
			got.ExecutorResumeBlockedSeat != want.ExecutorResumeBlockedSeat ||
			got.ExecutorResumeBlockedNode != want.ExecutorResumeBlockedNode ||
			got.ExecutorResumeBlockedKind != want.ExecutorResumeBlockedKind ||
			got.ExecutorResumeBlockedFor != want.ExecutorResumeBlockedFor {
			t.Fatalf("%s: read %d run(s), the longest %s's held by %q on %s for %s; want %d, %s's held "+
				"by %q on %s for %s", step, got.ExecutorResumesBlocked, got.ExecutorResumeBlockedSeat,
				got.ExecutorResumeBlockedNode, got.ExecutorResumeBlockedKind, got.ExecutorResumeBlockedFor,
				want.ExecutorResumesBlocked, want.ExecutorResumeBlockedSeat, want.ExecutorResumeBlockedNode,
				want.ExecutorResumeBlockedKind, want.ExecutorResumeBlockedFor)
		}
	}
	both := statelog.Reading{ExecutorResumesBlocked: 3, ExecutorResumeBlockedSeat: "swe",
		ExecutorResumeBlockedNode: "sat-1", ExecutorResumeBlockedKind: "disk_full",
		ExecutorResumeBlockedFor: 40 * time.Minute}
	read("two satellites' runs", both)
	beat("sat-1", nil, false)
	read("a satellite's beat that carried no status", both)
	backend.Break(nil)
	read("a listing that failed", both)
	backend.Heal()
	if released, err := backend.Release(live, coord.NodeResource("sat-1"), sat1.Owner, sat1.Epoch); err != nil ||
		!released {
		t.Fatalf("sat-1's release = (%v, %v)", released, err)
	}
	read("a satellite gone", statelog.Reading{ExecutorResumesBlocked: 1, ExecutorResumeBlockedSeat: "ops",
		ExecutorResumeBlockedNode: "sat-2", ExecutorResumeBlockedKind: "fleet_keys_differ",
		ExecutorResumeBlockedFor: 10 * time.Minute})
}

// A NODE SAYS ON ITS HEARTBEAT WHICH RUNS OF ITS SEATS WAIT FOR AN OPERATOR, from
// the executor coordinator its runtime publishes: a finished job whose resume
// failed on a full disk counts its run, under the kind the engine's one
// classification of conditions gives the failure, and a node whose runs wait on
// nothing says none. That beat is all a data node raises executor_resume_blocked
// from for a seat this node holds.
//
// Mutation: leave the runs off the heartbeat, or say a coordinator's empty answer
// as a row, and the case goes red.
func TestANodeSaysOnItsHeartbeatWhichRunsWaitForAnOperator(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	provider, runner := sandbox.NewFakeProvider(), sandbox.NewFakeRunner("claude-code")
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// THE ENGINE'S OWN TWO ANSWERS, so the failure is told as a running node
	// tells it.
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers: testAnswers(), Pushes: noPushes{}, Audience: noAudience{},
		Queue: queuememory.New(), Pending: store, Manager: manager, Resume: fullDisk{},
		NodesFailure: nodesFailure, OperatorsCondition: operatorsCondition,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	e := &Engine{}
	e.useSandbox(store, coordinator)
	if said := beatOf(t, e).ResumesBlocked; said != nil {
		t.Fatalf("a node whose runs wait on nothing said %+v", said)
	}

	if _, err := coordinator.Launch(ctx, manager, sandbox.LaunchRequest{
		Turn: sandbox.TurnRef{TurnID: "t1", AgentID: uuid.NewString(), AgentHandle: "swe", Role: "SWE",
			WorkKey: "wk-1", WorkSince: time.Now().UTC(), Reply: "tool"},
		Brief: "fix the flaky test", Task: "get CI green",
		Spec: sandbox.Spec{Harness: "claude-code", PauseTTLSec: 1800},
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := store.MarkSuspended(ctx, "t1", sandbox.Suspension{State: []byte(`{"messages":[]}`)}); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	runner.Finish(sandbox.Result{Success: true, Text: "fixed"})
	run, _, err := store.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	completed := types.ExecutorFinished{Agent: run.AgentID, AgentHandle: "swe", RoleName: run.Role,
		TurnID: "t1", LaunchID: run.LaunchID, SandboxID: run.SandboxID, Harness: run.Harness}
	if err := coordinator.OnCompleted(ctx, completed, events.New(completed, events.TraceContext{})); err == nil {
		t.Fatal("the premise: a resume on a full disk is handed back")
	}

	// AS A PEER READS IT, off the wire the beat publishes.
	sent, _ := coord.StatusFromMeta(map[string]any{coord.StatusKey: beatOf(t, e).Meta()})
	said := sent.ResumesBlocked
	if said == nil || said.Runs != 1 || said.Seat != "swe" || said.Kind != "disk_full" || said.Since.IsZero() {
		t.Fatalf("the heartbeat says %+v of the run waiting on a full disk, want one run of swe's on "+
			"disk_full since its failure", said)
	}
}

// fullDisk is a node whose disk is full: every resume fails as a write the
// system refused for want of space.
type fullDisk struct{}

func (fullDisk) Resume(context.Context, sandbox.ResumeRequest) error {
	return fmt.Errorf("resuming: write the turn's log: %w", syscall.ENOSPC)
}
