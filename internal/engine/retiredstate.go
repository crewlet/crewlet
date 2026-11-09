package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A run parked at a retired state version is settled, never retried
// (execstate.ErrRetired), once no live node reads that version: at its seat's
// recovery and at any resume. A newer version still waits for a node that reads it.

// parkedRunUnresumable is [sandbox.CoordinatorOptions.Unresumable]: the run's
// parked state judged as a resume would decode it.
func (e *Engine) parkedRunUnresumable(ctx context.Context, run sandbox.PendingRun) (*sandbox.Unresumable, error) {
	_, _, err := execstate.Decode(run.ExecuteState)
	return e.retiredState(ctx, err)
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

// fleetReadsParkedState reports whether a live node may still read version: one
// whose build reads it, or one that does not say which version it reads.
func (e *Engine) fleetReadsParkedState(ctx context.Context, version int) (bool, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return false, nil
	}
	leases, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return false, fmt.Errorf("engine: reading the live nodes' builds: %w", err)
	}
	for _, lease := range leases {
		status, ok := coord.StatusFromMeta(lease.Meta)
		if !ok || status.ParkedStateVersion == 0 || status.ParkedStateVersion == version {
			return true, nil
		}
	}
	return false, nil
}
