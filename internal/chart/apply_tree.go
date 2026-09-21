package chart

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// THE STRUCTURE'S APPLY: where everything on [KindTree] lands.
//
// Three ops share this subject and one apply path, because all three write the
// same two columns — `chart_units.parent_key` and `chart_seats.unit_key` — plus
// the authored lead edge. What differs is what else each one writes: an import
// also stamps the ledger, and a removal also writes tombstones and deletes the
// rows.
//
// THE STRUCTURAL WRITE STAMPS `scoped_through` AND NEVER `version`. The record
// arbitrated on the TREE's subject, not on the object's own, so writing an
// object's version from here would set an expectation no writer on that
// object's subject could ever satisfy — the next content write would be refused
// for ever, and the object would be permanently unwritable. A read barrier
// compares MAX of the two, which is what makes the column a claim about
// freshness without being one about arbitration.
//
// A PLACEMENT FOR AN OBJECT THAT DOES NOT EXIST YET IS NOT AN ERROR. An import
// publishes the structure FIRST and the content follows on each object's own
// subject, so the ordinary case is a placement landing before the row it places.
// The apply creates a stub — the key, the placement, and nothing else — which
// the content record then fills in without touching the placement back.

// applyTree dispatches the three structural ops.
func (a *Applier) applyTree(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	switch p := payload.(type) {
	case PlacementPayload:
		return a.applyPlacement(ctx, tx, at, p.Edges, ChangeMoved)
	case ImportPayload:
		return a.applyImport(ctx, tx, at, p)
	case RemovePayload:
		return a.applyRemoval(ctx, tx, at, p)
	}
	return 0, fmt.Errorf("chart: the structural record at %s carries a %T "+
		"payload, and the structure's three ops are %s, %s and %s",
		at.position, payload, OpPlace, OpImport, OpRemove)
}

// applyPlacement writes a set of edges as FULL POST-STATE.
//
// An object absent from the set is untouched, which is what makes a placement
// composable with a content record on the same object: neither carries the
// other's half, so neither can revert it.
func (a *Applier) applyPlacement(ctx context.Context, tx *sql.Tx, at applyContext,
	edges []Edge, kind ChangeKind) (int, error) {

	rows := 0
	for _, edge := range edges {
		// A REMOVED OBJECT IS SKIPPED RATHER THAN PLACED. The envelope
		// gate cannot answer for a structural record — it names its
		// objects inside a payload a gate may not be able to decode — so
		// the refusal is per object, here, where the payload is in hand.
		removed, err := objectRemoved(ctx, tx, edge.Object)
		if err != nil {
			return 0, err
		}
		if removed {
			continue
		}
		n, err := a.placeOne(ctx, tx, at, edge, kind)
		if err != nil {
			return 0, err
		}
		rows += n
	}
	if len(edges) > 0 {
		n, err := a.writeHistory(ctx, tx, at, edges[0].Object, kind)
		if err != nil {
			return 0, err
		}
		rows += n
	}
	return rows, nil
}

