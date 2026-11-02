package artifactdata

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifacts"
)

// A SCHEMA CHANGE, decided in a snapshot of the schema it replaces (it is
// arbitrated on the store's subject, so the decide and the apply see the same
// schema) and judged again by the applier, which is authoritative.
//
// # What a change may do to the data
//
// Adding a column, a collection, an option, a label, a colour or a rule rewrites
// nothing: a read fills a default. Archiving an option rewrites nothing: a
// value naming it reads as archived until somebody reassigns it. Removing an
// option outright is refused (archive it), so nothing a reviewer chose
// disappears.
//
// Removing a column removes its values, and RETYPING one coerces them by the
// one table below, clearing a value the new type cannot hold; changing a
// collection's indexed paths re-indexes its documents. Each REWRITES documents,
// and one record rewrites at most [artifacts.MaxMigrationDocs] (review A7: an
// apply holds the estate's only writer, so its work is bounded); a change that
// would rewrite more is refused, naming the count, at the writer and again
// here. A retype the table has no coercion for is refused: add a column and
// copy into it.
//
// Dropping a collection is admitted only when it holds no live document. A
// collection a write created under the closed default is kept as it is when a
// change does not name it; naming it declares it.

// configPlan is what a schema change decided.
type configPlan struct {
	refusal *refusal

	// next is every declared collection after the change, by name.
	next []artifacts.Collection

	// existing is every collection the store holds now.
	existing []*collState

	// rewrites are the documents the change rewrites, with their new
	// fields, grouped by collection in collection order.
	rewrites []*docState
}

// coercible reports whether a column may be retyped from one type to another,
// and so whether its values are carried across by [coerce].
func coercible(from, to artifacts.ColumnType) bool {
	if from == to {
		return true
	}
	textual := func(t artifacts.ColumnType) bool {
		return t == artifacts.ColumnText || t == artifacts.ColumnLongText
	}
	single := func(t artifacts.ColumnType) bool {
		return t == artifacts.ColumnSelect || t == artifacts.ColumnStatus
	}
	switch {
	case textual(from) && textual(to), single(from) && single(to):
		return true
	case single(from) && to == artifacts.ColumnMultiSelect,
		from == artifacts.ColumnMultiSelect && single(to):
		return true
	case from == artifacts.ColumnNumber && textual(to), textual(from) && to == artifacts.ColumnNumber:
		return true
	case textual(to) && (from == artifacts.ColumnCheckbox || from == artifacts.ColumnURL ||
		from == artifacts.ColumnDate || from == artifacts.ColumnPerson):
		return true
	case textual(from) && (to == artifacts.ColumnURL || to == artifacts.ColumnDate ||
		to == artifacts.ColumnPerson):
		return true
	case from == artifacts.ColumnRating && to == artifacts.ColumnNumber,
		from == artifacts.ColumnNumber && to == artifacts.ColumnRating:
		return true
	}
	return false
}

// coerce is a value carried from one column to its retyped successor, and
// false where it cannot be held, so the value is cleared.
func coerce(from, to artifacts.Column, v canon.Value) (canon.Value, bool) {
	out := v
	switch {
	case to.Type == artifacts.ColumnMultiSelect && from.Type != artifacts.ColumnMultiSelect:
		out = canon.Value{Kind: canon.KindArray, Array: []canon.Value{v}}
	case from.Type == artifacts.ColumnMultiSelect && to.Type != artifacts.ColumnMultiSelect:
		if v.Kind != canon.KindArray || len(v.Array) == 0 {
			return canon.Value{}, false
		}
		out = v.Array[0]
	case v.Kind == canon.KindNumber && (to.Type == artifacts.ColumnText || to.Type == artifacts.ColumnLongText):
		out = canon.Value{Kind: canon.KindString, String: v.Number}
	case v.Kind == canon.KindBool && (to.Type == artifacts.ColumnText || to.Type == artifacts.ColumnLongText):
		out = canon.Value{Kind: canon.KindString, String: strconv.FormatBool(v.Bool)}
	case v.Kind == canon.KindString && to.Type == artifacts.ColumnNumber:
		text := strings.TrimSpace(v.String)
		if _, err := finite(text); err != nil {
			return canon.Value{}, false
		}
		parsed, err := canon.Parse([]byte(text), 1)
		if err != nil || parsed.Kind != canon.KindNumber {
			return canon.Value{}, false
		}
		out = parsed
	}
	if checkValue(to, out) != nil {
		return canon.Value{}, false
	}
	return out, true
}

