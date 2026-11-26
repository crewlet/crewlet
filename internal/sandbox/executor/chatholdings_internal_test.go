package executor

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// These cases hold the core a chat file's holdings reading walks on
// (chatholdings.go): the archive's one member, its lines, and each id once;
// and the digest's opening and scan (textdigest.go) and a release set's floor
// (holdings.go), which the readings share.

// clRead reads file with [chatLines], each line read a 7-byte piece at a time
// and kept only once its body answered io.EOF, as a reading applies a line.
func clRead(ctx context.Context, file io.Reader) ([]string, int, error) {
	var lines []string
	n, err := chatLines(ctx, file, func(i int, body io.Reader) error {
		if want := len(lines) + 1; i != want {
			return fmt.Errorf("line %d was handed as line %d", want, i)
		}
		var text []byte
		piece := make([]byte, 7)
		for {
			m, err := body.Read(piece)
			text = append(text, piece[:m]...)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}
		lines = append(lines, string(text))
		return nil
	})
	return lines, n, err
}

// clJSON reads file with [chatLines] as a reading does: each line one JSON
// value on one scanner, kept only once the scanner reached the line's end.
func clJSON(file io.Reader) ([]string, int, error) {
	var (
		lines []string
		sc    = jsonscan.New(nil)
	)
	n, err := chatLines(context.Background(), file, func(_ int, body io.Reader) error {
		sc.Reset(body)
		if _, err := sc.Next(); err != nil {
			return err
		}
		text, _, err := sc.Capture(1 << 10)
		if err != nil {
			return err
		}
		kept := string(text)
		if _, err := sc.Next(); !errors.Is(err, io.EOF) {
			return errors.Join(errors.New("a second value follows"), err)
		}
		lines = append(lines, kept)
		return nil
	})
	return lines, n, err
}

// A CHAT FILE IS READ ONLY TO ITS LAST BREAK: every `\n`-ended line is handed
// whole, numbered from 1, and the bytes after the last break, which the gate's
// cut drops before a resume ([tornCut]), never count, whether they are a torn
// record, a whole one missing its break, or the NULs GNU tar pads a member
// with.
//
// Mutation: answer a torn line's error, hand the tail as a line, or end a line
// at a `\n` the next piece holds, and a case goes red.
func TestAChatFileIsReadOnlyToItsLastBreak(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("v", 3*lineChunkBytes+5)
	for name, tc := range map[string]struct {
		file  string
		lines []string
	}{
		"two whole lines":            {"a\nbc\n", []string{"a", "bc"}},
		"a torn tail":                {"a\nbc", []string{"a"}},
		"a padded tail":              {"a\n\x00\x00\x00", []string{"a"}},
		"no break at all":            {"abc", nil},
		"empty":                      {"", nil},
		"an empty line handed":       {"a\n\n", []string{"a", ""}},
		"a line past the read piece": {long + "\nz\n" + long, []string{long, "z"}},
	} {
		lines, n, err := clRead(t.Context(), strings.NewReader(tc.file))
		if err != nil || n != len(tc.lines) || !slices.Equal(lines, tc.lines) {
			t.Errorf("%s: read %d lines %.40q, %v; want %d lines %.40q", name, n, lines, err, len(tc.lines), tc.lines)
		}
	}
	for name, tc := range map[string]struct {
		file  string
		lines []string
	}{
		"a torn record":            {`{"a":1}` + "\n" + `{"b":`, []string{`{"a":1}`}},
		"a whole record unbroken":  {`{"a":1}` + "\n" + `{"b":2}`, []string{`{"a":1}`}},
		"a malformed torn record":  {`{"a":1}` + "\n" + `{"b":2}}x`, []string{`{"a":1}`}},
		"each line its own value":  {`{"a":1}` + "\n" + `[2]` + "\n", []string{`{"a":1}`, `[2]`}},
		"one value cut at a break": {`{"a":` + "\n" + `1}` + "\n", nil},
	} {
		lines, n, err := clJSON(strings.NewReader(tc.file))
		wantErr := name == "one value cut at a break"
		if (err != nil) != wantErr || n != len(tc.lines) || !slices.Equal(lines, tc.lines) {
			t.Errorf("%s: read %d lines %q, %v; want %d lines %q, an error %v", name, n, lines, err,
				len(tc.lines), tc.lines, wantErr)
		}
	}
}

