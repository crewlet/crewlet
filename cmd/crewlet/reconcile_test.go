package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// The seed is the one piece of policy in this binary: a Tier B file on the
// command line names a FILE, and a running node serves the STORE. What these
// pin is the rule joining them, which is now two rules, because there are two
// flags:
//
//   -company        bootstraps a company that does not exist yet — none in the
//                   store, none in the fleet — and is otherwise ignored, loudly
//   -import-company activates over whatever the fleet runs
//
// Both are idempotent by CONTENT: an unchanged file writes nothing however
// many times a node boots. Every case boots through [bootSeed], the decision,
// the import and the first tick together, as a node does.

func seedStore(t *testing.T) *store.DB {
	t.Helper()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "seed.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func parse(t *testing.T, doc string) *config.Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

func quiet() *slog.Logger { return logging.Get("test") }

// testNode is the node id a seed in these tests runs as.
const testNode = "node-a"

func TestAFirstRunSeedsTheStore(t *testing.T) {
	t.Parallel()
	// Without this a first run has nothing to activate: the node serves a
	// company no peer can see, and a second node started against the same
	// store finds it unconfigured.
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	plan, err := decideSeed(t.Context(), &config.Bootstrap{},
		seedOf(parse(t, companyYAML)), storeOf(db, fixtureCipher), quiet())
	if err != nil {
		t.Fatal(err)
	}
	if err := seedCompany(t.Context(), db, fleet, nil, plan, fixtureCipher, testNode,
		builds, quiet()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The POINTER moved, or nothing in the fleet reconciles.
	target, found, err := fleet.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("target: found=%v err=%v", found, err)
	}
	seeded, held, err := db.Configs().Get(t.Context(), target.RevisionID)
	if err != nil || !held {
		t.Fatalf("the pointer names %s, which is not in this node's history (held=%v err=%v)",
			target.RevisionID, held, err)
	}
	if seeded.Source != "file" {
		t.Errorf("source = %q, want the file it came from", seeded.Source)
	}
	// AT ONE INSTANT: the history shows the activation at the pointer's.
	if store.EncodeTime(seeded.ActivatedAt) != store.EncodeTime(target.At) {
		t.Errorf("the seeded revision was activated at %s here and %s on the "+
			"pointer, want one instant", seeded.ActivatedAt, target.At)
	}
	// AND NOT YET WHAT THIS NODE BOOTS ON: only an apply makes it that, so
	// a file this node turns out not to run is never what it starts on.
	if booted, found, err := db.Configs().Boot(t.Context()); err != nil || found {
		t.Errorf("the seed made this node boot on %s before applying it (err %v)",
			booted.ID, err)
	}
	applies(t, db, fleet, fixtureCipher)
	booted := bootRow(t, db)
	if booted.ID != target.RevisionID ||
		store.EncodeTime(booted.ActivatedAt) != store.EncodeTime(target.At) {
		t.Errorf("once applied this node boots on %s at %s, want %s at the pointer's %s",
			booted.ID, booted.ActivatedAt, target.RevisionID, target.At)
	}
}

func TestAnUnchangedFileSeedsNothing(t *testing.T) {
	t.Parallel()
	// A node boots many times over its life. Importing on each one would
	// mint a revision per restart and move the pointer, so every peer in
	// the fleet would rebuild its epoch every time any node restarted.
	db := seedStore(t)
	company := parse(t, companyYAML)
	for range 5 {
		if err := bootSeed(t, db, coordmemory.NewFleet(), seedOf(company), fixtureCipher); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	revisions, err := db.Configs().List(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 {
		t.Fatalf("%d revisions for five boots of one file, want 1", len(revisions))
	}
}

// AN OVERRIDE IMPORTS AN EDITED FILE, once however many times it boots.
//
// This is `-import-company`: the deliberate "this file is the company again"
// gesture. Silently doing nothing here would be the worst outcome — an
// operator names a file explicitly to make it win, restarts, and nothing
// happens, with nothing anywhere saying why.
func TestAnOverrideImportsAnEditedFileOnce(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(companyYAML, "name: Acme", "name: Acme Renamed", 1)
	for range 3 {
		if err := bootSeed(t, db, coordmemory.NewFleet(), overrideOf(parse(t, edited)), fixtureCipher); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	revisions, err := db.Configs().List(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 {
		t.Fatalf("%d revisions, want the original plus one for the edit", len(revisions))
	}
	// CHAINED, so the history says what it replaced.
	if revisions[0].ParentID != revisions[1].ID {
		t.Errorf("the new revision's parent is %q, want %q",
			revisions[0].ParentID, revisions[1].ID)
	}
	var company map[string]any
	if err := json.Unmarshal(opened(t, bootRow(t, db).Payload), &company); err != nil {
		t.Fatal(err)
	}
	if company["name"] != "Acme Renamed" {
		t.Errorf("this node boots on %v, want the edited company", company["name"])
	}
}

func TestASealedStoreDoesNotReseedOnEveryBoot(t *testing.T) {
	t.Parallel()
	// THE trap. Every stored payload is sealed, so it is ciphertext
	// and a fresh nonce makes it differ on every seal, so comparing stored
	// BYTES would import a new revision on every boot — and move the
	// pointer, so the whole fleet rebuilds its epoch every restart.
	db := seedStore(t)
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": generatedKey(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	company := parse(t, companyYAML)
	for range 4 {
		if err := bootSeed(t, db, coordmemory.NewFleet(), seedOf(company), cipher); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	revisions, err := db.Configs().List(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 {
		t.Fatalf("%d revisions for four boots, want 1 — the seed is comparing "+
			"ciphertext rather than the document", len(revisions))
	}
	// And what was stored really is sealed.
	if !secrets.Sealed(revisions[0].Payload) {
		t.Error("the revision was stored in plaintext")
	}
	if bytes.Contains(revisions[0].Payload, []byte("Acme")) {
		t.Errorf("the company name survived into the stored form: %s", revisions[0].Payload)
	}
}

// A STORE SEALED UNDER A KEY THIS NODE DOES NOT HOLD REFUSES RATHER THAN
// RESEEDING.
//
// A node deployed with the wrong keyring — another fleet's Tier A, or a key
// dropped before every revision was re-sealed off it — cannot read the
// revision it boots on. Seeding over it would replace a company nobody here can decrypt
// with one this node happens to have on disk, silently, on a restart.
func TestAStoreSealedUnderAnotherKeyRefusesRatherThanReseeding(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": generatedKey(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	company := parse(t, companyYAML)
	if err := bootSeed(t, db, coordmemory.NewFleet(), seedOf(company), cipher); err != nil {
		t.Fatal(err)
	}
	err = bootSeed(t, db, coordmemory.NewFleet(), seedOf(company), fixtureCipher)
	if !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("a seed holding another keyring = %v, want the revision it boots on "+
			"refused as undecryptable", err)
	}
	revisions, err := db.Configs().List(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 {
		t.Fatalf("%d revisions, want the sealed one left alone", len(revisions))
	}
}

// opened is a stored payload as the fixture keyring reads it.
func opened(t *testing.T, payload []byte) []byte {
	t.Helper()
	document, err := secrets.Open(fixtureCipher, payload)
	if err != nil {
		t.Fatalf("open the stored revision: %v", err)
	}
	return document
}

func generatedKey(t *testing.T) []byte {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// A node whose COORDINATION store is empty restores the company it knows (2a).
//
// This is the case the embedded topology creates on every restart of a
// single-node deployment whose broker keeps no persistent store: the local
// database still holds the fleet's revision as this node last saw it, the
// fleet's pointer is gone, and without this the node would serve a company
// that nothing points at — so every peer, and its own reconciler, would read
// it as unconfigured.
func TestANodeWithNoPointerRestoresTheCompanyItKnows(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	company := parse(t, companyYAML)

	// A first start, which seeds the revision, publishes the pointer and
	// applies it.
	first := coordmemory.NewFleet()
	if err := bootSeed(t, db, first, seedOf(company), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	was, _, err := first.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// A restart onto a FRESH coordination store, with the same file and
	// the same database.
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(company), fixtureCipher); err != nil {
		t.Fatalf("restart: %v", err)
	}
	target, found, err := fleet.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("the restarted node published no pointer: found=%v err=%v", found, err)
	}
	if target.RevisionID != was.RevisionID ||
		store.EncodeTime(target.At) != store.EncodeTime(was.At) {
		t.Fatalf("the pointer names %s at %s, want the company this node knows, %s at %s",
			target.RevisionID, target.At, was.RevisionID, was.At)
	}
}

// A BOOT PUBLISHES NO REVISION IT HOLDS IN THE CLEAR.
//
// What a node publishes with no pointer to defer to, every peer applies — and
// every peer refuses a body that is not sealed, this node included. It is
// refused here instead, naming the command this node's operator runs: at the
// decision for the revision it boots on, and at the publish for the fleet's
// revision it restores, which the boot does not otherwise open.
//
// The control is the same store holding the revision sealed: published.
//
// Mutation: drop the open before the publish and the pointer moves.
func TestABootPublishesNoRevisionItHoldsInTheClear(t *testing.T) {
	t.Parallel()
	document, err := json.Marshal(parse(t, companyYAML))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		sealed bool
		plant  func(*store.Configs, context.Context, store.Revision) error
	}{
		{"the revision it boots on", false, (*store.Configs).Serve},
		{"the fleet's revision it restores", false, (*store.Configs).SawFleet},
		{"the control", true, (*store.Configs).SawFleet},
	} {
		db := seedStore(t)
		payload := json.RawMessage(document)
		if tc.sealed {
			if payload, err = secrets.Seal(fixtureCipher, document); err != nil {
				t.Fatal(err)
			}
		}
		if err := tc.plant(db.Configs(), t.Context(), store.Revision{
			ID: "stored-before", Source: "file", CreatedBy: "not-a-node",
			CreatedByKind: iam.ActorSystem, Summary: "stored before", Payload: payload,
			CreatedAt: time.Now().UTC(), ActivatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		fleet := coordmemory.NewFleet()
		err := bootSeed(t, db, fleet, tierBSeed{Path: "company.yaml"}, fixtureCipher)
		_, published, targetErr := fleet.Target(t.Context())
		if targetErr != nil {
			t.Fatal(targetErr)
		}
		if tc.sealed {
			if err != nil || !published {
				t.Fatalf("%s: a sealed revision was not published: published=%v err=%v",
					tc.name, published, err)
			}
			continue
		}
		if !errors.Is(err, secrets.ErrUnsealedWithKey) {
			t.Fatalf("%s: a boot over an unsealed revision = %v, want it refused unsealed",
				tc.name, err)
		}
		if !strings.Contains(err.Error(), "crewlet config import") {
			t.Errorf("%s: the refusal does not name the command that brings it back "+
				"sealed: %v", tc.name, err)
		}
		if published {
			t.Errorf("%s: the boot published a revision it holds in the clear", tc.name)
		}
	}
}

// A node booting into a running fleet must CONVERGE on what the fleet already
// decided, not overwrite it with whatever its local database happens to say
// (2c). Without this rule one restarted node could roll a company back.
func TestAStaleLocalRevisionDoesNotOverwriteTheFleet(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A PEER activated something else, later.
	peer, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "peer-revision", Summary: "from a peer",
		Payload: []byte(`{"name":"Peer"}`), At: time.Now().Add(2 * time.Hour), Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: "operator", Source: "api", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}})
	if err != nil {
		t.Fatalf("peer activate: %v", err)
	}
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("restart: %v", err)
	}
	after, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.RevisionID != peer.RevisionID {
		t.Fatalf("a restarting node rolled the fleet back to %s, want the peer's %s",
			after.RevisionID, peer.RevisionID)
	}
}

// seedOf is a `-company` bootstrap seed: imported only where no company exists
// yet — none in this node's store and none the fleet runs.
func seedOf(c *config.Company) tierBSeed {
	return tierBSeed{Path: "company.yaml", Company: c}
}

// overrideOf is the `-import-company` form: activated over whatever is there.
func overrideOf(c *config.Company) tierBSeed {
	return tierBSeed{Path: "company.yaml", Override: true, Company: c}
}

// bootSeed is one boot of a data node over db, composed as `crewlet run`
// composes it: the seed decided over the store before the engine starts
// ([decideSeed]), carried out once the broker is up ([seedCompany]), and the
// first reconcile tick of an engine that applies the fleet's revision
// ([applies]). Every case here goes through all three, because the decision is
// what a boot imports on and the tick is what the next boot boots on, and a
// case that called the import alone would certify a rule no node runs.
func bootSeed(t *testing.T, db *store.DB, plane coord.Plane, seed tierBSeed,
	cipher secrets.Cipher) error {

	t.Helper()
	return bootSeedSaying(t, db, plane, seed, cipher, quiet())
}

// bootSeedSaying is [bootSeed] logging to log, for a case about what a boot
// says.
func bootSeedSaying(t *testing.T, db *store.DB, plane coord.Plane, seed tierBSeed,
	cipher secrets.Cipher, log *slog.Logger) error {

	t.Helper()
	plan, err := decideSeed(t.Context(), &config.Bootstrap{}, seed, storeOf(db, cipher), log)
	if err != nil {
		return err
	}
	if err := seedCompany(t.Context(), db, plane, nil, plan, cipher, testNode, builds, log); err != nil {
		return err
	}
	applies(t, db, plane, cipher)
	return nil
}

// applies is the first reconcile tick of a node whose engine applies the
// fleet's revision, as [engine.Reconciler] records one: the revision kept as
// the fleet's this node last saw, and — applied — what it boots on, at the
// pointer's instant. A body that does not open under cipher is refused before
// either, as the reconciler refuses it.
//
// A MODEL of the tick rather than the tick, because these cases are about what
// a boot decides and publishes, which an engine would bury under building a
// company; internal/engine holds the tick itself to these two writes.
func applies(t *testing.T, db *store.DB, plane coord.Plane, cipher secrets.Cipher) {
	t.Helper()
	target, pointed, err := plane.Target(t.Context())
	if err != nil {
		t.Fatalf("the tick's read of the pointer: %v", err)
	}
	if !pointed {
		return
	}
	payload, found, err := plane.Payload(t.Context(), target.RevisionID)
	if err != nil || !found {
		t.Fatalf("the tick's read of revision %s: found=%v err=%v", target.RevisionID, found, err)
	}
	if _, err := secrets.Open(cipher, payload); err != nil {
		return
	}
	revision := configplane.RevisionOf(target, payload)
	if err := db.Configs().SawFleet(t.Context(), revision); err != nil {
		t.Fatalf("the tick's record of the fleet's revision: %v", err)
	}
	if err := db.Configs().Serve(t.Context(), revision); err != nil {
		t.Fatalf("the tick's move of the boot row: %v", err)
	}
}

// bootRow is the revision this node boots on, failing a case where it boots on
// none.
func bootRow(t *testing.T, db *store.DB) store.Revision {
	t.Helper()
	booted, found, err := db.Configs().Boot(t.Context())
	if err != nil || !found {
		t.Fatalf("the revision this node boots on: found=%v err=%v", found, err)
	}
	return booted
}

// fileRevisions is every revision a -company or -import-company file made on
// this node, which a case about what the seed writes counts, apart from the
// fleet's revisions the tick keeps.
func fileRevisions(t *testing.T, db *store.DB) []store.Revision {
	t.Helper()
	revisions, err := db.Configs().List(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var made []store.Revision
	for _, r := range revisions {
		if r.Source == "file" {
			made = append(made, r)
		}
	}
	return made
}

// builds is a node on which every file builds: the cases that take it are
// about what the seed writes, and [TestAFileThisNodeCannotBuildIsNeverImported]
// is about the build.
func builds(*config.Company) error { return nil }

// storeOf is a [storedReader] over a store a case holds open: what
// [storeAt] reads off disk for `crewlet run`. It takes no claim on the file —
// the case holds the handle itself — so the plan it decides holds nothing to
// release.
func storeOf(db *store.DB, cipher secrets.Cipher) storedReader {
	return func(ctx context.Context) (storedState, heldStore, error) {
		state, err := readStoredState(ctx, db.Configs(), cipher)
		return state, heldStore{}, err
	}
}

// A BOOTSTRAP SEED NEVER OVERWRITES A COMPANY THAT EXISTS.
//
// This is the whole reason there are two flags. `-company` used to import
// whenever its content differed from the active revision, which made every
// restart a write: an operator edits their company live — the dashboard, PUT
// /config, `crewlet config import` — then restarts a node whose file is a
// month old, and the file wins. A deleted role comes back, a changed model
// reverts, and nothing says so.
//
// The store has to be left EXACTLY as it was: not just "still what it boots
// on", but no new revision, no moved pointer, and the same document
// underneath. A seed
// that chained a revision and then activated the old one back would pass a
// weaker check and still have rewritten the fleet's history.
func TestABootstrapSeedDoesNotOverwriteAnExistingCompany(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatal(err)
	}
	before := bootRow(t, db)
	pointerBefore, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// The stale file, booted repeatedly, exactly as a restart would.
	edited := strings.Replace(companyYAML, "name: Acme", "name: Acme Renamed", 1)
	for range 3 {
		if err := bootSeed(t, db, fleet, seedOf(parse(t, edited)), fixtureCipher); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	revisions, err := db.Configs().List(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 {
		t.Fatalf("%d revisions, want only the one that bootstrapped the store: "+
			"a stale file rewrote a live company", len(revisions))
	}
	after := bootRow(t, db)
	if after.ID != before.ID {
		t.Errorf("the revision this node boots on moved from %s to %s", before.ID, after.ID)
	}
	var company map[string]any
	if err := json.Unmarshal(opened(t, after.Payload), &company); err != nil {
		t.Fatal(err)
	}
	if company["name"] != "Acme" {
		t.Errorf("this node boots on %v, want the company already in the store", company["name"])
	}
	pointerAfter, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if pointerAfter.RevisionID != pointerBefore.RevisionID {
		t.Errorf("the activation pointer moved from %s to %s — every peer "+
			"rebuilt its epoch for a file that should have been ignored",
			pointerBefore.RevisionID, pointerAfter.RevisionID)
	}
}

// A SEED THE FLEET DID NOT TAKE IS NOT THIS NODE'S COMPANY, whichever flag
// carried it. An activation that fails fails the boot, and the revision stored
// before it is history: booted on, it was what the restart ran and, found equal
// to the file, PUBLISHED rather than seeded again — over whatever a peer had
// activated in between, whenever the seed's instant was the later one, which
// for a bootstrap is the overwrite its create-only activation exists to
// refuse.
//
// Mutation: move the boot row to the seed's revision before the fleet takes
// it, and the first assertion goes red; publish what the node boots on rather
// than seeding again, and the restart's.
func TestASeedTheFleetDidNotTakeIsNotThisNodesCompany(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		seed tierBSeed
	}{
		{"a -company bootstrap", seedOf(parse(t, companyYAML))},
		{"an -import-company override", overrideOf(parse(t, companyYAML))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := seedStore(t)
			fleet := coordmemory.NewFleet()
			plan, err := decideSeed(t.Context(), &config.Bootstrap{}, tc.seed,
				storeOf(db, fixtureCipher), quiet())
			if err != nil {
				t.Fatal(err)
			}
			err = seedCompany(t.Context(), db, unreachableActivation{fleet}, nil, plan,
				fixtureCipher, testNode, builds, quiet())
			if !errors.Is(err, errActivationUnreachable) {
				t.Fatalf("seed = %v, want the boot failed by the activation's own error", err)
			}
			if booted, found, err := db.Configs().Boot(t.Context()); err != nil {
				t.Fatal(err)
			} else if found {
				for _, made := range fileRevisions(t, db) {
					if booted.ID == made.ID {
						t.Fatalf("this node boots on %s, which no fleet took: the next "+
							"boot runs it, and offers it over the pointer", booted.ID)
					}
				}
			}
			if tc.seed.Override {
				// The override is unconditional, so its restart replacing
				// a peer's company is what it is for.
				return
			}
			// A PEER BOOTSTRAPS while this node is down.
			peer := activateAsPeer(t, fleet, "peer-revision", []byte(`{"name":"Peer"}`))
			if err := bootSeedSaying(t, db, fleet, tc.seed, fixtureCipher, quiet()); err != nil {
				t.Fatalf("the restart: %v", err)
			}
			assertFleetOn(t, fleet, peer)
		})
	}
}

// errActivationUnreachable is the activation [unreachableActivation] fails.
var errActivationUnreachable = errors.New("the coordination store did not answer")

// unreachableActivation is a fleet whose activation fails outright, as one does
// when the coordination store stops answering between a node's read of the
// pointer and its write.
type unreachableActivation struct{ coord.Plane }

func (unreachableActivation) Activate(context.Context, coord.ActivationRequest) (coord.Activation, error) {
	return coord.Activation{}, errActivationUnreachable
}

// NO FILE AT ALL IS A NORMAL WAY TO RUN A NODE. The store is authoritative, so
// a node whose company already lives there needs no Tier B document — and the
// documented bootstrap path (start the node, then PUT /config) needs the node
// to start without one.
func TestNoSeedFileLeavesTheStoreAlone(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatal(err)
	}
	before := bootRow(t, db)
	// tierBSeed with no Company: `crewlet run` with no company.yaml.
	if err := bootSeed(t, db, fleet, tierBSeed{Path: "company.yaml"}, fixtureCipher); err != nil {
		t.Fatalf("seed with no file: %v", err)
	}
	revisions, err := db.Configs().List(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 {
		t.Fatalf("%d revisions, want the store untouched", len(revisions))
	}
	if after := bootRow(t, db); after.ID != before.ID {
		t.Errorf("the revision this node boots on changed with no file to change it")
	}
}

// AND AN EMPTY STORE WITH NO FILE IS NOT AN ERROR. It is the unconfigured
// node: it has nothing to activate and nothing to say about it, and its API is
// what an operator pushes the first revision into.
func TestNoSeedAndNoRevisionIsNotAFailure(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	if err := bootSeed(t, db, coordmemory.NewFleet(), tierBSeed{Path: "company.yaml"}, fixtureCipher); err != nil {
		t.Fatalf("an unconfigured node failed to seed: %v", err)
	}
	revisions, err := db.Configs().List(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 0 {
		t.Fatalf("%d revisions, want none", len(revisions))
	}
}

// activateAsPeer points fleet at a revision a peer wrote, its payload document
// sealed under the fixture keyring as every revision a fleet publishes is.
func activateAsPeer(t *testing.T, fleet coord.Plane, id string, document []byte) coord.Activation {
	t.Helper()
	return activateSealed(t, fleet, fixtureCipher, id, document)
}

// activateSealed is [activateAsPeer] for a peer sealing under cipher.
func activateSealed(t *testing.T, fleet coord.Plane, cipher secrets.Cipher, id string,
	document []byte) coord.Activation {

	t.Helper()
	sealed, err := secrets.Seal(cipher, document)
	if err != nil {
		t.Fatal(err)
	}
	activation, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: id, Summary: "from a peer", Payload: sealed, At: time.Now(),
		Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorOperator,
			Source: "api", CreatedAt: time.Now()},
	})
	if err != nil {
		t.Fatalf("peer activate: %v", err)
	}
	return activation
}

// unseenOnce is a fleet whose pointer the first read does not see: a peer's
// activation landing between a node's read of the pointer and its own write.
type unseenOnce struct {
	coord.Plane
	seen atomic.Bool
}

func (p *unseenOnce) Target(ctx context.Context) (coord.Activation, bool, error) {
	if p.seen.CompareAndSwap(false, true) {
		return coord.Activation{}, false, nil
	}
	return p.Plane.Target(ctx)
}

// A BOOTSTRAP SEED NEVER REPLACES A COMPANY THE FLEET RUNS.
//
// "No company yet" was decided from this node's own store, and an empty store
// is not a fleet with no company: a fresh data node joining a running fleet has
// one, and so does every boot of a node without `data`, whose store is scratch.
// Each imported its -company file and pointed the whole fleet at it, over the
// company every peer was running. The fleet is asked now, and the write is
// create-only, so a pointer that lands between the read and the write is not
// replaced either — and the revision the seed stored stays as inert history,
// never what this node boots on. A file that IS the fleet's
// revision is what every node of a uniform fleet carries, and is no news.
//
// Mutation: drop the pointer read, or the create-only expectation, and the
// fleet's revision is replaced.
func TestABootstrapSeedNeverReplacesTheFleetsCompany(t *testing.T) {
	t.Parallel()
	file := parse(t, companyYAML)
	document, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a fleet running another company", func(t *testing.T) {
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		live := activateAsPeer(t, fleet, "live-revision", []byte(`{"name":"Live"}`))
		var logged bytes.Buffer
		if err := bootSeedSaying(t, db, fleet, seedOf(file), fixtureCipher,
			slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
			t.Fatalf("boot: %v", err)
		}
		assertFleetOn(t, fleet, live)
		if made := fileRevisions(t, db); len(made) != 0 {
			t.Errorf("%d revisions stored from the file, want none: it was never the company",
				len(made))
		}
		for _, want := range []string{"company_seed_ignored", "live-revision", "-import-company"} {
			if !strings.Contains(logged.String(), want) {
				t.Errorf("the ignored file was not said with %q: %s", want, logged.String())
			}
		}
	})

	t.Run("a node without data, at every boot", func(t *testing.T) {
		// ITS STORE IS SCRATCH, so the decision reads nothing and every
		// boot is a bootstrap: before the fleet was asked, each restart of
		// a seats node carrying a company.yaml pointed the fleet at it.
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		live := activateAsPeer(t, fleet, "live-revision", []byte(`{"name":"Live"}`))
		scratch := &config.Bootstrap{Store: config.Store{Scratch: true}}
		unread := func(context.Context) (storedState, heldStore, error) {
			t.Error("a scratch store was read to decide the boot")
			return storedState{}, heldStore{}, nil
		}
		for range 2 {
			plan, err := decideSeed(t.Context(), scratch, seedOf(file), unread, quiet())
			if err != nil {
				t.Fatal(err)
			}
			if err := seedCompany(t.Context(), db, fleet, nil, plan, fixtureCipher,
				testNode, builds, quiet()); err != nil {
				t.Fatalf("boot: %v", err)
			}
			assertFleetOn(t, fleet, live)
		}
	})

	t.Run("a fleet running this very file", func(t *testing.T) {
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		live := activateAsPeer(t, fleet, "live-revision", document)
		var logged bytes.Buffer
		if err := bootSeedSaying(t, db, fleet, seedOf(file), fixtureCipher,
			slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
			t.Fatalf("boot: %v", err)
		}
		assertFleetOn(t, fleet, live)
		if strings.Contains(logged.String(), "company_seed_ignored") {
			t.Errorf("a file that is the fleet's revision was reported: %s", logged.String())
		}
	})

	t.Run("a pointer that lands after the read", func(t *testing.T) {
		t.Parallel()
		db := seedStore(t)
		real := coordmemory.NewFleet()
		live := activateAsPeer(t, real, "live-revision", []byte(`{"name":"Live"}`))
		var logged bytes.Buffer
		if err := bootSeedSaying(t, db, &unseenOnce{Plane: real}, seedOf(file), fixtureCipher,
			slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
			t.Fatalf("boot: %v", err)
		}
		assertFleetOn(t, real, live)
		made := fileRevisions(t, db)
		if len(made) != 1 {
			t.Fatalf("%d revisions from the file, want the refused seed kept as history",
				len(made))
		}
		if booted := bootRow(t, db); booted.ID == made[0].ID {
			t.Errorf("a seed the fleet refused is what this node boots on: its next boot " +
				"would offer it over the fleet's")
		}
		if !strings.Contains(logged.String(), "company_seed_ignored") {
			t.Errorf("the refused seed was not said: %s", logged.String())
		}
	})
}