// placeOne writes one object's placement.
func (a *Applier) placeOne(ctx context.Context, tx *sql.Tx, at applyContext,
	edge Edge, kind ChangeKind) (int, error) {

	id := NormalizeKey(edge.Object.ID)
	if id == "" {
		return 0, fmt.Errorf("chart: the structural record at %s states an "+
			"edge with no object, so nothing could be placed by it", at.position)
	}
	parent := NormalizeKey(edge.Parent)
	lead := NormalizeKey(edge.Lead)

	switch edge.Object.Kind {
	case KindUnit:
		unit, found, err := readUnit(ctx, tx, id)
		if err != nil {
			return 0, err
		}
		if !found {
			// THE STUB. An import publishes structure before content,
			// so the row a placement needs routinely does not exist
			// yet — and refusing here would make the ordinary order
			// the broken one.
			unit = Unit{V: DocumentVersion, Key: id, CreatedAt: at.brokerAt}
		}
		unit.ParentKey = parent
		unit.Lead = lead
		unit.UpdatedAt = at.brokerAt
		unit.LastChange = a.change(at, edge.Object, kind)
		n, err := a.writeStructure(ctx, tx, at, unit, found)
		if err != nil {
			return 0, err
		}
		// THE AUTHORED LEAD EDGE, which a unit states at its own end.
		// It is a table rather than a field alone because the inverse —
		// which units does this seat lead — is read as often as the
		// authored direction, and a query that decoded every unit's
		// document to answer it is a full scan wearing an index's name.
		edges, err := a.writeLead(ctx, tx, at, id, lead)
		if err != nil {
			return 0, err
		}
		a.note(edge.Object)
		return n + edges, nil
	case KindSeat:
		seat, found, err := readSeat(ctx, tx, id)
		if err != nil {
			return 0, err
		}
		if !found {
			seat = Seat{V: DocumentVersion, Handle: id, CreatedAt: at.brokerAt}
		}
		seat.UnitKey = parent
		seat.UpdatedAt = at.brokerAt
		seat.LastChange = a.change(at, edge.Object, kind)
		n, err := a.writeSeatStructure(ctx, tx, at, seat, found)
		if err != nil {
			return 0, err
		}
		a.note(edge.Object)
		return n, nil
	}
	return 0, fmt.Errorf("chart: the structural record at %s places a %s, and "+
		"only a unit and a seat have a place in the tree",
		at.position, edge.Object.Kind)
}

