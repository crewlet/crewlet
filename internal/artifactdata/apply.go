package artifactdata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier, and the rules that make it a PURE FUNCTION of the log.
//
//  1. IT READS NO CONFIG, NO CLOCK AND NO OTHER DOMAIN. The caller's role on
//     the artifact, whether the caller is an agent and the limits in force were
//     decided at the API and STAMPED into the record; every instant written is
//     the broker's ([statelog.ApplyOptions.StoredAt]).
//  2. IT DECIDES EVERY WRITE, AND A REFUSAL IS A ROW. Schema, field rules,
//     guards, caps and the stamped limits are judged here, in log order, so
//     every data node reaches one verdict; a write refused is applied as its history
//     row with the verdict and the reason, never as an error, which would stall
//     the log on every data node.
//  3. EVERY RECORD IS APPLIED ONCE. Its history row is keyed by its position,
//     and an apply that finds its own row already there writes nothing.

// Applier writes this node's copy of the datastore estate.
type Applier struct {
	// NodeID is this node's own id.
	NodeID string

	committed func([]Moved)

	// moved is filled inside Apply and drained by Committed, on one
	// goroutine with the commit in between.
	moved []Moved
}

var _ statelog.RetentionHook = (*Applier)(nil)

// NewApplier builds the datastore applier for one node. committed is called
// after a committed batch that moved anything, after the commit and never
// inside the transaction.
func NewApplier(nodeID string, committed func([]Moved)) *Applier {
	return &Applier{NodeID: nodeID, committed: committed}
}

// Committed is the post-commit half: what the batch moved, told once.
func (a *Applier) Committed(context.Context) {
	if len(a.moved) == 0 {
		return
	}
	moved := a.moved
	a.moved = nil
	if a.committed != nil {
		a.committed(moved)
	}
}

// Retained tells the listeners that a batch retained records this build
// cannot read, so whatever they decided may have moved.
func (a *Applier) Retained(context.Context, int) {
	a.moved = append(a.moved, ResetMove())
}

// Gated reports a record that must produce no rows: the EVICTION GATE and the
// DELETION GATE (a record about a store a purge destroyed, except the purge's
// own record).
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (statelog.Reason, bool, error) {
	if rec.Writer != "" {
		evicted, err := evictedAt(ctx, tx, rec.Writer, rec.Position.Packed())
		if err != nil {
			return "", false, err
		}
		if evicted {
			return statelog.ReasonEvicted, true, nil
		}
	}
	artifact, ok := gatedStore(rec.Subject.Kind, rec.Subject.ID)
	if !ok {
		return "", false, nil
	}
	author, marked, err := deletionMarker(ctx, tx, artifact)
	if err != nil || !marked {
		return "", false, err
	}
	if author == rec.OpID {
		return "", false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifactdata_deletions SET rejects = rejects + 1, last_reject_at = ?
		WHERE artifact_id = ?`, store.EncodeTime(rec.StoredAt), artifact); err != nil {
		return "", false, fmt.Errorf("artifactdata: count a gate hit on the purged store %s: %w",
			artifact, err)
	}
	return statelog.ReasonDeleted, true, nil
}

// evictedAt reports whether a record at packed, written by writer, falls in
// writer's eviction window (half-open at both ends).
func evictedAt(ctx context.Context, tx *sql.Tx, writer string, packed int64) (bool, error) {
	var from, readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM artifactdata_evictions
		WHERE node_id = ?`, writer).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("artifactdata: read the eviction gate for node %s: %w", writer, err)
	}
	evicted := from.Valid && packed > from.Int64
	back := readmitted.Valid && packed >= readmitted.Int64
	return evicted && !back, nil
}

