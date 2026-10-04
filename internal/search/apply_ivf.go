package search

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE INDEX'S HALF OF THE VECTOR APPLIER: installing the centroids a record
// carries, filing a written vector in them, re-filing a batch of rows when a
// reassign record says so, recording a later measurement, and keeping the
// per-list counts every one of those moves. Every write here is one the
// applier makes from a committed record, in the framework's transaction — the
// index is derived state on every node for exactly the reason the vectors are.

// IndexHead is the semantic index's row, without the centroids: what is
// installed, in which embedding space, and what its latest measurement found.
type IndexHead struct {
	// Generation is the packed position of the record that installed it.
	Generation int64
	// Log is the vector log it was trained for.
	Log   string
	Model string
	Dim   int
	// Lists is zero for a verdict that the corpus has no index.
	Lists, Probes int
	TrainedOn     int
	// Largest is the most rows the training filed in one list.
	Largest int
	Why     IndexVerdict
	Digest  string
	// TrainedAt is the training's own clock, from its record.
	TrainedAt time.Time

	// Measurement is the latest measurement — the training's, or a later
	// measure record's — and nil when nothing was measured. MeasuredAt is
	// that record's clock.
	Measurement *Measurement
	MeasuredAt  time.Time

	// measuredBy is the packed position of the record that set the
	// measurement, which an older one never replaces.
	measuredBy int64
}

// InSpace reports whether the head is about the given embedding space — an
// index there, or a verdict about it.
func (h IndexHead) InSpace(model string, dim int) bool {
	return h.Model == model && h.Dim == dim
}

// live reports an installed index in the given embedding space.
func (h IndexHead) live(model string, dim int) bool {
	return h.Lists > 0 && h.InSpace(model, dim)
}

// ReadIndex is the semantic index's row, or false when there has never been one
// — what an alarm reading and an evaluation report, read through the same
// statement the probe and the applier read it with.
func ReadIndex(ctx context.Context, tx *sql.Tx) (IndexHead, bool, error) {
	return readIndexHead(ctx, tx)
}

// readIndexHead is the installed index's row, or false when there has never
// been one.
func readIndexHead(ctx context.Context, tx *sql.Tx) (IndexHead, bool, error) {
	var h IndexHead
	var why string
	var trainedAt int64
	var sources sql.NullInt64
	var recall, floor sql.NullFloat64
	var shape, shapeSource sql.NullString
	var measuredAt sql.NullInt64
	var misses int
	err := tx.QueryRowContext(ctx, indexHeadStatement).Scan(&h.Generation, &h.Log,
		&h.Model, &h.Dim, &h.Lists, &h.Probes, &h.TrainedOn, &h.Largest, &why,
		&h.Digest, &trainedAt, &sources, &recall, &floor, &shape, &shapeSource,
		&misses, &measuredAt, &h.measuredBy)
	if errors.Is(err, sql.ErrNoRows) {
		return IndexHead{}, false, nil
	}
	if err != nil {
		return IndexHead{}, false, fmt.Errorf("search: read the index: %w", err)
	}
	h.Why = IndexVerdict(why)
	h.TrainedAt = store.DecodeTime(trainedAt)
	if recall.Valid {
		h.Measurement = &Measurement{
			Sources: int(sources.Int64), Recall: recall.Float64,
			Floor: floor.Float64, Shape: QueryShape(shape.String),
			ShapeSource: Source(shapeSource.String), HeadMisses: misses,
		}
		h.MeasuredAt = store.DecodeTime(measuredAt.Int64)
	}
	return h, true, nil
}

// indexHeadStatement reads the head — every column but none of the centroids,
// which are a table of their own for exactly this read (migration 0033).
const indexHeadStatement = `
	SELECT generation, log, model, dim, lists, probes, trained_on, largest, why,
	       digest, trained_at, measured_sources, recall, floor, shape,
	       shape_source, head_misses, measured_at, measure_position
	FROM kb_ivf WHERE id = 1`

