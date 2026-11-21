package statelog

import (
	"context"
	"database/sql"
	"fmt"
)

// WHAT A SNAPSHOT OF A DOMAIN'S ROWS HOLDS OF ITS LOG. The checkpoint moves
// past a record this node RETAINS (one it cannot decode, or signed under a
// keyring key it does not hold), and past every later one whose scope meets
// it (package doc, "A record this build cannot decode is RETAINED"), so it is
// the prefix SETTLED, not APPLIED. A reader deciding from an ABSENT row —
// nobody owns this key, this session has ended — needs the applied prefix, or
// a row a retained record would have written reads as one no record wrote,
// and deciding from that destroys what cannot be put back.
//
// Read inside the caller's own transaction, not off the runner: rows, retained
// records and checkpoint commit together (package doc, "The checkpoint commits
// with the rows"), so the prefix describes exactly the rows the caller reads.

// Prefix is how much of a domain's log one snapshot of its rows holds.
type Prefix struct {
	// Settled is the checkpoint the snapshot's rows were committed with:
	// every record at or below it was consumed, which is not the same as
	// applied — see the file's header.
	Settled Position

	// Retained is the earliest record at or below Settled this node holds
	// and could not apply, when Retains is set.
	Retained Deferral
	Retains  bool
}

// Applied is the position through which every record in this snapshot
// produced its rows here, or none on any node (a gated record). It is the
// settled checkpoint with nothing retained, else just below the earliest
// retained record, since one retained because its scope met an earlier one is
// above that one. A reader that decides from an absence compares THIS against
// the position it needs covered, never [Prefix.Settled].
func (p Prefix) Applied() Position {
	if !p.Retains || p.Retained.Position.Packed() > p.Settled.Packed() {
		return p.Settled
	}
	below := p.Retained.Position
	if below.Seq > 0 {
		below.Seq--
	}
	return below
}

// PrefixIn reads d's prefix inside tx, the caller's own snapshot of the estate
// d's rows live in, on d's own log ([Domain.Stream]): every spec a node runs d
// under names that log ([StreamSpec.Instantiates]), differing only in a byte
// ceiling nothing here reads, so this is the checkpoint d's applier commits.
func PrefixIn(ctx context.Context, tx *sql.Tx, d Domain) (Prefix, error) {
	t, err := newTables(d, d.Stream())
	if err != nil {
		return Prefix{}, err
	}
	row, _, err := t.readCursor(ctx, tx)
	if err != nil {
		return Prefix{}, err
	}
	settled := row.at
	retained, retains, err := t.oldestDeferred(ctx, tx)
	if err != nil {
		return Prefix{}, err
	}
	return Prefix{Settled: settled, Retained: retained, Retains: retains}, nil
}

// DeferredIn reports, inside tx, the earliest record this node holds and could
// not apply whose declared scope intersects s: the coverage probe, answered in
// the caller's snapshot. It asks EVERY retained record's scope, never only the
// earliest record, which is usually about something else. Three-valued: a hit,
// a definite miss, and an error that says nothing either way.
func DeferredIn(ctx context.Context, tx *sql.Tx, d Domain, s ScopeSet) (
	Deferral, bool, error) {

	if s.Empty() {
		// A PROBE THAT NAMES NOTHING cannot be answered "no", or an empty
		// scope would read as uncovered.
		return Deferral{}, false, fmt.Errorf("statelog: a coverage probe over "+
			"%s names no objects, so nothing can be said about what a retained "+
			"record would cover", d.Name())
	}
	t, err := newTables(d, d.Stream())
	if err != nil {
		return Deferral{}, false, err
	}
	return t.deferredIn(ctx, tx, s)
}
