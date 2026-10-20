package fleetsecrets

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/secrets"
)

// LocalStore is the node-local secret table this package migrates rows off.
//
// Declared here rather than imported, because this is the CONSUMER: three
// methods is all a migration needs, and a store handle that could reach the
// whole database would eventually be used for something else.
type LocalStore interface {
	List(ctx context.Context) ([]secrets.Record, error)
	Get(ctx context.Context, name string) (string, error)
	Unset(ctx context.Context, name string) (bool, error)
}

// Migrate moves a node's own secret rows onto the fleet and removes them.
//
// # Why it must both copy AND remove
//
// A node that keeps writing the local table when the KV is out of reach —
// which on the default embedded topology is every moment the engine is
// stopped — needs those rows to reach the fleet, or `crewlet secrets set`
// before a first boot would set a value nothing ever reads. Copying is that
// half.
//
// Removing is the half that is easy to skip and cannot be. A local row left
// behind is read on every subsequent boot, so a later `secrets unset` on the
// fleet would be silently undone by the stale copy resurfacing, forever. The
// migration has to terminate, and deleting the source is what terminates it.
//
// # The LATER write wins
//
// A local row is an operator's write made while this node's engine was
// stopped, and it is a ROTATION as often as a first value: `crewlet secrets
// set GL new` on a stopped node told its operator the value would reach the
// fleet at the next start. So a name the fleet already holds is replaced when
// the local row was written AFTER the fleet's — and left alone, its local copy
// removed, when the fleet's is the later one: a rotation made through a
// running node after this one stopped must not be undone by a value the
// operator had already moved on from. Both instants are wall clocks of the
// nodes that wrote them, so two writes closer together than those clocks
// agree are ordered by the skew, which is the price of honouring an offline
// write at all. Either outcome is logged by name, because both are a value an
// operator set that the fleet does or does not now hold.
//
// # Its own earlier copy is not a later write
//
// A pass that put a row on the fleet and then stopped before removing the
// local copy — the delete failed, the put's acknowledgement was lost, the
// process was killed between the two — leaves the fleet holding this very
// row: the instant it carries is the local row's own, kept exactly, under
// [secrets.SourceMigrated] and the local row's author. The next pass finds a
// tie rather than a later write, and it is this node's own copy, so the local
// row is removed as already moved. Read as a later write, as it was, it was
// reported as an offline write somebody had since rotated over, naming a value
// the fleet holds exactly as the operator set it.
//
// # It is not best effort
//
// A row that cannot be copied is NOT removed, and the error is returned. The
// alternative — logging and carrying on — would delete a credential this node
// is the only holder of, and the first symptom would be a vendor 401 hours
// later on a node that never had the value. What the pass did before it
// stopped is still reported, by name, since those rows are the fleet's now.
func Migrate(ctx context.Context, from LocalStore, to *Store) (Migration, error) {
	var done Migration
	if from == nil || to == nil {
		return done, nil
	}
	local, err := from.List(ctx)
	if err != nil {
		// NOT EVEN A MISSING KEYRING IS SKIPPED. That was a no-op once,
		// for a node that kept its secrets in the environment; every node
		// holds a keyring now, so a local store that cannot be read is a
		// fault like any other, and a silent skip would leave this node's
		// rows where no peer can see them with nothing said.
		return done, fmt.Errorf("fleetsecrets: read this node's secrets: %w", err)
	}
	if len(local) == 0 {
		return done, nil
	}
	// READ ONCE, not per name: the pass runs at boot on a node that may
	// hold dozens of rows, and asking the KV per row would turn a startup
	// step into a round trip storm against a broker that is also serving
	// every other node's reconcile.
	onFleet, err := to.List(ctx)
	if err != nil {
		return done, fmt.Errorf("fleetsecrets: read the fleet's secrets: %w", err)
	}
	fleetRows := make(map[string]secrets.Record, len(onFleet))
	for _, row := range onFleet {
		fleetRows[row.Name] = row
	}

	// REPORTED ON EVERY WAY OUT, a failed pass included: a row whose put
	// landed is the fleet's whatever happened after it, and a pass that
	// returned before its report named it in no log at all.
	defer done.report(ctx)
	for _, row := range local {
		held, ok := fleetRows[row.Name]
		switch {
		case !ok || row.UpdatedAt.After(held.UpdatedAt):
			value, err := from.Get(ctx, row.Name)
			if err != nil {
				return done, fmt.Errorf(
					"fleetsecrets: open this node's %s to migrate it: %w",
					row.Name, err)
			}
			// The ORIGINAL author — who, of what kind, through which
			// credential — and the ORIGINAL instant are preserved: "who
			// set this" is the question the provenance columns exist to
			// answer, and "when" is what the next migration compares
			// against. Stamped with this pass's own clock, the record
			// would say when this node booted rather than when its
			// operator wrote the value, and a second stopped node's
			// later offline write would read as the earlier one — and be
			// dropped — whenever this node happened to start first.
			//
			// THE SOURCE IS THE ONE FACT REPLACED, with this pass's own
			// [secrets.SourceMigrated], because it names the write path
			// and the write that put the row on the fleet was this one:
			// the stopped node's word (`cli`, `provision`, `llm-login`)
			// named a write to its own table, which no peer ever saw.
			if err := to.Set(ctx, row.Name, value, secrets.Author{
				Name: row.UpdatedBy, Kind: row.UpdatedByKind,
				OperatorID: row.OperatorID,
			}, secrets.SourceMigrated, row.UpdatedAt); err != nil {
				return done, fmt.Errorf("fleetsecrets: migrate %s: %w", row.Name, err)
			}
			done.Moved = append(done.Moved, row.Name)
			if ok {
				done.Replaced = append(done.Replaced, row.Name)
			}
		case ownCopy(row, held):
			done.AlreadyMoved = append(done.AlreadyMoved, row.Name)
		default:
			done.Superseded = append(done.Superseded, row.Name)
		}
		if _, err := from.Unset(ctx, row.Name); err != nil {
			return done, fmt.Errorf(
				"fleetsecrets: remove this node's copy of %s after migrating it: %w",
				row.Name, err)
		}
	}
	return done, nil
}

