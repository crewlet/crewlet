package ledgerstore

import (
	"context"
	"sync"
	"time"
)

// MemoryCompletions is the in-memory completion ledger.
type MemoryCompletions struct {
	mu   sync.Mutex
	rows map[string]map[string]time.Time // handle -> key -> completed at
}

// NewMemoryCompletions returns the in-process twin of the completion ledger.
func NewMemoryCompletions() *MemoryCompletions {
	return &MemoryCompletions{rows: make(map[string]map[string]time.Time)}
}

var _ Completions = (*MemoryCompletions)(nil)

// Worked returns the subset of keys already recorded for this seat.
func (m *MemoryCompletions) Worked(_ context.Context, handle string, keys []string) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]bool, len(keys))
	seat := m.rows[handle]
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, ok := seat[k]; ok {
			out[k] = true
		}
	}
	return out
}

// WorkedAt returns the keys already recorded for this seat, each with when.
func (m *MemoryCompletions) WorkedAt(_ context.Context, handle string, keys []string) map[string]time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]time.Time, len(keys))
	seat := m.rows[handle]
	for _, k := range keys {
		if at, ok := seat[k]; ok && k != "" {
			out[k] = at
		}
	}
	return out
}

// Record marks a key worked.
func (m *MemoryCompletions) Record(_ context.Context, handle, key, _ string, at time.Time) error {
	if key == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seat, ok := m.rows[handle]
	if !ok {
		seat = make(map[string]time.Time)
		m.rows[handle] = seat
	}
	// FIRST write wins, matching the durable store's ON CONFLICT DO
	// NOTHING: the completion time is when the work actually happened, and
	// a redelivery must not move it forward past the retention cutoff.
	if _, exists := seat[key]; !exists {
		seat[key] = at
	}
	return nil
}
