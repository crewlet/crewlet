package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
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
// vendor sends.
func (e *Engine) Resolve(value string) string { return e.resolver().Value(value) }

// resolver is the chain this node resolves ${VAR} through.
//
// Never nil: a node with no store, or no keyring, resolves from the
// environment alone — which is the pre-store behaviour and a supported
// deployment, not a degraded one.
func (e *Engine) resolver() *config.Resolver {
	if r := e.env.Load(); r != nil && *r != nil {
		return *r
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
	resolved := config.WithStore(config.MapSource(values))
	e.env.Store(&resolved)
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
// the migration path off it (adr-203). A credential is company-wide state: it
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
