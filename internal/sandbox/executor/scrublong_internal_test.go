package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/plainjson"
)

// longValue is an engine value the cases below plant inside a long string.
const longValue = "opaque-engine-value-0f3c9a"

// base64Run is n bytes of base64 from r, with longValue planted at each of
// at, as a box's file holding the value's text at four-character boundaries
// is written by its harness's own encoding of the file.
func base64Run(r *rand.Rand, n int, at ...int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	for _, i := range at {
		copy(b[i:], longValue)
	}
	return string(b)
}

// A VALUE PAST EIGHT MEBIBYTES IS KEPT, AND SCRUBBED THROUGHOUT: a Claude Code
// transcript line holding a whole PDF's base64 twice, as its Read writes one (a
// `tool_result`'s document and the call's own result), and an OpenCode export
// whose `file` part holds an image's data URI, each string past the eight
// mebibytes the scrub once refused a session at. The engine's value planted
// inside the long string is taken out at every place, as a marker each, and
// every other byte of the string is kept, byte for byte, what the whole-text
// scrub of the string writes and the string as written but for the value.
//
// Mutation: refuse a string past the scanner's buffer, write a long string's
// pieces unscrubbed, or lose a piece at a cut, and a case goes red.
func TestAValuePastEightMebibytesIsKeptAndScrubbedThroughout(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 3))
	const size = 8<<20 + 64<<10
	places := []int{0, 4096, textWholeBytes - 8, textWholeBytes + 4, 1 << 20, size - len(longValue)}
	data := base64Run(r, size, places...)
	s := newScrubber([]string{longValue})
	scrubbed := strings.ReplaceAll(data, longValue, scrubMarker)
	if kept := keptText(s, data); kept != scrubbed {
		t.Fatal("the whole-text scrub changed more of the string than the planted value: pick another seed")
	}

	t.Run("a Claude Code line", func(t *testing.T) {
		t.Parallel()
		line := func(d string) string {
			return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1",` +
				`"content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` +
				d + `"}}]}]},"toolUseResult":{"type":"pdf","file":{"filePath":"/w/a.pdf","base64":"` + d + `"}}}`
		}
		archive := tarOf(t, map[string]string{"projects/p/s.jsonl": line(data) + "\n"})
		var out bytes.Buffer
		if err := scrubTar(bytes.NewReader(archive), reopenOf(archive), &out, s, testBounds); err != nil {
			t.Fatalf("the session was refused: %v", err)
		}
		got := untarOf(t, out.Bytes())["projects/p/s.jsonl"]
		if want := line(scrubbed) + "\n"; got != want {
			t.Errorf("the line was kept as %d bytes, %d markers; want %d bytes, the value out at each of its %d "+
				"places twice", len(got), strings.Count(got, scrubMarker), len(want), len(places))
		}
	})
	t.Run("an OpenCode export", func(t *testing.T) {
		t.Parallel()
		doc := func(d string) string {
			return `{"info":{"id":"ses_1"},"messages":[{"info":{"id":"msg_1","role":"user"},"parts":[{"type":"file",` +
				`"mime":"image/png","url":"data:image/png;base64,` + d + `"}]}]}`
		}
		archive := gzipOf(t, doc(data))
		var out bytes.Buffer
		if err := scrubDocument(bytes.NewReader(archive), reopenOf(archive), &out, s, testBounds); err != nil {
			t.Fatalf("the export was refused: %v", err)
		}
		zr, err := gzip.NewReader(&out)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		if want := doc(scrubbed); string(got) != want {
			t.Errorf("the export was kept as %d bytes, %d markers; want %d bytes, the value out at each of its "+
				"%d places", len(got), strings.Count(string(got), scrubMarker), len(want), len(places))
		}
	})
}

