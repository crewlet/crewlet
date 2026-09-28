package engine

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
)

// THE ${VAR} RESOLVER THIS NODE USES, and why it is one rather than ten.
//
// Every place that reads a configured value — a provider's api_key, an
// integration's token, a per-role mcp_env, a sandbox's env block — resolves
// ${VAR} references. Each of them used to call config.EnvOnly() directly,
// which meant the resolution ORDER was decided ten times: putting the secret
// store in front of the environment would have been ten edits, and the
// eleventh call site added afterwards would have silently skipped it while
// looking exactly like the others.
//
// So the node holds one, and the store goes in front of the environment in
// exactly one place. The config package deliberately refuses a process
// global — every call site here can be handed one — so this is a field, not
// an install.
//
// # Store first, environment behind
//
// A rotated secret must win over a stale `.env` that was exported into this
// process months ago. That is the whole reason the store exists: rotation is
// an UPDATE of one row, and if the environment could shadow it the rotation
// would appear to work and change nothing.

// Resolve answers what a config value's ${VAR} references currently resolve
// to, through this node's own chain — the secret store, then the environment.
//
// Exported because the webhook edge needs it and lives outside this package:
// it verifies with the VALUE, never with the reference, and a literal
// "${GITLAB_SIGNING_SECRET}" reaching a verifier refuses every delivery the
// third-party app sends.
func (e *Engine) Resolve(value string) string { return e.resolver().Value(value) }

// LookupSecret answers what ONE ${VAR} name resolves to on this node, and
// whether anything answered.
//
// Three-valued in the way that matters to an operator surface: the bool
// separates "the store and the environment both have nothing for this name"
// from "it resolved to the empty string", and the setup screen renders those
// differently. Resolve cannot make that distinction, because expansion
// treats an empty answer as found.
func (e *Engine) LookupSecret(name string) (string, bool) { return e.resolver().LookupOK(name) }

// resolver is the chain this node resolves ${VAR} through.
//
// Never nil: until a snapshot is installed — and on an engine holding no
// store at all — it resolves from the environment alone.
func (e *Engine) resolver() *config.Resolver {
	if v := e.env.Load(); v != nil {
		return v.resolver
	}
	return config.EnvOnly()
}

// secretView is this node's ${VAR} resolver and the store snapshot it answers
// from, published together — see [Engine.env].
type secretView struct {
	resolver *config.Resolver

	// values is the store snapshot the resolver was built from, never nil:
	// an engine with no store holds no view at all.
	values map[string]string
}

// installSecrets publishes one snapshot as this node's resolver. The caller
// holds [Engine.secretsMu].
func (e *Engine) installSecrets(values map[string]string) {
	e.env.Store(&secretView{
		resolver: config.WithStore(config.MapSource(values)), values: values,
	})
}

// refreshSecrets rebuilds the resolver from the secret store.
//
// A SNAPSHOT, not a live query, because ${VAR} expansion happens deep inside
// config walking — per role, per provider, per MCP server — and a database
// round trip there would put the store on the path of every config read.
//
// Refreshed on every apply, which is also the documented ROTATION GESTURE:
// re-activating an unchanged revision is how an operator picks up a secret
// they just rotated, and it works precisely because the snapshot is rebuilt
// here rather than cached for the life of the process.
//
// # A failure leaves the previous snapshot standing
//
// It does NOT fall back to the environment. Falling back is the stale-.env
// shadowing this whole mechanism exists to prevent, and it would happen at
// the worst moment — a store blip during an apply — silently swapping every
// rotated credential for whatever the process was booted with.
// Reports whether a snapshot was installed, so a caller — and the suite —
// can tell "this node has no secret store" from "the store answered with
// nothing in it", which the resolver renders identically.
func (e *Engine) refreshSecrets(ctx context.Context) bool {
	// SERIALISED WITH THE CHART'S RE-READ, which merges into the snapshot
	// this replaces: the store is read under the same lock the snapshot is
	// installed under, so whichever of the two runs later read the store
	// later, and neither installs a snapshot older than the other's.
	e.secretsMu.Lock()
	defer e.secretsMu.Unlock()
	values, err := e.secretSnapshot(ctx)
	if err != nil {
		// NO KEYRING IS NOT A QUIET CASE ANY MORE. It was a supported
		// deployment, logged at debug; [openCipher] now refuses to build
		// an engine without one, so meeting it here is a fault like any
		// other unreadable store, and it keeps the previous snapshot for
		// the same reason.
		log.ErrorContext(ctx, "secret_store_unreadable", "error", err,
			"detail", "the previous snapshot keeps serving; rotated secrets "+
				"will not be picked up until the store is readable")
		return false
	}
	if values == nil {
		// NO STORE AT ALL, which is not an empty one: installing a
		// snapshot here would log secret_snapshot_loaded on a node that
		// has nowhere to load one from, and an operator reading that
		// line would conclude the store is wired when it is not.
		return false
	}
	e.installSecrets(values)
	// NAMES ONLY. This is the one log line that could put a company's whole
	// credential set into a file, so it counts them instead.
	log.InfoContext(ctx, "secret_snapshot_loaded", "secrets", len(values))
	return true
}

