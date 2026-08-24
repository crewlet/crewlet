package store

import (
	"context"
	"fmt"
	"time"
)

// NotifyWindow is the width of one notification-valve window.
//
// One second, and the unit the operator's number is written in:
// `notification_rate_limit: 5` reads as "five notifications per seat per
// second". Wider would let a genuine loop run longer before tripping —
// two seats answering each other saturate a second easily — and narrower
// would trip on an ordinary burst of webhooks from one push.
const NotifyWindow = time.Second

// NotifyBucket is the valve's key for one seat.
//
// Keyed on the DERIVED agent id rather than the handle, matching the budget
// counter and the diary: renaming a seat starts a fresh window rather than
// inheriting whatever the previous holder of the name was doing.
func NotifyBucket(agentID string) string { return "notify:" + agentID }

// RateLimits is the shared fixed-window counter behind the notification
// valve.
type RateLimits struct{ db *DB }

// RateLimits returns the counter backed by this database.
func (d *DB) RateLimits() *RateLimits { return &RateLimits{db: d} }

// allowSQL increments a window's count only while it is under the limit.
//
// The WHERE on the DO UPDATE is what makes this one statement rather than a
// read and a write with a race between them: a peer racing the last of an
// allowance either lands inside it or updates nothing, and RowsAffected says
// which. It applies ONLY to the update branch — a window with no row yet
// takes the INSERT, which has no existing value to test — so the first call
// of a window is always allowed, which is right for any limit of 1 or more.
//
// No RETURNING: outside the intersection of the two certified drivers
// (rewrite/decisions/002).
const allowSQL = `
INSERT INTO rate_limits (bucket, window_start, hits)
VALUES (?, ?, 1)
ON CONFLICT (bucket, window_start) DO UPDATE SET hits = rate_limits.hits + 1
WHERE rate_limits.hits < ?`

// Allow reports whether a bucket is under its limit for the window now falls
// in, counting this call against it.
//
// A limit of 0 or less is UNLIMITED and costs nothing: the valve defaults
// off, so a deployment that never asked for it never touches the table.
//
// An error means the counter could not be reached. The caller FAILS OPEN on
// it — see the migration — so the error is returned for logging rather than
// as a refusal, and Allow reports true alongside it. Reporting false would
// turn a store blip into a company that answers nobody.
func (l *RateLimits) Allow(ctx context.Context, bucket string, limit int, now time.Time) (bool, error) {
	if limit <= 0 || bucket == "" {
		return true, nil
	}
	res, err := l.db.sql.ExecContext(ctx, allowSQL,
		bucket, EncodeTime(WindowStart(now, NotifyWindow)), limit)
	if err != nil {
		return true, fmt.Errorf("store: rate limit %s: %w", bucket, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return true, fmt.Errorf("store: rate limit %s: rows affected: %w", bucket, err)
	}
	return n > 0, nil
}

// WindowStart floors t to the start of the fixed window containing it.
//
// Exported because the store and any caller reasoning about the valve must
// floor identically — two flooring rules produce two windows for one instant,
// and the disagreement shows up only as a limit that behaves differently
// depending on who asked.
func WindowStart(t time.Time, width time.Duration) time.Time {
	if width <= 0 {
		return t.UTC()
	}
	return t.UTC().Truncate(width)
}

// Purge deletes windows that started before cutoff, returning how many went.
//
// Housekeeping, not expiry: a window older than one width can no longer
// affect an answer, so a deployment whose sweep never runs grows a table
// rather than limiting wrongly. The cutoff is honoured rather than clearing
// the table — a purge that wiped every window would reset the LIVE one and
// let a full limit through again the instant the sweep ran.
func (l *RateLimits) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := l.db.sql.ExecContext(ctx,
		`DELETE FROM rate_limits WHERE window_start < ?`, EncodeTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: purge rate limits: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge rate limits: rows affected: %w", err)
	}
	return n, nil
}