// ivfMemo holds the decoded index, checked against the digest of the bytes it
// was decoded from.
//
// # Why a memo, and why its check is the CONTENT
//
// A written vector is filed by reading the centroids — up to three quarters of
// a megabyte — and a node catching up applies thousands of vectors a second,
// so decoding the blob per record is a megabyte of reads per vector for a value
// that changes once per retraining. A search likewise ranks every list of the
// index before it reads any.
//
// ONE SLOT, because a node holds one index — the estate's, over its one vector
// log — and a retrained index REPLACES the one before it rather than lingering
// beside it, so the memo holds nothing a retrain left behind.
//
// A hit needs the DIGEST to match, never the generation: a generation is a
// position on the log, and two stores in one process — a test's, or a snapshot
// being adopted beside the live file — can hold different centroids at one
// position. A digest names the bytes, so a hit is the same centroids wherever
// they were read from.
type ivfMemo struct {
	mu   sync.Mutex
	held *memoEntry
}

type memoEntry struct {
	digest string
	index  IVF
}

// load is the installed index's centroids, decoded.
func (m *ivfMemo) load(ctx context.Context, tx *sql.Tx, h IndexHead) (IVF, error) {
	if m != nil {
		m.mu.Lock()
		held := m.held
		m.mu.Unlock()
		if held != nil && held.digest == h.Digest {
			return held.index, nil
		}
	}
	var digest string
	var blob []byte
	err := tx.QueryRowContext(ctx,
		`SELECT digest, centroids FROM kb_ivf_centroids WHERE id = 1`).Scan(&digest, &blob)
	if err != nil {
		return IVF{}, fmt.Errorf("search: read the index's centroids: %w", err)
	}
	if digest != h.Digest || digestOf(blob) != h.Digest {
		// THE HEAD, THE BLOB AND ITS DIGEST ARE WRITTEN IN ONE
		// TRANSACTION by the one applier, so a mismatch is a row something
		// else wrote.
		return IVF{}, fmt.Errorf("search: the index's centroids do not match " +
			"the digest its head names — kb_ivf was written by something other " +
			"than the vector applier")
	}
	index, err := DecodeIVF(h.Dim, h.Lists, blob)
	if err != nil {
		return IVF{}, err
	}
	if m != nil {
		m.mu.Lock()
		m.held = &memoEntry{digest: h.Digest, index: index}
		m.mu.Unlock()
	}
	return index, nil
}

// probeMemo is the query path's own decoded copies, shared by every search in
// the process for [ivfMemo]'s reason.
var probeMemo = &ivfMemo{}

