package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A PARKED RUN OF A RETIRED STATE VERSION IS SETTLED ONLY ONCE NO LIVE NODE
// READS IT: a node that reads it, or says nothing, may still resume it, and a
// newer version is only ever waited for.
func TestARetiredParkedStateIsSettledOnlyOnceNoLiveNodeReadsIt(t *testing.T) {
	t.Parallel()
	retired, newer := execstate.Version-1, execstate.Version+1
	for _, tc := range []struct {
		name    string
		version int
		// nodes are the live nodes' advertised versions; -1 is a node whose
		// lease carries no status at all.
		nodes   []int
		settled bool
	}{
		{name: "no live node", version: retired, settled: true},
		{name: "every node reads only this build's version", version: retired,
			nodes: []int{execstate.Version, execstate.Version}, settled: true},
		{name: "a node still reads it", version: retired,
			nodes: []int{execstate.Version, retired}},
		{name: "a node says no version", version: retired, nodes: []int{execstate.Version, 0}},
		{name: "a node publishes no status", version: retired, nodes: []int{execstate.Version, -1}},
		{name: "a newer version waits", version: newer, nodes: []int{execstate.Version}},
		{name: "this build's version resumes", version: execstate.Version},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			backend := coordmem.New()
			for i, v := range tc.nodes {
				present(ctx, t, backend, "node-"+strconv.Itoa(i), v)
			}
			e := &Engine{backends: &Backends{Coord: backend}}
			got, err := e.parkedRunUnresumable(ctx, runParkedAtVersion(t, tc.version))
			if err != nil {
				t.Fatalf("judged with error: %v", err)
			}
			if (got != nil) != tc.settled {
				t.Fatalf("settled = %+v, want settled %v", got, tc.settled)
			}
			if got != nil && got.Reason != types.ExecutorFailureStateRetired {
				t.Fatalf("settled under %q, want %q", got.Reason, types.ExecutorFailureStateRetired)
			}
		})
	}
}

// THIS NODE NEVER HOLDS ITS OWN SETTLE BACK: its build reads only the current
// version, even while its own presence carries no status yet, as at boot.
func TestANodeWhoseOwnPresenceSaysNothingYetSettlesARetiredState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := coordmem.New()
	present(ctx, t, backend, "node-self", -1)
	e := &Engine{backends: &Backends{Coord: backend}, testNode: "node-self"}
	got, err := e.parkedRunUnresumable(ctx, runParkedAtVersion(t, execstate.Version-1))
	if err != nil || got == nil {
		t.Fatalf("judged %+v, %v; want the run settled by the only node, which reads only %d",
			got, err, execstate.Version)
	}
}

// A FLEET THAT CANNOT BE READ SETTLES NOTHING: the error is the answer, and
// the run waits for its answer as before.
func TestAnUnreadableFleetSettlesNoParkedRun(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{Coord: blindPresence{Backend: coordmem.New()}}}
	got, err := e.parkedRunUnresumable(context.Background(), runParkedAtVersion(t, execstate.Version-1))
	if err == nil || got != nil {
		t.Fatalf("judged %+v, %v; want an error and nothing settled", got, err)
	}
}

// A RESUME OF A RETIRED STATE NO LIVE NODE READS IS ABANDONED BEFORE ITS TURN,
// which settles the run; a newer state is still a routing failure that goes back.
func TestAResumeOfARetiredStateSettlesItsRun(t *testing.T) {
	t.Parallel()
	r := &resumer{engine: &Engine{backends: &Backends{Coord: coordmem.New()}}}
	err := r.resume(context.Background(), sandbox.ResumeRequest{
		Run: runParkedAtVersion(t, execstate.Version-1)})
	unresumable, ok := errors.AsType[*sandbox.Unresumable](err)
	if !ok || !errors.Is(err, sandbox.ErrResumeAbandoned) ||
		unresumable.Reason != types.ExecutorFailureStateRetired {
		t.Fatalf("resume of a retired state = %v, want it abandoned as %q", err,
			types.ExecutorFailureStateRetired)
	}
	err = r.resume(context.Background(), sandbox.ResumeRequest{
		Run: runParkedAtVersion(t, execstate.Version+1)})
	if !errors.Is(err, sandbox.ErrResumeUnavailable) || errors.Is(err, sandbox.ErrResumeAbandoned) {
		t.Fatalf("resume of a newer state = %v, want it handed back for a node that reads it", err)
	}
}

