package a2a

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// SQLStore is the durable channel store.
type SQLStore struct{ db *store.DB }

// NewSQLStore wraps a database handle.
func NewSQLStore(db *store.DB) *SQLStore { return &SQLStore{db: db} }

var _ Store = (*SQLStore)(nil)

const openSQL = `
INSERT INTO a2a_channels (channel_id, requester, target, message_count, opened_at, closed_at, last_at)
VALUES (?, ?, ?, ?, ?, NULL, ?)
ON CONFLICT (channel_id) DO NOTHING`

// Open records a new channel.
//
// ON CONFLICT DO NOTHING rather than an error: the id is minted per ask, so a
// collision means a retried publish of one ask, and raising would turn a
// harmless retry into a failed tool call the agent then re-plans around.
func (s *SQLStore) Open(ctx context.Context, ch Channel) error {
	if ch.ID == "" {
		return fmt.Errorf("a2a: cannot open a channel with no id")
	}
	if _, err := s.db.SQL().ExecContext(ctx, openSQL,
		ch.ID, ch.Requester, ch.Target, ch.Messages,
		store.EncodeTime(ch.OpenedAt), store.EncodeTime(ch.LastAt),
	); err != nil {
		return fmt.Errorf("a2a: open channel %s: %w", ch.ID, err)
	}
	return nil
}

const selectSQL = `
SELECT channel_id, requester, target, message_count, opened_at, closed_at, last_at
FROM a2a_channels WHERE channel_id = ?`

// Get returns one channel by key.
func (s *SQLStore) Get(ctx context.Context, id string) (Channel, error) {
	row := s.db.SQL().QueryRowContext(ctx, selectSQL, id)
	ch, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNoChannel
	}
	if err != nil {
		return Channel{}, fmt.Errorf("a2a: read channel %s: %w", id, err)
	}
	return ch, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanChannel(row scanner) (Channel, error) {
	var (
		ch       Channel
		opened   int64
		last     int64
		closedAt sql.NullInt64
	)
	if err := row.Scan(&ch.ID, &ch.Requester, &ch.Target, &ch.Messages,
		&opened, &closedAt, &last); err != nil {
		return Channel{}, err
	}
	ch.OpenedAt = store.DecodeTime(opened)
	ch.LastAt = store.DecodeTime(last)
	ch.ClosedAt = store.TimeAt(closedAt)
	return ch, nil
}

// Close marks the channel closed.
//
// The UPDATE is guarded on closed_at IS NULL so a second close does not move
// the timestamp: both parties may close, and the first one is when it actually
// happened. The read follows the write in one transaction so the returned
// state is the one that is stored, rather than the one this call intended.
func (s *SQLStore) Close(ctx context.Context, id string, at time.Time) (Channel, error) {
	var ch Channel
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE a2a_channels SET closed_at = ?, last_at = ? WHERE channel_id = ? AND closed_at IS NULL`,
			store.EncodeTime(at), store.EncodeTime(at), id,
		); err != nil {
			return err
		}
		var err error
		ch, err = scanChannel(tx.QueryRowContext(ctx, selectSQL, id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNoChannel
	}
	if err != nil {
		return Channel{}, fmt.Errorf("a2a: close channel %s: %w", id, err)
	}
	return ch, nil
}

// CountMessage increments the counter and bumps the activity stamp.
func (s *SQLStore) CountMessage(ctx context.Context, id string, at time.Time) (int, error) {
	var count int
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE a2a_channels SET message_count = message_count + 1, last_at = ? WHERE channel_id = ?`,
			store.EncodeTime(at), id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return sql.ErrNoRows
		}
		return tx.QueryRowContext(ctx,
			`SELECT message_count FROM a2a_channels WHERE channel_id = ?`, id).Scan(&count)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoChannel
	}
	if err != nil {
		return 0, fmt.Errorf("a2a: count message on %s: %w", id, err)
	}
	return count, nil
}

// CloseIdle closes every open channel idle since before cutoff.
//
// Selected then updated by id rather than updated in one statement, because
// the caller needs to know WHICH channels closed: each one is an ask that was
// never answered, and publishing that is how an operator finds a seat that has
// stopped replying.
func (s *SQLStore) CloseIdle(ctx context.Context, cutoff, at time.Time) ([]Channel, error) {
	var closed []Channel
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT channel_id, requester, target, message_count, opened_at, closed_at, last_at
			 FROM a2a_channels WHERE closed_at IS NULL AND last_at < ? ORDER BY channel_id`,
			store.EncodeTime(cutoff))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			ch, err := scanChannel(rows)
			if err != nil {
				return err
			}
			ch.ClosedAt = at
			closed = append(closed, ch)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, ch := range closed {
			if _, err := tx.ExecContext(ctx,
				`UPDATE a2a_channels SET closed_at = ? WHERE channel_id = ? AND closed_at IS NULL`,
				store.EncodeTime(at), ch.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("a2a: close idle channels: %w", err)
	}
	return closed, nil
}

// Purge deletes channels closed before cutoff.
func (s *SQLStore) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.SQL().ExecContext(ctx,
		`DELETE FROM a2a_channels WHERE closed_at IS NOT NULL AND closed_at < ?`,
		store.EncodeTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("a2a: purge channels: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("a2a: purge channels: %w", err)
	}
	return n, nil
}
