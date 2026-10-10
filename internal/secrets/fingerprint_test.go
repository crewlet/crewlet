package secrets

import (
	"bytes"
	"strings"
	"testing"
)

// ringOf builds a keyring from fixed key material, so two calls with the same
// letters are two nodes holding the same key.
func ringOf(keys map[string]byte) Keyring {
	ring := Keyring{Keys: map[string][]byte{}}
	for id, fill := range keys {
		ring.Keys[id] = bytes.Repeat([]byte{fill}, keyBytes)
		ring.ActiveID = id
	}
	return ring
}

func fingerprint(t *testing.T, ring Keyring, domain, value string) Fingerprint {
	t.Helper()
	f, err := ring.Fingerprint(domain, []byte(value))
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	return f
}

// TWO NODES HOLDING ONE VALUE UNDER ONE KEYRING AGREE, and holding two values
// they differ — the comparison the whole primitive exists for, in both
// directions, and symmetric.
func TestOneKeyringSaysWhetherTwoValuesAreTheSame(t *testing.T) {
	t.Parallel()
	ring := ringOf(map[string]byte{"k1": 1})
	a := fingerprint(t, ring, "test", "table one")
	if got := a.Against(fingerprint(t, ring, "test", "table one")); got != AgreementSame {
		t.Errorf("one value against itself = %s, want same", got)
	}
	b := fingerprint(t, ring, "test", "table two")
	if got := a.Against(b); got != AgreementDifferent {
		t.Errorf("two values = %s, want different", got)
	}
	if got := b.Against(a); got != AgreementDifferent {
		t.Errorf("two values the other way round = %s, want different", got)
	}
}

// A ROLLING KEYRING ROTATION NEVER LEAVES TWO NODES WITH NOTHING TO COMPARE.
//
// The new key is added on every node, made active and the old one dropped, one
// restart at a time: at each step two neighbouring nodes share at least one
// key, and a fingerprint under the active key alone — or under one key derived
// from the whole ring — would differ between them over an identical value.
func TestARollingKeyringRotationKeepsTheComparison(t *testing.T) {
	t.Parallel()
	steps := []Keyring{
		ringOf(map[string]byte{"k1": 1}),
		ringOf(map[string]byte{"k1": 1, "k2": 2}),
		ringOf(map[string]byte{"k2": 2}),
	}
	for i := 0; i+1 < len(steps); i++ {
		before := fingerprint(t, steps[i], "test", "the table")
		after := fingerprint(t, steps[i+1], "test", "the table")
		if got := before.Against(after); got != AgreementSame {
			t.Errorf("step %d against step %d = %s, want same", i, i+1, got)
		}
		changed := fingerprint(t, steps[i+1], "test", "another table")
		if got := before.Against(changed); got != AgreementDifferent {
			t.Errorf("step %d against a changed table at step %d = %s, want different", i, i+1, got)
		}
	}
}

// NOTHING IN COMMON IS NOT AN AGREEMENT: a node with no keyring, and two
// whose keyrings share no key, are incomparable — never "same", which would
// clear a check nobody ran, and never "different", which would page somebody
// over a fleet that chose to run without a keyring.
func TestNothingInCommonIsIncomparable(t *testing.T) {
	t.Parallel()
	one := fingerprint(t, ringOf(map[string]byte{"k1": 1}), "test", "the table")
	none := fingerprint(t, Keyring{}, "test", "the table")
	if len(none) != 0 {
		t.Fatalf("a ring with no keys fingerprinted to %v", none)
	}
	if got := one.Against(none); got != AgreementIncomparable {
		t.Errorf("against no keyring = %s, want incomparable", got)
	}
	other := fingerprint(t, ringOf(map[string]byte{"k9": 9}), "test", "another table")
	if got := one.Against(other); got != AgreementIncomparable {
		t.Errorf("against a disjoint keyring = %s, want incomparable", got)
	}
}

// ONE KEY ID, TWO KEYS, IS NOT TWO TABLES. A keyring written with different
// material under the same id digests one value differently, and the check is
// what stops that reading as a divergence in the value.
func TestAKeyIdOverDifferentMaterialComparesNothing(t *testing.T) {
	t.Parallel()
	a := fingerprint(t, ringOf(map[string]byte{"k1": 1}), "test", "the table")
	b := fingerprint(t, ringOf(map[string]byte{"k1": 7}), "test", "the table")
	if a["k1"].Digest == b["k1"].Digest {
		t.Fatal("two keys digested one value alike, so this case proves nothing")
	}
	if got := a.Against(b); got != AgreementIncomparable {
		t.Errorf("one id over two keys = %s, want incomparable", got)
	}
}

// THE DOMAIN SEPARATES, AND NOTHING OF THE VALUE TRAVELS. One value under two
// domains digests differently, so a fingerprint of one kind of thing never
// matches another's; and neither the value nor the key material appears in
// what is published.
func TestAFingerprintIsOfItsDomainAndCarriesNoInput(t *testing.T) {
	t.Parallel()
	ring := ringOf(map[string]byte{"k1": 1})
	a := fingerprint(t, ring, "one", "sk-not-a-real-key")
	b := fingerprint(t, ring, "two", "sk-not-a-real-key")
	if a["k1"].Digest == b["k1"].Digest {
		t.Error("one value under two domains digested alike")
	}
	for _, d := range a {
		if strings.Contains(d.Digest+d.Check, "sk-not") {
			t.Errorf("the published fingerprint %+v carries its input", d)
		}
	}
	if _, err := ring.Fingerprint("", nil); err == nil {
		t.Error("a fingerprint with no domain was taken")
	}
	short := Keyring{Keys: map[string][]byte{"k1": {1, 2, 3}}}
	if _, err := short.Fingerprint("one", nil); err == nil {
		t.Error("a fingerprint was taken under a key of the wrong size")
	}
}

// A PAIR THAT DISAGREES UNDER ANY KEY IT SHARES IS NEVER REPORTED AS AGREEING,
// whichever key a map's iteration happens to reach first.
func TestDifferentWinsOverSame(t *testing.T) {
	t.Parallel()
	ring := ringOf(map[string]byte{"k1": 1, "k2": 2})
	a := fingerprint(t, ring, "test", "the table")
	forged := Fingerprint{"k1": a["k1"], "k2": {Check: a["k2"].Check, Digest: strings.Repeat("0", 64)}}
	for range 20 {
		if got := a.Against(forged); got != AgreementDifferent {
			t.Fatalf("a pair agreeing under k1 and differing under k2 = %s, want different", got)
		}
	}
	for _, ag := range Agreements {
		if !ag.Valid() {
			t.Errorf("%q is listed and not valid", ag)
		}
	}
	if Agreement("maybe").Valid() {
		t.Error("an agreement this build never sends is valid")
	}
}
