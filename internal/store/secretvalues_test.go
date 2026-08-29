package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

var secretClock = time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)

// ring builds a keyring with the named keys, the first active.
func ring(t *testing.T, ids ...string) secrets.Keyring {
	t.Helper()
	k := secrets.Keyring{ActiveID: ids[0], Keys: map[string][]byte{}}
	for i, id := range ids {
		key, err := secrets.GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		// Deterministic per position, so a rekey test can rebuild the same
		// keyring with a different active id and still open what it wrote.
		key[0] = byte(i)
		k.Keys[id] = key
	}
	return k
}

func secretStore(t *testing.T, k secrets.Keyring) (*store.SecretValues, *store.DB, secrets.Cipher) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if len(k.Keys) == 0 {
		return db.SecretValues(nil), db, nil
	}
	cipher, err := secrets.NewCipher(k)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return db.SecretValues(cipher), db, cipher
}

func mustSet(t *testing.T, s *store.SecretValues, name, value string) {
	t.Helper()
	if err := s.Set(context.Background(), name, value, "operator", "cli", secretClock); err != nil {
		t.Fatalf("Set(%s): %v", name, err)
	}
}

func TestASecretRoundTrips(t *testing.T) {
	t.Parallel()
	s, _, _ := secretStore(t, ring(t, "k1"))
	mustSet(t, s, "ANTHROPIC_API_KEY", "sk-ant-not-a-real-key")

	got, err := s.Get(context.Background(), "ANTHROPIC_API_KEY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-ant-not-a-real-key" {
		t.Fatalf("value = %q", got)
	}
}

// NOTHING IS STORED IN THE CLEAR. The table has no plaintext mode, and this
// is what says so: the row a `SELECT` returns must not be the secret.
func TestTheStoredRowIsCiphertext(t *testing.T) {
	t.Parallel()
	s, db, _ := secretStore(t, ring(t, "k1"))
	mustSet(t, s, "TOKEN", "the-actual-secret")

	var sealed, keyID string
	err := db.SQL().QueryRowContext(t.Context(),
		`SELECT value, key_id FROM secret_values WHERE name = 'TOKEN'`).Scan(&sealed, &keyID)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if sealed == "the-actual-secret" {
		t.Fatal("the secret is stored in the clear")
	}
	if !secrets.IsEnvelope(sealed) {
		t.Fatalf("stored value %q is not a sealed envelope", sealed)
	}
	// THE KEY ID IS DENORMALISED so a rotation sweep can find stale rows
	// without decrypting any of them.
	if keyID != "k1" {
		t.Fatalf("key_id = %q, want the sealing key", keyID)
	}
}

// THE NAME IS BOUND IN as associated data, so a ciphertext moved to another
// row fails to decrypt rather than silently impersonating a different
// secret — which is what someone with UPDATE but not the key could do.
func TestACiphertextMovedToAnotherRowWillNotOpen(t *testing.T) {
	t.Parallel()
	s, db, _ := secretStore(t, ring(t, "k1"))
	mustSet(t, s, "READONLY_TOKEN", "harmless")
	mustSet(t, s, "ADMIN_TOKEN", "privileged")

	if _, err := db.SQL().ExecContext(t.Context(),
		`UPDATE secret_values SET value = (
			SELECT value FROM secret_values WHERE name = 'ADMIN_TOKEN')
		 WHERE name = 'READONLY_TOKEN'`); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got, err := s.Get(context.Background(), "READONLY_TOKEN"); err == nil {
		t.Fatalf("the moved ciphertext opened as %q", got)
	}
}

// A MISSING SECRET AND AN UNREADABLE ONE ARE DIFFERENT FACTS: one is an
// operator's unset variable, the other is a keyring that no longer opens
// what it wrote. Collapsing them makes a dropped key look like a typo.
func TestAMissingSecretIsItsOwnError(t *testing.T) {
	t.Parallel()
	s, _, _ := secretStore(t, ring(t, "k1"))
	_, err := s.Get(context.Background(), "NEVER_SET")
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("err = %v, want secrets.ErrNotFound", err)
	}
}

