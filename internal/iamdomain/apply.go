package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// applyLog is the applier's own voice, apart from the read side's: the one
// thing it says is that a removal's key outlived the removal, and an operator
// grepping for that should not have to know it was filed under reads.
var applyLog = logging.Get("iam.apply")

// The applier, and the four rules that make it a PURE FUNCTION of the log.
//
// N nodes derive one SQL state from one ordered stream, and this domain CLAIMS
// IDENTITY: the claim that their tables are byte-identical is checkable, by a
// comparison of the replicated rows themselves. It survives exactly as long as
// these four hold, and they are every sibling domain's four because they are
// properties of the framework rather than of any one domain:
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
// # The one rule this applier has that no sibling needs
//
// IT NEVER DECRYPTS. Every sealed value the record carries is written through
// as bytes. That is not a convenience — it is what keeps rule 1 true in a
// domain whose values are encrypted: opening one would mean reading a key out
// of the fleet secret store, inside the apply transaction, on a path that must
// produce identical rows on a node whose key store is unreachable. An applier
// that could fail on a coordination read is an applier that stalls the log
// when coordination is down, which is the failure every three-valued rule in
// this tree exists to avoid.
//
// A SHRED IS THE EXCEPTION AND IT IS POST-COMMIT. Destroying a removed
// person's key is a consequence of a record here that is not a row, so it
// happens in [Applier.Committed], after the rows are durable — and if it
// fails, the row says removed while the key lives, which the identity duties
// retry. The other order would be a key destroyed for a removal that then
// rolled back.
//
// THE DIRECTORY SIGNAL IS THE OTHER, and it is post-commit for the same
// reason. A suspension withdraws the seat's contact identities from this
// node's notify registry with no org-chart record at all, and the apply is the
// only thing that sees that happen on EVERY node — the change feed relays a
// record to one. So a committed batch that moved a seat's standing tells the
// engine, which re-reads the directory and rebuilds the registry whole.

// Applier writes this node's copy of the identity estate.
type Applier struct {
	// NodeID is this node's own id, which the eviction gate compares a
	// record's writer against.
	NodeID string

	// shredder destroys a removed person's key. Nil is legal and means a
	// node that holds no key store — a satellite, or a test — in which
	// case a removal deletes rows and the key is somebody else's to
	// destroy. It is NOT nil on a node that serves requests, and the
	// registration is what supplies it.
	shredder Shredder

	// shred is filled inside Apply and drained by Committed. NOT guarded
	// by a mutex, and the framework's contract is why: an applier is ONE
	// writer, and Apply and Committed are called from the same goroutine
	// with the commit in between.
	shred []string

	// directory is told, after a committed batch, that who holds which
	// seat — or at what stage — may have moved. Nil is legal and means
	// nobody is listening.
	//
	// IT CARRIES NOTHING, deliberately: its one listener is the notify
	// registry, which is rebuilt WHOLE from a fresh read of the directory
	// and swapped, because a diff applied to a fresh registry drops every
	// identity it did not touch. So there is nothing a list of changed
	// people could be used for except to be wrong about.
	directory func()

	// directoryMoved is set inside Apply when a record wrote a row the
	// directory's standing is read from — a person's stage, a seat claim
	// or its release, a removal — and drained by Committed, on shred's
	// terms. A SIGN-IN SETS NOTHING: it is the bulk of this log's traffic
	// and it moves no seat's standing, so a rebuild per session would be a
	// registry rebuilt per login for nothing.
	directoryMoved bool
}

// Shredder destroys a person's key, which is what removing them does.
//
// CONSUMER-DEFINED and one method wide: [Sealer] satisfies it, and the applier
// needs nothing else from it. A seam this narrow is also what lets the suite
// watch what a removal asked for without standing up a coordination backend.
type Shredder interface {
	Shred(ctx context.Context, personID string) (bool, error)
}

// NewApplier builds the identity estate's applier for one node.
//
// directory is called after a committed batch that moved a seat's standing —
// see the field. AFTER the commit and never inside the transaction, for the
// reason internal/chart's view trigger gives: the store re-runs the body of an
// attempt that failed transiently, and a listener told about rows that then
// rolled back would rebuild from rows no node holds.
func NewApplier(nodeID string, shredder Shredder, directory func()) *Applier {
	return &Applier{NodeID: nodeID, shredder: shredder, directory: directory}
}

