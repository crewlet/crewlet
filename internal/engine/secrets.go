package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
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
	return config.NewResolver(e.environment())
}

// environment is the source every resolver this node builds ends in
// ([Options.Environment]): the process's own for an engine that was not
// handed one.
func (e *Engine) environment() config.Source {
	if e.environ == nil {
		return config.EnvSource{}
	}
	return e.environ
}

// getenv reads one variable from source the way [os.Getenv] reads the
// process's: the empty string for one that is unset.
func getenv(source config.Source) func(string) string {
	return func(name string) string {
		value, _ := source.Lookup(name)
		return value
	}
}

// secretView is this node's ${VAR} resolver and the store snapshot it answers
// from, published together — see [Engine.env].
type secretView struct {
	resolver *config.Resolver

	// values is the store snapshot the resolver was built from, never nil:
	// an engine with no store holds no view at all.
	values map[string]string
}

// installSecrets publishes one snapshot as this node's resolver: THE STORE IN
// FRONT OF THIS NODE'S ENVIRONMENT ([Engine.environment]), which is the chain
// [config.WithStore] builds over the process's own. The caller holds
// [Engine.secretsMu].
func (e *Engine) installSecrets(values map[string]string) {
	e.env.Store(&secretView{
		resolver: config.NewResolver(config.MapSource(values), e.environment()),
		values:   values,
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
// rotated credential for whatever the process was booted with. There IS a
// previous snapshot on every engine [New] returns, because the boot does not
// go on without one ([Engine.bootSecrets]), and an apply refuses to build on
// an engine that holds a store and none ([Engine.Apply]).
//
// Reports whether a snapshot was installed, so a caller — and the suite —
// can tell "this node has no secret store" from "the store answered with
// nothing in it", which the resolver renders identically.
func (e *Engine) refreshSecrets(ctx context.Context) bool {
	installed, err := e.loadSecrets(ctx)
	if err != nil {
		// NO KEYRING IS NOT A QUIET CASE ANY MORE. It was a supported
		// deployment, logged at debug; [openCipher] now refuses to build
		// an engine without one, so meeting it here is a fault like any
		// other unreadable store, and it keeps the previous snapshot for
		// the same reason.
		log.ErrorContext(ctx, "secret_store_unreadable", "error", err,
			"detail", "the previous snapshot keeps serving; rotated secrets "+
				"will not be picked up until the store is readable")
	}
	return installed
}

// loadSecrets reads the secret store once and installs what it read as this
// node's snapshot, or reports why it could not. A false with no error is an
// engine holding no store at all, which installs nothing.
func (e *Engine) loadSecrets(ctx context.Context) (bool, error) {
	// SERIALISED WITH EVERY OTHER REFRESH: the store is read under the
	// same lock the snapshot is installed under, so whichever of two runs
	// later read the store later, and neither installs a snapshot older
	// than the other's.
	e.secretsMu.Lock()
	defer e.secretsMu.Unlock()
	values, err := e.secretSnapshot(ctx)
	if err != nil {
		return false, err
	}
	if values == nil {
		// NO STORE AT ALL, which is not an empty one: installing a
		// snapshot here would log secret_snapshot_loaded on a node that
		// has nowhere to load one from, and an operator reading that
		// line would conclude the store is wired when it is not.
		return false, nil
	}
	e.installSecrets(values)
	// NAMES ONLY. This is the one log line that could put a company's whole
	// credential set into a file, so it counts them instead.
	log.InfoContext(ctx, "secret_snapshot_loaded", "secrets", len(values))
	return true, nil
}

// bootSecretsFirstWait is how long a boot waits before it asks a secret store
// that did not answer again, doubling from here to [configplane.ReconcileInterval]:
// what a boot meets is a coordination group still electing its leader or a
// leaf link a moment from up, which clear in seconds, and past that the store
// is asked as often as the control plane asks for the activation pointer.
const bootSecretsFirstWait = time.Second

// bootSecrets takes the snapshot a booting node resolves its company from, and
// does not return without one.
//
// NO EPOCH IS BUILT FROM THE ENVIRONMENT ALONE on a node that holds a secret
// store. A failed read keeps the previous snapshot standing
// ([Engine.refreshSecrets]), and at boot there is no previous snapshot: a boot
// whose read failed went on to build its company from whatever the process was
// started with, so a provider whose `model` is a ${VAR} only the store held
// refused the boot, and one whose key was such a reference ran without it.
// The first apply did not heal it either, since its own read could fail the
// same way and it recorded the environment-only epoch as applied. So the boot
// holds a snapshot before it builds anything, every engine [New] returns holds
// one, and the previous-snapshot rule is true on every read after it.
//
// TWO KINDS OF FAILURE, told apart because only one of them clears by waiting:
//   - A value this node's keyring cannot open ([secrets.ErrDecrypt]), or no
//     keyring at all, fails the boot, naming the secret. Every node runs the
//     same Tier A keyring, so it is a key dropped from it, and only a restart
//     with that key back, or the value set again, changes the answer.
//   - Anything else is a store that did not answer, asked again from
//     [bootSecretsFirstWait] doubling to the reconcile interval for as long as
//     the boot's context lives. The node is past its boundary by then: its
//     probes answer, /ready says it is starting, and it holds no seat.
func (e *Engine) bootSecrets(ctx context.Context) error {
	for attempt := 1; ; attempt++ {
		_, err := e.loadSecrets(ctx)
		if err == nil {
			return nil
		}
		if errors.Is(err, secrets.ErrDecrypt) || errors.Is(err, secrets.ErrNoKeyring) {
			return fmt.Errorf("engine: the secret store holds a value this node "+
				"cannot open, so its company's ${VAR} references cannot be "+
				"resolved: put the key that sealed it back in secrets.keys, or set "+
				"the value again with `crewlet secrets set`: with this engine "+
				"stopped it writes the fleet's store itself, or, on a cluster "+
				"member or a seats node, through a running node named with "+
				"-api: %w", err)
		}
		wait := backoff.Jitter(backoff.Doubling(attempt, bootSecretsFirstWait,
			configplane.ReconcileInterval), configplane.ReconcileJitter)
		log.WarnContext(ctx, "secret_store_unreadable", "error", err,
			"attempt", attempt, "retry_in", wait,
			"detail", "no company is built until the secret store answers, so "+
				"none resolves its references from the environment alone; "+
				"/ready answers starting meanwhile")
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("engine: the boot stopped before the secret store "+
				"answered: %w", context.Cause(ctx))
		case <-timer.C:
		}
	}
}

// holdsSecretStore reports whether this engine has a store its snapshot is
// read from — every engine [New] builds, and a hand-built one only when it was
// handed one.
func (e *Engine) holdsSecretStore() bool {
	return e.backends != nil && e.backends.Fleet != nil
}

// secretSnapshot reads and unseals every stored secret, or reports why not.
//
// THE FLEET'S STORE IS THE ONLY ONE. A credential is company-wide state: it
// was the last kind living somewhere only one node could see, so a rotation
// reached the node an operator pointed the CLI at and nowhere else — and a
// command run with the engine stopped writes the fleet's store too, so there
// is no node-local half left to read.
//
// Nil values with a nil error means an Engine with no fleet store at all —
// one built by hand in a test, since [New] refuses a set of backends without
// one — which leaves the environment-only resolver in place.
func (e *Engine) secretSnapshot(ctx context.Context) (map[string]string, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil, nil
	}
	if e.cipher == nil {
		return nil, secrets.ErrNoKeyring
	}
	return fleetsecrets.New(e.backends.Fleet, e.cipher).All(ctx)
}

// openCipher builds this node's keyring cipher, or refuses the boot.
//
// A NODE WITH NO KEYRING DOES NOT START. It used to be a supported posture —
// secrets from the environment, the store unused, the company document read
// in plaintext — and every node needs the keyring now: every state-log record
// is signed and verified under it, and the document a peer fetches from the
// coordination store is authenticated by its seal. Tier A refuses a file
// without one ([config.Bootstrap.Validate]); this is the engine's own
// refusal, for a Bootstrap that did not come through that door, and it runs
// BEFORE anything opens, so a keyless node leaves no store file behind — the
// unconfigured node included, which used to start no state log until it had a
// company and so met no signer at all. A keyring that is configured but broken
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

// PersonSealer is what this node seals and opens a person's own values with:
// their name, their address, an invitation's address and a second factor's
// seed, all under the fleet keyring ([iamdomain.Sealer]).
//
// OVER THE ENGINE'S OWN CIPHER, the one that seals the company's secret store
// and signs every state-log record, so a keyring rotation is one gesture for
// all of them.
//
// NIL ONLY ON AN ENGINE WITH NO KEYRING — one built by hand in a test, since
// [New] refuses a node without one — and every write that seals is then
// refused BY NAME at the call, where a sealer over nothing would have written
// cleartext.
func (e *Engine) PersonSealer() *iamdomain.Sealer {
	if e == nil || e.cipher == nil {
		return nil
	}
	sealer, err := iamdomain.NewSealer(e.cipher)
	if err != nil {
		return nil
	}
	return sealer
}

// IdentityKeyring is the identity estate's half of a keyring rotation, or nil
// on an engine with no core runtime — one a test built by hand, since every
// engine [New] builds opens the identity estate at boot, company or none.
//
// ITS OWN HALF, because a store write cannot do it: the same keyring seals
// every person's name, address and second factor, in rows derived from the
// identity log — a node that rewrote its own would disagree with its peers, and
// a replay would write the old ciphertext back — so a rekey moves them with a
// RECORD per person, through the node's own writer ([iamdomain.Writer.Reseal]).
// The node and not the caller, for the secret store's reason: a rekey re-seals
// values it did not choose, and whoever rotated the keyring is the rotation's
// own fact, which the rekey route logs.
func (e *Engine) IdentityKeyring() *IdentityKeyring {
	reader, writer := e.IAM(), e.IAMWriter()
	if reader == nil || writer == nil {
		return nil
	}
	return &IdentityKeyring{reader: reader, writer: writer}
}

// IdentityKeyring counts and moves what the keyring seals in the identity
// estate — see [Engine.IdentityKeyring].
type IdentityKeyring struct {
	reader *iamdomain.Reader
	writer *iamdomain.Writer
}

// SealedKeys counts the values this node's identity rows hold that anything
// opens, by the keyring key each is sealed under, as of now.
func (k *IdentityKeyring) SealedKeys(ctx context.Context) (iamdomain.SealedCount, error) {
	return k.reader.SealedKeys(ctx, time.Now().UTC())
}

// Reseal moves every person's values onto this node's active keyring key.
func (k *IdentityKeyring) Reseal(ctx context.Context) (iamdomain.ResealReport, error) {
	return k.writer.Reseal(ctx, k.reader)
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
