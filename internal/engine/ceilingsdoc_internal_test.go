package engine

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/search"
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

// yearFiveHeadroomAtAsk is the vector log's headroom at the bottom of a full
// re-embedding at year five, on the ceiling a 64 GiB volume asks for — the
// one figure of retention.md's excursion that is not the engine's arithmetic
// but the corpus's (about 17 MB per agent seat per year). The page states it,
// and every other headroom figure there is derived from it and from the
// ceilings below, so a ceiling that moves moves them.
const yearFiveHeadroomAtAsk = 0.51

// THE PAGES' VOLUME ARITHMETIC IS THE ENGINE'S.
//
// deployment.md, replication.md and retention.md each tell an operator what a
// volume needs: what the logs reserve at their smallest and so the free space
// a first boot cannot do without, what the logs ask for together once every
// derived ask has reached its clamp, the free space above which none of them
// is scaled down (that sum over the logs' half of the broker's three
// quarters), and what the vector log is created with on a 64 GiB volume. Each
// figure is a product of [StreamBudgetShare], [MinDomainCeiling], every
// registered domain's Tier A ask, the division that fits them into the pool
// and the broker's grant — and nothing tied the prose to any of them, which is
// how the threshold read 290 GiB where the arithmetic says 388: the pool was
// taken as half the free space rather than half of three quarters of it.
//
// So each is COMPUTED here from the code that sizes the ceilings and looked
// for in the pages that state it, with the whitespace of a wrapped paragraph
// folded away. A clamp moved, the share changed or a domain registered fails
// here, naming the figure the pages must now say.
func TestThePagesStateTheVolumeArithmeticTheEngineSizesBy(t *testing.T) {
	t.Parallel()
	domains := registeredDomains()
	count := numberWord(t, len(domains))

	// What every log asks for on a volume with `free` bytes free, and what
	// the division fits them to in the pool that volume gives them.
	asks := func(free int64) map[string]domainCeiling {
		out := make(map[string]domainCeiling, len(domains))
		for _, domain := range domains {
			ceiling, err := tierACeiling(config.Stream{}, domain, free)
			if err != nil {
				t.Fatalf("tierACeiling(%s): %v", domain.Name(), err)
			}
			out[domain.Name()] = ceiling
		}
		return out
	}
	share := StreamBudgetShare * embeddedBrokerGrantNum / embeddedBrokerGrantDen
	created := func(free int64) map[string]domainCeiling {
		return divide(asks(free), int64(float64(free)*share))
	}
	sum := func(ceilings map[string]domainCeiling) int64 {
		var total int64
		for _, ceiling := range ceilings {
			total += ceiling.Bytes
		}
		return total
	}

	// THE FIRST BOOT: what the logs reserve on a volume with nothing to
	// give them — every derived log scaled to its floor, every fixed one
	// held at what it asked for, which is below the floor for some — and
	// the free space whose three quarters that is.
	smallest := sum(created(0))
	reserve := gibWords(t, smallest)
	firstBoot := gibWords(t, smallest*embeddedBrokerGrantDen/embeddedBrokerGrantNum)

	// THE FULL ASK: what the logs ask for together once every derived ask
	// has reached its clamp — which is a plateau, and the sum at twice the
	// free space is how this tells one from a sum still growing.
	const big = int64(1) << 40
	full := sum(asks(big))
	if grown := sum(asks(2 * big)); grown != full {
		t.Fatalf("the logs ask for %d bytes at %d free and %d at twice that: a "+
			"derived ask is not clamped, so \"at their clamps\" names nothing "+
			"and the pages' threshold has no value", full, big, grown)
	}
	fullAsk := gibWords(t, full)

	// THE THRESHOLD: the full ask over the logs' half of the broker's grant.
	threshold := int64(math.Round(float64(full) / share / float64(gib)))
	shareWords := fractionWords(t, share)

	// THE VECTOR LOG ON A 64 GiB VOLUME: what it asks for there, what the
	// division creates it with, and what it reaches at its clamp — with the
	// year-five re-embedding's headroom on each, from the page's own anchor.
	vectors := search.Domain{}.Name()
	const volume = 64 * gib
	vectorAsk := asks(volume)[vectors].Bytes
	vectorCreated := created(volume)[vectors].Bytes
	vectorClamp := asks(big)[vectors].Bytes
	if vectorAsk == 0 || vectorCreated == 0 || vectorClamp == 0 {
		t.Fatalf("no registered domain is named %q, so the vector log's figures "+
			"cannot be computed", vectors)
	}
	peak := (1 - yearFiveHeadroomAtAsk) * float64(vectorAsk)
	headroom := func(ceiling int64) int {
		return int(math.Round(100 * (1 - peak/float64(ceiling))))
	}

	for page, want := range map[string][]string{
		"docs/guides/deployment.md": {
			"at least " + firstBoot + " free",
			reserve + " — is what the " + count + " logs reserve at their smallest",
			"half of three quarters, so " + shareWords + " of the free space",
			fmt.Sprintf("less than about %d GiB free", threshold),
			fmt.Sprintf("the %s ask for %s, %s of %d", count, fullAsk, shareWords, threshold),
		},
		"docs/guides/replication.md": {
			"at least " + firstBoot + " free",
			reserve + " — is what the " + count + " logs reserve at their smallest",
		},
		"docs/guides/retention.md": {
			fmt.Sprintf("about %d %% at the %s a %s volume asks for",
				int(math.Round(100*yearFiveHeadroomAtAsk)), gibWords(t, vectorAsk),
				gibWords(t, volume)),
			fmt.Sprintf("about %d %% at the %s it is created with",
				headroom(vectorCreated), gibWords(t, vectorCreated)),
			fmt.Sprintf("%d %% at the %s a volume of about %d GiB or more gets",
				headroom(vectorClamp), gibWords(t, vectorClamp), threshold),
		},
	} {
		text := foldedPage(t, page)
		for _, phrase := range want {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s does not say %q. The engine sizes the %s logs into "+
					"%s of the free space: they reserve %s at their smallest, so a "+
					"first boot needs %s; they ask for %s at their clamps, so "+
					"nothing is scaled from about %d GiB free; and a %s volume "+
					"creates the vector log at %s of the %s it asks for",
					page, phrase, count, shareWords, reserve, firstBoot, fullAsk,
					threshold, gibWords(t, volume), gibWords(t, vectorCreated),
					gibWords(t, vectorAsk))
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

// gibWords renders a byte count in GiB as the pages write it: a whole number,
// or one with a single decimal where the count is a whole number of half GiB —
// which the identity log's 512 MiB makes of every sum it is in. A finer count
// fails rather than being rounded, because a page would have to round it too.
func gibWords(t *testing.T, bytes int64) string {
	t.Helper()
	switch {
	case bytes%gib == 0:
		return fmt.Sprintf("%d GiB", bytes/gib)
	case bytes%(gib/2) == 0:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gib))
	}
	t.Fatalf("%d bytes is no whole or half GiB; teach gibWords and the pages "+
		"the figure the arithmetic now leaves", bytes)
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
