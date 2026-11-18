package iamdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// applyLog is the applier's own logger, apart from the read side's, so an
// operator grepping for an apply need not know to look under reads.
var applyLog = logging.Get("iam.apply")

// The applier is a pure function of the log ([statelog.Applier]). Here that
// means a derived instant is a MAX over a set, never a fold over arrival order
// (two nodes at one checkpoint saw one set in different orders), and every
// upsert's version guard SKIPS rather than errs: a redelivery is ordinary, and
// a constraint violation would abort the batch on every node and stall the
// log (package doc, "ONE SUBJECT FOR THE DIRECTORY").

// IT NEVER DECRYPTS: a sealed value is written through as bytes and a removal
// erases it by shape ([eraseSealed]), or two nodes a key rotation reached
// differently would write different rows from one record.
//
// WHAT A BATCH MOVED IS SAID POST-COMMIT ([Moved]): a suspension withdraws
// contact identities and closes dashboard sockets with no row to watch, and
// only the apply sees it on every data node (the change feed relays to one).

// Applier writes this node's copy of the identity estate.
type Applier struct {
	// NodeID is this node's own id, which the eviction gate compares a
	// record's writer against.
	NodeID string

	// committed is told, after a committed batch, what it moved — see
	// [Moved]. Nil is legal and means nobody is listening.
	committed func(Moved)

	// moved is filled inside Apply and drained by Committed. No mutex: an
	// applier is one writer, and the framework calls both from one goroutine
	// with the commit between. A re-run body fills it twice, costing only a
	// re-read ([Moved] on why over-reporting is safe).
	moved Moved
}

// NewApplier builds the identity estate's applier for one node.
//
// committed is called after a committed batch that moved anything ([Moved]),
// never inside the transaction: the store re-runs a transiently failed body,
// and a listener told of rows that then rolled back would decide from rows no
// node holds.
func NewApplier(nodeID string, committed func(Moved)) *Applier {
	return &Applier{NodeID: nodeID, committed: committed}
}

// Committed is the post-commit half: it tells this node's contact routing and
// open connections what the batch moved.
//
// IT MUST NOT BLOCK, nor may the listener: it runs on the apply loop's
// goroutine with the next batch waiting.
func (a *Applier) Committed(context.Context) {
	if a.moved.Empty() {
		return
	}
	// RESET BEFORE THE CALL, so a move the listener raises while it runs
	// reaches the next batch rather than being erased.
	moved := a.moved
	a.moved = Moved{}
	if a.committed != nil {
		a.committed(moved)
	}
}

// movedPerson records that the row of person id moved, under the login the row
// holds — see [heldLogin].
func (a *Applier) movedPerson(ctx context.Context, tx *sql.Tx, id string) error {
	login, err := heldLogin(ctx, tx, id)
	if err != nil {
		return err
	}
	a.moved.person(id, login)
	return nil
}

// heldLogin is the login person id's row holds, read in the apply's own
// transaction, or "" for a row that holds none or does not exist. A move names
// it beside the person so a credential bound through that login — a Tier A
// token's `token:<id>` row — is named too ([Moved.Logins]).
func heldLogin(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var login string
	err := tx.QueryRowContext(ctx, personLoginStatement, id).Scan(&login)
	if err != nil && !errorsIsNoRows(err) {
		return "", fmt.Errorf("iamdomain: read the login of person %s, whose "+
			"row this record moved: %w", id, err)
	}
	return login, nil
}

// personLoginStatement is the login one person's row holds, by the row's own
// key: what an apply names a moved person by ([heldLogin]) and what a decide
// forms a credential's reason from ([loginOf]).
const personLoginStatement = `SELECT login FROM iam_people WHERE id = ?`

// THE APPLIER HEARS WHAT A BATCH RETAINED: see [Applier.Retained].
var _ statelog.RetentionHook = (*Applier)(nil)

// Retained is told, after a committed batch and before [Applier.Committed],
// that the batch RETAINED records this node did not apply: a newer build's, one
// under a keyring key this node lacks, or one a retained record's scope covers.
//
// IT MOVES EVERYONE ([Moved.Everyone]): the person is in an unreadable payload
// and the scope is a bucket, so no list can name them. Every read of a person
// in that bucket now answers unknown and the guard refuses their REST requests
// 503; only a move naming everyone reaches a connection opened on a decision
// such a record (a revocation, a suspension) may have overturned. NOT
// [Moved.Seats]: contact routing is rebuilt from rows, and it wrote none.
func (a *Applier) Retained(context.Context, int) {
	a.moved.Everyone = true
}

