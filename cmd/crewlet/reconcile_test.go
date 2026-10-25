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
//   -import-company activates over whatever the store already holds
//
// Both are idempotent by CONTENT: an unchanged file writes nothing however
// many times a node boots. Every case boots through [bootSeed], the decision
// and the import together, as a node does.

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
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	active, found, err := db.Configs().Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active: found=%v err=%v", found, err)
	}
	if active.Source != "file" {
		t.Errorf("source = %q, want the file it came from", active.Source)
	}
	// And the POINTER moved with it, or nothing in the fleet reconciles.
	target, found, err := fleet.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("target: found=%v err=%v", found, err)
	}
	if target.RevisionID != active.ID {
		t.Errorf("the pointer names %s, want the seeded revision %s",
			target.RevisionID, active.ID)
	}
	// AT ONE INSTANT. The local row's is what this node's config history
	// shows and what the reconciler holds against the pointer's: two
	// readings of the clock would date the activation twice, and the
	// reconciler's first tick would re-activate the revision locally to
	// realign what the seed wrote a moment earlier.
	if store.EncodeTime(active.ActivatedAt) != store.EncodeTime(target.At) {
		t.Errorf("the seeded revision was activated at %s here and %s on the "+
			"pointer, want one instant", active.ActivatedAt, target.At)
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
	active, _, err := db.Configs().Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var company map[string]any
	if err := json.Unmarshal(opened(t, active.Payload), &company); err != nil {
		t.Fatal(err)
	}
	if company["name"] != "Acme Renamed" {
		t.Errorf("active company = %v, want the edited one", company["name"])
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
// dropped before every revision was re-sealed off it — cannot read the active
// revision. Seeding over it would replace a company nobody here can decrypt
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
		t.Fatalf("a seed holding another keyring = %v, want the active revision "+
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

// A node whose COORDINATION store is empty publishes what it already has.
//
// This is the case the embedded topology creates on every restart of a
// single-node deployment whose broker keeps no persistent store: the local
// database still holds the active revision, the fleet's pointer is gone, and
// without this the node would serve a company that nothing points at — so
// every peer, and its own reconciler, would read it as unconfigured.
//
// It is also what carries an OFFLINE `crewlet config import` through: that
// command marks a revision active locally and says it cannot move the
// pointer, because on this topology the pointer lives inside the engine's own
// process.
func TestANodeWithNoPointerPublishesItsActiveRevision(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	company := parse(t, companyYAML)

	// A first start, which seeds the revision and publishes the pointer.
	if err := bootSeed(t, db, coordmemory.NewFleet(), seedOf(company), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	active, found, err := db.Configs().Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active: found=%v err=%v", found, err)
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
	if target.RevisionID != active.ID {
		t.Fatalf("the pointer names %s, want the revision this node holds (%s)",
			target.RevisionID, active.ID)
	}
}

// A BOOT PUBLISHES NO REVISION IT HOLDS IN THE CLEAR.
//
// What a node publishes with no pointer to defer to, every peer applies — and
// every peer refuses a body that is not sealed, this node included. It is
// refused here instead, naming the command this node's operator runs.
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
	for _, sealed := range []bool{false, true} {
		db := seedStore(t)
		payload := json.RawMessage(document)
		if sealed {
			if payload, err = secrets.Seal(fixtureCipher, document); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Configs().InsertActive(t.Context(), store.Revision{
			Source: "file", CreatedBy: "not-a-node", CreatedByKind: iam.ActorSystem,
			Summary: "stored before", Payload: payload, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		fleet := coordmemory.NewFleet()
		err := bootSeed(t, db, fleet, tierBSeed{Path: "company.yaml"}, fixtureCipher)
		_, published, targetErr := fleet.Target(t.Context())
		if targetErr != nil {
			t.Fatal(targetErr)
		}
		if sealed {
			// THE CONTROL.
			if err != nil || !published {
				t.Fatalf("a sealed active revision was not published: published=%v err=%v",
					published, err)
			}
			continue
		}
		if !errors.Is(err, secrets.ErrUnsealedWithKey) {
			t.Fatalf("a boot over an unsealed revision = %v, want it refused unsealed", err)
		}
		if !strings.Contains(err.Error(), "crewlet config import") {
			t.Errorf("the refusal does not name the command that brings it back sealed: %v", err)
		}
		if published {
			t.Error("the boot published a revision it holds in the clear")
		}
	}
}

// A node booting into a running fleet must CONVERGE on what the fleet already
// decided, not overwrite it with whatever its local database happens to say.
// Without this rule one restarted node could roll a company back.
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
// ([decideSeed]), then carried out once the broker is up ([seedCompany]).
// Every case here goes through both, because the decision is what a boot
// imports on, and a case that called the import alone would certify a rule
// no node runs.
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
	return seedCompany(t.Context(), db, plane, nil, plan, cipher, testNode, builds, log)
}

// builds is a node on which every file builds: the cases that take it are
// about what the seed writes, and [TestAFileThisNodeCannotBuildIsNeverImported]
// is about the build.
func builds(*config.Company) error { return nil }

// storeOf is a [storedReader] over a store a case holds open: what
// [storeAt] reads off disk for `crewlet run`.
func storeOf(db *store.DB, cipher secrets.Cipher) storedReader {
	return func(ctx context.Context) (storedActive, bool, error) {
		return readStoredActive(ctx, db.Configs(), cipher)
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
// The store has to be left EXACTLY as it was: not just "still active", but no
// new revision, no moved pointer, and the same document underneath. A seed
// that chained a revision and then activated the old one back would pass a
// weaker check and still have rewritten the fleet's history.
func TestABootstrapSeedDoesNotOverwriteAnExistingCompany(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatal(err)
	}
	before, _, err := db.Configs().Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
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
	after, _, err := db.Configs().Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID {
		t.Errorf("the active revision moved from %s to %s", before.ID, after.ID)
	}
	var company map[string]any
	if err := json.Unmarshal(opened(t, after.Payload), &company); err != nil {
		t.Fatal(err)
	}
	if company["name"] != "Acme" {
		t.Errorf("active company = %v, want the one already in the store", company["name"])
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
	before, _, err := db.Configs().Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
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
	after, _, err := db.Configs().Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID {
		t.Errorf("the active revision changed with no file to change it")
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
// replaced either — and the revision the seed stored stays as inactive history
// rather than this node's active one, which its next boot would publish over
// the fleet's whenever it read as the newer. A file that IS the fleet's
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
		if revisions, _ := db.Configs().List(t.Context(), 0, 0); len(revisions) != 0 {
			t.Errorf("%d revisions stored, want none: the file was never the company", len(revisions))
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
		unread := func(context.Context) (storedActive, bool, error) {
			t.Error("a scratch store was read to decide the boot")
			return storedActive{}, false, nil
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
		if _, found, err := db.Configs().Active(t.Context()); err != nil || found {
			t.Errorf("a seed the fleet refused is this node's active revision (%v, %v): "+
				"its next boot would publish it over the fleet's", found, err)
		}
		if revisions, _ := db.Configs().List(t.Context(), 0, 0); len(revisions) != 1 {
			t.Errorf("%d revisions, want the refused seed kept as history", len(revisions))
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
		mine, _, err := db.Configs().Active(t.Context())
		if err != nil {
			t.Fatal(err)
		}
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
		active, _, err := db.Configs().Active(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if active.ID != after.RevisionID || active.ParentID != mine.ID {
			t.Errorf("this node holds %s (parent %q), want the import %s chained to %s",
				active.ID, active.ParentID, after.RevisionID, mine.ID)
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

	t.Run("the fleet's own revision", func(t *testing.T) {
		t.Parallel()
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		live := activateAsPeer(t, fleet, "live-revision", document)
		if err := bootSeed(t, db, fleet, overrideOf(file), fixtureCipher); err != nil {
			t.Fatalf("override: %v", err)
		}
		assertFleetOn(t, fleet, live)
		if revisions, _ := db.Configs().List(t.Context(), 0, 0); len(revisions) != 0 {
			t.Errorf("%d revisions stored for a file the fleet already runs, want none",
				len(revisions))
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
