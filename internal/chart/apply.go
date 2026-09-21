package chart

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier, and the four rules that make it a PURE FUNCTION of the log.
//
// N nodes derive one SQL state from one ordered stream, and this domain CLAIMS
// IDENTITY: the claim that their tables are byte-identical is checkable, by a
// checksum over the replicated file. It survives exactly as long as these four
// hold, and they are the tracker's and the knowledge base's four because they
// are properties of the framework rather than of any one domain:
//
//  1. IT READS NO CONFIG AND NO CLOCK. Everything it would otherwise read
//     arrives in [statelog.ApplyOptions]: the batch's instant, the broker's own
//     timestamp, and the epoch values the domain declared it reads.
//  2. IT WRITES ONLY WHAT THE RECORD SAYS. No lookup against a live counter, no
//     "current" anything — the record carries its complete new values, which is
//     what makes a replay from zero produce the rows the original apply did.
//  3. EVERY DERIVED INSTANT IS A MAX OVER A SET, never a fold over an arrival
//     order. Two nodes at one checkpoint have seen the same set in a different
//     order, so a fold gives them different answers.
//  4. EVERY UPSERT CARRIES ITS VERSION GUARD, and the guard is a SKIP rather
//     than an error: a redelivered record is ordinary traffic, and a constraint
//     violation inside this transaction would abort it identically on every
//     node and stall the whole fleet's log.
//
// # The two things it does that neither sibling does
//
// TWO SUBJECTS MEET ON ONE ROW. A unit's content arrives on [KindUnit] and its
// placement on [KindTree], in two records that arbitrate separately and both
// write `chart_units`. So a content apply must never touch `parent_key` or
// `lead`, and a structural apply must never touch the content columns — each
// reads the row, changes its own half, and writes the whole document back. The
// version guard is what keeps that safe under redelivery, and `scoped_through`
// is what keeps a structural write from poisoning the row's own expectation:
// see [applyPlacement].
//
// STRUCTURE WINS OVER CONTENT, and only in one direction. A content record for
// an object a removal has already destroyed must write nothing, for ever —
// which is what `chart_removed` is, and why the deletion gate reads it before
// any kind rule runs. The inverse is not true and must not be: a structural
// record about an object whose content has not arrived yet writes the
// placement anyway, because an import publishes the structure first and the
// content follows on each object's own subject.

// Applier writes this node's copy of the org chart.
type Applier struct {
	// NodeID is this node's own id, which the eviction gate compares a
	// record's writer against.
	NodeID string

	// touched is the set of objects a committed batch changed, drained by
	// [Applier.Committed].
	//
	// # Why the applier is what notices
	//
	// The company view every turn reads is DERIVED from these rows —
	// normalised, with lead inheritance and manages-expansion applied — and
	// something has to say when to derive it again. The change feed is
	// deliberately not that something: it relays a record to ONE node, so
	// the other nodes' views would go on serving a chart they had already
	// applied and could not see they had.
	//
	// The apply is the other thing that sees every change, and it sees it
	// on every node. Nil is legal and means nobody is listening.
	touched func([]ObjectRef)

	// changed is filled inside Apply and drained by Committed.
	//
	// NOT GUARDED BY A MUTEX, and the framework's own contract is why: an
	// applier is ONE writer, and Apply and Committed are called from the
	// same goroutine with the commit in between. It is a slice rather than
	// a flag because the rebuild is per object — a company of five hundred
	// seats should not re-derive every one of them because one goal was
	// reworded.
	changed []ObjectRef
}

// NewApplier builds the chart's applier for one node.
//
// onChange is called after a COMMITTED batch, with every object it wrote.
// After the commit and never inside the transaction: the store re-runs the
// body of an attempt that failed transiently, so a callback inside Apply would
// fire twice for one record — and a view rebuilt from a transaction that then
// rolled back is a view of rows no node holds.
func NewApplier(nodeID string, onChange func([]ObjectRef)) *Applier {
	return &Applier{NodeID: nodeID, touched: onChange}
}

// Committed is the post-commit half.
//
// The one consequence of a chart record that is not a row: the derived company
// view is stale, and whoever holds one has to rebuild it. Every OTHER
// consequence is a row, and a wake is derived by the change feed — something
// that outlives this process — rather than published here as a courtesy.
func (a *Applier) Committed(context.Context) {
	if len(a.changed) == 0 {
		return
	}
	changed := a.changed
	// RESET BEFORE THE CALLBACK, not after. The callback may panic or may
	// take long enough for the next batch to be waiting behind it, and a
	// set drained afterwards would either be lost or delivered twice.
	a.changed = nil
	if a.touched != nil {
		a.touched(changed)
	}
}

