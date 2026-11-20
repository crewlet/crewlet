package redact_test

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// liveRun is what a stream wrote for a text and answered, at each flush how
// much it had written and how much of that it answered final, and how many
// windows its keys' stage decided early.
type liveRun struct {
	out     string
	err     error
	flushes [][2]int64
	early   int
}

// readLive is text written to a stream holding h, live where flush is not
// nil, in pieces of 1 to most bytes drawn from r, flushed after each write
// where flush says, and once more after Close.
func readLive(text string, h int, r *rand.Rand, most int, flush func() bool) liveRun {
	var run liveRun
	var out bytes.Buffer
	s := redact.NewStreamHolding(&out, h, flush != nil)
	redact.CountEarly(s, &run.early)
	for at := 0; at < len(text); {
		n := min(len(text)-at, 1+r.IntN(most))
		_, _ = s.Write([]byte(text[at : at+n]))
		at += n
		if flush != nil && flush() {
			final, _, _ := s.Flush()
			run.flushes = append(run.flushes, [2]int64{int64(out.Len()), final})
		}
	}
	run.err = s.Close()
	final, _, _ := s.Flush()
	run.flushes = append(run.flushes, [2]int64{int64(out.Len()), final})
	run.out = out.String()
	return run
}

// liveText is a text a live stream is read over, the hold it is read at, and
// whether it is built in a layout that can refuse at that hold
// ([redact.ErrDecidedEarly]): a run about as long as the hold beside a key's
// block and a claim decided past it.
type liveText struct {
	name, text string
	hold       int
	canRefuse  bool
}

// liveTexts is a text of each kind the package's tests draw, at each length a
// live stream reads apart: a transcript of every shape the keys' reader tells
// apart, below wholeBytes, across it and past it; the keys in every wrapping;
// every shape a stage carries across its window; and lines too long to hold
// among them, all shorter than the hold they are read at. Then texts a stream
// decides early: a shape of each hold at 256 KiB, an armour beside a run, an
// AWS secret's blanks or a Google token's dots in no layout that refuses there;
// and at the stream's own hold, after 600 KiB of lines, each layout the package
// doc names, and its neighbours, beside a run past the lag.
func liveTexts(r *rand.Rand) (texts []liveText) {
	add := func(name, text string, h int, canRefuse bool) {
		texts = append(texts, liveText{name, text, h, canRefuse})
	}
	transcript := func(size int) string {
		var b strings.Builder
		for b.Len() < size {
			b.WriteString(transcriptLines[r.IntN(len(transcriptLines))])
			b.WriteByte('\n')
		}
		return b.String()
	}
	for _, size := range []int{redact.WholeBytes / 3, redact.WholeBytes - 100, redact.WholeBytes + 1000, 3 * redact.WholeBytes} {
		text := transcript(size)
		add(fmt.Sprintf("a transcript of %d bytes", len(text)), text, redact.HoldBytes(), false)
	}
	// A line too long to hold (a base64 dump; password's keys, the last
	// taking the line after it as its value) between transcripts.
	b64 := strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9Q", 3<<14)
	add("a base64 line of 3 MiB in a transcript", transcript(redact.WholeBytes)+"data: "+b64+"\n"+transcript(512<<10), redact.HoldBytes(), false)
	keys := "x " + strings.Repeat("password: ", 209715) + "\nhunter2secretvalue9\n"
	add("a line of password's keys in a transcript", transcript(redact.WholeBytes)+keys+transcript(512<<10), redact.HoldBytes(), false)
	for name, text := range streamCorpus(r) {
		add("the keys of "+name+" in every wrapping", text, redact.HoldBytes(), false)
	}
	add("the stages' shapes", redact.StreamText(r, 4*redact.WholeBytes), redact.HoldBytes(), false)
	// A shape of each hold, every one a stream refuses at this hold, and
	// three that [TestAStreamThatDecidesEarlySaysSo] holds it never does.
	each := []string{"a1", "a3", "a5", "a6", "b1", "c1", "d1", "e1", "g1", "g2", "gd1", "hc5", "p1", "pa3", "pb1", "pc4"}
	for _, s := range redact.HoldShapes(768 << 10) {
		if slices.Contains(each, s[0]) {
			add("hold shape "+s[0], s[1], 256<<10, !slices.Contains([]string{"a1", "b1", "gd1"}, s[0]))
		}
	}
	add("an early text", redact.EarlyText(r, 1<<20, 512<<10), 256<<10, true)
	layouts, named := redact.RefusingLayouts(r)
	lines := strings.Repeat("ok\n", 200<<10)
	for i, l := range layouts {
		add(l[0]+", after 600 KiB of lines", lines+l[1], redact.HoldBytes(), named[i])
	}
	return texts
}

