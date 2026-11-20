package redact

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
)

// NewStreamHolding is a Stream whose keys' stage decides a window once the
// text has run h past it ([newStreamHolding]), a live one where asked.
func NewStreamHolding(w io.Writer, h int, liveStream bool) *Stream {
	s := newStreamHolding(w, h)
	if liveStream {
		s.live = &live{due: settleEvery}
	}
	return s
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
// stream chains [Settled] ([Stream.settle]).
func settledFrom(text string, at int) int {
	return at + Settled(text[at:])
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
		s := NewLiveStream(&out)
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
			settled = settledFrom(text[:at], settled)
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
	s := NewLiveStream(&out)
	_, _ = s.Write([]byte(text[:10*len(line)+7]))
	if _, _, err := s.Flush(); err != nil || out.String() != Secrets(text[:10*len(line)]) || !strings.Contains(out.String(), Marker) {
		t.Fatalf("a flush of ten lines and a part answered %v and wrote %q, not the ten lines redacted", err, out.String())
	}
	_, _ = s.Write([]byte(text[10*len(line)+7:]))
	if err := s.Close(); err != nil || out.String() != Secrets(text) {
		t.Errorf("past wholeBytes, Close answered %v, and wrote %d bytes where Secrets writes %d %s", err, out.Len(), len(Secrets(text)), parted(out.String(), Secrets(text)))
	}
}

// A FLUSH COUNTS NOTHING A WINDOW DECIDED EARLY WAITS ON: at a hold low
// enough that windows are decided early and some texts refuse, a [NewStream]
// and a live stream settling no more ([unsettledBytes]) answer as final no
// more than the end had written when any window still waiting on its claims
// was decided, whether it asked of a claim or not (a late word can change a
// block it let go, [keyStage.handed]); and what they answer stays a prefix of
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
			s := NewStreamHolding(&out, h, liveStream)
			var waiting [][2]int64
			s.observe = &keyObserver{decided: func(_, hi int) { waiting = append(waiting, [2]int64{int64(hi), s.written()}) }}
			var finals []int64
			for at := 0; at < len(tx.s); {
				n := min(len(tx.s)-at, 1+r.IntN(96<<10))
				_, _ = s.Write([]byte(tx.s[at : at+n]))
				at += n
				if l := s.live; l != nil && s.p != nil && !l.stopped {
					// Past the unsettled bound once the stages run, as
					// a long line puts it ([Stream.settle]).
					s.p.feed(l.raw)
					l.raw, l.stopped = nil, true
				}
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

// A LIVE STREAM PAST ITS UNSETTLED BOUND TRAILS AS A STREAM DOES: a line
// longer than [unsettledBytes] that never ends stops its settling for good,
// what it writes is still Secrets of the text, and the held count it answers
// is a lower bound. A text whose lines end never gets there, flushed or not.
//
// Mutation: hold the unsettled text whatever its length ([Stream.settle]),
// and the stream holds the whole line; settle only on a flush
// ([Stream.settleIn]), and a stream never flushed holds all it was written.
func TestALiveStreamPastItsUnsettledBoundTrailsAsAStreamDoes(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 4))
	long := testRun(r, testB64url, 2<<20)
	text := streamText(r, wholeBytes+pieceBytes) + "\nsk-" + long + " and on"
	var out bytes.Buffer
	s := NewLiveStream(&out)
	most := 0
	for at := 0; at < len(text); {
		n := min(len(text)-at, 1+r.IntN(64<<10))
		_, _ = s.Write([]byte(text[at : at+n]))
		at += n
		most = max(most, len(s.live.raw))
		_, held, err := s.Flush()
		if err != nil || held > at {
			t.Fatalf("a flush at %d answered %d held and %v", at, held, err)
		}
		if s.live.stopped && held == 0 {
			t.Fatalf("a flush at %d in a line it settles no more answered none of it held", at)
		}
	}
	if !s.live.stopped || most > unsettledBytes+1 {
		t.Errorf("a line of %d MiB held %d bytes unsettled at most, settling stopped: %v", len(long)>>20, most, s.live.stopped)
	}
	if err := s.Close(); err != nil || out.String() != Secrets(text) {
		t.Errorf("Close answered %v, and the stream wrote Secrets of the text: %v", err, out.String() == Secrets(text))
	}
	lines := streamText(r, 4<<20)
	s = NewLiveStream(io.Discard)
	most = 0
	for at := 0; at < len(lines); at += 64 << 10 {
		_, _ = s.Write([]byte(lines[at:min(len(lines), at+64<<10)]))
		if s.live.raw != nil {
			most = max(most, len(s.live.raw))
		}
	}
	if s.live.stopped || most > unsettledBytes {
		t.Errorf("%d MiB of lines none longer than %d KiB, written with no flush, held %d KiB unsettled; settling stopped: %v", len(lines)>>20, 3*stageAdvance>>10, most>>10, s.live.stopped)
	}
	_ = s.Close()
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
		s := NewStreamHolding(&out, holdBytes(), c.liveStream)
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

// A FLUSH THAT STOPS SETTLING DECIDES NOTHING UNSETTLED: where the flush's
// own settling runs past [unsettledBytes] and hands the stages a line not
// yet ended, the stages trail it as a [NewStream] does rather than decide it
// as at the text's end, and the stream still writes Secrets of the text.
// A write asks [Settled] before that much is held, so the test lets it hold
// more.
//
// Mutation: decide the stages after such a settle ([Stream.Flush]), and the
// token is redacted as if it ended at the flush.
func TestAFlushThatStopsSettlingDecidesNothingUnsettled(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 5))
	text := streamText(r, wholeBytes+pieceBytes) + "\nsk-" + testRun(r, testB64url, 2<<20) + " and on\n"
	cut := len(text) - (1 << 20)
	var out bytes.Buffer
	s := NewLiveStream(&out)
	_, _ = s.Write([]byte(text[:wholeBytes+1]))
	s.live.due = 4 * unsettledBytes
	_, _ = s.Write([]byte(text[wholeBytes+1 : cut]))
	if _, _, err := s.Flush(); err != nil || !s.live.stopped {
		t.Fatalf("a flush holding %d bytes unsettled answered %v; settling stopped: %v", cut-wholeBytes, err, s.live.stopped)
	}
	_, _ = s.Write([]byte(text[cut:]))
	if err := s.Close(); err != nil || out.String() != Secrets(text) {
		t.Errorf("Close answered %v, and the stream wrote another text than Secrets %s", err, parted(out.String(), Secrets(text)))
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
		s := NewStreamHolding(&out, h, flushAt > 0)
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