// READS FAIL CLOSED. Everything else in this package answers safely and
// carries on; an unreadable secret must raise, because "" becomes an empty
// Bearer token hours later on a request whose 401 names the vendor.
func TestAWrongKeyringRaisesRatherThanReturningNothing(t *testing.T) {
	t.Parallel()
	written := ring(t, "k1")
	s, db, _ := secretStore(t, written)
	mustSet(t, s, "TOKEN", "the-actual-secret")

	// Another keyring entirely — an operator who lost the key material.
	other, err := secrets.NewCipher(ring(t, "k1"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	stranger := db.SecretValues(other)
	got, err := stranger.Get(context.Background(), "TOKEN")
	if err == nil {
		t.Fatalf("an unreadable secret came back as %q", got)
	}
	if errors.Is(err, secrets.ErrNotFound) {
		t.Fatal("an unreadable secret was reported as a missing one")
	}
	if _, err := stranger.All(context.Background()); err == nil {
		t.Fatal("the snapshot silently skipped a row it could not open")
	}
}

// NO KEYRING MEANS NO STORE, not a plaintext one. A dedicated secret store
// that can hold unencrypted secrets is a footgun with no upside.
func TestWithoutAKeyringTheStoreRefusesEverything(t *testing.T) {
	t.Parallel()
	s, _, _ := secretStore(t, secrets.Keyring{})
	if err := s.Set(context.Background(), "TOKEN", "v", "op", "cli", secretClock); !errors.Is(err, secrets.ErrNoKeyring) {
		t.Errorf("Set err = %v, want secrets.ErrNoKeyring", err)
	}
	if _, err := s.Get(context.Background(), "TOKEN"); !errors.Is(err, secrets.ErrNoKeyring) {
		t.Errorf("Get err = %v, want secrets.ErrNoKeyring", err)
	}
	if _, err := s.All(context.Background()); !errors.Is(err, secrets.ErrNoKeyring) {
		t.Errorf("All err = %v, want secrets.ErrNoKeyring", err)
	}
}

// ROTATION IS AN UPDATE OF ONE ROW, which is the whole reason the store is
// separate from the config: writing a literal into the config would archive
// the old secret for ever, since every revision is an immutable copy.
func TestRotationReplacesTheValueInPlace(t *testing.T) {
	t.Parallel()
	s, _, _ := secretStore(t, ring(t, "k1"))
	mustSet(t, s, "TOKEN", "old")
	mustSet(t, s, "TOKEN", "new")

	got, err := s.Get(context.Background(), "TOKEN")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "new" {
		t.Fatalf("value = %q, want the rotated one", got)
	}
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("rotation left %d rows, want 1", len(list))
	}
}

// A LISTING CARRIES NO VALUES. It is what an operator reads to answer "is X
// set", and one that carried plaintext would put every credential a company
// has into one response body.
func TestAListingIsMetadataOnly(t *testing.T) {
	t.Parallel()
	s, _, _ := secretStore(t, ring(t, "k1"))
	mustSet(t, s, "ZETA", "z")
	mustSet(t, s, "ALPHA", "a")

	list, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d secrets, want 2", len(list))
	}
	// NAME-ORDERED, because this is scanned rather than searched, and an
	// order that moves on every rotation is one you have to search.
	if list[0].Name != "ALPHA" || list[1].Name != "ZETA" {
		t.Fatalf("order = %s, %s", list[0].Name, list[1].Name)
	}
	if list[0].UpdatedBy != "operator" || list[0].Source != "cli" {
		t.Errorf("provenance = %+v", list[0])
	}
	if !list[0].UpdatedAt.Equal(secretClock) {
		t.Errorf("updated_at = %s, want %s", list[0].UpdatedAt, secretClock)
	}
}

func TestUnsetReportsWhetherARowWent(t *testing.T) {
	t.Parallel()
	s, _, _ := secretStore(t, ring(t, "k1"))
	mustSet(t, s, "TOKEN", "v")

	gone, err := s.Unset(context.Background(), "TOKEN")
	if err != nil || !gone {
		t.Fatalf("Unset = %v, %v", gone, err)
	}
	if gone, err = s.Unset(context.Background(), "TOKEN"); err != nil || gone {
		t.Fatalf("the second Unset = %v, %v, want false with no error", gone, err)
	}
}

