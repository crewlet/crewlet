package store

import (
	"errors"
	"testing"
	"time"
)

// TestTheRetryClassifierCoversEveryTransientBeginFailure names the three
// conditions a transaction may be retried on, WHICH ONE each is, and why.
//
// The cause matters as much as the retryability: they carry different budgets
// and different pauses, because a five-second lock wait retried eight times on
// a one-millisecond backoff is forty seconds of stall — measured — where a
// stale snapshot retried the same way is microseconds. A classifier that
// answered only yes/no is what let those share a budget.
//
// The classifier is a TEXT match over the driver's own messages, which is
// fragile by construction — so the cases are written out rather than left to a
// reader to infer, and a message the driver stops using shows up here as a case
// that no longer describes anything.
func TestTheRetryClassifierCoversEveryTransientBeginFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want txCause
		why  string
	}{
		{
			name: "a stale snapshot",
			err:  errors.New("turso: error: database snapshot is stale"),
			want: causeStaleSnapshot,
			why: "a commit landed between this transaction's read and its write. " +
				"Since beginModeDriver only a DEFERRED begin can reach it, which " +
				"is DB.Read's — a write takes the lock at BEGIN and is never " +
				"aborted. It keeps the eight-attempt budget: the conflict returns " +
				"with no wait of its own, so the pause is microseconds",
		},
		{
			name: "a locked database",
			err:  errors.New("turso: error: database is locked"),
			want: causeLockTimeout,
			why: "another writer holds it — but this error only EXISTS after the " +
				"full busy timeout already elapsed, so it gets one retry rather " +
				"than eight and a pause anchored to that timeout rather than to a " +
				"commit",
		},
		{
			name: "a connection returned to the pool with a transaction open",
			err: errors.New("turso: error: Transaction error: cannot start a " +
				"transaction within a transaction"),
			want: causeDirtyConn,
			why: "the next attempt draws a DIFFERENT connection, and a clean one " +
				"begins normally. Without this the caller that happened to draw " +
				"the dirty one fails permanently — the projector's boot " +
				"reconcile restarted every two seconds for the life of the " +
				"process and never hydrated",
		},
		{
			name: "a constraint violation",
			err:  errors.New("turso: error: UNIQUE constraint failed: probe.id"),
			want: causeFatal,
			why: "the same statement fails the same way on every connection, so a " +
				"retry burns the whole budget and reports the failure later",
		},
		{
			name: "a missing table",
			err:  errors.New("turso: error: no such table: probe"),
			want: causeFatal,
			why:  "a schema fault is not a race",
		},
		{
			name: "no error at all",
			err:  nil,
			want: causeFatal,
			why:  "nothing to retry",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.err); got != c.want {
				t.Fatalf("classify(%v) = %s, want %s — %s", c.err,
					causeName(got), causeName(c.want), c.why)
			}
		})
	}
}

// A LOCK WAIT IS NOT RETRIED EIGHT TIMES, which is the whole point of the
// causes above being three facts rather than one bool.
//
// The arithmetic is what matters and it is not subtle: this error is only
// produced AFTER the full busy timeout has elapsed, so every attempt costs
// seconds rather than microseconds. At the stale-snapshot budget that is eight
// waits of five seconds — forty seconds of stall, with eight full replays of
// whatever the transaction had done — and it was measured at 40.7s by a test
// that forced the contention before this split existed.
//
// The pause is asserted too, and for the same reason: a widening microsecond
// backoff between two multi-second waits is noise, while a pause anchored to
// the configured timeout is what de-synchronises two writers that lost
// together.
func TestALockWaitGetsItsOwnBudgetRatherThanTheSnapshotOne(t *testing.T) {
	const busy = 5 * time.Second
	b := budget(busy)

	if got := b.attempts(causeLockTimeout); got != lockAttempts {
		t.Errorf("a lock timeout gets %d attempts, want %d", got, lockAttempts)
	}
	if got := b.attempts(causeStaleSnapshot); got != txAttempts {
		t.Errorf("a stale snapshot gets %d attempts, want %d — its budget is "+
			"measured for a conflict that returns with no wait of its own", got, txAttempts)
	}
	if b.attempts(causeLockTimeout) >= b.attempts(causeStaleSnapshot) {
		t.Errorf("a lock timeout is budgeted at least as generously as a stale "+
			"snapshot (%d vs %d); each of its attempts costs a full busy timeout, "+
			"so that is the forty-second stall this split exists to remove",
			b.attempts(causeLockTimeout), b.attempts(causeStaleSnapshot))
	}
	// The worst case a caller can be made to wait on the lock, stated as
	// the number rather than left to be derived: one retry means the
	// holder gets two busy timeouts, which is the dashboard's own query
	// timeout that defaultBusyTimeout is half of.
	if worst := time.Duration(b.attempts(causeLockTimeout)) * busy; worst != 10*time.Second {
		t.Errorf("the worst-case lock wait is %v, want 10s — the anchor is the "+
			"dashboard's query timeout, and defaultBusyTimeout is defined as half "+
			"of it", worst)
	}

	for range 50 {
		if beat := b.beat(causeLockTimeout, 0); beat >= busy/10 {
			t.Fatalf("the lock-retry pause was %v, want under %v: it is jitter to "+
				"separate two writers that timed out together, not a wait of its "+
				"own — the busy timeout already is that", beat, busy/10)
		}
	}
}

// AND A DIRTY CONNECTION IS RETRIED ON EVERY PATH, pinned included, because
// on every path the next attempt now draws a different connection.
//
// There were two budgets. A [Writer]'s refused this cause outright: the retry
// is the fix only when the next attempt gets ANOTHER connection, and a pin
// had one for the life of the handle, so retrying was eight guaranteed
// failures against the same dirty one. An attempt that leaves its transaction
// possibly open now retires the connection it ran on and a pinned writer pins
// a fresh one in its place, so the premise of the second budget is gone — and
// a budget kept after its reason is a policy nobody can re-derive.
func TestADirtyConnectionIsRetriedBecauseTheNextAttemptDrawsAnother(t *testing.T) {
	const busy = 5 * time.Second
	if got := budget(busy).attempts(causeDirtyConn); got == 0 {
		t.Error("a dirty connection is not retried, but the attempt that met it " +
			"retired it — so the next attempt draws a different one, on the pool " +
			"and on a pinned writer alike")
	}
	// ONE BUDGET, and this is what says so: every transaction in the
	// package reaches it through the same constructor, so a second policy
	// cannot appear without appearing here.
	if got := budget(busy).attempts(causeDirtyConn); got != txAttempts {
		t.Errorf("a dirty connection gets %d attempts, want %d — it is a failure "+
			"that returns with no wait of its own, which is the budget %s is "+
			"measured for", got, txAttempts, causeName(causeStaleSnapshot))
	}
}
