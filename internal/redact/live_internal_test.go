package redact

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
)

// allocated is how many bytes f allocates.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// SETTLED COPIES NO TEXT IT READS: over 4 MiB of a plain log, [Settled]
// allocates within a quarter of the text of what its readers ([settle])
// allocate reading it, so a caller holding the text pays for no copy of it.
// Not parallel: a parallel test's allocations would count as its own.
//
// Mutation: hand the settling a copy of the text, and Settled allocates the
// text's length more.
func TestSettledCopiesNoTextItReads(t *testing.T) {
	line := "2026-11-19T10:00:00Z INFO build step 12 of 40 passed in 0.412s\n"
	text := strings.Repeat(line, 4<<20/len(line))
	readers := allocated(func() { _, _ = settle(text) })
	got := allocated(func() { _ = Settled(text) })
	if got > readers+uint64(len(text)/4) {
		t.Errorf("Settled over %d bytes allocated %d bytes, against %d its readers allocate", len(text), got, readers)
	}
}

// NewStreamHolding is a Stream whose keys' stage decides a window once the
// text has run h past it ([newStreamHolding]), a live one where asked.
func NewStreamHolding(w io.Writer, h int, liveStream bool) *Stream {
	s := newStreamHolding(w, h)
	if liveStream {
		s.live = &live{}
	}
	return s
}

// HeldToTheLoop is s with every ask it makes of Settled's readers, where it
// is live, held to the loop ([SettledTwoWays]) from one reading of the holds;
// the first where they differ fails t. A stream that is not live asks none.
func HeldToTheLoop(t testing.TB, s *Stream) *Stream {
	if s.live == nil {
		return s
	}
	differed := false
	s.live.view.ask = func(text string) int {
		swept, looped := SettledTwoWays(text)
		if swept != looped && !differed {
			differed = true
			t.Errorf("a live stream's ask of %d bytes: Settled settled %d, and a hold a pass %d; its end:\n%q", len(text), swept, looped, text[max(0, len(text)-2000):])
		}
		return swept
	}
	return s
}

// Settling is a [settling] for the package's external tests: Settle answers
// how much more of the text written is settled; Held is what it holds, Kept
// the buffer it keeps, and Asks how many times it asked Settled's readers.
type Settling struct{ s growing }

func (s *Settling) Write(p []byte) { s.s.write(p) }

func (s *Settling) Settle() (n int) {
	s.s.settle(func(b []byte) { n = len(b) })
	return n
}

func (s *Settling) Held() int { return len(s.s.text) }

func (s *Settling) Kept() int { return cap(s.s.text) }

func (s *Settling) Asks() int { return s.s.asks }

// SettlingKeeps is [settlingKeeps].
const SettlingKeeps = settlingKeeps

// Asks is how many times a live stream asked Settled's readers.
func Asks(s *Stream) int { return s.live.view.asks + s.live.raw.asks }

// CountEarly counts in n each window s's keys' stage decides early.
func CountEarly(s *Stream, n *int) {
	s.observe = &keyObserver{decided: func(int, int) { *n++ }}
}

// RefusingLayouts is [refusingLayouts]: each its name, its text, and whether
// it is in a layout the package doc names.
func RefusingLayouts(r *rand.Rand) (layouts [][2]string, named []bool) {
	for _, l := range refusingLayouts(r) {
		layouts, named = append(layouts, [2]string{l.name, l.text}), append(named, l.refuses)
	}
	return layouts, named
}

// RefusedAt is the byte of the text a refusal names, or -1 for any other
// answer.
func RefusedAt(err error) int {
	var e *decidedEarly
	if errors.As(err, &e) {
		return e.at
	}
	return -1
}

// HoldShapes is every hold shape ([holdShapes]) at n, its name's first word
// and its text.
func HoldShapes(n int) [][2]string {
	var out [][2]string
	for _, s := range holdShapes(n) {
		out = append(out, [2]string{s.name[:strings.IndexByte(s.name, ' ')], s.text()})
	}
	return out
}

// EarlyText is [earlyText].
var EarlyText = earlyText

