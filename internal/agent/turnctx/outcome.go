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

// Wrote records records a committed write created or wrote, each once and
// held to [types.MaxWrittenRecords] ([types.FoldWritten]); a nil set records
// nothing. What a bridged call adds is its log's ([Written.RecordsSince]),
// which a running run's router reads (ADR-0167): a resumed turn's set starts
// with none, since the log holds what its calls wrote.
func (w *Written) Wrote(records ...types.WrittenRecord) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.records, _ = types.FoldWritten(w.records, records...)
}

// Records reports the records the turn's writes created or wrote, in first
// write order.
func (w *Written) Records() []types.WrittenRecord {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.records)
}

// RecordsSince is the records recorded after before, a [Written.Records] read
// earlier: what the writes between the two reads created or wrote.
func (w *Written) RecordsSince(before []types.WrittenRecord) []types.WrittenRecord {
	var out []types.WrittenRecord
	for _, r := range w.Records() {
		if !slices.Contains(before, r) {
			out = append(out, r)
		}
	}
	return out
}
