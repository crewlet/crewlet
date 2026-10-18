package artifactdata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifacts"
)

// THE ROWS AN APPLY READS: a store's catalogue row and the view an evaluation
// decides over, both inside the applier's own transaction. The writer reads
// the same rows for its early refusals, through the same functions, so the two
// halves never read a store two ways.

// storeRow is one store's catalogue row.
type storeRow struct {
	artifact                     string
	no                           int64
	docs, tombstones             int64
	bodyBytes, fieldEntries      int64
	historyBytes, footprint      int64
	writes                       int64
	digest                       canon.Digest
	historyFloor, tombstoneFloor int64
	nextCollection, nextPath     int64
	createdAt                    int64
	version                      int64
}

// computeFootprint is the store's footprint from its counters.
func (s storeRow) computeFootprint() int64 {
	return s.bodyBytes + fieldEntryBytes*s.fieldEntries + s.historyBytes
}

// readStore reads one store's catalogue row, false where there is none.
func readStore(ctx context.Context, tx *sql.Tx, artifact string) (storeRow, bool, error) {
	s := storeRow{artifact: artifact}
	var digest []byte
	err := tx.QueryRowContext(ctx, `
		SELECT store_no, docs, tombstones, body_bytes, field_entries, history_bytes,
		       footprint, writes, state_digest, history_floor, tombstones_floor,
		       next_collection_no, next_path_no, created_at, version
		FROM artifact_data_stores WHERE artifact_id = ?`, artifact).Scan(
		&s.no, &s.docs, &s.tombstones, &s.bodyBytes, &s.fieldEntries, &s.historyBytes,
		&s.footprint, &s.writes, &digest, &s.historyFloor, &s.tombstoneFloor,
		&s.nextCollection, &s.nextPath, &s.createdAt, &s.version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return storeRow{artifact: artifact}, false, nil
	case err != nil:
		return storeRow{}, false, fmt.Errorf("artifactdata: read store %s: %w", artifact, err)
	}
	if s.digest, err = canon.DigestFrom(digest); err != nil {
		return storeRow{}, false, fmt.Errorf("artifactdata: store %s: %w", artifact, err)
	}
	return s, true, nil
}

// companyFootprint is every store's footprint together, zero before the first.
func companyFootprint(ctx context.Context, tx *sql.Tx) (int64, error) {
	var fp int64
	err := tx.QueryRowContext(ctx,
		`SELECT footprint FROM artifact_data_usage WHERE scope = ?`, usageScope).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("artifactdata: read the company's datastore usage: %w", err)
	}
	return fp, nil
}

// usageScope is the one row the company's counters live in.
const usageScope = "company"

// readCollections reads every collection a store holds, in collection order.
func readCollections(ctx context.Context, tx *sql.Tx, storeNo int64) ([]*collState, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT collection_no, spec, implicit, docs FROM artifact_data_collections
		WHERE store_no = ? ORDER BY collection_no`, storeNo)
	if err != nil {
		return nil, fmt.Errorf("artifactdata: read store %d's collections: %w", storeNo, err)
	}
	defer func() { _ = rows.Close() }()
	var out []*collState
	for rows.Next() {
		var (
			no, docs int64
			spec     string
			implicit int
		)
		if err := rows.Scan(&no, &spec, &implicit, &docs); err != nil {
			return nil, fmt.Errorf("artifactdata: read store %d's collections: %w", storeNo, err)
		}
		c, err := decodeSpec(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, newCollState(c, no, implicit != 0, true, docs))
	}
	return out, rows.Err()
}

// txView is an evaluation's view of one store through a transaction.
type txView struct {
	ctx     context.Context
	tx      *sql.Tx
	store   storeRow
	present bool
	company int64
	colls   int64
}

// newTxView builds the view over one store as the transaction finds it.
func newTxView(ctx context.Context, tx *sql.Tx, store storeRow, present bool) (*txView, error) {
	v := &txView{ctx: ctx, tx: tx, store: store, present: present}
	company, err := companyFootprint(ctx, tx)
	if err != nil {
		return nil, err
	}
	v.company = company
	if present {
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM artifact_data_collections WHERE store_no = ?`,
			store.no).Scan(&v.colls); err != nil {
			return nil, fmt.Errorf("artifactdata: count store %s's collections: %w", store.artifact, err)
		}
	}
	return v, nil
}

