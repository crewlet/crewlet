package secrets

import (
	"errors"
	"strings"
	"testing"
)

func testRing(t *testing.T) Keyring {
	t.Helper()
	k1, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	k2, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return Keyring{ActiveID: "k2", Keys: map[string][]byte{"k1": k1, "k2": k2}}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	c, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	const secret = "sk-ant-not-a-real-key"
	token, err := c.Encrypt(secret, AADForVar("ANTHROPIC_API_KEY"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !IsEnvelope(token) {
		t.Errorf("Encrypt produced %q, which is not a recognised envelope", token)
	}
	if strings.Contains(token, secret) {
		t.Error("the envelope contains its own plaintext")
	}
	got, err := c.Decrypt(token, AADForVar("ANTHROPIC_API_KEY"))
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != secret {
		t.Errorf("Decrypt = %q, want %q", got, secret)
	}
}

// TestAADBindsLocation is the property that makes a sealed value
// non-portable: a ciphertext moved to a different field or variable must
// fail to authenticate, so nobody can promote a high-privilege token by
// copying it into a lower-privilege slot.
func TestAADBindsLocation(t *testing.T) {
	t.Parallel()
	c, _ := NewCipher(testRing(t))
	token, _ := c.Encrypt("privileged", AADForVar("ADMIN_TOKEN"))

	if _, err := c.Decrypt(token, AADForVar("READONLY_TOKEN")); !errors.Is(err, ErrDecrypt) {
		t.Errorf("a ciphertext moved between variables decrypted: %v", err)
	}
	if _, err := c.Decrypt(token, AADForDocument); !errors.Is(err, ErrDecrypt) {
		t.Errorf("a variable ciphertext decrypted as a document: %v", err)
	}
}

// TestEveryFailureIsOneError guards against a decryption oracle: an attacker
// probing a config must not be able to tell a wrong key from a wrong field
// from tampered bytes.
func TestEveryFailureIsOneError(t *testing.T) {
	t.Parallel()
	c, _ := NewCipher(testRing(t))
	good, _ := c.Encrypt("value", AADForVar("A"))

	other, _ := NewCipher(testRing(t)) // different key material entirely
	tampered := good[:len(good)-4] + "AAAA"

	for _, tc := range []struct {
		name string
		run  func() (string, error)
	}{
		{"unknown key id", func() (string, error) { return c.Decrypt("enc:v1:nope:AAAA", AADForVar("A")) }},
		{"wrong keyring", func() (string, error) { return other.Decrypt(good, AADForVar("A")) }},
		{"wrong aad", func() (string, error) { return c.Decrypt(good, AADForVar("B")) }},
		{"tampered ciphertext", func() (string, error) { return c.Decrypt(tampered, AADForVar("A")) }},
		{"not an envelope", func() (string, error) { return c.Decrypt("plain", AADForVar("A")) }},
		{"truncated", func() (string, error) { return c.Decrypt("enc:v1:k2:", AADForVar("A")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.run()
			if !errors.Is(err, ErrDecrypt) {
				t.Errorf("got %v, want ErrDecrypt — failure modes must be indistinguishable", err)
			}
		})
	}
}

// TestRotationKeepsOldCiphertextReadable is why the key id rides in the
// envelope: during a rotation one revision holds values under a mix of keys.
func TestRotationKeepsOldCiphertextReadable(t *testing.T) {
	t.Parallel()
	ring := testRing(t)

	old := Keyring{ActiveID: "k1", Keys: ring.Keys}
	oldCipher, _ := NewCipher(old)
	sealedUnderOld, _ := oldCipher.Encrypt("legacy", AADForVar("V"))

	// Rotate: k2 is now active, k1 remains in the ring.
	rotated, _ := NewCipher(ring)
	got, err := rotated.Decrypt(sealedUnderOld, AADForVar("V"))
	if err != nil || got != "legacy" {
		t.Fatalf("after rotation, old ciphertext = (%q, %v), want (legacy, nil)", got, err)
	}
	fresh, _ := rotated.Encrypt("current", AADForVar("V"))
	if !strings.HasPrefix(fresh, EnvelopePrefix+"k2:") {
		t.Errorf("new value sealed under %q, want the active key", fresh)
	}
}

func TestKeyringValidation(t *testing.T) {
	t.Parallel()
	good, _ := GenerateKey()
	for _, tc := range []struct {
		name string
		ring Keyring
	}{
		{"empty", Keyring{}},
		{"no active id", Keyring{Keys: map[string][]byte{"k": good}}},
		{"active key absent", Keyring{ActiveID: "missing", Keys: map[string][]byte{"k": good}}},
		{"short key", Keyring{ActiveID: "k", Keys: map[string][]byte{"k": []byte("too short")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewCipher(tc.ring); err == nil {
				t.Error("NewCipher accepted an unusable keyring; it must fail loudly")
			}
		})
	}
}

// TestChainStoreBeatsEnvironment is the security-critical resolution order:
// a stale .env must not shadow a rotated secret. The reverse order shipped
// once, and it made rotation a no-op that looked like a success.
func TestChainStoreBeatsEnvironment(t *testing.T) {
	t.Parallel()
	store := NewMapSource(map[string]string{"TOKEN": "rotated"})
	env := SourceFunc(func(n string) (string, bool) {
		if n == "TOKEN" {
			return "stale", true
		}
		return "", false
	})

	chain := NewChain(store, env)
	if got, ok := chain.Lookup("TOKEN"); !ok || got != "rotated" {
		t.Errorf("Lookup = (%q, %v), want (rotated, true)", got, ok)
	}
}

func TestChainFallsThroughAndSkipsNil(t *testing.T) {
	t.Parallel()
	env := NewMapSource(map[string]string{"ONLY_ENV": "value"})
	chain := NewChain(nil, NewMapSource(nil), env)

	if got, ok := chain.Lookup("ONLY_ENV"); !ok || got != "value" {
		t.Errorf("Lookup = (%q, %v), want (value, true)", got, ok)
	}
	if _, ok := chain.Lookup("NOWHERE"); ok {
		t.Error("Lookup reported a value for an unknown name")
	}
}

// TestFoundEmptyIsNotMissing: an operator who deliberately sets a variable
// to empty has said something, and falling through would override them.
func TestFoundEmptyIsNotMissing(t *testing.T) {
	t.Parallel()
	chain := NewChain(
		NewMapSource(map[string]string{"V": ""}),
		NewMapSource(map[string]string{"V": "fallback"}),
	)
	if got, ok := chain.Lookup("V"); !ok || got != "" {
		t.Errorf("Lookup = (%q, %v), want (\"\", true)", got, ok)
	}
}

// TestSnapshotReplaceIsAtomic exercises the swap the engine performs on every
// config activation, under -race.
func TestSnapshotReplaceIsAtomic(t *testing.T) {
	t.Parallel()
	snap := NewSnapshot(map[string]string{"V": "first"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 500 {
			snap.Replace(map[string]string{"V": "second"})
			snap.Replace(map[string]string{"V": "first"})
		}
	}()
	for range 500 {
		if v, ok := snap.Lookup("V"); !ok || (v != "first" && v != "second") {
			t.Errorf("Lookup during replace = (%q, %v)", v, ok)
			break
		}
	}
	<-done
}
