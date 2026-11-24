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

// A RUN WAITING FOR AN OPERATOR IS RAISED FROM THE STATUS RECORD OF THE NODE
// HOLDING ITS SEAT, by every data node, whichever node holds it (the package
// doc's "The runs waiting for an operator"). The reading sums the fleet's runs
// and names the one waiting longest, by its seat, its holder, its condition
// and how long. A live node whose record is not counted keeps its last word at
// the same epoch, a failed listing stands on the last that answered, and a node
// gone or claimed again at a new epoch says nothing it said before.
//
// Mutation: read this node's own memory alone, read a node whose record is not
// counted as holding no such run, clear the reading on a failed listing, or keep
// a gone or restarted node's word, and a step goes red.
func TestARunWaitingForAnOperatorIsRaisedFromItsHoldersStatusRecord(t *testing.T) {
	t.Parallel()
	live := t.Context()
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	backend := coordtest.NewFaulty(coordmem.New())
	fleet := coordmem.NewFleet()
	claim := func(node, owner string) *coord.Lease {
		t.Helper()
		lease, _, err := backend.TryAcquire(live, coord.NodeResource(node), coord.AcquireOptions{
			Owner: owner, TTL: time.Hour, Preferred: node, Ungated: true,
			Meta: map[string]any{"roles": []string{"seats"}},
		})
		if err != nil || lease == nil {
			t.Fatalf("%s's presence: %v", node, err)
		}
		return lease
	}
	say := func(lease *coord.Lease, blocked *coord.ResumesBlocked) {
		t.Helper()
		node, _ := coord.NodeID(lease.Resource)
		if err := coordtest.PutNodeStatus(live, fleet, coord.NodeStatusRecord{Node: node, Epoch: lease.Epoch, At: now,
			ResumesBlocked: blocked}); err != nil {
			t.Fatalf("%s's record: %v", node, err)
		}
	}
	say(claim("core-1", "core-1:1"), nil)
	sat1 := claim("sat-1", "sat-1:1")
	say(sat1, &coord.ResumesBlocked{Runs: 2, Seat: "swe", Kind: "disk_full", Since: now.Add(-40 * time.Minute)})
	sat2 := claim("sat-2", "sat-2:1")
	say(sat2, &coord.ResumesBlocked{Runs: 1, Seat: "ops", Kind: "fleet_keys_differ",
		Since: now.Add(-10 * time.Minute)})
	// A DATA NODE HOLDING NO SUCH RUN ITSELF, as a core node of a fleet whose
	// seats run on satellites does.
	e := &Engine{backends: &Backends{Coord: backend, Fleet: fleet}, testNode: "core-1"}
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
	if err := fleet.DeleteNodeStatus(live, "sat-1", sat1.Epoch); err != nil {
		t.Fatalf("remove sat-1's record: %v", err)
	}
	read("a satellite whose record is not counted", both)
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
	// A NODE THAT CLAIMED ITS PRESENCE AGAIN is a new incarnation: what the old
	// one said, in its record or this reader's memory, is not its word.
	if released, err := backend.Release(live, coord.NodeResource("sat-2"), sat2.Owner, sat2.Epoch); err != nil ||
		!released {
		t.Fatalf("sat-2's release = (%v, %v)", released, err)
	}
	if again := claim("sat-2", "sat-2:2"); again.Epoch == sat2.Epoch {
		t.Fatalf("the premise: sat-2 claimed again at its old epoch %d", again.Epoch)
	}
	read("a satellite restarted", statelog.Reading{})
}

// A NODE SAYS IN ITS STATUS RECORD WHICH RUNS OF ITS SEATS WAIT FOR AN OPERATOR,
// from the executor coordinator its runtime publishes: a finished job whose
// resume failed on a full disk counts its run, under the kind the engine's one
// classification of conditions gives the failure, and a node whose runs wait on
// nothing says none. That record is all a data node raises
// executor_resume_blocked from for a seat this node holds.
//
// Mutation: leave the runs off the record, or say a coordinator's empty answer
// as a row, and the case goes red.
func TestANodeSaysInItsStatusRecordWhichRunsWaitForAnOperator(t *testing.T) {
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
	if said := statusRecordOf(t, e).ResumesBlocked; said != nil {
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

	// AS A PEER READS IT, off the wire the record is written as.
	said := recordedRows(t, statusRecordOf(t, e)).ResumesBlocked
	if said == nil || said.Runs != 1 || said.Seat != "swe" || said.Kind != "disk_full" || said.Since.IsZero() {
		t.Fatalf("the record says %+v of the run waiting on a full disk, want one run of swe's on "+
			"disk_full since its failure", said)
	}
}

// fullDisk is a node whose disk is full: every resume fails as a write the
// system refused for want of space.
type fullDisk struct{}

func (fullDisk) Resume(context.Context, sandbox.ResumeRequest) error {
	return fmt.Errorf("resuming: write the turn's log: %w", syscall.ENOSPC)
}
