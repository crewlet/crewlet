package schedule

import (
	"context"
	"sync"
	"time"
)

// MemoryLedger is the in-process [Ledger] twin.
//
// It is what the scheduler's own tests run against. The engine wires the SQL
// ledger, or none on a node with no store (see [SharedClaimer]): the twin
// loses the one thing the SQL backend exists for, a row that survives the
// process.
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