// A JSON STRING IS WRITTEN AS PLAINJSON WRITES IT, HOWEVER ITS TEXT IS CUT:
// every byte below a space and every escape plainjson spells, U+2028 and
// U+2029, runes of every length cut between their bytes, and bytes that begin
// no rune, alone, before a rune's continuation and at the text's end, each cut
// at every byte for a short text and at random for a long one; and the text's
// first bytes kept as written to it, and whether it ran past them.
//
// Mutation: escape a rune's first bytes before the rest arrive, write the
// held bytes at the end as they are, or spell an escape otherwise than
// plainjson does, and a text here is written otherwise.
func TestAJSONStringIsWrittenAsPlainjsonWritesItHoweverItsTextIsCut(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 4))
	atoms := []string{"a", " ", "\"", "\\", "/", "<", ">", "&", "\x7f", "\u00e9", "\u2028", "\u2029", "\U0001F600",
		"\uFFFD", "\xff", "\xe2\x82", "\xf0\x9f", "\xc3", "\x80", "\xed\xa0\x80"}
	for c := range 0x20 {
		atoms = append(atoms, string(rune(c)))
	}
	texts := []string{"", "plain", "\xe2\x82", "a\xf0\x9f\x98", strings.Join(atoms, "")}
	for range 400 {
		var b strings.Builder
		for range r.IntN(60) {
			b.WriteString(atoms[r.IntN(len(atoms))])
		}
		texts = append(texts, b.String())
	}
	for _, text := range texts {
		want, err := plainjson.Marshal(text, "")
		if err != nil {
			t.Fatal(err)
		}
		cuttings := [][]int{nil}
		for at := 1; at < len(text); at++ {
			cuttings = append(cuttings, []int{at})
		}
		for range 4 {
			cuttings = append(cuttings, randomCuts(r, len(text)))
		}
		for _, cuts := range cuttings {
			var out bytes.Buffer
			got, capture, over := writeJSONString(t, &out, text, cuts, 5)
			if got != string(want) {
				t.Fatalf("%q cut at %v: written %q, want plainjson's %q", text, cuts, got, want)
			}
			if wantCapture := text[:min(5, len(text))]; capture != wantCapture || over != (len(text) > 5) {
				t.Fatalf("%q: kept %q (past it %v), want %q", text, capture, over, wantCapture)
			}
		}
	}
}

// randomCuts is up to eight places to cut a text of n bytes at, in order.
func randomCuts(r *rand.Rand, n int) []int {
	if n < 2 {
		return nil
	}
	var cuts []int
	at := 0
	for range r.IntN(8) {
		at += 1 + r.IntN(max(1, n/4))
		if at >= n {
			break
		}
		cuts = append(cuts, at)
	}
	return cuts
}

// writeJSONString writes text through a [jsonStringWriter] in the pieces cuts
// make, and answers what it wrote, the text's first capMax bytes it kept, and
// whether the text ran past them.
func writeJSONString(t *testing.T, out *bytes.Buffer, text string, cuts []int, capMax int) (string, string,
	bool) {

	t.Helper()
	var o rewriteOut
	w := bufio.NewWriter(out)
	o.reset(w, false)
	var e jsonStringWriter
	if err := e.begin(&o, capMax); err != nil {
		t.Fatal(err)
	}
	from := 0
	for _, at := range append(cuts, len(text)) {
		if _, err := e.Write([]byte(text[from:at])); err != nil {
			t.Fatal(err)
		}
		from = at
	}
	if err := e.end(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if o.n != int64(out.Len()) {
		t.Fatalf("counted %d bytes and wrote %d", o.n, out.Len())
	}
	return out.String(), string(e.capture), e.over
}

// A REWRITE'S FRAME IS NO LARGER THAN THE FIGURE THE SCRUB'S MEMORY IS STATED
// BY: a frame for each container open, ten thousand at the scanner's depth.
//
// Mutation: add a field to the frame, and the figure is past.
func TestARewritesFrameIsWithinTheFigureTheScrubStates(t *testing.T) {
	t.Parallel()
	if size := unsafe.Sizeof(rewriteFrame{}); size > rewriteFrameBytes {
		t.Errorf("a rewrite's frame is %d bytes, past the %d scrubHeldBytes counts", size, rewriteFrameBytes)
	}
}

// THE SCRUB WRITES THE SAME, HOWEVER ITS INPUT IS CUT: the box's pipe hands
// the archive over in reads of any length, and both readings of a tar, the
// first counting and the second writing, read whatever each read hands them.
// Over a session of every shape the scrub reads a line by (a record, a torn
// line, a line of blanks, a line of one byte, a credential's key and value
// past the rewrite's buffer, a long string with values planted across it, a
// loaded-tools record that loses a definition), a `.json` file, a torn one and
// text, the archive read in random reads writes what it writes read whole.
//
// Mutation: carry a line's state wrongly across a read, or decide a line's
// kind from what one read held, and a cutting writes otherwise.
func TestTheScrubWritesTheSameHoweverItsInputIsCut(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 5))
	s := newScrubber([]string{longValue, ltServer})
	long := base64Run(r, 3*textWholeBytes, 10, textWholeBytes-3, 2*textWholeBytes+1)
	lines := []string{
		`{"type":"user","message":{"content":"` + long + `"}}`,
		`{"text":"torn ` + long[:textWholeBytes+100],
		"   ",
		"7",
		"",
		`{"input":{"` + strings.Repeat("x", textWholeBytes) + `_api_key":"` + long + `"}}`,
		ltRecord("2.1.322", "", ltEcho, ltRenamed, ltSend),
		`{"a":1} {"b":2}`,
		`{"text":"` + shapedKey + ` and password: hunter22"}`,
	}
	archive := tarOf(t, map[string]string{
		"projects/p/s.jsonl":       strings.Join(lines, "\n") + "\n" + `{"last":"no break"}`,
		"tasks/s/1.json":           `{"text":"` + long + `"}`,
		"tasks/s/2.json":           `{"text":"torn`,
		"projects/p/s/out/out.txt": "a key " + shapedKey + "\n" + long,
	})
	var whole bytes.Buffer
	if err := scrubTar(bytes.NewReader(archive), reopenOf(archive), &whole, s, testBounds); err != nil {
		t.Fatalf("the archive read whole was refused: %v", err)
	}
	for range 3 {
		cut := func() (io.ReadCloser, error) { return io.NopCloser(&cutReader{r: r, b: archive}), nil }
		first, _ := cut()
		var got bytes.Buffer
		if err := scrubTar(first, cut, &got, s, testBounds); err != nil {
			t.Fatalf("the archive read in pieces was refused: %v", err)
		}
		if !bytes.Equal(got.Bytes(), whole.Bytes()) {
			t.Fatal("the archive read in pieces was written otherwise than read whole")
		}
	}
	kept := untarOf(t, whole.Bytes())["projects/p/s.jsonl"]
	if strings.Contains(kept, longValue) || strings.Contains(kept, shapedKey) || strings.Contains(kept, "hunter22") {
		t.Error("the transcript kept a secret")
	}
}