// planConfigure decides a schema change against the store's rows: who may make
// it, what it must hold, and which documents it rewrites. A refusal is a plan
// carrying it; an error is the store's.
func planConfigure(ctx context.Context, tx *sql.Tx, s storeRow, present bool,
	authority artifacts.Authority, collections []artifacts.Collection) (configPlan, error) {

	no := func(v artifacts.DataVerdict, format string, args ...any) (configPlan, error) {
		return configPlan{refusal: refuse(v, format, args...)}, nil
	}
	if r := checkAuthority(authority); r != nil {
		return configPlan{refusal: r}, nil
	}
	if !authority.Role.Includes(artifacts.RoleEditor) {
		return no(artifacts.DataForbidden, "changing the schema needs editor, and the writer is %s",
			authority.Role)
	}
	if len(collections) > artifacts.MaxCollections {
		return no(artifacts.DataQuota, "the schema declares %d collections and the maximum is %d",
			len(collections), artifacts.MaxCollections)
	}
	names := map[string]bool{}
	for _, c := range collections {
		compiled, err := Compile(c)
		if err != nil {
			return configPlan{refusal: fromErr(err)}, nil
		}
		if !sameRules(compiled, c) {
			return no(artifacts.DataInvalid, "collection %q names preset %s but not the rules it "+
				"compiles to; a schema is stored compiled", c.Name, c.Preset)
		}
		if err := ValidateCollection(c); err != nil {
			return configPlan{refusal: fromErr(err)}, nil
		}
		if names[c.Name] {
			return no(artifacts.DataInvalid, "collection %q is declared twice", c.Name)
		}
		names[c.Name] = true
	}
	p := configPlan{next: collections}
	if present {
		existing, err := readCollections(ctx, tx, s.no)
		if err != nil {
			return configPlan{}, err
		}
		p.existing = existing
	}
	byName := map[string]*collState{}
	for _, c := range p.existing {
		byName[c.spec.Name] = c
	}
	// AN AGENT TIGHTENS AND NEVER LOOSENS (03 section 4.2), judged against
	// every collection it touches, the closed default of one a write created
	// included.
	var old []artifacts.Collection
	for _, c := range p.existing {
		if c.spec.Name == StateCollection || (c.implicit && !names[c.spec.Name]) {
			continue
		}
		old = append(old, c.spec)
	}
	if err := agentMayChange(authority.Actor.Kind, Loosenings(old, collections)); err != nil {
		return no(artifacts.DataForbidden, "%s", strings.TrimPrefix(err.Error(), ErrForbidden.Error()+": "))
	}
	for _, c := range p.existing {
		if c.spec.Name == StateCollection || c.implicit || names[c.spec.Name] {
			continue
		}
		if c.docs > 0 {
			return no(artifacts.DataInvalid, "collection %q holds %d rows; delete them before "+
				"dropping it", c.spec.Name, c.docs)
		}
	}
	total := 0
	for _, next := range collections {
		cur, held := byName[next.Name]
		if !held {
			continue
		}
		affected, r := affectedFields(cur.spec, next)
		if r != nil {
			return configPlan{refusal: r}, nil
		}
		reindex := !slices.Equal(slices.Sorted(slices.Values(cur.spec.Indexed)),
			slices.Sorted(slices.Values(next.Indexed)))
		if len(affected) == 0 && !reindex {
			continue
		}
		ids, err := migrationDocs(ctx, tx, s.no, cur, affected, reindex)
		if err != nil {
			return configPlan{}, err
		}
		if len(ids) == 0 {
			continue
		}
		total += len(ids)
		if total > artifacts.MaxMigrationDocs {
			r := refuse(artifacts.DataInvalid, "the change rewrites more than %d documents, the "+
				"most one schema change rewrites; change fewer columns at once, or empty the "+
				"collection first", artifacts.MaxMigrationDocs)
			r.class = ErrMigrationTooLarge
			return configPlan{refusal: r}, nil
		}
		docs, err := rewriteDocs(ctx, tx, s, cur, next, ids)
		if err != nil {
			return configPlan{}, err
		}
		for _, d := range docs {
			d.reindex = reindex
			if r := checkWhole(d); r != nil {
				return configPlan{refusal: r}, nil
			}
		}
		p.rewrites = append(p.rewrites, docs...)
	}
	return p, nil
}