func (v *txView) totals() storeTotals {
	return storeTotals{
		docs: v.store.docs, bodyBytes: v.store.bodyBytes, fieldEntries: v.store.fieldEntries,
		historyBytes: v.store.historyBytes, collections: v.colls, companyFootprint: v.company,
	}
}

func (v *txView) collection(name string) (*collState, error) {
	if v.present {
		var (
			no, docs int64
			spec     string
			implicit int
		)
		err := v.tx.QueryRowContext(v.ctx, `
			SELECT collection_no, spec, implicit, docs FROM artifact_data_collections
			WHERE store_no = ? AND name = ?`, v.store.no, name).Scan(&no, &spec, &implicit, &docs)
		switch {
		case err == nil:
			c, err := decodeSpec(spec)
			if err != nil {
				return nil, err
			}
			return newCollState(c, no, implicit != 0, true, docs), nil
		case !errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("artifactdata: read collection %s: %w", name, err)
		}
	}
	if name == StateCollection {
		return newCollState(stateSpec(), 0, true, false, 0), nil
	}
	return nil, nil
}

func (v *txView) doc(c *collState, id string) (*docState, error) {
	d := &docState{coll: c, id: id}
	if !c.stored {
		return d, nil
	}
	var body sql.NullString
	err := v.tx.QueryRowContext(v.ctx, `
		SELECT body, created_by, created_by_kind, created_at, version
		FROM artifact_data_docs WHERE store_no = ? AND collection_no = ? AND doc_id = ?`,
		v.store.no, c.no, id).Scan(&body, &d.createdBy, &d.createdByKind, &d.createdAt, &d.version0)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return d, nil
	case err != nil:
		return nil, fmt.Errorf("artifactdata: read document %s/%s: %w", c.spec.Name, id, err)
	}
	d.row = true
	if !body.Valid {
		return d, nil
	}
	parsed, err := canon.Parse([]byte(body.String), canon.MaxDepth)
	if err != nil || parsed.Kind != canon.KindObject {
		return nil, fmt.Errorf("artifactdata: document %s/%s holds a body this encoder did not "+
			"write: %v", c.spec.Name, id, err)
	}
	d.live0, d.live = true, true
	d.body0 = []byte(body.String)
	d.fields0 = make(map[string]canon.Value, len(parsed.Members))
	d.fields = make(map[string]canon.Value, len(parsed.Members))
	for _, m := range parsed.Members {
		d.fields0[m.Key] = m.Value
		d.fields[m.Key] = m.Value
	}
	if err := v.tx.QueryRowContext(v.ctx, `
		SELECT COUNT(*) FROM artifact_data_fields
		WHERE store_no = ? AND collection_no = ? AND doc_id = ?`,
		v.store.no, c.no, id).Scan(&d.entries0); err != nil {
		return nil, fmt.Errorf("artifactdata: count document %s/%s's index entries: %w",
			c.spec.Name, id, err)
	}
	return d, nil
}

func (v *txView) ownedLive(c *collState, actor string) (int64, error) {
	if !c.stored {
		return 0, nil
	}
	var n int64
	if err := v.tx.QueryRowContext(v.ctx, `
		SELECT COUNT(*) FROM artifact_data_docs
		WHERE store_no = ? AND collection_no = ? AND created_by = ? AND body IS NOT NULL`,
		v.store.no, c.no, actor).Scan(&n); err != nil {
		return 0, fmt.Errorf("artifactdata: count %s's rows in %s: %w", actor, c.spec.Name, err)
	}
	return n, nil
}

// historyCap is the hot history a store keeps under limits: at most
// [artifacts.MaxHistoryBytesPerStore], and never more than a quarter of the
// store's own stamped footprint bound, so history never crowds out the data
// an operator sized the store for.
func historyCap(limits artifacts.Limits) int64 {
	if limits.DataStoreBytes <= 0 {
		return artifacts.MaxHistoryBytesPerStore
	}
	return min(artifacts.MaxHistoryBytesPerStore, limits.DataStoreBytes/4)
}