// Committed is the post-commit half: the two consequences of a record here
// that are not rows — this node's contact routing hearing that a seat's
// standing may have moved, and a removed person's key being destroyed.
//
// IT IS BEST EFFORT AND SAYS SO. A shred that fails leaves a person removed
// from every node's rows with their key still live, which is a state the
// identity duties find and retry — and the alternative, failing the apply,
// would stall the whole fleet's log on a coordination outage, for a deletion
// that is already durable everywhere it matters.
func (a *Applier) Committed(ctx context.Context) {
	// THE DIRECTORY FIRST, and reset before the call: the listener is a
	// non-blocking signal, and the shreds below are network round trips a
	// suspension's contact withdrawal must not wait behind.
	if a.directoryMoved {
		a.directoryMoved = false
		if a.directory != nil {
			a.directory()
		}
	}
	if len(a.shred) == 0 {
		return
	}
	// RESET BEFORE THE CALLS, not after. One of them may take long enough
	// for the next batch to be waiting behind it, and a set drained
	// afterwards would either be lost or delivered twice.
	people := a.shred
	a.shred = nil
	if a.shredder == nil {
		return
	}
	for _, id := range people {
		// THE FAILURE IS SAID AND NOT RETURNED. There is no caller to
		// return it to — this runs after the commit, on the applier's
		// own goroutine — and the durable record of what still needs
		// destroying is the `iam_removed` row, which [ShredRemoved]
		// reads on every pass of the key duty until the delete lands.
		// Said at WARN, because until then the person's name and
		// address are readable from every backup taken before the
		// removal, and an operator asked "is that person gone" deserves
		// a log that says not yet.
		if _, err := a.shredder.Shred(ctx, id); err != nil {
			applyLog.WarnContext(ctx, "iam_key_shred_deferred",
				"person", id, "node", a.NodeID, "error", err.Error(),
				"detail", "the removal is applied and this person's key "+
					"still exists; the key duty retries until it is destroyed")
		}
	}
}

// Gated reports a record that must produce no rows at all.
//
// TWO GATES, READ FROM THIS SAME TRANSACTION, because the answer has to come
// from the state the record would have applied against rather than from a
// cache that may be a heartbeat old:
//
//  1. THE EVICTION GATE. A record written by a node the fleet evicted before
//     the record's own position is dropped everywhere. It depends on nothing
//     but the log's own order, which is what makes it the fence that holds
//     when coordination cannot be reached at all.
//  2. THE REMOVAL GATE. A record about a person a removal destroyed applies
//     nowhere, for ever. Without it a redelivery months later would write back
//     somebody the company off-boarded — on one node only, in a table that
//     claims identity, and in this domain that is somebody who can sign in.
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

	// THE REMOVAL GATE IS ANSWERED FROM THE PERSON, and the person is on
	// the MutationRecord rather than on the envelope — see the field's own
	// doc for why an id is not an envelope field. A record this build
	// cannot decode therefore reaches here with no person, and is NOT
	// gated: that is correct and deliberate, because such a record is
	// DEFERRED rather than applied, and the deferral is reprocessed by a
	// build that can read it, at which point this gate runs properly.
	person, ok := a.gatedPerson(rec)
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
	// THE ONE EXCEPTION IS THE RECORD THAT WROTE THE TOMBSTONE, by its own
	// op id — not by its op kind. "Any removal" would let a SECOND removal
	// of one person through, and by the committed position would fail for a
	// republished copy, leaving a node holding only that copy unable to
	// write its own tombstone at all.
	if author.Valid && author.String == rec.OpID {
		return "", false, nil
	}
	return statelog.ReasonDeleted, true, nil
}