// assertFleetOn fails a case whose fleet has moved off the activation want.
func assertFleetOn(t *testing.T, fleet coord.Plane, want coord.Activation) {
	t.Helper()
	after, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.RevisionID != want.RevisionID || after.Epoch != want.Epoch {
		t.Errorf("the fleet moved from %s (epoch %d) to %s (epoch %d): a node's file "+
			"replaced the company the fleet runs", want.RevisionID, want.Epoch,
			after.RevisionID, after.Epoch)
	}
}

// -import-company IS JUDGED AGAINST THE REVISION THE FLEET RUNS.
//
// It compared the file with this node's own copy, so a node that was down
// while a peer changed the company — restarted with -import-company naming the
// file its own copy still was — found nothing to import and left the fleet on
// the peer's revision: the one flag that exists to make a file the company
// again silently did nothing. And a file that IS the fleet's revision imports
// nothing, wherever this node's copy stands.
//
// Mutation: compare with this node's copy alone and the stale node imports
// nothing.
func TestAnOverrideIsJudgedAgainstTheFleetsRevision(t *testing.T) {
	t.Parallel()
	file := parse(t, companyYAML)
	document, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a node whose stale copy is the file", func(t *testing.T) {
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		if err := bootSeed(t, db, fleet, seedOf(file), fixtureCipher); err != nil {
			t.Fatal(err)
		}
		mine := bootRow(t, db)
		peer := activateAsPeer(t, fleet, "peer-revision", []byte(`{"name":"Peer"}`))
		if err := bootSeed(t, db, fleet, overrideOf(file), fixtureCipher); err != nil {
			t.Fatalf("override: %v", err)
		}
		after, _, err := fleet.Target(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.RevisionID == peer.RevisionID || after.RevisionID == mine.ID {
			t.Fatalf("-import-company left the fleet on %s, want a new revision of the file",
				after.RevisionID)
		}
		payload, _, err := fleet.Payload(t.Context(), after.RevisionID)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(opened(t, payload), document) {
			t.Errorf("the fleet runs %s, want the file's document", opened(t, payload))
		}
		// BUILT ON WHAT IT REPLACED, which is the fleet's revision and not
		// this node's stale copy, and kept in this node's history so the
		// chain holds.
		imported, held, err := db.Configs().Get(t.Context(), after.RevisionID)
		if err != nil || !held || imported.ParentID != peer.RevisionID {
			t.Errorf("the import is %s (parent %q, held=%v err=%v), want it built on the "+
				"fleet's %s", imported.ID, imported.ParentID, held, err, peer.RevisionID)
		}
		if _, held, err := db.Configs().Get(t.Context(), peer.RevisionID); err != nil || !held {
			t.Errorf("the revision the import replaced is not in this node's history "+
				"(held=%v err=%v)", held, err)
		}
		if booted := bootRow(t, db); booted.ID != after.RevisionID {
			t.Errorf("once applied this node boots on %s, want the import %s",
				booted.ID, after.RevisionID)
		}
	})

	t.Run("a fleet revision this node cannot open", func(t *testing.T) {
		// THE FILE ITSELF, sealed under a key this node does not hold: the
		// override cannot be shown it is the file, so it replaces it as it
		// asks — and says that it could not compare.
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		foreign, err := secrets.NewCipher(secrets.Keyring{
			ActiveID: "k1", Keys: map[string][]byte{"k1": generatedKey(t)},
		})
		if err != nil {
			t.Fatal(err)
		}
		before := activateSealed(t, fleet, foreign, "foreign-revision", document)
		var logged bytes.Buffer
		if err := bootSeedSaying(t, db, fleet, overrideOf(file), fixtureCipher,
			slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
			t.Fatalf("override: %v", err)
		}
		after, _, err := fleet.Target(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.RevisionID == before.RevisionID {
			t.Errorf("-import-company left the fleet on %s, a revision it could not "+
				"read, want the file imported", before.RevisionID)
		}
		for _, want := range []string{"company_import_uncompared", "foreign-revision"} {
			if !strings.Contains(logged.String(), want) {
				t.Errorf("the uncompared override was not said with %q: %s", want,
					logged.String())
			}
		}
	})

	t.Run("a company this node knows and cannot open, with no pointer", func(t *testing.T) {
		// THE STATE THE OVERRIDE EXISTS TO REPAIR: the company this node
		// knows was sealed under a key its keyring has since dropped, and
		// the fleet's pointer is gone. The file replaces it, said as an
		// uncompared import, rather than the boot being refused over the
		// revision it was asked to replace.
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		foreign, err := secrets.NewCipher(secrets.Keyring{
			ActiveID: "k1", Keys: map[string][]byte{"k1": generatedKey(t)},
		})
		if err != nil {
			t.Fatal(err)
		}
		lost := activateSealed(t, fleet, foreign, "lost-revision", document)
		// Kept as the fleet's, as a tick that could not open it keeps it;
		// then the coordination store is rebuilt and the pointer is gone.
		refuses(t, db, fleet)
		fleet = coordmemory.NewFleet()
		var logged bytes.Buffer
		if err := bootSeedSaying(t, db, fleet, overrideOf(file), fixtureCipher,
			slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
			t.Fatalf("override over a company this node cannot open: %v", err)
		}
		after, pointed, err := fleet.Target(t.Context())
		if err != nil || !pointed {
			t.Fatalf("the override published nothing (pointed=%v err=%v)", pointed, err)
		}
		imported, held, err := db.Configs().Get(t.Context(), after.RevisionID)
		if err != nil || !held || imported.ParentID != lost.RevisionID {
			t.Errorf("the import is %s (parent %q, held=%v err=%v), want the file built "+
				"on the company it replaced, %s", after.RevisionID, imported.ParentID, held,
				err, lost.RevisionID)
		}
		for _, want := range []string{"company_import_uncompared", "lost-revision"} {
			if !strings.Contains(logged.String(), want) {
				t.Errorf("the uncompared override was not said with %q: %s", want,
					logged.String())
			}
		}
	})

	t.Run("the company this node knows, with no pointer", func(t *testing.T) {
		// THE OVERRIDE IS A CHOICE MADE NOW: the file is the revision this
		// node knows, so it is published again, at this node's clock and not
		// at the instant the revision last activated — a peer restoring a
		// pointer lost later weighs what it remembers against that instant,
		// and an operator's choice read as old loses to a stale memory.
		//
		// Mutation: publish at the revision's own instant and the pointer
		// carries it.
		t.Parallel()
		db := seedStore(t)
		if err := bootSeed(t, db, coordmemory.NewFleet(), seedOf(file), fixtureCipher); err != nil {
			t.Fatal(err)
		}
		known := bootRow(t, db)
		fleet := coordmemory.NewFleet()
		before := time.Now().UTC()
		if err := bootSeed(t, db, fleet, overrideOf(file), fixtureCipher); err != nil {
			t.Fatalf("override: %v", err)
		}
		after, pointed, err := fleet.Target(t.Context())
		if err != nil || !pointed {
			t.Fatalf("the override published nothing (pointed=%v err=%v)", pointed, err)
		}
		if after.RevisionID != known.ID || after.Restored || after.At.Before(before) {
			t.Errorf("the pointer names %s at %s (restored=%v), want the known %s chosen "+
				"at or after %s, not at its own %s", after.RevisionID, after.At,
				after.Restored, known.ID, before, known.ActivatedAt)
		}
		if made := fileRevisions(t, db); len(made) != 1 {
			t.Errorf("%d revisions of the file stored, want the one the first boot made",
				len(made))
		}
	})

	t.Run("the fleet's own revision", func(t *testing.T) {
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		live := activateAsPeer(t, fleet, "live-revision", document)
		if err := bootSeed(t, db, fleet, overrideOf(file), fixtureCipher); err != nil {
			t.Fatalf("override: %v", err)
		}
		assertFleetOn(t, fleet, live)
		if made := fileRevisions(t, db); len(made) != 0 {
			t.Errorf("%d revisions stored for a file the fleet already runs, want none",
				len(made))
		}
	})
}

