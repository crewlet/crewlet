package a2a

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"
)

// MemoryStore is the in-memory twin.
//
// It exists to be a real implementation of the same contract, not a stub: the
// conformance suite runs against both, so a twin that models the durable store
// wrongly fails there rather than certifying the bug.
type MemoryStore struct {
	mu       sync.Mutex
	channels map[string]Channel
}

// NewMemoryStore returns the in-process twin of the durable A2A store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{channels: make(map[string]Channel)}
}

var _ Store = (*MemoryStore)(nil)

// Open records a new channel, ignoring a duplicate id.
//
// Ignoring rather than overwriting, matching the durable store's ON CONFLICT
// DO NOTHING. The id is minted per ask, so a collision means a retried publish
// of ONE ask — and overwriting would reset the message counter and replace the
// participants of a channel that is already carrying an answer.
//
// Found by the shared conformance suite: the twin overwrote while the SQL
// store ignored, so an in-memory deployment and a durable one disagreed about
// what a retry does.
func (m *MemoryStore) Open(_ context.Context, ch Channel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.channels[ch.ID]; exists {
		return nil
	}
	m.channels[ch.ID] = ch
	return nil
}

// Get returns one channel by key.
func (m *MemoryStore) Get(_ context.Context, id string) (Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.channels[id]
	if !ok {
		return Channel{}, ErrNoChannel
	}
	// A VALUE, so a caller mutating what it got back cannot reach in and
	// rewrite the store's own record. Channel holds no reference types, so
	// the copy is complete — a field that later does will break this, which
	// is why the suite asserts it rather than trusting the shape.
	return ch, nil
}

// Close ends a channel, so no further ask can land on it.
func (m *MemoryStore) Close(_ context.Context, id string, at time.Time) (Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.channels[id]
	if !ok {
		return Channel{}, ErrNoChannel
	}
	if ch.Open() {
		ch.ClosedAt = at
		ch.LastAt = at
		m.channels[id] = ch
	}
	return ch, nil
}

// CountMessage records one message against a channel's own budget.
func (m *MemoryStore) CountMessage(_ context.Context, id string, at time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.channels[id]
	if !ok {
		return 0, ErrNoChannel
	}
	ch.Messages++
	ch.LastAt = at
	m.channels[id] = ch
	return ch.Messages, nil
}

// CloseIdle ends every channel with no traffic since the cutoff.
func (m *MemoryStore) CloseIdle(_ context.Context, cutoff, at time.Time) ([]Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var closed []Channel
	for _, id := range slices.Sorted(maps.Keys(m.channels)) {
		ch := m.channels[id]
		if !ch.Open() || !ch.LastAt.Before(cutoff) {
			continue
		}
		ch.ClosedAt = at
		m.channels[id] = ch
		closed = append(closed, ch)
	}
	return closed, nil
}

// Purge removes channels closed before the cutoff.
func (m *MemoryStore) Purge(_ context.Context, cutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, ch := range m.channels {
		if !ch.Open() && ch.ClosedAt.Before(cutoff) {
			delete(m.channels, id)
			n++
		}
	}
	return n, nil
}
