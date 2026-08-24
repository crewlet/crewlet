package ledgerstore

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
)

// MemoryCompletions is the in-memory completion ledger.
type MemoryCompletions struct {
	mu   sync.Mutex
	rows map[string]map[string]time.Time // handle -> key -> completed at
}

func NewMemoryCompletions() *MemoryCompletions {
	return &MemoryCompletions{rows: make(map[string]map[string]time.Time)}
}

var _ Completions = (*MemoryCompletions)(nil)

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

func (m *MemoryCompletions) Purge(_ context.Context, cutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for handle, seat := range m.rows {
		for key, at := range seat {
			if at.Before(cutoff) {
				delete(seat, key)
				n++
			}
		}
		if len(seat) == 0 {
			delete(m.rows, handle)
		}
	}
	return n, nil
}

// memoryEntry is one stored conversation row.
type memoryEntry struct {
	entry   ledger.Session
	workKey string
	at      time.Time
	seq     int
}

// MemoryConversations is the in-memory conversation ledger.
type MemoryConversations struct {
	mu   sync.Mutex
	rows map[string][]memoryEntry // handle\x00conversation -> entries
	seq  int
}

func NewMemoryConversations() *MemoryConversations {
	return &MemoryConversations{rows: make(map[string][]memoryEntry)}
}

var _ Conversations = (*MemoryConversations)(nil)

// convKey joins the two halves with a byte that cannot appear in either.
//
// A separator that CAN appear would let ("a", "b:c") and ("a:b", "c") collide
// onto one conversation — which is a seat reading another thread's history as
// its own.
func convKey(handle, conversation string) string { return handle + "\x00" + conversation }

func (m *MemoryConversations) Append(_ context.Context, handle, conversation string,
	entry ledger.Session, workKey string, at time.Time, maxEntries int,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := convKey(handle, conversation)
	rows := m.rows[key]

	// Deduped on the WORK key, and only when there is one: '' is the
	// documented "a turn with no ledgerable trigger", and collapsing those
	// would keep one entry for every unkeyed turn this seat ever ran.
	if workKey != "" {
		for _, r := range rows {
			if r.workKey == workKey {
				return nil
			}
		}
	}
	m.seq++
	rows = append(rows, memoryEntry{entry: entry, workKey: workKey, at: at, seq: m.seq})
	if maxEntries > 0 && len(rows) > maxEntries {
		rows = slices.Clone(rows[len(rows)-maxEntries:])
	}
	m.rows[key] = rows
	return nil
}

func (m *MemoryConversations) History(_ context.Context, handle, conversation string, limit int) ([]ledger.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.rows[convKey(handle, conversation)]
	if limit > 0 && len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	out := make([]ledger.Session, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.entry)
	}
	if len(out) == 0 {
		// Nil, not an empty slice: the caller renders "" for no history
		// and an empty non-nil slice reads the same, but only nil says
		// "there is nothing here" to a reader comparing against nil.
		return nil, nil
	}
	return out, nil
}

func (m *MemoryConversations) Threads(_ context.Context, handle string, limit int) ([]Thread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Thread
	for key, rows := range m.rows {
		owner, conversation, found := strings.Cut(key, "\x00")
		if !found || owner != handle || len(rows) == 0 {
			continue
		}
		thread := Thread{Key: conversation, Entries: len(rows)}
		for _, r := range rows {
			if r.at.After(thread.LastAt) {
				thread.LastAt = r.at
			}
		}
		out = append(out, thread)
	}
	// Newest activity first, then by key. The tiebreak is not decoration:
	// a map walk has no order, so without it this twin hands two readers
	// different pages for the same data — which is precisely the kind of
	// divergence a memory twin exists to NOT have against the real store.
	slices.SortFunc(out, func(a, b Thread) int {
		if !a.LastAt.Equal(b.LastAt) {
			return b.LastAt.Compare(a.LastAt)
		}
		return strings.Compare(a.Key, b.Key)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryConversations) Purge(_ context.Context, cutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for key, rows := range m.rows {
		kept := rows[:0]
		for _, r := range rows {
			if r.at.Before(cutoff) {
				n++
				continue
			}
			kept = append(kept, r)
		}
		if len(kept) == 0 {
			delete(m.rows, key)
			continue
		}
		m.rows[key] = slices.Clone(kept)
	}
	return n, nil
}
