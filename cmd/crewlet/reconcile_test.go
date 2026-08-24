package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// The seed is the one piece of policy in this binary: `-company` names a FILE,
// and a running node serves the STORE. What these pin is the rule joining
// them — the file is imported when, and only when, the store does not already
// hold it.

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
	if err := seedCompany(t.Context(), db, parse(t, companyYAML), nil, quiet()); err != nil {
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
	target, found, err := db.ControlPlane().Target(t.Context())
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
		if err := seedCompany(t.Context(), db, company, nil, quiet()); err != nil {
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

func TestAnEditedFileIsImportedOnce(t *testing.T) {
	t.Parallel()
	// The alternative — silently preferring the store — is the worst of
	// the three: an operator edits a config, restarts, and nothing happens,
	// with nothing anywhere saying why.
	db := seedStore(t)
	if err := seedCompany(t.Context(), db, parse(t, companyYAML), nil, quiet()); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(companyYAML, "name: Acme", "name: Acme Renamed", 1)
	for range 3 {
		if err := seedCompany(t.Context(), db, parse(t, edited), nil, quiet()); err != nil {
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
		if err := seedCompany(t.Context(), db, company, cipher, quiet()); err != nil {
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
	if err := seedCompany(t.Context(), db, company, cipher, quiet()); err != nil {
		t.Fatal(err)
	}
	err = seedCompany(t.Context(), db, company, nil, quiet())
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
