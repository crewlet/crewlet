package whsec_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/whsec"
)

// A MINTED SECRET IS ONE THE VERIFIER CAN USE. The two halves of this
// package have to meet, and they are the only halves — if minting and
// decoding disagreed, every delivery would fail with a signature mismatch
// and nothing would name the encoding.
func TestAMintedSecretDecodesToItsOwnKey(t *testing.T) {
	t.Parallel()
	secret, err := whsec.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(secret, whsec.Prefix) {
		t.Fatalf("minted %q, which carries no %s prefix", secret, whsec.Prefix)
	}
	key, ok := whsec.Key(secret)
	if !ok {
		t.Fatalf("the verifier cannot decode a secret this package minted: %q", secret)
	}
	if len(key) != whsec.KeyBytes {
		t.Errorf("key is %d bytes, want %d", len(key), whsec.KeyBytes)
	}
	// The KEY is the decoded bytes, never the printable form. Keying on the
	// text is the mistake that costs every delivery while looking correct
	// from both ends.
	if string(key) == secret {
		t.Error("the key is the printable secret rather than its decoded bytes")
	}
}

// TWO MINTS DIFFER. A constant would verify every forged delivery in every
// deployment, and there is no other symptom.
func TestTwoMintsDiffer(t *testing.T) {
	t.Parallel()
	first, err := whsec.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	second, err := whsec.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if first == second {
		t.Fatal("two mints produced the same secret")
	}
}

// WHAT THE VENDOR COULD NOT HAVE PRODUCED IS REFUSED.
//
// The short-key case is the one that drifted while this rule lived in two
// places: the config validator required 32 bytes and the webhook verifier
// took a payload of any length, so a ${VAR} holding a 16-byte key was
// refused as a literal and silently accepted as a reference — verifying
// every delivery against a weaker key than the operator believed they had.
func TestASecretTheVendorCouldNotHaveProducedIsRefused(t *testing.T) {
	t.Parallel()
	std := func(n int) string {
		return whsec.Prefix + base64.StdEncoding.EncodeToString(make([]byte, n))
	}
	for _, tc := range []struct{ name, secret string }{
		{"empty", ""},
		{"no prefix", base64.StdEncoding.EncodeToString(make([]byte, 32))},
		{"a hyphen for the underscore", "whsec-" + base64.StdEncoding.EncodeToString(make([]byte, 32))},
		{"not base64 at all", whsec.Prefix + "not base64!!"},
		{"url-safe base64", whsec.Prefix + base64.URLEncoding.EncodeToString([]byte{0xfb, 0xff, 0xfe, 0xfd})},
		{"a 16-byte key", std(16)},
		{"a 64-byte key", std(64)},
		{"the prefix alone", whsec.Prefix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if key, ok := whsec.Key(tc.secret); ok {
				t.Fatalf("accepted %q as a %d-byte signing key", tc.secret, len(key))
			}
			if whsec.Valid(tc.secret) {
				t.Error("Valid disagrees with Key, so two callers get two answers")
			}
		})
	}
}