// deletionMarker is a store's deletion marker: the purge record's id, and
// whether there is one.
func deletionMarker(ctx context.Context, tx *sql.Tx, artifact string) (string, bool, error) {
	var author string
	err := tx.QueryRowContext(ctx,
		`SELECT purge_record_id FROM artifactdata_deletions WHERE artifact_id = ?`,
		artifact).Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("artifactdata: read the deletion gate for store %s: %w",
			artifact, err)
	}
	return author, true, nil
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record       MutationRecord
	authority    artifacts.Authority
	position     statelog.Position
	packed       int64
	brokerAt     time.Time
	maxVariables int
}

// at is the broker instant as it is stored.
func (c applyContext) at() int64 { return store.EncodeTime(c.brokerAt) }

// Apply writes one record's rows.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("artifactdata: decode the record at %s: %w", rec.Position, err)
	}
	c := applyContext{
		record: record, position: rec.Position, packed: rec.Position.Packed(),
		brokerAt: opts.StoredAt, maxVariables: opts.MaxVariables,
	}
	switch record.Subject.Kind {
	case KindBarrier:
		return 0, nil
	case KindTrimmed:
		// THE TRIM'S OWN RECORD. The framework removes this log's anchors
		// below the floor its subject names, in this same transaction
		// (ADR-0073); this domain writes nothing for it.
		return 0, nil
	case KindEviction:
		return a.applyEviction(ctx, tx, c)
	case KindGeneration:
		return a.applyGeneration(ctx, tx, c)
	case KindStore:
		return a.applyStore(ctx, tx, c)
	}
	return 0, fmt.Errorf("artifactdata: %s is not a kind this build applies, and the record at "+
		"%s claims version %d: a kind is declared before it is published",
		record.Subject.Kind, rec.Position, record.V)
}