// cutReader hands b over in reads of random lengths, one byte to a few KiB.
type cutReader struct {
	r *rand.Rand
	b []byte
}

func (c *cutReader) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), 1+c.r.IntN(4096))], c.b)
	c.b = c.b[n:]
	return n, nil
}

// A TORN LINE IS WRITTEN AS ITS TEXT, AS IT WAS, AND THE FIRST READING COUNTS
// IT BOTH WAYS: a line that is no one JSON value (cut short past the rewrite's
// buffer, so its JSON is read a long way before its grammar fails, a line of
// two values, a line of blanks) is written exactly as the whole-text scrub of
// its text writes it, never its JSON prefix respelled; the plan keeps a bit
// for each line of two bytes or more, set for one written as text, and none
// for a shorter one; and what the first reading counts is what the second
// writes ([keptLines]).
//
// Mutation: write a torn line's prefix as its rewrite did, count only the
// JSON candidate, give a short line a bit, or read a line of blanks as an
// empty JSON line, and a check goes red.
func TestATornLineIsWrittenAsItsTextAndCountedBothWays(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 6))
	s := newScrubber([]string{longValue})
	long := base64Run(r, textWholeBytes+512, 7, textWholeBytes)
	torn := `{"text":"respelled A ` + long
	lines := []string{`{"a":"b"}`, torn, "", `{"x":1} {"y":2}`, "5", " \t ", `{"b":"c"}`}
	text := strings.Join(lines, "\n") + "\n"
	plan := &tarPlan{}
	if _, err := measureLines(strings.NewReader(text), s, plan); err != nil {
		t.Fatal(err)
	}
	var bits []bool
	for i := range plan.lines.count() {
		bits = append(bits, plan.lines.list.get(i))
	}
	if want := []bool{false, true, true, true, false}; !slicesEqual(bits, want) {
		t.Errorf("the plan's bits are %v, want %v: a bit for each line of two bytes or more, set for text", bits, want)
	}
	got := strings.Split(keptLines(t, text, s), "\n")
	for i, want := range []string{`{"a":"b"}`, keptText(s, torn), "", keptText(s, `{"x":1} {"y":2}`), "5", " \t ",
		`{"b":"c"}`} {
		if got[i] != want {
			t.Errorf("line %d was kept as %.80q, want %.80q", i, got[i], want)
		}
	}
	if !strings.Contains(got[1], `A`) || strings.Contains(got[1], longValue) {
		t.Errorf("the torn line was not kept as its scrubbed text: %.120q", got[1])
	}
}