// HoldBytes is [holdBytes], WholeBytes [wholeBytes].
var HoldBytes, WholeBytes = holdBytes, wholeBytes

// settledFrom is how far the text is settled from at, chained as a live
// stream chains [Settled] ([Stream.settle]), each ask held to the loop
// ([SettledByBoth]).
func settledFrom(t testing.TB, text string, at int) int {
	return at + SettledByBoth(t, text[at:])
}

// lined is text with a line break after every 2 KiB that runs without one,
// so a flush has lines to settle.
func lined(text string) string {
	var b strings.Builder
	run := 0
	for i := range len(text) {
		b.WriteByte(text[i])
		if run++; text[i] == '\n' {
			run = 0
		} else if run == 2<<10 {
			b.WriteByte('\n')
			run = 0
		}
	}
	return b.String()
}

// A FLUSH WRITES THE REDACTION AS FAR AS THE TEXT IS SETTLED: after each
// flush the stream has written exactly Secrets of the text up to the settled
// point, all of it final, and answers the rest of the text as held. Written in
// lines and flushed after each, below [wholeBytes], across it and past it,
// where the first flush past it also hands the stages what the head settled.
//
// Each claims source is known to the settled point and no further.
//
// Mutation: make Flush decide nothing ([pipeline.decideHeld]), and the stream
// writes less than the settled point holds; drop the head's flush
// ([Stream.Flush]), and a short text shows none of its settled part; leave a
// claims source known to the end ([ruleStage.decideHeld]), and it reads so.
func TestAFlushWritesTheRedactionAsFarAsTheTextIsSettled(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 1))
	texts := map[string]string{
		"below wholeBytes":  lined(streamText(r, wholeBytes/3)),
		"across wholeBytes": lined(streamText(r, wholeBytes+3*pieceBytes)),
		"past wholeBytes":   lined(streamText(r, 5*wholeBytes)),
		"transcript lines":  strings.Repeat("ok\n[tool 3] go test ./...\npassword: hunter2x9\n"+testBegin+"\n"+testKeyLine+"\n"+testEnd+"\n", 2*wholeBytes/170),
	}
	for name, text := range texts {
		var out bytes.Buffer
		s := HeldToTheLoop(t, NewLiveStream(&out))
		settled, flushes := 0, 0
		for at := 0; at < len(text); {
			n := strings.IndexByte(text[at:], '\n') + 1
			if n == 0 || r.IntN(3) == 0 {
				n = min(len(text)-at, 1+r.IntN(48<<10))
			}
			_, _ = s.Write([]byte(text[at : at+n]))
			at += n
			if r.IntN(4) != 0 {
				continue
			}
			final, held, err := s.Flush()
			flushes++
			settled = settledFrom(t, text[:at], settled)
			want := Secrets(text[:settled])
			switch {
			case err != nil:
				t.Fatalf("%s: a flush at %d answered %v", name, at, err)
			case out.String() != want:
				t.Fatalf("%s: a flush at %d of %d bytes settled to %d wrote another text than Secrets of it %s", name, at, len(text), settled, parted(out.String(), want))
			case final != int64(out.Len()) || held != at-settled:
				t.Fatalf("%s: a flush at %d settled to %d answered %d final and %d held, where it wrote %d and holds %d", name, at, settled, final, held, out.Len(), at-settled)
			}
			for i := 0; s.p != nil && i < len(s.p.sources); i++ {
				if f := s.p.sources[i].frontier; f > settled {
					t.Fatalf("%s: a flush at %d settled to %d left claims source %d known to %d, past where it decided", name, at, settled, i, f)
				}
			}
		}
		if err := s.Close(); err != nil || out.String() != Secrets(text) {
			t.Errorf("%s: Close answered %v, and wrote Secrets of the text: %v", name, err, out.String() == Secrets(text))
		}
		if final, held, err := s.Flush(); err != nil || final != int64(out.Len()) || held != 0 {
			t.Errorf("%s: a flush after Close answered %d final, %d held and %v, where the stream wrote %d", name, final, held, err, out.Len())
		}
		t.Logf("%s: %d bytes, %d flushes", name, len(text), flushes)
	}
}

