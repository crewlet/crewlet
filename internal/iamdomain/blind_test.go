package iamdomain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A key nothing real uses, sized by the floor's constant, not a literal.
var testBlindKey = []byte(strings.Repeat("k", iamdomain.MinBlindKeyBytes))

// EVERY NODE COMPUTES THE SAME BLIND FOR ONE ADDRESS: a blind IS the subject a
// claim on an address arbitrates against, so two nodes deriving different
// blinds would publish to two subjects that never contend, and BOTH claims
// would land as a duplicate identity.
func TestOneAddressYieldsOneBlindOnEveryNode(t *testing.T) {
	t.Parallel()
	a, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	// A SECOND BLINDER over the same key: a second node.
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

// AND THE FOLD IS THE ORG CHART'S FOLD, so one address reaches one person:
// lower-cased and plus-tag stripped is how a vendor payload resolves to a
// SEAT, and must be how a sign-in resolves to a PERSON.
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
	// AND TWO DIFFERENT ADDRESSES DO NOT COLLIDE, the half a broken
	// derivation would otherwise pass.
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

// A BLIND CARRIES NOTHING OF THE ADDRESS, the reason the mechanism exists.
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
	// AND NOTHING OF THE ADDRESS: this value is in the clear in every
	// directory record, data node row and invitation trail row.
	for _, fragment := range []string{"sarah", "chen", "example", "@"} {
		if strings.Contains(blind, fragment) {
			t.Errorf("the blind %q contains %q of the address it is for", blind, fragment)
		}
	}
}

// AN ADDRESS'S BLIND IS THE SAME ON EVERY BUILD: a build deriving it
// differently would re-key every address at once, so a sign-in resolves
// nobody and a second person could take each address. The value is pinned,
// not recomputed, so the derivation's SHAPE (domain, class inside the MAC,
// separators) is held and not only its determinism.
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

// AN UNKEYED OR SHORT-KEYED BLINDER IS REFUSED RATHER THAN BUILT, with
// [iamdomain.ErrNoBlindKey]: an empty key blinds plausibly for every deployment
// ([iamdomain.NewBlinder]), and a short one makes the enumerable address space
// a lookup table.
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

// AN EMPTY ADDRESS HAS NO BLIND: a claim on one would arbitrate on a subject
// every addressless person shares, refusing the second one enrolled.
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

// A DIFFERENT COMPANY'S KEY GIVES A DIFFERENT BLIND: an unkeyed digest of an
// enumerable address is a lookup table.
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
