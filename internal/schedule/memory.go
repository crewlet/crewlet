package schedule

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
)

// MemoryLedger is the in-process [Ledger] twin.
//
// It is what a single-node run with no database gets, and what every test
// runs against. It loses the one thing the SQL backend exists for — a claim
// does not survive the process — so a fleet or a restart-tolerant deployment
// wires the SQL ledger instead.
//
// Correct under concurrent use, which is not a courtesy: a plain set is only
// safe behind a single-threaded scheduler, and every one of those implicit
// serialisations is a real race here. The mutex is what makes "exactly one
// claimer wins" true when four goroutines claim the same identity at once.
type MemoryLedger struct {
	mu    sync.Mutex
	claim map[FireKey]Run
}

// NewMemoryLedger returns an empty in-memory ledger.
func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{claim: map[FireKey]Run{}} }

// Claim records a fire, reporting whether this call wrote the row.
func (m *MemoryLedger) Claim(_ context.Context, run Run) (bool, error) {
	run = Stamped(run)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claim == nil {
		m.claim = map[FireKey]Run{}
	}
	if _, taken := m.claim[run.FireKey]; taken {
		return false, nil
	}
	m.claim[run.FireKey] = run
	return true, nil
}

// Recent returns the newest rows first, at most limit of them.
func (m *MemoryLedger) Recent(_ context.Context, limit int) ([]Run, error) {
	if limit <= 0 {
		return nil, nil
	}
	m.mu.Lock()
	rows := make([]Run, 0, len(m.claim))
	for _, run := range m.claim {
		rows = append(rows, run)
	}
	m.mu.Unlock()

	// Map iteration is deliberately randomised in Go, so the sort has to be
	// TOTAL — not merely "newest first with ties left alone". Sorting on
	// FiredAt only would hand a caller a different page on every call for
	// rows written in the same millisecond, and the SQL backend, ordering
	// the same rows by the same columns, would disagree with it.
	slices.SortFunc(rows, byNewestThenIdentity)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// Purge drops rows fired strictly before the cutoff, and their claim keys
// with them.
func (m *MemoryLedger) Purge(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dropped := 0
	for key, run := range m.claim {
		if run.FiredAt.Before(before) {
			// Deleting the map entry drops the record AND the claim in one
			// move, because here they are the same thing. Two structures
			// means remembering to sweep both, and the one that gets
			// forgotten is the claim — which turns a purge into a
			// permanent silent refusal.
			delete(m.claim, key)
			dropped++
		}
	}
	return dropped, nil
}

// byNewestThenIdentity is the ordering both backends answer Recent in. See
// [Ledger.Recent] for why the tiebreak is part of the contract.
func byNewestThenIdentity(a, b Run) int {
	if c := b.FiredAt.Compare(a.FiredAt); c != 0 {
		return c
	}
	return cmp.Or(
		cmp.Compare(a.Scope, b.Scope),
		cmp.Compare(a.ScopeID, b.ScopeID),
		cmp.Compare(a.ScheduleName, b.ScheduleName),
		cmp.Compare(a.FireLabel, b.FireLabel),
		cmp.Compare(a.TargetHandle, b.TargetHandle),
	)
}
