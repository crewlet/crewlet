package engine

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
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

// engineWithFleetSecrets builds a node with BOTH stores, the way a real one
// has them: its own table and the fleet's shared bucket.
func engineWithFleetSecrets(t *testing.T) (*Engine, *store.SecretValues, *fleetsecrets.Store) {
	t.Helper()
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, cipher := testKeyring(t)
	fleet := coordmem.NewFleet()
	e := &Engine{backends: &Backends{Store: db, Fleet: fleet}, cipher: cipher}
	return e, db.SecretValues(cipher), fleetsecrets.New(fleet, cipher)
}

// THE FLEET'S VALUE WINS OVER A SURVIVING LOCAL ROW.
//
// A node upgraded into the fleet store still has its own rows until the boot
// migration clears them, and a rotation since then has landed on the fleet.
// If the stale local copy shadowed it, the rotation would appear to work on
// every node but this one.
func TestTheFleetsSecretBeatsASurvivingLocalRow(t *testing.T) {
	e, local, fleet := engineWithFleetSecrets(t)
	if err := local.Set(t.Context(), "GL", "the-old-token", "sam", "cli", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := fleet.Set(t.Context(), "GL", "the-rotated-token", "sam", "cli", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	e.refreshSecrets(t.Context())
	if got := e.resolver().Value("${GL}"); got != "the-rotated-token" {
		t.Fatalf("${GL} = %q, want the fleet's value", got)
	}
}

// A LOCAL ROW THE FLEET HAS NOT SEEN STILL RESOLVES, which is what keeps a
// node that was written to while it was stopped serving until its migration
// runs.
func TestALocalRowTheFleetLacksStillResolves(t *testing.T) {
	e, local, _ := engineWithFleetSecrets(t)
	if err := local.Set(t.Context(), "ONLY_LOCAL", "v", "sam", "cli", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	e.refreshSecrets(t.Context())
	if got := e.resolver().Value("${ONLY_LOCAL}"); got != "v" {
		t.Fatalf("${ONLY_LOCAL} = %q, want the node's own row", got)
	}
}

// THE BOOT MIGRATION EMPTIES THE LOCAL TABLE, which is what stops a stale row
// shadowing a later unset on the fleet at every boot from now on.
func TestTheBootMigrationMovesTheLocalRowsAndClearsThem(t *testing.T) {
	e, local, fleet := engineWithFleetSecrets(t)
	if err := local.Set(t.Context(), "GL", "glpat-x", "sam", "cli", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	e.migrateSecrets(t.Context())

	values, err := fleet.All(t.Context())
	if err != nil || values["GL"] != "glpat-x" {
		t.Fatalf("the fleet holds %v (err %v)", values, err)
	}
	rows, err := local.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("the local table still holds %+v after the migration", rows)
	}
}

// refusingFleet is a coordination store whose secret writes fail — a broker
// that is reachable for reads and refusing writes, which is what a JetStream
// re-election or a full store looks like from here.
// EMBEDDED THROUGH AN ALIAS, because coord.Plane declares a Fleet() method of
// its own: embedding coord.Fleet directly names the field "Fleet", which
// collides with that method and leaves the type satisfying nothing.
type fleetBackend = coord.Fleet

type refusingFleet struct {
	fleetBackend
}

var _ coord.Fleet = refusingFleet{}

func (refusingFleet) PutSecret(context.Context, coord.SecretRecord) error {
	return errors.New("the bucket refused the write")
}

// A FAILED MIGRATION DOES NOT FAIL THE BOOT, and the node keeps serving from
// its own rows. Taking a working node down over a store blip is worse than
// running one more boot on values only it can see — and the rows it could not
// copy must survive, or the credential is gone from the only place it existed.
func TestAFailedMigrationLeavesTheNodeServing(t *testing.T) {
	e, local, _ := engineWithFleetSecrets(t)
	if err := local.Set(t.Context(), "GL", "glpat-x", "sam", "cli", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	e.backends.Fleet = refusingFleet{fleetBackend: e.backends.Fleet}

	e.migrateSecrets(t.Context())

	rows, err := local.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the local rows are %+v, want the uncopied one kept: deleting "+
			"it would destroy a credential this node is the only holder of", rows)
	}
	e.refreshSecrets(t.Context())
	if got := e.resolver().Value("${GL}"); got != "glpat-x" {
		t.Fatalf("${GL} = %q, want the node still serving its own row", got)
	}
}

// THE BOOT MIGRATES BEFORE IT SNAPSHOTS, and in that order.
//
// Derived from the source rather than driven through a full boot, because the
// case that would drive it needs a real broker, a real store and a company
// config — and what can go wrong is nothing more than two lines drifting
// apart. The symptom is silent either way: with the call gone, a value set on
// a stopped node never reaches its peers; with it after the snapshot, the
// node's first epoch resolves from rows the migration is about to delete.
func TestTheBootMigratesBeforeItSnapshots(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatalf("parse run.go: %v", err)
	}
	migrate, refresh := -1, -1
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "New" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "migrateSecrets":
				migrate = fset.Position(call.Pos()).Line
			case "refreshSecrets":
				if refresh < 0 {
					refresh = fset.Position(call.Pos()).Line
				}
			}
			return true
		})
	}
	switch {
	case refresh < 0:
		t.Fatal("New no longer builds the secret snapshot, so this guard is " +
			"asserting nothing — point it at whatever replaced it")
	case migrate < 0:
		t.Fatal("New does not migrate this node's own secret rows, so a value " +
			"set while the engine was stopped never reaches its peers")
	case migrate > refresh:
		t.Fatalf("the migration runs at line %d, after the snapshot at %d: "+
			"the first epoch resolves from rows about to be deleted",
			migrate, refresh)
	}
}
