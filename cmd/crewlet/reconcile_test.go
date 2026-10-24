package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
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
//   -company        bootstraps an EMPTY store and is otherwise ignored, loudly
//   -import-company activates over whatever the store already holds
//
// Both are idempotent by CONTENT: an unchanged file writes nothing however
// many times a node boots.

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
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), fixtureCipher, testNode, quiet()); err != nil {
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
		if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedOf(company), fixtureCipher, testNode, quiet()); err != nil {
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
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), fixtureCipher, testNode, quiet()); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(companyYAML, "name: Acme", "name: Acme Renamed", 1)
	for range 3 {
		if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, overrideOf(parse(t, edited)), fixtureCipher, testNode, quiet()); err != nil {
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
		if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedOf(company), cipher, testNode, quiet()); err != nil {
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
	if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedOf(company), cipher, testNode, quiet()); err != nil {
		t.Fatal(err)
	}
	err = seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedOf(company), fixtureCipher, testNode, quiet())
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

// sealed is a company document as a peer of the fixture keyring's fleet
// publishes it: every activated payload is sealed, and a node opens the
// fleet's under its own keyring before comparing it with its file.
func sealed(t *testing.T, document []byte) []byte {
	t.Helper()
	payload, err := secrets.Seal(fixtureCipher, document)
	if err != nil {
		t.Fatalf("seal the fleet's document: %v", err)
	}
	return payload
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
	if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedOf(company), fixtureCipher, testNode, quiet()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	active, found, err := db.Configs().Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active: found=%v err=%v", found, err)
	}

	// A restart onto a FRESH coordination store, with the same file and
	// the same database.
	fleet := coordmemory.NewFleet()
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(company), fixtureCipher, testNode, quiet()); err != nil {
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
		err := seedCompany(t.Context(), db, fleet, nil, tierBSeed{Path: "company.yaml"},
			fixtureCipher, testNode, quiet())
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
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), fixtureCipher, testNode, quiet()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A PEER activated something else, later.
	peer, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "peer-revision", Summary: "from a peer",
		Payload: []byte(`{"name":"Peer"}`), At: time.Now().Add(2 * time.Hour), Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: "operator", Source: "api", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}})
	if err != nil {
		t.Fatalf("peer activate: %v", err)
	}
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), fixtureCipher, testNode, quiet()); err != nil {
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

// seedOf is a `-company` bootstrap seed: imported only into an empty store.
func seedOf(c *config.Company) tierBSeed {
	return tierBSeed{Path: "company.yaml", Company: c}
}

// overrideOf is the `-import-company` form: activated over whatever is there.
func overrideOf(c *config.Company) tierBSeed {
	return tierBSeed{Path: "company.yaml", Override: true, Company: c}
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
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), fixtureCipher, testNode, quiet()); err != nil {
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
		if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, edited)), fixtureCipher, testNode, quiet()); err != nil {
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

// AN EMPTY STORE IS NOT AN EMPTY COMPANY: a bootstrap seed into a node whose
// own store holds nothing does not overwrite the company its FLEET runs.
//
// Two ordinary nodes have an empty store: a node without `data`, whose scratch
// store is deleted at every boot, and a data node joining a running fleet. The
// seed asked only its own store, found nothing, and activated the file over
// the fleet's pointer — so a stateless node carrying the default
// ./company.yaml reverted every node's live edits at each of its restarts, and
// a node added with the quickstart's command reverted them once.
func TestABootstrapSeedDoesNotOverwriteTheFleetsCompany(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	running := parse(t, companyYAML)
	document, err := json.Marshal(running)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "peer-revision", Summary: "edited live", Payload: sealed(t, document),
		At: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: "operator",
			Source: "api", CreatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatalf("the fleet's activation: %v", err)
	}

	for _, tc := range []struct {
		name  string
		file  *config.Company
		warns bool
	}{
		{"a stale file", parse(t, strings.Replace(companyYAML, "name: Acme",
			"name: Acme Stale", 1)), true},
		// THE FLEET'S OWN DOCUMENT is nothing to say anything about.
		{"the fleet's own document", running, false},
	} {
		// IN TURN, on the one fleet: the override below must not move the
		// pointer under a case that is still reading it.
		t.Run(tc.name, func(t *testing.T) {
			db := seedStore(t)
			var logged bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logged, nil))
			if err := seedCompany(t.Context(), db, fleet, nil, seedOf(tc.file), fixtureCipher,
				testNode, log); err != nil {
				t.Fatalf("seed: %v", err)
			}
			target, _, err := fleet.Target(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if target.RevisionID != peer.RevisionID || target.Epoch != peer.Epoch {
				t.Fatalf("the pointer moved to %s (epoch %d), want the fleet's %s "+
					"(epoch %d): a node with an empty store overwrote the company "+
					"its fleet runs", target.RevisionID, target.Epoch,
					peer.RevisionID, peer.Epoch)
			}
			if _, found, _ := db.Configs().Active(t.Context()); found {
				t.Error("the ignored seed stored a revision: the reconciler adopts " +
					"the fleet's, and this node would offer its own at its next boot")
			}
			said := strings.Contains(logged.String(), "company_seed_ignored") &&
				strings.Contains(logged.String(), peer.RevisionID)
			if said != tc.warns {
				t.Errorf("company_seed_ignored naming the fleet's revision said = %v, "+
					"want %v: %s", said, tc.warns, logged.String())
			}
		})
	}

	// AN OVERRIDE still replaces it: that is what -import-company is for.
	db := seedStore(t)
	if err := seedCompany(t.Context(), db, fleet, nil, overrideOf(parse(t,
		strings.Replace(companyYAML, "name: Acme", "name: Acme Imported", 1))), fixtureCipher,
		testNode, quiet()); err != nil {
		t.Fatalf("override: %v", err)
	}
	if target, _, _ := fleet.Target(t.Context()); target.RevisionID == peer.RevisionID {
		t.Error("an -import-company override into an empty store did not replace " +
			"the fleet's company")
	}
}

