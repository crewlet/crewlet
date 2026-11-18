package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The framework's own implementation of [Rows]. Most of a snapshot is the
// FRAMEWORK's (the arbitration anchor, the checkpoint, the deferred record and
// its scope index), and a domain implementing this seam would be a second copy
// of the two-clause containment probe, where a wrong clause is silent data
// loss: a probe that misses a deferred record lets a writer take the
// retry-at-zero branch and overwrite a mutation no later reprocess can
// recover. Only the GUARDS (permanently removed? exists at all?) are the
// domain's, so they arrive as one callback.

// Guards answers the two object-level facts a first write needs, from inside
// the snapshot's own transaction.
//
// A CALLBACK RATHER THAN TWO TABLE NAMES: a domain may keep its deletion
// markers beside its objects, or have no removal at all. Returning false for
// both means "no permanent removal and no first-writer guard", right for a
// domain whose objects are all created by their first record.
type Guards func(ctx context.Context, tx *sql.Tx, subj Subject) (deleted, guard bool, err error)

// SnapshotRows is [Rows] over one domain's own estate.
type SnapshotRows struct {
	db     store.ReplicatedReader
	tables tables
	guards Guards
}

// NewRows builds the publisher's read seam for a domain, on the one of its
// logs spec names — whose stream keys the checkpoint a decision is paired with
// and whose prefix keys every anchor an expectation is formed from.
//
// THE REPLICATED ESTATE'S READ HANDLE, and nothing looser: the node's own
// database reads the same way, and a seam over it answers every decision from
// a file with none of the domain's tables — "no such table", or worse, the
// empty answer of a table that happens to share the name.
func NewRows(db store.ReplicatedReader, d Domain, spec StreamSpec, guards Guards) (*SnapshotRows, error) {
	if db.IsZero() {
		return nil, fmt.Errorf("statelog: a read seam needs the replicated estate")
	}
	t, err := newTables(d, spec)
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
	opID string, held func(tx *sql.Tx, entry OpEntry) error,
	decide func(tx *sql.Tx, checkpoint Position) (Decision, error)) (Snap, error) {

	var snap Snap
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		// THE OPERATION FIRST — see [Snap.Held]. A domain with no ledger
		// answers false here, which is every write on it.
		entry, ok, err := r.tables.op(ctx, tx, opID)
		if err != nil {
			return err
		}
		if ok {
			if err = held(tx, entry); err != nil {
				return err
			}
			snap.Held, snap.HeldOK = entry.Position, true
			return nil
		}
		// THE CHECKPOINT THESE ROWS ARE AT, from the same transaction, where
		// it commits with the rows. The expectation-zero fence compares it
		// rather than the applier's live position ([Snap.Checkpoint]), and it
		// is read BEFORE the decision, which is stamped with its generation.
		row, _, err := r.tables.readCursor(ctx, tx)
		if err != nil {
			return err
		}
		checkpoint := row.at
		snap.Checkpoint = checkpoint

		decision, err := decide(tx, checkpoint)
		if err != nil {
			return err
		}
		snap.Decision = decision

		// The generation an absent anchor is reported AT is the
		// checkpoint's, which is the generation the decision was stamped
		// with: an anchor from a previous one is comparable and safely
		// stale rather than an expectation.
		anchor, err := r.tables.anchor(ctx, tx, r.tables.subjectOf(subj), checkpoint.Generation)
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
// AN EMPTY SCOPE IS NOT AN EMPTY PROBE: a caller that could not say what its
// write is about is one every deferred record concerns, and "nothing is
// deferred" is the one answer that licenses the unsafe branch. The domain's
// root path is not known here, so an empty set is replaced by one matching
// every stored path.
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
// CONCLUSIVE ONLY at or below this node's applied position and only for an
// operation minted at or after [Rows.LostBefore]: "absent" below the
// checkpoint means "not applied here YET", and "absent" before the watermark
// may mean "swept". Either read as "somebody else won" republishes a write
// that already landed.
func (r *SnapshotRows) Op(ctx context.Context, opID string) (OpEntry, bool, error) {
	if r.tables.ops == "" {
		// A DOMAIN WITH NO LEDGER CANNOT ANSWER, and saying so is not the
		// same as saying no: the caller's own arm for a ledgerless domain
		// is what decides, and answering false here would make an
		// ambiguous publish look resolved.
		return OpEntry{}, false, nil
	}
	var entry OpEntry
	var held bool
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		entry, held, err = r.tables.op(ctx, tx, opID)
		return err
	})
	if err != nil {
		return OpEntry{}, false, err
	}
	// THE ROW'S OWN FOUND FLAG, not its position: a zero position is a
	// value a row can hold, and reading it as absence would answer a
	// present row "never applied".
	return entry, held, nil
}

// LostBefore answers how far back the domain's ledger may have lost rows of an
// operation on a subject of this kind — see [Rows.LostBefore].
func (r *SnapshotRows) LostBefore(ctx context.Context, kind string) (time.Time, bool, error) {
	var (
		before time.Time
		ok     bool
	)
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		before, ok, err = r.tables.lostBefore(ctx, tx, kind)
		return err
	})
	return before, ok, err
}

// CheckTables reports whether a domain's declared tables accept the
// framework's own statements.
//
// A domain declares three table NAMES and the framework writes their COLUMNS,
// interpolating the name into SQL it owns, so nothing in Go connects the two:
// a migration that spelled a column differently, or gave the position three
// columns where the framework writes one, compiles, migrates, opens, serves
// every read, and fails the first time a record this build cannot decode
// arrives: the rarest path, and its failure is a stalled log. So the suite
// runs each statement against a fresh estate and rolls it back.
func CheckTables(ctx context.Context, db Estate, d Domain, spec StreamSpec) error {
	t, err := newTables(d, spec)
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
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := t.retain(ctx, tx, rec, false, db.Caps().MaxVariables); err != nil {
			return fmt.Errorf("the deferred record's own tables: %w", err)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if _, _, err := t.deferredIn(ctx, tx, rec.Scope); err != nil {
			return fmt.Errorf("the deferral probe: %w", err)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := t.writeOp(ctx, tx, rec.OpID, t.subjectOf(rec.Subject), at,
			rec.StoredAt, store.DecodeTime(0)); err != nil {
			return fmt.Errorf("the operation ledger: %w", err)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if _, _, err := t.op(ctx, tx, rec.OpID); err != nil {
			return fmt.Errorf("the operation ledger's own read: %w", err)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if _, err := t.appliedRecord(ctx, tx, rec.OpID, at, rec.StoredAt); err != nil {
			return fmt.Errorf("the operation ledger's record read: %w", err)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if _, err := t.retainedRecord(ctx, tx, at, rec.StoredAt); err != nil {
			return fmt.Errorf("the deferred record's own read by position: %w", err)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := t.advanceAnchor(ctx, tx, t.subjectOf(rec.Subject), at); err != nil {
			return fmt.Errorf("the arbitration anchor: %w", err)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if _, err := t.anchor(ctx, tx, t.subjectOf(rec.Subject), at.Generation); err != nil {
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
	// The transaction committed, so the sentinel did not travel: a store
	// that swallows a returned error would pass this check having written
	// rows, so it is reported rather than assumed.
	return fmt.Errorf("statelog: the table check committed instead of rolling "+
		"back, so it has written rows into %q's estate", d.Name())
}

// errCheckRolledBack is the sentinel that abandons the check's transaction.
var errCheckRolledBack = errors.New("statelog: table check complete")
