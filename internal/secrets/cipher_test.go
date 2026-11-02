package secrets

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// KEY MATERIAL IS DECODED ONE WAY, and what is not a key says so without
// saying what it was.
//
// The Tier A loader and `crewlet validate` both read material through this, so
// a key one of them accepts is a key the other accepts too. A newline around
// the material is a paste, not a different key.
func TestKeyMaterialIsDecodedOneWay(t *testing.T) {
	t.Parallel()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeKey("  " + EncodeKey(key) + "\n")
	if err != nil || !bytes.Equal(decoded, key) {
		t.Fatalf("DecodeKey(EncodeKey(key)) = (%x, %v), want the key", decoded, err)
	}
	for name, material := range map[string]string{
		"not base64":       "not-base64-at-all",
		"three bytes":      "AAAA",
		"a corrupted tail": EncodeKey(key) + "!!!",
		"empty":            "",
	} {
		got, err := DecodeKey(material)
		if !errors.Is(err, ErrKeyMaterial) || got != nil {
			t.Errorf("%s: DecodeKey = (%x, %v), want ErrKeyMaterial", name, got, err)
			continue
		}
		if material != "" && strings.Contains(err.Error(), material) {
			t.Errorf("%s: the refusal carries the material: %v", name, err)
		}
	}
}

// A DERIVED KEY IS THE FLEET'S, ONE PER PURPOSE. Every node holding the keyring
// derives the same key for one info string, so what it keys (a model's sign-in
// identity) compares across nodes and restarts; another purpose, another active
// key or another keyring derives another, and none of them is the key itself.
//
// Mutation: derive from the key id rather than its material, or ignore info,
// and two purposes or two keyrings share one key.
func TestADerivedKeyIsTheFleetsOnePerPurpose(t *testing.T) {
	t.Parallel()
	one, two := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	ring := &Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": one, "k2": two}}
	again := &Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": bytes.Clone(one)}}
	rotated := &Keyring{ActiveID: "k2", Keys: map[string][]byte{"k1": one, "k2": two}}

	key := ring.Derive("crewlet/llm-sign-in-id/v1")
	if len(key) != 32 {
		t.Fatalf("derived %d bytes, want 32", len(key))
	}
	if !bytes.Equal(key, again.Derive("crewlet/llm-sign-in-id/v1")) {
		t.Error("two holders of one keyring derive different keys")
	}
	if bytes.Equal(key, ring.Derive("crewlet/another-purpose/v1")) {
		t.Error("two purposes derive one key")
	}
	if bytes.Equal(key, rotated.Derive("crewlet/llm-sign-in-id/v1")) {
		t.Error("a rotated keyring derives the key it derived before")
	}
	if bytes.Equal(key, one) {
		t.Error("the derived key is the active key itself")
	}
	if got := (&Keyring{ActiveID: "missing"}).Derive("x"); got != nil {
		t.Errorf("a keyring with no active key derived %x", got)
	}
}