// applyStore dispatches a store-subject record on its op.
func (a *Applier) applyStore(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	artifact := c.record.Subject.ID
	if _, ok := payload.(PurgePayload); ok {
		return a.applyPurge(ctx, tx, c, artifact)
	}
	s, present, err := readStore(ctx, tx, artifact)
	if err != nil {
		return 0, err
	}
	if present {
		var done int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM artifact_data_writes WHERE store_no = ? AND seq = ?`,
			s.no, c.packed).Scan(&done); err != nil {
			return 0, fmt.Errorf("artifactdata: read whether the record at %s applied: %w",
				c.position, err)
		}
		if done > 0 {
			return 0, nil
		}
	}
	switch p := payload.(type) {
	case WritePayload:
		c.authority = p.Authority
		return a.applyOps(ctx, tx, c, s, present, p.Write.Ops, p.Write.ClientID, OpWrite, 0)
	case UndoPayload:
		c.authority = p.Authority
		return a.applyOps(ctx, tx, c, s, present, p.Ops, p.ClientID, OpUndo, p.Undoes)
	case ConfigurePayload:
		c.authority = p.Authority
		return a.applyConfigure(ctx, tx, c, s, present, p)
	}
	return 0, fmt.Errorf("artifactdata: the store record at %s carries a %T for op %s",
		c.position, payload, c.record.Op)
}

// applyOps applies one write or undo: the evaluation, then its rows, or its
// refusal as a row.
func (a *Applier) applyOps(ctx context.Context, tx *sql.Tx, c applyContext, s storeRow,
	present bool, ops []artifacts.DataOp, clientID string, op OpKind, undoes int64) (int, error) {

	// THE RECORD'S ACTOR IS THE STAMP'S: a record naming one author in its
	// envelope and another in its authority is a writer's mistake, refused
	// rather than attributed to either.
	if c.authority.Actor.Name != c.record.Actor || c.authority.Actor.Kind != c.record.ActorKind {
		return a.commitRows(ctx, tx, c, s, present, refuse(artifacts.DataInvalid, "the record's "+
			"author and its authority name different actors"), nil, op, clientID, undoes, false)
	}
	v, err := newTxView(ctx, tx, s, present)
	if err != nil {
		return 0, err
	}
	p, err := evaluate(v, c.authority, ops, op == OpUndo)
	if err != nil {
		return 0, err
	}
	if p.refusal != nil {
		return a.commitRows(ctx, tx, c, s, present, p.refusal, nil, op, clientID, undoes, false)
	}
	return a.commitRows(ctx, tx, c, s, present, nil, func(s *storeRow, h *historyRow,
		deltas *[]DocDelta) (int, int64, error) {
		n, err := a.writePlan(ctx, tx, c, s, p, h, deltas)
		return n, p.docsDelta, err
	}, op, clientID, undoes, false)
}

// rowWriter writes an applied record's own rows, moves the store's counters and
// digest, fills its history row and its movements, and answers the rows it
// wrote and how far it moved the store's live documents.
type rowWriter func(s *storeRow, h *historyRow, deltas *[]DocDelta) (int, int64, error)

// commitRows writes one record on a store: the store row (made on the store's
// first record), the record's own rows through write (or, refused, nothing of
// them), the history row and its prune, the store's counters and the company's.
func (a *Applier) commitRows(ctx context.Context, tx *sql.Tx, c applyContext, s storeRow, present bool,
	refused *refusal, write rowWriter, op OpKind, clientID string, undoes int64, schema bool) (int, error) {

	rows := 0
	usage := usageDelta{}
	if !present {
		n, err := createStore(ctx, tx, c, &s)
		if err != nil {
			return 0, err
		}
		rows += n
		usage.stores = 1
	}
	before := s.computeFootprint()
	h := historyRow{op: op, clientID: clientID, undoes: undoes}
	var deltas []DocDelta
	if refused != nil {
		h.verdict, h.reason = refused.verdict, refused.reason
		for _, ref := range refused.conflicts {
			h.changes = append(h.changes, Change{Collection: ref.Collection, Doc: ref.Doc,
				Op: ChangeConflict, Creator: refused.creators[ref]})
		}
		schema = false
	} else {
		h.verdict = artifacts.DataApplied
		n, docs, err := write(&s, &h, &deltas)
		if err != nil {
			return 0, err
		}
		rows += n
		usage.docs = docs
	}
	h.digest = s.digest
	bytes, n, err := writeHistory(ctx, tx, c, s, h)
	if err != nil {
		return 0, err
	}
	rows += n
	s.historyBytes += bytes
	s.writes++
	pruned, err := pruneHistory(ctx, tx, c, &s, c.authority.Limits)
	if err != nil {
		return 0, err
	}
	rows += pruned
	s.footprint = s.computeFootprint()
	usage.footprint = s.footprint - before
	if err := saveStore(ctx, tx, c, s); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return 0, err
	}
	n, err = addUsage(ctx, tx, usage)
	if err != nil {
		return 0, err
	}
	rows += 1 + n
	a.note(c, s.artifact, deltas, h, schema)
	return rows, nil
}

// writePlan writes an applied plan's rows and moves s's counters and digest.
func (a *Applier) writePlan(ctx context.Context, tx *sql.Tx, c applyContext, s *storeRow, p plan,
	h *historyRow, deltas *[]DocDelta) (int, error) {

	rows := 0
	for _, coll := range p.newColl {
		n, err := insertCollection(ctx, tx, c, s, coll)
		if err != nil {
			return 0, err
		}
		rows += n
	}
	paths, n, err := internPaths(ctx, tx, c, s, p.docs)
	if err != nil {
		return 0, err
	}
	rows += n
	undoable := len(p.docs) > 0
	collDelta := map[int64][2]int64{}
	for _, d := range p.docs {
		n, err := writeDoc(ctx, tx, c, s, d, paths)
		if err != nil {
			return 0, err
		}
		rows += n
		ch, whole := changeOf(d, c.packed, opOf(d))
		h.changes = append(h.changes, ch)
		h.touches = append(h.touches, docTouch{collection: d.coll.no, doc: d.id})
		if !whole {
			undoable = false
		}
		if d.takeover {
			// THE ROW CHANGED HANDS: its previous creator's catch-up,
			// narrowed to their own rows, would never see the delete it
			// missed, so every catch-up from before now reads whole.
			s.tombstoneFloor = max(s.tombstoneFloor, c.packed)
		}
		cd := collDelta[d.coll.no]
		switch {
		case d.live && !d.live0:
			cd[0]++
		case !d.live && d.live0:
			cd[0]--
		}
		cd[1] += int64(len(d.body)) - int64(len(d.body0))
		collDelta[d.coll.no] = cd
		*deltas = append(*deltas, deltaOf(c, d))
	}
	h.undoable = undoable && undoFits(h.changes)
	for _, no := range slices.Sorted(maps.Keys(collDelta)) {
		cd := collDelta[no]
		if _, err := tx.ExecContext(ctx, `
			UPDATE artifact_data_collections SET docs = docs + ?, body_bytes = body_bytes + ?
			WHERE store_no = ? AND collection_no = ?`, cd[0], cd[1], s.no, no); err != nil {
			return 0, fmt.Errorf("artifactdata: count collection %d of store %s: %w", no,
				s.artifact, err)
		}
		rows++
	}
	s.docs += p.docsDelta
	s.tombstones += p.tombstoneDelta
	s.bodyBytes += p.bodyDelta
	s.fieldEntries += p.entriesDelta
	return rows, nil
}

// deltaOf is the movement one changed document makes.
func deltaOf(c applyContext, d *docState) DocDelta {
	delta := DocDelta{
		Collection: d.coll.spec.Name, Doc: d.id, Version: c.packed, Deleted: !d.live,
		Read: d.coll.spec.Read, CreatedBy: d.createdBy,
		UpdatedBy: iam.Actor{Name: c.record.Actor, Kind: c.record.ActorKind}, UpdatedAt: c.brokerAt,
	}
	if d.live {
		delta.Body = append([]byte(nil), d.body...)
	}
	return delta
}

// note records what one record moved, for the post-commit half.
func (a *Applier) note(c applyContext, artifact string, deltas []DocDelta, h historyRow, schema bool) {
	id, err := artifacts.ParseID(artifact)
	if err != nil {
		// A store whose subject names no artifact id was refused at every
		// write, and no socket follows it.
		return
	}
	a.moved = append(a.moved, Moved{
		Artifact: id, Docs: deltas, Schema: schema, Through: c.position, At: c.brokerAt,
		Writes: []WriteSummary{{
			Seq: c.packed, OpID: c.record.OpID, Op: h.op, Verdict: h.verdict,
			Actor:    iam.Actor{Name: c.record.Actor, Kind: c.record.ActorKind, OperatorID: c.record.OperatorID},
			ClientID: h.clientID, At: c.brokerAt,
		}},
	})
}

// createStore inserts a store's catalogue row at its first record: its number
// is that record's position, the same on every data node.
func createStore(ctx context.Context, tx *sql.Tx, c applyContext, s *storeRow) (int, error) {
	s.no, s.nextCollection, s.nextPath = c.packed, 1, 1
	s.createdAt = c.at()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_data_stores
			(artifact_id, store_no, state_digest, created_at, updated_at, version)
		VALUES (?, ?, ?, ?, ?, ?)`, s.artifact, s.no, canon.Digest{}.Bytes(), s.createdAt,
		s.createdAt, c.packed); err != nil {
		return 0, fmt.Errorf("artifactdata: create store %s at %s: %w", s.artifact, c.position, err)
	}
	return 1, nil
}

