package memory

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// RaiseFeedMark writes a wake feed's mark unless the one held is at the same
// generation and at or above it. See the KV backend for why it is raise-only.
func (f *Fleet) RaiseFeedMark(_ context.Context, m coord.FeedMark) (bool, error) {
	if err := m.Validate(); err != nil {
		return false, err
	}
	if m.At.IsZero() {
		m.At = time.Now().UTC()
	}
	key := coord.FeedMarkKey(m.Stream, m.Group)
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, found := f.feedMarks[key]; found && !m.Supersedes(held) {
		return false, nil
	}
	if f.feedMarks == nil {
		f.feedMarks = map[string]coord.FeedMark{}
	}
	f.feedMarks[key] = m
	return true, nil
}

// FeedMark reads one log's mark for one group.
func (f *Fleet) FeedMark(_ context.Context, stream, group string) (coord.FeedMark, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held, found := f.feedMarks[coord.FeedMarkKey(stream, group)]
	return held, found, nil
}
