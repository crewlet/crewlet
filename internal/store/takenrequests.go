package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TakenRequests is the node file as a signed channel's guard keeps the ids of
// the writes it took (internal/signedask's Ledger): `signed_request_taken`,
// one row per channel and id, until the instant the channel stops admitting
// the request. A row this file holds is a replay's, so a restart forgets none.
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

// ErrScratchLedger is a ledger over a SCRATCH node file ([DB.Scratch]), which
// is deleted at every boot and so keeps no id past a restart. Refused at every
// call, so a channel whose replay writes never starts on a node without the
// data role, rather than starting on a promise its file cannot keep.
var ErrScratchLedger = errors.New("store: this node's file is scratch, deleted at every boot, so it " +
	"keeps no signed request's id past a restart; a channel whose replay writes is served only on a " +
	"node with the data role (node.roles)")

// usable says why t can keep nothing: no file, or a scratch one.
func (t TakenRequests) usable() error {
	switch {
	case t.db == nil:
		return errNoTakenFile
	case t.db.Scratch():
		return ErrScratchLedger
	}
	return nil
}

// Keep records id as taken on channel until until, in one committed
// transaction. fresh is false, and nothing changed, where the file holds the
// id already.
func (t TakenRequests) Keep(ctx context.Context, channel, id string, until time.Time) (bool, error) {
	if err := t.usable(); err != nil {
		return false, err
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

// Forget deletes channel's ids whose until is not after now.
func (t TakenRequests) Forget(ctx context.Context, channel string, now time.Time) error {
	if err := t.usable(); err != nil {
		return err
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
