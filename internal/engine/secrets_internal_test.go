package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/secrets"
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

// engineWithSecrets is a node with the fleet's secret store, as every node has
// it, and that store as its writers see it.
func engineWithSecrets(t *testing.T) (*Engine, *fleetsecrets.Store) {
	t.Helper()
	_, cipher := testKeyring(t)
	fleet := coordmem.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}, cipher: cipher}
	return e, fleetsecrets.New(fleet, cipher)
}

// THE STORE WINS OVER THE ENVIRONMENT: rotation is an UPDATE of one row, and a
// stale `.env` shadowing it would make the rotation appear to work and change
// nothing.
func TestARotatedSecretBeatsAStaleEnvironment(t *testing.T) {
	e, sv := engineWithSecrets(t)
	t.Setenv("SOME_TOKEN", "the-stale-one-from-dot-env")

	if got := e.resolver().Value("${SOME_TOKEN}"); got != "the-stale-one-from-dot-env" {
		t.Fatalf("before any store read, ${SOME_TOKEN} = %q", got)
	}
	if err := sv.Set(t.Context(), "SOME_TOKEN", "the-rotated-one",
		testAuthor, secrets.SourceAPI, time.Now().UTC()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e.refreshSecrets(t.Context())

	if got := e.resolver().Value("${SOME_TOKEN}"); got != "the-rotated-one" {
		t.Fatalf("${SOME_TOKEN} = %q, want the rotated value", got)
	}
}

// A NAME THE STORE DOES NOT HOLD still resolves from the environment: the
// store is a front for credentials, not a replacement for configuration.
func TestTheEnvironmentStillAnswersWhatTheStoreDoesNot(t *testing.T) {
	e, sv := engineWithSecrets(t)
	t.Setenv("PLAIN_URL", "https://tracker.example.com")
	if err := sv.Set(t.Context(), "SOME_TOKEN", "sealed",
		testAuthor, secrets.SourceAPI, time.Now().UTC()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e.refreshSecrets(t.Context())

	if got := e.resolver().Value("${PLAIN_URL}"); got != "https://tracker.example.com" {
		t.Fatalf("${PLAIN_URL} = %q, want the environment's", got)
	}
}

// AN UNREADABLE STORE LEAVES THE PREVIOUS SNAPSHOT STANDING rather than
// falling back to the environment, which on a store blip during an apply would
// silently swap every rotated credential for whatever the process booted with.
func TestAnUnreadableStoreKeepsThePreviousSnapshot(t *testing.T) {
	e, sv := engineWithSecrets(t)
	t.Setenv("SOME_TOKEN", "the-stale-one")
	if err := sv.Set(t.Context(), "SOME_TOKEN", "the-rotated-one",
		testAuthor, secrets.SourceAPI, time.Now().UTC()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e.refreshSecrets(t.Context())

	// The keyring is lost: a dropped key, or a row from a peer this node
	// cannot open.
	_, stranger := testKeyring(t)
	e.cipher = stranger
	e.refreshSecrets(t.Context())

	if got := e.resolver().Value("${SOME_TOKEN}"); got != "the-rotated-one" {
		t.Fatalf("${SOME_TOKEN} = %q, want the previous snapshot to keep "+
			"serving rather than the environment taking over", got)
	}
}

// A NODE WITH NO STORE AT ALL is the env-only case, never reported as a
// missing keyring: the two have different fixes.
func TestANodeWithNoStoreResolvesFromTheEnvironment(t *testing.T) {
	e := &Engine{}
	t.Setenv("SOME_TOKEN", "from-the-environment")
	e.refreshSecrets(t.Context())
	if got := e.resolver().Value("${SOME_TOKEN}"); got != "from-the-environment" {
		t.Fatalf("${SOME_TOKEN} = %q", got)
	}
}

// A KEYRING THAT IS CONFIGURED BUT BROKEN FAILS THE BOOT: degrading quietly to
// environment resolution would give a node that looks healthy and reads none
// of the rotated secrets.
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

// NO KEYRING CONFIGURED FAILS THE BOOT, naming the field, even on a node that
// booted with no company and so meets no signer yet: every node needs the
// keyring, and the engine refuses to build rather than hand every seam beneath
// it a nil to interpret.
func TestNoKeyringConfiguredFailsTheBoot(t *testing.T) {
	t.Parallel()
	cipher, err := openCipher(&config.Bootstrap{})
	if err == nil || !strings.Contains(err.Error(), "secrets.keys") {
		t.Fatalf("openCipher(no keyring) = %v, want a refusal naming secrets.keys", err)
	}
	if cipher != nil {
		t.Fatal("a cipher was built for a node with no keys")
	}
	if cipher, err = openCipher(nil); err == nil || cipher != nil {
		t.Fatalf("openCipher(nil) = %v, %v, want a refusal", cipher, err)
	}
}

// "THIS NODE HAS NO SECRET STORE" AND "THE STORE IS EMPTY" resolve identically
// but are different facts; only the second has a snapshot.
func TestOnlyARealStoreProducesASnapshot(t *testing.T) {
	t.Parallel()
	if (&Engine{}).refreshSecrets(t.Context()) {
		t.Error("an engine with no store reported that it loaded a snapshot")
	}

	fleet := coordmem.NewFleet()
	if (&Engine{backends: &Backends{Fleet: fleet}}).refreshSecrets(t.Context()) {
		t.Error("an engine with no keyring reported that it loaded a snapshot")
	}

	_, cipher := testKeyring(t)
	e := &Engine{backends: &Backends{Fleet: fleet}, cipher: cipher}
	if !e.refreshSecrets(t.Context()) {
		t.Error("an empty but real store produced no snapshot")
	}
}

// NO ${VAR} REACHES THE ENGINE'S OWN KEYS: the blind-index key shares the
// fleet's bucket with the operator's credentials, and `${…}` in an `mcp_env`
// would hand it to a child process. So the snapshot holds the operator's rows
// alone and no reference can spell an engine name: neither the snapshot, a
// lookup nor an expansion answers with one.
func TestNoReferenceReachesTheEnginesOwnKeys(t *testing.T) {
	e, fleet := engineWithSecrets(t)
	const key = "iam/blind-index-key"
	if created, err := fleet.Estate().Create(t.Context(), key, "the-blind-index-key",
		secrets.Author{Name: "node-a", Kind: "system"}, secrets.SourceIAM,
		time.Now().UTC()); err != nil || !created {
		t.Fatalf("mint the engine's key: (%v, %v)", created, err)
	}
	if err := fleet.Set(t.Context(), "GL", "glpat-x", testAuthor, secrets.SourceAPI,
		time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	values, err := e.secretSnapshot(t.Context())
	if err != nil {
		t.Fatalf("secretSnapshot: %v", err)
	}
	if _, held := values[key]; held || values["GL"] != "glpat-x" {
		t.Fatalf("the snapshot holds %d values, the engine's key among them: %t",
			len(values), held)
	}
	e.refreshSecrets(t.Context())
	if got, found := e.LookupSecret(key); found || got != "" {
		t.Errorf("a lookup of the engine's key answered (%q, %t)", got, found)
	}
	if got := e.Resolve("${" + key + "}"); strings.Contains(got, "the-blind-index-key") {
		t.Errorf("a reference to the engine's key expanded to %q", got)
	}
	if got := e.Resolve("${GL}"); got != "glpat-x" {
		t.Errorf("the operator's own ${GL} = %q, want its value", got)
	}
}
