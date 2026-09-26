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
// something they set on one node before the fleet store existed, and the
// original provenance is genuinely gone — the local row's own UpdatedBy is
// preserved, but the write that put it on the KV was this one.
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
// # The later write wins
//
// A name on both sides keeps whichever value was written LATER: the local row
// is copied over the fleet's when its UpdatedAt is after the fleet's, and is
// removed without copying otherwise, a tie included. Neither side is the
// older write by construction. A fleet row can be one a peer rotated after
// this node's local write, and copying over it would resurrect a value an
// operator rotated away from; a local row can be one an operator wrote while
// the whole fleet was stopped, after its last rotation, and dropping it would
// discard the newest value there is while the command that wrote it reported
// it stored. Refusing the local write instead is not available: on the
// default topology the fleet's store is inside the engine's own process, so
// with the fleet stopped nothing can read what it holds.
//
// The comparison is between two clocks — the host that wrote the local row
// and the node that wrote the fleet's — so two writes closer together than
// the skew between those hosts can be ordered wrongly. Preferring either side
// whatever the times orders one of the two cases above wrongly however far
// apart the writes were.
//
// A COPIED ROW KEEPS ITS OWN WRITE TIME on the fleet, and the migration's
// instant is only its ceiling. The stamp is what the next node's migration
// compares its own local rows against, so stamping the boot would make a
// value written earlier outrank one written later on a node that boots
// second; and a stamp past now — this host's clock stepped back since the
// write — would outrank every write made on a correct clock until the clocks
// passed it.
//
// # It is not best effort
//
// A row that cannot be copied is NOT removed, and the error is returned. The
// alternative — logging and carrying on — would delete a credential this node
// is the only holder of, and the first symptom would be a vendor 401 hours
// later on a node that never had the value.
func Migrate(ctx context.Context, from LocalStore, to *Store, now time.Time) ([]string, error) {
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
	fleetWrote := make(map[string]time.Time, len(onFleet))
	for _, row := range onFleet {
		fleetWrote[row.Name] = row.UpdatedAt
	}

	var moved, superseded []string
	for _, row := range local {
		if at, held := fleetWrote[row.Name]; !held || row.UpdatedAt.After(at) {
			value, err := from.Get(ctx, row.Name)
			if err != nil {
				return moved, fmt.Errorf(
					"fleetsecrets: open this node's %s to migrate it: %w",
					row.Name, err)
			}
			// The ORIGINAL author is preserved and the source is
			// stamped: "who set this" is the question the provenance
			// columns exist to answer, and answering it with the
			// migration would erase the only record of it.
			written := row.UpdatedAt
			if written.After(now) {
				written = now
			}
			if err := to.Set(ctx, row.Name, value, row.UpdatedBy, MigrateSource, written); err != nil {
				return moved, fmt.Errorf("fleetsecrets: migrate %s: %w", row.Name, err)
			}
			moved = append(moved, row.Name)
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
			"detail", "these were this node's own rows; every node reads them now")
	}
	if len(superseded) > 0 {
		// AT WARN, because the command that wrote each of these said it
		// was stored: this is the one place that says it was not kept.
		log.WarnContext(ctx, "secrets_local_superseded", "names", superseded,
			"detail", "these were written on this node while it was stopped, "+
				"and the fleet holds a value for each written at or after that "+
				"time; the fleet's is kept and this node's copy is removed")
	}
	return moved, nil
}