// sameRules reports two collections whose rule fields agree.
func sameRules(a, b artifacts.Collection) bool {
	return a.Read == b.Read && a.Create == b.Create && a.Delete == b.Delete && a.Preset == b.Preset
}

// affectedFields is every top-level field whose values a collection's change
// rewrites: a removed column's, and a retyped column's. A retype with no
// coercion, and an option removed outright, refuse the change.
func affectedFields(cur, next artifacts.Collection) ([]string, *refusal) {
	if cur.Columns == nil || next.Columns == nil {
		// A SCHEMALESS COLLECTION GAINING A SCHEMA, or one losing it,
		// rewrites nothing: reads are lenient, and the next whole write
		// of a row holds it to whatever the schema then says.
		return nil, nil
	}
	nextCols := map[string]artifacts.Column{}
	for _, col := range next.Columns {
		nextCols[col.Key] = col
	}
	var out []string
	for _, col := range cur.Columns {
		n, kept := nextCols[col.Key]
		if !kept {
			out = append(out, col.Key)
			continue
		}
		if !coercible(col.Type, n.Type) {
			return nil, refuse(artifacts.DataInvalid, "column %s of %s cannot be retyped from %s "+
				"to %s; add a column and copy into it", col.Key, next.Name, col.Type, n.Type)
		}
		if hasOptions(col.Type) && hasOptions(n.Type) {
			for _, o := range col.Options {
				if !slices.ContainsFunc(n.Options, func(x artifacts.Option) bool { return x.ID == o.ID }) {
					return nil, refuse(artifacts.DataInvalid, "column %s of %s drops option %q; "+
						"archive it instead, so the rows that chose it keep their value",
						col.Key, next.Name, o.ID)
				}
			}
		}
		if col.Type != n.Type {
			out = append(out, col.Key)
		}
	}
	slices.Sort(out)
	return out, nil
}

