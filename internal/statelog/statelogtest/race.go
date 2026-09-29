package statelogtest

import (
	"context"
	"sync"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Race is a broker a case puts its hand into: an [statelog.Appender] that runs
// the hand ONCE, just before the first append to a subject it names reaches
// the broker — after the write authority has asked every question it asks
// before an append, and before the record lands.
//
// IT IS HOW A RELEASE RACES A WRITER. A node leaving a partition stops
// deciding there and then releases the partition's logs, and the one write its
// release exists for is the one already past its last question when the leave
// began — which lands above the release, and is dropped on every holder. No
// ordering of whole calls reaches that write; a hand between its question and
// its landing does.
type Race struct {
	statelog.Appender

	mu   sync.Mutex
	on   func(subject string) bool
	hand func()
}

// NewRace is a broker over inner with no hand in it: every append goes
// straight through until a case calls [Race.Before].
func NewRace(inner statelog.Appender) *Race {
	return &Race{Appender: inner}
}

// Before puts hand in front of the next append to a subject on names. The
// hand runs once, on the appending goroutine, and whatever it appends itself
// goes straight through.
func (r *Race) Before(on func(subject string) bool, hand func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on, r.hand = on, hand
}

// Append runs the hand when this append is the one it waits for, then appends.
func (r *Race) Append(ctx context.Context, subject, msgID string, expect *uint64,
	body []byte) (uint64, bool, error) {

	r.mu.Lock()
	var hand func()
	if r.hand != nil && r.on(subject) {
		hand, r.on, r.hand = r.hand, nil, nil
	}
	r.mu.Unlock()
	if hand != nil {
		hand()
	}
	return r.Appender.Append(ctx, subject, msgID, expect, body)
}
