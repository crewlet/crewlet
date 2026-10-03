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
// the authored lead edge and, from record version 3, a seat's authored
// `manages:` edges. What differs is what else each one writes: an import also
// stamps the ledger, and a removal also writes tombstones and deletes the rows.
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
// the content record then fills in without touching the placement back. What
// may create one is an edge that states a create, an import's edge and a
// version-1 edge; a move or a lead change never does ([Applier.placeEdge]).

// applyTree dispatches the three structural ops.
func (a *Applier) applyTree(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	switch p := payload.(type) {
	case PlacementPayload:
		return a.applyPlacement(ctx, tx, at, readAt(at, p.Edges), ChangeMoved)
	case ImportPayload:
		p.Edges = readAt(at, p.Edges)
		return a.applyImport(ctx, tx, at, p)
	case RemovePayload:
		return a.applyRemoval(ctx, tx, at, p)
	}
	return 0, fmt.Errorf("chart: the structural record at %s carries a %T "+
		"payload, and the structure's three ops are %s, %s and %s",
		at.position, payload, OpPlace, OpImport, OpRemove)
}

// readAt is edges as the record's own version reads them.
//
// A VERSION-1 EDGE IS AN OBJECT, A PARENT AND A LEAD, and nothing else: the
// verb, a rename's source and a seat's kind are version-2 fields, which the
// build a version-1 record was written for had no field to read them into. No
// version-1 writer states one, and a record that did meant nothing by it to the
// build that first applied it — so it means nothing here either, or a replay
// would decline a "create" that build applied as the placement it was.
//
// AND A SEAT'S `manages:` LIST IS A VERSION-3 FIELD, on the same terms: below
// it the list was the content record's to state, so an edge's is read as absent
// and the apply leaves the stored list alone rather than replacing it with
// nothing.
func readAt(at applyContext, edges []Edge) []Edge {
	out := make([]Edge, len(edges))
	for i, edge := range edges {
		switch {
		case at.managesStructural():
			out[i] = edge
		case at.exact():
			edge.Manages = nil
			out[i] = edge
		default:
			out[i] = Edge{Object: edge.Object, Parent: edge.Parent, Lead: edge.Lead}
		}
	}
	return out
}

// applyPlacement writes a set of edges as FULL POST-STATE.
//
// An object absent from the set is untouched, which is what makes a placement
// composable with a content record on the same object: neither carries the
// other's half, so neither can revert it.
//
// FALLBACK is the change kind an edge with no verb records — a placement's
// "moved", an import's "imported" — and every edge that states its verb
// records that instead ([changeFor]).
//
// # Renames first, then placements
//
// Every edge carries the object's FINAL state, under the addresses the batch
// ends with — a seat moved into a team the same batch renamed names the new
// key as its parent — so the renames run first and every placement lands on
// the rows they leave. A rename's cascade may move a row an edge then places;
// the edge is the later word and wins, which is why the structural guards are
// open at this position ([cascadeUnits]).
//
// # An edge under a unit this record did not make lands nowhere either
//
// When a create or a rename is declined here, the address it would have given
// a unit is not that unit's — it is nobody's, or it is whatever took it on
// another subject. So an edge placing something under that address is declined
// in turn (reason `parent`) rather than filing the object under a unit its
// batch never meant, and a unit so declined declines its own contents: the
// edges come in the order the batch named them, which puts a container before
// what it holds.
func (a *Applier) applyPlacement(ctx context.Context, tx *sql.Tx, at applyContext,
	edges []Edge, fallback ChangeKind) (int, error) {

	// EVERY VERB IS ONE THIS BUILD APPLIES, checked before any edge is: an
	// edge declined under a unit its record failed to make is counted by
	// its verb, and a verb nobody declared must fail the record there too
	// rather than be counted under a word the counter does not have.
	for _, edge := range edges {
		if err := checkVerb(at, edge); err != nil {
			return 0, err
		}
	}
	rows := 0
	// missing is every unit address an edge of this record meant to make or
	// keep and did not.
	missing := map[string]bool{}
	renamed := make([]bool, len(edges))
	for i, edge := range edges {
		if edge.Op != OpRename {
			continue
		}
		n, landed, err := a.renameEdge(ctx, tx, at, edge)
		if err != nil {
			return 0, err
		}
		rows += n
		renamed[i] = landed
	}
	var first *Edge
	for i, edge := range edges {
		ref := ObjectRef{Kind: edge.Object.Kind, ID: NormalizeKey(edge.Object.ID)}
		landed := false
		switch parent := NormalizeKey(edge.Parent); {
		case edge.Op == OpRename && !renamed[i]:
			// DECLINED ALREADY, by the rename itself: the object is
			// still on the address the batch found it at.
		case at.exact() && parent != "" && missing[parent]:
			// FROM VERSION 2: a version-1 placement filed an edge under
			// whatever address it named, and is read for ever as that.
			a.declineChange(at, edgeOp(edge), ref, &addressRefusal{
				Rule: RuleNoSuchParent, Reason: "parent",
				Detail: fmt.Sprintf("%s is placed under %q, and the change "+
					"this record made to that unit was declined, so the "+
					"address is not the unit its batch meant", ref, parent)})
		default:
			n, placed, err := a.placeEdge(ctx, tx, at, edge, fallback)
			if err != nil {
				return 0, err
			}
			rows += n
			landed = placed
		}
		if !landed && ref.Kind == KindUnit {
			missing[ref.ID] = true
		}
		if landed && first == nil {
			first = &edges[i]
		}
	}
	// THE HISTORY NAMES AN EDGE THAT LANDED, and a record none of whose
	// edges did writes none: a row saying a unit was created, beside a
	// decline saying the create never happened, is a history of something
	// that did not occur. A VERSION-1 RECORD'S NAMES ITS FIRST EDGE, landed
	// or not, which is the row its first apply wrote — `chart_history` is
	// in the identity claim, so a replay writing another would be a second
	// history of one log.
	if !at.exact() && len(edges) > 0 {
		first = &edges[0]
	}
	if first != nil {
		n, err := a.writeHistory(ctx, tx, at, first.Object,
			changeFor(first.Op, fallback))
		if err != nil {
			return 0, err
		}
		rows += n
	}
	return rows, nil
}