// THE APPLIER MAINTAINS TWO DERIVED COLUMNS: see [Applier.Rederive].
var _ statelog.Deriver = (*Applier)(nil)

// DerivationVersion is the rule set this build derives its columns at: 1 is an
// invitation's `seat_id` (replicated migration 0042), 2 adds a removal
// tombstone's `seat_id` (0122). The apply writes each from the record, and
// [Applier.Rederive] recomputes them when the checkpoint's rule set differs: a
// reanchor's checkpoint (derivation 0), or rows adopted from another build
// ([statelog.Deriver]). Bump it on any change to what a derived column holds,
// so a node holding rows derived under another rule set converges.
const DerivationVersion = 2

// DerivationVersion implements [statelog.Deriver].
func (a *Applier) DerivationVersion() int { return DerivationVersion }

// Rederive implements [statelog.Deriver]: every invitation's `seat_id`
// recomputed from its document, and every removal tombstone's from its claims.
//
// THE SAME DECODE THE APPLY WRITES EACH FROM ([DecodeInvitation], the released
// [Identifiers]), never a `json_extract` in SQL, a second definition of the
// seat that would drift the day either is edited. Only differing rows are
// written, and only the column (the version stays the record's), so a
// re-derived row equals what a replay writes.
func (a *Applier) Rederive(ctx context.Context, tx *sql.Tx,
	_ statelog.ApplyOptions) (int, error) {

	invitations, err := rederiveInvitationSeats(ctx, tx)
	if err != nil {
		return invitations, err
	}
	removals, err := rederiveRemovalSeats(ctx, tx)
	return invitations + removals, err
}

// rederiveInvitationSeats is [Applier.Rederive]'s invitations: each row's
// `seat_id` recomputed from its document.
//
// READ WHOLE BEFORE ANY ROW IS WRITTEN: an UPDATE of the table a cursor walks
// may or may not be seen by it again.
func rederiveInvitationSeats(ctx context.Context, tx *sql.Tx) (int, error) {
	type stale struct{ id, seat string }
	rows, err := tx.QueryContext(ctx, invitationSeatsStatement)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: read the invitations to re-derive "+
			"their seats: %w", err)
	}
	var repair []stale
	for rows.Next() {
		var (
			id, held string
			document []byte
		)
		if err = rows.Scan(&id, &held, &document); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("iamdomain: read an invitation to re-derive "+
				"its seat: %w", err)
		}
		invitation, decodeErr := DecodeInvitation(document)
		if decodeErr != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("iamdomain: open invitation %s to re-derive "+
				"its seat: %w", id, decodeErr)
		}
		if invitation.Seat != held {
			repair = append(repair, stale{id: id, seat: invitation.Seat})
		}
	}
	if err = rows.Close(); err != nil {
		return 0, fmt.Errorf("iamdomain: read the invitations to re-derive "+
			"their seats: %w", err)
	}
	if err = rows.Err(); err != nil {
		return 0, fmt.Errorf("iamdomain: read the invitations to re-derive "+
			"their seats: %w", err)
	}
	for _, r := range repair {
		if _, err = tx.ExecContext(ctx, rederiveSeatStatement,
			r.seat, r.id); err != nil {
			return 0, fmt.Errorf("iamdomain: re-derive invitation %s's "+
				"seat: %w", r.id, err)
		}
	}
	return len(repair), nil
}

