// Package signedasktest is a signed channel's ledger for tests
// (signedask.Ledger): the ids a node's own file would keep, held in memory, so
// a test restarts a guard over the same ledger, counts what it kept and makes
// it refuse. The node file's own ledger is internal/store's TakenRequests.
package signedasktest

import (
	"context"
	"sync"
	"time"
)

// Ledger is the ids each channel kept, by channel.
type Ledger struct {
	mu     sync.Mutex
	kept   map[string]map[string]time.Time
	refuse error
}

// NewLedger is an empty ledger.
func NewLedger() *Ledger { return &Ledger{kept: map[string]map[string]time.Time{}} }

// Refuse makes every later Keep and Forget fail with err; nil heals it.
func (l *Ledger) Refuse(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refuse = err
}

// Keep records id on channel until until, fresh where it was not held.
func (l *Ledger) Keep(_ context.Context, channel, id string, until time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refuse != nil {
		return false, l.refuse
	}
	if l.kept[channel] == nil {
		l.kept[channel] = map[string]time.Time{}
	}
	if _, held := l.kept[channel][id]; held {
		return false, nil
	}
	l.kept[channel][id] = until
	return true, nil
}

// Forget deletes channel's ids whose until is not after now.
func (l *Ledger) Forget(_ context.Context, channel string, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refuse != nil {
		return l.refuse
	}
	for id, until := range l.kept[channel] {
		if !now.Before(until) {
			delete(l.kept[channel], id)
		}
	}
	return nil
}

// Len is how many ids channel holds.
func (l *Ledger) Len(channel string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.kept[channel])
}

// Holds reports whether channel holds id.
func (l *Ledger) Holds(channel, id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, held := l.kept[channel][id]
	return held
}
