package pages

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE EVICTION GATE'S WRITE SIDE AND ITS READ SIDE, for this domain's log.
//
// An eviction is a record ON A LOG, and each applier reads the gate from its
// own domain's table: [Applier.Gated] drops a record by what `pages_evictions`
// says, and the tracker's applier by what `tracker_evictions` says. So an
// eviction published on the tracker's log drops nothing here — the evicted
// node's knowledge-base writes go on applying on every peer — and the trim,
// which counts nodes per log, goes on counting it against this log's floor.
// Evicting a node from the fleet is one record on every log whose applier
// installs the gate; this file is this log's.

// EvictNode installs the gate that drops a node's records on this log.
//
// THE GATE IS POSITIONAL, as the tracker's is: the record says "what this node
// wrote above my own position applies nowhere", so every node reaches the same
// verdict about every record with no clock and no coordination read — which
// is what makes it hold when coordination cannot be reached at all.
func (s *Store) EvictNode(ctx context.Context, actor Actor, opID, nodeID string) (
	statelog.Result, error) {

	return s.gateNode(ctx, actor, opID, nodeID, false)
}

// ReadmitNode is the INVERSE COMMIT rather than a delete, so an eviction's whole
// history survives a replay — and a node that was evicted, readmitted and
// evicted again reads correctly rather than as one long absence.
func (s *Store) ReadmitNode(ctx context.Context, actor Actor, opID, nodeID string) (
	statelog.Result, error) {

	return s.gateNode(ctx, actor, opID, nodeID, true)
}

func (s *Store) gateNode(ctx context.Context, actor Actor, opID, nodeID string,
	readmit bool) (statelog.Result, error) {

	if err := actor.validate(); err != nil {
		return statelog.Result{}, err
	}
	if nodeID == "" {
		return statelog.Result{}, invalid("node", "an eviction names no node")
	}
	subject := EvictionSubject(nodeID)
	scope := ScopeSet{Subject: true}
	at := s.now()
	return s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return s.decide(actor, subject, OpEviction, scope, opID, Eviction{
				V: GateRecordVersion, NodeID: nodeID,
				EvictedBy: actor.Name(), EvictedAt: at, Readmitted: readmit,
			}, nil, at)
		},
	})
}

// EvictionRow is one node's eviction from this log as this node's own
// replicated rows hold it — the APPLIED shape, where [Eviction] is the record
// that wrote it.
//
// THE TRACKER'S SHAPE, field for field ([tracker.EvictionRow]), because the
// trim reads both through one conversion and a second shape is a second
// reading of what "back" means.
type EvictionRow struct {
	// NodeID is who was evicted, and Stream the log this row is about.
	NodeID string
	Stream string

	// At is when the eviction was applied and By who ran it, as the record
	// named them.
	At time.Time
	By string

	// From is the COMPOSED position the eviction takes effect above, and
	// Readmitted the one a readmission takes effect at. A readmission is an
	// inverse commit rather than a delete, and a later eviction clears it,
	// so IsBack says whether the LAST gesture on this node was a
	// readmission.
	From       uint64
	Readmitted uint64
	IsBack     bool
}

// Evictions reads every eviction this node has applied from this domain's log,
// out of db — the REPLICATED estate, where the applier writes them.
//
// NOT SCOPED BY A STREAM ARGUMENT, unlike [tracker.Evictions]: this table has
// no stream column, because nothing but this domain's applier writes it and it
// applies this domain's log alone. The rows carry the log's name all the same,
// so a caller holding rows from both domains can tell them apart.
//
// THROUGH THE STORE HANDLE AND NOT A `*sql.DB`: [store.DB.Read] answers
// [store.ErrNoEstate] for a replicated estate that is not open — an adoption
// closes it between its rename and its reopen — where a statement on a nil
// pool panics.
func Evictions(ctx context.Context, db *store.DB) ([]EvictionRow, error) {
	stream := Domain{}.Stream().Name
	var out []EvictionRow
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM pages_evictions ORDER BY node_id`)
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
		return nil, fmt.Errorf("pages: read the evictions on %s: %w", stream, err)
	}
	return out, nil
}