// saveStore writes a store's counters, digest and floors back.
func saveStore(ctx context.Context, tx *sql.Tx, c applyContext, s storeRow) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifact_data_stores SET docs = ?, tombstones = ?, body_bytes = ?,
			field_entries = ?, doc_bytes = ?, history_bytes = ?, footprint = ?, writes = ?,
			state_digest = ?, history_floor = ?, tombstones_floor = ?, next_collection_no = ?,
			next_path_no = ?, updated_at = ?, version = ?
		WHERE artifact_id = ?`, s.docs, s.tombstones, s.bodyBytes, s.fieldEntries, s.docBytes,
		s.historyBytes, s.footprint, s.writes, s.digest.Bytes(), s.historyFloor,
		s.tombstoneFloor, s.nextCollection, s.nextPath, c.at(), c.packed, s.artifact); err != nil {
		return fmt.Errorf("artifactdata: save store %s at %s: %w", s.artifact, c.position, err)
	}
	return nil
}

// usageDelta is how far a record moves the company's counters.
type usageDelta struct{ stores, docs, footprint int64 }

// addUsage moves the company's counters.
func addUsage(ctx context.Context, tx *sql.Tx, d usageDelta) (int, error) {
	if d == (usageDelta{}) {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_data_usage (scope, stores, docs, footprint) VALUES (?, ?, ?, ?)
		ON CONFLICT (scope) DO UPDATE SET
			stores = artifact_data_usage.stores + excluded.stores,
			docs = artifact_data_usage.docs + excluded.docs,
			footprint = artifact_data_usage.footprint + excluded.footprint`,
		usageScope, d.stores, d.docs, d.footprint); err != nil {
		return 0, fmt.Errorf("artifactdata: move the company's datastore counters: %w", err)
	}
	return 1, nil
}