// migrateSecrets moves this node's own secret rows onto the fleet, once.
//
// Run at BOOT and nowhere else, because that is the only moment both stores
// are reachable and nothing is resolving yet. `crewlet secrets set` on a
// stopped node writes the local table — the default topology's broker lives
// in the engine's own process, so there is nowhere else for it to go — and
// this is what carries those rows to the peers.
//
// A FAILURE DOES NOT FAIL THE BOOT. [fleetsecrets.Migrate] removes nothing it
// did not manage to copy, so a broken pass leaves the local rows intact and
// [Engine.secretSnapshot] keeps reading them: the node runs on exactly what
// it ran on before, and the next start retries. Failing the boot instead
// would take a working node down over a store blip, and carrying on silently
// would be worse still — hence the error log naming what did not move.
func (e *Engine) migrateSecrets(ctx context.Context) {
	if e.backends == nil || e.backends.Store == nil ||
		e.backends.Fleet == nil || e.cipher == nil {
		return
	}
	_, err := fleetsecrets.Migrate(ctx,
		e.backends.Store.SecretValues(e.cipher),
		fleetsecrets.New(e.backends.Fleet, e.cipher),
		time.Now().UTC())
	if err != nil {
		log.ErrorContext(ctx, "secret_migration_incomplete", "error", err,
			"detail", "this node's own secret rows are still local and no peer "+
				"can see them; it keeps serving from them and retries at the "+
				"next start")
	}
}

// secretSnapshot reads and unseals every stored secret, or reports why not.
//
// THE FLEET'S STORE IS THE ONE, and the node's own database is read only as
// the migration path off it. A credential is company-wide state: it
// was the last kind living somewhere only one node could see, so a rotation
// reached the node an operator pointed the CLI at and nowhere else.
//
// Nil values with a nil error means an Engine with no store at all — one
// built by hand in a test, since [New] refuses a set of backends without
// them — which leaves the environment-only resolver in place.
func (e *Engine) secretSnapshot(ctx context.Context) (map[string]string, error) {
	if e.backends == nil {
		return nil, nil
	}
	cipher := e.cipher
	local := e.backends.Store
	fleet := e.backends.Fleet
	if local == nil && fleet == nil {
		return nil, nil
	}
	if cipher == nil {
		return nil, secrets.ErrNoKeyring
	}

	// THE LOCAL ROWS FIRST, so the fleet's win. In steady state there are
	// none — [Engine.migrateSecrets] emptied the table at boot — and this
	// read costs one query against an empty table. It is here for the two
	// states where the table is NOT empty: a node booting for the first
	// time after the upgrade, and one whose migration could not finish.
	// Either way the fleet's copy is what every node agrees on, so a
	// surviving local row must never shadow it.
	merged := map[string]string{}
	if local != nil {
		values, err := local.SecretValues(cipher).All(ctx)
		if err != nil {
			return nil, err
		}
		maps.Copy(merged, values)
		if len(values) > 0 {
			log.WarnContext(ctx, "secrets_still_node_local", "secrets", len(values),
				"detail", "the boot migration did not move these onto the "+
					"fleet, so no peer can see them")
		}
	}
	if fleet != nil {
		values, err := fleetsecrets.New(fleet, cipher).All(ctx)
		if err != nil {
			return nil, err
		}
		maps.Copy(merged, values)
	}
	return merged, nil
}

// openCipher builds this node's keyring cipher, or refuses the boot.
//
// A NODE WITH NO KEYRING DOES NOT START. It used to be a supported posture —
// secrets from the environment, the store unused, the company document read
// in plaintext — and every node needs the keyring now: every state-log record
// is signed and verified under it, and the document a peer fetches from the
// coordination store is authenticated by its seal. Tier A refuses a file
// without one ([config.Bootstrap.Validate]); this is the engine's own
// refusal, for a Bootstrap that did not come through that door, and it is
// what an unconfigured node — which starts no state log, and so meets no
// signer — would otherwise boot past. A keyring that is configured but broken
// fails here too, naming the key.
func openCipher(boot *config.Bootstrap) (secrets.Cipher, error) {
	if boot == nil {
		return nil, fmt.Errorf("engine: no bootstrap config, so no keyring")
	}
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		return nil, fmt.Errorf("engine: secrets keyring: %w", err)
	}
	return cipher, nil
}

