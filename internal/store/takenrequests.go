package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TakenRequestsMigration is the node migration that made the table
// [TakenRequests] keeps, for the gates that name it.
const TakenRequestsMigration = "0073_a_signed_writes_id_outlives_its_nodes_restart.sql"

// TakenRequests is the node file as a signed channel's guard keeps the ids of
// the writes it took (internal/signedask's Ledger): `signed_request_taken`,
// one row per channel and id, until the instant the channel stops admitting
// the request.
//
// "KEPT" MEANS IT SURVIVES A CRASH, not only a clean close: every write is one
// [DB.Tx], whose commit the node file makes under `PRAGMA synchronous = FULL`
// (an fsync per commit, and F_FULLFSYNC on Darwin; [connector]), so a Keep
// that returned is on the disk before the request it records runs.
type TakenRequests struct{ db *DB }

// NewTakenRequests is d, the node's own file, as a guard's ledger.
func NewTakenRequests(d *DB) TakenRequests { return TakenRequests{db: d} }

// errNoTakenFile is a ledger built over no file: a wiring fault, refused at
// every call rather than a panic in a handler that serves the fleet.
var errNoTakenFile = errors.New("store: the signed requests' ledger has no node file")

// Keep records id as taken on channel until until, in one committed
// transaction. fresh is false, and nothing changed, where the file holds the
// id already.
func (t TakenRequests) Keep(ctx context.Context, channel, id string, until time.Time) (bool, error) {
	if t.db == nil {
		return false, errNoTakenFile
	}
	var fresh bool
	err := t.db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO signed_request_taken (channel, id, until)
			VALUES (?, ?, ?) ON CONFLICT (channel, id) DO NOTHING`, channel, id, EncodeTime(until))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		fresh = n == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("store: keep signed request %s of %s: %w", id, channel, err)
	}
	return fresh, nil
}

// Kept deletes channel's ids whose until is not after now, and answers the
// rest by id.
func (t TakenRequests) Kept(ctx context.Context, channel string, now time.Time) (map[string]time.Time, error) {
	if err := t.Forget(ctx, channel, now); err != nil {
		return nil, err
	}
	kept := map[string]time.Time{}
	err := t.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, until FROM signed_request_taken WHERE channel = ?`, channel)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var until int64
			if err := rows.Scan(&id, &until); err != nil {
				return err
			}
			kept[id] = DecodeTime(until)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: read the signed requests %s took: %w", channel, err)
	}
	return kept, nil
}

// Forget deletes channel's ids whose until is not after now.
func (t TakenRequests) Forget(ctx context.Context, channel string, now time.Time) error {
	if t.db == nil {
		return errNoTakenFile
	}
	err := t.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM signed_request_taken WHERE channel = ? AND until <= ?`, channel, EncodeTime(now))
		return err
	})
	if err != nil {
		return fmt.Errorf("store: forget the signed requests %s no longer admits: %w", channel, err)
	}
	return nil
}
