package executor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A FAILURE IS HELD TO ITS BOUND EXACTLY, as it leaves, redacted whole. Two
// things can grow it past: KeepEnd's mark on a line kept by its end (outside
// the bytes it counts), and a credential's name ending one piece matching
// across the `:\n` into the next. The widest unread note removes the slack
// that would hide both.
//
// Mutation: return the first composition without measuring it redacted, and
// the key-name case is fifteen bytes past the bound; leave the mark out of the
// room as well, and the single-line case is three past it.
//
// At a 64 KiB bound, not the real one: a marker and a join are the same bytes
// at any bound.
func TestAFailureIsHeldToItsBoundAsItLeaves(t *testing.T) {
	t.Parallel()
	// Thirteen digits of MiB, the width of the widest note.
	const unread = int64(2e18)
	const bound = 64 << 10
	oneLine := strings.Repeat("e", bound*3/2)
	for name, sentences := range map[string][]string{
		"a single line kept by its end":     {"the harness exited with status 1"},
		"a key name meeting the next piece": {"the harness exited with status 1", "the harness could not read pwd"},
	} {
		got := failureDetail(sentences, oneLine, unread, bound, redact.Secrets)
		if len(got) > bound {
			t.Errorf("%s: the failure is %d bytes, %d past its bound", name, len(got), len(got)-bound)
		}
		if redact.Secrets(got) != got {
			t.Errorf("%s: the failure was not redacted whole, so redacting it again would grow it", name)
		}
		if !strings.HasPrefix(got, "the harness exited with status 1") || !strings.HasSuffix(got, "eee") {
			t.Errorf("%s: the failure lost its opening or the stream's end", name)
		}
	}
}

// ONE LINE IS HELD AT MOST: a line past the bound is counted, never kept, and
// the lines around it are read as they were.
//
// The bound is one past [lineChunkBytes], so a line at it still arrives in
// more than one piece, the case the counting is for.
func TestALineIsReadInOnePieceUpToTheBound(t *testing.T) {
	t.Parallel()
	const bound = 64<<10 + 1
	var seen []string
	var skipped []int64
	var bounds []int
	dec := &recorder{line: func(l []byte) { seen = append(seen, string(l)) },
		skip: func(n int64, at int) { skipped, bounds = append(skipped, n), append(bounds, at) }}
	huge := strings.Repeat("x", bound+5)
	whole := strings.Repeat("y", bound-1)
	if err := eachLine(strings.NewReader("a\r\n"+huge+"\n"+whole+"\nb\nlast"), dec, bound); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, "|") != "a|"+whole+"|b|last" {
		t.Errorf("read %d lines; want the lines around the long one, one exactly at the bound, "+
			"and the unterminated last", len(seen))
	}
	if len(skipped) != 1 || skipped[0] != int64(len(huge)+1) || bounds[0] != bound {
		t.Errorf("skipped = %v past %v; want one line of %d bytes past %d", skipped, bounds, len(huge)+1, bound)
	}
}

// A RUNNER READS TO THE ENGINE'S OWN BOUNDS (sandbox.MaxFailureBytes,
// maxLineBytes): the cases past either run at their own ([Bounded] in
// export_test.go), so this ties those to what every runner New makes.
func TestARunnerReadsToTheEnginesOwnBounds(t *testing.T) {
	t.Parallel()
	if maxLineBytes != sandbox.MaxFileBytes {
		t.Errorf("maxLineBytes = %d, want sandbox.MaxFileBytes (%d)", maxLineBytes, sandbox.MaxFileBytes)
	}
	for _, r := range []*Runner{NewClaudeCode(), NewOpenCode()} {
		if r.failureBound != sandbox.MaxFailureBytes || r.lineBound != maxLineBytes {
			t.Errorf("%s reads a failure to %d and a line to %d; want sandbox.MaxFailureBytes (%d) "+
				"and maxLineBytes (%d)", r.Name(), r.failureBound, r.lineBound,
				sandbox.MaxFailureBytes, maxLineBytes)
		}
		if f, ok := r.Follow(sandbox.RunHandle{}).(*follower); !ok || f.lineBound != r.lineBound {
			t.Errorf("%s's live reading does not read lines to its runner's bound", r.Name())
		}
	}
}

