package store

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errWritersQueued is a write transaction that waited past the busy timeout
// for the ones queued ahead of it on the same database.
//
// It is the SAME wait the driver's own "database is locked" reports, done in
// order and ended by the same knob, so [classify] gives it the same cause and
// the same budget: waiting again is a longer wait rather than a second
// effect.
var errWritersQueued = errors.New("store: the write transactions queued ahead " +
	"of this one did not finish within the busy timeout; raise " +
	"store.busy_timeout_seconds if a transaction on this database " +
	"legitimately runs that long")

// writeQueue orders one database's write transactions: first to ask, first to
// begin. The driver's write lock is a TRY-LOCK with no queue: a loser sleeps on
// the busy handler (1 ms growing to 100 ms) and wins only if a poll lands in the
// gap between holders, so a writer committing back to back STARVES a waiter
// (darwin's fullfsync: TestAForeignCommitDoesNotAbortAnApplierTransaction saw
// 2338 commits beside four runs of the applier's body). The driver's lock stays
// the exclusion; this is the ORDER, which [statelog]'s occupancy model assumes:
// waiters in a slice, the turn HANDED to the next so no late arrival barges. A
// mutex cannot be abandoned by a writer whose context ended and promises no
// order. One per FILE ([fileLock]), shared by every handle on the path; a
// statement through [DB.SQL] outside a transaction is not queued. The zero
// value is ready.
type writeQueue struct {
	mu sync.Mutex
	// held is whether a writer has the queue. It stays true across a
	// handoff, so a waiter is only ever appended behind a holder.
	held    bool
	waiters []chan struct{}
}

// acquire returns once the caller is at the front, wait elapses, or ctx ends.
// A nil error means the caller holds the queue and MUST release it.
//
// BOUNDED, and by the waiter's own busy timeout rather than by a constant: a
// wait here IS the wait for the file's write lock, done in order, so it ends
// when the driver's own would and fails the same retryable way. Unbounded, a
// transaction that waited from inside its own body on another write
// transaction on the same database — which the driver answers with a busy
// timeout — would wait for ever instead.
func (q *writeQueue) acquire(ctx context.Context, wait time.Duration) error {
	turn, holds := q.join()
	if holds {
		return nil
	}
	if wait <= 0 {
		wait = defaultBusyTimeout
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var err error
	select {
	case <-turn:
		return nil
	case <-timer.C:
		err = errWritersQueued
	case <-ctx.Done():
		err = ctx.Err()
	}
	if q.leave(turn) {
		q.release()
	}
	return err
}

// join takes the queue if it is free, and otherwise joins the line: the
// returned channel is closed when the caller reaches the front.
func (q *writeQueue) join() (turn chan struct{}, holds bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.held {
		q.held = true
		return nil, true
	}
	turn = make(chan struct{})
	q.waiters = append(q.waiters, turn)
	return turn, false
}

// leave takes a waiter that gave up out of the line, and reports whether it
// was too late: already handed the queue, in which case the caller HOLDS it.
//
// GIVING UP RACES THE HANDOFF, and losing that race must not lose the queue.
// A waiter dropped with the queue in its hands would stall every writer
// behind it until the busy timeout, and then the next one again, so the
// caller that finds itself holding passes it on instead.
func (q *writeQueue) leave(turn chan struct{}) (holds bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, w := range q.waiters {
		if w == turn {
			q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
			return false
		}
	}
	return true
}

// release hands the queue to the longest waiter, or frees it when there is
// none.
//
// THE HANDOFF HAPPENS UNDER THE MUTEX, together with taking the waiter out of
// the line, which is what makes [writeQueue.leave]'s answer trustworthy: a
// waiter is either still in the slice or already holding, never briefly both
// nor briefly neither.
func (q *writeQueue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiters) == 0 {
		q.held = false
		return
	}
	next := q.waiters[0]
	q.waiters[0] = nil
	q.waiters = q.waiters[1:]
	close(next)
}
