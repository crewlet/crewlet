package tracker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Evictions is every eviction this log's applied rows hold — the rows the trim
// reads to stop counting an evicted node on THIS log once its fence window has
// passed. See [statelog.EvictionRow] for why the fleet's tombstones live in each
// domain's own rows rather than in coordination.
//
// ON THE DOMAIN, AND ABOUT THIS LOG ONLY. It took the stream as an argument,
// and the trim passed every identity-claiming domain's own — so for the pages
// log it asked this table for rows filed under the pages stream, found none,
// and went on counting an evicted node there for as long as the fleet ran.
// Each domain answers for its own log now, and the trim asks the domain it is
// trimming.
//
// THROUGH THE NODE HANDLE'S REPLICATED PEER, and that is a correctness property
// rather than a style: the peer is a legitimate nil while an adoption swaps the
// file and after `Close`, and a statement issued on a nil pool panics inside
// database/sql — which is what an earlier shape of this did when the trim's own
// tick raced a shutdown. [store.DB.Read] answers [store.ErrNoEstate] instead.
func (Domain) Evictions(ctx context.Context, db *store.DB) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
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
			if err := rows.Scan(&e.NodeID, &at, &e.By, &from, &readmitted); err != nil {
				return err
			}
			e.At = store.DecodeTime(at)
			e.From = uint64(from)
			e.Readmitted = uint64(readmitted.Int64)
			// THE COMPARISON [Fence.Evicted] MAKES, so the trim and the
			// node's own fence can never disagree about whether it is
			// back.
			e.Back = readmitted.Valid && readmitted.Int64 > from
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tracker: read the evictions on this log: %w", err)
	}
	return out, nil
}
