package engine

import (
	"context"
	"encoding/base64"
	"errors"
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
// Never nil: a node with no store, or no keyring, resolves from the
// environment alone — which is the pre-store behaviour and a supported
// deployment, not a degraded one.
func (e *Engine) resolver() *config.Resolver {
	if r := e.env.Load(); r != nil {
		return r
	}
	return config.EnvOnly()
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
	values, err := e.secretSnapshot(ctx)
	if err != nil {
		if errors.Is(err, secrets.ErrNoKeyring) {
			// No keyring is a supported deployment: secrets come from the
			// environment and the store is simply not in use. Logged once
			// at debug rather than warned on every apply.
			log.DebugContext(ctx, "secret_store_unused", "reason", "this node has no keyring")
			return false
		}
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
	e.env.Store(config.WithStore(config.MapSource(values)))
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
// Nil values with a nil error means "this node has no secret store at all",
// which leaves the environment-only resolver in place — a supported
// deployment, not a degraded one.
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
		if err != nil && !errors.Is(err, secrets.ErrNoKeyring) {
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

// openCipher builds this node's keyring cipher, or reports that it has none.
//
// A NODE WITH NO KEYRING IS SUPPORTED, and gets nil rather than an error:
// secrets then come from the environment and the store is simply not in use.
// A keyring that is CONFIGURED but broken is a different thing entirely —
// an operator asked for encryption and did not get it — so that fails the
// boot rather than degrading quietly to plaintext resolution.
func openCipher(boot *config.Bootstrap) (secrets.Cipher, error) {
	if boot == nil || len(boot.Secrets.Keys) == 0 {
		return nil, nil
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
// [chart.Sealer] is two verbs — seal one value, read the blind-index key —
// and the store's surface is eleven. A consumer-defined interface is what
// keeps the chart from importing a rekey, a listing and a migration it has no
// business with, and what lets a test satisfy it without a coordination
// backend.
//
// NIL IS A REAL CONFIGURATION rather than a missing wire: a company with no
// fleet backend or no keyring has no store, and the chart's own write path
// then REFUSES a literal credential by name rather than putting one on a log
// every node applies. What nil must never mean is "store it in the clear".
func (e *Engine) chartSealer() chart.Sealer {
	if e.backends == nil || e.backends.Fleet == nil || e.cipher == nil {
		return nil
	}
	node := ""
	if e.node != nil {
		node = e.node.ID()
	}
	return &chartSealer{
		store: fleetsecrets.New(e.backends.Fleet, e.cipher),
		node:  node,
		now:   time.Now,
	}
}

// chartSealer adapts the company's secret store to [chart.Sealer].
type chartSealer struct {
	store *fleetsecrets.Store
	node  string
	now   func() time.Time
}

// Seal stores one value under the name the chart derived.
func (c *chartSealer) Seal(ctx context.Context, name, value string) error {
	// SOURCE "chart", which is what an operator listing their secrets reads
	// to tell a credential a founder typed into a seat from one a
	// provisioner minted. The two have different remedies when they stop
	// working, and a listing that called both "api" would send somebody to
	// the wrong place.
	return c.store.Set(ctx, name, value, c.node, "chart", c.now())
}

// BlindKey is the material an email's blind index is computed under.
//
// # Why it is a stored secret rather than the keyring's own key
//
// The keyring seals values and ideally no node needs it to answer a question.
// A blind index is the opposite: EVERY node computes and compares it on every
// inbound vendor payload, so its key has to be readable by every node on the
// ordinary read path. Making it a record in the company's own store is what
// keeps those two key roles apart — a fleet can rotate one without making the
// other's ciphertext unreadable.
//
// IT IS MINTED ON FIRST USE and never afterwards. Changing it would make every
// stored index unmatchable at once, with no gesture that recomputes them, so a
// rotation is a migration rather than a setting — which is why nothing here
// takes one.
func (c *chartSealer) BlindKey(ctx context.Context) ([]byte, error) {
	value, err := c.store.Get(ctx, ChartBlindIndexKey)
	switch {
	case err == nil && value != "":
		return decodeBlindKey(value)
	case err != nil && !errors.Is(err, secrets.ErrNotFound):
		return nil, fmt.Errorf("engine: read the chart's blind-index key: %w", err)
	}
	// MINTED ONCE, and a racing node's write wins harmlessly: both mint a
	// key, one lands, and the read below takes whichever did. What must not
	// happen is a node computing under a key it invented and did not store,
	// which would index every address to a value no peer can match.
	minted, err := secrets.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("engine: mint the chart's blind-index key: %w", err)
	}
	if err := c.store.Set(ctx, ChartBlindIndexKey, secrets.EncodeKey(minted),
		c.node, "chart", c.now()); err != nil {
		return nil, fmt.Errorf("engine: store the chart's blind-index key: %w", err)
	}
	stored, err := c.store.Get(ctx, ChartBlindIndexKey)
	if err != nil {
		return nil, fmt.Errorf("engine: read back the chart's blind-index "+
			"key: %w", err)
	}
	return decodeBlindKey(stored)
}

// decodeBlindKey reads the stored key back out of the form it was written in.
//
// THE READ-BACK IS WHAT MAKES THE RACE HARMLESS: two nodes that both mint one
// each store theirs, one lands, and both then read the same value. A node that
// used the key it minted without reading back would index every address to a
// value no peer can match — and nothing would report it, because the index is
// opaque by construction.
func decodeBlindKey(value string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("engine: the chart's blind-index key is not "+
			"the form it is written in: %w", err)
	}
	if len(key) == 0 {
		return nil, errors.New("engine: the chart's blind-index key is empty, " +
			"so every address would index to one value")
	}
	return key, nil
}

// ChartBlindIndexKey is the secret name the chart's blind-index key lives
// under.
//
// IN THE COMPANY'S OWN STORE, under the reference grammar every other secret
// follows, so an operator listing their secrets sees it and a rekey moves it
// with the rest.
const ChartBlindIndexKey = "CREWLET_CHART_BLIND_INDEX_KEY"

// personSealer is the per-person key store, which seals a person's own values
// AND is what the identity applier shreds through.
//
// ONE CONSTRUCTION FOR BOTH, deliberately: a sealer IS a shredder, because
// destroying somebody's key is what a removal does — and two constructions
// would be two key stores that have to agree about which key belongs to whom,
// with nothing comparing them.
//
// NIL ON A NODE WITH NO KEYRING, and that is a legitimate state rather than a
// wiring mistake: such a node cannot seal or open anything, so a removal there
// deletes the rows and the key is a peer's to destroy. Returning a sealer over
// a nil cipher instead would make every shred report success while destroying
// nothing, which is the failure a removal exists to prevent.
func (e *Engine) PersonSealer() *iamdomain.Sealer {
	if e == nil || e.cipher == nil || e.backends.Fleet == nil {
		return nil
	}
	sealer, err := iamdomain.NewSealer(fleetsecrets.New(e.backends.Fleet, e.cipher))
	if err != nil {
		return nil
	}
	return sealer
}

// personKeys is [Engine.PersonSealer] as the applier's seam.
//
// THE CONVERSION IS EXPLICIT because a typed nil in an interface is not nil:
// returning the pointer directly would hand the applier a non-nil Shredder
// wrapping a nil Sealer, and every shred would panic on a node with no
// keyring — which is exactly the node this is meant to answer nil for.
func (e *Engine) personKeys() iamdomain.Shredder {
	sealer := e.PersonSealer()
	if sealer == nil {
		return nil
	}
	return sealer
}

// personBlinder derives the company's address blind, or nil.
//
// NIL IS A DOCUMENTED POSTURE rather than a failure: a node with no company
// secret store cannot read the blind key, and the writes that need one are
// refused BY NAME at the call. A node that refused to boot would take a fleet
// down over a company setting — and a blinder built over an EMPTY key would be
// worse than either, because an empty HMAC key produces stable, plausible
// blinds that look exactly like working ones.
func (e *Engine) PersonBlinder() *iamdomain.Blinder {
	if e == nil || e.backends.Fleet == nil || e.cipher == nil {
		return nil
	}
	store := fleetsecrets.New(e.backends.Fleet, e.cipher)
	value, err := store.Get(context.Background(), iamdomain.BlindKeyName)
	if err != nil || value == "" {
		return nil
	}
	blinder, err := iamdomain.NewBlinder([]byte(value))
	if err != nil {
		return nil
	}
	return blinder
}