// AND A PEER THAT ACTIVATES BETWEEN THE READ AND THE WRITE WINS: a bootstrap
// activates create-only, so the company a peer made exist in the meantime is
// not replaced, and the boot goes on to converge on it rather than failing.
//
// LOSING LEAVES NOTHING BEHIND, either. The seed stores its revision before
// it asks the fleet, and stored ACTIVE that revision was this node's company
// whatever the fleet answered: a restart before the reconciler adopted the
// peer's booted on it and — the seed's instant being the later one — published
// it over the company the fleet runs, the very overwrite the create-only
// activation had just refused.
func TestABootstrapSeedThatLosesTheRaceToAPeerStandsDown(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	peer, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "peer-revision", Summary: "a peer's bootstrap",
		Payload: sealed(t, []byte(`{"name":"Peer"}`)), At: time.Now().UTC(),
		Origin: coord.RevisionOrigin{Author: "node-b", AuthorKind: "node",
			Source: "file", CreatedAt: time.Now().UTC()},
	})
	if err != nil {
		t.Fatalf("the peer's activation: %v", err)
	}
	db := seedStore(t)
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	// The pointer read happened before the peer's write landed.
	if err := seedCompany(t.Context(), db, unreadPointer{fleet}, nil,
		seedOf(parse(t, companyYAML)), fixtureCipher, testNode, log); err != nil {
		t.Fatalf("a bootstrap that lost the race failed the boot: %v", err)
	}
	target, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if target.RevisionID != peer.RevisionID {
		t.Errorf("the pointer names %s, want the peer's %s: a bootstrap replaced a "+
			"company a peer activated after it looked", target.RevisionID, peer.RevisionID)
	}
	if !strings.Contains(logged.String(), "company_seed_ignored") {
		t.Errorf("the lost race was not said: %s", logged.String())
	}
	if active, found, err := db.Configs().Active(t.Context()); err != nil {
		t.Fatal(err)
	} else if found {
		t.Errorf("this node's active revision is %s after the seed lost the race, "+
			"want none until the reconciler adopts the fleet's: the node boots on "+
			"its active revision, and offers it to the fleet whenever it is newer "+
			"than the pointer", active.ID)
	}

	// THE RESTART, before any tick adopted the peer's revision.
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)),
		fixtureCipher, testNode, quiet()); err != nil {
		t.Fatalf("the restart: %v", err)
	}
	if after, _, err := fleet.Target(t.Context()); err != nil {
		t.Fatal(err)
	} else if after.RevisionID != peer.RevisionID {
		t.Errorf("the restart moved the pointer to %s, want the peer's %s: a seed "+
			"that lost the race was published one boot later", after.RevisionID,
			peer.RevisionID)
	}
}

// A SEED THE FLEET DID NOT TAKE IS NOT THIS NODE'S COMPANY, whichever flag
// carried it. An activation that fails fails the boot, and the revision stored
// before it is history: marked active, it was what the restart booted on and,
// found equal to the file, PUBLISHED rather than seeded again — over whatever
// a peer had activated in between, whenever the seed's instant was the later
// one, which for a bootstrap is the overwrite its create-only activation
// exists to refuse.
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
			err := seedCompany(t.Context(), db, unreachableActivation{fleet}, nil,
				tc.seed, fixtureCipher, testNode, quiet())
			if !errors.Is(err, errActivationUnreachable) {
				t.Fatalf("seed = %v, want the boot failed by the activation's own error", err)
			}
			if active, found, err := db.Configs().Active(t.Context()); err != nil {
				t.Fatal(err)
			} else if found {
				t.Fatalf("this node's active revision is %s, which no fleet took: "+
					"the next boot runs it, and offers it over the pointer", active.ID)
			}
			if tc.seed.Override {
				// The override is unconditional, so its restart replacing
				// a peer's company is what it is for.
				return
			}
			// A PEER BOOTSTRAPS while this node is down, at an instant
			// before the seed's own.
			peer, err := fleet.Activate(t.Context(), coord.ActivationRequest{
				RevisionID: "peer-revision", Summary: "a peer's bootstrap",
				Payload: sealed(t, []byte(`{"name":"Peer"}`)), At: time.Now().Add(-time.Minute),
				Origin: coord.RevisionOrigin{Author: "node-b", AuthorKind: "node",
					Source: "file", CreatedAt: time.Now().Add(-time.Minute)},
			})
			if err != nil {
				t.Fatalf("the peer's activation: %v", err)
			}
			if err := seedCompany(t.Context(), db, fleet, nil, tc.seed, fixtureCipher, testNode,
				quiet()); err != nil {
				t.Fatalf("the restart: %v", err)
			}
			if after, _, err := fleet.Target(t.Context()); err != nil {
				t.Fatal(err)
			} else if after.RevisionID != peer.RevisionID {
				t.Errorf("the restart moved the pointer to %s, want the peer's %s",
					after.RevisionID, peer.RevisionID)
			}
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
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), fixtureCipher, testNode, quiet()); err != nil {
		t.Fatal(err)
	}
	before, _, err := db.Configs().Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// tierBSeed with no Company: `crewlet run` with no company.yaml.
	if err := seedCompany(t.Context(), db, fleet, nil, tierBSeed{Path: "company.yaml"}, fixtureCipher, testNode, quiet()); err != nil {
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
	if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil,
		tierBSeed{Path: "company.yaml"}, fixtureCipher, testNode, quiet()); err != nil {
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
