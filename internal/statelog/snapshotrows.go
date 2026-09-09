package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// The framework's own implementation of [Rows], and why it is here rather than
// in each domain.
//
// Three of the four things a snapshot returns are the FRAMEWORK's: the
// arbitration anchor lives in its table, and the deferred record and its scope
// index live in tables whose shape the framework writes. A domain implementing
// this seam for itself would be a second copy of the two-clause containment
// probe — the one piece of SQL in this design where getting a clause wrong is
// silent data loss rather than a wrong answer, because a probe that misses a
// deferred record lets a writer take the retry-at-zero branch and overwrite a
// mutation no later reprocess can recover.
//
// What is genuinely the domain's is the pair of GUARDS: whether an object was
// permanently removed, and whether it exists at all. Those are rows only the
// domain knows the shape of, so they arrive as one small callback.

// Guards answers the two object-level facts a first write needs, from inside
// the snapshot's own transaction.
//
// A CALLBACK RATHER THAN TWO TABLE NAMES, because the questions are not always
// one table each: a domain may keep its deletion markers beside its objects, or
// have no concept of removal at all. Returning false for both is a legitimate
// answer and means "this domain has no permanent removal and no first-writer
// guard", which is exactly right for a domain whose objects are all created by
// their first record.
type Guards func(ctx context.Context, tx *sql.Tx, subj Subject) (deleted, guard bool, err error)

// SnapshotRows is [Rows] over one domain's own estate.
type SnapshotRows struct {
	db     *store.DB
	tables tables
	guards Guards
}

// NewRows builds the publisher's read seam for a domain.
func NewRows(db *store.DB, d Domain, guards Guards) (*SnapshotRows, error) {
	if db == nil {
		return nil, fmt.Errorf("statelog: a read seam needs a store")
	}
	t, err := newTables(d)
	if err != nil {
		return nil, err
	}
	return &SnapshotRows{db: db, tables: t, guards: guards}, nil
}

// Snapshot opens ONE read transaction, lets the domain decide inside it, and
// returns the framework's own inputs read in that same transaction.
//
// The single transaction is the whole contract: an expectation is a claim
// about the LOG and a decision is a claim about the ROWS, and pairing an old
// decision with a new expectation is how two writers mint the same key — the
// broker matches the expectation, accepts the append, and both callers are
// told they won.
func (r *SnapshotRows) Snapshot(ctx context.Context, subj Subject, scope ScopeSet,
	decide func(*sql.Tx) (Decision, error)) (Snap, error) {

	var snap Snap
	err := r.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		decision, err := decide(tx)
		if err != nil {
			return err
		}
		snap.Decision = decision

		// The generation the anchor is read AT is the decision's own,
		// because that is the generation the write will publish in: an
		// anchor from a previous one is comparable and safely stale
		// rather than an expectation.
		anchor, err := r.tables.anchor(ctx, tx, subj.String(), decision.Envelope.Gen)
		if err != nil {
			return err
		}
		snap.Anchor = anchor

		deferral, deferred, err := r.tables.deferredIn(ctx, tx, probeScope(scope))
		if err != nil {
			return err
		}
		snap.Deferral, snap.Deferred = deferral, deferred

		if r.guards != nil {
			deleted, guard, err := r.guards(ctx, tx, subj)
			if err != nil {
				return err
			}
			snap.Deleted, snap.Guard = deleted, guard
		}
		return nil
	})
	if err != nil {
		return Snap{}, err
	}
	return snap, nil
}

// probeScope is what the deferral probe is run over.
//
// AN EMPTY SCOPE IS NOT AN EMPTY PROBE. A caller that could not say what its
// write is about is a caller every deferred record concerns, and answering
// "nothing is deferred" there is the one answer that licenses the unsafe
// branch. The domain's own root path is not known here, so the fallback is the
// scope the caller gave with one addition: an empty set probes nothing and is
// replaced by a set that matches every stored path.
func probeScope(s ScopeSet) ScopeSet {
	if !s.Empty() {
		return s
	}
	return ScopeSet{Paths: []string{everythingPath}}
}

// everythingPath is a root every domain's own paths sit under by construction,
// because [Ancestors] walks to the first segment: a probe rooted here covers
// any stored path via the LIKE clause, whatever alphabet the domain chose.
const everythingPath = ""

