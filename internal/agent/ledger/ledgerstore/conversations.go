package ledgerstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/store"
)

// Conversations is what this seat already said in one thread, issue or channel.
type Conversations interface {
	// Append records one turn's entry.
	//
	// FAILS OPEN — the caller logs and carries on. Losing an entry costs
	// the next turn some history; failing the turn over it costs the reply
	// the requester is waiting for.
	//
	// It STAMPS the entry's [ledger.Session.Ordinal] — one past the newest
	// entry already held for this conversation — so an entry says its
	// place in the whole record however many older ones are later trimmed.
	//
	// Deduped on the WORK key, never a turn id: two nodes completing one
	// trigger mint two turn ids, so a turn-keyed dedupe RECORDS the
	// duplicate instead of collapsing it.
	Append(ctx context.Context, handle, conversation string, entry ledger.Session,
		workKey string, at time.Time, maxEntries int) error

	// History returns prior entries oldest-first, newest `limit` kept. What
	// it leaves out, and what a trim or the sweep already deleted, the
	// oldest entry's [ledger.Session.Ordinal] counts.
	//
	// RAISES on failure rather than returning nothing. Swallowing made
	// "unreadable" and "nothing said yet" one answer, and a screen drew a
	// database outage as a silent seat.
	History(ctx context.Context, handle, conversation string, limit int) ([]ledger.Session, error)

	// Threads lists the conversations this seat has entries in, newest
	// activity first.
	//
	// The OPERATOR's view of the same ledger a turn reads. It answers
	// "which threads is this seat carrying context for", which History
	// cannot: History needs a conversation key, and the key is exactly
	// what somebody looking at a seat does not have.
	//
	// RAISES like History, and for the same reason: an unreadable ledger
	// and a seat that has said nothing are opposite facts.
	Threads(ctx context.Context, handle string, limit int) ([]Thread, error)

	// Purge deletes entries older than cutoff.
	Purge(ctx context.Context, cutoff time.Time) (int64, error)
}

// Thread is one conversation a seat holds entries for.
type Thread struct {
	// Key is the surface-scoped conversation identity — a thread, an
	// issue, a channel. Opaque here; the notification layer owns its
	// grammar.
	Key string

	// Entries is how many turns this seat has recorded in it.
	Entries int

	// LastAt is the newest entry's stamp, which is what the listing is
	// ordered by: a reader scanning a seat's threads is looking for the
	// one that moved most recently.
	LastAt time.Time
}

// SQLConversations is the durable conversation ledger.
type SQLConversations struct{ db *store.DB }

// NewConversations wraps a database handle.
func NewConversations(db *store.DB) *SQLConversations { return &SQLConversations{db: db} }

var _ Conversations = (*SQLConversations)(nil)

// Append adds one turn to a thread's history. Writes fail open.
func (s *SQLConversations) Append(ctx context.Context, handle, conversation string,
	entry ledger.Session, workKey string, at time.Time, maxEntries int,
) error {
	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		// THE ORDINAL IS READ AND WRITTEN IN ONE TRANSACTION, which takes
		// the write lock at BEGIN, so two appends to one conversation
		// cannot both read the same newest entry.
		ordinal, err := nextOrdinal(ctx, tx, handle, conversation)
		if err != nil {
			return err
		}
		entry.Ordinal = ordinal
		blob, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("ledgerstore: encode session for %s: %w", handle, err)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO conversation_sessions
			   (entry_id, agent_handle, conversation_key, work_key, turn_id,
			    entry, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT DO NOTHING`,
			// The row's name across the fleet, minted here because this is
			// where the row is created. `id` is an AUTOINCREMENT and means
			// nothing off this node; memory replication carries the entry
			// under this one. Fresh every call, including for a duplicate
			// the guard below is about to reject — an id nothing keeps
			// costs nothing.
			uuid.NewString(),
			handle, conversation, workKey, entry.TurnID, string(blob), store.EncodeTime(at),
		); err != nil {
			return err
		}
		// DO NOTHING rather than catching a constraint error, because the
		// duplicate is EXPECTED — a redelivery of a trigger this seat has
		// already answered — and telling it apart from a real write
		// failure would mean parsing driver-specific error text. The
		// partial unique index is the guard; this is it succeeding.
		if maxEntries <= 0 {
			return nil
		}
		// TRIM ON WRITE. The retention sweep alone is not enough: a chat
		// DM keys on the whole CHANNEL, so one conversation never stops
		// growing however recent its entries are.
		//
		// What it deletes is COUNTED, not lost from sight: the survivors
		// keep the ordinals they were stamped with, so the oldest one says
		// how many turns came before it, and [ledger.RenderHistory] tells
		// the seat they are not shown. What was said in them is still in
		// the thread the conversation key names, which is where that
		// marker sends the seat; each turn's own record is its phase
		// events, which carry the same conversation key.
		_, err = tx.ExecContext(ctx,
			`DELETE FROM conversation_sessions
			 WHERE agent_handle = ? AND conversation_key = ? AND id NOT IN (
			   SELECT id FROM conversation_sessions
			   WHERE agent_handle = ? AND conversation_key = ?
			   ORDER BY created_at DESC, id DESC LIMIT ?)`,
			handle, conversation, handle, conversation, maxEntries)
		return err
	})
}

// nextOrdinal is one past the ordinal of the newest entry this conversation
// holds, or 1 for its first.
//
// Off the newest entry's own stamp rather than a COUNT of rows: the trim and
// the sweep delete rows, so a count restarts low after either and hands two
// entries the same place. An entry stored before ordinals existed carries 0,
// and the next one after it is numbered 1 — which undercounts what came
// before, and never claims turns that did not happen.
func nextOrdinal(ctx context.Context, tx *sql.Tx, handle, conversation string) (int, error) {
	var blob string
	err := tx.QueryRowContext(ctx,
		`SELECT entry FROM conversation_sessions
		 WHERE agent_handle = ? AND conversation_key = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		handle, conversation).Scan(&blob)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 1, nil
	case err != nil:
		return 0, fmt.Errorf("ledgerstore: read the newest entry of %s's "+
			"conversation: %w", handle, err)
	}
	var newest ledger.Session
	if err := json.Unmarshal([]byte(blob), &newest); err != nil {
		// An undecodable newest row is History's to report, which it does
		// by name; here it only means the count restarts, which
		// undercounts rather than failing the append.
		return 1, nil //nolint:nilerr // see the comment above
	}
	return newest.Ordinal + 1, nil
}

