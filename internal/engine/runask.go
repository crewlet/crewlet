package engine

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// runAsks is where `ask_and_wait` records an executor run's question
// (ADR-0068), and where the run's submission reads whether it asked: the run's
// own row, in the pending-run store of the node's current sandbox runtime.
//
// READ PER CALL, never captured when the epoch is equipped: an apply that
// changes `providers.sandbox` replaces the runtime.
type runAsks struct{ engine *Engine }

var _ builtin.HeldDesk = runAsks{}

// errNoRunStore is why a question cannot be recorded, nor an executor's row
// read, on a node holding no sandbox runtime: there is no row for either.
var errNoRunStore = errors.New("engine: this node holds no sandbox runtime, so an " +
	"executor has no row to record its question on or to read it from")

// RecordAsk records the ask on the job's row; see
// [sandbox.PendingStore.RecordAsk].
func (d runAsks) RecordAsk(ctx context.Context, turnID, launch string, ask sandbox.Ask,
) (sandbox.AskOutcome, error) {
	rt := d.engine.sandbox.Load()
	if rt == nil || rt.pending == nil {
		return "", errNoRunStore
	}
	return rt.pending.RecordAsk(ctx, turnID, launch, ask)
}

// Get reads the run's row; see [sandbox.PendingStore.Get].
func (d runAsks) Get(ctx context.Context, turnID string) (sandbox.PendingRun, bool, error) {
	rt := d.engine.sandbox.Load()
	if rt == nil || rt.pending == nil {
		return sandbox.PendingRun{}, false, errNoRunStore
	}
	return rt.pending.Get(ctx, turnID)
}

// ListActiveForSeat reads the seat's unfinished runs; see
// [sandbox.PendingStore.ListActiveForSeat].
func (d runAsks) ListActiveForSeat(ctx context.Context, handle string) ([]sandbox.PendingRun, error) {
	rt := d.engine.sandbox.Load()
	if rt == nil || rt.pending == nil {
		return nil, errNoRunStore
	}
	return rt.pending.ListActiveForSeat(ctx, handle)
}