// slicesEqual reports whether a and b hold the same bits.
func slicesEqual(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// THE FIRST READING DECIDES A LINE'S CUT, AND THE SECOND APPLIES IT: the plan
// names the line that loses a loaded tool definition, by how many lines with
// a bit lie before it, the list it loses it from as the line's containers
// count it, how many elements that list holds and a bit for each, set for one
// cut, of the list JSON.parse reads; and the second reading writes the line
// less the cut ones and their commas, the size the first counted.
//
// Mutation: keep a bit for the list read past, count the cut's commas
// wrongly, or count a cut's line from the transcript's start rather than from
// the cut before it, and the plan or the size is otherwise.
func TestTheFirstReadingDecidesALinesCutAndTheSecondAppliesIt(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{ltServer})
	first := `{"a":1}`
	record := ltRecord("2.1.310", "", ltEcho, ltRenamed, ltSend, ltRenamed)
	twice := strings.Replace(record, `"entries":[`, `"entries":[`+ltRenamed+`],"entries":[`, 1)
	text := first + "\n" + twice + "\n" + first + "\n" + twice + "\n"
	plan := &tarPlan{}
	if _, err := measureLines(strings.NewReader(text), s, plan); err != nil {
		t.Fatal(err)
	}
	// Two cuts, of lines 1 and 3: each a line after the one before it.
	// The line's containers: the line, the attachment, the list read past
	// and the definition in it, four of its own, and then the list JSON.parse
	// reads, the eighth.
	for i, wantGap := range []uint64{1, 1} {
		gap, _ := plan.cuts.takeNumber()
		list, _ := plan.cuts.takeNumber()
		n, ok := plan.cuts.takeNumber()
		if !ok || gap != wantGap || list != 8 || n != 4 {
			t.Fatalf("cut %d: %d lines after the last, container %d, %d elements; want %d, 8 and 4", i, gap,
				list, n, wantGap)
		}
		var bits []bool
		for range n {
			bit, _ := plan.cuts.take()
			bits = append(bits, bit)
		}
		if want := []bool{false, true, false, true}; !slicesEqual(bits, want) {
			t.Errorf("cut %d's bits are %v, want %v", i, bits, want)
		}
	}
	if !plan.cuts.done() {
		t.Errorf("the plan holds %d bits past its two cuts", plan.cuts.count()-plan.cuts.read)
	}
	wantTwice := strings.Replace(ltRecord("2.1.310", "", ltEcho, ltSend), `"entries":[`,
		`"entries":[`+ltRenamed+`],"entries":[`, 1)
	kept := first + "\n" + strings.ReplaceAll(wantTwice, ltServer, scrubMarker) + "\n"
	if got, want := keptLines(t, text, s), kept+kept; got != want {
		t.Errorf("kept\n%s\nwant\n%s", got, want)
	}
}

// A CREDENTIAL'S VALUE IS ITS MARKER, HOWEVER ITS KEY AND ITS VALUE ARE CUT: a
// key whose credential's words straddle the edge of what the rewrite reads of
// a name at once, a key longer than that by far, a value of any length and a
// number, each written as the marker, and an empty value as the empty string.
//
// Mutation: read a key's credential from its last piece alone, or write the
// value's first piece rather than the marker, and a case goes red.
func TestACredentialsValueIsItsMarkerHoweverItsKeyAndValueAreCut(t *testing.T) {
	t.Parallel()
	s := newScrubber(nil)
	long := strings.Repeat("v", textWholeBytes+100)
	marker, _ := plainjson.Marshal(credentialMarker, "")
	for _, pad := range []int{0, textWholeBytes - 9, textWholeBytes - 4, textWholeBytes - 1, textWholeBytes,
		textWholeBytes + 3, 3 * textWholeBytes} {
		key := strings.Repeat("x", pad) + "_api_key"
		for name, tc := range map[string]struct{ value, want string }{
			"a short value": {`"short-secret"`, string(marker)},
			"a long value":  {`"` + long + `"`, string(marker)},
			"a number":      {`73519046284`, string(marker)},
			"an empty one":  {`""`, `""`},
		} {
			got := rewriteLine(t, `{"`+key+`":`+tc.value+`}`, s)
			if want := `{"` + key + `":` + tc.want + `}`; got != want {
				t.Errorf("a key of %d bytes, %s: kept %.80q, want %.80q", len(key), name, got, want)
			}
		}
	}
}