// A FILE THIS NODE CANNOT BUILD IS NEVER IMPORTED, and one it is not importing
// is never built.
//
// The engine boots unconfigured where the file is to be imported, so nothing
// builds the file until the first reconcile applies it — after the seed has
// stored it and pointed the fleet at it. A company valid on paper that this
// node could not build (an E2B key that resolves to nothing) was therefore
// the fleet's company, an override over the company every peer ran included,
// refused by this node's every apply and by its next boot, while the boot
// that wrote it went on serving nothing. So the seed builds the file first
// and a refusal writes nothing, the way the boot on the file once refused with
// nothing written.
//
// The other half matters as much: a node restarting into a running fleet with
// a stale file it ignores must not be refused over that file. The build is the
// import's, never the boot's.
//
// Mutations: build after the activation and the fleet moves; build in the
// bootstrap branch before the pointer is read and the ignored file refuses the
// boot.
func TestAFileThisNodeCannotBuildIsNeverImported(t *testing.T) {
	t.Parallel()
	unbuildable := errors.New("e2b: providers.sandbox.api_key resolved empty")
	refuses := func(*config.Company) error { return unbuildable }
	file := parse(t, companyYAML)

	boot := func(t *testing.T, db *store.DB, fleet coord.Plane, seed tierBSeed,
		build revisionBuild) error {
		t.Helper()
		plan, err := decideSeed(t.Context(), &config.Bootstrap{}, seed,
			storeOf(db, fixtureCipher), quiet())
		if err != nil {
			t.Fatal(err)
		}
		return seedCompany(t.Context(), db, fleet, nil, plan, fixtureCipher, testNode,
			build, quiet())
	}
	unwritten := func(t *testing.T, db *store.DB) {
		t.Helper()
		if revisions, _ := db.Configs().List(t.Context(), 0, 0); len(revisions) != 0 {
			t.Errorf("%d revisions stored, want none", len(revisions))
		}
	}

	t.Run("a first company", func(t *testing.T) {
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		err := boot(t, db, fleet, seedOf(file), refuses)
		if !errors.Is(err, unbuildable) || !strings.Contains(err.Error(), "company.yaml") {
			t.Fatalf("a bootstrap this node cannot build = %v, want the build's refusal "+
				"naming the file", err)
		}
		unwritten(t, db)
		if _, found, err := fleet.Target(t.Context()); err != nil || found {
			t.Errorf("the fleet was pointed at a company this node cannot build "+
				"(%v, %v)", found, err)
		}
	})

	t.Run("an override over the fleet's company", func(t *testing.T) {
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		live := activateAsPeer(t, fleet, "live-revision", []byte(`{"name":"Live"}`))
		if err := boot(t, db, fleet, overrideOf(file), refuses); !errors.Is(err, unbuildable) {
			t.Fatalf("an override this node cannot build = %v, want the build's refusal", err)
		}
		unwritten(t, db)
		assertFleetOn(t, fleet, live)
	})

	t.Run("an ignored file is not built", func(t *testing.T) {
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		live := activateAsPeer(t, fleet, "live-revision", []byte(`{"name":"Live"}`))
		asked := func(*config.Company) error {
			t.Error("a file the fleet's company outranks was built")
			return unbuildable
		}
		if err := boot(t, db, fleet, seedOf(file), asked); err != nil {
			t.Fatalf("a node restarting into a fleet refused over a file it ignores: %v", err)
		}
		unwritten(t, db)
		assertFleetOn(t, fleet, live)
	})

	t.Run("a seed with no build to hold it to", func(t *testing.T) {
		// NOT READ AS "NO CHECK": the one wiring that omitted the build would
		// import whatever it was handed.
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		if err := boot(t, db, fleet, seedOf(file), nil); err == nil {
			t.Fatal("a seed with no build imported its file")
		}
		unwritten(t, db)
	})
}

