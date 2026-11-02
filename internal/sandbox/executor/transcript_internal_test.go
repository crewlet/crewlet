package executor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
)

// A LINE OF MANY NOTE OPENINGS COSTS WHAT ITS BYTES COST, however long the
// line: finding a captured session's notes in a quarter of a mebibyte of
// openings written as one line costs what it does in the same openings on
// lines of four KiB — openings that are no heading, and openings on a line
// that closes as a heading does. Every opening used to look for its line's end
// again, and every opening on a line that closed as a heading was a note of
// its own whose next was looked for the same way: a line of a record runs to
// [maxLineBytes], and one written as nothing but openings cost the openings
// times the line — tens of minutes of a core, past every bound a read has and
// every check of its caller, for one line a box's own agent can write.
//
// A COMPARISON, NOT A CLOCK, timed as internal/redact's linear cases are
// ([withinFactor]): read linearly the two cost the same; read an opening at a
// time to its line's end, the one line costs fifty to a hundred times the
// short ones. Four times is allowed.
//
// Mutation: look for the line's end from every opening, or for the next
// heading from the opening after the one found rather than from its line's
// end, and the one line costs fifty times the short ones or more.
func TestALineOfManyNoteOpeningsCostsWhatItsBytesCost(t *testing.T) {
	t.Parallel()
	lines := func(lineBytes int, closing string) string {
		var b strings.Builder
		for b.Len() < 256<<10 {
			for start := b.Len(); b.Len()-start < lineBytes; {
				b.WriteString(noteHeadOpen)
			}
			b.WriteString(closing)
			b.WriteByte('\n')
		}
		return b.String()
	}
	read := func(text string) {
		for range notesIn(text) {
		}
	}
	for name, closing := range map[string]string{
		"openings that are no heading":            "",
		"openings on a line closing as a heading": noteHeadClose,
	} {
		short, long := lines(4<<10, closing), lines(256<<10, closing)
		if ok, shortCost, longCost := withinFactor(read, short, long, 4); !ok {
			t.Errorf("%s: %v on one line of 256 KiB against %v on lines of 4 KiB — an opening "+
				"looks for its line's end again", name, longCost, shortCost)
		}
	}
}

// costFloor is the shortest a timed sample of [withinFactor] is: the noise a
// loaded runner adds to a sample is a preemption's overshoot, milliseconds
// whatever the sample, so a sample of a millisecond or two is mostly noise.
const costFloor = 20 * time.Millisecond

// withinFactor reports whether read costs at most factor times as much over
// long as over short — the same bytes laid out otherwise — and what each cost:
// the fastest of five samples of each, taken in turn after a warm-up read of
// each, so a runner busy for a moment slows both or neither. Each sample is as
// many reads as it takes the short text's to reach twice [costFloor]. A
// comparison that fails is measured again, three times in all, and fails only
// if every measurement does: that only ever turns a failure into a pass, so
// what this guards has to cost far more than the factor every time, as a read
// that is quadratic in a line does. A long sample past the factor times the
// calibrated short one fails its round at once, so a regression's cost — its
// samples are the costliest thing here — is paid once a round rather than five
// times.
func withinFactor(read func(string), short, long string, factor int) (bool, time.Duration, time.Duration) {
	timed := func(text string, reps int) time.Duration {
		start := time.Now()
		for range reps {
			read(text)
		}
		return time.Since(start)
	}
	reps, calibrated := 1, timed(short, 1)
	for reps < 1<<20 && calibrated < 2*costFloor {
		reps *= 2
		calibrated = timed(short, reps)
	}
	timed(short, 1)
	timed(long, 1)
	var shortCost, longCost time.Duration
	for range 3 {
		shortCost, longCost = time.Duration(1<<62), time.Duration(1<<62)
		for range 5 {
			shortCost = min(shortCost, timed(short, reps))
			sample := timed(long, reps)
			longCost = min(longCost, sample)
			if sample > time.Duration(factor)*calibrated {
				break
			}
		}
		if longCost <= time.Duration(factor)*shortCost {
			return true, shortCost, longCost
		}
	}
	return false, shortCost, longCost
}

