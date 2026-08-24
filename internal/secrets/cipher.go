// Package secrets seals configuration secrets at rest and resolves them at
// read time.
//
// Two composable mechanisms share one keyring. A Cipher seals a value into a
// self-describing envelope, used both for whole-document company-config
// encryption and for the per-variable secret store. A Source answers ${VAR}
// lookups ahead of the process environment, so a rotated secret takes effect
// without anyone editing a file.
//
// # The envelope
//
// "enc:v1:<key_id>:<base64>". It carries the id of the key that sealed it,
// so one revision can hold ciphertexts under a MIX of keys during a rotation
// and each decrypts under the right entry. The "v1" is how a future
// algorithm change stays distinguishable from existing ciphertext rather
// than silently failing to parse.
//
// # Associated data is not optional
//
// Every seal binds the value's location — a field path, or the variable's
// name in the secret store — as AEAD associated data. Without it a
// ciphertext is portable: an operator (or an attacker with write access to
// the config) could move a sealed high-privilege token into a
// lower-privilege field and the engine would decrypt it happily. With it,
// the move fails to authenticate.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	// EnvelopePrefix tags every sealed value.
	EnvelopePrefix = "enc:v1:"

	// keyBytes is the AES-256 key size.
	keyBytes = 32
)

// ErrDecrypt is returned for EVERY unsealing failure — wrong key, unknown
// key id, tampered ciphertext, or an associated-data mismatch.
//
// Deliberately uniform: a caller that could tell those apart would have a
// decryption oracle, and the difference between "wrong key" and "wrong
// field" is exactly the signal an attacker probing a config would want.
var ErrDecrypt = errors.New("secret could not be decrypted")

// ErrNoKey means the keyring has no usable key. Distinct from ErrDecrypt
// because it is an operator configuration problem, not a data problem, and
// it must fail loudly rather than yielding an empty string — an empty secret
// becomes an empty Bearer token discovered hours later.
var ErrNoKey = errors.New("no encryption key configured")

// Cipher seals and unseals secrets.
type Cipher interface {
	// Encrypt seals plaintext, binding aad as associated data.
	Encrypt(plaintext, aad string) (string, error)
	// Decrypt unseals an envelope, requiring the same aad.
	Decrypt(token, aad string) (string, error)
}

// IsEnvelope reports whether a value is a well-formed sealed envelope.
//
// Used to tell an already-sealed value from a plaintext one so re-sealing a
// document is idempotent, and so a redaction pass can recognise ciphertext
// without attempting to decrypt it.
func IsEnvelope(value string) bool {
	_, ok := EnvelopeKeyID(value)
	return ok
}

// EnvelopeKeyID reads which keyring key sealed a value.
//
// THE ONE PARSE of the envelope's shape, because the key id is denormalised
// into a column — a rotation sweep finds stale rows by comparing it, without
// decrypting any of them — and a second reading of the format is how the
// column and the ciphertext come to disagree about which key opens a row.
//
// False for anything that is not a well-formed envelope, which is what
// [IsEnvelope] is: telling a sealed value from a plaintext one is the same
// question as "which key sealed it", answered once.
func EnvelopeKeyID(value string) (string, bool) {
	id, _, ok := splitEnvelope(value)
	return id, ok
}

// splitEnvelope is THE parse of the envelope's shape: prefix, key id,
// base64 payload. Both the key-id reader above and Decrypt below go through
// it, because two readings of one format is how a column that says which key
// opens a row comes to disagree with the ciphertext in it.
func splitEnvelope(value string) (id string, raw []byte, ok bool) {
	if !strings.HasPrefix(value, EnvelopePrefix) {
		return "", nil, false
	}
	id, blob, cut := strings.Cut(strings.TrimPrefix(value, EnvelopePrefix), ":")
	if !cut || id == "" || blob == "" {
		return "", nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return "", nil, false
	}
	return id, decoded, true
}

