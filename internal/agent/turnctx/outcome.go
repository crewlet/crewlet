package turnctx

import (
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
)

// Commit records one committed change. A nil set records nothing.
func (w *Written) Commit(c types.CommittedChange) {
	if w == nil || !c.Valid() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !slices.Contains(w.changes, c) {
		w.changes = append(w.changes, c)
	}
}

// Changes reports the kinds of change the turn's writes committed, in first
// commit order: what a parked turn carries across its park.
func (w *Written) Changes() []types.CommittedChange {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.changes)
}

// Since is the changes committed after before, a [Written.Changes] read
// earlier: what the calls between the two reads committed.
func (w *Written) Since(before []types.CommittedChange) []types.CommittedChange {
	var out []types.CommittedChange
	for _, c := range w.Changes() {
		if !slices.Contains(before, c) {
			out = append(out, c)
		}
	}
	return out
}

// Outcome is the turn's outcome: the highest its committed changes reach.
func (w *Written) Outcome() types.TurnOutcome { return types.OutcomeOf(w.Changes()) }
