package chart

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// THE HISTORY, which is what a card, a digest and an operator asking "when did
// this team change hands" all render from.
//
// ONE ROW PER OPERATION, keyed on the operation id with ON CONFLICT DO NOTHING.
// That is what makes a redelivery free: the row is written once and a second
// delivery of the same record writes nothing, on every node, without any of
// them comparing positions.
//
// IT IS THE ONE PLACE THIS DOMAIN WRITES A ROW THAT NO LATER RECORD CORRECTS.
// Every other table here is a post-state an upsert replaces; a history entry is
// what HAPPENED, so it is create-only and never rewritten. A change that turns
// out to have been wrong is followed by another change, not by an edit to the
// record of the first.

// writeHistory writes one entry for the object a record is about.
//
// A RECORD WITH SEVERAL OBJECTS WRITES ONE ROW, named for the first. That is
// deliberate and is what the op id being the key forces: a row per object would
// need a key per object, and the only thing available to compose one from is
// the object's own id — which makes the row un-repeatable the moment the same
// operation is replayed against a set that has since changed. What a reader
// wants from a reorganisation is the OPERATION, and the objects it moved are in
// the record the feed relays and in each object's own `last_change`.
func (a *Applier) writeHistory(ctx context.Context, tx *sql.Tx, at applyContext,
	object ObjectRef, kind ChangeKind) (int, error) {

	if at.record.OpID == "" {
		// NO OP ID, NO HISTORY ROW. The only record without one is the
		// barrier, which never reaches here — so this is a writer that
		// omitted it, and inventing an id would make the row
		// unrepeatable across a replay.
		return 0, fmt.Errorf("chart: the record at %s writes a history entry "+
			"and carries no operation id, which is what the row is keyed on",
			at.position)
	}
	change := a.change(at, object, kind)
	document, err := EncodeChange(*change)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chart_history
			(id, object_kind, object_id, kind, actor, actor_kind, operator_id,
			 revision, summary, turn_id, quiet, created_at, broker_at, version,
			 document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		change.ID, string(change.Object.Kind), change.Object.ID, string(kind),
		change.Actor, string(change.ActorKind), change.OperatorID,
		change.Revision, change.Summary, change.TurnID, boolInt(change.Quiet),
		store.EncodeTime(at.brokerAt), store.EncodeTime(at.brokerAt),
		at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("chart: write the history entry for %s at %s: %w",
			object, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// change builds the entry a history row and an object's `last_change` both
// carry.
//
// ONE CONSTRUCTOR FOR BOTH, because they are the same value: a card reads the
// object's own row and a feed reads the table, and two constructions of one
// entry is how those two start disagreeing about what happened.
//
// QUIET IS "HAS NO NOTIFY" — a question about the record's own shape rather
// than a flag a writer can forget to set. A quiet commit is a full record,
// arbitrated exactly like a loud one, and writes its history row like every
// other: quiet means it wakes nobody, and nothing else.
func (a *Applier) change(at applyContext, object ObjectRef, kind ChangeKind) *Change {
	change := &Change{
		V: DocumentVersion, ID: at.record.OpID, Kind: kind,
		Object:     ObjectRef{Kind: object.Kind, ID: NormalizeKey(object.ID)},
		Actor:      at.record.Actor,
		ActorKind:  at.record.ActorKind,
		OperatorID: at.record.OperatorID,
		Revision:   at.record.Revision,
		TurnID:     at.record.TurnID,
		Quiet:      at.record.Notify == nil,
		CreatedAt:  at.brokerAt,
	}
	if at.record.Notify != nil {
		change.Summary = at.record.Notify.Summary
	}
	return change
}

// boolInt is a flag as the column stores it.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