// ownCopy reports whether the fleet's row is the one a migration of local
// made: the same instant, which a migration keeps exactly, under
// [secrets.SourceMigrated], from the same author through the same credential.
// A write a running node made in the same microsecond would have to match all
// of that and come from this pass's own source to be mistaken for it, and if
// it did it holds this very author's write anyway.
func ownCopy(local, held secrets.Record) bool {
	return held.UpdatedAt.Equal(local.UpdatedAt) &&
		held.Source == secrets.SourceMigrated &&
		held.UpdatedBy == local.UpdatedBy &&
		held.UpdatedByKind == local.UpdatedByKind &&
		held.OperatorID == local.OperatorID
}

// Migration is what one pass did with each of this node's rows, by name.
type Migration struct {
	// Moved were copied onto the fleet; Replaced is the part of them that
	// replaced a value the fleet held, written before this one.
	Moved, Replaced []string

	// AlreadyMoved were on the fleet already, put there by an earlier pass
	// of this node that stopped before it removed its local copy.
	AlreadyMoved []string

	// Superseded were kept off the fleet, which holds a value written after
	// them.
	Superseded []string
}

// report logs each outcome by name, never by value.
func (m Migration) report(ctx context.Context) {
	if len(m.Moved) > 0 {
		// NAMES ONLY, and at info: this happens once per node and an
		// operator reading the boot log needs to see that a value they
		// set locally is now the fleet's.
		log.InfoContext(ctx, "secrets_migrated_onto_the_fleet", "names", m.Moved,
			"replaced", m.Replaced,
			"detail", "these were this node's own rows; every node reads them now, "+
				"and the replaced ones were written here after the fleet's value")
	}
	if len(m.AlreadyMoved) > 0 {
		log.InfoContext(ctx, "secrets_already_on_the_fleet", "names", m.AlreadyMoved,
			"detail", "an earlier pass put these on the fleet and stopped before it removed "+
				"this node's copies, which are removed now")
	}
	if len(m.Superseded) > 0 {
		// A WARNING, because an operator set these and the fleet does not
		// hold what they set: somebody rotated them through a running node
		// after this one stopped, and that later value is kept.
		log.WarnContext(ctx, "secrets_offline_writes_superseded", "names", m.Superseded,
			"detail", "these were set on this node while its engine was stopped, and the fleet "+
				"holds a value written after them, which is kept; set them again through a "+
				"running node if this node's value is the one meant")
	}
}
