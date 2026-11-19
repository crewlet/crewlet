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

// liveRun is what a stream wrote for a text and answered, and at each flush
// how much it had written and how much of that it answered final.
type liveRun struct {
	out     string
	err     error
	flushes [][2]int64
}

// readLive is text written to a stream holding h, live where flush is not
// nil, in pieces of 1 to most bytes drawn from r, flushed after each write
// where flush says, and once more after Close.
func readLive(text string, h int, r *rand.Rand, most int, flush func() bool) liveRun {
	var run liveRun
	var out bytes.Buffer
	s := redact.NewStreamHolding(&out, h, flush != nil)
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

// liveTexts is a text of each kind the package's tests draw, at each length
// a live stream reads apart: a transcript of every shape the keys' reader
// tells apart, below wholeBytes, across it and past it; the keys in every
// wrapping; every shape a stage carries across its window; and texts built to
// hold a stream, read at a hold that refuses some of them.
func liveTexts(r *rand.Rand) (texts []string, names []string, holds []int) {
	add := func(name, text string, h int) {
		texts, names, holds = append(texts, text), append(names, name), append(holds, h)
	}
	for _, size := range []int{redact.WholeBytes / 3, redact.WholeBytes - 100, redact.WholeBytes + 1000, 3 * redact.WholeBytes} {
		var b strings.Builder
		for b.Len() < size {
			b.WriteString(transcriptLines[r.IntN(len(transcriptLines))])
			b.WriteByte('\n')
		}
		add(fmt.Sprintf("a transcript of %d bytes", b.Len()), b.String(), redact.HoldBytes())
	}
	for name, text := range streamCorpus(r) {
		add("the keys of "+name+" in every wrapping", text, redact.HoldBytes())
	}
	add("the stages' shapes", redact.StreamText(r, 4*redact.WholeBytes), redact.HoldBytes())
	// A shape of each hold, and every one a stream refuses at this hold.
	each := []string{"a1", "a3", "a5", "a6", "b1", "c1", "d1", "e1", "g1", "g2", "gd1", "hc5", "p1", "pa3", "pb1", "pc4"}
	for _, s := range redact.HoldShapes(768 << 10) {
		if slices.Contains(each, s[0]) {
			add("hold shape "+s[0], s[1], 256<<10)
		}
	}
	add("an early text", redact.EarlyText(r, 1<<20, 512<<10), 256<<10)
	return texts, names, holds
}

// A LIVE STREAM WRITES WHAT THE WHOLE TEXT REDACTS TO HOWEVER IT IS FLUSHED:
// over [liveTexts], cut at random and flushed at random points, where Close
// answers nil it wrote [redact.Secrets] of the text; at every flush, and after
// a refusal, what it answered final is a prefix of Secrets; it refuses only
// where a [redact.NewStream] does, from no earlier a byte; and never flushed,
// exactly where one does. A flush decides claims a stream decides late, so it
// can spare a refusal, as it does for most of the texts built to refuse.
//
// Mutation: decide each stage to what it was handed whether or not it is
// settled ([redact.Stream.Flush]), or have the end keep what the head's
// flushes wrote, and a text is written otherwise than Secrets.
func TestALiveStreamWritesWhatTheWholeTextRedactsToHoweverItIsFlushed(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 2))
	texts, names, holds := liveTexts(r)
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
	refused, spared := 0, 0
	for i, text := range texts {
		want := redact.Secrets(text)
		plain := readLive(text, holds[i], r, 64<<10, nil)
		if plain.err != nil {
			refused++
		}
		for _, sc := range schedules {
			if sc.most < 1<<10 && len(text) > 3*redact.WholeBytes {
				continue
			}
			got := readLive(text, holds[i], r, sc.most, func() bool { return sc.odds > 0 && r.IntN(sc.odds) == 0 })
			name := names[i] + ", " + sc.name
			at, plainAt := redact.RefusedAt(got.err), redact.RefusedAt(plain.err)
			if got.err == nil && plain.err != nil {
				spared++
			}
			switch {
			case got.err != nil && (plain.err == nil || at < plainAt):
				t.Errorf("%s: the live stream answered %v, where a stream answered %v", name, got.err, plain.err)
			case sc.odds == 0 && at != plainAt:
				t.Errorf("%s: the live stream answered %v, where a stream answered %v", name, got.err, plain.err)
			case got.err == nil && got.out != want:
				t.Errorf("%s: Close answered nil, and the live stream wrote another text than Secrets %s", name, streamParted(got.out, want))
			}
			for _, f := range got.flushes {
				if f[1] > f[0] || got.out[:f[1]] != want[:min(int(f[1]), len(want))] {
					t.Errorf("%s: a flush answered %d of %d bytes final, which are not Secrets' %s", name, f[1], f[0], streamParted(got.out[:min(f[1], int64(len(got.out)))], want))
					break
				}
			}
		}
	}
	t.Logf("%d texts, %d refused by a stream; %d flushed live readings of those not refused", len(texts), refused, spared)
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