// AN EXPORT'S FIRST READING WRITES NOTHING: an export the scrub refuses (torn,
// past the plaintext's bound, two documents) reaches nothing the take writes
// to, the pipe, the checksum, the uploader or the meter, with not a byte, and
// is never opened again; one read whole is opened a second time and written;
// and an export that changed between the readings is refused.
//
// Mutation: write the first reading, or write the second without holding it
// to the first's length, and a case goes red.
func TestAnExportsFirstReadingWritesNothing(t *testing.T) {
	t.Parallel()
	s := newScrubber(nil)
	sound := `{"info":{"id":"ses_1"},"messages":[{"text":"` + strings.Repeat("a", 3*textWholeBytes) + `"}]}`
	for name, doc := range map[string]string{
		"a torn export":        sound[:len(sound)-3],
		"two documents":        sound + sound,
		"past the plain bound": `{"text":"` + strings.Repeat("a", 2<<20) + `"}`,
		"no JSON":              `{"text": nope}`,
	} {
		archive := gzipOf(t, doc)
		opened := 0
		reopen := func() (io.ReadCloser, error) {
			opened++
			return io.NopCloser(bytes.NewReader(archive)), nil
		}
		dst := &counted{}
		err := scrubDocument(bytes.NewReader(archive), reopen, dst, s, scrubBounds{plain: 1 << 20})
		if err == nil || dst.n != 0 || opened != 0 {
			t.Errorf("%s: %v, %d bytes written, opened again %d times; want it refused, nothing written, "+
				"never opened again", name, err, dst.n, opened)
		}
	}
	archive := gzipOf(t, sound)
	opened := 0
	reopen := func() (io.ReadCloser, error) {
		opened++
		return io.NopCloser(bytes.NewReader(archive)), nil
	}
	var out bytes.Buffer
	if err := scrubDocument(bytes.NewReader(archive), reopen, &out, s, testBounds); err != nil || opened != 1 ||
		out.Len() == 0 {
		t.Errorf("a sound export: %v, opened again %d times, %d bytes written", err, opened, out.Len())
	}
	changed := gzipOf(t, strings.Replace(sound, `"ses_1"`, `"ses_12"`, 1))
	if err := scrubDocument(bytes.NewReader(archive), reopenOf(changed), io.Discard, s,
		testBounds); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("an export that changed between the readings: %v, want it refused as changed", err)
	}
}

// A TEXT THE REDACTION REFUSES REFUSES THE SESSION, AND SAYS WHY, AND A WRITE
// THAT FAILED IS NO SUCH TEXT: what the redaction's stream answered of its own
// is read off the take's error, wrapped as errRedaction, and the session is
// refused rather than kept with that text; while an error of the writer the
// scrub writes onto (the take's pipe its reader closed, a tar writer that
// failed), which the stream answers at its close as well, is handed on as it
// came, never labelled a text the redaction could not redact.
//
// Mutation: drop the stream's error at its close, wrap the writer's error as
// errRedaction, or hand a refusal on unwrapped, and a case goes red.
func TestATextTheRedactionRefusesRefusesTheSessionAndSaysWhy(t *testing.T) {
	t.Parallel()
	failed := errors.New("the take's pipe was closed by its reader")
	refused := errors.New("the redaction could not decide")
	for name, tc := range map[string]struct {
		err, wrote  error
		refusal, as bool
	}{
		"nothing":                     {},
		"the writer's error":          {err: failed, wrote: failed, as: true},
		"the writer's error, wrapped": {err: fmt.Errorf("emitting: %w", failed), wrote: failed, as: true},
		"a refusal of its own":        {err: refused, refusal: true, as: true},
		"a refusal after a write":     {err: refused, wrote: failed, refusal: true, as: true},
	} {
		got := redactionRefusal(tc.err, tc.wrote)
		switch {
		case (got != nil) != (tc.err != nil):
			t.Errorf("%s: answered %v for %v", name, got, tc.err)
		case errors.Is(got, errRedaction) != tc.refusal:
			t.Errorf("%s: answered %v; a refusal of the redaction's own is errRedaction, and only that", name,
				got)
		case tc.as && !errors.Is(got, tc.err) && !strings.Contains(got.Error(), tc.err.Error()):
			t.Errorf("%s: answered %v, which does not say %v", name, got, tc.err)
		}
	}
	text := newScrubber(nil).text(refusingWriter{failed})
	if _, err := text.Write([]byte("some text")); err != nil {
		t.Fatal(err)
	}
	if err := text.Close(); !errors.Is(err, failed) || errors.Is(err, errRedaction) {
		t.Errorf("a text whose writer failed closed on %v; want the writer's error, as it came", err)
	}
}

// refusingWriter fails every write with err.
type refusingWriter struct{ err error }

func (f refusingWriter) Write([]byte) (int, error) { return 0, f.err }

