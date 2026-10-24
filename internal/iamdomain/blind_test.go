package iamdomain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A key nothing real uses, 32 bytes, so the floor is satisfied by shape
// rather than by a literal this file would have to keep in step with it.
var testBlindKey = []byte(strings.Repeat("k", iamdomain.MinBlindKeyBytes))

// EVERY NODE COMPUTES THE SAME BLIND FOR ONE ADDRESS, and it is the property
// the whole subject grammar rests on.
//
// A blind IS the subject a claim on an address arbitrates against. Two nodes
// that derived different blinds for one address would publish to two subjects,
// neither would contend with the other, and BOTH claims would land — which is
// the duplicate identity the grammar exists to prevent, arriving through the
// one door the grammar cannot watch.
func TestOneAddressYieldsOneBlindOnEveryNode(t *testing.T) {
	t.Parallel()
	a, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	// A SECOND BLINDER over the same key, which is what a second node is.
	b, err := iamdomain.NewBlinder(append([]byte(nil), testBlindKey...))
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	for _, address := range []string{
		"sarah.chen@example.com",
		"Sarah.Chen@Example.COM",
		" notif+sarah-chen@example.com ",
	} {
		first, err := a.Email(address)
		if err != nil {
			t.Fatalf("blind %q: %v", address, err)
		}
		second, err := b.Email(address)
		if err != nil {
			t.Fatalf("blind %q: %v", address, err)
		}
		if first != second {
			t.Errorf("two nodes blinded %q as %q and %q — each would claim it "+
				"on a subject of its own, neither would contend, and both "+
				"claims would land", address, first, second)
		}
	}
}

// AND THE FOLD IS THE ORG CHART'S FOLD, so one address reaches one person.
//
// The three spellings above are one address: lower-cased and plus-tag
// stripped is how a vendor payload resolves to a SEAT, and it has to be how a
// sign-in resolves to a PERSON, or the two halves of "who is this" answer
// differently about one string.
func TestEveryFormOfOneAddressBlindsTheSame(t *testing.T) {
	t.Parallel()
	b, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	want, err := b.Email("sarah.chen@example.com")
	if err != nil {
		t.Fatalf("blind: %v", err)
	}
	for _, spelling := range []string{
		"Sarah.Chen@Example.COM",
		"  sarah.chen@example.com  ",
		"sarah.chen+jira@example.com",
		"Sarah.Chen+GitHub@EXAMPLE.com",
	} {
		got, err := b.Email(spelling)
		if err != nil {
			t.Fatalf("blind %q: %v", spelling, err)
		}
		if got != want {
			t.Errorf("%q blinds to %q and the canonical form to %q — a person "+
				"would be found by one spelling of their address and not "+
				"another", spelling, got, want)
		}
	}
	// AND TWO DIFFERENT ADDRESSES DO NOT COLLIDE, which is the other half
	// and the one a broken derivation passes without.
	other, err := b.Email("dev.patel@example.com")
	if err != nil {
		t.Fatalf("blind: %v", err)
	}
	if other == want {
		t.Fatal("two different addresses blind to one value — the directory " +
			"would refuse every person after the first as holding somebody " +
			"else's address")
	}
}

// A BLIND CARRIES NOTHING OF THE ADDRESS, which is the only reason the whole
// mechanism exists.
func TestABlindCarriesNothingOfTheAddress(t *testing.T) {
	t.Parallel()
	b, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	blind, err := b.Email("sarah.chen@example.com")
	if err != nil {
		t.Fatalf("blind: %v", err)
	}
	if strings.ContainsAny(blind, ". \t\n*>/+=%_") {
		t.Errorf("%q carries a character a path, a pattern or a filter reads "+
			"as structure", blind)
	}
	// AND IT CARRIES NOTHING OF THE ADDRESS. The whole point: this value
	// is in every directory record on the log, in every node's rows and in
	// every trail row an invitation writes, in the clear.
	for _, fragment := range []string{"sarah", "chen", "example", "@"} {
		if strings.Contains(blind, fragment) {
			t.Errorf("the blind %q contains %q of the address it is for", blind, fragment)
		}
	}
}