// A TEXT SHORTER THAN wholeBytes SHOWS ITS SETTLED PART ON A FLUSH: the head
// is redacted whole, so before the stages start nothing else would write it.
// Its settled lines are written at once, and what follows them later; once
// the text passes wholeBytes the stages' first bytes, which the flush wrote
// already, are not written twice ([endSink]).
//
// Mutation: write nothing on a flush of the head ([Stream.Flush]), or have
// the end keep what it should drop, and the text shows nothing, or twice.
func TestAShortTextShowsItsSettledPartOnAFlush(t *testing.T) {
	t.Parallel()
	line := "step 3 passed\ntoken sk-" + strings.Repeat("q", 40) + "\n"
	text := strings.Repeat(line, wholeBytes/len(line)+40)
	var out bytes.Buffer
	s := HeldToTheLoop(t, NewLiveStream(&out))
	_, _ = s.Write([]byte(text[:10*len(line)+7]))
	if _, _, err := s.Flush(); err != nil || out.String() != Secrets(text[:10*len(line)]) || !strings.Contains(out.String(), Marker) {
		t.Fatalf("a flush of ten lines and a part answered %v and wrote %q, not the ten lines redacted", err, out.String())
	}
	_, _ = s.Write([]byte(text[10*len(line)+7:]))
	if err := s.Close(); err != nil || out.String() != Secrets(text) {
		t.Errorf("past wholeBytes, Close answered %v, and wrote %d bytes where Secrets writes %d %s", err, out.Len(), len(Secrets(text)), parted(out.String(), Secrets(text)))
	}
}

// A LIVE STREAM KEEPS NONE OF ITS HEAD'S BUFFER ONCE ITS STAGES RUN: a text
// of [wholeBytes] whose last line Settled holds back, flushed, then written
// past it without growing the head, keeps what is past the settled point in
// a buffer of its own, so the head's goes; Close answers nil and the stream
// wrote [Secrets]. The head itself only Write changes ([live]'s view).
//
// Mutation: keep the rest as a view of the head, and the stream holds the
// head's buffer while its stages run.
func TestALiveStreamKeepsNoneOfItsHeadsBufferOnceItsStagesRun(t *testing.T) {
	t.Parallel()
	tail := "password: "
	text := strings.Repeat("ok\n", (wholeBytes-len(tail))/3)
	text += strings.Repeat("x", wholeBytes-len(tail)-len(text)) + tail
	var out bytes.Buffer
	s := HeldToTheLoop(t, NewLiveStream(&out))
	s.head = make([]byte, 0, wholeBytes+4<<10)
	head := s.head[:cap(s.head)]
	_, _ = s.Write([]byte(text))
	if _, held, err := s.Flush(); err != nil || held < len(tail) || s.p != nil {
		t.Fatalf("a flush of the head answered %d held and %v, the stages started %v", held, err, s.p != nil)
	}
	more := "hunter2secretvalue9\nok\npassword: "
	_, _ = s.Write([]byte(more))
	if s.p == nil {
		t.Fatalf("a text of %d bytes is still held whole", len(text)+len(more))
	}
	if rest := s.live.raw.text; cap(rest) > 0 {
		for i := range head {
			if &head[i] == &rest[:1][0] {
				t.Fatalf("the text past the settled point is a view of the head at %d", i)
			}
		}
	}
	text += more
	if err := s.Close(); err != nil || out.String() != Secrets(text) {
		t.Errorf("Close answered %v, and the stream wrote another text than Secrets %s", err, parted(out.String(), Secrets(text)))
	}
}

