package fleetsecrets

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// MigrateSource is the provenance stamped on a migrated row.
//
// Distinct from "cli" and "provision" so a listing says where a value came
// from: an operator looking at a row written by this pass is looking at
// something they set with `crewlet secrets set` while this node's engine was
// stopped, and the original provenance is genuinely gone — the local row's own
// UpdatedBy and UpdatedAt are preserved, but the write that put it on the KV
// was this one.
const MigrateSource = "migrated"

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
// # It is not best effort
//
// A row that cannot be copied is NOT removed, and the error is returned. The
// alternative — logging and carrying on — would delete a credential this node
// is the only holder of, and the first symptom would be a vendor 401 hours
// later on a node that never had the value.
func Migrate(ctx context.Context, from LocalStore, to *Store) ([]string, error) {
	if from == nil || to == nil {
		return nil, nil
	}
	local, err := from.List(ctx)
	if err != nil {
		if errors.Is(err, secrets.ErrNoKeyring) {
			return nil, nil
		}
		return nil, fmt.Errorf("fleetsecrets: read this node's secrets: %w", err)
	}
	if len(local) == 0 {
		return nil, nil
	}
	// READ ONCE, not per name: the pass runs at boot on a node that may
	// hold dozens of rows, and asking the KV per row would turn a startup
	// step into a round trip storm against a broker that is also serving
	// every other node's reconcile.
	onFleet, err := to.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("fleetsecrets: read the fleet's secrets: %w", err)
	}
	fleetAt := make(map[string]time.Time, len(onFleet))
	for _, row := range onFleet {
		fleetAt[row.Name] = row.UpdatedAt
	}

	var moved, replaced, superseded []string
	for _, row := range local {
		at, held := fleetAt[row.Name]
		later := !held || row.UpdatedAt.After(at)
		if later {
			value, err := from.Get(ctx, row.Name)
			if err != nil {
				return moved, fmt.Errorf(
					"fleetsecrets: open this node's %s to migrate it: %w",
					row.Name, err)
			}
			// The ORIGINAL author and the ORIGINAL instant are
			// preserved and the source is stamped: "who set this" is
			// the question the provenance columns exist to answer, and
			// "when" is what the next migration compares against.
			// Stamped with this pass's own clock, the record would say
			// when this node booted rather than when its operator wrote
			// the value, and a second stopped node's later offline
			// write would read as the earlier one — and be dropped —
			// whenever this node happened to start first.
			if err := to.Set(ctx, row.Name, value, row.UpdatedBy, MigrateSource, row.UpdatedAt); err != nil {
				return moved, fmt.Errorf("fleetsecrets: migrate %s: %w", row.Name, err)
			}
			moved = append(moved, row.Name)
			if held {
				replaced = append(replaced, row.Name)
			}
		} else {
			superseded = append(superseded, row.Name)
		}
		if _, err := from.Unset(ctx, row.Name); err != nil {
			return moved, fmt.Errorf(
				"fleetsecrets: remove this node's copy of %s after migrating it: %w",
				row.Name, err)
		}
	}
	if len(moved) > 0 {
		// NAMES ONLY, and at info: this happens once per node and an
		// operator reading the boot log needs to see that a value they
		// set locally is now the fleet's.
		log.InfoContext(ctx, "secrets_migrated_onto_the_fleet", "names", moved,
			"replaced", replaced,
			"detail", "these were this node's own rows; every node reads them now, "+
				"and the replaced ones were written here after the fleet's value")
	}
	if len(superseded) > 0 {
		// A WARNING, because an operator set these and the fleet does not
		// hold what they set: somebody rotated them through a running node
		// after this one stopped, and that later value is kept.
		log.WarnContext(ctx, "secrets_offline_writes_superseded", "names", superseded,
			"detail", "these were set on this node while its engine was stopped, and the fleet "+
				"holds a value written after them, which is kept; set them again through a "+
				"running node if this node's value is the one meant")
	}
	return moved, nil
}