// placeEdge applies one edge, by what its batch did to the object.
//
// # Three answers, all read here, where the payload is in hand
//
// The envelope gate cannot answer for a structural record — it names its
// objects inside a payload a gate may not be able to decode — and an import
// decides nothing at its decide, so this is the one place every placement
// passes and every rule a creation is held to is asked again. Each is DECLINED
// rather than raised: see [Applier.declineChange].
//
//   - A CREATE ([OpCreateUnit], [OpCreateSeat]) makes its object, held to
//     every creation's rules ([refuseCreate]); an address the object is
//     ALREADY on is declined too, because applied as a placement it would
//     move whatever holds it. The one exception is the object THIS record
//     created, met again by a redelivery, which is simply already done.
//   - A MOVE, A LEAD CHANGE OR A RENAME places an object its batch found in
//     the chart — a rename's once [Applier.renameEdge] has moved it — and one
//     that is not there any more is declined rather than created.
//   - NO VERB is what every version-1 edge and every import's edge means: the
//     object is created where it is absent and placed where it is not. A
//     version-1 edge's creation is held to the rules its first apply asked —
//     a removed address or another object's identity — and never to the
//     address's shape, which version 2 added ([refuseAddress]).
func (a *Applier) placeEdge(ctx context.Context, tx *sql.Tx, at applyContext,
	edge Edge, fallback ChangeKind) (rows int, landed bool, err error) {

	ref := ObjectRef{Kind: edge.Object.Kind, ID: NormalizeKey(edge.Object.ID)}
	present, through, err := structuralMark(ctx, tx, ref)
	if err != nil {
		return 0, false, err
	}
	switch edge.Op {
	case "":
		if !present {
			if declined, refuseErr := a.declineCreate(ctx, tx, at, edgeOp(edge), ref); refuseErr != nil || declined {
				return 0, false, refuseErr
			}
		}
	case OpCreateUnit, OpCreateSeat:
		if present {
			if through >= at.packed {
				// THIS RECORD'S OWN CREATE, met again: a redelivery,
				// which already landed and writes nothing more.
				return 0, true, nil
			}
			a.declineChange(at, edgeOp(edge), ref, &addressRefusal{
				Rule: RuleKeyTaken, Reason: "present",
				Detail: fmt.Sprintf("%s is already in the chart, so a record "+
					"creating it would move it: the create is declined and "+
					"the object stays where it is", ref)})
			return 0, false, nil
		}
		if declined, refuseErr := a.declineCreate(ctx, tx, at, edgeOp(edge), ref); refuseErr != nil || declined {
			return 0, false, refuseErr
		}
	case OpMove, OpSetLead, OpSetKind, OpSetManages, OpRename:
		// A RENAME'S EDGE IS PLACED ONLY ONCE ITS RENAME LANDED, so its
		// object is on the address it names: its final parent and lead
		// are written exactly as a move's are.
		if !present {
			refused, readErr := absentRefusal(ctx, tx, ref)
			if readErr != nil {
				return 0, false, readErr
			}
			a.declineChange(at, edgeOp(edge), ref, refused)
			return 0, false, nil
		}
	default:
		return 0, false, checkVerb(at, edge)
	}
	n, err := a.placeOne(ctx, tx, at, edge, changeFor(edge.Op, fallback))
	return n, err == nil, err
}

