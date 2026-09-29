package chart

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Covers proves, in one snapshot, that this node's chart rows hold every
// record the chart log held at end — the log's last sequence, which the caller
// reads BEFORE asking, so a record landing between the two can only make the
// answer "not current" — or says, wrapping [ErrNotCurrent], why they do not.
//
// For a caller that judges something ABSENT from a derivation of these rows
// without reading them itself: the mailbox sweep retires a seat's mail once
// the seat has been missing from the roster for a day, and a roster composed
// from rows that have not applied the hire — this node behind its peers, or
// holding the hire's record it could not apply — reads a live seat as gone.
// The proof is against what the snapshot APPLIED, never its checkpoint, for
// the reason [Reader.SealedNames] gives.
func (r *Reader) Covers(ctx context.Context, end uint64) error {
	return r.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		prefix, err := statelog.PrefixIn(ctx, tx, Domain{})
		if err != nil {
			return fmt.Errorf("chart: read how much of the log these rows hold: %w", err)
		}
		return coversLog(prefix, end)
	})
}
