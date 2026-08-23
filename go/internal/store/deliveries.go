package store

import (
	"context"
	"fmt"
	"time"
)

// DeliveryTTL is how long a claimed delivery stays claimed.
//
// Sized to cover queue redelivery and an operator's replay, NOT a provider's
// own retry schedule — those back off for far longer (Plane starts at ~600 s)
// and only fire when the API layer failed to answer 2xx, which is exactly when
// the delivery was never claimed in the first place.
//
// The consequence of getting it wrong is visible in one direction only: too
// long and a deliberate replay ten minutes later vanishes into a row nothing
// will clear.
const DeliveryTTL = 5 * time.Minute

// Deliveries is the first-claim-wins registry of handled inbound deliveries.
type Deliveries struct {
	db  *DB
	ttl time.Duration
}

// DeliveryLog returns the dedupe registry backed by this database.
func (d *DB) DeliveryLog() *Deliveries { return &Deliveries{db: d, ttl: DeliveryTTL} }

// WithTTL returns a registry that expires claims after ttl.
//
// A method rather than an Open option because the TTL is a property of the
// QUESTION, not of the database: the maintenance sweep and the webhook edge
// hold the same handle and want different windows.
func (l *Deliveries) WithTTL(ttl time.Duration) *Deliveries {
	if ttl <= 0 {
		ttl = DeliveryTTL
	}
	return &Deliveries{db: l.db, ttl: ttl}
}

// TTL reports the claim window.
func (l *Deliveries) TTL() time.Duration { return l.ttl }

// claimSQL re-claims a row older than the TTL rather than refusing it.
//
// Without the time predicate the claim is PERMANENT, which is not what the
// TTL, this file or the migration say. The WHERE rides on the DO UPDATE, so it
// stays one statement: a row that fails the predicate updates nothing and
// reports no rows affected.
//
// Reported through RowsAffected rather than RETURNING, which is a newer SQLite
// feature and outside the dialect intersection the two certified drivers share
// (rewrite/decisions/002).
const claimSQL = `
INSERT INTO webhook_deliveries (source, delivery_key, seen_at)
VALUES (?, ?, ?)
ON CONFLICT (source, delivery_key) DO UPDATE SET seen_at = ?
WHERE webhook_deliveries.seen_at < ?`

// Claim records a delivery, reporting whether this caller won it.
//
// True means handle it; false means somebody already did. An empty key is
// always won: there is no stable identity to dedupe on, and handling a
// possible duplicate beats dropping a delivery nobody else holds a copy of.
func (l *Deliveries) Claim(ctx context.Context, source, key string, now time.Time) (bool, error) {
	if key == "" {
		return true, nil
	}
	stamp := EncodeTime(now)
	res, err := l.db.sql.ExecContext(ctx, claimSQL,
		source, key, stamp, stamp, EncodeTime(now.Add(-l.ttl)))
	if err != nil {
		return false, fmt.Errorf("store: claim %s delivery %s: %w", source, key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim %s delivery %s: rows affected: %w",
			source, key, err)
	}
	return n > 0, nil
}

// Release drops a claim, so the provider's retry can win it.
//
// The compensating half of Claim, and it is load-bearing rather than tidy: the
// claim is taken BEFORE the delivery is republished, because two concurrent
// retries must not both wake the seat. If that republish then fails, the
// delivery was claimed and never handled — and the provider's retry, which is
// the only other copy of it, would be refused by a row nothing will clear
// inside the TTL. Silent, unretried, unrecoverable loss.
//
// Best effort by nature: a release that fails leaves the claim standing, which
// costs one delivery and is exactly the situation without this method at all.
func (l *Deliveries) Release(ctx context.Context, source, key string) error {
	if key == "" {
		return nil
	}
	if _, err := l.db.sql.ExecContext(ctx,
		`DELETE FROM webhook_deliveries WHERE source = ? AND delivery_key = ?`,
		source, key); err != nil {
		return fmt.Errorf("store: release %s delivery %s: %w", source, key, err)
	}
	return nil
}

// Purge deletes claims last seen before cutoff, returning how many went.
//
// Garbage collection, not expiry — Claim already enforces the TTL — so a
// deployment whose sweep never runs grows a table rather than answering wrongly.
func (l *Deliveries) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := l.db.sql.ExecContext(ctx,
		`DELETE FROM webhook_deliveries WHERE seen_at < ?`, EncodeTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: purge deliveries: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge deliveries: rows affected: %w", err)
	}
	return n, nil
}