// History returns a thread's prior turns, most recent last. Reads RAISE:
// "unreadable" and "nothing said yet" are different facts, and a screen
// that drew a database outage as a silent seat is why.
func (s *SQLConversations) History(ctx context.Context, handle, conversation string, limit int) ([]ledger.Session, error) {
	query := `SELECT entry FROM conversation_sessions
	          WHERE agent_handle = ? AND conversation_key = ?
	          ORDER BY created_at DESC, id DESC`
	args := []any{handle, conversation}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ledgerstore: read conversation for %s: %w", handle, err)
	}
	defer rows.Close()

	var out []ledger.Session
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, fmt.Errorf("ledgerstore: scan conversation for %s: %w", handle, err)
		}
		var entry ledger.Session
		if err := json.Unmarshal([]byte(blob), &entry); err != nil {
			// One unreadable row must not cost the whole history: the
			// other entries still stop a duplicate reply, which is what
			// the ledger is for. Skipped loudly rather than silently.
			log.WarnContext(ctx, "conversation_entry_undecodable", "seat", handle,
				"conversation", conversation, "error", err)
			continue
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledgerstore: read conversation for %s: %w", handle, err)
	}
	// Queried newest-first so LIMIT keeps the RECENT ones, reversed here
	// because a conversation reads forwards. Ordering ascending in SQL and
	// limiting would have kept the oldest — the opposite of what a
	// follow-up turn needs.
	slices.Reverse(out)
	return out, nil
}

// Threads lists the conversations this seat holds entries in.
//
// Aggregated in SQL rather than by reading every entry: a chat DM keys on the
// whole CHANNEL, so one conversation can hold the trim limit's worth of
// entries and a seat can hold many conversations. Counting them in Go would
// move the entire ledger through the process to render a list of keys.
func (s *SQLConversations) Threads(ctx context.Context, handle string, limit int) ([]Thread, error) {
	query := `SELECT conversation_key, COUNT(*), MAX(created_at)
	          FROM conversation_sessions WHERE agent_handle = ?
	          GROUP BY conversation_key
	          ORDER BY MAX(created_at) DESC, conversation_key`
	args := []any{handle}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ledgerstore: list conversations for %s: %w", handle, err)
	}
	defer rows.Close()

	var out []Thread
	for rows.Next() {
		var thread Thread
		var micros int64
		if err := rows.Scan(&thread.Key, &thread.Entries, &micros); err != nil {
			return nil, fmt.Errorf("ledgerstore: scan conversations for %s: %w", handle, err)
		}
		thread.LastAt = store.DecodeTime(micros)
		out = append(out, thread)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledgerstore: list conversations for %s: %w", handle, err)
	}
	return out, nil
}

// Purge deletes turns recorded before cutoff.
func (s *SQLConversations) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.SQL().ExecContext(ctx,
		`DELETE FROM conversation_sessions WHERE created_at < ?`, store.EncodeTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("ledgerstore: purge conversations: %w", err)
	}
	return rowsAffected(res)
}

// rowsAffected reads a result's row count, naming the package in the failure.
//
// Its own function because a driver that cannot report one is a driver
// problem rather than a query problem, and a caller reading a bare
// "unsupported" from three call sites cannot tell which.
func rowsAffected(res sql.Result) (int64, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ledgerstore: rows affected: %w", err)
	}
	return n, nil
}