// renameEdge runs one edge's rename, reporting whether the object now answers to
// the address the edge names.
//
// THREE WAYS IT DOES NOT, each declined rather than raised ([Applier.
// declineChange]): the object is not on the address the batch found it at —
// removed, or renamed by a record on another subject the log ordered between —
// or the address it moves onto is one it may not take ([Applier.rekeyRefused]).
// And ONE WAY IT ALREADY DID: a redelivery finds the object on the new address,
// stamped at this very position, and that is this record's own work.
//
// A RENAME THAT LANDS MOVES THE `manages:` ENTRIES NAMING ITS OBJECT too
// ([Applier.moveManages]), which a version-1 rekey — the other caller of the
// same bodies — never did.
func (a *Applier) renameEdge(ctx context.Context, tx *sql.Tx, at applyContext,
	edge Edge) (rows int, landed bool, err error) {

	key := NormalizeKey(edge.Object.ID)
	from := NormalizeKey(edge.From)
	if from == "" || from == key {
		return 0, false, fmt.Errorf("chart: the structural record at %s renames "+
			"%s from %q, and a rename moves an object off one address onto "+
			"another", at.position, edge.Object, from)
	}
	was := ObjectRef{Kind: edge.Object.Kind, ID: from}
	present, _, err := structuralMark(ctx, tx, was)
	if err != nil {
		return 0, false, err
	}
	if !present {
		done, through, markErr := structuralMark(ctx, tx,
			ObjectRef{Kind: edge.Object.Kind, ID: key})
		if markErr != nil {
			return 0, false, markErr
		}
		if done && through >= at.packed {
			return 0, true, nil
		}
		refused, readErr := absentRefusal(ctx, tx, was)
		if readErr != nil {
			return 0, false, readErr
		}
		a.declineChange(at, declinedRename, was, refused)
		return 0, false, nil
	}
	// THE ENTRIES NAMING IT, read before the rename moves anything: which
	// object an address reaches is a question about the rows as they were.
	named, err := managesNaming(ctx, tx, edge.Object.Kind, from)
	if err != nil {
		return 0, false, err
	}
	switch edge.Object.Kind {
	case KindUnit:
		rows, landed, err = a.rekeyUnit(ctx, tx, at, key, from)
	case KindSeat:
		rows, landed, err = a.rekeySeat(ctx, tx, at, key, from)
	default:
		return 0, false, fmt.Errorf("chart: the structural record at %s renames "+
			"a %s, and only a unit and a seat hold an address", at.position,
			edge.Object.Kind)
	}
	if err != nil || !landed {
		return rows, landed, err
	}
	moved, err := a.moveManages(ctx, tx, at, edge.Object.Kind, key, named)
	if err != nil {
		return 0, false, err
	}
	return rows + moved, true, nil
}

// checkVerb fails a record whose edge states a verb this build does not apply.
//
// A VERB THIS BUILD DOES NOT KNOW, on a record at a version it reads, is a
// writer publishing an operation it never declared. Failing is what makes that
// mistake visible, for [Applier.Apply]'s reason.
//
// A SET_MANAGES BELOW VERSION 3 IS ONE OF THOSE: no writer at an earlier
// version declared the verb, since the list was the content record's.
func checkVerb(at applyContext, edge Edge) error {
	switch edge.Op {
	case "", OpCreateUnit, OpCreateSeat, OpMove, OpSetLead, OpSetKind, OpRename:
		return nil
	case OpSetManages:
		if at.managesStructural() {
			return nil
		}
	}
	return fmt.Errorf("chart: the structural record at %s states %s on %s, "+
		"which is not an operation this build applies at version %d",
		at.position, edge.Op, edge.Object, at.record.V)
}

// edgeOp is the word a decline of an edge is counted under, whichever rule
// declined it: `place` for an edge that states no verb, `create` for either
// create — the object's kind is the log line's, not the counter's — and the
// verb itself for the rest.
func edgeOp(edge Edge) declineOp {
	switch edge.Op {
	case "":
		return declinedPlace
	case OpCreateUnit, OpCreateSeat:
		return declinedCreate
	case OpMove:
		return declinedMove
	case OpSetLead:
		return declinedSetLead
	case OpSetKind:
		return declinedSetKind
	case OpSetManages:
		return declinedSetManages
	case OpRename:
		return declinedRename
	}
	// A VERB THIS BUILD DOES NOT KNOW never reaches a decline: [checkVerb]
	// fails its record before any edge is applied.
	return declinedPlace
}