// A LINE READER KEEPS ITS PLACE ACROSS READS: a line split between reads is
// handed on whole once its break arrives, a line past the bound is counted as
// one across reads, a read that fails hands on nothing and keeps what it had,
// and the unfinished last line waits until the stream is whole, then is handed
// on even by a read that brings nothing. Every byte is counted as taken, and a
// long line's buffer is not kept past the line.
//
// Mutation: begin each read with nothing held, or nothing counted past the
// bound, and a split line is read as pieces; hand nothing on from a read with
// end that took no byte, and the held line is lost; keep a long line's buffer,
// and the reader holds it between reads.
func TestALineReaderKeepsItsPlaceAcrossReads(t *testing.T) {
	t.Parallel()
	r := streamLines{bound: 8}
	var got []string
	feed := func(src io.Reader, end bool) error {
		return r.read(bufio.NewReaderSize(src, 16), end,
			func(l []byte) error { got = append(got, fmt.Sprintf("%q broken %v", l, r.broken)); return nil },
			func(n int64, _, _ []byte) error { got = append(got, fmt.Sprintf("skipped %d", n)); return nil })
	}
	boom := errors.New("the store stopped answering")
	for i, step := range []struct {
		src  io.Reader
		end  bool
		want error
	}{
		{strings.NewReader("ab"), false, nil},
		{strings.NewReader("c\nde"), false, nil},
		{strings.NewReader("fghijkl"), false, nil},
		{strings.NewReader("mn\nxy"), false, nil},
		{io.MultiReader(strings.NewReader("z"), iotest.ErrReader(boom)), false, boom},
		{strings.NewReader("\nlast"), true, nil},
	} {
		if err := feed(step.src, step.end); !errors.Is(err, step.want) {
			t.Fatalf("read %d = %v, want %v", i, err, step.want)
		}
	}
	want := []string{`"abc" broken true`, "skipped 12", `"xyz" broken true`, `"last" broken false`}
	if strings.Join(got, "|") != strings.Join(want, "|") || r.taken != 24 {
		t.Errorf("read %q, %d bytes taken; want %q, all 24", got, r.taken, want)
	}
	for _, tc := range []struct{ held, want string }{
		{"ab", `"ab" broken false`},
		{"0123456789", "skipped 10"},
	} {
		r, got = streamLines{bound: 8}, nil
		if err := feed(strings.NewReader(tc.held), false); err != nil || len(got) != 0 {
			t.Fatalf("a read without end of %q = %v, handing on %q; want nothing", tc.held, err, got)
		}
		if err := feed(strings.NewReader(""), true); err != nil || strings.Join(got, "|") != tc.want {
			t.Errorf("an empty read with end after %q = %v, handing on %q; want %q", tc.held, err, got, tc.want)
		}
	}

	long := streamLines{bound: 1 << 20}
	var lines []int
	err := long.read(bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 2*lineChunkBytes)+"\nsmall"),
		lineChunkBytes), false, func(l []byte) error { lines = append(lines, len(l)); return nil }, nil)
	if err != nil || len(lines) != 1 || lines[0] != 2*lineChunkBytes || string(long.held) != "small" {
		t.Fatalf("read lines %v, holding %q, %v; want the long line, then the short one held", lines, long.held, err)
	}
	if cap(long.held) > lineChunkBytes {
		t.Errorf("between reads the reader holds a buffer of %d bytes; want at most %d", cap(long.held),
			lineChunkBytes)
	}
}

