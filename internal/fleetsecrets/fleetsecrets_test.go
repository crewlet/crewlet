package fleetsecrets_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

var clock = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

// ring builds a keyring with the named keys, the first active.
func ring(t *testing.T, ids ...string) secrets.Cipher {
	t.Helper()
	k := secrets.Keyring{ActiveID: ids[0], Keys: map[string][]byte{}}
	for _, id := range ids {
		// FULLY DERIVED FROM THE ID, not random with one byte pinned: a
		// rekey test builds a SECOND keyring holding the same ids in a
		// different order, and it has to open what the first one wrote.
		sum := sha256.Sum256([]byte(id))
		k.Keys[id] = sum[:]
	}
	cipher, err := secrets.NewCipher(k)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return cipher
}

func fleetStore(t *testing.T, cipher secrets.Cipher) (*fleetsecrets.Store, coord.Fleet) {
	t.Helper()
	f := coordmem.NewFleet()
	return fleetsecrets.New(f, cipher), f
}

func mustSet(t *testing.T, s *fleetsecrets.Store, name, value string) {
	t.Helper()
	if err := s.Set(t.Context(), name, value, "sam", "cli", clock); err != nil {
		t.Fatalf("Set(%s): %v", name, err)
	}
}

// COORDINATION NEVER SEES PLAINTEXT. That is the whole reason a shared bucket
// is safe to put credentials in: a peer that can read it learns which names
// exist and when they changed, not what they are.
func TestTheBucketHoldsCiphertextAndNothingElse(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	s, fleet := fleetStore(t, cipher)
	mustSet(t, s, "GITLAB_TOKEN", "glpat-not-a-real-token")

	rec, found, err := fleet.Secret(t.Context(), "GITLAB_TOKEN")
	if err != nil || !found {
		t.Fatalf("Secret: %v found=%t", err, found)
	}
	if strings.Contains(rec.Value, "glpat") {
		t.Fatal("the stored value carries the plaintext, so every node that " +
			"can read the bucket can read the credential")
	}
	if rec.KeyID != "k1" {
		t.Errorf("key_id = %q, want it denormalised out of the envelope so a "+
			"rekey sweep can find stale rows without decrypting them", rec.KeyID)
	}
}