// declineCreate asks every creation's rules of an edge that would create its
// object, and declines it when they refuse. It reports whether it declined.
func (a *Applier) declineCreate(ctx context.Context, tx *sql.Tx, at applyContext,
	op declineOp, ref ObjectRef) (bool, error) {

	refused, err := refuseAddress(ctx, tx, at, ref.Kind, ref.ID, "")
	if err != nil || refused == nil {
		return false, err
	}
	a.declineChange(at, op, ref, refused)
	return true, nil
}

// refuseAddress is why the record at hand may not give key to an object of
// kind, asked under the rules that record was written under: every rule
// ([refuseCreate]) from version 2, and for a version-1 record the ones it met
// when it first applied ([refuseHeld]). SELF is [refuseCreate]'s.
func refuseAddress(ctx context.Context, tx *sql.Tx, at applyContext,
	kind ObjectKind, key, self string) (*addressRefusal, error) {

	if at.exact() {
		return refuseCreate(ctx, txBook{tx: tx}, kind, key, self)
	}
	return refuseHeld(ctx, txBook{tx: tx}, kind, key, self)
}

// absentRefusal is why an object a change named is not in the rows: a removal,
// or nothing this node holds.
func absentRefusal(ctx context.Context, tx *sql.Tx, ref ObjectRef) (*addressRefusal, error) {
	gone, err := objectRemoved(ctx, tx, ref)
	if err != nil {
		return nil, err
	}
	if gone {
		return &addressRefusal{Rule: RuleKeyRemoved, Reason: "removed",
			Detail: fmt.Sprintf("%s was removed by a record the log ordered "+
				"before this one, and a removed object is never written again", ref)}, nil
	}
	return &addressRefusal{Rule: RuleNoSuchObject, Reason: "absent",
		Detail: fmt.Sprintf("%s is not in the chart, and nothing but a create "+
			"makes an object", ref)}, nil
}

// structuralMark reports whether the chart holds an object, and the position
// its structure was last written through.
func structuralMark(ctx context.Context, tx *sql.Tx, ref ObjectRef) (bool, int64, error) {
	var table, column string
	switch ref.Kind {
	case KindUnit:
		table, column = "chart_units", "key"
	case KindSeat:
		table, column = "chart_seats", "handle"
	default:
		return false, 0, nil
	}
	var through int64
	err := tx.QueryRowContext(ctx,
		`SELECT scoped_through FROM `+table+` WHERE `+column+` = ?`, ref.ID).Scan(&through)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, 0, nil
	case err != nil:
		return false, 0, fmt.Errorf("chart: read %s: %w", ref, err)
	}
	return true, through, nil
}

// changeFor is the change kind an edge records: its verb's, or the record's
// own where the edge states none.
func changeFor(op OperationKind, fallback ChangeKind) ChangeKind {
	switch op {
	case OpCreateUnit, OpCreateSeat:
		return ChangeCreated
	case OpMove:
		return ChangeMoved
	case OpSetLead:
		return ChangeLed
	case OpRename:
		return ChangeRekeyed
	case OpSetKind:
		return ChangeKindSet
	case OpSetManages:
		return ChangeManages
	}
	return fallback
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
		// THE KIND WHERE THE EDGE STATES ONE, which every version-2 edge
		// does ([Edge.Kind]); a version-1 edge leaves the row's alone.
		if edge.Kind != "" {
			seat.Kind = edge.Kind
		}
		seat.UnitKey = parent
		seat.UpdatedAt = at.brokerAt
		seat.LastChange = a.change(at, edge.Object, kind)
		n, err := a.writeSeatStructure(ctx, tx, at, seat, found)
		if err != nil {
			return 0, err
		}
		// THE SEAT'S WHOLE `manages:` LIST, FROM VERSION 3 — structure, so
		// the edge states it whole and empty is a seat that manages nobody
		// ([Edge.Manages]). Below it [readAt] has read the list as absent
		// and the rows keep the one a content record wrote.
		edges := 0
		if at.managesStructural() {
			edges, err = replaceEdgeSet(ctx, tx, at, "chart_manages", "manager",
				"target", id, edge.Manages)
			if err != nil {
				return 0, err
			}
		}
		a.note(edge.Object)
		return n + edges, nil
	}
	return 0, fmt.Errorf("chart: the structural record at %s places a %s, and "+
		"only a unit and a seat have a place in the tree",
		at.position, edge.Object.Kind)
}

