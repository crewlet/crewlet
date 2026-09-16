package store

import (
	"errors"
	"testing"
)

// TestTheRetryClassifierCoversEveryTransientBeginFailure names the three
// conditions a transaction may be retried on, and why each is transient.
//
// The classifier is a TEXT match over the driver's own messages, which is
// fragile by construction — so the cases are written out rather than left to a
// reader to infer, and a message the driver stops using shows up here as a case
// that no longer describes anything.
func TestTheRetryClassifierCoversEveryTransientBeginFailure(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		retry bool
		why   string
	}{
		{
			name:  "a stale snapshot",
			err:   errors.New("turso: error: database snapshot is stale"),
			retry: true,
			why: "the driver's BeginTx issues a plain BEGIN, so a read-then-write " +
				"that loses a race fails immediately rather than waiting out a " +
				"busy timeout — and without a retry that is a lost write on a " +
				"database with no writer but this process",
		},
		{
			name:  "a locked database",
			err:   errors.New("turso: error: database is locked"),
			retry: true,
			why:   "another writer holds it and will not for long",
		},
		{
			name: "a connection returned to the pool with a transaction open",
			err: errors.New("turso: error: Transaction error: cannot start a " +
				"transaction within a transaction"),
			retry: true,
			why: "the next attempt draws a DIFFERENT connection, and a clean one " +
				"begins normally. Without this the caller that happened to draw " +
				"the dirty one fails permanently — the projector's boot " +
				"reconcile restarted every two seconds for the life of the " +
				"process and never hydrated",
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
			if got := staleSnapshot(c.err); got != c.retry {
				t.Fatalf("staleSnapshot(%v) = %v, want %v — %s", c.err, got,
					c.retry, c.why)
			}
		})
	}
}