// THE NAME IS BOUND IN, so an envelope moved to another row fails to open
// rather than silently impersonating a different secret — which is what an
// attacker with write access to the bucket but no key would otherwise do.
func TestAnEnvelopeMovedToAnotherNameDoesNotOpen(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	s, fleet := fleetStore(t, cipher)
	mustSet(t, s, "REAL", "value")

	rec, _, err := fleet.Secret(t.Context(), "REAL")
	if err != nil {
		t.Fatal(err)
	}
	rec.Name = "IMPOSTOR"
	if err := fleet.PutSecret(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(t.Context(), "IMPOSTOR"); err == nil {
		t.Fatal("a relocated envelope opened, so a row can be made to stand " +
			"in for a different credential")
	}
}

// A SNAPSHOT FAILS CLOSED. A partial one is the worst outcome available: the
// names that are missing resolve to whatever the environment happens to hold,
// which is exactly the stale-.env shadowing the store exists to prevent, and
// it happens silently.
func TestASnapshotRefusesRatherThanSkippingAnUnopenableRow(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1"))
	mustSet(t, s, "GOOD", "value")
	// A row sealed by a keyring this store does not hold — a key dropped
	// from the config, which is the only way this happens.
	stranger := fleetsecrets.New(fleet, ring(t, "k9"))
	if err := stranger.Set(t.Context(), "FOREIGN", "other", "sam", "cli", clock); err != nil {
		t.Fatal(err)
	}

	values, err := s.All(t.Context())
	if err == nil {
		t.Fatalf("All returned %v with a row it could not open; every name it "+
			"omitted now resolves from the environment instead", values)
	}
	if !strings.Contains(err.Error(), "FOREIGN") {
		t.Errorf("the error does not name the row that failed: %v", err)
	}
}

// A LISTING NEEDS NO KEYRING AND CARRIES NO ENVELOPE. "Is X set, and when did
// it change" is asked far more often than "what is X", and answering it must
// not require the ability to decrypt — nor put ciphertext into a scrollback.
func TestAListingCarriesNoValueAndNeedsNoKey(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1"))
	mustSet(t, s, "B", "second")
	mustSet(t, s, "A", "first")

	rows, err := fleetsecrets.New(fleet, nil).List(t.Context())
	if err != nil {
		t.Fatalf("a node with no keyring could not list what exists: %v", err)
	}
	if len(rows) != 2 || rows[0].Name != "A" || rows[1].Name != "B" {
		t.Fatalf("rows = %+v, want them name-ordered", rows)
	}
	for _, row := range rows {
		if row.Value != "" {
			t.Errorf("%s carried its envelope into a listing", row.Name)
		}
	}
}

// A MISSING NAME IS ITS OWN SENTINEL, distinct from a keyring that no longer
// opens what it wrote. Collapsing them would make a dropped key look exactly
// like a variable nobody set.
func TestAMissingNameIsDistinctFromAnUnopenableOne(t *testing.T) {
	t.Parallel()
	s, _ := fleetStore(t, ring(t, "k1"))
	if _, err := s.Get(t.Context(), "NOTHING"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("err = %v, want secrets.ErrNotFound", err)
	}
}

// NO KEYRING IS A REFUSAL, not a store that quietly holds plaintext.
func TestWithoutAKeyringEveryWriteAndReadIsRefused(t *testing.T) {
	t.Parallel()
	s, _ := fleetStore(t, nil)
	if err := s.Set(t.Context(), "A", "v", "sam", "cli", clock); !errors.Is(err, secrets.ErrNoKeyring) {
		t.Errorf("Set err = %v, want secrets.ErrNoKeyring", err)
	}
	if _, err := s.Get(t.Context(), "A"); !errors.Is(err, secrets.ErrNoKeyring) {
		t.Errorf("Get err = %v, want secrets.ErrNoKeyring", err)
	}
	if _, err := s.All(t.Context()); !errors.Is(err, secrets.ErrNoKeyring) {
		t.Errorf("All err = %v, want secrets.ErrNoKeyring", err)
	}
}

// A REKEY MOVES ONLY STALE ROWS AND REPORTS THE NAMES, which is what an
// operator confirms before retiring the old key. A count cannot answer "which
// one did not move".
func TestARekeyMovesTheStaleRowsAndNamesThem(t *testing.T) {
	t.Parallel()
	old := ring(t, "k1", "k2")
	s, fleet := fleetStore(t, old)
	mustSet(t, s, "A", "one")
	mustSet(t, s, "B", "two")

	// The same key material, with k2 active.
	rotated := fleetsecrets.New(fleet, ring(t, "k2", "k1"))
	moved, err := rotated.Rekey(t.Context(), "k2", "sam", clock)
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if strings.Join(moved, ",") != "A,B" {
		t.Fatalf("moved = %v, want both names", moved)
	}
	// A SECOND RUN IS A NO-OP, which is what makes this safe in a deploy
	// script.
	again, err := rotated.Rekey(t.Context(), "k2", "sam", clock)
	if err != nil || len(again) != 0 {
		t.Fatalf("a second rekey moved %v (err %v), want nothing", again, err)
	}
	values, err := rotated.All(t.Context())
	if err != nil || values["A"] != "one" || values["B"] != "two" {
		t.Fatalf("after the rekey the values are %v (err %v)", values, err)
	}
}

// A REKEY ABORTS ON A ROW IT CANNOT OPEN, rather than reporting success over
// a secret that is now unreadable for ever — which is the state the operator
// is about to retire the old key on the strength of.
func TestARekeyRefusesToLeaveARowBehind(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1"))
	mustSet(t, s, "MINE", "value")
	stranger := fleetsecrets.New(fleet, ring(t, "k9"))
	if err := stranger.Set(t.Context(), "THEIRS", "other", "sam", "cli", clock); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Rekey(t.Context(), "k1", "sam", clock); err == nil {
		t.Fatal("a rekey reported success while leaving a row under a key " +
			"this node cannot open")
	}
}

// ---- the migration --------------------------------------------------- //

func localStore(t *testing.T, cipher secrets.Cipher) *store.SecretValues {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db.SecretValues(cipher)
}

// THE MIGRATION COPIES AND THEN REMOVES, and the removal is the half that is
// easy to skip and cannot be: a local row left behind is read on every
// subsequent boot, so a later unset on the fleet would be silently undone by
// the stale copy resurfacing, forever.
func TestMigrationMovesTheRowsAndEmptiesTheLocalTable(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	local := localStore(t, cipher)
	fleet, _ := fleetStore(t, cipher)
	if err := local.Set(t.Context(), "GL", "glpat-x", "sam", "cli", clock); err != nil {
		t.Fatal(err)
	}

	moved, err := fleetsecrets.Migrate(t.Context(), local, fleet, clock)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if strings.Join(moved, ",") != "GL" {
		t.Fatalf("moved = %v, want the one local row", moved)
	}
	values, err := fleet.All(t.Context())
	if err != nil || values["GL"] != "glpat-x" {
		t.Fatalf("the fleet holds %v (err %v)", values, err)
	}
	rows, err := local.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("the local table still holds %+v, so a later unset on the "+
			"fleet would be undone at the next boot", rows)
	}
}