// THE SCRUB HOLDS NO MORE THAN THE FIGURE ITS DOC STATES, AND NOTHING THAT
// GROWS WITH A STRING: a session of one string near a reading's plaintext
// bound (a test's bound of 32 MiB, scrubHeldBytes's figure at it), scrubbed
// both readings, holds at most that figure between two reads of its archive,
// and a session of a string four times as long holds no more than a little
// over what the shorter one held. Read off the heap in use after a collection
// at every sixty-fourth read, against the heap in use before the scrub began,
// the archive already in it: what the scrub carries from one read to the next,
// which is what it holds, while what it allocates and lets go of within a read
// is not. Not in parallel: another case's heap would be read as the scrub's.
//
// The figure itself is measured at the real bound, 256 MiB, by
// BenchmarkTheScrubAtThePlaintextBound.
//
// Mutation: read a long string whole rather than through the streams, or hold
// a transcript's line to learn what it is, and the longer string holds it.
func TestTheScrubHoldsNoMoreThanItsStatedFigure(t *testing.T) {
	const plain = 32 << 20
	s := newScrubber([]string{longValue})
	held := func(n int) int64 {
		archive := func() []byte {
			r := rand.New(rand.NewPCG(1170, uint64(n)))
			line := `{"type":"user","message":{"content":"` + base64Run(r, n, 100, n/2) + `"}}` + "\n"
			return tarOf(t, map[string]string{"projects/p/s.jsonl": line})
		}()
		var most uint64
		reopen := func() (io.ReadCloser, error) { return io.NopCloser(&heapSampler{b: archive, most: &most}), nil }
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		most = before.HeapAlloc
		first := &heapSampler{b: archive, most: &most}
		if err := scrubTar(first, reopen, io.Discard, s, scrubBounds{plain: plain}); err != nil {
			t.Fatalf("a session of one string of %d bytes was refused: %v", n, err)
		}
		return int64(most) - int64(before.HeapAlloc)
	}
	small, large := held(6<<20), held(24<<20)
	figure := scrubHeldBytes(plain, len(jsonEscaped(longValue)))
	t.Logf("held %d KiB for a string of 6 MiB, %d KiB for one of 24 MiB; the figure at a %d MiB reading is %d KiB",
		small>>10, large>>10, plain>>20, figure>>10)
	if large > figure {
		t.Errorf("the scrub held %d bytes, past the %d its doc states at this bound", large, figure)
	}
	if large-small > 2<<20 {
		t.Errorf("a string four times as long held %d bytes more: the scrub holds what grows with the string",
			large-small)
	}
}

// heapSampler hands b over in reads, and reads the heap in use after a
// collection at every sixty-fourth, keeping the most in most.
type heapSampler struct {
	b     []byte
	reads int
	most  *uint64
}

func (h *heapSampler) Read(p []byte) (int, error) {
	if len(h.b) == 0 {
		return 0, io.EOF
	}
	if h.reads%64 == 0 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		*h.most = max(*h.most, m.HeapAlloc)
	}
	h.reads++
	n := copy(p, h.b)
	h.b = h.b[n:]
	return n, nil
}

// untarLines is the kept transcript of archive, member name, split into its
// lines.
func untarLines(t *testing.T, archive []byte, name string) []string {
	t.Helper()
	return strings.Split(untarOf(t, archive)[name], "\n")
}

// A KEPT LINE IS STILL ONE JSON VALUE OR THE TEXT IT WAS: every line of a
// transcript the scrub keeps from a random session is JSON where its own line
// was, and else the whole-text scrub of its own line.
//
// Mutation: write a torn line's JSON prefix as its rewrite did, and a torn
// line is kept as neither.
func TestAKeptLineIsJSONWhereItWasAndItsTextWhereItWasNot(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 7))
	s := newScrubber([]string{longValue})
	var lines []string
	for range 200 {
		body := `{"text":"` + base64Run(r, r.IntN(3*textWholeBytes), 0) + `","n":` + strings.Repeat("1", 1+r.IntN(4)) + `}`
		if r.IntN(3) == 0 {
			body = body[:r.IntN(len(body))]
		}
		lines = append(lines, body)
	}
	archive := tarOf(t, map[string]string{"projects/p/s.jsonl": strings.Join(lines, "\n") + "\n"})
	var out bytes.Buffer
	if err := scrubTar(bytes.NewReader(archive), reopenOf(archive), &out, s, testBounds); err != nil {
		t.Fatal(err)
	}
	kept := untarLines(t, out.Bytes(), "projects/p/s.jsonl")
	for i, line := range lines {
		isJSON := len(line) >= 2 && json.Valid([]byte(line))
		switch {
		case isJSON && !json.Valid([]byte(kept[i])):
			t.Errorf("line %d was JSON and is kept as %.60q", i, kept[i])
		case !isJSON && kept[i] != keptText(s, line):
			t.Errorf("line %d was no JSON and is kept as %.60q, not its text", i, kept[i])
		}
	}
}

