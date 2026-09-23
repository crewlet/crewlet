package statelog

import (
	"context"
	"database/sql"
	"fmt"
)

// WHAT A SNAPSHOT OF A DOMAIN'S ROWS HOLDS OF ITS LOG, read inside the
// caller's own transaction.
//
// # The checkpoint is not evidence that a record produced its rows
//
// The checkpoint moves past a record this node RETAINS — one at a version this
// build cannot read, one signed under a keyring key it does not hold, and every
// later record whose scope meets one of those — because a record retained is a
// record CONSUMED, and a checkpoint that stopped at it would stall the node for
// the length of a rolling upgrade. So a checkpoint at or past a position says
// the node has SEEN everything up to it, and nothing about whether each of
// those records wrote its rows here. The floor theorem draws the same line: the
// checkpoint is the prefix SETTLED rather than APPLIED.
//
// For most readers the difference is a staleness the read levels already
// answer. For a reader that decides from an ABSENCE it is the whole question —
// "nobody owns this key", "this session has ended", "no blind was ever derived
// here" are each an absent row, and an absent row on a node that retained the
// record which would have written it reads exactly like one no record ever
// wrote. Deciding from that destroys what cannot be put back.
//
// # Why these are read inside the caller's snapshot and not off the runner
//
// The rows, the retained records and the checkpoint commit in ONE transaction
// (contract 2), so a read transaction sees the three at one instant: a prefix
// read in the transaction that reads the rows describes exactly those rows. A
// reading taken from the runner's memory is a statement about some other
// instant — before the rows the caller then reads, or after — and every caller
// that paired the two had to reason about which order made that safe.

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
// produced its rows here, or produced none on any node (a gated record, which
// by its own determinism writes nothing anywhere).
//
// It is the settled checkpoint with nothing retained, and the position just
// below the earliest retained record otherwise: a record retained because its
// scope met an earlier one is itself above that one, so nothing below the
// earliest was held back. A reader that decides from an absence compares THIS
// against the position it needs covered, never [Prefix.Settled].
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

// PrefixIn reads d's prefix inside tx, which is the caller's own snapshot of
// the estate d's rows live in.
func PrefixIn(ctx context.Context, tx *sql.Tx, d Domain) (Prefix, error) {
	t, err := newTables(d)
	if err != nil {
		return Prefix{}, err
	}
	settled, _, _, err := t.readCursor(ctx, tx)
	if err != nil {
		return Prefix{}, err
	}
	retained, retains, err := t.oldestDeferred(ctx, tx)
	if err != nil {
		return Prefix{}, err
	}
	return Prefix{Settled: settled, Retained: retained, Retains: retains}, nil
}

// DeferredIn reports, inside tx, the earliest record this node holds and could
// not apply whose declared scope intersects s — the framework's own coverage
// probe, answered in the caller's snapshot rather than a transaction of its
// own.
//
// THE INDEX, NEVER THE EARLIEST RECORD. Whether a retained record is about the
// objects a caller is reading is a question about EVERY retained record's
// scope: the earliest one is usually about something else, and a caller that
// asked only it would read a later record about exactly its objects as none.
//
// Three-valued in the usual way: a hit, a definite miss, and an error that says
// nothing either way.
func DeferredIn(ctx context.Context, tx *sql.Tx, d Domain, s ScopeSet) (
	Deferral, bool, error) {

	if s.Empty() {
		// A PROBE THAT NAMES NOTHING cannot be answered "no": the
		// framework's own reader refuses the same question rather than
		// letting an empty scope read as uncovered.
		return Deferral{}, false, fmt.Errorf("statelog: a coverage probe over "+
			"%s names no objects, so nothing can be said about what a retained "+
			"record would cover", d.Name())
	}
	t, err := newTables(d)
	if err != nil {
		return Deferral{}, false, err
	}
	return t.deferredIn(ctx, tx, s)
}