// THE SNAPSHOT IS WHAT THE RESOLVER READS, so it must carry every name — a
// partial one lets the missing names resolve from the environment, which is
// exactly the stale-.env shadowing the store exists to prevent.
func TestTheSnapshotCarriesEverySecret(t *testing.T) {
	t.Parallel()
	s, _, _ := secretStore(t, ring(t, "k1"))
	for name, value := range map[string]string{
		"A_TOKEN": "1", "B_TOKEN": "2", "C_TOKEN": "3",
	} {
		mustSet(t, s, name, value)
	}
	got, err := s.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 || got["A_TOKEN"] != "1" || got["C_TOKEN"] != "3" {
		t.Fatalf("snapshot = %v", got)
	}
}

// A REKEY MOVES ONLY THE STALE ROWS, and reports which — a pass that moved
// 12 of 13 raises a question ("which one?") a count cannot answer.
func TestRekeyMovesOnlyWhatIsNotAlreadyActive(t *testing.T) {
	t.Parallel()
	k := ring(t, "k1", "k2")
	s, db, _ := secretStore(t, k)
	mustSet(t, s, "OLD_A", "a")
	mustSet(t, s, "OLD_B", "b")

	// Rotate: k2 becomes active, k1 still decrypts what it sealed.
	rotated := secrets.Keyring{ActiveID: "k2", Keys: k.Keys}
	cipher, err := secrets.NewCipher(rotated)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	after := db.SecretValues(cipher)
	// A value written after the rotation is already on the active key and
	// must not be touched by the pass.
	if err = after.Set(context.Background(), "NEW_C", "c", "op", "cli", secretClock); err != nil {
		t.Fatalf("Set: %v", err)
	}

	moved, err := after.Rekey(context.Background(), "k2", "operator", secretClock)
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if len(moved) != 2 || moved[0] != "OLD_A" || moved[1] != "OLD_B" {
		t.Fatalf("moved = %v, want the two stale rows in name order", moved)
	}

	// EVERY VALUE SURVIVES the re-seal — a rekey that lost one would be
	// discovered as a 401 weeks later.
	for name, want := range map[string]string{"OLD_A": "a", "OLD_B": "b", "NEW_C": "c"} {
		got, err := after.Get(context.Background(), name)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// AND THE POINT OF THE PASS: nothing is left under the retired key, so
	// it can be dropped from the config.
	list, err := after.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range list {
		if r.KeyID != "k2" {
			t.Errorf("%s is still on %s after a rekey", r.Name, r.KeyID)
		}
	}
}

// A SECOND REKEY IS A NO-OP, which is what makes the command safe to re-run
// after a partial failure.
func TestRekeyingTwiceMovesNothingTheSecondTime(t *testing.T) {
	t.Parallel()
	s, _, _ := secretStore(t, ring(t, "k1"))
	mustSet(t, s, "TOKEN", "v")
	if moved, err := s.Rekey(context.Background(), "k1", "op", secretClock); err != nil || len(moved) != 0 {
		t.Fatalf("Rekey on an already-active row = %v, %v", moved, err)
	}
}

// A ROW THIS KEYRING CANNOT OPEN ABORTS THE WHOLE PASS. Moving the others
// while leaving it would report a successful rekey over a secret that is now
// unreadable for ever — and the operator would then retire the key.
func TestARekeyAbortsOnARowItCannotOpen(t *testing.T) {
	t.Parallel()
	s, db, _ := secretStore(t, ring(t, "k1"))
	mustSet(t, s, "GOOD", "v")

	// A row sealed by a key this ring never had.
	stranger, err := secrets.NewCipher(ring(t, "lost"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	sealed, err := stranger.Encrypt("orphan", secrets.AADForVar("ORPHAN"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := db.SQL().ExecContext(t.Context(),
		`INSERT INTO secret_values (name, value, key_id, updated_at, updated_by, source)
		 VALUES ('ORPHAN', ?, 'lost', 0, 'op', 'test')`, sealed); err != nil {
		t.Fatalf("insert orphan: %v", err)
	}

	if moved, err := s.Rekey(context.Background(), "k1", "op", secretClock); err == nil {
		t.Fatalf("the pass reported success having moved %v", moved)
	}
}
