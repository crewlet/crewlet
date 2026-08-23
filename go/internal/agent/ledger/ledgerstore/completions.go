// Package ledgerstore persists the turn engine's two cross-turn ledgers.
//
// A subpackage rather than more files in ledger, so ledger keeps the property
// its own doc claims: it imports nothing from crewlet. The turn context, the
// prompt builder and the API layer all hold ledger VALUES, and a package that
// dragged the database behind it would be held by all three.
//
// THE TWO LEDGERS HAVE DIFFERENT FAILURE POLARITIES, and both are deliberate:
//
//   - Completions fail OPEN in both directions. Not knowing whether work was
//     done has one safe answer and it is the pre-ledger one — do the work. A
//     read that failed closed would make a database blip look like a company
//     that had already answered everything.
//   - Conversations SPLIT: writes fail open, reads RAISE. Swallowing a read
//     failure made "unreadable" and "nothing said yet" one answer, and a
//     screen drew a database outage as a silent seat.
package ledgerstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/store"
)

var log = logging.Get("agent.ledgerstore")

// Completions answers "has this trigger already been worked?".
type Completions interface {
	// Worked returns the subset of keys already recorded for this seat.
	//
	// FAILS OPEN: an error yields no keys, so every trigger is treated as
	// unworked and runs. Duplicating a turn is recoverable; silently
	// answering nothing is not.
	Worked(ctx context.Context, handle string, keys []string) map[string]bool

	// Record marks a key worked. Best effort — see Worked.
	Record(ctx context.Context, handle, key, turnID string, at time.Time) error

	// Purge deletes rows completed before cutoff.
	//
	// The caller's cutoff floor is the scheduler's catchup window, not a
	// round number: deleting a row a tick could still evaluate lets that
	// fire run twice, which is the one failure this table prevents.
	Purge(ctx context.Context, cutoff time.Time) (int64, error)
}

// SQLCompletions is the durable completion ledger.
type SQLCompletions struct{ db *store.DB }

// NewCompletions wraps a database handle.
func NewCompletions(db *store.DB) *SQLCompletions { return &SQLCompletions{db: db} }

var _ Completions = (*SQLCompletions)(nil)

func (s *SQLCompletions) Worked(ctx context.Context, handle string, keys []string) map[string]bool {
	found, err := s.lookup(ctx, handle, keys)
	if err != nil {
		// ONE fail-open exit. Scanning straight into the result map and
		// bailing mid-loop would return a PARTIAL answer with no way for
		// the caller to tell — some triggers marked worked, the rest
		// unknown, and a seat that silently skips half a conversation.
		// Knowing nothing is the safe answer, and it is this one.
		log.Warn("completion_read_failed", "seat", handle, "error", err,
			"detail", "treating every trigger as unworked, which may duplicate a turn")
		return map[string]bool{}
	}
	return found
}

// lookup returns the recorded subset, or an error and NO partial result.
func (s *SQLCompletions) lookup(ctx context.Context, handle string, keys []string) (map[string]bool, error) {
	args := make([]any, 0, len(keys)+1)
	args = append(args, handle)
	placeholders := make([]string, 0, len(keys))
	for _, k := range keys {
		// An empty key is the documented "a turn with no ledgerable
		// trigger". It is never recorded, so querying for it can only
		// match a row some other writer put there — and if one ever did,
		// that single row would read as "already worked" for every
		// unkeyed turn this seat ever runs.
		if k == "" {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, k)
	}
	if len(placeholders) == 0 {
		return map[string]bool{}, nil
	}

	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT work_key FROM turn_completions WHERE agent_handle = ? AND work_key IN (`+
			strings.Join(placeholders, ", ")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	found := make(map[string]bool, len(placeholders))
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		found[key] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return found, nil
}

func (s *SQLCompletions) Record(ctx context.Context, handle, key, turnID string, at time.Time) error {
	if key == "" {
		return nil
	}
	if _, err := s.db.SQL().ExecContext(ctx,
		`INSERT INTO turn_completions (work_key, agent_handle, turn_id, completed_at)
		 VALUES (?, ?, ?, ?) ON CONFLICT (agent_handle, work_key) DO NOTHING`,
		key, handle, turnID, store.EncodeTime(at)); err != nil {
		return fmt.Errorf("ledgerstore: record completion for %s: %w", handle, err)
	}
	return nil
}

func (s *SQLCompletions) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.SQL().ExecContext(ctx,
		`DELETE FROM turn_completions WHERE completed_at < ?`, store.EncodeTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("ledgerstore: purge completions: %w", err)
	}
	return rowsAffected(res)
}

func rowsAffected(res sql.Result) (int64, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ledgerstore: rows affected: %w", err)
	}
	return n, nil
}
