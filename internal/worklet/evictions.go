package worklet

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Evictions is every eviction this log's applied rows hold: what the trim
// reads to stop counting an evicted node on THIS log once its fence window
// has passed ([statelog.EvictionRow] says why the tombstones live in each
// domain's rows rather than in coordination).
//
// ON THE DOMAIN, about this log only: the trim asks the domain it trims, since
// one domain's table asked about another log's stream finds no rows and the
// evicted node is counted there for ever. THROUGH db, resolved on this call: the
// estate is not open while an adoption swaps its file or after close, and a
// statement on a closed pool panics in database/sql, where
// [store.ReplicatedReader.Read] answers [store.ErrNoEstate].
func (Domain) Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM tracker_evictions WHERE log_stream = ?
			ORDER BY node_id`, trackerStream)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		out = nil
		for rows.Next() {
			var (
				e          statelog.EvictionRow
				at, from   int64
				readmitted sql.NullInt64
			)
			if scanErr := rows.Scan(&e.NodeID, &at, &e.By, &from, &readmitted); scanErr != nil {
				return scanErr
			}
			e.At = store.DecodeTime(at)
			e.From = uint64(from)
			e.Readmitted = uint64(readmitted.Int64)
			// THE COMPARISON [Fence.Evicted] MAKES, so the trim and the
			// node's own fence never disagree about whether it is back.
			e.Back = readmitted.Valid && readmitted.Int64 > from
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("worklet: read the evictions on this log: %w", err)
	}
	return out, nil
}