// chartSealer is the chart's sealing seam over the company's secret store.
//
// # Why an adapter rather than the store itself
//
// [chart.Sealer] is one verb — seal one value — and the store's surface is
// eleven. A consumer-defined interface is what keeps the chart from importing
// a rekey, a listing and a migration it has no business with, and what lets a
// test satisfy it without a coordination backend.
//
// # It had a second verb, a blind-index key, and nothing derived a value under it
//
// A keyed blind of a seat's address was designed for a chart column the
// applier filled and nothing read, and the only reader of its key was a writer
// method nothing called — so the key was minted on demand, without the guard
// the identity estate's has against minting over a deleted one, for an index
// no row held. Both are gone: a seat's address is SEALED here like any other
// literal (the row carries its `${VAR}`), and the party registry resolves that
// reference and matches the address in memory with iam.NormalizeEmail, the
// fold the identity estate blinds under. A person's own name and address live
// under a key that can be deleted in the identity directory, not in the chart.
//
// NIL ONLY ON AN ENGINE WITH NO STORE — one built by hand in a test, since
// [New] refuses a node without a keyring or a fleet backend — and the chart's
// own write path then REFUSES a literal credential by name rather than putting
// one on a log every node applies. What nil must never mean is "store it in
// the clear".
func (e *Engine) chartSealer() chart.Sealer {
	if e.backends == nil || e.backends.Fleet == nil || e.cipher == nil {
		return nil
	}
	return &chartSealer{
		store: fleetsecrets.New(e.backends.Fleet, e.cipher),
		now:   time.Now,
	}
}

// chartSealer adapts the company's secret store to [chart.Sealer].
type chartSealer struct {
	store *fleetsecrets.Store
	now   func() time.Time
}

// Seal stores one value under the name the chart derived, recording the party
// writing the seat as its author.
func (c *chartSealer) Seal(ctx context.Context, name, value string,
	by secrets.Author) error {

	// SOURCE "chart", which is what an operator listing their secrets reads
	// to tell a credential a founder typed into a seat from one a
	// provisioner minted. The two have different remedies when they stop
	// working, and a listing that called both "api" would send somebody to
	// the wrong place. It is also what the orphan sweep asks before it
	// deletes a value nothing names ([chart.OrphanedSeals]), so it is the
	// chart's own constant rather than a literal here.
	return c.store.Set(ctx, name, value, by, chart.SealSource, c.now())
}

// PersonSealer is the per-person key store, which seals a person's own values
// AND is what the identity applier shreds through.
//
// ONE CONSTRUCTION FOR BOTH, deliberately: a sealer IS a shredder, because
// destroying somebody's key is what a removal does — and two constructions
// would be two key stores that have to agree about which key belongs to whom,
// with nothing comparing them.
//
// NIL ONLY ON AN ENGINE WITH NO STORE — one built by hand in a test, since
// [New] refuses a node without a keyring or a fleet backend. Returning a
// sealer over a nil cipher instead would make every shred report success while
// destroying nothing, which is the failure a removal exists to prevent.
func (e *Engine) PersonSealer() *iamdomain.Sealer {
	if e == nil || e.cipher == nil || e.backends.Fleet == nil {
		return nil
	}
	sealer, err := iamdomain.NewSealer(fleetsecrets.New(e.backends.Fleet, e.cipher).Estate())
	if err != nil {
		return nil
	}
	return sealer
}

// personKeys is [Engine.PersonSealer] as the applier's seam.
//
// THE CONVERSION IS EXPLICIT because a typed nil in an interface is not nil:
// returning the pointer directly would hand the applier a non-nil Shredder
// wrapping a nil Sealer, and every shred would panic on exactly the engine
// this is meant to answer nil for.
func (e *Engine) personKeys() iamdomain.Shredder {
	sealer := e.PersonSealer()
	if sealer == nil {
		return nil
	}
	return sealer
}

// PersonBlinder is where this node gets the company's address blind, or nil.
//
// A SOURCE, RESOLVED AT EACH USE, and the key it reads is MINTED by the first
// node that needs one. This used to read the key once at boot and answer nil
// when it was absent — and nothing anywhere minted it, so on every deployment
// every enrolment with an address, every invitation and every sign-in by
// address was refused for a key that did not exist.
//
// NIL ONLY ON AN ENGINE WITH NO STORE — one built by hand in a test, since
// [New] refuses a node without a keyring or a fleet backend — and the writes
// that need a blind are then refused BY NAME at the call. Returned as the
// interface with an explicit nil, never a typed one, so a caller's nil check
// means what it says.
func (e *Engine) PersonBlinder() iamdomain.Blinds {
	if e == nil || e.backends == nil || e.backends.Fleet == nil || e.cipher == nil {
		return nil
	}
	return personBlindSource{e: e}
}
