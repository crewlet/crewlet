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
	"unicode/utf8"

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

// A KEY THAT BEGAN BEFORE THE WINDOW IS STILL REDACTED: the end is read with
// [redact.LookBack] before it and redacted over all of it before anything is
// dropped.
func TestAKeyThatBeganBeforeTheWindowIsStillRedacted(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n", 40)
	key := "-----BEGIN RSA PRIVATE KEY-----\n" + body + "-----END RSA PRIVATE KEY-----\n"
	keep := 1024
	// The key's BEGIN sits before the last `keep` bytes, its body and END inside.
	stream := strings.Repeat("noise\n", 2000) + key + "done\n"
	data := []byte(stream)
	window := keep + redact.LookBack
	tail := sandbox.FileTail{Data: data[max(0, len(data)-window):], Size: int64(len(data))}

	got, unread := streamEnd(tail, keep)
	if strings.Contains(got, "MIIEowIBAAKCAQEA") {
		t.Errorf("a key body reached the window unredacted: %q", got)
	}
	if !strings.HasSuffix(got, "done\n") || unread <= 0 {
		t.Errorf("the window = …%q with %d unread; want the stream's end and its start counted",
			got[max(0, len(got)-40):], unread)
	}
	if redact.Contains(got) {
		t.Error("the window still holds a credential shape")
	}
}

// THE WINDOW STARTS ON A LINE past the context, and everything before it is
// counted; a line the stream opens that is longer than the window keeps its
// end on a character, marked, and a line a read began inside, running to the
// stream's end, is not shown at all, since only a line read from its start
// redacts as the whole stream does.
//
// Mutation: keep the end of a line begun before the settled start, and the
// last case shows it.
func TestTheEndOfAStreamIsWholeLines(t *testing.T) {
	t.Parallel()
	const keep = 1000
	const line = "a line the process wrote\n"
	stream := strings.Repeat(line, (keep+redact.LookBack)/len(line)+100)
	read := keep + redact.LookBack
	if (len(stream)-read)%len(line) == 0 {
		t.Fatal("the read opens on a line; the case needs one that opens inside a line, as a byte count lands")
	}
	tail := sandbox.FileTail{Data: []byte(stream[len(stream)-read:]), Size: int64(len(stream))}
	got, unread := streamEnd(tail, keep)
	if got == "" || len(got) > keep || !strings.HasPrefix(got, line) || !strings.HasSuffix(stream, got) ||
		unread != int64(len(stream)-len(got)) {
		t.Errorf("streamEnd = %d bytes opening %.30q, %d unread; want whole lines of the stream's end, at "+
			"most %d bytes, and every byte before them counted", len(got), got, unread, keep)
	}

	long := []byte(strings.Repeat("日", 2000))
	got, _ = streamEnd(sandbox.FileTail{Data: long, Size: int64(len(long))}, 100)
	kept, marked := strings.CutPrefix(got, "…")
	if !marked || len(kept) > 100 || !utf8.ValidString(kept) {
		t.Errorf("a line longer than the window = %q; want its end, marked, on a character", got)
	}

	for _, one := range []string{strings.Repeat("x", 3*redact.LookBack), strings.Repeat("x", 3*redact.LookBack) + "\n"} {
		tail = sandbox.FileTail{Data: []byte(one[len(one)-read:]), Size: int64(len(one))}
		got, unread = streamEnd(tail, keep)
		if got != "" || unread != int64(len(one)) {
			t.Errorf("a line a read began inside = %d bytes opening %.20q, %d unread; want none of it "+
				"shown, and all %d bytes counted", len(got), got, unread, len(one))
		}
	}
}

