package doclet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The trash's far end: what the trash reaper (internal/trashreap) reads and
// writes to keep [StatusTrashed]'s promise: recoverable for the trash term,
// gone after.
//
// The purge is decided here, not by the reaper: between its listing from this
// node's copy and the purge, somebody may restore the page, or restore and
// trash it again, which restarts its term. So [Store.PurgeTrashed] asks again
// inside the snapshot the record is decided in; a restore that lands first
// sends the decision round again, and that round refuses.

// ErrNotInTrash refuses a purge of a page that is not in the trash, or went in
// at or after the cutoff the purge was asked for: restored, or trashed again
// since it was listed, so its term has not run out.
var ErrNotInTrash = errors.New("doclet: the page is not in the trash past the cutoff")

// TrashedBefore lists the pages this node's copy holds in the trash since
// before cutoff, oldest first and then by id, at most limit of them.
//
// FROM THIS NODE'S OWN COPY, with no barrier: the list only nominates, and
// [Store.PurgeTrashed] decides again in a snapshot the broker arbitrates, so a
// copy that is behind costs a refused purge or a later pass, never a wrong one.
//
// OLDEST FIRST so a capped pass purges the longest past term; THE ID BREAKS A
// TIE so every node lists the same order. A SEEK, with no sort: the partial
// index on `trashed_at` and id (replicated 0066) holds only trashed pages, so
// the LIMIT stops the seek at the cap.
func (s *Store) TrashedBefore(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	switch {
	case cutoff.IsZero():
		return nil, invalid("cutoff", "a trash listing needs the instant a page must "+
			"have been trashed before; a zero one would list nothing honestly")
	case limit < 1:
		return nil, invalid("limit", "a trash listing lists at least one page, and %d "+
			"is fewer", limit)
	}
	var out []string
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, trashedBeforeStatement,
			store.EncodeTime(cutoff), string(StatusTrashed), limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("doclet: list the pages trashed before %s: %w",
			cutoff.UTC().Format(time.RFC3339), err)
	}
	return out, nil
}

// trashedBeforeStatement is the trash listing [Store.TrashedBefore] makes,
// named for the plan gate. It restates the partial index's predicate
// (`trashed_at IS NOT NULL`), which is what lets the planner choose it.
const trashedBeforeStatement = `SELECT id FROM pages_heads
	  WHERE trashed_at IS NOT NULL AND trashed_at < ? AND status = ?
	  ORDER BY trashed_at, id
	  LIMIT ?`

// PurgeTrashed destroys a page that has been in the trash since before cutoff,
// as [Store.Purge] does, and refuses one that has not ([ErrNotInTrash]).
//
// Made as the engine ([SystemActor]) under the actor's operation key
// ([Actor.OpKey]), so a retry within one pass collapses into the first. THE
// CONDITION IS ASKED IN THE DECISION'S SNAPSHOT: see the file head. A page
// gone answers [ErrNotFound], what a second reaper finds.
//
// QUIET: watchers were told at the trash; a second wake would spend a turn on
// every watching seat for nothing. The reason names the cutoff alone, so a
// retry's operation ([Store.operation]) is the first attempt's.
func (s *Store) PurgeTrashed(ctx context.Context, actor Actor, pageID string,
	cutoff time.Time) (Written, error) {

	if cutoff.IsZero() {
		return Written{}, invalid("cutoff", "a purge of the trash needs the instant "+
			"the page must have been trashed before")
	}
	return s.status(ctx, actor, pageID, statusMove{
		op: OpPurge, kind: ChangeRemoved, quiet: true,
		reason: fmt.Sprintf("in the trash since before %s, past the trash term",
			cutoff.UTC().Format(time.RFC3339)),
		admit: func(head Page) error { return trashedBefore(head, cutoff) },
	})
}

// trashedBefore refuses a head that is not in the trash since before cutoff.
func trashedBefore(head Page, cutoff time.Time) error {
	switch {
	case head.Status != StatusTrashed:
		return fmt.Errorf("%w: page %s is %s", ErrNotInTrash, head.ID, head.Status)
	case head.TrashedAt == nil:
		// A TRASHED HEAD THAT SAYS NOTHING OF WHEN is no page this build's
		// applier writes, and its term cannot be judged, so it is never
		// purged on one.
		return fmt.Errorf("%w: page %s is in the trash with no instant it went in",
			ErrNotInTrash, head.ID)
	case !head.TrashedAt.Before(cutoff):
		return fmt.Errorf("%w: page %s went into the trash at %s, not before %s",
			ErrNotInTrash, head.ID, head.TrashedAt.UTC().Format(time.RFC3339),
			cutoff.UTC().Format(time.RFC3339))
	}
	return nil
}
