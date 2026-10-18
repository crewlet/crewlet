package artifactdata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/store"
)

// THE HOT HISTORY: one row per record a store applies, refusals included, each
// naming what changed with the values before and after (each bounded, a hash
// beyond) and the state digest after it, kept [artifacts.DataHistoryHotWindow]
// and at most [historyCap] per store, pruned in each record's own transaction
// in one total order from the record's broker time. Deterministic, because both
// bounds read only the record and same-log rows.

// ChangeOp is what one record did to one document.
type ChangeOp string

const (
	ChangeCreate   ChangeOp = "create"
	ChangeUpdate   ChangeOp = "update"
	ChangeDelete   ChangeOp = "delete"
	ChangeMigrate  ChangeOp = "migrate"  // a schema change rewrote it
	ChangeConflict ChangeOp = "conflict" // a guard named it and it had moved
	ChangeSchema   ChangeOp = "schema"   // a schema change touched this collection
)

// Change is one document's part of a history row.
type Change struct {
	Collection string        `json:"c"`
	Doc        string        `json:"d,omitempty"`
	Op         ChangeOp      `json:"op"`
	Before     int64         `json:"bv,omitempty"`
	After      int64         `json:"av,omitempty"`
	Fields     []FieldChange `json:"f,omitempty"`
}

// FieldChange is one field's values before and after. An absent value is a
// field the document did not hold; a value past
// [artifacts.MaxHistoryValueBytes] is recorded by its SHA-256 instead, and the
// row is then not undoable.
type FieldChange struct {
	Field      string          `json:"f"`
	Before     json.RawMessage `json:"b,omitempty"`
	After      json.RawMessage `json:"a,omitempty"`
	BeforeHash string          `json:"bh,omitempty"`
	AfterHash  string          `json:"ah,omitempty"`
}

// bounded is a value as a history row records it: its bytes, or its hash past
// the bound.
func bounded(v canon.Value) (json.RawMessage, string) {
	raw := canon.Encode(v)
	if int64(len(raw)) <= artifacts.MaxHistoryValueBytes {
		return raw, ""
	}
	sum := sha256.Sum256(raw)
	return nil, hex.EncodeToString(sum[:])
}

// changeOf is what d's record did to it, and whether every value it needs to
// be undone is held whole.
func changeOf(d *docState, after int64, op ChangeOp) (Change, bool) {
	ch := Change{Collection: d.coll.spec.Name, Doc: d.id, Op: op, Before: d.version0, After: after}
	undoable := true
	for _, name := range unionKeys(d.fields0, d.fields) {
		old, had := d.fields0[name]
		val, has := d.fields[name]
		if had && has && sameValue(old, val) {
			continue
		}
		fc := FieldChange{Field: name}
		if had {
			fc.Before, fc.BeforeHash = bounded(old)
			if fc.BeforeHash != "" {
				undoable = false
			}
		}
		if has {
			fc.After, fc.AfterHash = bounded(val)
		}
		ch.Fields = append(ch.Fields, fc)
	}
	return ch, undoable
}

// opOf is the change a document's record made.
func opOf(d *docState) ChangeOp {
	switch {
	case d.live && !d.live0:
		return ChangeCreate
	case !d.live && d.live0:
		return ChangeDelete
	}
	return ChangeUpdate
}

// historyRow is one row of the hot history, before it is written.
type historyRow struct {
	op       OpKind
	verdict  artifacts.DataVerdict
	reason   string
	clientID string
	changes  []Change
	touches  []docTouch
	undoable bool
	undoes   int64
	digest   canon.Digest
}

// docTouch is one document a history row changed.
type docTouch struct {
	collection int64
	doc        string
}

