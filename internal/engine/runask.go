package engine

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// runAsks is where `ask_and_wait` records the question an executor's run asks
// (ADR-0068): the run's own row, through the pending-run store of the sandbox
// runtime this node holds at the moment of the call.
//
// READ PER CALL, never captured when the epoch is equipped: the runtime is
// replaced by an apply that changes `providers.sandbox`, and a run asking
// after that must reach the store the node holds now.
type runAsks struct{ engine *Engine }

var _ builtin.RunAskDesk = runAsks{}

// errNoRunStore is why a question cannot be recorded on a node holding no
// sandbox runtime: there is no run row for it to land on.
var errNoRunStore = errors.New("engine: this node holds no sandbox runtime, so a coding " +
	"run's question has no row to be recorded on")

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
