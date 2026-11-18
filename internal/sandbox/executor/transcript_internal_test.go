package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/crewlet/crewlet/internal/redact"
)

// AN OPENCODE MESSAGE READS WHAT ITS STEP MEASURED AND THE FAILURE IT ENDED
// ON, off each driven release's export (testdata/opencode, described by
// openCodeExportFixture): a step that ended without a failure carries its
// prompt's uncached input and its cache reads; the step its provider refused
// and each step the session was interrupted in carry their failure.
//
// Mutation: spell `type`, `tokens`, `input`, `cache`, `read` or `error`
// otherwise than OpenCode 2 writes it, and a step reads as one that measured
// nothing, or as one that ended without its failure.
func TestAnOpenCodeMessageReadsWhatItsStepMeasuredAndTheFailureItEndedOn(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2.0.32", "2.2.5"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "opencode", version, "export.json"))
		if err != nil {
			t.Fatalf("read the fixture: %v", err)
		}
		var export struct {
			Messages []openCodeMessage `json:"messages"`
		}
		if err = json.Unmarshal(raw, &export); err != nil {
			t.Fatalf("%s: the export does not decode: %v", version, err)
		}
		measured := 0
		failures := map[string]int{}
		for _, m := range export.Messages {
			switch {
			case m.Type != openCodeAssistant:
			case !absent(m.Error):
				var failure struct {
					Type string `json:"type"`
				}
				if err = json.Unmarshal(m.Error, &failure); err != nil {
					t.Errorf("%s: a step's failure does not decode: %v", version, err)
				}
				failures[failure.Type]++
			case m.Tokens == nil || m.Tokens.Input <= 0 || m.Tokens.Cache.Read <= 0:
				t.Errorf("%s: a step that ended without a failure reads as measuring %+v", version, m.Tokens)
			default:
				measured++
			}
		}
		if measured == 0 || failures["provider.invalid-request"] != 1 || failures["aborted"] != 2 {
			t.Errorf("%s: %d steps read as measured and the failures %v, want some measured, the one "+
				"the provider refused and the two the session was interrupted in", version, measured, failures)
		}
	}
}

// A LINE OF MANY NOTE OPENINGS COSTS WHAT ITS BYTES COST: a quarter mebibyte
// of openings on one line reads as fast as on lines of 4 KiB, whether or not
// the line closes as a heading. A line runs to [maxLineBytes] and a box's own
// agent can write one, so a read quadratic in the line costs tens of minutes
// of a core, past every bound of the read and its caller. A comparison, not a
// clock, as internal/redact's linear cases ([withinFactor]): quadratic, the
// one line costs fifty to a hundred times the short ones; four is allowed.
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
// long as over short (the same bytes laid out otherwise), and what each cost:
// the fastest of five interleaved samples after a warm-up, so a briefly busy
// runner slows both or neither; a sample is as many reads as short needs to
// reach twice [costFloor]. A failing comparison is measured again, three
// rounds in all, and fails only if every round does: that only turns failures
// into passes, so it catches only what costs far more than factor every time,
// as a read quadratic in a line does. A long sample past factor times the
// calibrated short one ends its round, so a regression's samples are paid once
// a round, not five times.
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

// AN ENTRY'S TEXT IS AN OPENING OF WHAT ITS WHOLE TEXT REDACTS TO, wherever
// the bound falls: never a byte the whole text's redaction would not hand out,
// and cut, marked, within the bound. The cases:
//
//   - key blocks that redact to markers, shrinking the window, ahead of a
//     GitHub token it cuts, whose first twenty characters match no rule;
//   - a token across the bound at every offset, redacted whole, never cut first;
//   - a key, prose after its BEGIN, its body read back by an END past the bound.
//
// Mutation: cut the window's redaction by bytes alone, and the token's opening
// is handed out; cut before redacting, and the token across the bound is; read
// a window short of a key block past the bound, and the END's body is.
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
		got, cut, _ := redactedWithin(c.text, c.bound)
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

// sharesBytes reports whether s's bytes lie inside whole's: whether keeping s
// keeps whole alive.
func sharesBytes(s, whole string) bool {
	if s == "" || whole == "" {
		return false
	}
	start := uintptr(unsafe.Pointer(unsafe.StringData(whole)))
	at := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	return at >= start && at < start+uintptr(len(whole))
}

// AN ENTRY HOLDS ONLY ITS OWN BYTES, and a read remembers no call by more than
// a name's. A short note's text, thread and tool, sliced from a decoded value
// of four MiB, come out of the shaping (an opening and a match's window alike)
// as bytes of their own: a slice kept on the page holds the whole value alive
// until the answer is sent. A call whose id or tool's name runs past a name's
// bound is not remembered for its result; a harness's call is.
//
// Mutation: hand an entry out as the redaction and the cut left it, and its
// text, thread and tool are the decoded value's own bytes; remember every
// call whatever it names, and the long id or name is held for its result.
func TestAnEntryHoldsOnlyItsOwnBytes(t *testing.T) {
	t.Parallel()
	opening := "## A note from ada, sent while this turn was running\nTarget main."
	decoded := opening + "\n" + strings.Repeat("x", 4<<20)
	b := &transcriptBuilder{bounds: TranscriptReadBounds(), entries: []TranscriptEntry{},
		calls: map[string]string{}}
	matched := &transcriptBuilder{bounds: SeatTranscriptBounds(), entries: []TranscriptEntry{},
		calls: map[string]string{}, filter: TranscriptFilter{Terms: []string{"target"}}}
	for shaping, by := range map[string]*transcriptBuilder{"an opening": b, "a match's window": matched} {
		if err := by.add(TranscriptEntry{Kind: EntryNote, Thread: decoded[3:9], Tool: decoded[10:14]},
			decoded[:len(opening)]); err != nil || len(by.entries) != 1 {
			t.Fatalf("%s: the entry was added (%v), and the page holds %d", shaping, err, len(by.entries))
		}
		e := by.entries[0]
		if e.Text != opening || e.Thread != decoded[3:9] || e.Tool != decoded[10:14] {
			t.Fatalf("%s: the entry reads %q, thread %q, tool %q", shaping, e.Text, e.Thread, e.Tool)
		}
		for name, field := range map[string]string{"text": e.Text, "thread": e.Thread, "tool": e.Tool} {
			if sharesBytes(field, decoded) {
				t.Errorf("%s: the entry's %s is the decoded value's own bytes, which it holds alive",
					shaping, name)
			}
		}
	}

	long := strings.Repeat("a", transcriptNameMaxBytes+1)
	for name, c := range map[string]struct{ id, tool string }{
		"an id past a name's bound":         {long, "Bash"},
		"a tool's name past a name's bound": {"toolu_1", long},
	} {
		b.called(c.id, c.tool)
		if got := b.answered(c.id); got != "" || len(b.calls) != 0 {
			t.Errorf("%s: the call was remembered (%d held), and its result names %.20q", name,
				len(b.calls), got)
		}
	}
	b.called("toolu_2", "Bash")
	if got := b.answered("toolu_2"); got != "Bash" {
		t.Errorf("a harness's call is answered as %q, want its tool", got)
	}
}
