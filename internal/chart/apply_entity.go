package chart

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/store"
)

// THE OBJECT ROWS, and the one rule that governs all of them.
//
// A unit and a seat each have TWO WRITERS on two different subjects: the
// object's own content on [KindUnit] or [KindSeat], and its placement on
// [KindTree]. They arbitrate separately, land in either order, and both write
// the same row.
//
// So every apply here READS THE ROW, CHANGES ONLY ITS OWN HALF, AND WRITES THE
// WHOLE DOCUMENT BACK. A content apply that wrote `parent_key` would reparent
// an object from a record that never mentioned the tree; a structural apply
// that wrote `name` would revert a rename from a record that never carried one.
// The DOCUMENT COLUMN is the authored entity and the typed columns are a
// projection rewritten from it on every apply — so a field this build does not
// know survives in the blob, and no reader ever sees a column and a document
// that disagree.
//
// THE VERSION GUARD IS WHAT MAKES THAT SAFE UNDER REDELIVERY. Both writers
// compare `excluded.version > version`, so the newest record wins whichever
// arrived last, and a redelivery of an older one writes nothing rather than
// resurrecting the half it carried.

// applyUnit writes one unit's own CONTENT.
//
// It never touches `parent_key` or `lead`: both are structure, and a content
// record that could move a unit would be a second writer of the tree contending
// with nobody. See [applyPlacement] for the other half.
func (a *Applier) applyUnit(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	content, ok := payload.(UnitPayload)
	if !ok {
		return 0, fmt.Errorf("chart: the record at %s is a unit upsert and "+
			"carries a %T payload", at.position, payload)
	}
	// THE APPLIER RECOMPUTES THE SUBJECT FROM THE PAYLOAD AND REFUSES A
	// MISMATCH. The broker arbitrated this record on the subject, so a
	// record that took one address and wrote another would have contended
	// with the wrong writers — and it is the one error here worth failing
	// the batch for, because every node reaches it identically and the
	// record is malformed rather than merely unreadable.
	key := NormalizeKey(content.Key)
	if key != at.subject().ID {
		return 0, fmt.Errorf("chart: the record at %s arbitrated on unit %q "+
			"and its payload claims %q — a record takes one address and it is "+
			"the one it contended for", at.position, at.subject().ID, key)
	}

	unit, found, err := readUnit(ctx, tx, key)
	if err != nil {
		return 0, err
	}
	if !found {
		unit = Unit{V: DocumentVersion, Key: key, CreatedAt: at.brokerAt}
	}
	// THE CONTENT HALF ONLY. Everything the structure owns — ParentKey,
	// Lead, FormerKeys — is carried over from the row untouched.
	unit.V = DocumentVersion
	unit.Name = content.Name
	unit.Type = content.Type
	unit.Purpose = content.Purpose
	unit.Goals = content.Goals
	unit.Channel = content.Channel
	unit.Project = content.Project
	unit.Space = content.Space
	unit.KnowledgeRefs = content.KnowledgeRefs
	// Whole, for [Applier.applySeat]'s reason.
	unit.Runtime = content.Runtime
	unit.UpdatedAt = at.brokerAt
	unit.LastChange = a.change(at, ObjectRef{Kind: KindUnit, ID: key},
		ChangeEdited)

	rows, err := writeUnit(ctx, tx, at, unit)
	if err != nil {
		return 0, err
	}
	n, err := a.writeHistory(ctx, tx, at, ObjectRef{Kind: KindUnit, ID: key},
		ChangeEdited)
	if err != nil {
		return 0, err
	}
	a.note(ObjectRef{Kind: KindUnit, ID: key})
	return rows + n, nil
}