// A NAME ALREADY ON THE FLEET IS NOT OVERWRITTEN. The local row is by
// definition the older write — the fleet is where every rotation since has
// landed — so copying it would resurrect a value an operator rotated away
// from on another node.
func TestMigrationNeverOverwritesTheFleetsValue(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	local := localStore(t, cipher)
	fleet, _ := fleetStore(t, cipher)
	if err := local.Set(t.Context(), "GL", "the-old-token", "sam", "cli", clock); err != nil {
		t.Fatal(err)
	}
	mustSet(t, fleet, "GL", "the-rotated-token")

	moved, err := fleetsecrets.Migrate(t.Context(), local, fleet, clock)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(moved) != 0 {
		t.Errorf("moved = %v, want nothing: the fleet already had it", moved)
	}
	values, _ := fleet.All(t.Context())
	if values["GL"] != "the-rotated-token" {
		t.Fatalf("GL = %q, want the fleet's newer value untouched", values["GL"])
	}
	// AND THE STALE LOCAL COPY IS STILL REMOVED, or it would shadow the
	// fleet's row at every boot from now on.
	rows, _ := local.List(t.Context())
	if len(rows) != 0 {
		t.Fatalf("the shadowing local row survived: %+v", rows)
	}
}

// A ROW THAT COULD NOT BE COPIED IS NOT REMOVED. Deleting it would destroy a
// credential this node is the only holder of, and the first symptom would be
// a vendor 401 hours later on a node that never had the value.
func TestMigrationKeepsWhatItCouldNotCopy(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	local := localStore(t, cipher)
	if err := local.Set(t.Context(), "GL", "glpat-x", "sam", "cli", clock); err != nil {
		t.Fatal(err)
	}
	// A fleet store with no keyring refuses every write.
	fleet := fleetsecrets.New(coordmem.NewFleet(), nil)

	if _, err := fleetsecrets.Migrate(t.Context(), local, fleet, clock); err == nil {
		t.Fatal("a migration that could write nothing reported success")
	}
	rows, err := local.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the local rows are %+v, want the uncopied one kept", rows)
	}
}

// THE ORIGINAL AUTHOR SURVIVES. "Who set this" is the question the provenance
// columns exist to answer, and answering it with the migration would erase
// the only record of it.
func TestMigrationPreservesWhoWroteTheRow(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	local := localStore(t, cipher)
	fleet, _ := fleetStore(t, cipher)
	if err := local.Set(t.Context(), "GL", "v", "dana", "gitlab-provision", clock); err != nil {
		t.Fatal(err)
	}

	if _, err := fleetsecrets.Migrate(t.Context(), local, fleet, clock); err != nil {
		t.Fatal(err)
	}
	rows, err := fleet.List(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v (err %v)", rows, err)
	}
	if rows[0].UpdatedBy != "dana" {
		t.Errorf("updated_by = %q, want the original author", rows[0].UpdatedBy)
	}
	if rows[0].Source != fleetsecrets.MigrateSource {
		t.Errorf("source = %q, want %q so a reader can tell where the row "+
			"came from", rows[0].Source, fleetsecrets.MigrateSource)
	}
}

// A NODE WITH NOTHING LOCAL COSTS ONE READ AND WRITES NOTHING, which is the
// steady state on every boot after the first.
func TestMigrationOnAnEmptyTableDoesNothing(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	fleet, _ := fleetStore(t, cipher)
	moved, err := fleetsecrets.Migrate(t.Context(), localStore(t, cipher), fleet, clock)
	if err != nil || moved != nil {
		t.Fatalf("moved = %v, err = %v; want a silent no-op", moved, err)
	}
}

// A NODE WITH NO KEYRING HAS NOTHING TO MIGRATE and must not fail the boot
// over it: secrets then come from the environment and the store is not in use.
func TestMigrationWithoutAKeyringIsASilentNoOp(t *testing.T) {
	t.Parallel()
	fleet, _ := fleetStore(t, ring(t, "k1"))
	_, err := fleetsecrets.Migrate(context.Background(), localStore(t, nil), fleet, clock)
	if err != nil {
		t.Fatalf("a node with no keyring failed its migration: %v", err)
	}
}