// insertCollection writes a collection a record creates, numbered next, and
// adds its term to the digest.
func insertCollection(ctx context.Context, tx *sql.Tx, c applyContext, s *storeRow, coll *collState) (int, error) {
	spec, err := canonicalSpec(coll.spec)
	if err != nil {
		return 0, err
	}
	coll.no = s.nextCollection
	s.nextCollection++
	implicit := 0
	if coll.implicit {
		implicit = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_data_collections
			(store_no, collection_no, name, spec, implicit, docs, body_bytes, version)
		VALUES (?, ?, ?, ?, ?, 0, 0, ?)`, s.no, coll.no, coll.spec.Name, string(spec), implicit,
		c.packed); err != nil {
		return 0, fmt.Errorf("artifactdata: create collection %s of store %s: %w",
			coll.spec.Name, s.artifact, err)
	}
	coll.stored = true
	s.digest = s.digest.Add(canon.CollectionTerm(coll.spec.Name, spec))
	return 1, nil
}

// internPaths numbers every path the changed documents' index entries name,
// assigning the paths the store has never held their numbers in sorted order.
func internPaths(ctx context.Context, tx *sql.Tx, c applyContext, s *storeRow, docs []*docState) (map[string]int64, int, error) {
	want := map[string]bool{}
	for _, d := range docs {
		for _, e := range d.entries {
			want[e.path] = true
		}
	}
	return internNames(ctx, tx, c, s, slices.Sorted(maps.Keys(want)))
}

// internNames numbers paths, the new ones in the order given.
func internNames(ctx context.Context, tx *sql.Tx, c applyContext, s *storeRow, paths []string) (map[string]int64, int, error) {
	out := map[string]int64{}
	rows := 0
	for _, path := range paths {
		var no int64
		err := tx.QueryRowContext(ctx, `
			SELECT path_no FROM artifact_data_paths WHERE store_no = ? AND path = ?`,
			s.no, path).Scan(&no)
		switch {
		case err == nil:
			out[path] = no
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return nil, 0, fmt.Errorf("artifactdata: read path %s of store %s: %w", path, s.artifact, err)
		}
		no = s.nextPath
		s.nextPath++
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO artifact_data_paths (store_no, path_no, path) VALUES (?, ?, ?)`,
			s.no, no, path); err != nil {
			return nil, 0, fmt.Errorf("artifactdata: intern path %s of store %s at %s: %w", path,
				s.artifact, c.position, err)
		}
		out[path] = no
		rows++
	}
	return out, rows, nil
}