// Gated reports a record that must produce no rows at all.
//
// TWO GATES, READ FROM THIS SAME TRANSACTION, because the answer has to come
// from the state the record would have applied against rather than from a cache
// that may be a heartbeat old:
//
//  1. THE EVICTION GATE. A record written by a node the fleet evicted before
//     the record's own position is dropped everywhere. It depends on nothing
//     but the log's own order, which is what makes it the fence that holds when
//     coordination cannot be reached at all.
//  2. THE REMOVAL GATE. A record about an object a removal destroyed applies
//     nowhere, for ever. Without it a redelivery months later would write back
//     a seat the company deliberately dissolved — on one node only, in a table
//     that claims identity.
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (
	statelog.Reason, bool, error) {

	if rec.Writer != "" {
		var from, readmitted sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position
			FROM chart_evictions WHERE node_id = ?`, rec.Writer).
			Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return "", false, fmt.Errorf("chart: read the eviction gate for "+
				"node %s: %w", rec.Writer, err)
		default:
			at := rec.Position.Packed()
			// THE WINDOW IS HALF-OPEN AT BOTH ENDS, and both ends
			// matter: a record at or below the eviction's own position
			// was written while the node was still counted, and one at
			// or above a readmission is written by a node the fleet
			// has taken back.
			evicted := from.Valid && at > from.Int64
			back := readmitted.Valid && at >= readmitted.Int64
			if evicted && !back {
				return statelog.ReasonEvicted, true, nil
			}
		}
	}

	// The removal gate reads the object's own tombstone, and it is answered
	// from the ENVELOPE alone — a record at an unknown version reaches here
	// with an opaque payload, so a gate that had to read one could not run
	// at the moment it matters most.
	ref, ok := gatedObject(rec)
	if !ok {
		return "", false, nil
	}
	var author sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT record_id FROM chart_removed
		WHERE object_kind = ? AND object_id = ?`,
		string(ref.Kind), ref.ID).Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("chart: read the removal gate for %s: %w",
			ref, err)
	}
	// THE ONE EXCEPTION IS THE RECORD THAT WROTE THE TOMBSTONE, by its own
	// op id — not by its op kind. "Any removal" would let a SECOND removal
	// of one object through, and by the committed position would fail for a
	// republished copy, leaving a node holding only that copy unable to
	// write its own tombstone at all.
	if author.Valid && author.String == rec.OpID {
		return "", false, nil
	}
	return statelog.ReasonDeleted, true, nil
}

// gatedObject is the object a record is ABOUT, for the removal gate.
//
// ONLY A CONTENT SUBJECT NAMES ONE. A unit's and a seat's own subject IS the
// object, so the gate reads it straight off the envelope. Every other kind
// names its objects inside a payload the gate may not be able to decode —
// a structural record, an import, a removal itself — so those are gated on
// nothing here, and each apply refuses per object instead: see
// [removedObjects].
//
// That split is what keeps the gate answerable from the envelope while still
// making a removal permanent.
func gatedObject(rec statelog.Record) (ObjectRef, bool) {
	switch ObjectKind(rec.Subject.Kind) {
	case KindUnit:
		return ObjectRef{Kind: KindUnit, ID: rec.Subject.ID}, true
	case KindSeat:
		return ObjectRef{Kind: KindSeat, ID: rec.Subject.ID}, true
	}
	return ObjectRef{}, false
}

