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
	"github.com/crewlet/crewlet/internal/statelog"
)

// vectorYearFivePeak is the vector log's peak during a full re-embedding at
// year five of the reference company: the whole corpus, which a model change
// republishes and keeps in the window for a week (about 17 MB per agent seat
// per year). It is the one figure of retention.md's excursion that is not the
// engine's arithmetic but the corpus's. The page quotes it as the headroom it
// leaves on the ceiling a 64 GiB volume asks for, and every other headroom
// figure there is derived from it and from the ceilings below, so a ceiling
// that moves moves them; TestTheVectorChangelogIsCreatedToHoldAModelChange
// holds the ceiling a first boot creates to it.
const vectorYearFivePeak = int64(8_460_000_000)

// THE PAGES' VOLUME ARITHMETIC IS THE ENGINE'S.
//
// deployment.md and retention.md tell an operator what the state logs get from
// a volume: what they ask for together once every derived ask has reached its
// clamp, the free space above which none of them is scaled down, and what the
// vector log is created with on a 64 GiB volume. Each figure is a product of
// [StreamBudgetShare], [MinDomainCeiling], every registered domain's Tier A
// ask, the division that fits them into the pool, the broker's grant
// ([embeddedShareNum] over [embeddedShareDen]) and the object bucket's
// reservation ([derivedObjectsReservation]), which a boot spends BEFORE it
// sizes the logs — and nothing tied the prose to any of them, which is how
// the threshold read 290 GiB where the arithmetic of its day said 388: the
// pool was taken as half the free space rather than half of three quarters of
// it, and on a broker that also reserves the bucket's quarter it is less
// again.
//
// So each is COMPUTED here from the code that sizes the ceilings and looked
// for in the pages that state it, with the whitespace of a wrapped paragraph
// folded away. A clamp moved, the share changed, the bucket's derivation moved
// or a domain registered fails here, naming the figure the pages must now say.
// What a fresh node needs at the least is [BrokerFloor]'s, which holds the
// deployment guide's own table, so it is not stated a second time here.
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
	// THE POOL A FIRST BOOT DIVIDES, nothing set: the broker's limit is
	// three quarters of the volume, the nats object bucket's derived
	// reservation is spent from it first, and the logs share half of what
	// that leaves; with the objects in S3 no bucket is reserved.
	pool := func(free int64, bucket bool) int64 {
		limit := free / embeddedShareDen * embeddedShareNum
		if bucket {
			limit -= derivedObjectsReservation(limit, 0)
		}
		return int64(float64(limit) * StreamBudgetShare)
	}
	created := func(free int64) map[string]domainCeiling {
		return divide(asks(free), pool(free, true))
	}
	sum := func(ceilings map[string]domainCeiling) int64 {
		var total int64
		for _, ceiling := range ceilings {
			total += ceiling.Bytes
		}
		return total
	}

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

	// THE THRESHOLD: the least whole number of GiB free whose pool holds the
	// full ask — searched, because the bucket's reservation has regimes of
	// its own and an inverse written here would restate them.
	threshold := func(bucket bool) int64 {
		free := int64(1)
		for pool(free*gib, bucket) < full {
			free++
		}
		return free
	}
	withBucket, inS3 := threshold(true), threshold(false)
	shareWords := fractionWords(t, float64(pool(withBucket*gib, true))/float64(withBucket*gib))
	s3Words := fractionWords(t, float64(pool(inS3*gib, false))/float64(inS3*gib))

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
	peak := float64(vectorYearFivePeak)
	headroom := func(ceiling int64) int {
		return int(math.Round(100 * (1 - peak/float64(ceiling))))
	}
	// WHERE THE CREATED LOG HOLDS THE EXCURSION CLEAR OF THE ALARM: the
	// re-embedding's peak is the corpus's, a fixed number of bytes, while
	// the ceiling a volume creates the log with shrinks with the volume, so
	// a small enough volume creates a log the excursion fills. The page says
	// from what free space the created log holds it and, below that, the
	// ceiling to set — rather than a headroom that is negative. And it says
	// which of two failures that is: a log created over the peak but under
	// a tenth of headroom raises the alarm and holds the change, while one
	// created under the peak cannot hold the change at all, and calling that
	// an alarm told an operator the excursion would only be noticed.
	clears := func(ceiling int64) bool {
		return float64(ceiling)*(1-statelog.HeadroomAlarmFraction) >= peak
	}
	clearFrom := int64(1)
	for !clears(created(clearFrom * gib)[vectors].Bytes) {
		clearFrom++
	}
	needed := int64(math.Ceil(peak / (1 - statelog.HeadroomAlarmFraction) / float64(gib)))
	createdAt := createdWords(t, vectorCreated)
	createdPhrase := fmt.Sprintf("about %d %% at the %s it is created with",
		headroom(vectorCreated), createdAt)
	switch {
	case float64(vectorCreated) < peak:
		createdPhrase = fmt.Sprintf("a %s volume creates the log at %s, too small "+
			"to hold that re-embedding at all: below about %d GiB free, set "+
			"`stream.tracker_vectors_max_bytes` to at least %d GiB",
			gibWords(t, volume), createdAt, clearFrom, needed)
	case !clears(vectorCreated):
		createdPhrase = fmt.Sprintf("a %s volume creates the log at %s, which that "+
			"re-embedding fills past the alarm: below about %d GiB free, set "+
			"`stream.tracker_vectors_max_bytes` to at least %d GiB",
			gibWords(t, volume), createdAt, clearFrom, needed)
	}

	for page, want := range map[string][]string{
		"docs/guides/deployment.md": {
			shareWords + " of the free space",
			fmt.Sprintf("less than about %d GiB free", withBucket),
			// THE SUM AND ITS ADDENDS, which the page gives in place of the
			// share of the threshold: the threshold and the share are each
			// held on their own above and below.
			fmt.Sprintf("the %s ask for %s:", count, fullAsk),
			fmt.Sprintf("%s of the free space, so from about %d GiB", s3Words, inS3),
		},
		"docs/guides/retention.md": {
			fmt.Sprintf("about %d %% at the %s a %s volume asks for",
				headroom(vectorAsk), gibWords(t, vectorAsk),
				gibWords(t, volume)),
			createdPhrase,
			fmt.Sprintf("%d %% at the %s a volume of about %d GiB or more gets",
				headroom(vectorClamp), gibWords(t, vectorClamp), withBucket),
		},
	} {
		text := foldedPage(t, page)
		for _, phrase := range want {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s does not say %q. The engine sizes the %s logs into "+
					"%s of the free space (%s with the objects in S3): they ask "+
					"for %s at their clamps, so nothing is scaled from about %d "+
					"GiB free (%d in S3); and a %s volume creates the vector log "+
					"at %s of the %s it asks for",
					page, phrase, count, shareWords, s3Words, fullAsk, withBucket,
					inS3, gibWords(t, volume), createdAt,
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
// fails rather than being rounded, because a page would have to round it too:
// an ask is a quarter or a sixteenth of a volume the pages name in whole GiB,
// a clamp or a fixed default, so a finer one is a figure no page has decided
// how to state. A ceiling the division creates is another kind, and
// [createdWords] states it.
func gibWords(t *testing.T, bytes int64) string {
	t.Helper()
	switch {
	case bytes%gib == 0:
		return fmt.Sprintf("%d GiB", bytes/gib)
	case bytes%(gib/2) == 0:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gib))
	case (3*bytes+3)%gib < 6:
		// A WHOLE NUMBER OF THIRDS, to the byte an ask of two thirds of a
		// clamp is floored to: the code log asks for two thirds of the
		// mutation log's, so every sum it is in ends in one, which the pages
		// write as the vulgar fraction [gibText] spells (228⅔).
		return gibText(bytes) + " GiB"
	}
	t.Fatalf("%d bytes is no whole or half GiB; teach gibWords and the pages "+
		"the figure the arithmetic now leaves", bytes)
	return ""
}