// Op answers where an operation was applied on this node.
//
// CONCLUSIVE ONLY at or below this node's applied position and only above its
// own adoption instant: the ops table is this node's applier's own record, so
// "absent" below the checkpoint means "not applied here YET" and "absent"
// below an adoption means "scrubbed out of the snapshot I arrived with".
// Either read as "somebody else won" republishes a write that already landed.
func (r *SnapshotRows) Op(ctx context.Context, opID string) (Position, bool, error) {
	if r.tables.ops == "" {
		// A DOMAIN WITH NO LEDGER CANNOT ANSWER, and saying so is not the
		// same as saying no: the caller's own arm for a ledgerless domain
		// is what decides, and answering false here would make an
		// ambiguous publish look resolved.
		return Position{}, false, nil
	}
	var at Position
	err := r.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		got, _, err := r.tables.op(ctx, tx, opID)
		at = got
		return err
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Position{}, false, nil
	case err != nil:
		return Position{}, false, err
	case at.Seq == 0 && at.Generation == 0:
		return Position{}, false, nil
	}
	return at, true, nil
}

// CheckTables reports whether a domain's declared tables accept the
// framework's own statements.
//
// # Why this is a check and not a comment
//
// A domain declares three table NAMES and the framework writes their COLUMNS.
// Nothing in Go connects the two: the statements are built by interpolating the
// name into SQL the framework owns, so a migration that spelled a column
// differently — or gave the position three columns where the framework writes
// one — compiles, migrates, opens, serves every read, and fails the first time
// a record this build cannot decode arrives. That is the rarest path in the
// system and the one whose failure is a stalled log.
//
// So the suite runs one of each statement against a fresh estate and rolls it
// back. What it proves is exactly what a comment cannot: the shapes agree.
func CheckTables(ctx context.Context, db *store.DB, d Domain) error {
	t, err := newTables(d)
	if err != nil {
		return err
	}
	at := Position{Stream: t.stream, Generation: 1, Seq: 1}
	rec := Record{
		Envelope: Envelope{
			V: d.RecordVersion() + 1, Kind: "probe",
			Subject: Subject{Kind: "probe", ID: "check"},
			OpID:    "statelog-check",
			Scope:   ScopeSet{Paths: []string{"check"}},
		},
		Position: at,
		Payload:  []byte(`{"v":0}`),
		StoredAt: store.DecodeTime(0),
	}
	// EVERY STATEMENT, INSIDE ONE TRANSACTION THAT IS THEN ABANDONED — so
	// the check writes nothing and still exercises the writes.
	probe := errCheckRolledBack
	err = db.Replicated().Tx(ctx, func(tx *sql.Tx) error {
		if err := t.retain(ctx, tx, rec, false); err != nil {
			return fmt.Errorf("the deferred record's own tables: %w", err)
		}
		if _, _, err := t.deferredIn(ctx, tx, rec.Scope); err != nil {
			return fmt.Errorf("the deferral probe: %w", err)
		}
		if err := t.writeOp(ctx, tx, rec.OpID, rec.Subject.String(), at,
			store.DecodeTime(0)); err != nil {
			return fmt.Errorf("the operation ledger: %w", err)
		}
		if _, _, err := t.op(ctx, tx, rec.OpID); err != nil {
			return fmt.Errorf("the operation ledger's own read: %w", err)
		}
		if err := t.advanceAnchor(ctx, tx, rec.Subject.String(), at); err != nil {
			return fmt.Errorf("the arbitration anchor: %w", err)
		}
		if _, err := t.anchor(ctx, tx, rec.Subject.String(), at.Generation); err != nil {
			return fmt.Errorf("the arbitration anchor's own read: %w", err)
		}
		return probe
	})
	if errors.Is(err, probe) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("statelog: domain %q declares tables the framework's "+
			"own statements cannot use: %w", d.Name(), err)
	}
	// The transaction committed, which means the sentinel did not travel —
	// a store that swallows a returned error would make this check pass
	// having written rows, so it is reported rather than assumed.
	return fmt.Errorf("statelog: the table check committed instead of rolling "+
		"back, so it has written rows into %q's estate", d.Name())
}

// errCheckRolledBack is the sentinel that abandons the check's transaction.
var errCheckRolledBack = errors.New("statelog: table check complete")