// writeStructure writes a unit row from a STRUCTURAL record.
//
// `scoped_through` AND NEVER `version`: see this file's header. A row that
// already exists is updated in place under a monotone guard on that column
// alone, so a redelivered placement writes nothing and a content record's own
// expectation is never poisoned.
func (a *Applier) writeStructure(ctx context.Context, tx *sql.Tx, at applyContext,
	unit Unit, exists bool) (int, error) {

	document, err := EncodeUnit(unit)
	if err != nil {
		return 0, err
	}
	if exists {
		res, err := tx.ExecContext(ctx, `
			UPDATE chart_units
			SET parent_key = ?, lead = ?, updated_at = ?,
			    scoped_through = ?, document = ?
			WHERE key = ? AND scoped_through < ?`,
			unit.ParentKey, unit.Lead, store.EncodeTime(unit.UpdatedAt),
			at.packed, document, unit.Key, at.packed)
		if err != nil {
			return 0, fmt.Errorf("chart: place unit %s at %s: %w",
				unit.Key, at.position, err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	// THE STUB'S OWN `version` IS ZERO, which is the whole reason it is
	// safe to create one here: the first CONTENT record on this object
	// carries a position above zero and therefore wins its guard. A stub
	// written at this record's position would refuse every content record
	// published before it, and an import's content follows its structure by
	// construction.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chart_units
			(key, former_keys_json, name, type, purpose, channel, project,
			 space, parent_key, lead, created_at, updated_at, version,
			 scoped_through, document)
		VALUES (?, '[]', '', '', '', '', '', '', ?, ?, ?, ?, 0, ?, ?)
		ON CONFLICT (key) DO NOTHING`,
		unit.Key, unit.ParentKey, unit.Lead,
		store.EncodeTime(unit.CreatedAt), store.EncodeTime(unit.UpdatedAt),
		at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("chart: place the new unit %s at %s: %w",
			unit.Key, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// writeSeatStructure is [Applier.writeStructure] for a seat.
func (a *Applier) writeSeatStructure(ctx context.Context, tx *sql.Tx,
	at applyContext, seat Seat, exists bool) (int, error) {

	document, err := EncodeSeat(seat)
	if err != nil {
		return 0, err
	}
	if exists {
		res, err := tx.ExecContext(ctx, `
			UPDATE chart_seats
			SET unit_key = ?, updated_at = ?, scoped_through = ?, document = ?
			WHERE handle = ? AND scoped_through < ?`,
			seat.UnitKey, store.EncodeTime(seat.UpdatedAt), at.packed,
			document, seat.Handle, at.packed)
		if err != nil {
			return 0, fmt.Errorf("chart: place seat %s at %s: %w",
				seat.Handle, at.position, err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	// A STUB SEAT'S KIND IS `agent`, and that is a real decision rather
	// than a default falling out of the zero value: `kind` is NOT NULL and
	// every seat a chart places is one a company intends to run until its
	// content record says otherwise. A human seat's own record follows on
	// its subject and corrects it under the ordinary version guard.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chart_seats
			(handle, former_keys_json, kind, name, email, email_index,
			 backstory, goal, project, space, unit_key, created_at, updated_at,
			 version, scoped_through, document)
		VALUES (?, '[]', ?, '', '', '', '', '', '', '', ?, ?, ?, 0, ?, ?)
		ON CONFLICT (handle) DO NOTHING`,
		seat.Handle, string(SeatAgent), seat.UnitKey,
		store.EncodeTime(seat.CreatedAt), store.EncodeTime(seat.UpdatedAt),
		at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("chart: place the new seat %s at %s: %w",
			seat.Handle, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// writeLead writes one unit's authored lead edge, or clears it.
//
// AN EMPTY LEAD DELETES THE ROW rather than writing an empty handle: the
// inverse index is read as "which units does this seat lead", and a row naming
// nobody would answer that question for a seat whose handle is the empty
// string. A unit with no lead of its own is the ordinary case, because
// inheritance is what fills it in.
func (a *Applier) writeLead(ctx context.Context, tx *sql.Tx, at applyContext,
	unitKey, lead string) (int, error) {

	if lead == "" {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM chart_leads WHERE unit_key = ? AND version < ?`,
			unitKey, at.packed)
		if err != nil {
			return 0, fmt.Errorf("chart: clear the lead of %s at %s: %w",
				unitKey, at.position, err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chart_leads (unit_key, handle, version)
		VALUES (?, ?, ?)
		ON CONFLICT (unit_key) DO UPDATE SET
			handle = excluded.handle, version = excluded.version
		WHERE excluded.version > chart_leads.version`,
		unitKey, lead, at.packed)
	if err != nil {
		return 0, fmt.Errorf("chart: write the lead of %s at %s: %w",
			unitKey, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyImport writes one company revision's whole authored structure.
//
// THE LEDGER IS CHECKED FIRST AND IT IS THE WHOLE POINT. Re-activating an
// UNCHANGED revision is the credential-rotation gesture and is therefore
// routine; without this row it would rewrite every object in the chart and wake
// everybody a second time. With it, every node reaches the same no-op the same
// way — from a row, rather than from a comparison each node makes for itself.
func (a *Applier) applyImport(ctx context.Context, tx *sql.Tx, at applyContext,
	p ImportPayload) (int, error) {

	if p.Revision == "" {
		return 0, fmt.Errorf("chart: the import at %s names no revision, and "+
			"the ledger that makes a re-import a no-op is keyed on one",
			at.position)
	}
	var seen sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT position FROM chart_import_ledger WHERE revision = ?`,
		p.Revision).Scan(&seen)
	switch {
	case err == nil && seen.Valid:
		// ALREADY IMPORTED, at this position or an earlier one. Nothing
		// to write and nothing to wake — which is exactly what the
		// rotation gesture should cost.
		return 0, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("chart: read the import ledger for revision %s "+
			"at %s: %w", p.Revision, at.position, err)
	}

	rows, err := a.applyPlacement(ctx, tx, at, p.Edges, ChangeImported)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chart_import_ledger
			(revision, position, at, by, objects, record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (revision) DO NOTHING`,
		p.Revision, at.packed, store.EncodeTime(at.brokerAt),
		at.record.Actor, len(p.Edges), at.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("chart: write the import ledger row for revision "+
			"%s at %s: %w", p.Revision, at.position, err)
	}
	n, _ := res.RowsAffected()
	return rows + int(n), nil
}

// applyRemoval writes the tombstones and deletes the rows.
//
// THE TOMBSTONE IS WRITTEN FIRST AND OUTLIVES EVERYTHING. A removal is the one
// operation here with no inverse: every other record is a full post-state under
// a monotone guard, so a node that applied a stale one is repaired by the next
// record on that object, and nothing ever names a removed object again. Without
// the row, a redelivery of any earlier record would write the object straight
// back — on one node, in a table that claims identity.
//
// IT ALSO OUTLIVES THE RECORD ITSELF. A removal below the trim floor has no
// record left on the log to prove it happened, and this row is what still says
// so to a replay and to the write fence.
func (a *Applier) applyRemoval(ctx context.Context, tx *sql.Tx, at applyContext,
	p RemovePayload) (int, error) {

	rows := 0
	for _, object := range p.Objects {
		id := NormalizeKey(object.ID)
		if id == "" {
			return 0, fmt.Errorf("chart: the removal at %s names an object "+
				"with no id, so nothing could be removed by it", at.position)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO chart_removed
				(object_kind, object_id, at, record_id, actor, actor_kind,
				 reason, version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (object_kind, object_id) DO NOTHING`,
			string(object.Kind), id, store.EncodeTime(at.brokerAt),
			at.record.OpID, at.record.Actor, string(at.record.ActorKind),
			p.Reason, at.packed)
		if err != nil {
			return 0, fmt.Errorf("chart: tombstone %s %s at %s: %w",
				object.Kind, id, at.position, err)
		}
		n, _ := res.RowsAffected()
		rows += int(n)

		gone, err := a.deleteObject(ctx, tx, at, object.Kind, id)
		if err != nil {
			return 0, err
		}
		rows += gone
		a.note(ObjectRef{Kind: object.Kind, ID: id})
	}
	if len(p.Objects) > 0 {
		n, err := a.writeHistory(ctx, tx, at, p.Objects[0], ChangeRemoved)
		if err != nil {
			return 0, err
		}
		rows += n
	}
	return rows, nil
}

// deleteObject removes one object's row and every edge it owns.
//
// THE EDGES IT OWNS, never the edges that name it. A `manages:` entry pointing
// at a removed seat is what somebody WROTE, and the organisation model already
// reads a dangling reference as a warning rather than an error — so deleting
// the other end here would silently edit a document nobody edited, and the next
// config apply would write it straight back.
func (a *Applier) deleteObject(ctx context.Context, tx *sql.Tx, at applyContext,
	kind ObjectKind, id string) (int, error) {

	switch kind {
	case KindUnit:
		unit, err := tx.ExecContext(ctx, `DELETE FROM chart_units WHERE key = ?`, id)
		if err != nil {
			return 0, fmt.Errorf("chart: remove unit %s at %s: %w", id, at.position, err)
		}
		lead, err := tx.ExecContext(ctx, `DELETE FROM chart_leads WHERE unit_key = ?`, id)
		if err != nil {
			return 0, fmt.Errorf("chart: remove the lead edge of %s at %s: %w",
				id, at.position, err)
		}
		u, _ := unit.RowsAffected()
		l, _ := lead.RowsAffected()
		return int(u + l), nil
	case KindSeat:
		seat, err := tx.ExecContext(ctx, `DELETE FROM chart_seats WHERE handle = ?`, id)
		if err != nil {
			return 0, fmt.Errorf("chart: remove seat %s at %s: %w", id, at.position, err)
		}
		manages, err := tx.ExecContext(ctx,
			`DELETE FROM chart_manages WHERE manager = ?`, id)
		if err != nil {
			return 0, fmt.Errorf("chart: remove the manages edges of %s at "+
				"%s: %w", id, at.position, err)
		}
		s, _ := seat.RowsAffected()
		m, _ := manages.RowsAffected()
		return int(s + m), nil
	}
	return 0, fmt.Errorf("chart: the removal at %s names a %s, and only a unit "+
		"and a seat are objects in the chart", at.position, kind)
}

// objectRemoved reports the tombstone the removal gate cannot read from an
// envelope.
func objectRemoved(ctx context.Context, tx *sql.Tx, ref ObjectRef) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM chart_removed WHERE object_kind = ? AND object_id = ?`,
		string(ref.Kind), NormalizeKey(ref.ID)).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("chart: read the tombstone of %s: %w", ref, err)
	}
	return true, nil
}