// runParkedAtVersion is a parked run whose state carries version.
func runParkedAtVersion(t *testing.T, version int) sandbox.PendingRun {
	t.Helper()
	blob, err := json.Marshal(map[string]any{"version": version})
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.PendingRun{TurnID: "t1", AgentHandle: "app", Status: sandbox.StatusAwaiting,
		ExecuteState: blob}
}

// present publishes a live node's presence advertising version, or no status
// at all for -1.
func present(ctx context.Context, t *testing.T, backend coord.Backend, node string, version int) {
	t.Helper()
	meta := map[string]any{}
	if version >= 0 {
		meta[coord.StatusKey] = coord.NodeStatus{ParkedStateVersion: version}.Meta()
	}
	if _, _, err := backend.TryAcquire(ctx, coord.NodeResource(node), coord.AcquireOptions{
		Owner: node + ":1", TTL: time.Hour, Preferred: node, Ungated: true, Meta: meta,
	}); err != nil {
		t.Fatal(err)
	}
}

// A RUN PARKED ON A QUESTION ABOUT A FINISHED OR REMOVED WORKLET ITEM IS
// SETTLED: its question is moot. An open item, an item on another tracker, a run
// on no item, and an item that cannot be read settle nothing.
func TestAParkedRunIsSettledWhenItsItemCloses(t *testing.T) {
	t.Parallel()
	worklet7 := &types.WorkItem{Backend: types.WorkWorklet, ID: "task-7", Key: "W-7"}
	for _, tc := range []struct {
		name    string
		item    *types.WorkItem
		tasks   taskReader
		settled bool
		fails   bool
	}{
		{name: "done", item: worklet7, tasks: taskAt(worklet.StatusDone), settled: true},
		{name: "cancelled", item: worklet7, tasks: taskAt(worklet.StatusCancelled), settled: true},
		{name: "removed", item: worklet7, tasks: taskErr{worklet.ErrNoTask}, settled: true},
		{name: "in progress", item: worklet7, tasks: taskAt(worklet.StatusInProgress)},
		{name: "on another tracker", item: &types.WorkItem{Backend: types.WorkJira, ID: "10042"},
			tasks: taskAt(worklet.StatusDone)},
		{name: "on no item", tasks: taskAt(worklet.StatusDone)},
		{name: "unreadable", item: worklet7, tasks: taskErr{errors.New("no copy serves")}, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := sandbox.PendingRun{TurnID: "t1", AgentHandle: "app", Status: sandbox.StatusAwaiting,
				WorkItem: tc.item}
			got, err := finishedItem(context.Background(), tc.tasks, run)
			if (err != nil) != tc.fails {
				t.Fatalf("judged with error %v, want an error %v", err, tc.fails)
			}
			if (got != nil) != tc.settled {
				t.Fatalf("settled = %+v, want settled %v", got, tc.settled)
			}
			if got != nil && got.Reason != types.ExecutorFailureItemFinished {
				t.Fatalf("settled under %q, want %q", got.Reason, types.ExecutorFailureItemFinished)
			}
		})
	}
}

// taskAt answers every task read with a task in status.
type taskAt worklet.Status

func (s taskAt) Task(context.Context, string, worklet.DetailWants,
	statelog.Freshness) (worklet.TaskDetail, error) {
	var detail worklet.TaskDetail
	detail.Task.Status = worklet.Status(s)
	return detail, nil
}

// taskErr answers every task read with an error.
type taskErr struct{ err error }

func (e taskErr) Task(context.Context, string, worklet.DetailWants,
	statelog.Freshness) (worklet.TaskDetail, error) {
	return worklet.TaskDetail{}, e.err
}