// THE PLAN HOLDS AT MOST A SIXTEENTH OF THE PLAINTEXT, HOWEVER THE BOX SHAPES
// ITS SESSION: between the tar's two readings the scrub holds its plan and
// nothing else of the archive, and the plan is a bit for every two bytes of
// plaintext at most, beside the blocks its streams are filling
// ([planBlockBytes]); never a block, or a record, for each line it cuts or
// each transcript it reads. Two shapes a box's code can write at will: a
// transcript of short lines, each a loaded-tools record that loses its one
// definition (the line holds a marker), and a tar of many small transcripts.
// Read off the heap in use after a collection at the moment the second
// reading opens the archive, against the heap in use before the scrub began.
// Not in parallel: another case's heap would be read as the scrub's.
//
// Mutation: keep a block of bits for each cut line, or a plan of its own for
// each transcript, and the plan holds kibibytes a line or a member, far past
// the figure.
func TestThePlanHoldsAtMostASixteenthOfThePlaintext(t *testing.T) {
	s := newScrubber([]string{longValue})
	cut := `{"attachment":{"type":"deferred_tools_record","entries":[1]},"m":"[REDACTED:x]"}` + "\n"
	for name, members := range map[string][]tarMember{
		"a transcript of 20,000 lines that each lose a definition": {{h: tar.Header{
			Name: "projects/p/s.jsonl", Typeflag: tar.TypeReg, Mode: 0o600}, body: strings.Repeat(cut, 20_000)}},
		"20,000 transcripts of one short line": func() []tarMember {
			var m []tarMember
			for i := range 20_000 {
				m = append(m, tarMember{h: tar.Header{Name: fmt.Sprintf("projects/p/%05d.jsonl", i),
					Typeflag: tar.TypeReg, Mode: 0o600}, body: "ab\n"})
			}
			return m
		}(),
	} {
		archive := tarOfMembers(t, members)
		plain := plainOf(t, archive)
		held := heldBetweenReadings(t, archive, s)
		figure := plain/16 + planBlockBytes
		t.Logf("%s: %d KiB of plaintext from a %d KiB archive; the plan held %d KiB, its figure %d KiB", name,
			plain>>10, len(archive)>>10, held>>10, figure>>10)
		// A little room for the runtime's own allocations between two
		// readings of the heap, far under a kibibyte a line or a member.
		if held > figure+64<<10 {
			t.Errorf("%s: the plan held %d bytes between the readings, past the %d a sixteenth of the "+
				"plaintext and its blocks come to", name, held, figure)
		}
	}
}

// heldBetweenReadings scrubs archive and answers the heap in use, after a
// collection, at the moment the second reading opens the archive, less the
// heap in use before the scrub began: what the first reading hands the second.
func heldBetweenReadings(t *testing.T, archive []byte, s scrubber) int64 {
	t.Helper()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var before, at runtime.MemStats
	reopen := func() (io.ReadCloser, error) {
		runtime.GC()
		runtime.ReadMemStats(&at)
		return io.NopCloser(bytes.NewReader(archive)), nil
	}
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := scrubTar(bytes.NewReader(archive), reopen, io.Discard, s, testBounds); err != nil {
		t.Fatalf("the session was refused: %v", err)
	}
	return int64(at.HeapAlloc) - int64(before.HeapAlloc)
}

// plainOf is how many bytes of plaintext a gzipped archive holds.
func plainOf(t *testing.T, archive []byte) int64 {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, zr)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A LINE'S READING THAT IS NOT WRITTEN NEVER REFUSES IT: of a line's two
// readings, the first reading keeps its text where the line is short or no
// JSON value and its JSON otherwise, and only the kept reading's failure
// refuses the session. A JSON line whose text the redaction could not finish
// is kept as its JSON, whose strings the redaction reads on their own, as a
// `.json` file read whole as a document leaves its text's failure unread.
//
// Mutation: answer the text's failure before looking at which reading is
// kept, or keep a line of a reading that failed, and a case goes red.
func TestALinesReadingThatIsNotWrittenNeverRefusesIt(t *testing.T) {
	t.Parallel()
	textFailed := errors.New("the text could not be redacted")
	jsonFailed := errors.New("a string could not be redacted")
	torn := &jsonscan.Error{Fault: jsonscan.FaultCut}
	for name, tc := range map[string]struct {
		short            bool
		jsonErr, textErr error
		text             bool
		err              error
	}{
		"a JSON line":                      {},
		"a JSON line whose text failed":    {textErr: textFailed},
		"a JSON line whose string failed":  {jsonErr: jsonFailed, err: jsonFailed},
		"a torn line":                      {jsonErr: torn, text: true},
		"a torn line whose text failed":    {jsonErr: torn, textErr: textFailed, text: true, err: textFailed},
		"a short line":                     {short: true, text: true},
		"a short line whose text failed":   {short: true, textErr: textFailed, text: true, err: textFailed},
		"a JSON line both of whose failed": {jsonErr: jsonFailed, textErr: textFailed, err: jsonFailed},
	} {
		text, err := lineKeptAs(tc.short, tc.jsonErr, tc.textErr)
		if !errors.Is(err, tc.err) || err == nil && text != tc.text {
			t.Errorf("%s: kept as text %v, refused on %v; want text %v, refused on %v", name, text, err,
				tc.text, tc.err)
		}
	}
}