// digestOf is the stored digest of a centroids blob.
func digestOf(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// fileUnder is where a vector written now is filed: the installed index's
// generation and the vector's nearest list, or (0, 0) when the corpus has no
// index in its embedding space.
func (a Applier) fileUnder(ctx context.Context, tx *sql.Tx, head IndexHead, model string, dim int, embedding []byte) (int64, int, error) {
	if !head.live(model, dim) {
		return 0, 0, nil
	}
	index, err := a.index.load(ctx, tx, head)
	if err != nil {
		return 0, 0, err
	}
	// THE CODE FROM THE RECORD'S OWN BYTES, which is the code the database
	// stores beside it: [Quantize] agrees with `vector1bit` bit for bit
	// (TestTheStoredCodeIsTheQuantizedCode), so filing from it needs no read
	// back of the row this apply is about to write.
	return head.Generation, index.Nearest(Quantize(unpack(embedding))), nil
}

// unpack reads packed little-endian float32.
func unpack(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

// filing is where one row of the narrow table is filed, as it was read.
type filing struct {
	generation int64
	list       int
}

// filedAt reads where a source's narrow row is filed, and false when it has
// no row.
func filedAt(ctx context.Context, tx *sql.Tx, source Source, id string) (filing, bool, error) {
	var f filing
	err := tx.QueryRowContext(ctx, `
		SELECT ivf_gen, ivf_list FROM kb_vectors_bin
		WHERE source = ? AND source_id = ?`, string(source), id).Scan(&f.generation, &f.list)
	if errors.Is(err, sql.ErrNoRows) {
		return filing{}, false, nil
	}
	if err != nil {
		return filing{}, false, fmt.Errorf("search: read where %s %s is filed: %w",
			source, id, err)
	}
	return f, true, nil
}

// countFiled moves the per-list counts by delta rows of one source.
//
// kb_ivf_lists holds EXACTLY the rows filed under the installed generation, and
// the only writers of that set are the applier's own statements — so every one
// of them calls this in the same transaction, and a count that would fall
// below zero is a table something else wrote, refused rather than clamped. A
// count that reaches zero is deleted, so every holder holds the same rows
// whatever order it reached the same state in.
func countFiled(ctx context.Context, tx *sql.Tx, list int, source Source, delta int) error {
	if delta == 0 {
		return nil
	}
	if delta > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO kb_ivf_lists (list, source, filed) VALUES (?, ?, ?)
			ON CONFLICT (list, source) DO UPDATE SET filed = filed + excluded.filed`,
			list, string(source), delta); err != nil {
			return fmt.Errorf("search: count %d row(s) into list %d: %w", delta, list, err)
		}
		return nil
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE kb_ivf_lists SET filed = filed + ?
		WHERE list = ? AND source = ? AND filed >= ?`, delta, list, string(source), -delta)
	if err != nil {
		return fmt.Errorf("search: count %d row(s) out of list %d: %w", -delta, list, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("search: list %d holds fewer than %d filed %s row(s) — "+
			"kb_ivf_lists disagrees with kb_vectors_bin, which only the vector "+
			"applier writes", list, -delta, source)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kb_ivf_lists WHERE list = ? AND source = ? AND filed = 0`,
		list, string(source)); err != nil {
		return fmt.Errorf("search: drop list %d's empty count: %w", list, err)
	}
	return nil
}

// ListCounts is how many rows of each source every list of the installed
// index holds, by list.
type ListCounts map[int]map[Source]int

// Total is how many rows list holds, of every source.
func (c ListCounts) Total(list int) int {
	n := 0
	for _, filed := range c[list] {
		n += filed
	}
	return n
}

// Of is how many rows of the given sources list holds — every source when
// none is named.
func (c ListCounts) Of(list int, sources []Source) int {
	if len(sources) == 0 {
		return c.Total(list)
	}
	n := 0
	for _, s := range sources {
		n += c[list][s]
	}
	return n
}

// readListCounts is kb_ivf_lists, whole: at most a few thousand rows.
func readListCounts(ctx context.Context, tx *sql.Tx) (ListCounts, error) {
	rows, err := tx.QueryContext(ctx, `SELECT list, source, filed FROM kb_ivf_lists`)
	if err != nil {
		return nil, fmt.Errorf("search: read the index's list counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := ListCounts{}
	for rows.Next() {
		var list, filed int
		var source string
		if err := rows.Scan(&list, &source, &filed); err != nil {
			return nil, err
		}
		if out[list] == nil {
			out[list] = map[Source]int{}
		}
		out[list][Source(source)] = filed
	}
	return out, rows.Err()
}

// install writes the index a centroids record carries — or its verdict that
// there is none.
//
// GUARDED BY THE POSITION, as a vector is: a centroids record at or below the
// installed generation is a redelivery or a record the compaction has already
// superseded, and installing it would put an older index over a newer one.
//
// IT TOUCHES NO ROW. Every row stays filed under the index it was filed under
// — stale now — until a reassign record for this generation re-files its
// batch; a search reads the stale rows in full meanwhile ([Stage1]). Filing
// them here would hold this store's writer for the whole corpus. What it
// does empty is the per-list counts, because they count the rows filed under
// the INSTALLED generation, and there are none yet.
func (a Applier) install(ctx context.Context, tx *sql.Tx, vec VectorRecord, at statelog.Position) (int, error) {
	head, ok, err := readIndexHead(ctx, tx)
	if err != nil {
		return 0, err
	}
	generation := at.Packed()
	if ok && generation <= head.Generation {
		return 0, nil
	}
	x := vec.Index
	var sources, measuredAt any
	var recall, floor any
	var shape, shapeSource any
	misses := 0
	if m := x.Measurement; m != nil {
		sources, recall, floor = m.Sources, m.Recall, m.Floor
		shape, shapeSource, misses = string(m.Shape), string(m.ShapeSource), m.HeadMisses
		measuredAt = store.EncodeTime(vec.CreatedAt)
	}
	digest := digestOf(x.Centroids)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO kb_ivf
			(id, generation, log, basis, seed, model, dim, lists, probes,
			 trained_on, largest, why, digest, trained_at, measured_sources,
			 recall, floor, shape, shape_source, head_misses, measured_at,
			 measure_position)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			generation       = excluded.generation,
			log              = excluded.log,
			basis            = excluded.basis,
			seed             = excluded.seed,
			model            = excluded.model,
			dim              = excluded.dim,
			lists            = excluded.lists,
			probes           = excluded.probes,
			trained_on       = excluded.trained_on,
			largest          = excluded.largest,
			why              = excluded.why,
			digest           = excluded.digest,
			trained_at       = excluded.trained_at,
			measured_sources = excluded.measured_sources,
			recall           = excluded.recall,
			floor            = excluded.floor,
			shape            = excluded.shape,
			shape_source     = excluded.shape_source,
			head_misses      = excluded.head_misses,
			measured_at      = excluded.measured_at,
			measure_position = excluded.measure_position`,
		generation, x.Log, x.Basis, int64(x.Seed), vec.Model, vec.Dim, x.Lists,
		x.Probes, x.TrainedOn, x.Largest, string(x.Why), digest,
		store.EncodeTime(vec.CreatedAt), sources, recall, floor, shape,
		shapeSource, misses, measuredAt, generation); err != nil {
		return 0, fmt.Errorf("search: install the index at %s: %w", at, err)
	}
	for _, clear := range []string{
		`DELETE FROM kb_ivf_centroids`, `DELETE FROM kb_ivf_rollout`,
		`DELETE FROM kb_ivf_lists`,
	} {
		if _, err := tx.ExecContext(ctx, clear); err != nil {
			return 0, fmt.Errorf("search: retire the previous index at %s: %w", at, err)
		}
	}
	if x.Lists == 0 {
		return 1, nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO kb_ivf_centroids (id, digest, centroids) VALUES (1, ?, ?)`,
		digest, x.Centroids); err != nil {
		return 0, fmt.Errorf("search: store the index's centroids at %s: %w", at, err)
	}
	for n, r := range x.Rollout {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO kb_ivf_rollout (batch, source, from_id, to_id)
			VALUES (?, ?, ?, ?)`, n, string(r.Source), r.From, r.To); err != nil {
			return 0, fmt.Errorf("search: store the index's rollout at %s: %w", at, err)
		}
	}
	return 3 + len(x.Rollout), nil
}

// measure records a later measurement of the installed index, and the probe
// count it chose.
//
// ONLY OF THE INDEX IT NAMES, and only above the measurement it replaces: a
// measurement of a replaced index describes lists nobody reads, and an older
// one — a redelivery — would put back a probe count the corpus has moved past.
func (a Applier) measure(ctx context.Context, tx *sql.Tx, vec VectorRecord, at statelog.Position) (int, error) {
	rec := *vec.Measure
	head, ok, err := readIndexHead(ctx, tx)
	if err != nil || !ok || head.Lists == 0 || head.Generation != rec.Index ||
		at.Packed() <= head.measuredBy {
		return 0, err
	}
	m := rec.Measurement
	if _, err := tx.ExecContext(ctx, `
		UPDATE kb_ivf SET probes = ?, measured_sources = ?, recall = ?, floor = ?,
			shape = ?, shape_source = ?, head_misses = ?, measured_at = ?,
			measure_position = ?
		WHERE id = 1`, min(rec.Probes, head.Lists), m.Sources, m.Recall, m.Floor,
		string(m.Shape), string(m.ShapeSource), m.HeadMisses,
		store.EncodeTime(vec.CreatedAt), at.Packed()); err != nil {
		return 0, fmt.Errorf("search: record the index's measurement at %s: %w", at, err)
	}
	return 1, nil
}

// reassign re-files one batch of the installed index's rollout.
//
// A BATCH FOR ANY OTHER GENERATION APPLIES NOTHING: its index has been
// replaced, and the rollout of the one that replaced it covers the same rows.
// Every holder applies the same batch at the same position, reads its range
// from the same rollout table, and a row's list is a function of its own code
// and the centroids alone — so every holder re-files the same rows into the
// same lists.
func (a Applier) reassign(ctx context.Context, tx *sql.Tx, batch ReassignRecord) (int, error) {
	head, ok, err := readIndexHead(ctx, tx)
	if err != nil || !ok || head.Lists == 0 || head.Generation != batch.Index {
		return 0, err
	}
	var r RolloutRange
	var source string
	err = tx.QueryRowContext(ctx, `
		SELECT source, from_id, to_id FROM kb_ivf_rollout WHERE batch = ?`,
		batch.Batch).Scan(&source, &r.From, &r.To)
	if errors.Is(err, sql.ErrNoRows) {
		// A BATCH THE INDEX'S OWN ROLLOUT DOES NOT HAVE is a writer fault:
		// the one writer publishes batches of the rollout it installed.
		return 0, fmt.Errorf("search: reassign batch %d of index %d, whose "+
			"rollout has no such batch", batch.Batch, batch.Index)
	}
	if err != nil {
		return 0, fmt.Errorf("search: read reassign batch %d's range: %w", batch.Batch, err)
	}
	r.Source = Source(source)
	index, err := a.index.load(ctx, tx, head)
	if err != nil {
		return 0, err
	}
	query, args := reassignStatement(r, head)

	type move struct {
		row  int64
		list int
	}
	var moves []move
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("search: read the range a reassign re-files: %w", err)
	}
	for rows.Next() {
		var row int64
		var bits []byte
		if err := rows.Scan(&row, &bits); err != nil {
			_ = rows.Close()
			return 0, err
		}
		code, err := codeFromBits(bits, head.Dim)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		moves = append(moves, move{row: row, list: index.Nearest(code)})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()
	counted := map[int]int{}
	for _, m := range moves {
		if _, err := tx.ExecContext(ctx,
			`UPDATE kb_vectors_bin SET ivf_gen = ?, ivf_list = ? WHERE rowid = ?`,
			head.Generation, m.list, m.row); err != nil {
			return 0, fmt.Errorf("search: re-file a row under the index: %w", err)
		}
		counted[m.list]++
	}
	// EVERY ROW IT MOVED WAS STALE — the read above selects only rows not
	// filed under this generation — so each one is counted in, once.
	for list, n := range counted {
		if err := countFiled(ctx, tx, list, r.Source, n); err != nil {
			return 0, err
		}
	}
	return len(moves), nil
}

// reassignStatement is the read a reassign batch re-files, and its arguments.
//
// THE PRIMARY KEY'S SEEK on (source, source_id), then the space and the
// generation as residuals: a row of another embedding space is never filed in
// this index, and a row already filed under it — written after the index was
// installed — is left exactly as it is. A function of its own so the plan
// gate explains the statement the applier runs.
func reassignStatement(r RolloutRange, head IndexHead) (string, []any) {
	query := `
		SELECT rowid, bits FROM kb_vectors_bin
		WHERE source = ? AND source_id >= ?`
	args := []any{string(r.Source), r.From}
	if r.To != "" {
		query += ` AND source_id < ?`
		args = append(args, r.To)
	}
	query += ` AND model = ? AND dim = ? AND ivf_gen <> ?`
	return query, append(args, head.Model, head.Dim, head.Generation)
}