// A FLUSH COUNTS NOTHING A WINDOW DECIDED EARLY WAITS ON: at a hold low
// enough that windows are decided early and some texts refuse, a [NewStream]
// answers as final no more than the end had written when any window still
// waiting on its claims was decided, whether it asked of a claim or not (a
// late word can change a block it let go, [keyStage.handed]); a live stream's
// flush leaves none waiting; and what either answers stays a prefix of
// Secrets, refused or not.
//
// Mutation: count everything written as final, or keep a window's write only
// where it asked of a claim ([keyStage.step]), and a flush counts past a
// window still waiting.
func TestAFlushCountsNothingAWindowDecidedEarlyWaitsOn(t *testing.T) {
	t.Parallel()
	const h = 256 << 10
	r := rand.New(rand.NewPCG(1666, 3))
	type text struct{ name, s string }
	var texts []text
	// After more lines than the stages' lag, so the end writes before a
	// window is decided early and before the refusal.
	lines := strings.Repeat("the build passed\n", (4<<20)/17)
	for _, s := range holdShapes(768 << 10) {
		if n := s.name[:strings.IndexByte(s.name, ' ')]; n == "c1" || n == "pb1" {
			texts = append(texts, text{s.name, lines + s.text()})
		}
	}
	texts = append(texts, text{"earlyText", lines + earlyText(r, 1<<20, 512<<10)})
	// The window after the one that reads a block writes it and asks of no
	// claim; the lines after it run until the claims pass the first.
	block := testBegin + "\n" + testKeyLine + "\n" + testKeyLine + "\n" + testEnd + "\n"
	texts = append(texts, text{"a key's block among plain lines", lines + block + lines})
	refused, kept := 0, 0
	for _, tx := range texts {
		want := Secrets(tx.s)
		for _, liveStream := range []bool{false, true} {
			var out bytes.Buffer
			s := HeldToTheLoop(t, NewStreamHolding(&out, h, liveStream))
			var waiting [][2]int64
			s.observe = &keyObserver{decided: func(_, hi int) { waiting = append(waiting, [2]int64{int64(hi), s.written()}) }}
			var finals []int64
			for at := 0; at < len(tx.s); {
				n := min(len(tx.s)-at, 1+r.IntN(96<<10))
				_, _ = s.Write([]byte(tx.s[at : at+n]))
				at += n
				final, _, _ := s.Flush()
				finals = append(finals, final)
				if s.p == nil {
					continue
				}
				for _, w := range waiting {
					if w[0] >= int64(s.p.keys.frontier()) && final > w[1] {
						t.Fatalf("%s (live %v): a flush answered %d bytes final, past the %d written when a window still waiting was decided", tx.name, liveStream, final, w[1])
					}
				}
			}
			err := s.Close()
			final, _, _ := s.Flush()
			finals = append(finals, final)
			got := out.String()
			if err != nil {
				refused++
				kept += len(got) - int(final)
			}
			for _, f := range finals {
				if f > int64(len(got)) || got[:f] != want[:min(int(f), len(want))] {
					t.Errorf("%s (live %v): a flush answered %d bytes final, and they are not Secrets' %s", tx.name, liveStream, f, parted(got[:min(int(f), len(got))], want))
					break
				}
			}
		}
	}
	if refused == 0 || kept == 0 {
		t.Errorf("%d readings refused and %d bytes written past what they counted final: the hold decided no window early to check", refused, kept)
	}
	t.Logf("%d readings refused, %d bytes written past their final counts", refused, kept)
}

// ALL A CLOSED STREAM WROTE IS FINAL: after Close answers nil, a flush
// answers every byte the stream wrote final, whether the head's redaction
// was written by Close, by a flush, or by the stages, live or not.
//
// Mutation: leave out of the count what the stream writes of its head itself
// ([Stream.emit]), and a short text's flush after Close answers too little.
func TestAFlushAfterCloseCountsAllTheStreamWrote(t *testing.T) {
	t.Parallel()
	short := "step 3\ntoken sk-" + strings.Repeat("q", 40) + "\nok\n"
	long := strings.Repeat(short, wholeBytes/len(short)+100)
	for _, c := range []struct {
		name       string
		text       string
		liveStream bool
		flush      bool
	}{
		{"a short text", short, false, false},
		{"a long text", long, false, false},
		{"a short text, live, never flushed", short, true, false},
		{"a short text, live, flushed", short, true, true},
		{"a long text, live, flushed", long, true, true},
	} {
		var out bytes.Buffer
		s := HeldToTheLoop(t, NewStreamHolding(&out, holdBytes(), c.liveStream))
		_, _ = s.Write([]byte(c.text[:len(c.text)/2]))
		if c.flush {
			_, _, _ = s.Flush()
		}
		_, _ = s.Write([]byte(c.text[len(c.text)/2:]))
		if err := s.Close(); err != nil || out.String() != Secrets(c.text) {
			t.Fatalf("%s: Close answered %v, and wrote Secrets of the text: %v", c.name, err, out.String() == Secrets(c.text))
		}
		if final, held, err := s.Flush(); err != nil || final != int64(out.Len()) || held != 0 {
			t.Errorf("%s: a flush after Close answered %d final, %d held and %v, where the stream wrote %d", c.name, final, held, err, out.Len())
		}
	}
}

