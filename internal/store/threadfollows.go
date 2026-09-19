package store

import (
	"context"
	"fmt"
	"time"
)

// ThreadFollows is what is LEFT of the per-seat chat thread-follow state after
// migration 0028 moved it to the coordination store.
//
// # Why this survives at all
//
// The follows are coordination's now — a company-wide fact, and the node's own
// file was the wrong estate for it (see ADR-0003, and the migration's own
// text). What is left here is the one-time HANDOFF SOURCE: rows written by
// builds before the move are still in this table, and nothing but Go code can
// carry them onto the bucket, because a `.sql` file has no KV client.
//
// So the surface is two methods rather than the five it had. There is no
// Follow, no Following and no Purge, and their absence is the point: a writer
// here would be a second place the answer lives, and a reader would be a node
// answering from its own copy of a fact the fleet decides. Only
// internal/notify/followsync uses this, once per boot, and its steady state is
// an empty table.
type ThreadFollows struct{ db *DB }

// ThreadFollows returns the handoff source backed by this database.
func (d *DB) ThreadFollows() *ThreadFollows { return &ThreadFollows{db: d} }

// Follow is one row carried onto the fleet.
//
// The reason and the activity stamp travel; `created_at` does not, because the
// coordination record has no field for it — a follow there carries why and
// when it was last asserted, which is what a reader asks. Losing the
// first-joined instant on rows written before the move is the honest price of
// the move, and it is stated here rather than discovered.
//
// WHAT THE STAMP DOES NOT DO is preserve the expiry clock. Retention on the
// coordination side is the BUCKET's age, measured from the write, and the
// record's own instant is diagnostic — nothing branches on it (see
// `coord/kv.followRecord`). So a row that was 89 days stale when it was
// handed off gets a fresh ninety, and the honest reading of that is that the
// handoff is a re-assert: the same thing a mention would have done, once, for
// rows somebody has been following all along.
type Follow struct {
	Backend   string
	Handle    string
	Channel   string
	Thread    string
	Reason    string
	UpdatedAt time.Time
}

// List reads every follow this node still holds locally.
//
// WHOLE, not paged, and deliberately: this is a table nothing has written
// since the build that moved it, read once at boot, and the alternative —
// paging — would need a cursor that survives a crash mid-handoff for no gain,
// since a partial handoff simply leaves the rest of the rows to the next boot.
func (f *ThreadFollows) List(ctx context.Context) ([]Follow, error) {
	rows, err := f.db.sql.QueryContext(ctx,
		`SELECT backend, agent_handle, channel_id, thread_id, reason, updated_at
		 FROM chat_thread_follows
		 ORDER BY backend, agent_handle, channel_id, thread_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list the thread follows to hand off: %w", err)
	}
	defer rows.Close()

	var out []Follow
	for rows.Next() {
		var one Follow
		var updated int64
		if err := rows.Scan(&one.Backend, &one.Handle, &one.Channel,
			&one.Thread, &one.Reason, &updated); err != nil {
			return nil, fmt.Errorf("store: scan a thread follow: %w", err)
		}
		one.UpdatedAt = DecodeTime(updated)
		out = append(out, one)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the thread follows to hand off: %w", err)
	}
	return out, nil
}

// Drop removes one follow by its identity, reporting whether a row went.
//
// BY IDENTITY rather than by rowid, so it is safe to call with a value that
// came back from a List taken earlier: the table has no writer, so a row
// cannot have changed under it, and a row that is already gone answers false
// rather than failing.
func (f *ThreadFollows) Drop(ctx context.Context, one Follow) (bool, error) {
	res, err := f.db.sql.ExecContext(ctx,
		`DELETE FROM chat_thread_follows
		 WHERE backend = ? AND agent_handle = ? AND channel_id = ? AND thread_id = ?`,
		one.Backend, one.Handle, one.Channel, one.Thread)
	if err != nil {
		return false, fmt.Errorf("store: drop the handed-off follow on %s thread %s for %s: %w",
			one.Backend, one.Thread, one.Handle, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: drop the handed-off follow on %s thread %s for %s: "+
			"rows affected: %w", one.Backend, one.Thread, one.Handle, err)
	}
	return n > 0, nil
}
