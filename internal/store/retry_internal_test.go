package store

import (
	"errors"
	"fmt"
	"testing"
)

// TestTheRetryClassifierCoversEveryTransientFailure names the conditions a
// transaction may be retried on, why each is transient, and the one it no
// longer may be.
//
// The classifier is a TEXT match over the driver's own messages, which is
// fragile by construction, so the cases are written out rather than left to a
// reader to infer, and a message the driver stops using shows up here as a case
// that no longer describes anything.
func TestTheRetryClassifierCoversEveryTransientFailure(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		retry bool
		why   string
	}{
		{
			name:  "a locked database",
			err:   errors.New("turso: database is busy: database is locked"),
			retry: true,
			why:   "a statement outside this process's queue holds the lock and will not for long",
		},
		{
			name:  "a queue that did not drain in time",
			err:   fmt.Errorf("store: begin: %w", errWritersQueued),
			retry: true,
			why: "it is the same wait as a locked database, ended by the same " +
				"busy timeout, and matched by identity rather than by text",
		},
		{
			name: "a connection returned to the pool with a transaction open",
			err: errors.New("turso: error: Transaction error: cannot start a " +
				"transaction within a transaction"),
			retry: true,
			why: "the next attempt draws a DIFFERENT connection, and a clean one " +
				"begins normally. Without this the caller that happened to draw " +
				"the dirty one fails permanently: the projector's boot " +
				"reconcile restarted every two seconds for the life of the " +
				"process and never hydrated",
		},
		{
			name:  "a stale snapshot",
			err:   errors.New("turso: error: database snapshot is stale, rollback and retry the transaction"),
			retry: false,
			why: "no transaction the store begins can meet one: a write holds " +
				"the lock from its BEGIN and a read never upgrades, so the only " +
				"way to reach it is a write inside DB.Read, and re-running that " +
				"would repeat a write its caller declared to be a read",
		},
		{
			name:  "a constraint violation",
			err:   errors.New("turso: error: UNIQUE constraint failed: probe.id"),
			retry: false,
			why: "the same statement fails the same way on every connection, so a " +
				"retry burns the whole budget and reports the failure later",
		},
		{
			name:  "a missing table",
			err:   errors.New("turso: error: no such table: probe"),
			retry: false,
			why:   "a schema fault is not a race",
		},
		{
			name:  "no error at all",
			err:   nil,
			retry: false,
			why:   "nothing to retry",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, got := retryable(c.err); got != c.retry {
				t.Fatalf("retryable(%v) = %v, want %v: %s", c.err, got,
					c.retry, c.why)
			}
		})
	}
}