// WINDOWS DECIDED EARLY PAST THE LIMIT KEEP THE EARLIEST WRITE: once more
// than [earlyWindows] wait on their claims, the oldest two merge, and what
// stays final is still no more than the end had written when the first of
// them was decided, however many merge into it.
//
// Mutation: keep the later write of the two merged ([keyStage.decide]), and
// the count runs past the first window still waiting.
func TestWindowsMergedPastTheLimitKeepTheEarliestWrite(t *testing.T) {
	t.Parallel()
	k := &keyStage{out: &endSink{n: 1 << 30}}
	for i := range 3 * earlyWindows {
		k.decide(earlyWrite{end: (i + 1) * stageAdvance, written: int64(1000 + i)})
		if len(k.decided) > earlyWindows {
			t.Fatalf("after %d windows decided early, %d wait, past %d", i+1, len(k.decided), earlyWindows)
		}
		if got := k.finalWritten(); got != 1000 {
			t.Fatalf("after %d windows decided early, the first written at 1000, %d is answered final", i+1, got)
		}
	}
	if last := k.decided[len(k.decided)-1]; last.written != int64(999+3*earlyWindows) {
		t.Errorf("the newest window waiting wrote %d, not its own %d", last.written, 999+3*earlyWindows)
	}
}

// A FLUSH MAY REFUSE A PASSWORD'S KEY ON THE LINE AFTER A KEY'S BEGIN, the
// run of about the lag as its value's line: a layout the package doc names.
// A flush decides every stage at the settled point, which moves the keys'
// stage's later windows, so here a stream flushed once refuses where an
// unflushed one answers nil. Either way it writes no text but Secrets.
func TestAFlushMayRefuseAPasswordsKeyOnTheLineAfterAKeysBegin(t *testing.T) {
	t.Parallel()
	h := holdBytes()
	r := rand.New(rand.NewPCG(5, 5))
	prefix := strings.Repeat("ok\n", 204800)
	text := prefix + "-----BEGIN PRIVATE KEY-----\npassword: \nsk-" + testRun(r, testB64url, 5431296) + "\n" +
		strings.Repeat("ok\n", stageStep/3+1)
	want := Secrets(text)
	read := func(flushAt int) (string, error) {
		var out bytes.Buffer
		s := HeldToTheLoop(t, NewStreamHolding(&out, h, flushAt > 0))
		for at := 0; at < len(text); {
			n := min(len(text)-at, 64<<10)
			if flushAt > at && flushAt < at+n {
				n = flushAt - at
			}
			_, _ = s.Write([]byte(text[at : at+n]))
			if at += n; at == flushAt {
				final, _, err := s.Flush()
				if f := int(final); err == nil && (f > out.Len() || f > len(want) || out.String()[:f] != want[:f]) {
					t.Errorf("the flush answered %d final, not a prefix of Secrets", final)
				}
			}
		}
		err := s.Close()
		return out.String(), err
	}
	if got, err := read(0); err != nil || got != want {
		t.Fatalf("unflushed, the stream answered %v and wrote Secrets: %v; want nil and Secrets", err, got == want)
	}
	if got, err := read(len(prefix) - stageAdvance + 600); !errors.Is(err, ErrDecidedEarly) {
		t.Errorf("flushed once before the BEGIN, the stream answered %v and wrote Secrets: %v; want it refused", err, got == want)
	}
}