// A CARRIAGE RETURN IN A WHOLE LINE IS NO READING: Node's readline ends a
// line there, so the harness's load does not read the line the file holds.
// One in the torn tail is dropped with it.
//
// Mutation: end a line only at `\n`, or drop a whole line's `\r`, and a case
// goes red.
func TestACarriageReturnInAWholeChatLineIsNoReading(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		file  string
		lines []string
		err   error
	}{
		"before a break":   {"a\r\nb\n", nil, errChatHoldingsCarriage},
		"inside a line":    {"a\nb\rc\n", []string{"a"}, errChatHoldingsCarriage},
		"in the torn tail": {"a\nb\rc", []string{"a"}, nil},
	} {
		lines, _, err := clRead(t.Context(), strings.NewReader(tc.file))
		if !errors.Is(err, tc.err) || !slices.Equal(lines, tc.lines) {
			t.Errorf("%s: read %q, %v; want %q, %v", name, lines, err, tc.lines, tc.err)
		}
	}
}

// A LINE IS KEPT ONLY ONCE READ TO ITS BREAK: a reading that answers for a
// line it did not read to its end kept facts of bytes it never saw, whole
// line or torn tail alike, so the file is no reading.
//
// Mutation: drop the check, or let a torn tail excuse it, and a case goes red.
func TestAChatLineKeptUnreadIsNoReading(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"a\nb\n", "abc"} {
		_, err := chatLines(t.Context(), strings.NewReader(file), func(int, io.Reader) error { return nil })
		if !errors.Is(err, errChatHoldingsUnread) {
			t.Errorf("%q read unread answers %v; want %v", file, err, errChatHoldingsUnread)
		}
	}
}

// A WHOLE LINE'S FAULT IS THE READING'S ANSWER, and the file's own reader's
// failure inside a line too: only the torn tail is dropped.
//
// Mutation: drop every line's error, or take the reader's failure for a torn
// tail, and a case goes red.
func TestAWholeChatLinesFaultIsTheAnswer(t *testing.T) {
	t.Parallel()
	fault := errors.New("the line's fault")
	_, err := chatLines(t.Context(), strings.NewReader("a\nbad\nc\n"), func(n int, body io.Reader) error {
		if _, err := io.Copy(io.Discard, io.LimitReader(body, 1)); err != nil || n == 2 {
			return cmp.Or(err, fault)
		}
		_, err := io.Copy(io.Discard, body)
		return err
	})
	if !errors.Is(err, fault) {
		t.Errorf("a whole line's fault answers %v; want %v", err, fault)
	}
	lines, n, err := clRead(t.Context(), io.MultiReader(strings.NewReader("a\nb"), iotest.ErrReader(errScrubBound)))
	if !errors.Is(err, errScrubBound) || n != 1 || !slices.Equal(lines, []string{"a"}) {
		t.Errorf("a reader failing inside a line reads %d lines %q, %v; want 1, %v", n, lines, err, errScrubBound)
	}
}

// A CHAT FILE'S READING STOPS BETWEEN LINES WHEN ITS CONTEXT ENDS: no line
// after is handed.
//
// Mutation: drop the context's check, and this goes red.
func TestAChatFilesReadingStopsBetweenLinesWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handed := 0
	_, err := chatLines(ctx, strings.NewReader("a\nb\nc\n"), func(_ int, body io.Reader) error {
		handed++
		cancel()
		_, err := io.Copy(io.Discard, body)
		return err
	})
	if !errors.Is(err, context.Canceled) || handed != 1 {
		t.Errorf("handed %d lines and answered %v; want 1 and %v", handed, err, context.Canceled)
	}
}

// clLineAllocates is the most [TestAChatFilesReadingHoldsNoLine] allows a
// 64 MiB line's read to allocate: the line reader's 64 KiB buffer and the
// test's 32 KiB piece, four times over; the line held once is 256 times it.
const clLineAllocates = 256 << 10

// A CHAT FILE'S READING HOLDS NO LINE: a 64 MiB line passes through within
// [clLineAllocates], whatever its length.
//
// Not parallel: it reads the process's allocation count, the fewest bytes of
// three reads ([allocationsOf]).
//
// Mutation: buffer a line whole, and this goes red.
func TestAChatFilesReadingHoldsNoLine(t *testing.T) {
	// A piece a Read: a byte a Read, one read of the line takes the race
	// detector some 50 s on a two-core host.
	vs := bytes.Repeat([]byte("v"), textDigestBytes)
	var (
		total int64
		n     int
		err   error
	)
	_, allocated := allocationsOf(t, func() {
		total = 0
		piece := make([]byte, textDigestBytes)
		file := io.MultiReader(&repeating{lines: vs, left: 64 << 20}, strings.NewReader("\n"))
		n, err = chatLines(t.Context(), file, func(_ int, body io.Reader) error {
			m, err := io.CopyBuffer(io.Discard, struct{ io.Reader }{body}, piece)
			total += m
			return err
		})
	})
	if err != nil || n != 1 || total != 64<<20 {
		t.Fatalf("read %d lines of %d bytes, %v; want one of %d", n, total, err, 64<<20)
	}
	if allocated > clLineAllocates {
		t.Errorf("a 64 MiB line allocated %d bytes; want at most %d", allocated, clLineAllocates)
	}
}

