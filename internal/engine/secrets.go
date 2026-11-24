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

// THE NODE HOLDS ONE ${VAR} RESOLVER, so the resolution order is decided in
// one place and a new call site (an api_key, a token, an mcp_env, a sandbox
// env) cannot silently skip the store. A field, not an install: the config
// package refuses a process global.
//
// The store goes in front of the environment: rotation updates one row, and
// a stale exported `.env` that could shadow it would make it change nothing.

// Resolve answers what a config value's ${VAR} references currently resolve
// to, through this node's chain: the secret store, then the environment.
// Exported for the webhook edge, which must verify with the VALUE: a literal
// "${GITLAB_SIGNING_SECRET}" reaching a verifier refuses every delivery.
func (e *Engine) Resolve(value string) string { return e.resolver().Value(value) }

// LookupSecret answers what ONE ${VAR} name resolves to on this node, and
// whether anything answered: the setup screen renders "nothing has this name"
// and "it resolved to the empty string" differently, which Resolve cannot
// tell apart.
func (e *Engine) LookupSecret(name string) (string, bool) { return e.resolver().LookupOK(name) }

// deriveKey is a key for one purpose from this node's secrets keyring, alike on
// every node holding it ([secrets.Keyring.Derive]), or nil where the node holds
// none. A seat-access session fingerprints a credential with it.
func (e *Engine) deriveKey(info string) []byte {
	if e.boot == nil {
		return nil
	}
	ring, err := e.boot.Secrets.Keyring()
	if err != nil {
		return nil
	}
	return ring.Derive(info)
}

// resolver is the chain this node resolves ${VAR} through. Never nil: until a
// snapshot is installed, or with no store at all, it is the environment alone.
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

	// records is each of values' row, metadata only, read in the same pass:
	// when and by whom the value this node resolves was written, which its
	// expiry alarm and Settings › Models & Fleets both judge from ([Engine.storedRecords]).
	records map[string]secrets.Record
}

// installSnapshot publishes one snapshot as this node's resolver, the store
// in front of [Engine.environment] (the chain [config.WithStore] builds). The
// caller holds [Engine.secretsMu].
func (e *Engine) installSnapshot(snapshot fleetsecrets.Snapshot) {
	e.env.Store(&secretView{
		resolver: config.NewResolver(config.MapSource(snapshot.Values), e.environment()),
		values:   snapshot.Values,
		records:  snapshot.Records,
	})
}

// refreshSecrets rebuilds the resolver from the secret store, reporting
// whether a snapshot was installed ("no store" and "an empty store" resolve
// identically). A snapshot, not a live query, so ${VAR} expansion deep in
// config walking never makes a database round trip. Rebuilt on every apply,
// which is the rotation gesture: re-activating an unchanged revision picks up
// a rotated secret.
//
// A FAILURE KEEPS THE PREVIOUS SNAPSHOT, never falling back to the
// environment, which on a store blip would swap every rotated credential for
// the boot-time one. Every engine [New] returns holds one
// ([Engine.bootSecrets]), and [Engine.Apply] refuses an engine with a store
// but no snapshot.
func (e *Engine) refreshSecrets(ctx context.Context) bool {
	installed, err := e.loadSecrets(ctx)
	if err != nil {
		// No keyring is a fault here too ([openCipher] refuses an engine
		// without one), keeping the previous snapshot like any other.
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
	// Read under the install lock, so of two refreshes the later install
	// always holds the later read.
	e.secretsMu.Lock()
	defer e.secretsMu.Unlock()
	snapshot, err := e.secretSnapshot(ctx)
	if err != nil {
		return false, err
	}
	if snapshot.Values == nil {
		// No store at all, not an empty one: logging secret_snapshot_loaded
		// here would tell an operator a store is wired when it is not.
		return false, nil
	}
	e.installSnapshot(snapshot)
	// A count only: this line could otherwise log the whole credential set.
	log.InfoContext(ctx, "secret_snapshot_loaded", "secrets", len(snapshot.Values))
	return true, nil
}

// bootSecretsFirstWait is a boot's first wait before re-asking a secret store
// that did not answer, doubling to [configplane.ReconcileInterval]: a leader
// election or a leaf link coming up clears in seconds, and past that the
// store is asked as often as the activation pointer.
const bootSecretsFirstWait = time.Second

// bootSecrets takes the snapshot a booting node resolves its company from, and
// does not return without one: an epoch built from the environment alone
// would resolve a store-only ${VAR} wrong, and every later read relies on a
// previous snapshot ([Engine.refreshSecrets]). Only one failure clears by
// waiting:
//   - A value the keyring cannot open ([secrets.ErrDecrypt]), or no keyring,
//     fails the boot: it is a key dropped from the shared Tier A keyring,
//     cleared only by a restart with it back or the value set again.
//   - Anything else is retried from [bootSecretsFirstWait] while the context
//     lives; meanwhile the probes answer, /ready says starting, and the node
//     holds no seat.
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
// The fleet's store is the only one: a credential is company-wide state, so a
// rotation must reach every node, and no node-local half exists.
//
// Nil values with a nil error is an engine with no fleet store (hand-built in
// a test; [New] refuses one), leaving the environment-only resolver.
func (e *Engine) secretSnapshot(ctx context.Context) (fleetsecrets.Snapshot, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return fleetsecrets.Snapshot{}, nil
	}
	if e.cipher == nil {
		return fleetsecrets.Snapshot{}, secrets.ErrNoKeyring
	}
	return fleetsecrets.New(e.backends.Fleet, e.cipher).All(ctx)
}

// openCipher builds this node's keyring cipher, or refuses the boot: every
// state-log record is signed under it and a peer's fetched company document is
// authenticated by its seal, so a node with no keyring (or a broken one) does
// not start. The engine's own refusal, for a Bootstrap that skipped
// [config.Bootstrap.Validate]; it runs before anything opens, so a keyless
// node, an unconfigured one included, leaves no store file behind.
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
// seed, all under the fleet keyring ([iamdomain.Sealer]). Over the engine's
// own cipher, so one keyring rotation covers it, the secret store and the
// state log.
//
// Nil only with no keyring (hand-built in a test; [New] refuses one), so a
// sealing write is refused by name rather than written in cleartext.
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
// on an engine with no core runtime (hand-built in a test; [New] always opens
// the identity estate).
//
// Its own half because the sealed values live in rows derived from the
// identity log: a node rewriting its own copy would disagree with its peers
// and a replay would restore the old ciphertext, so a rekey writes a record
// per person through the node's writer ([iamdomain.Writer.Reseal]). The node,
// not the caller, writes it: the rekey re-seals values the caller did not
// choose, and the rekey route logs who rotated.
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
// A source resolved at each use, whose key the first node needing it mints:
// a key read once at boot would stay absent and refuse every address.
//
// Nil only with no store (hand-built in a test; [New] refuses one), so the
// writes needing a blind are refused by name. An untyped nil, so a caller's
// nil check means what it says.
func (e *Engine) PersonBlinder() iamdomain.Blinds {
	if e == nil || e.backends == nil || e.backends.Fleet == nil || e.cipher == nil {
		return nil
	}
	return personBlindSource{e: e}
}
