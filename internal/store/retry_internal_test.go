package store

import (
	"errors"
	"testing"
)

// TestTheRetryClassifierCoversEveryTransientBeginFailure names the three
// conditions a transaction may be retried on, why each is transient, and —
// since they stopped being one bool — WHICH of them each driver message is.
//
// The classifier is a TEXT match over the driver's own messages, which is
// fragile by construction — so the cases are written out rather than left to a
// reader to infer, and a message the driver stops using shows up here as a case
// that no longer describes anything.
//
// Pinning the CAUSE rather than just "retryable" is what makes this a guard
// again: as a bool it agreed with a classifier that answered `true` for
// everything, and the whole reason [Conflict] exists is that an abort and a
// busy lock are opposite facts about the driver.
func TestTheRetryClassifierCoversEveryTransientBeginFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want Conflict
		why  string
	}{
		{
			name: "a stale snapshot",
			err:  errors.New("turso: error: database snapshot is stale"),
			want: ConflictAbort,
			why: "the driver's BeginTx issues a plain BEGIN, so a read-then-write " +
				"that loses a race fails immediately rather than waiting out a " +
				"busy timeout — and without a retry that is a lost write on a " +
				"database with no writer but this process",
		},
		{
			name: "a locked database",
			err:  errors.New("turso: error: database is locked"),
			want: ConflictLock,
			why: "another writer holds it and will not for long — and the attempt " +
				"wrote NOTHING, which is what separates it from an abort",
		},
		{
			name: "a connection returned to the pool with a transaction open",
			err: errors.New("turso: error: Transaction error: cannot start a " +
				"transaction within a transaction"),
			want: ConflictDirty,
			why: "the next attempt draws a DIFFERENT connection, and a clean one " +
				"begins normally. Without this the caller that happened to draw " +
				"the dirty one fails permanently — the projector's boot " +
				"reconcile restarted every two seconds for the life of the " +
				"process and never hydrated",
		},
		{
			name: "a constraint violation",
			err:  errors.New("turso: error: UNIQUE constraint failed: probe.id"),
			want: ConflictNone,
			why: "the same statement fails the same way on every connection, so a " +
				"retry burns the whole budget and reports the failure later",
		},
		{
			name: "a missing table",
			err:  errors.New("turso: error: no such table: probe"),
			want: ConflictNone,
			why:  "a schema fault is not a race",
		},
		{
			name: "no error at all",
			err:  nil,
			want: ConflictNone,
			why:  "nothing to retry",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.err)
			if got != c.want {
				t.Fatalf("Classify(%v) = %q, want %q — %s", c.err, got, c.want, c.why)
			}
			if !got.Valid() {
				t.Fatalf("Classify(%v) = %q, which is not a value this build knows", c.err, got)
			}
			if want := c.want != ConflictNone; got.Retryable() != want {
				t.Fatalf("Classify(%v).Retryable() = %v, want %v", c.err, got.Retryable(), want)
			}
		})
	}
}
