package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

func testKeyring(t *testing.T) (secrets.Keyring, secrets.Cipher) {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	ring := secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": key}}
	cipher, err := secrets.NewCipher(ring)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return ring, cipher
}

func engineWithSecrets(t *testing.T) (*Engine, *store.SecretValues) {
	t.Helper()
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, cipher := testKeyring(t)
	e := &Engine{backends: &Backends{Store: db}, cipher: cipher}
	return e, db.SecretValues(cipher)
}

// THE STORE WINS OVER THE ENVIRONMENT, which is the whole reason it exists:
// rotation is an UPDATE of one row, and if a stale `.env` exported into this
// process months ago could shadow it, the rotation would appear to work and
// change nothing.
func TestARotatedSecretBeatsAStaleEnvironment(t *testing.T) {
	e, sv := engineWithSecrets(t)
	t.Setenv("SOME_TOKEN", "the-stale-one-from-dot-env")

	if got := e.resolver().Value("${SOME_TOKEN}"); got != "the-stale-one-from-dot-env" {
		t.Fatalf("before any store read, ${SOME_TOKEN} = %q", got)
	}
	if err := sv.Set(t.Context(), "SOME_TOKEN", "the-rotated-one",
		"operator", "cli", time.Now().UTC()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e.refreshSecrets(t.Context())

	if got := e.resolver().Value("${SOME_TOKEN}"); got != "the-rotated-one" {
		t.Fatalf("${SOME_TOKEN} = %q, want the rotated value", got)
	}
}

// A NAME THE STORE DOES NOT HOLD still resolves from the environment. The
// store is a front, not a replacement: most deployments keep most of their
// configuration in the environment and put only credentials here.
func TestTheEnvironmentStillAnswersWhatTheStoreDoesNot(t *testing.T) {
	e, sv := engineWithSecrets(t)
	t.Setenv("PLAIN_URL", "https://tracker.example.com")
	if err := sv.Set(t.Context(), "SOME_TOKEN", "sealed",
		"operator", "cli", time.Now().UTC()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e.refreshSecrets(t.Context())

	if got := e.resolver().Value("${PLAIN_URL}"); got != "https://tracker.example.com" {
		t.Fatalf("${PLAIN_URL} = %q, want the environment's", got)
	}
}

// A NODE WITH NO KEYRING RESOLVES FROM THE ENVIRONMENT and does not fail:
// that is the pre-store behaviour and a supported deployment, not a
// degraded one.
func TestANodeWithNoKeyringUsesTheEnvironmentAlone(t *testing.T) {
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &Engine{backends: &Backends{Store: db}}
	t.Setenv("SOME_TOKEN", "from-the-environment")

	e.refreshSecrets(t.Context())
	if got := e.resolver().Value("${SOME_TOKEN}"); got != "from-the-environment" {
		t.Fatalf("${SOME_TOKEN} = %q", got)
	}
}

// AN UNREADABLE STORE LEAVES THE PREVIOUS SNAPSHOT STANDING rather than
// falling back to the environment. Falling back is the stale-.env shadowing
// the whole mechanism exists to prevent, and it would happen at the worst
// moment: a store blip during an apply, silently swapping every rotated
// credential for whatever the process booted with.
func TestAnUnreadableStoreKeepsThePreviousSnapshot(t *testing.T) {
	e, sv := engineWithSecrets(t)
	t.Setenv("SOME_TOKEN", "the-stale-one")
	if err := sv.Set(t.Context(), "SOME_TOKEN", "the-rotated-one",
		"operator", "cli", time.Now().UTC()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e.refreshSecrets(t.Context())

	// The keyring is lost — the operator dropped a key, or the row was
	// written by a peer this node cannot open.
	_, stranger := testKeyring(t)
	e.cipher = stranger
	e.refreshSecrets(t.Context())

	if got := e.resolver().Value("${SOME_TOKEN}"); got != "the-rotated-one" {
		t.Fatalf("${SOME_TOKEN} = %q, want the previous snapshot to keep "+
			"serving rather than the environment taking over", got)
	}
}

// A NODE WITH NO STORE AT ALL is the env-only case, and must not be reported
// as a missing keyring — the two are different situations with different
// fixes.
func TestANodeWithNoStoreResolvesFromTheEnvironment(t *testing.T) {
	e := &Engine{}
	t.Setenv("SOME_TOKEN", "from-the-environment")
	e.refreshSecrets(t.Context())
	if got := e.resolver().Value("${SOME_TOKEN}"); got != "from-the-environment" {
		t.Fatalf("${SOME_TOKEN} = %q", got)
	}
}

// A KEYRING THAT IS CONFIGURED BUT BROKEN FAILS THE BOOT. An operator asked
// for encryption; degrading quietly to environment resolution would give
// them a node that looks healthy and reads none of their rotated secrets.
func TestABrokenKeyringFailsRatherThanDegrading(t *testing.T) {
	t.Parallel()
	boot := &config.Bootstrap{}
	boot.Secrets = config.Secrets{
		ActiveKeyID: "k1",
		Keys:        []config.SecretKey{{ID: "k1", Material: "not-base64!!"}},
	}
	if _, err := openCipher(boot); err == nil {
		t.Fatal("a keyring with unusable material was accepted")
	}
}

// NO KEYRING CONFIGURED IS NOT AN ERROR, which is what keeps every existing
// deployment working.
func TestNoKeyringConfiguredIsNotAFailure(t *testing.T) {
	t.Parallel()
	cipher, err := openCipher(&config.Bootstrap{})
	if err != nil {
		t.Fatalf("openCipher: %v", err)
	}
	if cipher != nil {
		t.Fatal("a cipher was built for a node with no keys")
	}
	if cipher, err = openCipher(nil); err != nil || cipher != nil {
		t.Fatalf("openCipher(nil) = %v, %v", cipher, err)
	}
}

// "THIS NODE HAS NO SECRET STORE" AND "THE STORE IS EMPTY" render
// identically through the resolver and are different facts — one is a
// deployment without a database, the other a company that has stored
// nothing yet. Only the second has a snapshot.
func TestOnlyARealStoreProducesASnapshot(t *testing.T) {
	t.Parallel()
	if (&Engine{}).refreshSecrets(t.Context()) {
		t.Error("a node with no store reported that it loaded a snapshot")
	}

	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if (&Engine{backends: &Backends{Store: db}}).refreshSecrets(t.Context()) {
		t.Error("a node with no keyring reported that it loaded a snapshot")
	}

	_, cipher := testKeyring(t)
	e := &Engine{backends: &Backends{Store: db}, cipher: cipher}
	if !e.refreshSecrets(t.Context()) {
		t.Error("an empty but real store produced no snapshot")
	}
}
