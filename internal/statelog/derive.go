package statelog

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// Deriver is an [Applier] that maintains derived columns: values computed from
// rows it holds rather than copied from a record (a task's reopen count).
//
// A record version cannot see a derived column; the RULE differs between
// builds, and a new rule applied incrementally over an old rule's rows leaves
// each node a mix that depends on when it upgraded. So the rule set's version
// is stored on the checkpoint row (migration
// `a_checkpoint_names_the_rules_that_derived_it`), and a boot whose version
// differs, higher or lower, re-derives every derived column in the transaction
// that records it. Rederive is the domain's Go, never a migration's UPDATE (a
// second copy of the rule, which drifts): a migration adds the column,
// defaulted, and the version bump fills it.
type Deriver interface {
	// DerivationVersion is the rule set this build derives at: at least 1,
	// bumped by any change to what a derived column holds, the first included.
	DerivationVersion() int

	// Rederive recomputes every derived column from the rows in tx and reports
	// how many rows it wrote. The applier's purity rules hold: a function of
	// the rows and opts alone, and rows re-derived at P then maintained to Q
	// equal rows re-derived at Q (a Deriver's own test states it).
	Rederive(ctx context.Context, tx *sql.Tx, opts ApplyOptions) (rows int, err error)
}

// rederive brings this node's rows to the applier's derivation rules, once, in
// one transaction on the loop's pinned connection. Nothing to do when the
// checkpoint names this build's rules, or when there is none (the first write
// stamps the version). A non-[Deriver] derives at zero, so a row naming rules
// it no longer has is set back to zero.
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
			// The options every batch applies under, the current epoch
			// included ([Runner.batchOptions]): it writes what an apply would.
			if rows, err = deriver.Rederive(ctx, tx, r.batchOptions(r.now())); err != nil {
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