// keyLine is one line of a private key's body at PEM's width, as a harness's
// record quotes a key a run read.
const keyLine = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"

// logLines is lines of a build's log from text's end until it is n bytes long.
func logLines(text string, n int) string {
	var b strings.Builder
	b.WriteString(text)
	for b.Len() < n {
		b.WriteString("a line of the build's log\n")
	}
	return b.String()
}

// AN ENTRY'S TEXT IS AN OPENING OF WHAT ITS WHOLE TEXT REDACTS TO, whatever the
// text holds and wherever the bound falls in it: never a byte the whole text's
// redaction would not hand out, and cut, marked, within the bound.
//
//   - Key blocks ahead of a token the window cuts. Twenty-one private keys,
//     each a marker once redacted, and a GitHub token straddling where a window
//     of the bound and a key block past it ends: the window redacted to less
//     than the bound, and the token it had cut came through as its first
//     twenty characters, which no rule matches.
//   - A token across the bound, at every offset: begun inside the bound, it is
//     read whole and redacted as one, never cut first.
//   - A key whose body is read back from its END, which is many KiB past the
//     bound: a line of prose straight after its BEGIN ends what a read forward
//     from the armour takes, so only the END reaches the body inside the bound.
//
// Mutation: cut what the window redacts by its bytes alone, and the token's
// opening is handed out; cut the text before redacting it, and the token
// across the bound is; read a window short of a key block past the bound, and
// the body the END reads back is.
func TestAnEntryIsAnOpeningOfWhatItsWholeTextRedactsTo(t *testing.T) {
	t.Parallel()
	key := "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat(keyLine+"\n", 50) +
		"-----END RSA PRIVATE KEY-----\n"
	token := "ghp_" + strings.Repeat("Qx7Rb2", 6)
	type entry struct {
		text  string
		bound int
	}
	cases := map[string]entry{}

	keys := strings.Repeat(key, 21)
	at := TranscriptEntryMaxBytes + redactContext - 20
	before := logLines(keys, at-len("a line of the build's log\n"))
	before += strings.Repeat(" ", at-len(before))
	cases["key blocks ahead of a token the window cuts"] = entry{
		logLines(before+token+"\n", 160<<10), TranscriptEntryMaxBytes}

	for offset := 64 - len(token) + 1; offset < 64; offset += 3 {
		cases[fmt.Sprintf("a token across the bound at %d", offset)] = entry{
			logLines(strings.Repeat("x", offset)+token+"\n", 200<<10), 64}
	}

	cases["a key whose body is read back from its END past the bound"] = entry{
		logLines("-----BEGIN RSA PRIVATE KEY-----\nsee below\n"+strings.Repeat(keyLine+"\n", 200)+
			"-----END RSA PRIVATE KEY-----\n", 200<<10), 1 << 10}

	for name, c := range cases {
		got, cut := redactedWithin(c.text, c.bound)
		kept := strings.TrimSuffix(got, "…")
		if whole := redact.Secrets(c.text); !strings.HasPrefix(whole, kept) {
			t.Errorf("%s: the entry hands out %q, which the whole text does not redact to", name,
				kept[max(0, len(kept)-80):])
		}
		if !cut || len(got) > c.bound || !strings.HasSuffix(got, "…") {
			t.Errorf("%s: the entry is %d bytes, cut %v, ending %q; want it cut, marked, within %d",
				name, len(got), cut, got[max(0, len(got)-8):], c.bound)
		}
		if strings.Contains(got, "ghp_") || strings.Contains(got, keyLine[:16]) {
			t.Errorf("%s: the entry hands out a credential's material: …%q", name, got[max(0, len(got)-80):])
		}
	}
}
