package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ThreadFollows is the per-seat chat thread-follow state.
//
// One store for every chat backend, scoped by a `backend` key rather than a
// table each: the follow MODEL is identical everywhere — mention, collective
// address, participation, explicit subscription — and only the shape of a
// thread id differs. Two tables would mean two of every statement saying the
// same thing, and a third backend would need a third.
type ThreadFollows struct{ db *DB }

// ThreadFollows returns the follow state backed by this database.
func (d *DB) ThreadFollows() *ThreadFollows { return &ThreadFollows{db: d} }

// followSQL upserts, refreshing both the reason and the activity stamp.
//
// The reason is OVERWRITTEN rather than kept: a seat first pulled into a
// thread by a collective shout and later named personally is now following
// for the stronger reason, and an operator asking why it answered should see
// the mention, not the shout that happened to come first.
const followSQL = `
INSERT INTO chat_thread_follows
    (backend, agent_handle, channel_id, thread_id, reason, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (backend, agent_handle, channel_id, thread_id) DO UPDATE
SET reason = excluded.reason, updated_at = excluded.updated_at`

// Follow records that a seat follows a thread, or refreshes an existing
// follow's reason and activity stamp.
func (f *ThreadFollows) Follow(ctx context.Context, backend, handle, channel, thread, reason string, at time.Time) error {
	if backend == "" || handle == "" || thread == "" {
		return fmt.Errorf("store: follow needs a backend, a handle and a thread")
	}
	// created_at is written on the insert branch and deliberately NOT
	// touched on the update: it says when this seat first joined the
	// thread, which is the question updated_at cannot answer once it has
	// been refreshed by a re-assert.
	stamp := EncodeTime(at)
	if _, err := f.db.sql.ExecContext(ctx, followSQL,
		backend, handle, channel, thread, reason, stamp, stamp); err != nil {
		return fmt.Errorf("store: follow %s thread %s for %s: %w",
			backend, thread, handle, err)
	}
	return nil
}

// Following reports why a seat follows a thread, and whether it does.
//
// The reason comes back rather than a bare bool because a caller deciding
// what to do with a delivery wants it: "this seat was named here" and "this
// seat was in the room when somebody shouted" lead to different handling and
// very different log lines.
func (f *ThreadFollows) Following(ctx context.Context, backend, handle, channel, thread string) (string, bool, error) {
	if backend == "" || handle == "" || thread == "" {
		return "", false, nil
	}
	var reason string
	err := f.db.sql.QueryRowContext(ctx,
		`SELECT reason FROM chat_thread_follows
		 WHERE backend = ? AND agent_handle = ? AND channel_id = ? AND thread_id = ?`,
		backend, handle, channel, thread).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read follow %s thread %s for %s: %w",
			backend, thread, handle, err)
	}
	return reason, true, nil
}

// Unfollow drops a follow, reporting whether one was there.
//
// The counterpart of an explicit subscription: a seat told to stop watching
// a thread must actually stop, and waiting out the retention horizon is not
// stopping.
func (f *ThreadFollows) Unfollow(ctx context.Context, backend, handle, channel, thread string) (bool, error) {
	res, err := f.db.sql.ExecContext(ctx,
		`DELETE FROM chat_thread_follows
		 WHERE backend = ? AND agent_handle = ? AND channel_id = ? AND thread_id = ?`,
		backend, handle, channel, thread)
	if err != nil {
		return false, fmt.Errorf("store: unfollow %s thread %s for %s: %w",
			backend, thread, handle, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: unfollow %s thread %s for %s: rows affected: %w",
			backend, thread, handle, err)
	}
	return n > 0, nil
}

// Purge deletes follows last active before cutoff, returning how many went.
func (f *ThreadFollows) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := f.db.sql.ExecContext(ctx,
		`DELETE FROM chat_thread_follows WHERE updated_at < ?`, EncodeTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: purge thread follows: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge thread follows: rows affected: %w", err)
	}
	return n, nil
}