// A LIVE STREAM WRITES WHAT THE WHOLE TEXT REDACTS TO HOWEVER IT IS FLUSHED:
// over [liveTexts], cut and flushed at random, where Close answers nil it
// wrote [redact.Secrets], and a flush then answers all of it final; what any
// flush answered final is a prefix of Secrets; a text in no layout that can
// refuse, flushed around windows decided early, is never refused; never
// flushed, it answers as a [redact.NewStream] does. In a layout that can, a
// flush may spare, make or move a refusal ([redact.Stream.Flush]).
//
// Mutation: decide each stage to what it was handed, settled or not, or have
// the end keep what the head's flushes wrote, and a text is written otherwise
// than Secrets; refuse at a flush while a window decided early waits on its
// claims, and a text in no layout that can refuse is refused.
func TestALiveStreamWritesWhatTheWholeTextRedactsToHoweverItIsFlushed(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 2))
	texts := liveTexts(r)
	schedules := []struct {
		name string
		most int
		odds int
	}{
		{"never flushed", 64 << 10, 0},
		{"lines' worth, flushed after each", 200, 1},
		{"up to 8 KiB, flushed 1 in 4", 8 << 10, 4},
		{"up to 96 KiB, flushed 1 in 2", 96 << 10, 2},
	}
	refused, differ, early := 0, 0, 0
	for _, tx := range texts {
		want := redact.Secrets(tx.text)
		plain := readLive(tx.text, tx.hold, r, 64<<10, nil)
		if plain.err != nil {
			refused++
		}
		for _, sc := range schedules {
			if sc.most < 1<<10 && len(tx.text) > 3*redact.WholeBytes {
				continue
			}
			got := readLive(tx.text, tx.hold, r, sc.most, func() bool { return sc.odds > 0 && r.IntN(sc.odds) == 0 })
			name := tx.name + ", " + sc.name
			at, plainAt := redact.RefusedAt(got.err), redact.RefusedAt(plain.err)
			if at != plainAt {
				differ++
			}
			if sc.odds > 0 && !tx.canRefuse && got.early > 0 {
				early++
			}
			last := got.flushes[len(got.flushes)-1]
			switch {
			case got.err != nil && !tx.canRefuse:
				t.Errorf("%s: a text in no layout that can refuse was refused: %v", name, got.err)
			case sc.odds == 0 && (at != plainAt || plain.err == nil && got.out != plain.out):
				t.Errorf("%s: never flushed, the live stream answered %v and wrote another text, or refused otherwise, than a stream, which answered %v", name, got.err, plain.err)
			case got.err == nil && got.out != want:
				t.Errorf("%s: Close answered nil, and the live stream wrote another text than Secrets %s", name, streamParted(got.out, want))
			case got.err == nil && last[1] != int64(len(got.out)):
				t.Errorf("%s: Close answered nil, and a flush after it answered %d of the %d bytes it wrote final", name, last[1], len(got.out))
			}
			for _, f := range got.flushes {
				if f[1] > f[0] || got.out[:f[1]] != want[:min(int(f[1]), len(want))] {
					t.Errorf("%s: a flush answered %d of %d bytes final, which are not Secrets' %s", name, f[1], f[0], streamParted(got.out[:min(f[1], int64(len(got.out)))], want))
					break
				}
			}
		}
	}
	if early == 0 {
		t.Errorf("no flushed reading of a text in no layout that can refuse decided a window early, so none of them could be refused")
	}
	t.Logf("%d texts, %d refused by a stream; %d flushed live readings refused otherwise than it; %d flushed readings of a text in no layout that can refuse decided a window early", len(texts), refused, differ, early)
}