// A FAILED FIRST RECONCILE SAYS WHAT THE NODE IS SERVING INSTEAD, which on a
// node that booted unconfigured is nothing.
//
// The hint was written for a node that booted on a company, and every node
// that imports its file at this boot boots on none, as every node without
// `data` does at every boot — so it told the operator the node was "serving
// the configuration it booted with" while it served no company at all. Where
// the document itself is what has to change, a managed deployment changes it
// through its writers, never through an import its Tier A refuses.
//
// Mutation: answer the configured node's hint for every node and the
// unconfigured cases fail.
func TestAFailedFirstReconcileSaysWhatTheNodeServes(t *testing.T) {
	t.Parallel()
	open := &config.Bootstrap{}
	managed := &config.Bootstrap{API: config.API{Auth: config.APIAuth{
		CompanyWriters: []string{"gitops"}}}}
	for _, tc := range []struct {
		name       string
		boot       *config.Bootstrap
		configured bool
		want, not  []string
	}{
		{"a node serving a company", open, true,
			[]string{"the configuration it booted with"}, []string{"no company"}},
		{"a node that booted with none", open, false,
			[]string{"no company", "serves none", "crewlet config import"},
			[]string{"the configuration it booted with"}},
		{"a managed node that booted with none", managed, false,
			[]string{"no company", "managing system", "gitops"},
			[]string{"the configuration it booted with", "crewlet config import"}},
	} {
		hint := reconcileHint(tc.boot, tc.configured)
		for _, want := range tc.want {
			if !strings.Contains(hint, want) {
				t.Errorf("%s: the hint does not say %q: %s", tc.name, want, hint)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(hint, not) {
				t.Errorf("%s: the hint says %q: %s", tc.name, not, hint)
			}
		}
	}
}
