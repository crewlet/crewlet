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
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
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

// A TORN LINE IS WRITTEN AS ITS TEXT, AS IT WAS: a line that is no one JSON
// value (cut short past the rewrite's buffer, so its JSON is read a long way
// before its grammar fails, a line of two values, a line of blanks) is
// written exactly as the whole-text scrub of its text writes it, never its
// JSON prefix respelled, whether the reading reads it as JSON alone first and
// again as text ([keptLines]) or both ways at once; and what the first reading
// counts is what the second writes.
//
// Mutation: spool a torn line's prefix as its rewrite did, count a line known
// to be text as its JSON, or read a line of blanks as an empty JSON line, and a
// check goes red.
func TestATornLineIsWrittenAsItsText(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 6))
	s := newScrubber([]string{longValue})
	long := base64Run(r, textWholeBytes+512, 7, textWholeBytes)
	torn := `{"text":"respelled A ` + long
	lines := []string{`{"a":"b"}`, torn, "", `{"x":1} {"y":2}`, "5", " \t ", `{"b":"c"}`}
	text := strings.Join(lines, "\n") + "\n"
	plan := spooledPlan(t)
	plan.bothFrom = 1
	size, err := measureLines(strings.NewReader(text), s, plan)
	if err != nil {
		t.Fatal(err)
	}
	both := copiedSpool(t, plan, size)
	for reading, kept := range map[string]string{"as JSON alone first": keptLines(t, text, s), "both ways": both} {
		got := strings.Split(kept, "\n")
		for i, want := range []string{`{"a":"b"}`, keptText(s, torn), "", keptText(s, `{"x":1} {"y":2}`), "5",
			" \t ", `{"b":"c"}`} {
			if got[i] != want {
				t.Errorf("read %s, line %d was kept as %.80q, want %.80q", reading, i, got[i], want)
			}
		}
		if !strings.Contains(got[1], `A`) || strings.Contains(got[1], longValue) {
			t.Errorf("read %s, the torn line was not kept as its scrubbed text: %.120q", reading, got[1])
		}
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
// names the line that loses a loaded tool definition, by where the spool holds
// it past the edit before, the list it loses it from as the line's containers
// count it, how many elements that list holds, the bytes the cut takes and a
// bit for each element, set for one cut, of the list JSON.parse reads; and the
// second reading copies the line less the cut ones and their commas, the size
// the first counted.
//
// Mutation: keep a bit for the list read past, count the cut's commas
// wrongly, or place a cut's line from the spool's start rather than from the
// edit before it, and the plan or the size is otherwise.
func TestTheFirstReadingDecidesALinesCutAndTheSecondAppliesIt(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{ltServer})
	first := `{"a":1}`
	record := ltRecord("2.1.310", "", ltEcho, ltRenamed, ltSend, ltRenamed)
	twice := strings.Replace(record, `"entries":[`, `"entries":[`+ltRenamed+`],"entries":[`, 1)
	text := first + "\n" + twice + "\n" + first + "\n" + twice + "\n"
	plan := spooledPlan(t)
	size, err := measureLines(strings.NewReader(text), s, plan)
	if err != nil {
		t.Fatal(err)
	}
	wantTwice := strings.Replace(ltRecord("2.1.310", "", ltEcho, ltSend), `"entries":[`,
		`"entries":[`+ltRenamed+`],"entries":[`, 1)
	spooled := strings.ReplaceAll(twice, ltServer, scrubMarker)
	keptTwice := strings.ReplaceAll(wantTwice, ltServer, scrubMarker)
	// Two cuts, of lines 1 and 3: the first past the first line, the second
	// past the line it cut and the first line again. The line's containers:
	// the line, the attachment, the list read past and the definition in it,
	// four of its own, and then the list JSON.parse reads, the eighth.
	for i, wantGap := range []uint64{uint64(len(first) + 1), uint64(len(spooled) + 1 + len(first) + 1)} {
		gap, _ := plan.edits.takeNumber()
		list, _ := plan.edits.takeNumber()
		n, _ := plan.edits.takeNumber()
		taken, ok := plan.edits.takeNumber()
		if !ok || gap != wantGap || list != 8 || n != 4 || taken != uint64(len(spooled)-len(keptTwice)) {
			t.Fatalf("cut %d: %d bytes past the cut before, container %d, %d elements, %d bytes taken; want "+
				"%d bytes past, 8, 4 and %d", i, gap, list, n, taken, wantGap, len(spooled)-len(keptTwice))
		}
		var bits []bool
		for range n {
			bit, _ := plan.edits.take()
			bits = append(bits, bit)
		}
		if want := []bool{false, true, false, true}; !slicesEqual(bits, want) {
			t.Errorf("cut %d's bits are %v, want %v", i, bits, want)
		}
	}
	if !plan.edits.done() {
		t.Errorf("the plan holds %d bits past its two cuts", plan.edits.count()-plan.edits.read)
	}
	plan.edits.read = 0
	kept := first + "\n" + keptTwice + "\n"
	if got, want := copiedSpool(t, plan, size), kept+kept; got != want {
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

// AN EXPORT'S FIRST READING WRITES NOTHING THE TAKE WRITES TO: an export the
// scrub refuses (torn, past the plaintext's bound, two documents) reaches
// nothing the take writes to, the pipe, the checksum, the uploader or the
// meter, with not a byte, and is never opened again; one read whole is written
// from its spool, never opened again either.
//
// Mutation: write the first reading onto the take's writer, or read the box
// again for the second, and a case goes red.
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
	if err := scrubDocument(bytes.NewReader(archive), reopen, &out, s, testBounds); err != nil || opened != 0 ||
		out.Len() == 0 {
		t.Errorf("a sound export: %v, opened again %d times, %d bytes written", err, opened, out.Len())
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

// A TEXT THE REDACTION DECIDED TOO EARLY REFUSES THE SESSION, NAMING WHY:
// a string whose text is a key's armour and, after it, password's keys and a
// run longer than the redaction stream's hold makes the stream decide the
// armour's window before the keys' claims are known, and refuse
// (redact.ErrDecidedEarly). Whether it says so at its close or at a write, in
// a Claude Code line, a tool's output kept as text or an OpenCode export, the
// take is refused as a text the redaction could not redact, the stream's
// reason read off its error, and the first reading, which refused, wrote
// nothing.
//
// Mutation: hand a write's refusal on unwrapped ([textScrub.Write]), and every
// case at a write answers the stream's error alone, no errRedaction.
func TestATextTheRedactionDecidedTooEarlyRefusesTheSessionNamingWhy(t *testing.T) {
	t.Parallel()
	s := newScrubber(nil)
	refused := func(t *testing.T, err error, out *bytes.Buffer) {
		t.Helper()
		switch {
		case !errors.Is(err, errRedaction):
			t.Errorf("the take answered %v; want it refused as a text the redaction could not redact", err)
		case !errors.Is(err, redact.ErrDecidedEarly):
			t.Errorf("the take answered %v, which does not say the redaction decided too early", err)
		case out.Len() != 0:
			t.Errorf("the refused take wrote %d bytes", out.Len())
		}
	}
	// The stream finds the miss once the password's keys' claims come, the
	// stream's hold past the run's end: a short tail ends the text first, so
	// Close says so, and a tail longer than the hold is still being written.
	for when, tail := range map[string]int{"at its close": 256 << 10, "at a write": 2 << 20} {
		text := "k: -----BEGIN RSA PRIVATE KEY-----\n" +
			"MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n\n" +
			strings.Repeat("pwd:\n", 12000) + "sk-" + strings.Repeat("Ab0_-x9Z", 1<<20) + "\n" + strings.Repeat("ok\n", tail)
		encoded, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		t.Run("a Claude Code line, "+when, func(t *testing.T) {
			t.Parallel()
			line := `{"type":"user","message":{"role":"user","content":` + string(encoded) + `}}` + "\n"
			archive := tarOf(t, map[string]string{"projects/p/s.jsonl": line})
			var out bytes.Buffer
			refused(t, scrubTar(bytes.NewReader(archive), reopenOf(archive), &out, s, testBounds), &out)
		})
		t.Run("a tool's output kept as text, "+when, func(t *testing.T) {
			t.Parallel()
			archive := tarOf(t, map[string]string{"projects/p/s/tool-results/out.txt": text})
			var out bytes.Buffer
			refused(t, scrubTar(bytes.NewReader(archive), reopenOf(archive), &out, s, testBounds), &out)
		})
		t.Run("an OpenCode export, "+when, func(t *testing.T) {
			t.Parallel()
			doc := `{"info":{"id":"ses_1"},"messages":[{"info":{"id":"msg_1","role":"user"},"parts":[{"type":"text",` +
				`"text":` + string(encoded) + `}]}]}`
			archive := gzipOf(t, doc)
			var out bytes.Buffer
			refused(t, scrubDocument(bytes.NewReader(archive), reopenOf(archive), &out, s, testBounds), &out)
		})
	}
}

// refusingWriter fails every write with err.
type refusingWriter struct{ err error }

func (f refusingWriter) Write([]byte) (int, error) { return 0, f.err }

var _ = holdsScrubTerm("streams", "TestTheScrubHoldsNoMoreThanItsStatedFigure")

// THE SCRUB HOLDS NO MORE THAN THE FIGURE ITS DOC STATES: at a test's
// plaintext bound, the shape nearest it, a password-dense string read both
// ways, holds with what one GC step hides at most scrubHeldBytes, and one
// redaction stream over the string's start at most the "streams" term's
// share. Its one job is the whole above the miss: each term's membership is
// the table's ([TestEveryScrubTermIsHeldByATestOfItsOwn]) and its value its
// own test's. Under the miss, so no sampler holds them: the pieces, encoders,
// rewrites, spool and plan blocks. In live bytes ([atLiveBytes]), off a
// child's GC trace under GODEBUG=gcstoptheworld=1,gctrace=1.
//
// Mutation: count the streams' term at 8 MiB a stream.
func TestTheScrubHoldsNoMoreThanItsStatedFigure(t *testing.T) {
	if os.Getenv(liveBytesChild) != "" {
		scrubBoundShapes(t)
		return
	}
	held := runLiveBytesChild(t)
	for _, sh := range boundShapes() {
		h, ok := held[sh.name]
		if !ok {
			t.Fatalf("the child read no live heap for %s", sh.name)
		}
		figure := sh.figure(t)
		// THE MISS: between two marks the heap grows by at most liveBytesGOGC
		// percent of what the last one found, at most the heap before and the
		// figure on it; a peak that rises and falls inside that step is never
		// marked.
		miss := (h.before + figure) * liveBytesGOGC / 100
		t.Logf("%s held %d KiB, %d KiB unseen at most, of a figure of %d KiB", sh.name, h.high>>10, miss>>10,
			figure>>10)
		if h.high+miss > figure {
			t.Errorf("%s held %d bytes, and %d more can rise unseen, past the %d its doc states", sh.name, h.high,
				miss, figure)
		}
	}
}

// boundShape is a reading [TestTheScrubHoldsNoMoreThanItsStatedFigure] takes
// in its child, and the figure it is held to: input builds what the reading
// reads, outside it, and hands back the reading.
type boundShape struct {
	name   string
	figure func(t testing.TB) int64
	input  func(t *testing.T) func()
}

// boundShapes are the readings: a password-dense string read both ways
// scrubbed at liveBytesPlain, the shape nearest scrubHeldBytes, and one
// redaction stream over the first 12 MiB of it, past a stream's windows.
func boundShapes() []boundShape {
	body := liveBytesPlain - 64<<10
	dense := func(t *testing.T) string {
		var b strings.Builder
		b.WriteString("a line of text\n")
		if err := benchOneString(&b, body-64, "pwd=x "); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	scrubbed := func(text func(t *testing.T) string) func(t *testing.T) func() {
		return func(t *testing.T) func() {
			archive := tarOf(t, map[string]string{benchTranscript: text(t)})
			return func() {
				if err := scrubTar(bytes.NewReader(archive), reopenOf(archive), io.Discard,
					newScrubber([]string{longValue}), scrubBounds{plain: liveBytesPlain}); err != nil {
					t.Fatalf("the scrub refused the session: %v", err)
				}
			}
		}
	}
	whole := func(testing.TB) int64 { return scrubHeldBytes(liveBytesPlain, len(jsonEscaped(longValue))) }
	stream := func(t *testing.T) func() {
		text := dense(t)[:12<<20]
		return func() {
			s := redact.NewStream(io.Discard)
			for at := 0; at < len(text); at += 32 << 10 {
				if _, err := io.WriteString(s, text[at:min(len(text), at+32<<10)]); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	return []boundShape{
		{"a line of text, then a password-dense string both ways", whole, scrubbed(dense)},
		{"one redaction stream over a password-dense string", func(t testing.TB) int64 {
			return termFigure(t, "streams", liveBytesPlain, 0) / 2
		}, stream},
	}
}

// scrubBoundShapes scrubs each of [boundShapes] in the child, marking on
// stdout, apart from the GC trace on stderr, the live heap before each and
// the GC cycles it began and ended after.
func scrubBoundShapes(t *testing.T) {
	for _, want := range []string{"gcstoptheworld=1", "gctrace=1"} {
		if !strings.Contains(os.Getenv("GODEBUG"), want) {
			t.Fatalf("the child reads live bytes only under GODEBUG naming %s, which its env does not", want)
		}
	}
	defer debug.SetGCPercent(debug.SetGCPercent(liveBytesGOGC))
	for _, sh := range boundShapes() {
		reading := sh.input(t)
		before := liveNow()
		fmt.Printf("%s %d %d %s\n", liveBytesMark, before, gcCycles(), sh.name)
		reading()
		fmt.Printf("%s %d\n", liveBytesMark, gcCycles())
	}
}

// liveBytesChild marks the child [TestTheScrubHoldsNoMoreThanItsStatedFigure]
// runs itself in, and liveBytesMark its lines on stdout;
// liveBytesGOGC is the child's GC percent, the finest a step of heap growth
// between two marks can be; liveBytesPlain is the shapes' plaintext bound.
// liveBytesChildTimeout is the child's own hang detector: 25 minutes, 1.9
// times its slowest measured run (775 s under -race on a two-core slot, where
// CI's four-vCPU runners took 520 s for twice the readings), and under make
// test's 45, so a child that hangs fails with its own goroutine dump.
const liveBytesChildTimeout = 25 * time.Minute

const (
	liveBytesChild = "CREWLET_TEST_LIVE_BYTES_CHILD"
	liveBytesMark  = "live-bytes:"
	liveBytesGOGC  = 1
	liveBytesPlain = 32 << 20
)

// gcTraceLive is a GC trace line's cycle and live heap after its mark, in MiB.
var gcTraceLive = regexp.MustCompile(`^gc (\d+) @.*, \d+->\d+->(\d+) MB, `)

// shapeHeld is what a shape held at most over the live heap before it, and
// that heap.
type shapeHeld struct{ high, before int64 }

// runLiveBytesChild runs t's test again in a child of this binary under
// GODEBUG=gcstoptheworld=1,gctrace=1 and reads each shape's live high-water off
// its trace, failing t where the child fails or never ran it.
func runLiveBytesChild(t *testing.T) map[string]shapeHeld {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), self, "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v",
		"-test.timeout="+liveBytesChildTimeout.String())
	cmd.Env = append(os.Environ(), liveBytesChild+"=1", "GODEBUG="+strings.TrimPrefix(
		os.Getenv("GODEBUG")+",gcstoptheworld=1,gctrace=1", ","))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err = cmd.Run(); err != nil || !strings.Contains(stdout.String(), "--- PASS: "+t.Name()) {
		t.Fatalf("the child reading live bytes did not pass (%v):\n%s", err, stdout.String())
	}
	// EACH CYCLE'S LIVE HEAP, by its number, off the trace the runtime
	// writes whole to stderr, which nothing of the child's interleaves.
	live := map[int64]int64{}
	for line := range strings.Lines(stderr.String()) {
		if m := gcTraceLive.FindStringSubmatch(line); m != nil {
			cycle, cycleErr := strconv.ParseInt(m[1], 10, 64)
			mib, mibErr := strconv.ParseInt(m[2], 10, 64)
			if cycleErr != nil || mibErr != nil {
				t.Fatalf("the child's GC trace line %q", line)
			}
			live[cycle] = mib
		}
	}
	held := map[string]shapeHeld{}
	var name string
	var shape shapeHeld
	var from int64
	for line := range strings.Lines(stdout.String()) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), liveBytesMark+" ")
		if !ok {
			continue
		}
		var before, cycles int64
		if name == "" {
			if _, scanErr := fmt.Sscanf(rest, "%d %d", &before, &cycles); scanErr != nil {
				t.Fatalf("the child's mark %q: %v", line, scanErr)
			}
			name, from = strings.SplitN(rest, " ", 3)[2], cycles
			shape = shapeHeld{before: before}
			continue
		}
		if _, scanErr := fmt.Sscanf(rest, "%d", &cycles); scanErr != nil {
			t.Fatalf("the child's mark %q: %v", line, scanErr)
		}
		for c := from + 1; c <= cycles; c++ {
			mib, traced := live[c]
			if !traced {
				t.Fatalf("%s: the child's GC trace has no line for cycle %d", name, c)
			}
			// THE TRACE'S MIB ARE CUT DOWN, so each is read one up.
			shape.high = max(shape.high, (mib+1)<<20-shape.before)
		}
		held[name], name = shape, ""
	}
	return held
}

// gcCycles is how many GC cycles this process has completed.
func gcCycles() uint64 {
	s := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// liveNow is the live heap after a forced collection.
func liveNow() uint64 {
	runtime.GC()
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
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

var (
	_ = holdsScrubTerm("plan", "TestThePlanHoldsAtMostASixteenthOfThePlaintext")
	_ = holdsScrubTerm("plan blocks", "TestThePlanHoldsAtMostASixteenthOfThePlaintext")
)

// THE PLAN HOLDS AT MOST A SIXTEENTH OF THE PLAINTEXT, HOWEVER THE BOX SHAPES
// ITS SESSION: a bit for every two bytes beside the blocks its streams are
// filling ([planBlockBytes]), never a block or a record for each cut line or
// transcript, over a transcript of short lines each losing its one definition,
// one of a few, and a tar of many small transcripts. Read off the plan's own
// blocks as its first readings hand it on, which a reading of the heap varied
// by a spool's buffers across runs.
//
// Mutation: keep a block of bits for each cut line, or a plan of its own for
// each transcript, or count the "plan" term at a quarter or its blocks at
// none, and the plan holds past the figure.
func TestThePlanHoldsAtMostASixteenthOfThePlaintext(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{longValue})
	cut := `{"attachment":{"type":"deferred_tools_record","entries":[1]},"m":"[REDACTED:x]"}` + "\n"
	for name, members := range map[string][]tarMember{
		"a transcript of 20,000 lines that each lose a definition": {{h: tar.Header{
			Name: "projects/p/s.jsonl", Typeflag: tar.TypeReg, Mode: 0o600}, body: strings.Repeat(cut, 20_000)}},
		"one transcript of a few lines, its blocks past a sixteenth": {{h: tar.Header{
			Name: "projects/p/s.jsonl", Typeflag: tar.TypeReg, Mode: 0o600}, body: strings.Repeat(cut, 3)}},
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
		held := planHeld(t, archive, s)
		figure := termFigure(t, "plan", plain, 0) + termFigure(t, "plan blocks", plain, 0)
		t.Logf("%s: %d KiB of plaintext from a %d KiB archive; the plan held %d KiB, its figure %d KiB", name,
			plain>>10, len(archive)>>10, held>>10, figure>>10)
		if held > figure {
			t.Errorf("%s: the plan held %d bytes as the first readings handed it on, past the %d a sixteenth "+
				"of the plaintext and its blocks come to", name, held, figure)
		}
	}
}

// planHeld is the bytes of the blocks the plan of archive's first readings
// holds as they hand it on to the second.
func planHeld(t *testing.T, archive []byte, s scrubber) int64 {
	t.Helper()
	spool, err := newUnitSpool(spoolPlace{dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.remove()
	plan, err := firstTarReadings(bytes.NewReader(archive), reopenOf(archive), s, testBounds, spool)
	defer plan.letGo()
	if err != nil {
		t.Fatalf("the session was refused: %v", err)
	}
	return int64(len(plan.members.list.blocks)+len(plan.edits.list.blocks)) * bitBlockWords * 8
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

// THE SCRUB WRITES A SESSION'S MEDIA NO LARGER THAN ITS BOX PACKED IT: a Claude
// Code line holding a PDF's base64 twice and an OpenCode export holding an
// image's data URI, each packed as a box packs it (level 9 standing in for GNU
// gzip, which codes base64 as it does), are kept within 1% of the box's
// archive, since the take and the seal hold the two to one bound
// ([keptGzipLevel]).
//
// Mutation: write the kept archive at any level from one to six
// (gzip.NewWriter's is six) and each case is kept about 1.31 times the box's.
func TestTheScrubWritesASessionsMediaNoLargerThanItsBoxPackedIt(t *testing.T) {
	t.Parallel()
	data := base64Run(rand.New(rand.NewPCG(1170, 6)), 512<<10)
	s := newScrubber([]string{longValue})
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1",` +
		`"content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` +
		data + `"}}]}]},"toolUseResult":{"type":"pdf","file":{"filePath":"/w/a.pdf","base64":"` + data + `"}}}` + "\n"
	doc := `{"info":{"id":"ses_1"},"messages":[{"info":{"id":"msg_1","role":"user"},"parts":[{"type":"file",` +
		`"mime":"image/png","url":"data:image/png;base64,` + data + `"}]}]}`
	cases := []struct {
		name  string
		plain int
		box   []byte
		scrub func(first io.Reader, reopen func() (io.ReadCloser, error), dst io.Writer, s scrubber,
			b scrubBounds) error
	}{
		{"a Claude Code transcript", len(line), boxTarOf(t, "projects/p/s.jsonl", line), scrubTar},
		{"an OpenCode export", len(doc), boxGzipOf(t, doc), scrubDocument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if 5*len(c.box) > 4*c.plain {
				t.Fatalf("the box's archive is %d bytes of %d: it did not code the base64, so it measures nothing",
					len(c.box), c.plain)
			}
			var out bytes.Buffer
			if err := c.scrub(bytes.NewReader(c.box), reopenOf(c.box), &out, s, testBounds); err != nil {
				t.Fatalf("the session was refused: %v", err)
			}
			if 100*out.Len() > 101*len(c.box) {
				t.Errorf("the session was kept as %d bytes, %.2f times the %d its box packed: past the "+
					"bound the take admitted it under", out.Len(), float64(out.Len())/float64(len(c.box)), len(c.box))
			}
		})
	}
}

// boxTarOf is a gzipped tar of one file, packed as a box's GNU gzip packs it.
func boxTarOf(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)),
		Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// boxGzipOf is text gzipped as a box's GNU gzip packs it.
func boxGzipOf(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(zw, text); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A FIRST READING READS A JSON LINE, AND A `.json` FILE THAT IS JSON, AS JSON
// ALONE: their text scrub, never written, opens no text on the reading's text
// way; a line a reading reads both ways, from the first that is no JSON, opens
// one.
//
// Mutation: read every line or `.json` file both ways, and the text way opens.
func TestAFirstReadingReadsAJSONUnitAsJSONAlone(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{longValue})
	long := `{"text":"` + randomBase64(3, 2*scrubPieceBaseBytes) + `"}`
	for name, tc := range map[string]struct {
		read func(scrubber, *tarPlan) error
		text bool
	}{
		"a transcript's JSON lines": {read: func(s scrubber, plan *tarPlan) error {
			_, err := measureLines(strings.NewReader(`{"a":"b"}`+"\n"+long+"\n"), s, plan)
			return err
		}},
		"a `.json` file that is JSON": {read: func(s scrubber, plan *tarPlan) error {
			_, err := measureFile("tasks/s/1.json", strings.NewReader(long), s, plan)
			return err
		}},
		"a line read both ways": {text: true, read: func(s scrubber, plan *tarPlan) error {
			plan.bothFrom = 1
			_, err := measureLines(strings.NewReader(long[:len(long)-2]+"\n"), s, plan)
			return err
		}},
	} {
		pieces := newFirstPieces(nil)
		plan := spooledPlan(t)
		if err := tc.read(s.firstReading(pieces), plan); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if opened := pieces.ways[textWay].buf != nil; opened != tc.text {
			t.Errorf("%s: the reading opened a text on its text way: %v, want %v", name, opened, tc.text)
		}
	}
}

// A UNIT THAT IS NO JSON ENDS THE FIRST READING, AND THE NEXT READS IT AS TEXT
// AND EVERY ONE AFTER IT BOTH WAYS: a session whose lines and `.json` files are
// all JSON is read once; one holding a torn line, a `.json` file that is no
// JSON, or both, once more, from the first such unit. Each is kept as the
// scrub of its text, every other as JSON.
//
// Mutation: keep a unit read as JSON alone as text without reading again, or
// read again as JSON alone, and a case keeps other bytes or never ends.
func TestAUnitThatIsNoJSONEndsTheFirstReadingAndTheNextReadsItBothWays(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{longValue})
	torn := `{"text":"` + longValue + ` ` + randomBase64(4, textWholeBytes+512)
	lines := func(middle string) string {
		return `{"a":"b"}` + "\n" + middle + "\n" + `{"c":"` + longValue + `"}` + "\n"
	}
	for name, tc := range map[string]struct {
		transcript, file string
		readings         int
	}{
		"all JSON":                   {transcript: lines(`{"b":1}`), file: `{"k":"v"}`, readings: 1},
		"a torn line":                {transcript: lines(torn), file: `{"k":"v"}`, readings: 2},
		"a `.json` file not JSON":    {transcript: lines(`{"b":1}`), file: `{"k":` + longValue, readings: 2},
		"a torn line and a bad file": {transcript: lines(torn), file: `{"k":` + longValue, readings: 2},
		"a transcript of no JSON":    {transcript: "ab\ncd\n" + longValue + "\n", file: `{}`, readings: 2},
	} {
		archive := tarOf(t, map[string]string{"projects/p/s.jsonl": tc.transcript, "tasks/s/1.json": tc.file})
		opened := 0
		reopen := func() (io.ReadCloser, error) {
			opened++
			return io.NopCloser(bytes.NewReader(archive)), nil
		}
		var out bytes.Buffer
		if err := scrubTar(bytes.NewReader(archive), reopen, &out, s, testBounds); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if read := opened + 1; read != tc.readings {
			t.Errorf("%s: the archive was read %d times, want %d", name, read, tc.readings)
		}
		kept := untarOf(t, out.Bytes())
		if file := kept["tasks/s/1.json"]; json.Valid([]byte(tc.file)) != json.Valid([]byte(file)) ||
			!json.Valid([]byte(tc.file)) && file != keptText(s, tc.file) {
			t.Errorf("%s: the `.json` file was kept as %.80q", name, file)
		}
		for i, line := range strings.Split(strings.TrimSuffix(tc.transcript, "\n"), "\n") {
			got := strings.Split(kept["projects/p/s.jsonl"], "\n")[i]
			switch isJSON := json.Valid([]byte(line)); {
			case isJSON && (!json.Valid([]byte(got)) || strings.Contains(got, longValue)):
				t.Errorf("%s: line %d was JSON and is kept as %.80q", name, i, got)
			case !isJSON && got != keptText(s, line):
				t.Errorf("%s: line %d was no JSON and is kept as %.80q, not its text", name, i, got)
			}
		}
	}
}

// A READING GOES ON FROM WHERE THE FIRST ENDED: a session torn at a line, as
// a take landing mid-write finds it, or ended at a `.json` file that is no
// JSON, has each text before that unit read once, the plan and the spool of
// the first reading kept to it (a line after it, read both ways, is one text
// more: its line as text), and is kept as each of its lines and files is kept
// on its own.
//
// Mutation: read the archive again from its start, or from its member's
// start, keep the spool past the mark, or count its units from the start, and
// a case reads a text twice or keeps other bytes.
func TestAReadingGoesOnFromWhereTheFirstEnded(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{longValue})
	pdf := pdfLine(14, 300<<10)
	torn := `{"text":"` + longValue + ` ` + randomBase64(15, 1<<10)
	for name, tc := range map[string]struct {
		files map[string]string
		read  int
	}{
		"a transcript torn at its last line": {map[string]string{
			"projects/p/a.jsonl": pdf + "\n",
			"projects/p/s.jsonl": `{"a":"` + longValue + `"}` + "\n" + pdf + "\n" + torn + "\n",
			"projects/p/z.jsonl": pdf + "\n",
		}, 7},
		"a `.json` file that is no JSON": {map[string]string{
			"projects/p/s.jsonl": pdf + "\n",
			"tasks/s/1.json":     `{"k":"` + longValue + `"`,
		}, 2},
	} {
		archive := tarOf(t, tc.files)
		use := &memoUse{memo: newScrubMemo(), scope: "agent-a"}
		kept, opened, err := scrubTake(archiveTar, archive, s, use)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if read := use.n.hits + use.n.misses; opened != 2 || read != tc.read || use.n.diverged != 0 {
			t.Errorf("%s: %+v, read %d times; want %d texts read, one reading on", name, use.n, opened, tc.read)
		}
		got := untarOf(t, kept)
		for file, body := range tc.files {
			var want []string
			for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
				switch {
				case !strings.HasSuffix(file, ".jsonl"):
					want = append(want, keptText(s, line))
				case json.Valid([]byte(line)):
					want = append(want, rewriteLine(t, line, s))
				default:
					want = append(want, keptText(s, line))
				}
			}
			if w := strings.Join(want, "\n") + map[bool]string{true: "\n"}[strings.HasSuffix(body, "\n")]; got[file] != w {
				t.Errorf("%s: %s was kept as %.120q, want %.120q", name, file, got[file], w)
			}
		}
	}
}

// A PLAN CUT BACK TO A MARK READS AS WRITTEN AFTER IT: bits cut back past a
// block's end and written again read back as the kept ones and the new.
//
// Mutation: keep the blocks past the mark, or count none of the bits before
// it, and the bits read back otherwise.
func TestAPlanCutBackToAMarkReadsAsWrittenAfterIt(t *testing.T) {
	t.Parallel()
	const per = bitBlockWords * 64
	var p planBits
	var want []bool
	for i := range 2*per + 100 {
		p.put(i%3 == 0)
		if i < per+7 {
			want = append(want, i%3 == 0)
		}
	}
	p.list.truncate(per + 7)
	for i := range 50 {
		p.put(i%2 == 0)
		want = append(want, i%2 == 0)
	}
	if p.count() != len(want) || len(p.list.blocks) != 2 {
		t.Fatalf("the plan holds %d bits in %d blocks, want %d in 2", p.count(), len(p.list.blocks), len(want))
	}
	for i, w := range want {
		if v, ok := p.take(); !ok || v != w {
			t.Fatalf("bit %d reads %v (%v), want %v", i, v, ok, w)
		}
	}
}

// A `.json` FILE ITS ARCHIVE FAILS IN IS THAT FAILURE, NEVER A FILE THAT IS NO
// JSON: a file the archive's plaintext bound cuts short refuses the take at its
// first reading, with the bound's own error, rather than read again both ways.
//
// Mutation: read a document the archive failed in as no JSON, and the take
// opens the archive again before it is refused.
func TestAJSONFileItsArchiveFailsInRefusesTheTakeAtOnce(t *testing.T) {
	t.Parallel()
	archive := tarOf(t, map[string]string{"tasks/s/1.json": `{"text":"` + strings.Repeat("a", 2<<20) + `"}`})
	opened := 0
	reopen := func() (io.ReadCloser, error) {
		opened++
		return io.NopCloser(bytes.NewReader(archive)), nil
	}
	err := scrubTar(bytes.NewReader(archive), reopen, io.Discard, newScrubber(nil), scrubBounds{plain: 1 << 20})
	if !errors.Is(err, errScrubBound) || opened != 0 {
		t.Errorf("the take ended %v having opened the archive again %d times; want errScrubBound at once", err,
			opened)
	}
}

// EVERY VALUE UNDER A CREDENTIAL'S KEY IS ITS MARKER, AT ANY DEPTH: a string,
// a number, true and false under the key, in an array there or an object, at
// any depth, are each the credential's marker, a string; the containers stand
// as they were, their own keys and nulls kept, so a harness reads its record
// back; and a value of whole base64 past the mebibyte in an array under the
// key is its marker too, nothing kept apart, with media inline or apart.
//
// Mutation: take only a string whole, or a container's values only at its
// first depth, and a case goes red.
func TestEveryValueUnderACredentialsKeyIsItsMarker(t *testing.T) {
	t.Parallel()
	s := newScrubber(nil)
	marker, _ := plainjson.Marshal(credentialMarker, "")
	m := string(marker)
	for name, tc := range map[string]struct{ value, want string }{
		"a string":          {`"hunter2-secret"`, m},
		"a number":          {`73519046284`, m},
		"true":              {`true`, m},
		"false":             {`false`, m},
		"an array":          {`["hunter2",42,false,null]`, `[` + m + `,` + m + `,` + m + `,null]`},
		"an object":         {`{"a":"hunter2","b":{"c":7}}`, `{"a":` + m + `,"b":{"c":` + m + `}}`},
		"arrays in objects": {`{"x":[["deep"]]}`, `{"x":[[` + m + `]]}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := rewriteLine(t, `{"password":`+tc.value+`,"after":"kept"}`, s)
			if want := `{"password":` + tc.want + `,"after":"kept"}`; got != want {
				t.Errorf("kept %q, want %q", got, want)
			}
		})
	}
	t.Run("a payload in an array", func(t *testing.T) {
		t.Parallel()
		payload := mediaPayload(56, mediaMinBytes+8)
		archive := claudeArchive(t, `{"type":"user","password":["`+payload+`"]}`)
		for name, plain := range map[string]string{
			"media inline": mediaPlain(t, inlineTake(t, archiveTar, archive, s)),
			"media apart": func() string {
				whole, body := mediaTake(t, archiveTar, archive, s, executorkeep.SessionMaxBytes)
				if body[0] != sandbox.BodyWhole {
					t.Error("the value under the credential's key is kept apart")
				}
				return mediaPlain(t, whole)
			}(),
		} {
			if strings.Contains(plain, payload[:64]) || !strings.Contains(plain, `"password":[`+m+`]`) {
				t.Errorf("%s: the value in the array under the credential's key is kept", name)
			}
		}
	})
}