// createdWords renders a ceiling the division created, as the pages write it:
// as [gibWords] does where it is a whole number of half GiB, and otherwise to
// the nearest tenth of one.
//
// ROUNDED, where an ask is not, because a created ceiling is a share of the
// pool rather than a number anybody chose. Every scaled log gets the pool left
// beside the fixed ones in proportion to its ask, and the Dashboard
// Assistant's log is one of the scaled ones, down to its own floor
// ([domainCeiling.ScalesTo]). So on any volume where the logs are scaled down
// the share is no whole number of half GiB — the vector log's on a 64 GiB
// volume is its 16 GiB ask times that pool over a sum the assistant's half
// gibibyte is part of — and a page states it to a tenth.
func createdWords(t *testing.T, bytes int64) string {
	t.Helper()
	if bytes%(gib/2) == 0 {
		return gibWords(t, bytes)
	}
	return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gib))
}

// fractionWords is the share of the free space the logs are sized into, as the
// pages say it.
func fractionWords(t *testing.T, share float64) string {
	t.Helper()
	// FIVE EIGHTHS OF WHAT THE BROKER LEAVES, the logs' share since the code
	// log joined them ([StreamBudgetShare]): of three quarters less the
	// bucket's quarter that is 45/128 of the free space, and of three
	// quarters 15/32, which the pages give as a percentage, since nobody
	// reads a hundred-and-twenty-eighth.
	switch share {
	case 0.3515625:
		return "about 35 %"
	case 0.46875:
		return "about 47 %"
	}
	t.Fatalf("the logs are sized into %v of the free space, which the pages "+
		"have no words for; say it there and here", share)
	return ""
}

// numberWord is how the pages count the state logs.
func numberWord(t *testing.T, n int) string {
	t.Helper()
	words := []string{"zero", "one", "two", "three", "four", "five", "six",
		"seven", "eight", "nine", "ten", "eleven", "twelve"}
	if n < 0 || n >= len(words) {
		t.Fatalf("%d state logs: numberWord cannot say it", n)
	}
	return words[n]
}