// applySeat writes one seat's own CONTENT. It never touches `unit_key`.
func (a *Applier) applySeat(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	content, ok := payload.(SeatPayload)
	if !ok {
		return 0, fmt.Errorf("chart: the record at %s is a seat upsert and "+
			"carries a %T payload", at.position, payload)
	}
	handle := NormalizeKey(content.Handle)
	if handle != at.subject().ID {
		return 0, fmt.Errorf("chart: the record at %s arbitrated on seat %q "+
			"and its payload claims %q — a record takes one address and it is "+
			"the one it contended for", at.position, at.subject().ID, handle)
	}

	seat, found, err := readSeat(ctx, tx, handle)
	if err != nil {
		return 0, err
	}
	if !found {
		seat = Seat{V: DocumentVersion, Handle: handle, CreatedAt: at.brokerAt}
	}
	seat.V = DocumentVersion
	seat.Kind = content.Kind
	seat.Name = content.Name
	seat.Email = content.Email
	seat.Backstory = content.Backstory
	seat.Goal = content.Goal
	seat.Responsibilities = content.Responsibilities
	seat.BehavioralGuidelines = content.BehavioralGuidelines
	seat.Project = content.Project
	seat.Space = content.Space
	// THE WHOLE RUNTIME DOCUMENT, replaced rather than merged: it is one
	// value the writer holds whole, and a merge here would make a field
	// somebody deleted survive on whichever node applied an older record
	// last.
	seat.Runtime = content.Runtime
	seat.UpdatedAt = at.brokerAt
	seat.LastChange = a.change(at, ObjectRef{Kind: KindSeat, ID: handle},
		ChangeEdited)

	rows, err := writeSeat(ctx, tx, at, seat)
	if err != nil {
		return 0, err
	}
	// THE AUTHORED EDGE, written from the CONTENT record because that is
	// where `manages:` is authored — a seat states what it manages, and the
	// other end is derived by the index over `target`.
	edges, err := replaceEdgeSet(ctx, tx, at, "chart_manages", "manager",
		"target", handle, content.Manages)
	if err != nil {
		return 0, err
	}
	n, err := a.writeHistory(ctx, tx, at, ObjectRef{Kind: KindSeat, ID: handle},
		ChangeEdited)
	if err != nil {
		return 0, err
	}
	a.note(ObjectRef{Kind: KindSeat, ID: handle})
	return rows + edges + n, nil
}

// applyRekey moves one key onto one object and retires the old one.
//
// THE FORMER KEY GOES ON RESOLVING, in `former_keys_json`, until something else
// claims it: a key is pasted into chat and typed into `manages:` entries, so a
// rename that stopped the old one resolving would break every reference
// anybody had already written.
//
// IT STAMPS `scoped_through` AND NEVER `version`, for the reason a page's
// rename does: the record arbitrated on the KEY's own subject, not on the
// object's, so writing the object's version from here would set an expectation
// no writer on that object's subject can ever satisfy — and the object becomes
// permanently unwritable. A read barrier compares the max of the two.
func (a *Applier) applyRekey(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	move, ok := payload.(RekeyPayload)
	if !ok {
		return 0, fmt.Errorf("chart: the record at %s is a rekey and carries "+
			"a %T payload", at.position, payload)
	}
	key := NormalizeKey(move.Key)
	if key != at.subject().ID {
		return 0, fmt.Errorf("chart: the record at %s arbitrated on the key %q "+
			"and its payload claims %q — a claim takes one address and it is "+
			"the one it contended for", at.position, at.subject().ID, key)
	}
	former := NormalizeKey(move.FormerKey)
	if former == "" || former == key {
		return 0, fmt.Errorf("chart: the rekey at %s retires %q in favour of "+
			"%q — a claim that retires nothing, or itself, moves no reference "+
			"and leaves two addresses that both resolve", at.position, former, key)
	}

	switch move.Object.Kind {
	case KindUnit:
		return a.rekeyUnit(ctx, tx, at, key, former)
	case KindSeat:
		return a.rekeySeat(ctx, tx, at, key, former)
	}
	return 0, fmt.Errorf("chart: the rekey at %s moves a key onto a %s, and "+
		"only a unit and a seat hold one", at.position, move.Object.Kind)
}

