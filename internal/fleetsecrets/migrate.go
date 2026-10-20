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
// # Decided at the version it was judged against
//
// The fleet is read once, and the write that carries a later local row onto
// it is CONDITIONED ON THE ROW THAT READ RETURNED: a create where the fleet
// held none, an update at the version it held. A plain put of a judgement
// made earlier is how a rotation that landed through a running peer after the
// read — a later write than the one being carried — was overwritten by the
// older offline value, so the rule the pass states was broken by the very
// window it runs in. A write that loses is a row that moved, and it is read
// again and judged again on what it holds now, as a rekey judges a row that
// moved under it.
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
		// NOT EVEN A MISSING KEYRING IS SKIPPED. That was a no-op once,
		// for a node that kept its secrets in the environment; every node
		// holds a keyring now, so a local store that cannot be read is a
		// fault like any other, and a silent skip would leave this node's
		// rows where no peer can see them with nothing said.
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
	fleetAt := make(map[string]secrets.Record, len(onFleet))
	for _, row := range onFleet {
		fleetAt[row.Name] = row
	}

	var moved, replaced, superseded []string
	for _, row := range local {
		at, held := fleetAt[row.Name]
		outcome, err := to.carry(ctx, from, row, at, held)
		if err != nil {
			return moved, err
		}
		switch outcome {
		case carriedOnto:
			moved = append(moved, row.Name)
		case carriedOver:
			moved = append(moved, row.Name)
			replaced = append(replaced, row.Name)
		case supersededBy:
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

// carried is what [Store.carry] did with one local row.
type carried int

const (
	// carriedOnto is a row the fleet did not hold, created there.
	carriedOnto carried = iota + 1
	// carriedOver is a row written after the fleet's, which replaced it.
	carriedOver
	// supersededBy is a row the fleet holds a later write of, left alone.
	supersededBy
)

// carriedRowAttempts bounds how many times a migration judges a row again
// after the fleet's moved under it, for [movedRowAttempts]' reason: the
// writers a row can meet while a node boots are few and each lands once.
const carriedRowAttempts = movedRowAttempts

// carry writes one local row onto the fleet when it is the later of the two
// writes, conditioned on the fleet's row as it was judged — onFleet, which
// held says is there — and judges it again on what the fleet holds now
// whenever that write loses.
//
// The ORIGINAL author — who, of what kind, through which credential — and the
// ORIGINAL instant are preserved: "who set this" is the question the
// provenance columns exist to answer, and "when" is what the next migration
// compares against. Stamped with this pass's own clock, the record would say
// when this node booted rather than when its operator wrote the value, and a
// second stopped node's later offline write would read as the earlier one —
// and be dropped — whenever this node happened to start first.
//
// THE SOURCE IS THE ONE FACT REPLACED, with this pass's own
// [secrets.SourceMigrated], because it names the write path and the write that
// put the row on the fleet was this one: the stopped node's word (`cli`,
// `provision`, `llm-login`) named a write to its own table, which no peer ever
// saw.
func (s *Store) carry(ctx context.Context, from LocalStore, row secrets.Record,
	onFleet secrets.Record, held bool) (carried, error) {

	if held && !row.UpdatedAt.After(onFleet.UpdatedAt) {
		return supersededBy, nil
	}
	if s == nil || s.cipher == nil {
		return 0, fmt.Errorf("fleetsecrets: migrate %s: %w", row.Name, secrets.ErrNoKeyring)
	}
	// THE OPERATOR'S NAMESPACE AND THE REFERENCE GRAMMAR, as every write
	// onto the fleet is held to: a local row was checked when it was
	// written, and this is the write that puts it where every node reads.
	if err := operatorName(row.Name); err != nil {
		return 0, fmt.Errorf("fleetsecrets: migrate %s: %w", row.Name, err)
	}
	if err := secrets.CheckName(row.Name); err != nil {
		return 0, fmt.Errorf("fleetsecrets: migrate %s: %w", row.Name, err)
	}
	value, err := from.Get(ctx, row.Name)
	if err != nil {
		return 0, fmt.Errorf("fleetsecrets: open this node's %s to migrate it: %w",
			row.Name, err)
	}
	rec, err := s.seal(provenance(row.Name, secrets.Author{
		Name: row.UpdatedBy, Kind: row.UpdatedByKind, OperatorID: row.OperatorID,
	}, secrets.SourceMigrated, row.UpdatedAt), value)
	if err != nil {
		return 0, err
	}
	for attempt := 1; ; attempt++ {
		if held && !row.UpdatedAt.After(onFleet.UpdatedAt) {
			return supersededBy, nil
		}
		var wrote bool
		if held {
			wrote, err = s.fleet.UpdateSecret(ctx, rec, onFleet.Version)
		} else {
			wrote, err = s.fleet.CreateSecret(ctx, rec)
		}
		if err != nil {
			return 0, fmt.Errorf("fleetsecrets: migrate %s: %w", row.Name, err)
		}
		if wrote {
			if held {
				return carriedOver, nil
			}
			return carriedOnto, nil
		}
		if attempt == carriedRowAttempts {
			return 0, fmt.Errorf("fleetsecrets: %s was rewritten on the fleet %d "+
				"times while this node migrated its own copy; the copy is kept and "+
				"the next start tries again", row.Name, carriedRowAttempts)
		}
		current, found, err := s.fleet.Secret(ctx, row.Name)
		if err != nil {
			return 0, fmt.Errorf("fleetsecrets: re-read %s to migrate it: %w", row.Name, err)
		}
		onFleet, held = record(current), found
	}
}