// writeStructure writes a unit row from a STRUCTURAL record.
//
// `scoped_through` AND NEVER `version`: see this file's header. A row that
// already exists is updated in place under a monotone guard on that column
// alone — a later record's write is never reverted, a redelivered placement
// writes the same values again, and a content record's own expectation is never
// poisoned.
func (a *Applier) writeStructure(ctx context.Context, tx *sql.Tx, at applyContext,
	unit Unit, exists bool) (int, error) {

	if exists {
		return restampUnit(ctx, tx, at, unit)
	}
	document, err := EncodeUnit(unit)
	if err != nil {
		return 0, err
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

	if exists {
		return restampSeat(ctx, tx, at, seat)
	}
	// A NEW SEAT'S KIND IS THE EDGE'S, and `agent` only where a version-1
	// edge stated none: `kind` is NOT NULL, and in that version every seat
	// a chart placed was one the company intended to run until its content
	// record said otherwise — which a version-1 content record still does.
	// In the DOCUMENT as well as the column, which a stub used to leave
	// empty while the column said `agent`.
	if seat.Kind == "" {
		seat.Kind = SeatAgent
	}
	document, err := EncodeSeat(seat)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chart_seats
			(handle, former_keys_json, kind, name, email,
			 backstory, goal, project, space, unit_key, created_at, updated_at,
			 version, scoped_through, document)
		VALUES (?, '[]', ?, '', '', '', '', '', '', ?, ?, ?, 0, ?, ?)
		ON CONFLICT (handle) DO NOTHING`,
		seat.Handle, string(seat.Kind), seat.UnitKey,
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

		// AND THE ADDRESS IT WAS CREATED UNDER, when a rename moved it
		// off that one. The origin is the object's identity (ADR-0026,
		// ADR-0027) and the one address nothing else may ever take — a
		// creation onto it would inherit the removed seat's mailbox, its
		// diary and every person the directory still binds to it — and a
		// tombstone is what every creation path already refuses. Read
		// BEFORE the delete, which is the last moment the row can say it.
		identity, err := a.tombstoneIdentity(ctx, tx, at, object.Kind, id, p.Reason)
		if err != nil {
			return 0, err
		}
		rows += identity

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

// tombstoneIdentity writes a removed object's IDENTITY tombstone beside the
// one on the address it held, when the two differ.
//
// THE SAME RECORD ID as the address's own tombstone, so the removal gate's one
// exception — the record that wrote the tombstone — covers both.
//
// NOT WHILE SOMETHING LIVE ANSWERS TO IT AS ITS KEY. Nothing this build writes
// can put another object on a retired identity ([refuseCreate] refuses it on
// every path), but a tombstone on a live object's key would
// drop every later record on that object's own subject for ever, so the guard
// is the difference between a residue and an object nobody can edit. Every
// node reaches the same answer from the same rows, and a removal is a gate
// record no node defers.
func (a *Applier) tombstoneIdentity(ctx context.Context, tx *sql.Tx,
	at applyContext, kind ObjectKind, id, reason string) (int, error) {

	var identity string
	switch kind {
	case KindUnit:
		unit, found, err := readUnit(ctx, tx, id)
		if err != nil || !found {
			return 0, err
		}
		identity = unit.Origin()
	case KindSeat:
		seat, found, err := readSeat(ctx, tx, id)
		if err != nil || !found {
			return 0, err
		}
		identity = seat.Origin()
	default:
		return 0, nil
	}
	identity = NormalizeKey(identity)
	if identity == "" || identity == id {
		return 0, nil
	}
	live, err := objectPresent(ctx, tx, ObjectRef{Kind: kind, ID: identity})
	if err != nil || live {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chart_removed
			(object_kind, object_id, at, record_id, actor, actor_kind,
			 reason, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (object_kind, object_id) DO NOTHING`,
		string(kind), identity, store.EncodeTime(at.brokerAt),
		at.record.OpID, at.record.Actor, string(at.record.ActorKind),
		reason, at.packed)
	if err != nil {
		return 0, fmt.Errorf("chart: tombstone the identity %s of %s %s at %s: %w",
			identity, kind, id, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// deleteObject removes one object's row and every edge it owns.
//
// THE EDGES IT OWNS, never the edges that name it. A `manages:` entry pointing
// at a removed object names an address a tombstone keeps from ever resolving
// again, and the organisation model reports a dangling reference as a warning
// — which is where somebody decides whether the entry goes or names someone
// else. Deleting it here would take that decision for whoever wrote it, out of
// sight, with no record of which seat managed whom.
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