// AN ADDRESS'S BLIND IS THE SAME ON EVERY BUILD.
//
// The blind is the value the directory compares an address by and the column a
// sign-in resolves through, so a build that derived it differently would
// re-key every address in the company at once: a sign-in would resolve nobody,
// and a second person could take each address under its new blind. The value
// is pinned rather than recomputed so the derivation's SHAPE is held — the
// domain, the address class inside the MAC and the separators — and not only
// its determinism.
//
// Mutation: drop the class from the MAC, or the separator after it, and the
// blind changes.
func TestAnAddressBlindIsTheSameOnEveryBuild(t *testing.T) {
	t.Parallel()
	b, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	blind, err := b.Email("Sarah.Chen@example.com")
	if err != nil {
		t.Fatalf("blind: %v", err)
	}
	const want = "61af38c1dca85239e60ff6a1d888a53192a61a7c8a0fb27e90b41b2d098510aa"
	if blind != want {
		t.Errorf("the address blinds to %s, want %s — every address claim in "+
			"an existing estate was derived the old way", blind, want)
	}
}

// AN UNKEYED OR SHORT-KEYED BLINDER IS REFUSED RATHER THAN BUILT.
//
// An empty HMAC key is a valid HMAC key: it produces stable, plausible blinds
// that any other deployment with the same bug reproduces, and the estate looks
// exactly as it does when it works. A short key is what makes an address space
// an attacker can enumerate into a lookup table.
func TestABlinderRefusesAKeyThatBlindsNothing(t *testing.T) {
	t.Parallel()
	for name, key := range map[string][]byte{
		"no key at all": nil,
		"empty":         {},
		"one byte short": []byte(strings.Repeat("k",
			iamdomain.MinBlindKeyBytes-1)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, err := iamdomain.NewBlinder(key)
			if err == nil {
				t.Fatalf("a blinder was built over %d bytes", len(key))
			}
			if !errors.Is(err, iamdomain.ErrNoBlindKey) {
				t.Errorf("the refusal is %v, which a caller cannot tell from a "+
					"lookup that found nobody — and the two send a request to "+
					"opposite places", err)
			}
			if b != nil {
				t.Error("a refused blinder came back non-nil")
			}
		})
	}
}

// AN EMPTY ADDRESS HAS NO BLIND.
//
// A claim on one would arbitrate on a subject every addressless person in the
// company shares, so the second one to be enrolled would be refused for a
// collision with somebody they have nothing to do with.
func TestAnEmptyAddressHasNoBlind(t *testing.T) {
	t.Parallel()
	b, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	for _, empty := range []string{"", "   ", "\t\n"} {
		if blind, err := b.Email(empty); err == nil {
			t.Errorf("%q blinded to %q", empty, blind)
		}
	}
}

// A DIFFERENT COMPANY'S KEY GIVES A DIFFERENT BLIND, which is what "keyed"
// buys over a plain digest: an address is drawn from a set an attacker can
// enumerate, so an unkeyed digest of one is a lookup table.
func TestTheKeyIsWhatMakesTheBlindBlind(t *testing.T) {
	t.Parallel()
	mine, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	theirs, err := iamdomain.NewBlinder([]byte(strings.Repeat("z",
		iamdomain.MinBlindKeyBytes)))
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	const address = "sarah.chen@example.com"
	a, err := mine.Email(address)
	if err != nil {
		t.Fatalf("blind: %v", err)
	}
	b, err := theirs.Email(address)
	if err != nil {
		t.Fatalf("blind: %v", err)
	}
	if a == b {
		t.Fatal("two companies blind one address identically — the derivation " +
			"is not reading the key, so every blind in the estate is a plain " +
			"digest an attacker can build a table for")
	}
}