// writeDoc writes one changed document, its index entries and its digest term.
func writeDoc(ctx context.Context, tx *sql.Tx, c applyContext, s *storeRow, d *docState,
	paths map[string]int64) (int, error) {

	name := d.coll.spec.Name
	if d.live0 {
		s.digest = s.digest.Sub(canon.DocumentTerm(name, d.id, d.body0, d.version0))
	}
	if d.live {
		s.digest = s.digest.Add(canon.DocumentTerm(name, d.id, d.body, c.packed))
	}
	actor, kind := c.record.Actor, string(c.record.ActorKind)
	createdBy, createdKind, createdAt := d.createdBy, d.createdByKind, d.createdAt
	if createdAt == 0 {
		createdAt = c.at()
	}
	var body any
	if d.live {
		body = string(d.body)
	}
	// THE ROW'S METERED BYTES, from exactly what this writes, kept on the row
	// so the next write moves the store's count by the difference.
	metered := meteredDoc(&docState{id: d.id, live: d.live, body: d.body, entries: d.entries,
		createdBy: createdBy, createdByKind: createdKind}, actor, kind)
	var err error
	if d.row {
		_, err = tx.ExecContext(ctx, `
			UPDATE artifact_data_docs SET body = ?, size = ?, created_by = ?, created_by_kind = ?,
				created_at = ?, updated_by = ?, updated_by_kind = ?, updated_at = ?, version = ?,
				metered = ?
			WHERE store_no = ? AND collection_no = ? AND doc_id = ?`,
			body, len(d.body), createdBy, createdKind, createdAt, actor, kind, c.at(), c.packed,
			metered, s.no, d.coll.no, d.id)
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO artifact_data_docs
				(store_no, collection_no, doc_id, body, size, created_by, created_by_kind,
				 created_at, updated_by, updated_by_kind, updated_at, version, metered)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.no, d.coll.no, d.id, body, len(d.body), createdBy, createdKind, createdAt, actor,
			kind, c.at(), c.packed, metered)
	}
	if err != nil {
		return 0, fmt.Errorf("artifactdata: write document %s/%s at %s: %w", name, d.id, c.position, err)
	}
	s.docBytes += metered - d.metered0
	rows := 1
	// THE DOCUMENT'S INDEX IS REWRITTEN WHOLE: at most 64 rows, deleted and
	// inserted in one statement each, which is cheaper than diffing paths
	// and leaves no stale entry behind a path a value left.
	if d.live0 || d.entries0 > 0 {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := tx.ExecContext(ctx, `
			DELETE FROM artifact_data_fields WHERE store_no = ? AND collection_no = ? AND doc_id = ?`,
			s.no, d.coll.no, d.id)
		if err != nil {
			return 0, fmt.Errorf("artifactdata: clear document %s/%s's index: %w", name, d.id, err)
		}
		n, _ := res.RowsAffected()
		rows += int(n)
	}
	if !d.live {
		return rows, nil
	}
	n, err := store.InsertRows(ctx, tx, c.maxVariables, `
		INSERT INTO artifact_data_fields
			(store_no, collection_no, path_no, doc_id, ord, kind, num, txt) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?)`, ``, len(d.entries), func(i int) []any {
			e := d.entries[i]
			var num, txt any
			switch e.kind {
			case "n", "b":
				num = e.num
			case "s":
				txt = e.txt
			}
			return []any{s.no, d.coll.no, paths[e.path], d.id, e.ord, e.kind, num, txt}
		})
	if err != nil {
		return 0, fmt.Errorf("artifactdata: index document %s/%s: %w", name, d.id, err)
	}
	return rows + n, nil
}

