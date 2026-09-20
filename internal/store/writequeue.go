package store

import (
	"context"
	"errors"
	"sync"
	"time"
)

// THE WRITE LOCK IS TAKEN IN THE ORDER IT WAS ASKED FOR, and this file is the
// whole of how.
//
// # What begin.go left unanswered
//
// [beginModeDriver] moved a write transaction's contention from its first
// write to its BEGIN, which is what stops a foreign commit aborting a
// read-then-write. It did nothing about WHO GETS THE LOCK NEXT, because the
// driver has no answer to that either: the write lock is a TRY-LOCK with no
// queue behind it. A statement that cannot take it returns busy, and the busy
// handler sleeps on SQLite's schedule — 1 ms, growing to 100 ms — and tries
// again. Nothing is served in order. A waiter wins only if one of its polls
// happens to land in the gap between one holder's release and the next
// holder's acquire.
//
// So a writer that commits back to back STARVES a polling waiter, and the
// waiter's own busy timeout is the only thing that ends it. Measured on
// darwin, where `PRAGMA fullfsync` makes every commit an F_FULLFSYNC: a small
// commit holds the lock for about 4 ms and leaves a gap of microseconds, and
// a waiter loses nearly every poll — TestAForeignCommitDoesNotAbortAnApplier
// Transaction reported the applier's body running four times against a writer
// that committed 2338 times beside it. With fullfsync off on the same host
// each commit holds it for about 0.25 ms and the waiter wins within a few
// polls, which is why the Linux job never showed it. The platform only
// changes the odds: any writer committing back to back starves a polling
// waiter, and a fast disk narrows the window rather than closing it.
//
// That is not a slow test. Three domains' appliers and the tracker's
// housekeeping share the replicated estate's one file, and the state log's
// occupancy model — what [statelog] prices a bulk apply's delay to every
// other domain against — assumes a writer waits for the work in front of it
// rather than for its polls to line up.
//
// # What the queue is, and what it is not
//
// The driver's lock stays the MUTUAL EXCLUSION. This is the ORDER, which is
// the one thing the driver's busy handler cannot give: waiters in a slice,
// served from the front, and the turn HANDED to the next one rather than
// released for anyone to take, so a writer arriving at the moment of a
// release cannot barge past one that has been waiting. A writer's wait is
// then bounded by the work queued ahead of it.
//
// A sync.Mutex is the obvious alternative and cannot do it: it cannot be
// abandoned, so a writer whose context ended would stay in line until it
// reached the front, and neither a mutex nor a blocked channel send PROMISES
// an order. The runtime happens to be close to FIFO for both, and a fairness
// property resting on an implementation detail is one a Go release can take
// away without failing a test.
//
// # One per FILE, not per handle
//
// It lives on this process's claim on the file ([fileLock]), which every
// handle opened on that path shares. Two handles on one file are two pools on
// one lock — which the package doc calls safe — and they stay ONE line for it
// rather than two lines polling each other.
//
// # What it does not cover
//
// A statement issued outside a transaction through [DB.SQL] takes the
// driver's lock on its own and is not queued. It cannot be aborted, since its
// read and its write are one statement, but it waits on the busy handler
// rather than in order and a queued writer can pass over it. The replicated
// estate has none; the node estate's autocommit writers — the learning
// subsystem's single-statement inserts, the agent ledger's conversation
// upsert, the schedule ledger — are best effort by design.

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
// begin.
//
// The zero value is ready. It is NOT the exclusion — the driver's own lock
// is, and a caller that skips this queue still cannot write beside one that
// holds it.
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