// rederiveRemovalSeats is [Applier.Rederive]'s removal tombstones: each row's
// `seat_id` recomputed from the released identifiers its claims hold.
//
// READ WHOLE BEFORE ANY ROW IS WRITTEN, as [rederiveInvitationSeats] is. It
// reads every tombstone once per rule-set change: the cost the derived column
// takes off every later read of one seat.
func rederiveRemovalSeats(ctx context.Context, tx *sql.Tx) (int, error) {
	type stale struct{ person, seat string }
	rows, err := tx.QueryContext(ctx, removalSeatsStatement)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: read the removals to re-derive "+
			"their seats: %w", err)
	}
	var repair []stale
	for rows.Next() {
		var person, held, claims string
		if err = rows.Scan(&person, &held, &claims); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("iamdomain: read a removal to re-derive "+
				"its seat: %w", err)
		}
		var released Identifiers
		if decodeErr := json.Unmarshal([]byte(claims), &released); decodeErr != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("iamdomain: open person %s's released "+
				"identifiers to re-derive their seat: %w", person, decodeErr)
		}
		if released.SeatID != held {
			repair = append(repair, stale{person: person, seat: released.SeatID})
		}
	}
	if err = rows.Close(); err != nil {
		return 0, fmt.Errorf("iamdomain: read the removals to re-derive "+
			"their seats: %w", err)
	}
	if err = rows.Err(); err != nil {
		return 0, fmt.Errorf("iamdomain: read the removals to re-derive "+
			"their seats: %w", err)
	}
	for _, r := range repair {
		if _, err = tx.ExecContext(ctx, rederiveRemovalSeatStatement,
			r.seat, r.person); err != nil {
			return 0, fmt.Errorf("iamdomain: re-derive person %s's released "+
				"seat: %w", r.person, err)
		}
	}
	return len(repair), nil
}

// The statements [Applier.Rederive] runs: each table's rows in key order with
// the seat they hold and its source, and one row's seat set by its key.
const (
	invitationSeatsStatement     = `SELECT id, seat_id, document FROM iam_invites ORDER BY id`
	rederiveSeatStatement        = `UPDATE iam_invites SET seat_id = ? WHERE id = ?`
	removalSeatsStatement        = `SELECT person_id, seat_id, claims_json FROM iam_removed ORDER BY person_id`
	rederiveRemovalSeatStatement = `UPDATE iam_removed SET seat_id = ? WHERE person_id = ?`
)

// Gated reports a record that must produce no rows at all. Both gates read
// this transaction, the state the record would apply against, not a cache:
//
//  1. THE EVICTION GATE: a record a node wrote after the fleet evicted it is
//     dropped everywhere. It rests on log order alone, so it holds when
//     coordination is unreachable.
//  2. THE REMOVAL GATE: a record about a removed person applies nowhere, ever,
//     or a late redelivery would write an off-boarded person back, able to
//     sign in, on one node of a table that claims identity.
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (
	statelog.Reason, bool, error) {

	if rec.Writer != "" {
		var from, readmitted sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position
			FROM iam_evictions WHERE node_id = ?`, rec.Writer).
			Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return "", false, fmt.Errorf("iamdomain: read the eviction gate "+
				"for node %s: %w", rec.Writer, err)
		default:
			at := rec.Position.Packed()
			// Both ends matter: a record at or below the eviction's
			// position was written while the node still counted, one at
			// or above a readmission by a node the fleet took back.
			evicted := from.Valid && at > from.Int64
			back := readmitted.Valid && at >= readmitted.Int64
			if evicted && !back {
				return statelog.ReasonEvicted, true, nil
			}
		}
	}

	// A record [markedPerson] cannot decode is not gated: it is deferred,
	// and this gate runs when a build that reads it reprocesses it.
	person, ok := markedPerson(rec.Subject, rec.Payload)
	if !ok {
		return "", false, nil
	}
	var author sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT record_id FROM iam_removed WHERE person_id = ?`,
		person).Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("iamdomain: read the removal gate for "+
			"person %s: %w", person, err)
	}
	// THE RECORD THAT WROTE THE TOMBSTONE passes, by op id, not op kind:
	// "any removal" would pass a second removal of one person, and a
	// position differs for a republished copy, which then could never write
	// its own tombstone.
	if author.Valid && author.String == rec.OpID {
		return "", false, nil
	}
	return statelog.ReasonDeleted, true, nil
}

// markedPerson is the person whose tombstone gates a record on subj with body,
// and false for a record no tombstone covers. Both sides ask it, held to one
// answer by [statelog.Gates]: [Applier.Gated] drops what it covers, and
// [Gates.GatedAt] names the gate that dropped the publisher's own append (from
// its body; without one it read that append as a ledger contract violation).
//
// Only a kind [ObjectKind.GatedByRemoval] names is about a person. A directory
// or session subject names nobody, so this DECODES THE PAYLOAD, or every
// directory record would write a removed person back. An undecodable record is
// not gated: the applier defers it to a build that reads it. With no body (a
// record the publisher only found above its anchor) a directory or session
// record is not gated, and the re-decide meets the tombstone ([Writer.request]).
func markedPerson(subj statelog.Subject, body []byte) (string, bool) {
	kind := ObjectKind(subj.Kind)
	if !kind.GatedByRemoval() {
		return "", false
	}
	if kind == KindPerson && subj.ID != "" {
		return subj.ID, true
	}
	if body == nil {
		return "", false
	}
	record, err := Decode(body)
	if err != nil || record.Person == "" {
		return "", false
	}
	return record.Person, true
}