// writeHistory writes one history row and its touches, and answers the bytes
// it counts against the store's history bound.
func writeHistory(ctx context.Context, tx *sql.Tx, c applyContext, s storeRow, h historyRow) (int64, int, error) {
	changes, err := json.Marshal(h.changes)
	if err != nil {
		return 0, 0, fmt.Errorf("artifactdata: encode the history of %s: %w", c.position, err)
	}
	if h.changes == nil {
		changes = []byte("[]")
	}
	bytes := int64(len(changes)+len(h.reason)) + historyRowOverhead + touchBytes*int64(len(h.touches))
	var elevated, undoable int
	if c.authority.Elevated {
		elevated = 1
	}
	if h.undoable {
		undoable = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_data_writes
			(store_no, seq, op_id, op, actor, actor_kind, operator_id, origin, work_item,
			 elevated, client_id, at, verdict, reason, changes, bytes, digest_after,
			 undoable, undoes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.no, c.packed, c.record.OpID, string(h.op), c.record.Actor, string(c.record.ActorKind),
		c.record.OperatorID, string(c.authority.Origin), c.authority.WorkItem, elevated,
		h.clientID, c.at(), string(h.verdict), h.reason, string(changes), bytes,
		h.digest.Bytes(), undoable, h.undoes); err != nil {
		return 0, 0, fmt.Errorf("artifactdata: write the history of %s: %w", c.position, err)
	}
	rows := 1
	n, err := store.InsertRows(ctx, tx, c.maxVariables, `
		INSERT INTO artifact_data_touches (store_no, collection_no, doc_id, seq) VALUES `,
		`(?, ?, ?, ?)`, `ON CONFLICT (store_no, collection_no, doc_id, seq) DO NOTHING`,
		len(h.touches), func(i int) []any {
			return []any{s.no, h.touches[i].collection, h.touches[i].doc, c.packed}
		})
	if err != nil {
		return 0, 0, fmt.Errorf("artifactdata: write the touches of %s: %w", c.position, err)
	}
	return bytes, rows + n, nil
}

// pruneBatch is how many history rows, and how many tombstones, one record
// retires at most: a bounded batch, so a store returning to life after weeks
// does not delete a week of history in the transaction holding the estate's
// only writer, while a store written once a minute still retires rows faster
// than it writes them.
const pruneBatch = 256

// pruneHistory retires a store's history rows past the hot window or the byte
// bound, oldest first, never the record's own row, then the tombstones the
// advanced floor has passed. It moves s's counters and floors and answers the
// rows it deleted.
func pruneHistory(ctx context.Context, tx *sql.Tx, c applyContext, s *storeRow, limits artifacts.Limits) (int, error) {
	if c.brokerAt.IsZero() {
		return 0, nil
	}
	cutoff := store.EncodeTime(c.brokerAt.Add(-artifacts.DataHistoryHotWindow))
	budget := historyCap(limits)
	rows, err := tx.QueryContext(ctx, `
		SELECT seq, at, bytes FROM artifact_data_writes
		WHERE store_no = ? AND seq < ? ORDER BY at, seq LIMIT ?`, s.no, c.packed, pruneBatch)
	if err != nil {
		return 0, fmt.Errorf("artifactdata: read store %s's history: %w", s.artifact, err)
	}
	type old struct{ seq, at, bytes int64 }
	var oldest []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.seq, &o.at, &o.bytes); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("artifactdata: read store %s's history: %w", s.artifact, err)
		}
		oldest = append(oldest, o)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("artifactdata: read store %s's history: %w", s.artifact, err)
	}
	deleted := 0
	for _, o := range oldest {
		if o.at >= cutoff && s.historyBytes <= budget {
			break
		}
		for _, stmt := range []string{
			`DELETE FROM artifact_data_writes WHERE store_no = ? AND seq = ?`,
			`DELETE FROM artifact_data_touches WHERE store_no = ? AND seq = ?`,
		} {
			res, err := tx.ExecContext(ctx, stmt, s.no, o.seq)
			if err != nil {
				return 0, fmt.Errorf("artifactdata: prune store %s's history: %w", s.artifact, err)
			}
			n, _ := res.RowsAffected()
			deleted += int(n)
		}
		s.historyBytes -= o.bytes
		s.historyFloor = max(s.historyFloor, o.seq+1)
	}
	if s.historyFloor == 0 {
		return deleted, nil
	}
	// THE TOMBSTONES THE FLOOR HAS PASSED: a client catching up from below
	// the floor is told to read the store whole, so a delete older than the
	// floor no longer needs its row.
	tomb, err := tx.QueryContext(ctx, `
		SELECT collection_no, doc_id, version FROM artifact_data_docs
		WHERE store_no = ? AND body IS NULL AND version < ?
		ORDER BY version, collection_no, doc_id LIMIT ?`, s.no, s.historyFloor, pruneBatch)
	if err != nil {
		return 0, fmt.Errorf("artifactdata: read store %s's tombstones: %w", s.artifact, err)
	}
	type gone struct {
		coll    int64
		doc     string
		version int64
	}
	var stones []gone
	for tomb.Next() {
		var g gone
		if err := tomb.Scan(&g.coll, &g.doc, &g.version); err != nil {
			_ = tomb.Close()
			return 0, fmt.Errorf("artifactdata: read store %s's tombstones: %w", s.artifact, err)
		}
		stones = append(stones, g)
	}
	if err := errors.Join(tomb.Err(), tomb.Close()); err != nil {
		return 0, fmt.Errorf("artifactdata: read store %s's tombstones: %w", s.artifact, err)
	}
	for _, g := range stones {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM artifact_data_docs WHERE store_no = ? AND collection_no = ? AND doc_id = ?`,
			s.no, g.coll, g.doc); err != nil {
			return 0, fmt.Errorf("artifactdata: prune a tombstone of store %s: %w", s.artifact, err)
		}
		s.tombstones--
		s.tombstoneFloor = max(s.tombstoneFloor, g.version)
		deleted++
	}
	return deleted, nil
}
