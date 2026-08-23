package config_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
)

// The keyring is the deployment's SOLE ROOT OF TRUST: the store holds only
// ciphertext, and the key material lives in Tier A, never in the database it
// opens. What these pin is the boundary between "encryption is off" — a real,
// documented posture — and "encryption is on and broken", which must never
// boot.

func keyMaterial(t *testing.T) string {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestNoKeyringIsAPostureNotAFailure(t *testing.T) {
	t.Parallel()
	// A deployment with no keyring stores its company config in plaintext.
	// That is the opt-out and the state every deployment starts in, so
	// refusing here would make the first run of one impossible.
	var none config.Secrets
	cipher, err := none.Cipher()
	if err != nil {
		t.Fatalf("an unconfigured keyring errored: %v", err)
	}
	if cipher != nil {
		t.Error("no keyring produced a cipher, so plaintext storage is unreachable")
	}
}

func TestAConfiguredKeyringSeals(t *testing.T) {
	t.Parallel()
	ring := config.Secrets{
		ActiveKeyID: "k1",
		Keys:        []config.SecretKey{{ID: "k1", Material: keyMaterial(t)}},
	}
	cipher, err := ring.Cipher()
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	if cipher == nil {
		t.Fatal("a configured keyring produced no cipher, so the store would hold plaintext")
	}
	sealed, err := cipher.Encrypt("a secret", secrets.AADForDocument)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !strings.HasPrefix(sealed, "enc:v1:k1:") {
		t.Errorf("envelope = %q, want it stamped with the active key id", sealed)
	}
	opened, err := cipher.Decrypt(sealed, secrets.AADForDocument)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if opened != "a secret" {
		t.Errorf("opened %q", opened)
	}
}

func TestARotationKeyringSealsWithTheActiveKeyAndOpensWithEither(t *testing.T) {
	t.Parallel()
	// More than one entry is what makes a rotation zero-downtime: the
	// active key seals, every key decrypts, so ciphertext written under the
	// old one keeps opening until a rekey pass rewrites it.
	old := config.Secrets{
		ActiveKeyID: "k1",
		Keys:        []config.SecretKey{{ID: "k1", Material: keyMaterial(t)}},
	}
	oldCipher, err := old.Cipher()
	if err != nil {
		t.Fatal(err)
	}
	sealedUnderOld, err := oldCipher.Encrypt("written before the rotation", secrets.AADForDocument)
	if err != nil {
		t.Fatal(err)
	}

	rotated := config.Secrets{
		ActiveKeyID: "k2",
		Keys: []config.SecretKey{
			{ID: "k1", Material: old.Keys[0].Material},
			{ID: "k2", Material: keyMaterial(t)},
		},
	}
	cipher, err := rotated.Cipher()
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := cipher.Encrypt("written after", secrets.AADForDocument)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fresh, "enc:v1:k2:") {
		t.Errorf("a new write was sealed with %q, want the active key k2", fresh)
	}
	opened, err := cipher.Decrypt(sealedUnderOld, secrets.AADForDocument)
	if err != nil {
		t.Fatalf("ciphertext from before the rotation stopped opening: %v", err)
	}
	if opened != "written before the rotation" {
		t.Errorf("opened %q", opened)
	}
}

func TestUnusableKeyMaterialRefusesToBoot(t *testing.T) {
	t.Parallel()
	// Booting past it would seal the NEXT revision under a key nobody can
	// reproduce — the store then holds ciphertext no deployment can open,
	// and the failure surfaces at the restart after the one that caused it.
	ring := config.Secrets{
		ActiveKeyID: "k1",
		Keys:        []config.SecretKey{{ID: "k1", Material: "not base64 at all!!"}},
	}
	cipher, err := ring.Cipher()
	if err == nil {
		t.Fatal("unusable key material produced a cipher")
	}
	if cipher != nil {
		t.Error("a refused keyring still returned a cipher")
	}
	if !strings.Contains(err.Error(), "k1") {
		t.Errorf("the error does not name the key: %v", err)
	}
	// The MATERIAL never reaches the message, even when it is nonsense: a
	// key id is a name and key material is a credential, and an error goes
	// to a log.
	if strings.Contains(err.Error(), "not base64 at all") {
		t.Errorf("the error carries the key material: %v", err)
	}
}

func TestKeyMaterialWithACorruptedTailIsRefused(t *testing.T) {
	t.Parallel()
	// The case the length check alone misses, and the reason the decode
	// error is checked rather than discarded: base64 that decodes to a full
	// 32 bytes BEFORE the corruption. Ignoring the error would boot a
	// cipher keyed on the valid prefix — a working cipher with the wrong
	// key, which seals a revision nobody can ever open.
	good := keyMaterial(t)
	ring := config.Secrets{
		ActiveKeyID: "k1",
		Keys:        []config.SecretKey{{ID: "k1", Material: good + "!!!"}},
	}
	if _, err := ring.Cipher(); err == nil {
		t.Fatal("key material with a corrupted tail was accepted, so the " +
			"cipher is keyed on whatever decoded before the corruption")
	}
}

func TestKeyMaterialOfTheWrongLengthIsRefused(t *testing.T) {
	t.Parallel()
	// Valid base64 and not a key. AES-256 needs 32 bytes, and a shorter
	// one is an operator who truncated a paste.
	ring := config.Secrets{
		ActiveKeyID: "k1",
		Keys: []config.SecretKey{{ID: "k1",
			Material: base64.StdEncoding.EncodeToString([]byte("too short"))}},
	}
	if _, err := ring.Cipher(); err == nil {
		t.Fatal("a 9-byte key was accepted as AES-256")
	}
}
