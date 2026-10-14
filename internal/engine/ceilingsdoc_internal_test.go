package engine

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// embeddedBrokerGrant is the share of its store volume's free space an
// embedded broker grants when `stream.store_max_bytes` is unset — three
// quarters, as a numerator over a denominator.
//
// STATED HERE rather than read, because it is nats-server's own derivation,
// not a value this module declares: the engine reads the broker's limit at run
// time ([jetstream.BudgetServerStore]), and the pages restate the three
// quarters to say what a volume needs. TestTheStateLogsFitTheBrokerTheyBootOn
// is what exercises a real broker at that limit.
const (
	embeddedBrokerGrantNum = 3
	embeddedBrokerGrantDen = 4
)

// THE PAGES' VOLUME ARITHMETIC IS THE ENGINE'S.
//
// deployment.md, replication.md and retention.md each tell an operator what a
// volume needs: the free space a first boot cannot do without (every log's
// floor, inside the three quarters the broker grants), what the logs ask for
// together once every derived ask has reached its clamp, and the free space
// above which none of them is scaled down (that sum over the logs' half of the
// broker's grant). Each figure is a product of [StreamBudgetShare],
// [MinDomainCeiling], every registered domain's Tier A ask and the broker's
// grant — and nothing tied the prose to any of them, which is how the
// threshold once read 290 GiB where the arithmetic said about 387, and how a
// fifth log would have left every figure four logs wide.
//
// So each is COMPUTED here from the code that sizes the ceilings and looked for
// in the pages that state it, with the whitespace of a wrapped paragraph
// folded away. A clamp moved, the share changed or a domain registered fails
// here, naming the figure the pages must now say.
func TestThePagesStateTheVolumeArithmeticTheEngineSizesBy(t *testing.T) {
	t.Parallel()
	domains := registeredDomains()
	count := numberWord(t, len(domains))

	// THE FLOOR: every log at MinDomainCeiling, inside the broker's grant.
	if MinDomainCeiling%gib != 0 {
		t.Fatalf("MinDomainCeiling is %d bytes, not a whole number of GiB; "+
			"the pages state it as one", MinDomainCeiling)
	}
	floors := int64(len(domains)) * MinDomainCeiling
	floorEach := fmt.Sprintf("%d GiB", MinDomainCeiling/gib)
	firstBoot := gibFraction(t, floors*embeddedBrokerGrantDen, embeddedBrokerGrantNum)

	// THE FULL ASK: what the logs ask for together once every derived ask has
	// reached its clamp — which is a plateau, and the sum at twice the free
	// space is how this tells one from a sum still growing.
	plateau := func(free int64) int64 {
		var sum int64
		for _, domain := range domains {
			ceiling, err := tierACeiling(config.Stream{}, domain, free)
			if err != nil {
				t.Fatalf("tierACeiling(%s): %v", domain.Name(), err)
			}
			sum += ceiling.Bytes
		}
		return sum
	}
	const big = int64(1) << 40
	full := plateau(big)
	if grown := plateau(2 * big); grown != full {
		t.Fatalf("the logs ask for %d bytes at %d free and %d at twice that: a "+
			"derived ask is not clamped, so \"at their clamps\" names nothing "+
			"and the pages' threshold has no value", full, big, grown)
	}
	if full%gib != 0 {
		t.Fatalf("at their clamps the logs ask for %d bytes, not a whole number "+
			"of GiB; the pages state it as one", full)
	}
	fullAsk := fmt.Sprintf("%d GiB", full/gib)

	// THE THRESHOLD: the full ask over the logs' half of the broker's grant.
	share := StreamBudgetShare * embeddedBrokerGrantNum / embeddedBrokerGrantDen
	threshold := int64(math.Round(float64(full) / share / float64(gib)))
	shareWords := fractionWords(t, share)

	for page, want := range map[string][]string{
		"docs/guides/deployment.md": {
			"at least " + firstBoot + " free",
			"the " + count + " " + floorEach + " floors",
			"half of three quarters, so " + shareWords + " of the free space",
			fmt.Sprintf("less than about %d GiB free", threshold),
			fmt.Sprintf("the %s ask for %s, %s of %d", count, fullAsk, shareWords, threshold),
		},
		"docs/guides/replication.md": {
			"at least " + firstBoot + " free",
			"the " + count + " " + floorEach + " floors",
		},
		"docs/guides/retention.md": {
			fmt.Sprintf("about %d GiB or more", threshold),
		},
	} {
		text := foldedPage(t, page)
		for _, phrase := range want {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s does not say %q. The engine sizes the %s logs to "+
					"%s of the free space with a %s floor each, so a first boot "+
					"needs %s, they ask for %s at their clamps, and nothing is "+
					"scaled from about %d GiB free", page, phrase, count,
					shareWords, floorEach, firstBoot, fullAsk, threshold)
			}
		}
	}
}

// foldedPage is a docs page with every run of whitespace folded to one space,
// so a phrase the page wraps across lines is still one phrase.
func foldedPage(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(sourcetree.Root(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return strings.Join(strings.Fields(string(body)), " ")
}

// gibFraction renders num/den bytes in GiB as the pages write it: a whole
// number, or one with a third — the only fraction a grant of three quarters
// leaves on a floor of whole gibibytes.
func gibFraction(t *testing.T, num, den int64) string {
	t.Helper()
	if num%gib != 0 {
		t.Fatalf("%d/%d bytes is not a fraction of whole GiB", num, den)
	}
	whole, part := (num/gib)/den, (num/gib)%den
	switch {
	case part == 0:
		return fmt.Sprintf("%d GiB", whole)
	case den == 3 && part == 1:
		return fmt.Sprintf("%d⅓ GiB", whole)
	case den == 3 && part == 2:
		return fmt.Sprintf("%d⅔ GiB", whole)
	}
	t.Fatalf("%d/%d GiB has no rendering here; teach gibFraction and the pages "+
		"the fraction the arithmetic now leaves", num/gib, den)
	return ""
}

// fractionWords is the share of the free space the logs are sized into, as the
// pages say it.
func fractionWords(t *testing.T, share float64) string {
	t.Helper()
	if share == 0.375 {
		return "three eighths"
	}
	t.Fatalf("the logs are sized into %v of the free space, which the pages "+
		"have no words for; say it there and here", share)
	return ""
}

// numberWord is how the pages count the state logs.
func numberWord(t *testing.T, n int) string {
	t.Helper()
	words := []string{"zero", "one", "two", "three", "four", "five", "six",
		"seven", "eight", "nine", "ten"}
	if n < 0 || n >= len(words) {
		t.Fatalf("%d state logs: numberWord cannot say it", n)
	}
	return words[n]
}