// A DOCUMENT OF BLANKS ALONE IS KEPT EMPTY, as the scrub kept one when it read
// documents on encoding/json: a `.json` file of blanks, or of nothing, is
// kept as an empty file, and an export of them is kept, empty, rather than
// refused as no JSON; while a transcript's line of blanks is kept as its
// blanks, as every line that is no JSON value is kept as its text.
//
// Mutation: read a document of blanks as no JSON, and the file is kept as
// its blanks and the export refused.
func TestADocumentOfBlanksAloneIsKeptEmpty(t *testing.T) {
	t.Parallel()
	s := newScrubber(nil)
	for _, blanks := range []string{"", " ", "  \n\t\r\n"} {
		archive := tarOf(t, map[string]string{"tasks/s/1.json": blanks, "projects/p/s.jsonl": "{}\n" + blanks + "\n"})
		var out bytes.Buffer
		if err := scrubTar(bytes.NewReader(archive), reopenOf(archive), &out, s, testBounds); err != nil {
			t.Fatalf("%q: %v", blanks, err)
		}
		kept := untarOf(t, out.Bytes())
		if task, ok := kept["tasks/s/1.json"]; !ok || task != "" {
			t.Errorf("a .json file of %q was kept as %q (kept %v), want it empty", blanks, task, ok)
		}
		if lines, want := kept["projects/p/s.jsonl"], "{}\n"+blanks+"\n"; lines != want {
			t.Errorf("a transcript's line of %q was kept as %q, want %q", blanks, lines, want)
		}
		export := gzipOf(t, blanks)
		out.Reset()
		if err := scrubDocument(bytes.NewReader(export), reopenOf(export), &out, s, testBounds); err != nil {
			t.Errorf("an export of %q was refused: %v", blanks, err)
			continue
		}
		zr, err := gzip.NewReader(&out)
		if err != nil {
			t.Fatal(err)
		}
		if body, err := io.ReadAll(zr); err != nil || len(body) != 0 {
			t.Errorf("an export of %q was kept as %q (%v), want it empty", blanks, body, err)
		}
	}
}

// A PLAN'S BITS ARE READ BACK AS THEY WERE WRITTEN: bits, bytes and numbers of
// every width, the largest a 64-bit size holds among them, read in the order
// they were written, and the stream known to be read whole; and a list's bits
// moved into it arrive in order and leave the list empty, its blocks let go.
//
// Mutation: write a number's digits least first, or move a list's bits but
// its last, and a value is read back otherwise.
func TestAPlansBitsAreReadBackAsTheyWereWritten(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 8))
	var p planBits
	var numbers []uint64
	for range 2000 {
		n := r.Uint64() >> r.IntN(64)
		numbers = append(numbers, n)
	}
	numbers = append(numbers, 0, 1, 2, 1<<62, 1<<63-1)
	var list bitList
	var listed []bool
	for range 3*bitBlockWords*64 + 17 {
		v := r.IntN(3) == 0
		list.add(v)
		listed = append(listed, v)
	}
	for i, n := range numbers {
		p.putNumber(n)
		p.putByte(byte(i))
		p.put(i%2 == 0)
	}
	list.moveTo(&p)
	if list.count() != 0 || len(list.blocks) != 0 {
		t.Errorf("the list holds %d bits in %d blocks after its move, want none", list.count(), len(list.blocks))
	}
	for i, want := range numbers {
		n, ok := p.takeNumber()
		c, okc := p.takeByte()
		v, okv := p.take()
		if !ok || !okc || !okv || n != want || c != byte(i) || v != (i%2 == 0) {
			t.Fatalf("value %d read back as %d, %d, %v; want %d, %d, %v", i, n, c, v, want, byte(i), i%2 == 0)
		}
	}
	for i, want := range listed {
		if v, ok := p.take(); !ok || v != want {
			t.Fatalf("the moved list's bit %d read back as %v (%v), want %v", i, v, ok, want)
		}
	}
	if _, ok := p.take(); ok || !p.done() {
		t.Error("the stream reads past what was written")
	}
}