// Apply writes one record's rows.
//
// The dispatch is on the SUBJECT KIND, with a case for every kind, the silent
// ones included, so a kind added without one is a compile-time hole rather
// than a silently ignored record.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: decode the record at %s: %w",
			rec.Position, err)
	}
	at := applyContext{
		record:       record,
		position:     rec.Position,
		packed:       rec.Position.Packed(),
		brokerAt:     opts.StoredAt,
		maxVariables: opts.MaxVariables,
	}

	var rows int
	switch ObjectKind(rec.Subject.Kind) {
	case KindBarrier:
		// THE READ INDEX'S OWN APPEND: no row and no trail row on any node.
		// It is only a quorum-committed position a linearizable read waits
		// through, so it is absent from the class table.
		return 0, nil
	case KindTrimmed:
		// THE TRIM'S OWN RECORD: the framework removes this log's anchors
		// below its floor in this transaction (ADR-0080). No row and no trail
		// row, so it is listed as silent rather than classified.
		return 0, nil
	case KindDirectory:
		rows, err = a.applyDirectory(ctx, tx, &at)
	case KindPerson:
		rows, err = a.applyPerson(ctx, tx, at)
	case KindSession:
		rows, err = a.applySession(ctx, tx, at)
	case KindInvalidation:
		rows, err = a.applyInvalidation(ctx, tx, at)
	case KindSweep:
		rows, err = a.applySweep(ctx, tx, at)
	case KindEviction:
		rows, err = a.applyEviction(ctx, tx, at)
	case KindGeneration:
		rows, err = a.applyGeneration(ctx, tx, at)
	default:
		// A KIND THIS BUILD DOES NOT KNOW, AT A VERSION IT DECODES: the one
		// combination the two-pass decode cannot defer, refused rather than
		// counted as applied.
		return 0, fmt.Errorf("iamdomain: the record at %s is on subject kind "+
			"%q, which this build has no case for — it decoded, so it cannot "+
			"be deferred, and applying nothing silently would make this node's "+
			"rows differ from a peer that knows the kind",
			rec.Position, rec.Subject.Kind)
	}
	if err != nil {
		return rows, err
	}

	// AND THE AUTHENTICATION TRAIL, its class derived from the op rather than
	// carried ([Applier.writeHistory]).
	written, err := a.writeHistory(ctx, tx, at)
	return rows + written, err
}

// applyContext is everything one record's apply reads, gathered once.
//
// A VALUE, NOT FIVE ARGUMENTS: every arm needs all of it, and a signature grown
// one per arm is how one gets the batch's instant where the broker's belongs.
type applyContext struct {
	record   MutationRecord
	position statelog.Position

	// aboutKind and aboutID are the object the trail row is filed under where
	// the record's subject names nobody (a directory record's person, an
	// invitation's address), else empty, filing it under its subject. Set by
	// [Applier.applyDirectory] from the payload it decodes.
	aboutKind, aboutID string

	// blind is the address an invitation was issued to, which buckets a
	// record about nobody. Set the same way.
	blind string

	// packed is the composed (generation << 40) | sequence this record
	// sits at, which is every row's `version` and the whole of what a
	// monotone guard compares.
	packed int64

	// brokerAt is the BROKER's instant for this record, so a derived
	// timestamp is byte-identical on every data node.
	brokerAt time.Time

	maxVariables int
}

// unix is the broker instant as this estate stores it.
func (a applyContext) unix() int64 { return a.brokerAt.UnixMilli() }

// bucket is the partition every row this record writes belongs to: its
// PERSON's, not its scope's, which files a deferral and may span more than one
// person. A record about nobody takes its ADDRESS's (an invitation's issue,
// beside the invitation), else its subject id's, else its kind's (an
// invalidation).
func (a applyContext) bucket() int64 {
	switch {
	case a.record.Person != "":
		return int64(BucketOf(a.record.Person))
	case a.blind != "":
		return int64(BucketOf(a.blind))
	case a.record.Subject.ID != "":
		return int64(BucketOf(a.record.Subject.ID))
	}
	return int64(BucketOf(string(a.record.Subject.Kind)))
}