// Keyring is a set of named AES-256 keys, one of which is active.
//
// Multiple keys exist so a rotation is zero-downtime: the new key becomes
// active and seals new values, while old ciphertext keeps decrypting under
// the key that sealed it until a rekey pass rewrites it.
type Keyring struct {
	// ActiveID names the key new values are sealed under.
	ActiveID string
	// Keys maps key id to a 32-byte AES-256 key.
	Keys map[string][]byte
}

// Validate reports whether a keyring is usable.
func (k Keyring) Validate() error {
	if len(k.Keys) == 0 {
		return ErrNoKey
	}
	if k.ActiveID == "" {
		return fmt.Errorf("%w: no active key id", ErrNoKey)
	}
	if _, ok := k.Keys[k.ActiveID]; !ok {
		return fmt.Errorf("%w: active key %q is not in the keyring", ErrNoKey, k.ActiveID)
	}
	var errs []error
	for id, key := range k.Keys {
		if len(key) != keyBytes {
			errs = append(errs, fmt.Errorf("key %q is %d bytes, want %d", id, len(key), keyBytes))
		}
	}
	return errors.Join(errs...)
}

// keyringCipher is the AES-256-GCM implementation.
type keyringCipher struct {
	ring Keyring
}

// NewCipher builds a Cipher over a keyring.
func NewCipher(ring Keyring) (Cipher, error) {
	if err := ring.Validate(); err != nil {
		return nil, err
	}
	// Copy the key material so a later mutation of the caller's map
	// cannot silently change which key seals what.
	keys := make(map[string][]byte, len(ring.Keys))
	for id, k := range ring.Keys {
		keys[id] = append([]byte(nil), k...)
	}
	return &keyringCipher{ring: Keyring{ActiveID: ring.ActiveID, Keys: keys}}, nil
}

func (c *keyringCipher) aead(id string) (cipher.AEAD, error) {
	key, ok := c.ring.Keys[id]
	if !ok {
		return nil, ErrDecrypt
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrDecrypt
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrDecrypt
	}
	return gcm, nil
}

// Encrypt seals plaintext under the active key.
func (c *keyringCipher) Encrypt(plaintext, aad string) (string, error) {
	gcm, err := c.aead(c.ring.ActiveID)
	if err != nil {
		return "", fmt.Errorf("%w: active key unusable", ErrNoKey)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	// The nonce is prefixed to the ciphertext rather than stored
	// separately: it is not secret, it must never repeat under one key,
	// and keeping it beside the bytes it belongs to removes any way for
	// the two to be separated.
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return EnvelopePrefix + c.ring.ActiveID + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt unseals an envelope under whichever key sealed it.
func (c *keyringCipher) Decrypt(token, aad string) (string, error) {
	id, raw, ok := splitEnvelope(token)
	if !ok {
		return "", ErrDecrypt
	}
	gcm, err := c.aead(id)
	if err != nil {
		return "", ErrDecrypt
	}
	if len(raw) < gcm.NonceSize() {
		return "", ErrDecrypt
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ct, []byte(aad))
	if err != nil {
		// Every failure mode collapses to one error here. See ErrDecrypt.
		return "", ErrDecrypt
	}
	return string(plaintext), nil
}

// GenerateKey returns a fresh random AES-256 key.
func GenerateKey() ([]byte, error) {
	key := make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	return key, nil
}

// EncodeKey renders a key in the form the config's `material` field takes.
//
// Base64 of the raw bytes, which is what the loader decodes — so `crewlet
// secrets keygen` emits something pasteable rather than something to
// convert, and there is one definition of the encoding rather than one here
// and one in the loader.
func EncodeKey(key []byte) string { return base64.StdEncoding.EncodeToString(key) }

// AADForVar is the associated data binding a secret-store value to its
// variable name, so a ciphertext moved between rows fails to authenticate.
func AADForVar(name string) string { return "secret_values/" + name }

// AADForDocument is the associated data for a whole-config document.
const AADForDocument = "company_config/document"