// A KEY THAT BEGAN BEFORE THE READ IS NEVER SHOWN, however much redaction
// shrinks what follows: read without its BEGIN, a key is redacted back from
// its END only until three foreign lines (redact's keyBlocks and backReading),
// and later keys collapsing into markers must not pull the kept end into the
// context.
//
// Mutation: keep the end of the whole read's redaction (sandbox.KeepEnd over
// redact.Secrets of every whole line read), and the key's body is shown.
func TestAKeyThatBeganBeforeTheReadIsNeverShown(t *testing.T) {
	t.Parallel()
	const body = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
	const other = "MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGy0AHB7MaV0gM9g7x3E8Mq1"
	begin, end := "-----BEGIN RSA PRIVATE KEY-----\n", "-----END RSA PRIVATE KEY-----\n"
	var s strings.Builder
	s.WriteString(strings.Repeat("compiling a package of the build\n", 50))
	s.WriteString(begin)
	opened := s.Len()
	s.WriteString(strings.Repeat(body+"\n", 40))
	s.WriteString(strings.Repeat("retrying the upload in two seconds\n", 3))
	s.WriteString(strings.Repeat(body+"\n", 5) + end)
	// Keys enough to fill the context, each a 22-byte marker once redacted.
	for range redact.LookBack/(25*len(other)) + 8 {
		s.WriteString(begin + strings.Repeat(other+"\n", 25) + end)
	}
	s.WriteString("done\n")
	stream := s.String()
	// The read opens inside the key's body, ten lines and a few bytes in.
	data := []byte(stream[opened+10*(len(body)+1)+7:])
	keep := len(data) - redact.LookBack
	if keep <= 0 {
		t.Fatalf("the read is %d bytes; the case needs more than the %d of context", len(data), redact.LookBack)
	}
	got, unread := streamEnd(sandbox.FileTail{Data: data, Size: int64(len(stream))}, keep)
	if strings.Contains(got, body[:24]) || strings.Contains(got, other[:24]) || redact.Contains(got) {
		t.Errorf("a key's body reached what is shown:\n%s", got)
	}
	if !strings.HasSuffix(got, "done\n") || unread <= int64(len(stream)-len(data)) {
		t.Errorf("shown = …%q with %d unread; want the stream's end, and its start and the context counted",
			got[max(0, len(got)-40):], unread)
	}
}

// A STREAM'S END IS SHOWN ONLY PAST ALL ITS CONTEXT, however much redaction
// shrinks what it keeps: only the context's end settles what is shown, which
// is the end of the whole stream's redaction.
//
// Mutation: settle a key block's reach (redact.MaxKeyBlockBytes) into the read
// rather than [redact.LookBack], and the case goes red.
func TestAStreamsEndIsShownOnlyPastAllItsContext(t *testing.T) {
	t.Parallel()
	filler := strings.Repeat("compiling a package of the build\n", 2*redact.LookBack/33+1)
	label := "**" + strings.Repeat("a", 6000) + " password:**\n" + strings.Repeat("\n", 60000)
	// The read begins ten bytes into the label, after its opening markup.
	read := len(filler) + 10
	head := filler + label + "hunter2x9\n"
	head += strings.Repeat("x\n", (read+redact.LookBack-len(head))/2)
	head += strings.Repeat("\n", read+redact.LookBack-len(head))
	end := strings.Repeat("sk-"+strings.Repeat("B", 50000)+"\n", 8) + strings.Repeat("ok\n", 100)
	stream := head + end
	whole := redact.Secrets(stream)
	if strings.Contains(whole, "hunter2x9") {
		t.Fatal("the whole redaction keeps the value; the case needs one that takes it out")
	}
	got, unread := streamEnd(sandbox.FileTail{Data: []byte(stream[read:]), Size: int64(len(stream))}, len(end))
	if !strings.HasSuffix(whole, got) || strings.Contains(got, "hunter2x9") || !strings.HasSuffix(got, "ok\n") ||
		unread < int64(len(head)) {
		t.Errorf("shown = %d bytes opening %.40q, %d unread; want the end of the whole redaction from past the "+
			"context, and the context counted", len(got), got, unread)
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
		if err := readLines(bufio.NewReaderSize(strings.NewReader(text), 16), bound, line, skipped); err != nil {
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