// applyPurge removes a store permanently: every row it holds, in its own
// statement list (the estate keeps no foreign key), and the deletion marker,
// written whether or not the store has rows, so a record about it that
// arrives later is gated on every data node alike.
func (a *Applier) applyPurge(ctx context.Context, tx *sql.Tx, c applyContext, artifact string) (int, error) {
	actor := c.record.actor()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO artifactdata_deletions
			(artifact_id, at, by, by_kind, purge_record_id, rejects, last_reject_at, version)
		VALUES (?, ?, ?, ?, ?, 0, 0, ?)
		ON CONFLICT (artifact_id) DO NOTHING`,
		artifact, c.at(), actor.Name, string(actor.Kind), c.record.OpID, c.packed)
	if err != nil {
		return 0, fmt.Errorf("artifactdata: mark store %s purged: %w", artifact, err)
	}
	marked, _ := res.RowsAffected()
	rows := int(marked)
	s, present, err := readStore(ctx, tx, artifact)
	if err != nil || !present {
		return rows, err
	}
	for _, stmt := range []string{
		`DELETE FROM artifact_data_fields WHERE store_no = ?`,
		`DELETE FROM artifact_data_docs WHERE store_no = ?`,
		`DELETE FROM artifact_data_touches WHERE store_no = ?`,
		`DELETE FROM artifact_data_writes WHERE store_no = ?`,
		`DELETE FROM artifact_data_paths WHERE store_no = ?`,
		`DELETE FROM artifact_data_collections WHERE store_no = ?`,
		`DELETE FROM artifact_data_stores WHERE store_no = ?`,
	} {
		res, err := tx.ExecContext(ctx, stmt, s.no) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return 0, fmt.Errorf("artifactdata: purge store %s: %w", artifact, err)
		}
		n, _ := res.RowsAffected()
		rows += int(n)
	}
	n, err := addUsage(ctx, tx, usageDelta{stores: -1, docs: -s.docs, footprint: -s.footprint})
	if err != nil {
		return 0, err
	}
	if id, err := artifacts.ParseID(artifact); err == nil {
		a.moved = append(a.moved, Moved{Artifact: id, Purged: true, Through: c.position,
			At: c.brokerAt})
	}
	return rows + n, nil
}

// applyEviction records a node's removal from this log, or its readmission,
// as an INVERSE COMMIT rather than a delete.
func (a *Applier) applyEviction(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	e, ok := payload.(Eviction)
	if !ok || e.NodeID == "" {
		return 0, fmt.Errorf("artifactdata: the eviction at %s names no node", c.position)
	}
	var res sql.Result
	if e.Readmitted {
		res, err = tx.ExecContext(ctx, `
			UPDATE artifactdata_evictions SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`, c.packed, c.packed, e.NodeID, c.packed)
	} else {
		res, err = tx.ExecContext(ctx, `
			INSERT INTO artifactdata_evictions
				(node_id, at, by, from_position, readmitted_position, version)
			VALUES (?, ?, ?, ?, NULL, ?)
			ON CONFLICT (node_id) DO UPDATE SET
				at = excluded.at, by = excluded.by,
				from_position = excluded.from_position,
				readmitted_position = NULL, version = excluded.version
			WHERE excluded.version > artifactdata_evictions.version`,
			e.NodeID, c.at(), e.EvictedBy, c.packed, c.packed)
	}
	if err != nil {
		return 0, fmt.Errorf("artifactdata: apply the eviction of %s at %s: %w", e.NodeID, c.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyGeneration writes a reanchor's audit row, create-only.
func (a *Applier) applyGeneration(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	g, ok := payload.(Generation)
	if !ok {
		return 0, fmt.Errorf("artifactdata: the generation record at %s carries a %T", c.position, payload)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO artifactdata_log_generations
			(generation, at, by, new_stream_created_at, prev_last_seq_seen, record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (generation) DO NOTHING`,
		g.Generation, c.at(), g.By, store.EncodeTime(g.StreamAt), int64(g.PrevHighest), c.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("artifactdata: record generation %d at %s: %w", g.Generation, c.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