// clArchive is a tar of entries, each a name, a type and a body.
func clArchive(t *testing.T, entries ...[3]string) io.Reader {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		h := &tar.Header{Name: e[0], Typeflag: e[1][0], Mode: 0o600, Size: int64(len(e[2]))}
		if h.Typeflag != tar.TypeReg {
			h.Size, h.Linkname = 0, "elsewhere"
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e[2])); err != nil && h.Typeflag == tar.TypeReg {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

// A CHAT FILE'S ARCHIVE HOLDS ITS ONE CHAT FILE AND NOTHING ELSE: the first
// entry, a plain file its reading names; anything before it, a link or a
// directory in its place, or an entry after it, is no reading; and a tar cut
// inside the file is no torn tail.
//
// Mutation: skip to the named entry, take a link, or drop the end's check,
// and a case goes red.
func TestAChatFilesArchiveHoldsItsOneChatFile(t *testing.T) {
	t.Parallel()
	const name = "chats/s.jsonl"
	named := func(n string) bool { return n == name }
	chat := [3]string{name, string(tar.TypeReg), "a\n"}
	for label, tc := range map[string]struct {
		archive io.Reader
		open    error
		end     error
	}{
		"the one chat file":     {clArchive(t, chat), nil, nil},
		"another entry first":   {clArchive(t, [3]string{"x", string(tar.TypeReg), "b\n"}, chat), errChatHoldingsNone, nil},
		"a link in its place":   {clArchive(t, [3]string{name, string(tar.TypeSymlink), ""}), errChatHoldingsNone, nil},
		"a directory instead":   {clArchive(t, [3]string{name, string(tar.TypeDir), ""}), errChatHoldingsNone, nil},
		"an entry after it":     {clArchive(t, chat, [3]string{"x", string(tar.TypeReg), "b\n"}), nil, errChatHoldingsMore},
		"the chat file twice":   {clArchive(t, chat, chat), nil, errChatHoldingsMore},
		"an empty archive":      {clArchive(t), errChatHoldingsNone, nil},
		"no tar":                {strings.NewReader(strings.Repeat("x", 1024)), errNotTar, nil},
		"a tar cut in its file": {io.LimitReader(clArchive(t, chat), 513), nil, io.ErrUnexpectedEOF},
	} {
		tr, err := chatHoldingsMember(tc.archive, named)
		if !errors.Is(err, tc.open) {
			t.Errorf("%s: opened with %v; want %v", label, err, tc.open)
			continue
		}
		if err != nil {
			continue
		}
		lines, _, err := clRead(t.Context(), tr)
		if err == nil {
			err = chatHoldingsEnd(tr)
		}
		if !errors.Is(err, tc.end) || tc.end == nil && !slices.Equal(lines, []string{"a"}) {
			t.Errorf("%s: read %q, ended %v; want %v", label, lines, err, tc.end)
		}
	}
}

// A CHAT FILE'S READING NAMES WHY IT READ NONE: the caller gone, the
// reading's own words, the plain meter's bound, or no chat file, never a
// cause's bytes past its first line.
//
// Mutation: answer a cause unwrapped, or the bound as a malformed file, and a
// case goes red.
func TestAChatFilesReadingNamesWhyItReadNone(t *testing.T) {
	t.Parallel()
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if err := chatReadFailed(gone, errChatHoldingsMore); !errors.Is(err, context.Canceled) {
		t.Errorf("a gone caller's reading answers %v; want %v", err, context.Canceled)
	}
	if err := chatReadFailed(t.Context(), fmt.Errorf("wrapped: %w", errChatHoldingsMore)); !errors.Is(err, errChatHoldingsMore) ||
		err.Error() != errChatHoldingsMore.Error() {
		t.Errorf("the reading's own words answer %v; want %v", err, errChatHoldingsMore)
	}
	bound := chatReadFailed(t.Context(), fmt.Errorf("%w: %w", errNotTar, errScrubBound))
	var own *holdingsError
	if !errors.Is(bound, errScrubBound) || !errors.As(bound, &own) ||
		!strings.Contains(own.why, "past the bound a kept session unpacks to") {
		t.Errorf("the plain meter's bound answers %v; want the reading to name it", bound)
	}
	other := chatReadFailed(t.Context(), errors.New("first line\nsecond line"))
	if !errors.As(other, &own) || strings.Contains(other.Error(), "second line") {
		t.Errorf("a reader's words answer %q; want a reading's error cut to their first line", other)
	}
}

// EACH ID IS HELD ONCE ACROSS THE WHOLE FILE: an id kept twice is found in one
// block or across blocks, the zero value keeps ids, and ids kept after a check
// are checked again.
//
// Mutation: compare within a block alone, or answer false, and a case goes
// red.
func TestAnIdKeptTwiceIsFoundAcrossTheWholeFile(t *testing.T) {
	t.Parallel()
	var o onceEach
	for i := range 3*onceEachBlock + 7 {
		o.add(uint64(i))
	}
	if o.twice() {
		t.Fatalf("%d distinct ids read as one twice", 3*onceEachBlock+7)
	}
	o.add(5)
	if !o.twice() {
		t.Error("an id of the first block kept again in the last was not found")
	}
	var near onceEach
	near.add(7)
	near.add(9)
	near.add(7)
	if !near.twice() {
		t.Error("an id kept twice in one block was not found")
	}
}

// A DIGEST KEEPS ITS TEXT'S OPENING across its pieces, a new text's own after
// a reset, and refuses to answer for one longer than it keeps.
//
// Mutation: keep only the first piece's, keep the last text's past a reset,
// or answer a longer prefix, and a case goes red.
func TestADigestKeepsItsTextsOpening(t *testing.T) {
	t.Parallel()
	d := newTextDigest(nil)
	d.reset()
	d.write([]byte("<persis"))
	d.write([]byte("ted-output> and the rest"))
	d.end()
	if !d.opens("<persisted-output>") || d.opens("Output too large (") || !d.opens("") {
		t.Error("a text opening across two pieces does not read as its opening")
	}
	d.reset()
	d.write([]byte("<pe"))
	d.end()
	if d.opens("<persisted-output>") || !d.opens("<pe") {
		t.Error("a text shorter than a prefix reads as opening with it, or a reset kept the last text's")
	}
	defer func() {
		if recover() == nil {
			t.Error("a prefix longer than the opening kept was answered")
		}
	}()
	d.opens(strings.Repeat("x", textDigestHeadBytes+1))
}

// A DIGEST SCANS A STRING WHOLE: its decoded text, past the digest's piece and
// with escapes, is summed and measured to the end, and the scanner stands past
// it.
//
// Mutation: stop at a piece, or leave the sum unended, and this goes red.
func TestADigestScansAStringWhole(t *testing.T) {
	t.Parallel()
	text := `<x>` + strings.Repeat("é\n", textDigestBytes) + `"`
	raw := strings.ReplaceAll(strings.ReplaceAll(text, "\n", `\n`), `"`, `\"`)
	sc := jsonscan.New(strings.NewReader(`"` + raw + `"`))
	if k, err := sc.Next(); err != nil || k != jsonscan.String {
		t.Fatalf("no string: %v %v", k, err)
	}
	d := newTextDigest(nil)
	if err := d.scan(sc); err != nil {
		t.Fatal(err)
	}
	if d.whole != sha256.Sum256([]byte(text)) || d.n != len(text) || !d.opens("<x>é") {
		t.Errorf("a %d-byte string digested as %d bytes; want its decoded text whole", len(text), d.n)
	}
	if _, err := sc.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("the scanner stands at %v past the string; want its end", err)
	}
}

// A READING'S FLOOR IS ITS SET'S OLDEST RELEASE BY ITS NUMBERS, whatever the
// set's order and however its numbers sort as text; an unreadable member is
// none, and a set of none reads no floor.
//
// Mutation: answer the set's first member, or compare releases as text, and
// a case goes red.
func TestAReadingsFloorIsItsSetsOldestRelease(t *testing.T) {
	t.Parallel()
	for want, set := range map[string][]string{
		"0.28.4": {"0.28.4"},
		"2.3.2":  {"2.4.0", "2.3.10", "2.3.2"},
		"2.9.1":  {"2.10.0", "2.9.1"},
		"1.0.0":  {"0.9", "1.0.0", "v-x"},
		"":       {},
	} {
		if got := releaseSetFloor(set); got != want {
			t.Errorf("the floor of %q is %q; want %q", set, got, want)
		}
	}
}
