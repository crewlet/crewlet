package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// EvictionRow is one node's removal from the fleet, as this node's own
// replicated rows hold it — the APPLIED shape, where [Eviction] is the record
// that wrote it. Two types because a record evolves additively on the wire and
// a row is whatever the table's columns are.
//
// # Why the fleet's tombstones live here rather than in coordination
//
// An eviction is a RECORD on the log like any other, so every node applies it
// into `tracker_evictions` and every node's copy is identical — which is what
// makes the applier's own eviction gate work when coordination cannot be
// reached at all. Putting a second copy in a bucket would give the fleet two
// answers to "is this node evicted", and the one the gate depends on is this
// one.
//
// The trim reads them for a different question: an evicted node stops being
// counted once its tombstone is older than the fence window, which is what
// lets an operator advance a floor an absent node is pinning.
type EvictionRow struct {
	// NodeID is who was evicted, and Stream which log this is about —
	// positions on different streams do not compare, which is why the row
	// is keyed on both.
	NodeID string
	Stream string

	// At is when the eviction was applied and By the operator who ran it.
	At time.Time
	By string

	// From is the COMPOSED position the eviction takes effect above, and
	// Readmitted the one a readmission takes effect at. A readmission is
	// an inverse commit rather than a delete, so a node that has been
	// taken back still has a row here.
	From       uint64
	Readmitted uint64
	IsBack     bool
}

// Evictions reads every eviction recorded for one log.
//
// SCOPED TO THE STREAM, because that is how the table is keyed and because a
// position from another stream names a dead number space. A caller passing the
// wrong stream gets no rows rather than another log's evictions.
//
// THROUGH THE STORE HANDLE AND NOT A `*sql.DB`, and that is a correctness
// property rather than a style. `DB.SQL()` answers a NIL pool on a handle that
// is not open, and nil is a legitimate, documented state of the replicated
// peer — an adoption closes it between its rename and its reopen, and `Close`
// leaves it nil. A statement issued on that pool panics inside database/sql,
// which is what this did: the trim's own tick raced a shutdown and took the
// process down with a nil-pointer dereference three frames inside the standard
// library. [store.DB.Read] answers [store.ErrNoEstate] instead, so a caller in
// flight reaches a closed estate honestly.
func Evictions(ctx context.Context, db *store.DB, stream string) ([]EvictionRow, error) {
	var out []EvictionRow
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM tracker_evictions WHERE log_stream = ?
			ORDER BY node_id`, stream)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		out = nil
		for rows.Next() {
			var (
				e          EvictionRow
				at         int64
				readmitted sql.NullInt64
			)
			if err := rows.Scan(&e.NodeID, &at, &e.By, &e.From, &readmitted); err != nil {
				return err
			}
			e.Stream = stream
			e.At = store.DecodeTime(at)
			e.Readmitted, e.IsBack = uint64(readmitted.Int64), readmitted.Valid
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tracker: read the evictions on %s: %w", stream, err)
	}
	return out, nil
}
