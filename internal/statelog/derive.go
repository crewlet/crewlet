package statelog

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// Deriver is an [Applier] that maintains DERIVED COLUMNS — values it computes
// from the rows it already holds rather than copies out of a record: how often
// a task was reopened, a project's split of its open work, the hand-off count
// an assignment carries.
//
// # Why a derived column needs a version of its own
//
// A record version says which fields a record CARRIES, and it cannot see a
// derived column at all: the record that moves one is an ordinary record every
// build reads. What differs between builds is the RULE, and a rolling upgrade
// meets it twice — a build that adds a derived column finds rows its
// predecessor wrote without it, and a build that changes a rule finds rows the
// old rule wrote. Applied incrementally on top of either, the new rule gives
// every node a column that is a mix of two rules in proportions that depend on
// when that node upgraded, on tables a fleet asserts byte-identical.
//
// So the rule set is versioned, the version is stored ON THE CHECKPOINT ROW
// beside the rows it describes (replicated migration 0018 says why there), and
// the first boot of a build whose version differs re-derives every derived
// column from the rows in one transaction that also records the new version.
// "Differs", not "is higher": a node that adopted a NEWER build's artefact
// holds rows derived by a rule it does not have, and brings them to the rule
// it is about to maintain rather than extending them with another.
//
// # The same code, never a second copy in SQL
//
// Rederive is the domain's own Go, and the incremental maintenance in Apply is
// too. A backfill written again as a migration's UPDATE is a second
// implementation of one rule, and it answers differently the day either is
// edited — which is the drift this exists to rule out. A migration that adds a
// derived column therefore adds the column (defaulted) and nothing else; the
// version bump is what fills it.
type Deriver interface {
	// DerivationVersion is the rule set this build derives at. At least
	// 1; bumped by any change to what a derived column holds, including
	// the first one a domain adds.
	DerivationVersion() int

	// Rederive recomputes every derived column from the rows in this
	// transaction, and reports how many rows it wrote.
	//
	// THE APPLIER'S PURITY RULES HOLD HERE UNCHANGED: a function of the
	// rows and the options alone. Two nodes at one checkpoint must
	// produce the same values, and rows re-derived at position P and
	// then maintained incrementally to Q must equal rows re-derived at Q —
	// which is the property a Deriver's own test states.
	Rederive(ctx context.Context, tx *sql.Tx, opts ApplyOptions) (rows int, err error)
}

// rederive brings this node's rows to the applier's derivation rules, once,
// in one transaction on the loop's own pinned connection.
//
// NOTHING TO DO when the checkpoint already names this build's rules, and
// nothing to do when there is no checkpoint: no rows exist to derive, and the
// first transaction that writes some stamps the version on the row it
// creates. A domain whose applier is not a [Deriver] derives at zero, and a
// row naming a rule set it no longer has is simply set back to zero — there
// is no column left for the old rule to describe.
func (r *Runner) rederive(ctx context.Context, w *store.Writer) error {
	deriver, _ := r.applier.(Deriver)
	var from, rows int
	var moved bool
	err := w.Tx(ctx, func(tx *sql.Tx) error {
		moved, rows = false, 0
		stored, found, err := r.tables.readDerivation(ctx, tx)
		if err != nil || !found || stored == r.tables.derivation {
			return err
		}
		from, moved = stored, true
		if deriver != nil {
			opts := r.opts
			opts.Now = r.now()
			opts.MaxVariables = r.db.Caps().MaxVariables
			if rows, err = deriver.Rederive(ctx, tx, opts); err != nil {
				return fmt.Errorf("statelog: %s could not re-derive its rows from "+
					"derivation %d to %d: %w", r.domain.Name(), stored,
					r.tables.derivation, err)
			}
		}
		return r.tables.setDerivation(ctx, tx, r.tables.derivation)
	})
	if err != nil || !moved {
		return err
	}
	r.logger.InfoContext(ctx, "statelog_rederived",
		"domain", r.domain.Name(), "stream", r.spec.Name,
		"from", from, "to", r.tables.derivation, "rows", rows)
	return nil
}