// gatedPerson is the person a record is ABOUT, for the removal gate.
//
// IT DECODES THE PAYLOAD, which no sibling's gate does, and the reason is this
// domain's own shape: a record about a person routinely arbitrates on an
// ADDRESS, a login, a seat id or a session lineage, so a gate that read only
// the subject would drop the records on the person's own subject and let every
// claim record through — writing a removed person's address back onto a row
// nothing will ever correct.
//
// A RECORD IT CANNOT DECODE IS NOT GATED, and that is safe rather than a hole:
// such a record is DEFERRED rather than applied, and the deferral is
// reprocessed by a build that can read it, at which point this runs properly.
func (a *Applier) gatedPerson(rec statelog.Record) (string, bool) {
	if ObjectKind(rec.Subject.Kind) == KindPerson && rec.Subject.ID != "" {
		return rec.Subject.ID, true
	}
	record, err := Decode(rec.Payload)
	if err != nil || record.Person == "" {
		return "", false
	}
	return record.Person, true
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
		// THE READ INDEX'S OWN APPEND. It writes no row on any node, and
		// that is its entire content: it exists so a linearizable read
		// has a quorum-committed position to wait through. It writes no
		// history row either, which is why it is absent from the class
		// table rather than classified.
		return 0, nil
	case KindPerson:
		rows, err = a.applyPerson(ctx, tx, at)
	case KindEmail:
		rows, err = a.applyEmail(ctx, tx, at)
	case KindLogin, KindSeat:
		rows, err = a.applyToken(ctx, tx, at, ObjectKind(rec.Subject.Kind))
	case KindLink:
		rows, err = a.applyLink(ctx, tx, at)
	case KindSession:
		rows, err = a.applySession(ctx, tx, at)
	case KindInvalidation:
		rows, err = a.applyInvalidation(ctx, tx, at)
	case KindBootstrap:
		rows, err = a.applyBootstrap(ctx, tx, at)
	case KindSweep:
		rows, err = a.applySweep(ctx, tx, at)
	case KindEviction:
		rows, err = a.applyEviction(ctx, tx, at)
	case KindGeneration:
		rows, err = a.applyGeneration(ctx, tx, at)
	default:
		// A KIND THIS BUILD DOES NOT KNOW REACHES HERE ONLY AT A VERSION
		// IT CAN DECODE, which is a record a peer wrote against a
		// grammar this build shares and a kind it does not — the one
		// combination the two-pass decode cannot file for later. It
		// writes nothing and says so, rather than being counted as
		// applied.
		return 0, fmt.Errorf("iamdomain: the record at %s is on subject kind "+
			"%q, which this build has no case for — it decoded, so it cannot "+
			"be deferred, and applying nothing silently would make this node's "+
			"rows differ from a peer that knows the kind",
			rec.Position, rec.Subject.Kind)
	}
	if err != nil {
		return rows, err
	}

	// AND THE AUTHENTICATION TRAIL, derived from the op rather than
	// carried on the record — a writer that stated its own class could
	// file a suspension in the short horizon, and the row would simply be
	// gone when somebody went looking.
	written, err := a.writeHistory(ctx, tx, at)
	return rows + written, err
}

// applyContext is everything one record's apply reads, gathered once.
//
// A VALUE RATHER THAN FIVE ARGUMENTS, because every arm needs all of it and a
// signature that grew one per arm is how one of them comes to be passed the
// batch's instant where the broker's belongs.
type applyContext struct {
	record   MutationRecord
	position statelog.Position

	// packed is the composed (generation << 40) | sequence this record
	// sits at, which is every row's `version` and the whole of what a
	// monotone guard compares.
	packed int64

	// brokerAt is the BROKER's own instant for this record, which is what
	// makes a derived timestamp byte-identical on every node rather than a
	// clock each one read for itself.
	brokerAt time.Time

	maxVariables int
}

// unix is the broker instant as this estate stores it.
func (a applyContext) unix() int64 { return a.brokerAt.UnixMilli() }

// bucket is the partition every row this record writes belongs to.
//
// FROM THE PERSON, not from the scope: the scope is what a DEFERRAL is filed
// under and may legitimately be wider than one person, while a row belongs to
// exactly one bucket. A record about nobody — a sweep, a gate — states its own.
func (a applyContext) bucket() int64 {
	if a.record.Person == "" {
		return int64(BootstrapBucket())
	}
	return int64(BucketOf(a.record.Person))
}
