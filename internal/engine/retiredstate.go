package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A parked run no build will resume is settled, never retried: one parked at a
// retired state version no live node reads (execstate.ErrRetired), and one whose
// question is about a work item finished since. A newer version still waits.

// parkedRunUnresumable is [sandbox.CoordinatorOptions.Unresumable]: the run's
// parked state judged as a resume would decode it, then the item it asked about.
func (e *Engine) parkedRunUnresumable(ctx context.Context, run sandbox.PendingRun) (*sandbox.Unresumable, error) {
	_, _, decoded := execstate.Decode(run.ExecuteState)
	retired, err := e.retiredState(ctx, decoded)
	if retired != nil || err != nil {
		return retired, err
	}
	return e.itemFinished(ctx, run)
}

// itemFinished judges a parked run whose Worklet item was finished or removed
// since it asked: its question is moot, so it is settled rather than answered.
func (e *Engine) itemFinished(ctx context.Context, run sandbox.PendingRun) (*sandbox.Unresumable, error) {
	if e.router == nil {
		return nil, nil
	}
	return finishedItem(ctx, e.router.Work(), run)
}

// taskReader is the one tracker read the parked-run judge makes.
type taskReader interface {
	Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
		fresh statelog.Freshness) (worklet.TaskDetail, error)
}

// finishedItem is [Engine.itemFinished] over tasks.
func finishedItem(ctx context.Context, tasks taskReader, run sandbox.PendingRun) (*sandbox.Unresumable, error) {
	item := run.WorkItem
	if item == nil || item.Backend != types.WorkWorklet || item.ID == "" {
		return nil, nil
	}
	detail, err := tasks.Task(ctx, item.ID, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)})
	switch {
	case errors.Is(err, worklet.ErrNoTask):
	case err != nil:
		return nil, fmt.Errorf("engine: reading work item %s of parked run %s: %w", item.ID, run.TurnID, err)
	case !detail.Task.Status.Group().Finished():
		return nil, nil
	}
	return &sandbox.Unresumable{Reason: types.ExecutorFailureItemFinished, Detail: "the work item " +
		"the run's question was about was finished or removed while it waited, so the question is " +
		"withdrawn and the run is ended; the work it pushed, if any, is on its branch"}, nil
}

// retiredState judges a parked state's decode error: a run to settle where the
// state is a retired version no live node reads, nil for any other answer.
func (e *Engine) retiredState(ctx context.Context, decoded error) (*sandbox.Unresumable, error) {
	version, ok := errors.AsType[*execstate.VersionError](decoded)
	if !ok || !version.Retired() {
		return nil, nil
	}
	read, err := e.fleetReadsParkedState(ctx, version.Found)
	if err != nil || read {
		return nil, err
	}
	return &sandbox.Unresumable{Reason: types.ExecutorFailureStateRetired, Detail: fmt.Sprintf(
		"the run's parked turn was saved in a format no running build reads any more "+
			"(version %d; this build reads %d), so after the upgrade it is ended rather than "+
			"retried and its question is withdrawn; the work it pushed, if any, is on its branch",
		version.Found, execstate.Version)}, nil
}

// resumableElsewhere is [sandbox.CoordinatorOptions.ResumableElsewhere]:
// whether a live node other than this one reads the version of run's parked
// state ([Engine.fleetReadsParkedState]), so a resume this node cannot make
// for want of the build that wrote the state ([sandbox.ErrResumeElsewhere]) is
// not counted toward the resume give-up while one can, and is once none can.
// A run whose parked state names no version (none parked, or bytes that are no
// state) is read by no node.
//
// A REPLY KIND THIS BUILD DOES NOT READ ([resumeReply]) IS BOUNDED BY THE LIVE
// NODES' BUILDS instead ([Engine.anotherBuildLive]): the kind is a plain value
// on the run's row, no part of the parked state's version, so a build that
// adds one writes states every node of the fleet reads at that version. Read
// by the version, a rollback that left two nodes of this build held the seat
// for as long as both lived, each reading the other as able to resume the run
// and neither able to. While another build is live (the one that wrote the
// kind, mid-roll), the run waits for it; once every live node runs this build,
// none reads the kind, and the failure counts. A fleet left on two builds for
// good is the operator's, raised as fleet_builds_differ.
func (e *Engine) resumableElsewhere(ctx context.Context, run sandbox.PendingRun) (bool, error) {
	if _, err := resumeReply(run); errors.Is(err, sandbox.ErrResumeElsewhere) {
		return e.anotherBuildLive(ctx)
	}
	version, ok := parkedStateVersion(run.ExecuteState)
	if !ok {
		return false, nil
	}
	return e.fleetReadsParkedState(ctx, version)
}

// parkedStateVersion is the version a parked state was written at, read
// alone ([execstate.VersionOf]): never a decode of the whole state, which
// every failure another node or build may get past would otherwise make
// beside the resume's own. False for none, or for bytes that are no state. A
// state of this build's version whose body this build refuses still names the
// version another node reads: its body's own failure is met, and counted,
// where a node decodes it.
func parkedStateVersion(blob json.RawMessage) (int, bool) {
	version, parked, err := execstate.VersionOf(blob)
	return version, parked && err == nil
}

// fleetReadsParkedState reports whether another live node may still read
// version: one whose build reads it, or one that does not say which version it
// reads. This node reads [execstate.Version] by construction, said or not.
func (e *Engine) fleetReadsParkedState(ctx context.Context, version int) (bool, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return false, nil
	}
	leases, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return false, fmt.Errorf("engine: reading the live nodes' builds: %w", err)
	}
	self := e.selfNode()
	for _, lease := range leases {
		if node, _ := coord.ClassNode.Name(lease.Resource); self != "" && node == self {
			continue
		}
		status, ok := coord.StatusFromMeta(lease.Meta)
		if !ok || status.ParkedStateVersion == 0 || status.ParkedStateVersion == version {
			return true, nil
		}
	}
	return false, nil
}