func (a *Applier) rekeyUnit(ctx context.Context, tx *sql.Tx, at applyContext,
	key, former string) (int, error) {

	unit, found, err := readUnit(ctx, tx, former)
	if err != nil {
		return 0, err
	}
	if !found {
		// NOT AN ERROR. The object may have been removed, or this may be
		// a redelivery arriving after the move already landed — and both
		// are ordinary traffic on a log. Failing here would stall the
		// whole fleet on a record every node reaches identically and
		// none of them can act on.
		return 0, nil
	}
	// AND THE ADDRESS IS STILL FREE, asked HERE and not only at the decide.
	//
	// DECLINED, NOT FAILED, which is the whole point of asking again: the key
	// is this table's PRIMARY KEY, so a claim that reaches the UPDATE against
	// a row holding it raises `UNIQUE constraint failed` — and an apply error
	// is not one node's problem. Every node reads the same record, fails the
	// same way and cannot get past it, so one lost rename would take the
	// chart domain down across the fleet. The decide refuses this case where
	// an operator can be told ([Writer.WriteRekey]); it cannot refuse ALL of
	// it, because a create arbitrates on a different subject from a claim, so
	// the log may legally order a create of this address after the claim on
	// it was decided.
	//
	// Every node reaches the same verdict from the same rows, which is what
	// keeps the copies identical — the same reason [Applier.rekeyUnit]'s
	// absent-object case above returns nothing rather than raising.
	holder, held, err := addressHolder(ctx, tx, KindUnit, key)
	if err != nil {
		return 0, err
	}
	if held && holder != former {
		return 0, nil
	}
	// THE ORIGIN IS FROZEN BY THE FIRST REKEY AND NEVER AGAIN, which is the
	// one moment the create address is still known: a unit that has been
	// rekeyed before already carries it, and one that has not is answering
	// to the key it was created under right now. See [Unit.OriginKey].
	if unit.OriginKey == "" {
		unit.OriginKey = former
	}
	unit.Key = key
	unit.FormerKeys = retire(unit.FormerKeys, former)
	unit.UpdatedAt = at.brokerAt
	unit.LastChange = a.change(at, ObjectRef{Kind: KindUnit, ID: key}, ChangeRekeyed)
	document, err := EncodeUnit(unit)
	if err != nil {
		return 0, err
	}
	formerJSON, err := json.Marshal(unit.FormerKeys)
	if err != nil {
		return 0, fmt.Errorf("chart: encode the retired keys of %s: %w", key, err)
	}
	// ONE STATEMENT, and it is an UPDATE of the primary key rather than an
	// insert-and-delete: the two would leave the object absent between
	// them, and every foreign reference this domain keeps — the edges, the
	// children, the seats — is by key.
	res, err := tx.ExecContext(ctx, `
		UPDATE chart_units
		SET key = ?, former_keys_json = ?, updated_at = ?,
		    scoped_through = MAX(scoped_through, ?), document = ?
		WHERE key = ? AND scoped_through < ?`,
		key, string(formerJSON), store.EncodeTime(at.brokerAt), at.packed,
		document, former, at.packed)
	if err != nil {
		return 0, fmt.Errorf("chart: rekey unit %s to %s at %s: %w",
			former, key, at.position, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, nil
	}
	// EVERY REFERENCE BY KEY MOVES WITH IT, in the same transaction, so no
	// read ever sees a child pointing at a key nothing answers to.
	moved, err := a.moveUnitReferences(ctx, tx, at, key, former)
	if err != nil {
		return 0, err
	}
	rows, err := a.writeHistory(ctx, tx, at, ObjectRef{Kind: KindUnit, ID: key},
		ChangeRekeyed)
	if err != nil {
		return 0, err
	}
	a.note(ObjectRef{Kind: KindUnit, ID: key})
	return int(n) + moved + rows, nil
}

// moveUnitReferences repoints everything that names a unit by its key.
//
// THE AUTHORED EDGES ARE NOT AMONG THEM, deliberately. A `manages:` entry is
// what somebody WROTE, and a retired key goes on resolving — so rewriting the
// entry would edit a document nobody edited, and the next config apply would
// write the old spelling straight back. What moves here is the STRUCTURE: a
// child's parent and a seat's unit, neither of which anybody authored as text.
func (a *Applier) moveUnitReferences(ctx context.Context, tx *sql.Tx,
	at applyContext, key, former string) (int, error) {

	children, err := tx.ExecContext(ctx, `
		UPDATE chart_units SET parent_key = ?, scoped_through = MAX(scoped_through, ?)
		WHERE parent_key = ?`, key, at.packed, former)
	if err != nil {
		return 0, fmt.Errorf("chart: move the children of %s to %s at %s: %w",
			former, key, at.position, err)
	}
	seats, err := tx.ExecContext(ctx, `
		UPDATE chart_seats SET unit_key = ?, scoped_through = MAX(scoped_through, ?)
		WHERE unit_key = ?`, key, at.packed, former)
	if err != nil {
		return 0, fmt.Errorf("chart: move the seats of %s to %s at %s: %w",
			former, key, at.position, err)
	}
	leads, err := tx.ExecContext(ctx,
		`UPDATE chart_leads SET unit_key = ? WHERE unit_key = ?`, key, former)
	if err != nil {
		return 0, fmt.Errorf("chart: move the lead edge of %s to %s at %s: %w",
			former, key, at.position, err)
	}
	c, _ := children.RowsAffected()
	s, _ := seats.RowsAffected()
	l, _ := leads.RowsAffected()
	return int(c + s + l), nil
}

func (a *Applier) rekeySeat(ctx context.Context, tx *sql.Tx, at applyContext,
	handle, former string) (int, error) {

	seat, found, err := readSeat(ctx, tx, former)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	// AND THE HANDLE IS STILL FREE, declined rather than raised, for the
	// reason [Applier.rekeyUnit] gives at the same point: `handle` is this
	// table's PRIMARY KEY, and an apply that raises is a record every node
	// fails on identically and for ever.
	holder, held, err := addressHolder(ctx, tx, KindSeat, handle)
	if err != nil {
		return 0, err
	}
	if held && holder != former {
		return 0, nil
	}
	// FROZEN BY THE FIRST REKEY, for the reason [Applier.rekeyUnit] gives —
	// and here it is what keeps the seat's mailbox, lease, diary and
	// schedule ledger, all of which key on the id derived from it.
	if seat.OriginHandle == "" {
		seat.OriginHandle = former
	}
	seat.Handle = handle
	seat.FormerHandles = retire(seat.FormerHandles, former)
	seat.UpdatedAt = at.brokerAt
	seat.LastChange = a.change(at, ObjectRef{Kind: KindSeat, ID: handle}, ChangeRekeyed)
	document, err := EncodeSeat(seat)
	if err != nil {
		return 0, err
	}
	formerJSON, err := json.Marshal(seat.FormerHandles)
	if err != nil {
		return 0, fmt.Errorf("chart: encode the retired handles of %s: %w", handle, err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE chart_seats
		SET handle = ?, former_keys_json = ?, updated_at = ?,
		    scoped_through = MAX(scoped_through, ?), document = ?
		WHERE handle = ? AND scoped_through < ?`,
		handle, string(formerJSON), store.EncodeTime(at.brokerAt), at.packed,
		document, former, at.packed)
	if err != nil {
		return 0, fmt.Errorf("chart: rekey seat %s to %s at %s: %w",
			former, handle, at.position, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, nil
	}
	// THE AUTHORED EDGE'S OWNER MOVES, which is not the same as rewriting
	// what somebody wrote: the `manages:` list stays as authored, and what
	// changes is which seat is recorded as having authored it.
	manages, err := tx.ExecContext(ctx,
		`UPDATE chart_manages SET manager = ? WHERE manager = ?`, handle, former)
	if err != nil {
		return 0, fmt.Errorf("chart: move the manages edges of %s to %s at "+
			"%s: %w", former, handle, at.position, err)
	}
	leads, err := tx.ExecContext(ctx,
		`UPDATE chart_leads SET handle = ? WHERE handle = ?`, handle, former)
	if err != nil {
		return 0, fmt.Errorf("chart: move the lead edges of %s to %s at %s: %w",
			former, handle, at.position, err)
	}
	units, err := tx.ExecContext(ctx, `
		UPDATE chart_units SET lead = ?, scoped_through = MAX(scoped_through, ?)
		WHERE lead = ?`, handle, at.packed, former)
	if err != nil {
		return 0, fmt.Errorf("chart: move the led units of %s to %s at %s: %w",
			former, handle, at.position, err)
	}
	m, _ := manages.RowsAffected()
	l, _ := leads.RowsAffected()
	u, _ := units.RowsAffected()
	rows, err := a.writeHistory(ctx, tx, at, ObjectRef{Kind: KindSeat, ID: handle},
		ChangeRekeyed)
	if err != nil {
		return 0, err
	}
	a.note(ObjectRef{Kind: KindSeat, ID: handle})
	return int(n+m+l+u) + rows, nil
}

// retire puts a former key at the FRONT of the list and caps it.
//
// NEWEST FIRST AND THE OLDEST DROPPED, because the reference somebody typed
// last week is the one still being followed, and an object that has been
// renamed [MaxFormerKeys] times has a first name nobody remembers.
func retire(keys []string, former string) []string {
	out := make([]string, 0, len(keys)+1)
	out = append(out, former)
	for _, key := range keys {
		if key == former {
			continue
		}
		out = append(out, key)
	}
	if len(out) > MaxFormerKeys {
		out = out[:MaxFormerKeys]
	}
	return out
}

// --- the rows -------------------------------------------------------------- //

// readUnit reads one unit's stored document out of this transaction.
func readUnit(ctx context.Context, tx *sql.Tx, key string) (Unit, bool, error) {
	var document []byte
	err := tx.QueryRowContext(ctx,
		`SELECT document FROM chart_units WHERE key = ?`, key).Scan(&document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Unit{}, false, nil
	case err != nil:
		return Unit{}, false, fmt.Errorf("chart: read unit %s: %w", key, err)
	}
	unit, err := DecodeUnit(document)
	if err != nil {
		return Unit{}, false, fmt.Errorf("chart: decode unit %s: %w", key, err)
	}
	return unit, true, nil
}

// readSeat reads one seat's stored document out of this transaction.
func readSeat(ctx context.Context, tx *sql.Tx, handle string) (Seat, bool, error) {
	var document []byte
	err := tx.QueryRowContext(ctx,
		`SELECT document FROM chart_seats WHERE handle = ?`, handle).Scan(&document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Seat{}, false, nil
	case err != nil:
		return Seat{}, false, fmt.Errorf("chart: read seat %s: %w", handle, err)
	}
	seat, err := DecodeSeat(document)
	if err != nil {
		return Seat{}, false, fmt.Errorf("chart: decode seat %s: %w", handle, err)
	}
	return seat, true, nil
}

// writeUnit upserts one unit's row from its document.
//
// THE TYPED COLUMNS ARE A PROJECTION of the blob, rewritten whole on every
// apply: they exist so a query can filter and join without decoding every row,
// and the blob is what a field this build does not know survives in. Deriving
// them here, from the value that is about to be stored, is what makes it
// impossible for a reader to see a column and a document that disagree.
func writeUnit(ctx context.Context, tx *sql.Tx, at applyContext, unit Unit) (int, error) {
	document, err := EncodeUnit(unit)
	if err != nil {
		return 0, err
	}
	formerJSON, err := json.Marshal(nonNil(unit.FormerKeys))
	if err != nil {
		return 0, fmt.Errorf("chart: encode the retired keys of %s: %w", unit.Key, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chart_units
			(key, former_keys_json, name, type, purpose, channel, project,
			 space, parent_key, lead, created_at, updated_at, version,
			 scoped_through, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT (key) DO UPDATE SET
			former_keys_json = excluded.former_keys_json,
			name = excluded.name, type = excluded.type,
			purpose = excluded.purpose, channel = excluded.channel,
			project = excluded.project, space = excluded.space,
			parent_key = excluded.parent_key, lead = excluded.lead,
			updated_at = excluded.updated_at, version = excluded.version,
			document = excluded.document
		WHERE excluded.version > chart_units.version`,
		unit.Key, string(formerJSON), unit.Name, unit.Type, unit.Purpose,
		unit.Channel, unit.Project, unit.Space, unit.ParentKey, unit.Lead,
		store.EncodeTime(unit.CreatedAt), store.EncodeTime(unit.UpdatedAt),
		at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("chart: write unit %s at %s: %w",
			unit.Key, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// writeSeat upserts one seat's row from its document.
func writeSeat(ctx context.Context, tx *sql.Tx, at applyContext, seat Seat) (int, error) {
	document, err := EncodeSeat(seat)
	if err != nil {
		return 0, err
	}
	formerJSON, err := json.Marshal(nonNil(seat.FormerHandles))
	if err != nil {
		return 0, fmt.Errorf("chart: encode the retired handles of %s: %w",
			seat.Handle, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chart_seats
			(handle, former_keys_json, kind, name, email, email_index,
			 backstory, goal, project, space, unit_key, created_at, updated_at,
			 version, scoped_through, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT (handle) DO UPDATE SET
			former_keys_json = excluded.former_keys_json,
			kind = excluded.kind, name = excluded.name, email = excluded.email,
			email_index = excluded.email_index, backstory = excluded.backstory,
			goal = excluded.goal, project = excluded.project,
			space = excluded.space, unit_key = excluded.unit_key,
			updated_at = excluded.updated_at, version = excluded.version,
			document = excluded.document
		WHERE excluded.version > chart_seats.version`,
		seat.Handle, string(formerJSON), string(seat.Kind), seat.Name,
		seat.Email, iam.NormalizeEmail(seat.Email), seat.Backstory, seat.Goal,
		seat.Project, seat.Space, seat.UnitKey,
		store.EncodeTime(seat.CreatedAt), store.EncodeTime(seat.UpdatedAt),
		at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("chart: write seat %s at %s: %w",
			seat.Handle, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// replaceEdgeSet writes one owner's whole authored edge set, DELETE then
// INSERT, SORTED.
//
// SORTED is not tidiness. A collection written from a map in whatever order the
// runtime produced lands differently on every node, and this domain claims
// every replicated table is byte-identical — so the file's checksum diverges
// and the claim is false, silently, in a table nothing else compares.
//
// DELETE-THEN-INSERT rather than a diff, because the record carries the
// COMPLETE post-state: computing which entries to remove would be re-deriving,
// from two values, a set the writer already stated.
func replaceEdgeSet(ctx context.Context, tx *sql.Tx, at applyContext,
	table, owner, column, id string, values []string) (int, error) {

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+table+` WHERE `+owner+` = ?`, id); err != nil {
		return 0, fmt.Errorf("chart: clear %s for %s at %s: %w",
			table, id, at.position, err)
	}
	set := sortedKeys(values)
	rows, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO `+table+` (`+owner+`, `+column+`, version) VALUES`,
		`(?, ?, ?)`, `ON CONFLICT DO NOTHING`,
		len(set), func(i int) []any { return []any{id, set[i], at.packed} })
	if err != nil {
		return 0, fmt.Errorf("chart: write %s for %s at %s: %w",
			table, id, at.position, err)
	}
	return rows, nil
}

// sortedKeys is a folded, de-duplicated copy in a deterministic order.
//
// FOLDED HERE rather than by the writer, because an authored `manages:` entry
// is what a founder typed and the row is an ADDRESS: `Platform` and `platform`
// are one unit, and storing both would make the inverse index answer one seat
// twice for one edge.
//
// SORTED FOR THE LAYER UNDERNEATH THE ROWS, which is why it survives a table
// whose primary key already makes every READ order-independent: a b-tree built
// by inserting one key set in two different orders splits its pages at
// different points, so two nodes that reached the same rows by different routes
// hold the same logical table and different BYTES. This domain claims those
// bytes are identical, and nothing else in the tree compares them — so the
// cheapest place to make the claim true is here, before the insert.
func sortedKeys(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		key := NormalizeKey(v)
		if key == "" || slices.Contains(out, key) {
			continue
		}
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

// nonNil is an empty slice where a nil one would encode as `null`.
//
// The column's default is `'[]'` and a row written with `null` would decode as
// an absent list on the way back out — so two nodes that reached the same state
// by different routes would hold two different bytes in a table that claims
// identity.
func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
