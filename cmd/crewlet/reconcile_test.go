package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
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
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "seed.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
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

func TestAFirstRunSeedsTheStore(t *testing.T) {
	t.Parallel()
	// Without this a first run has nothing to activate: the node serves a
	// company no peer can see, and a second node started against the same
	// store finds it unconfigured.
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), nil, quiet()); err != nil {
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
}

func TestAnUnchangedFileSeedsNothing(t *testing.T) {
	t.Parallel()
	// A node boots many times over its life. Importing on each one would
	// mint a revision per restart and move the pointer, so every peer in
	// the fleet would rebuild its epoch every time any node restarted.
	db := seedStore(t)
	company := parse(t, companyYAML)
	for range 5 {
		if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedOf(company), nil, quiet()); err != nil {
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
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), nil, quiet()); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(companyYAML, "name: Acme", "name: Acme Renamed", 1)
	for range 3 {
		if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, overrideOf(parse(t, edited)), nil, quiet()); err != nil {
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
	if err := json.Unmarshal(active.Payload, &company); err != nil {
		t.Fatal(err)
	}
	if company["name"] != "Acme Renamed" {
		t.Errorf("active company = %v, want the edited one", company["name"])
	}
}

func TestASealedStoreDoesNotReseedOnEveryBoot(t *testing.T) {
	t.Parallel()
	// THE trap. With a keyring configured the stored payload is ciphertext
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
		if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedOf(company), cipher, quiet()); err != nil {
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
		t.Error("a keyring was configured and the revision was stored in plaintext")
	}
	if bytes.Contains(revisions[0].Payload, []byte("Acme")) {
		t.Errorf("the company name survived into the stored form: %s", revisions[0].Payload)
	}
}

func TestASealedStoreWithNoKeyringRefusesRatherThanReseeding(t *testing.T) {
	t.Parallel()
	// A node that lost its keyring cannot read the active revision. Seeding
	// over it would replace a company nobody can decrypt with one this node
	// happens to have on disk — silently, on a restart.
	db := seedStore(t)
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": generatedKey(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	company := parse(t, companyYAML)
	if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedOf(company), cipher, quiet()); err != nil {
		t.Fatal(err)
	}
	err = seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedOf(company), nil, quiet())
	if err == nil {
		t.Fatal("a node with no keyring seeded over a sealed revision")
	}
	if !strings.Contains(err.Error(), "sealed") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
	revisions, err := db.Configs().List(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 {
		t.Fatalf("%d revisions, want the sealed one left alone", len(revisions))
	}
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
	if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedOf(company), nil, quiet()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	active, found, err := db.Configs().Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active: found=%v err=%v", found, err)
	}

	// A restart onto a FRESH coordination store, with the same file and
	// the same database.
	fleet := coordmemory.NewFleet()
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(company), nil, quiet()); err != nil {
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

// A node booting into a running fleet must CONVERGE on what the fleet already
// decided, not overwrite it with whatever its local database happens to say.
// Without this rule one restarted node could roll a company back.
func TestAStaleLocalRevisionDoesNotOverwriteTheFleet(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), nil, quiet()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A PEER activated something else, later.
	peer, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "peer-revision", Summary: "from a peer",
		Payload: []byte(`{"name":"Peer"}`), At: time.Now().Add(2 * time.Hour)})
	if err != nil {
		t.Fatalf("peer activate: %v", err)
	}
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), nil, quiet()); err != nil {
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
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), nil, quiet()); err != nil {
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
		if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, edited)), nil, quiet()); err != nil {
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
	if err := json.Unmarshal(after.Payload, &company); err != nil {
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
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)), nil, quiet()); err != nil {
		t.Fatal(err)
	}
	before, _, err := db.Configs().Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// tierBSeed with no Company: `crewlet run` with no company.yaml.
	if err := seedCompany(t.Context(), db, fleet, nil, tierBSeed{Path: "company.yaml"}, nil, quiet()); err != nil {
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
		tierBSeed{Path: "company.yaml"}, nil, quiet()); err != nil {
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