// migrationDocs is the live documents of a collection a change rewrites: every
// one holding an affected field, or every one when its indexed paths change. A
// document holding a field is found through the field index, where every
// top-level field a document holds has at least one entry: its scalar, its
// list's scalar elements, or a presence marker where it holds nothing scalar
// (an emptied multiselect is the common case).
func migrationDocs(ctx context.Context, tx *sql.Tx, storeNo int64, c *collState, fields []string,
	reindex bool) ([]string, error) {

	var (
		rows *sql.Rows
		err  error
	)
	limit := artifacts.MaxMigrationDocs + 1
	if reindex {
		rows, err = tx.QueryContext(ctx, `
			SELECT doc_id FROM artifact_data_docs
			WHERE store_no = ? AND collection_no = ? AND body IS NOT NULL
			ORDER BY doc_id LIMIT ?`, storeNo, c.no, limit)
	} else {
		args := []any{storeNo, c.no}
		marks := make([]string, len(fields))
		for i, f := range fields {
			marks[i] = "?"
			args = append(args, f)
		}
		args = append(args, limit)
		rows, err = tx.QueryContext(ctx, `
			SELECT DISTINCT f.doc_id FROM artifact_data_fields f
			JOIN artifact_data_paths p ON p.store_no = f.store_no AND p.path_no = f.path_no
			WHERE f.store_no = ? AND f.collection_no = ? AND p.path IN (`+strings.Join(marks, ", ")+`)
			ORDER BY f.doc_id LIMIT ?`, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("artifactdata: find the documents %s's change rewrites: %w",
			c.spec.Name, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("artifactdata: find the documents %s's change rewrites: %w",
				c.spec.Name, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// rewriteDocs reads each document and computes what the change leaves it
// holding.
func rewriteDocs(ctx context.Context, tx *sql.Tx, s storeRow, cur *collState, next artifacts.Collection,
	ids []string) ([]*docState, error) {

	v := &txView{ctx: ctx, tx: tx, store: s, present: true}
	nextState := newCollState(next, cur.no, false, true, cur.docs)
	curCols := map[string]artifacts.Column{}
	for _, col := range cur.spec.Columns {
		curCols[col.Key] = col
	}
	var out []*docState
	for _, id := range ids {
		d, err := v.doc(cur, id)
		if err != nil {
			return nil, err
		}
		if !d.live0 {
			continue
		}
		d.coll = nextState
		fields := map[string]canon.Value{}
		for _, name := range sortedKeys(d.fields0) {
			val := d.fields0[name]
			from, wasDeclared := curCols[name]
			to, declared := nextState.columns[name]
			switch {
			case cur.spec.Columns == nil || next.Columns == nil:
				fields[name] = val
			case wasDeclared && !declared:
				// A REMOVED COLUMN'S VALUE GOES WITH IT.
			case wasDeclared && declared && from.Type != to.Type:
				if out, ok := coerce(from, to, val); ok {
					fields[name] = out
				}
			default:
				fields[name] = val
			}
		}
		d.fields, d.changed = fields, true
		out = append(out, d)
	}
	return out, nil
}

// applyConfigure applies a schema change: the plan, then its rows, or its
// refusal as a row.
func (a *Applier) applyConfigure(ctx context.Context, tx *sql.Tx, c applyContext, s storeRow,
	present bool, p ConfigurePayload) (int, error) {

	if c.authority.Actor.Name != c.record.Actor || c.authority.Actor.Kind != c.record.ActorKind {
		return a.commitRows(ctx, tx, c, s, present, refuse(artifacts.DataInvalid, "the record's "+
			"author and its authority name different actors"), nil, OpConfigure, "", 0, false)
	}
	cp, err := planConfigure(ctx, tx, s, present, c.authority, p.Collections)
	if err != nil {
		return 0, err
	}
	if cp.refusal != nil {
		return a.commitRows(ctx, tx, c, s, present, cp.refusal, nil, OpConfigure, "", 0, false)
	}
	return a.commitRows(ctx, tx, c, s, present, nil, func(s *storeRow, h *historyRow,
		deltas *[]DocDelta) (int, int64, error) {
		return a.writeConfigure(ctx, tx, c, s, cp, h, deltas)
	}, OpConfigure, "", 0, true)
}

// writeConfigure writes an applied schema change.
func (a *Applier) writeConfigure(ctx context.Context, tx *sql.Tx, c applyContext, s *storeRow,
	cp configPlan, h *historyRow, deltas *[]DocDelta) (int, int64, error) {

	rows := 0
	declared := map[string]artifacts.Collection{}
	for _, next := range cp.next {
		declared[next.Name] = next
	}
	held := map[string]*collState{}
	for _, cur := range cp.existing {
		held[cur.spec.Name] = cur
	}
	// DROPS, in collection order: the collection's row and its tombstones.
	for _, cur := range cp.existing {
		if cur.spec.Name == StateCollection || cur.implicit {
			continue
		}
		if _, kept := declared[cur.spec.Name]; kept {
			continue
		}
		spec, err := canonicalSpec(cur.spec)
		if err != nil {
			return 0, 0, err
		}
		var tombstones, metered int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(SUM(metered), 0) FROM artifact_data_docs
			WHERE store_no = ? AND collection_no = ? AND body IS NULL`, s.no, cur.no).Scan(
			&tombstones, &metered); err != nil {
			return 0, 0, fmt.Errorf("artifactdata: count collection %s's tombstones: %w", cur.spec.Name, err)
		}
		for _, stmt := range []string{
			`DELETE FROM artifact_data_docs WHERE store_no = ? AND collection_no = ?`,
			`DELETE FROM artifact_data_touches WHERE store_no = ? AND collection_no = ?`,
			`DELETE FROM artifact_data_collections WHERE store_no = ? AND collection_no = ?`,
		} {
			res, err := tx.ExecContext(ctx, stmt, s.no, cur.no)
			if err != nil {
				return 0, 0, fmt.Errorf("artifactdata: drop collection %s: %w", cur.spec.Name, err)
			}
			n, _ := res.RowsAffected()
			rows += int(n)
		}
		s.tombstones -= tombstones
		if tombstones > 0 {
			// THE DELETES GO WITH THE COLLECTION: a client that missed one
			// can no longer catch it up by version, so it reads whole.
			s.tombstoneFloor = max(s.tombstoneFloor, c.packed)
		}
		s.docBytes -= metered
		s.digest = s.digest.Sub(canon.CollectionTerm(cur.spec.Name, spec))
		h.changes = append(h.changes, Change{Collection: cur.spec.Name, Op: ChangeSchema})
	}
	// CHANGES AND CREATES, in the order the schema lists them.
	for _, next := range cp.next {
		spec, err := canonicalSpec(next)
		if err != nil {
			return 0, 0, err
		}
		cur, exists := held[next.Name]
		if !exists {
			n, err := insertCollection(ctx, tx, c, s, newCollState(next, 0, false, false, 0)) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return 0, 0, err
			}
			rows += n
			h.changes = append(h.changes, Change{Collection: next.Name, Op: ChangeSchema})
			continue
		}
		old, err := canonicalSpec(cur.spec)
		if err != nil {
			return 0, 0, err
		}
		if string(old) == string(spec) && !cur.implicit {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE artifact_data_collections SET spec = ?, implicit = 0, version = ?
			WHERE store_no = ? AND collection_no = ?`, string(spec), c.packed, s.no, cur.no); err != nil {
			return 0, 0, fmt.Errorf("artifactdata: change collection %s: %w", next.Name, err)
		}
		rows++
		if cur.spec.Read != next.Read {
			// A NEW READ RULE RESHAPES EVERY READER'S VIEW: rows a reader
			// may now see were never pushed to them, and a catch-up by
			// version returns only what changed since, so every catch-up
			// from before the change reads the store whole.
			s.tombstoneFloor = max(s.tombstoneFloor, c.packed)
		}
		s.digest = s.digest.Sub(canon.CollectionTerm(cur.spec.Name, old)).
			Add(canon.CollectionTerm(next.Name, spec))
		h.changes = append(h.changes, Change{Collection: next.Name, Op: ChangeSchema})
	}
	// THE REWRITES, through the one document writer every record uses.
	paths, n, err := internPaths(ctx, tx, c, s, cp.rewrites)
	if err != nil {
		return 0, 0, err
	}
	rows += n
	bodyDelta := map[int64]int64{}
	for _, d := range cp.rewrites {
		if sameDocument(d) && !d.reindex {
			continue
		}
		n, err := writeDoc(ctx, tx, c, s, d, paths)
		if err != nil {
			return 0, 0, err
		}
		rows += n
		s.bodyBytes += int64(len(d.body)) - int64(len(d.body0))
		s.fieldEntries += int64(len(d.entries)) - int64(d.entries0)
		bodyDelta[d.coll.no] += int64(len(d.body)) - int64(len(d.body0))
		ch, _ := changeOf(d, c.packed, ChangeMigrate)
		h.changes = append(h.changes, ch)
		h.touches = append(h.touches, docTouch{collection: d.coll.no, doc: d.id})
		*deltas = append(*deltas, deltaOf(c, d))
	}
	for _, no := range slices.Sorted(maps.Keys(bodyDelta)) {
		if _, err := tx.ExecContext(ctx, `
			UPDATE artifact_data_collections SET body_bytes = body_bytes + ?
			WHERE store_no = ? AND collection_no = ?`, bodyDelta[no], s.no, no); err != nil {
			return 0, 0, fmt.Errorf("artifactdata: count collection %d: %w", no, err)
		}
		rows++
	}
	return rows, 0, nil
}