// Apply writes one record's rows.
//
// The dispatch is on the SUBJECT KIND, and every kind has a case — including
// the one that writes nothing, which is a case rather than a default so that a
// kind added later without one is a compile-time hole rather than a silently
// ignored record.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("chart: decode the record at %s: %w", rec.Position, err)
	}
	at := applyContext{
		record:       record,
		position:     rec.Position,
		packed:       rec.Position.Packed(),
		brokerAt:     opts.StoredAt,
		maxVariables: opts.MaxVariables,
	}

	switch ObjectKind(rec.Subject.Kind) {
	case KindBarrier:
		// THE READ INDEX'S OWN APPEND. It writes no row on any node, and
		// that is its entire content: it exists so a linearizable read
		// has a quorum-committed position to wait through.
		return 0, nil
	case KindEviction:
		return a.applyEviction(ctx, tx, at)
	case KindGeneration:
		return a.applyGeneration(ctx, tx, at)
	case KindTree:
		return a.applyTree(ctx, tx, at)
	case KindUnit:
		return a.applyUnit(ctx, tx, at)
	case KindSeat:
		return a.applySeat(ctx, tx, at)
	case KindRekey:
		return a.applyRekey(ctx, tx, at)
	}
	// A KIND THIS BUILD DOES NOT KNOW REACHES HERE ONLY BY WAY OF A RECORD
	// AT A VERSION IT CAN READ, which is a writer publishing a kind it
	// never declared. Retaining it would file it under a kind nothing will
	// ever apply; failing is what makes the writer's mistake visible.
	return 0, fmt.Errorf("chart: %s is not a kind this build applies, and the "+
		"record at %s claims version %d — a kind is declared before it is "+
		"published", rec.Subject.Kind, rec.Position, record.V)
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position

	// packed is the composed position, which is the version every object
	// row's guard compares against.
	packed int64

	// brokerAt is the broker's own instant for this record. THE APPLIER
	// NEVER READS A CLOCK: this is what makes an effective instant
	// byte-identical on every node.
	//
	// It is also the ONLY instant this domain writes, which is why the
	// record's own authored one reaches no row here. It rides the record so
	// an operator can see when a reorganisation was DECIDED, and ordering
	// on it would let a skewed node file a change before one that provably
	// preceded it on the log.
	brokerAt time.Time

	// maxVariables is the engine's probed bind-parameter limit, carried
	// here because it is what sizes a multi-row INSERT and an applier holds
	// a transaction and nothing else.
	//
	// IT IS NOT AN INPUT TO WHAT THE ROWS SAY: it decides how many
	// statements a collection is written in, never which rows land or in
	// what order, so two nodes probing different limits still write
	// byte-identical tables. A ZERO is an unset field rather than a limit —
	// [store.RowsPerInsert] falls to one row per statement.
	maxVariables int
}

// subject is the record's own subject.
func (c applyContext) subject() Subject { return c.record.Subject }

// applyEviction records a node's removal from this log, or its readmission.
//
// A READMISSION IS AN INVERSE COMMIT rather than a delete, so an eviction's
// whole history survives a replay — and a node that was evicted, readmitted and
// evicted again reads correctly rather than as one long absence.
func (a *Applier) applyEviction(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	gate, ok := payload.(Eviction)
	if !ok {
		return 0, fmt.Errorf("chart: the record at %s is an eviction and "+
			"carries a %T payload", at.position, payload)
	}
	if gate.NodeID == "" {
		return 0, fmt.Errorf("chart: the eviction at %s names no node, so "+
			"there is nothing for the gate to drop", at.position)
	}
	if gate.Readmit {
		res, err := tx.ExecContext(ctx, `
			UPDATE chart_evictions
			SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`,
			at.packed, at.packed, gate.NodeID, at.packed)
		if err != nil {
			return 0, fmt.Errorf("chart: readmit node %s at %s: %w",
				gate.NodeID, at.position, err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chart_evictions
			(node_id, at, by, from_position, readmitted_position, version)
		VALUES (?, ?, ?, ?, NULL, ?)
		ON CONFLICT (node_id) DO UPDATE SET
			at = excluded.at, by = excluded.by,
			from_position = excluded.from_position,
			readmitted_position = NULL, version = excluded.version
		WHERE excluded.version > chart_evictions.version`,
		gate.NodeID, store.EncodeTime(at.brokerAt), gate.By, at.packed, at.packed)
	if err != nil {
		return 0, fmt.Errorf("chart: evict node %s at %s: %w",
			gate.NodeID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyGeneration writes a reanchor's audit row.
//
// CREATE-ONLY, keyed on the generation, so a replay of the new stream writes
// exactly the row the original did and a second record claiming one generation
// writes nothing rather than rewriting history.
func (a *Applier) applyGeneration(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	gen, ok := payload.(Generation)
	if !ok {
		return 0, fmt.Errorf("chart: the record at %s is a generation and "+
			"carries a %T payload", at.position, payload)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chart_log_generations
			(generation, at, by, new_stream_created_at, prev_last_seq_seen,
			 record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (generation) DO NOTHING`,
		int64(gen.Generation), store.EncodeTime(at.brokerAt), gen.By,
		store.EncodeTime(gen.NewStreamCreatedAt), int64(gen.PrevLastSeqSeen),
		at.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("chart: write the generation %d row at %s: %w",
			gen.Generation, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// note records that an object changed, for the post-commit rebuild.
func (a *Applier) note(refs ...ObjectRef) {
	for _, ref := range refs {
		if ref.ID == "" {
			continue
		}
		a.changed = append(a.changed, ref)
	}
}
