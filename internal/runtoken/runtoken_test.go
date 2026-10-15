package runtoken

import (
	"bytes"
	"testing"
	"time"
)

// DIFFERENT KEYRINGS ARE DIFFERENT KEYS, including the pairs that were the same
// bytes once concatenated. Each pair below derived ONE key when the material
// was joined bare after the domain and a `|`: the boundary between two entries,
// and between the domain and the first entry, was not part of the input.
//
// Mutation: hash `domain + "|" + strings.Join(sorted, "")` again, and the
// first two pairs collide; join the entries with a `|` instead, and the third
// still does.
func TestDifferentKeyringsDeriveDifferentKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		domainA   string
		materialA []string
		domainB   string
		materialB []string
	}{
		{"an entry boundary moved", "otel-run", []string{"ab", "c"}, "otel-run", []string{"a", "bc"}},
		{"the domain boundary moved", "a|b", []string{"c"}, "a", []string{"b|c"}},
		{"one entry holding the separator", "otel-run", []string{"k1|k2"}, "otel-run", []string{"k1", "k2"}},
		{"a key listed twice", "otel-run", []string{"k1:m", "k1:m"}, "otel-run", []string{"k1:m"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := KeyFrom(tc.domainA, tc.materialA)
			b := KeyFrom(tc.domainB, tc.materialB)
			if bytes.Equal(a, b) {
				t.Fatalf("KeyFrom(%q, %q) and KeyFrom(%q, %q) derived the same key",
					tc.domainA, tc.materialA, tc.domainB, tc.materialB)
			}
		})
	}
}

// EVERY NODE DERIVES THE SAME KEY from the same keyring, whatever order it
// lists the keys in — a fleet whose nodes disagree refuses every token that
// reaches a node other than the one that minted it.
func TestTheKeyringsOrderDoesNotChangeTheKey(t *testing.T) {
	t.Parallel()
	a := KeyFrom("otel-run", []string{"k1:first", "k2:second", "k3:third"})
	b := KeyFrom("otel-run", []string{"k3:third", "k1:first", "k2:second"})
	if !bytes.Equal(a, b) {
		t.Fatal("one keyring listed in two orders derived two keys")
	}
	if len(a) != 32 {
		t.Fatalf("the key is %d bytes, want a SHA-256's 32", len(a))
	}
}

// A DOMAIN'S TOKENS ARE ITS OWN: a token minted for the telemetry receiver is
// refused at the tool bridge, though both keys come from one keyring.
func TestATokenIsRefusedOutsideItsDomain(t *testing.T) {
	t.Parallel()
	material := []string{"k1:material"}
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return at }
	otel := New(Options{Key: KeyFrom("otel-run", material), Now: now})
	bridge := New(Options{Key: KeyFrom("mcp-bridge", material), Now: now})

	token := otel.Mint("4bf92f3577b34da6a3ce929d0e0e4736", time.Hour)
	if got := otel.Validate(token); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("the minting domain validated %q, want the subject", got)
	}
	if got := bridge.Validate(token); got != "" {
		t.Fatalf("another domain validated the token as %q", got)
	}
}

// NO KEYRING, NO DERIVED KEY: nil is what [New] turns into a per-process key,
// rather than a key derived from the domain alone, which anybody who can read
// this source could compute.
func TestNoMaterialDerivesNoKey(t *testing.T) {
	t.Parallel()
	if key := KeyFrom("otel-run", nil); key != nil {
		t.Fatalf("no material derived %x", key)
	}
}