// BenchmarkALiveStream is what a live stream costs against a [redact.Stream]
// over the same text, flushed every 64 KiB, 1 MiB or never: a plain log, a
// mix of every shape a stage carries, password lines, and lines dense with
// tokens. Its MB/s is the text's, and B/op what it allocates.
func BenchmarkALiveStream(b *testing.B) {
	r := rand.New(rand.NewPCG(1666, 9))
	const size = 8 << 20
	repeat := func(line string) string { return strings.Repeat(line, size/len(line)+1)[:size] }
	corpora := []struct{ name, text string }{
		{"plain log", repeat("2026-11-19T10:00:00Z INFO build step 12 of 40 passed in 0.412s\n")},
		{"mixed", redact.StreamText(r, size)},
		{"passwords", repeat("export DB_PASSWORD=hunter2x9 # rotate the password: monthly\n")},
		{"token-dense", repeat("Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJlLXZhbHVl sk-" + strings.Repeat("Ab3Cd4", 8) + "\n")},
	}
	for _, c := range corpora {
		for _, every := range []int{0, 64 << 10, 1 << 20, -1} {
			name := c.name + "/plain"
			switch {
			case every == -1:
				name = c.name + "/live, never flushed"
			case every > 0:
				name = fmt.Sprintf("%s/live, flushed every %d KiB", c.name, every>>10)
			}
			text := []byte(c.text)
			b.Run(name, func(b *testing.B) {
				b.SetBytes(int64(len(text)))
				b.ReportAllocs()
				for b.Loop() {
					s := redact.NewStream(io.Discard)
					if every != 0 {
						s = redact.NewLiveStream(io.Discard)
					}
					for at := 0; at < len(text); at += 16 << 10 {
						_, _ = s.Write(text[at:min(len(text), at+16<<10)])
						if every > 0 && (at+16<<10)%every == 0 {
							_, _, _ = s.Flush()
						}
					}
					if err := s.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// A LIVE STREAM SETTLES PAST A LINE OF ANY LENGTH AS THE WHOLE TEXT DOES: a
// line longer than any window (1.1 MiB, ending in a key that awaits its
// separator, a key pending, or a quoted key), then a line of 300 KiB of
// keys whose pairing that line's end decides, then a line that is the last
// key's value or not, at both parities: written in 64 KiB pieces and flushed
// at every line end, Close answers nil and the stream wrote [redact.Secrets].
//
// Mutation: past 1 MiB unsettled, hand the stages the text and settle again
// from the line's end once four reaches settle there, and the stream writes
// another text than Secrets.
func TestALiveStreamSettlesPastALineOfAnyLengthAsTheWholeTextDoes(t *testing.T) {
	t.Parallel()
	filler := "filler " + strings.Repeat("x", 1100<<10) + " "
	for _, sh := range [][3]string{
		{"passwd ", ": ", "password: "}, {"passwd ", ": ", `"password": "`}, {"passwd ", ": ", "pwd: "},
		{"password: ", "", "password: "}, {`"password": "`, "", `"password": "`},
	} {
		for _, k := range []int{30000, 30001} {
			text := filler + sh[0] + "\n" + sh[1] + strings.Repeat(sh[2], k) + "\nhunter2secretvalue9\n" + strings.Repeat("ok\n", 20)
			var out bytes.Buffer
			s := redact.NewLiveStream(&out)
			for at := 0; at < len(text); {
				n := min(len(text)-at, 64<<10)
				if i := strings.IndexByte(text[at:at+n], '\n'); i >= 0 {
					n = i + 1
				}
				_, _ = s.Write([]byte(text[at : at+n]))
				if at += n; text[at-1] == '\n' {
					_, _, _ = s.Flush()
				}
			}
			if err := s.Close(); err != nil || out.String() != redact.Secrets(text) {
				t.Errorf("a line ending %q, then %q and %q x %d: Close answered %v, and the stream wrote another text than Secrets %s", sh[0], sh[1], sh[2], k, err, streamParted(out.String(), redact.Secrets(text)))
			}
		}
	}
}

// A LIVE STREAM BELOW WHOLEBYTES ASKS ONLY ONCE A LINE HAS ENDED: a text
// shorter than [redact.WholeBytes] whose lines Settled holds back (an armour,
// then password's keys), flushed, then flushed after each of 50 one-byte
// writes with no line break, asks Settled's readers no more, and once more
// when a line ends; Close then answers nil and it wrote [redact.Secrets].
//
// Mutation: read the held text with a fresh settling at each flush, and
// every flush asks again.
func TestALiveStreamBelowWholeBytesAsksOnlyOnceALineHasEnded(t *testing.T) {
	t.Parallel()
	text := "k: -----BEGIN RSA PRIVATE KEY-----\n" + keyLine + "\n\n" + strings.Repeat("pwd:\n", 3000)
	if len(text)+100 > redact.WholeBytes {
		t.Fatalf("the text runs %d bytes, not below %d", len(text), redact.WholeBytes)
	}
	var out bytes.Buffer
	s := redact.NewLiveStream(&out)
	_, _ = s.Write([]byte(text))
	if _, held, _ := s.Flush(); held < len(text)/2 {
		t.Fatalf("Settled held %d of %d bytes, so a flush has little to ask of", held, len(text))
	}
	asked := redact.Asks(s)
	for range 50 {
		_, _ = s.Write([]byte("x"))
		_, _, _ = s.Flush()
	}
	if n := redact.Asks(s) - asked; n != 0 {
		t.Errorf("50 flushes with no line break since the last asked %d more times", n)
	}
	_, _ = s.Write([]byte("\nok\n"))
	_, _, _ = s.Flush()
	if n := redact.Asks(s) - asked; n != 1 {
		t.Errorf("a flush after a line ended asked %d more times, not once", n)
	}
	text += strings.Repeat("x", 50) + "\nok\n"
	if err := s.Close(); err != nil || out.String() != redact.Secrets(text) {
		t.Errorf("Close answered %v, and the stream wrote another text than Secrets %s", err, streamParted(out.String(), redact.Secrets(text)))
	}
}