// A STREAM READ IN PIECES IS READ AS ONE READ WHOLE: a [streamLines] fed a
// stream cut anywhere hands on the lines and skipped sizes [readLines] does.
// Seeded.
func TestAStreamReadInPiecesIsReadAsOneWhole(t *testing.T) {
	t.Parallel()
	const bound = 40
	record := func(log *[]string) (func([]byte) error, func(int64, []byte, []byte) error) {
		return func(l []byte) error { *log = append(*log, string(l)); return nil },
			func(n int64, _, _ []byte) error { *log = append(*log, fmt.Sprint("skipped ", n)); return nil }
	}
	rng := rand.New(rand.NewPCG(20261119, 15))
	for trial := range 500 {
		var b strings.Builder
		for range 1 + rng.IntN(8) {
			b.WriteString(strings.Repeat("ab", rng.IntN(bound)))
			b.WriteString([]string{"\n", "\r\n", "\n\n"}[rng.IntN(3)])
		}
		b.WriteString(strings.Repeat("c", rng.IntN(bound)))
		text := b.String()
		var whole, pieces []string
		line, skipped := record(&whole)
		if err := readLines(bufio.NewReaderSize(strings.NewReader(text), 16), bound, line,
			func(n int64) error { return skipped(n, nil, nil) }); err != nil {
			t.Fatal(err)
		}
		r := streamLines{bound: bound}
		line, skipped = record(&pieces)
		for at := 0; at < len(text); {
			next := min(len(text), at+1+rng.IntN(bound))
			if err := r.read(bufio.NewReaderSize(strings.NewReader(text[at:next]), 16), next == len(text),
				line, skipped); err != nil {
				t.Fatal(err)
			}
			at = next
		}
		if strings.Join(pieces, "|") != strings.Join(whole, "|") || r.taken != int64(len(text)) {
			t.Fatalf("trial %d, %q: in pieces %q, %d taken; want %q, all %d", trial, text, pieces, r.taken,
				whole, len(text))
		}
	}
}

// A LONG LINE'S BUFFER SERVES ITS READ AND GOES WITH IT: one read with end
// hands two long lines on from one buffer (Collect's and ReadTranscript's
// reads grow it once, not per line), and a kept reader holds at most a chunk
// once such a read returns, at the stream's end or on a callback's stop.
// Addresses and capacities are compared, no allocation counted, so -race
// leaves it stable.
//
// Mutation: let the buffer go after every long line, keep it after a read
// with end returns, or keep it on a stop, and one of the three fails.
func TestALongLinesBufferServesItsReadAndGoesWithIt(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 2*lineChunkBytes)
	var firsts []*byte
	err := readLines(bufio.NewReaderSize(strings.NewReader(long+"\n"+long+"\n"), lineChunkBytes), 1<<20,
		func(l []byte) error { firsts = append(firsts, &l[0]); return nil }, nil)
	if err != nil || len(firsts) != 2 || firsts[0] != firsts[1] {
		t.Errorf("read %d lines, one buffer %v, %v; want two from one buffer", len(firsts),
			len(firsts) == 2 && firsts[0] == firsts[1], err)
	}
	stop := errors.New("enough")
	for _, tc := range []struct {
		name string
		line func([]byte) error
		want error
	}{
		{"at the end", func([]byte) error { return nil }, nil},
		{"on a stop", func([]byte) error { return stop }, stop},
	} {
		r := streamLines{bound: 1 << 20}
		err := r.read(bufio.NewReaderSize(strings.NewReader(long+"\ntail"), lineChunkBytes), true, tc.line, nil)
		if !errors.Is(err, tc.want) || cap(r.held) > lineChunkBytes {
			t.Errorf("%s: read = %v holding %d bytes of buffer; want %v, at most %d", tc.name, err, cap(r.held),
				tc.want, lineChunkBytes)
		}
	}
}

type recorder struct {
	line func([]byte)
	skip func(int64, int)
}

func (r *recorder) Line(l []byte)          { r.line(l) }
func (r *recorder) Skipped(l LongLine)     { r.skip(l.Size, l.Bound) }
func (r *recorder) Undecided() int         { return 0 }
func (r *recorder) Result() sandbox.Result { return sandbox.Result{} }
func (r *recorder) Ended() bool            { return false }
func (r *recorder) Requests() int          { return 0 }
func (r *recorder) InFlight() int          { return 0 }
func (r *recorder) Entries(bool) []string  { return nil }

// AN ENTRY WITH NO WORDS IS NONE: added, it says nothing, and a run of skipped
// lines open before it stays open until an entry with words, or the end, says
// it.
//
// Mutation: add an empty entry as written, or flush the open run before
// dropping it, and a take before the end hands an entry over.
func TestAnEntryWithNoWordsIsNone(t *testing.T) {
	t.Parallel()
	var tl transcriptLines
	tl.add(" \t\r\n ")
	tl.skip(9, 8)
	tl.add("   ")
	if got := tl.take(false); len(got) != 0 {
		t.Fatalf("before the end, entries without words handed over %q; want none", got)
	}
	if got := tl.take(true); len(got) != 1 || !strings.Contains(got[0], "1 line(s) of output") {
		t.Fatalf("at the end, handed over %q; want the skipped line's note alone", got)
	}
}
