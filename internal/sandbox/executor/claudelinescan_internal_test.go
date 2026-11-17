package executor

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
)

// THE DIFFERENTIAL: a line read in one scanner pass ([scanReader]) answers
// what the whole decode answers of it ([decodeThreadLine]): record or refused,
// the fault, the kind of record a refused head names, the size, whether the
// error is errNotOneRecord (a refused head's in its own words), the byte facts,
// and for a record its head and every fact the pass reads. A value is read at
// any length here: the bound a value is kept within is raised
// ([scanReader.valueMax]), since the whole decode has none, and the bound's own
// claims are [TestALateFaultBeatsWhatTheLineSaidBeforeIt]'s.

// scanAnswer is what a reading answered of one line, line and err, as text:
// for a record, its place, its size, its head, every small value the pass
// keeps as written and whether it carries taskDelivery, and its byte facts;
// for a line that is no record, its place, its fault, the kind of record its
// head names, its size, whether its error is errNotOneRecord (a refused
// head's in its words), and its byte facts; else what ended the reading.
func scanAnswer(line threadLine, err error) string {
	var broken *threadLineError
	switch {
	case errors.As(err, &broken):
		said := ""
		if broken.Fault == faultHead {
			said = ": " + broken.Err.Error()
		}
		return fmt.Sprintf("line %d is no record: fault %q, type %q, size %d, not one JSON object %t%s\nbytes: %s\n",
			broken.N, broken.Fault, broken.Type, broken.Size, errors.Is(broken.Err, errNotOneRecord), said,
			scanBytes(broken.Bytes))
	case err != nil:
		return fmt.Sprintf("ended by %v\n", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "line %d: size %d\nhead: type %q, subtype %q, parent %q, sidechain %s, agent %q, replaces a span %t\n",
		line.N, line.Size, line.Head.Type, line.Head.Subtype, ctRaw(line.Head.Parent), ctSidechain(line.Head),
		line.Head.AgentID, line.Head.ReplacesSpan)
	f := line.facts()
	// EVERY SMALL VALUE BY ITS OWN FIELD, never through the pass's own table
	// of them, which a missing row would blind.
	for _, kept := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"uuid", f.uuid}, {"version", f.version}, {"origin", f.origin}, {"isApiErrorMessage", f.apiError},
		{"leafUuid", f.leafUUID}, {"explicit", f.explicit}, {"rewound", f.rewound}, {"isVirtual", f.isVirtual},
		{"interruptedByShutdown", f.interruptedByShutdown}, {"isCompactSummary", f.isCompactSummary},
		{"timestamp", f.timestamp}, {"isMeta", f.isMeta}, {"sourceToolAssistantUUID", f.sourceToolAssistantUUID},
	} {
		fmt.Fprintf(&b, "%s: %q\n", kept.name, ctRaw(kept.raw))
	}
	fmt.Fprintf(&b, "taskDelivery: %t\nbytes: %s\n", f.taskDelivery, scanBytes(line.Bytes))
	return b.String()
}

// scanBytes is every byte fact of b, as text.
func scanBytes(b threadBytes) string {
	return fmt.Sprintf("%d bytes, head %q, slot keyed %t, keyed %t by %q, marks %b", b.Len(), b.Head(), b.slotKeyed,
		b.keyed, b.key[:], b.marks)
}

// scanReading is what a [scanReader] made of a member: each answer of each
// line, in order, read to the member's end BEFORE any is printed, so a line
// that shared a buffer with a later one reads otherwise; then what ended the
// reading, what one more Next answered, and whether the member ended torn.
func scanReading(ctx context.Context, r *scanReader) []string {
	type read struct {
		line threadLine
		err  error
	}
	var reads []read
	for {
		line, err := r.Next(ctx)
		var broken *threadLineError
		if err != nil && !errors.As(err, &broken) {
			_, again := r.Next(ctx)
			answers := make([]string, 0, len(reads)+1)
			for _, read := range reads {
				answers = append(answers, scanAnswer(read.line, read.err))
			}
			return append(answers, fmt.Sprintf("ended by %v, then %v, torn %t\n", err, again, r.Torn()))
		}
		reads = append(reads, read{line, err})
	}
}

// checkScanReadsAsDecode reads text, one member, with a [scanReader] taking at
// most chunk bytes of it at a time, its bound on a value raised, and holds
// each line it reads to what the whole decode reads of the same line, text
// split at its breaks: the same answer ([scanAnswer]); then the member's end,
// torn exactly where a piece with no break ends it.
func checkScanReadsAsDecode(t testing.TB, text []byte, chunk int) {
	t.Helper()
	r := newScanReaderSize(bytes.NewReader(text), int64(len(text)), chunk)
	r.valueMax = math.MaxInt
	got := scanReading(t.Context(), r)
	pieces := bytes.Split(text, []byte("\n"))
	lines, last := pieces[:len(pieces)-1], pieces[len(pieces)-1]
	if end := fmt.Sprintf("ended by EOF, then EOF, torn %t\n", len(last) > 0); len(got) != len(lines)+1 ||
		got[len(lines)] != end {
		t.Fatalf("a member of %d lines and %d bytes after its last break, read in pieces of %d bytes at most, "+
			"reads as %d answers ending %q; want %d and %q", len(lines), len(last), chunk, len(got)-1,
			got[len(got)-1], len(lines), end)
	}
	for i, line := range lines {
		if want := scanAnswer(decodeThreadLine(i+1, line)); got[i] != want {
			t.Fatalf("line %d, %.300q of %d bytes, read in one scanner pass in pieces of %d bytes at most:\n%s"+
				"read whole:\n%s", i+1, line, len(line), chunk, got[i], want)
		}
	}
}

// A LINE READ IN ONE SCANNER PASS IS READ AS ITS WHOLE DECODE READS IT: every
// line of the corpus ([scanCorpus]) answers, read in one pass, what the whole
// decode answers ([scanAnswer]): record or refused, the fault, the kind of
// record a refused head names, the size, whether the error is
// errNotOneRecord, the byte facts, the head and every small value the pass
// keeps as written. The corpus is read as one member, so nothing of a line
// reaches the next, each line's answer printed only once every line is read,
// and in pieces of 17 bytes, so every token of a short line meets a piece's
// edge somewhere.
//
// Mutation: keep a key's first occurrence rather than its last; classify a
// line that is no object before the scanner's end of it; hand the scanner the
// line's break; compare names whatever their case; read on past a fault
// without reading the line to its break; keep a line's parent in the pass's
// buffer, which the next line's overwrites; read a byte order mark as a
// fault of syntax; give errNotOneRecord to a line that opened an object; name
// no kind for a refused head; or drop what of a piece the scanner had no room
// left for.
func TestTheScanReadsEveryLineAsTheDecodeReadsIt(t *testing.T) {
	t.Parallel()
	lines := scanCorpus(t)
	text := append(bytes.Join(lines, []byte("\n")), '\n')
	for _, chunk := range []int{lineChunkBytes, 17} {
		checkScanReadsAsDecode(t, text, chunk)
	}
}

// FuzzTheScanReadsEveryLineAsTheDecodeReadsIt holds a member of any bytes,
// split at its breaks into lines, read in one scanner pass whole and in pieces
// of a size its seed draws, to what the whole decode reads of each line, as
// [TestTheScanReadsEveryLineAsTheDecodeReadsIt] does.
func FuzzTheScanReadsEveryLineAsTheDecodeReadsIt(f *testing.F) {
	var seeds []string
	seeds = append(seeds, scanSeeds...)
	seeds = append(seeds, byteFactHardLines()...)
	for _, refusal := range ctBrokenLines() {
		seeds = append(seeds, refusal.line)
	}
	seeds = append(seeds, ctEnqueue, ctBrief(ctUUID(1)), ctAssistant(ctUUID(1), ctUUID(2), ctUseContent),
		ctResult(ctUUID(4), ctUUID(5), ctResultBody, ctResultOwn), ctLastPrompt, ctMeta,
		`{"type":"user","type":7}`, `{"type":7,"type":"user"}`, `{"\u0074ype":"user","Type":7}`,
		`{"parentUuid":"x","parentUuid":null,"isSidechain":"no","isSidechain":true}`)
	for i, seed := range seeds {
		f.Add([]byte(seed+"\n"), uint16(i))
	}
	f.Fuzz(func(t *testing.T, text []byte, chunk uint16) {
		checkScanReadsAsDecode(t, text, lineChunkBytes)
		checkScanReadsAsDecode(t, text, 16+int(chunk%512))
	})
}

// scanCorpus is every line the differential reads: every line of every Claude
// Code thread under testdata, the layer's own records and the byte facts' hard
// cases ([byteFactLines]); the scanner's own fuzz seeds ([scanSeeds]), split
// at their breaks; every line the layer's tests read as no record
// ([ctBrokenLines], [ctRefusedLines]) and a record nested as deep as a line
// is read; each kind of record mutated at every key the pass reads
// ([scanMutations]); and lines whose keys, a key written twice or a fault
// fall across the edge of what the member hands at once ([scanAcrossAFill]).
func scanCorpus(t *testing.T) [][]byte {
	t.Helper()
	lines := byteFactLines(t)
	for _, seed := range scanSeeds {
		lines = append(lines, bytes.Split([]byte(seed), []byte("\n"))...)
	}
	for _, refusals := range []map[string]ctRefusal{ctBrokenLines(), ctRefusedLines(t)} {
		for _, refusal := range refusals {
			lines = append(lines, []byte(refusal.line))
		}
	}
	lines = append(lines, []byte(ctDeep(10000)))
	lines = append(lines, scanMutations(lines)...)
	return append(lines, scanAcrossAFill()...)
}

// scanReadKeys is every key of a record a reading of it reads: the head's,
// the small values kept as written, taskDelivery, and the frames.
var scanReadKeys = []string{"type", "subtype", "agentId", "parentUuid", "isSidechain", "replacesSpan", "uuid",
	"version", "origin", "isApiErrorMessage", "leafUuid", "explicit", "rewound", "isVirtual", "interruptedByShutdown",
	"isCompactSummary", "timestamp", "isMeta", "sourceToolAssistantUUID", "taskDelivery", "message", "toolUseResult",
	"attachment", "compactMetadata", "retractedMessageUuids", "replacements", "batchToolUses"}

// scanKinds is a value of every kind JSON has, and an empty object and an
// empty string, each a value a key may be written with where another is
// read.
var scanKinds = []string{`"x"`, `""`, `{"a":1}`, `{}`, `[1]`, `true`, `false`, `null`, `1`}

// scanMember is one member of an object, its name decoded and its value as
// written; quoted, where set, its name as written in place of name's own
// quoting.
type scanMember struct {
	name, value, quoted string
}

// scanObject is the object of members, in order.
func scanObject(members []scanMember) []byte {
	b := []byte{'{'}
	for i, m := range members {
		if i > 0 {
			b = append(b, ',')
		}
		if m.quoted != "" {
			b = append(b, m.quoted...)
		} else {
			b, _ = jsontext.AppendQuote(b, m.name)
		}
		b = append(append(b, ':'), m.value...)
	}
	return append(b, '}')
}

// scanSpellings is key written as no key the layer reads, a letter's case
// changed, and as the key itself with its first letter escaped, which the
// layer reads as the key: each quoted.
func scanSpellings(key string) []string {
	flipped := []byte(key)
	flipped[0] ^= 0x20
	return []string{`"` + string(flipped) + `"`, fmt.Sprintf(`"\u%04x%s"`, key[0], key[1:])}
}

// scanMutations is a record of each kind among lines (one a type and
// subtype), mutated at every key a reading reads: the key moved first and
// moved last; and a value of every kind written before the record's own and
// after it, so a key written twice is read wholly by its last, whichever of
// the two is the one the record wrote, and every key is read of every kind;
// a key the record lacks among them; and after the record's own, a value of
// every kind under the key's name with a letter's case changed, which is
// another key, and under its name with a letter escaped, which is the key.
func scanMutations(lines [][]byte) [][]byte {
	var out [][]byte
	seen := map[string]bool{}
	for _, line := range lines {
		var members []scanMember
		if eachKey(line, func(name []byte, value json.RawMessage, _ int) error {
			members = append(members, scanMember{name: string(name), value: string(value)})
			return nil
		}) != nil {
			continue
		}
		shape := ""
		for _, m := range members {
			if m.name == "type" || m.name == "subtype" {
				shape += m.name + "=" + m.value + " "
			}
		}
		if seen[shape] {
			continue
		}
		seen[shape] = true
		for _, key := range scanReadKeys {
			var others []scanMember
			for _, m := range members {
				if m.name != key {
					others = append(others, m)
				} else {
					out = append(out, scanObject(append([]scanMember{m}, others...)),
						scanObject(append(append([]scanMember(nil), others...), m)))
				}
			}
			for _, kind := range scanKinds {
				written := scanMember{name: key, value: kind}
				out = append(out, scanObject(append([]scanMember{written}, members...)),
					scanObject(append(append([]scanMember(nil), members...), written)))
				for _, spelled := range scanSpellings(key) {
					out = append(out, scanObject(append(append([]scanMember(nil), members...),
						scanMember{value: kind, quoted: spelled})))
				}
			}
		}
	}
	return out
}

// scanAcrossAFill is lines whose key, key written twice, escaped name, value
// cut at the break or data after the value begins a few bytes either side of
// where the member's first piece ends ([lineChunkBytes]), each behind a
// member no reader reads that fills the line out to there; and lines whose
// literal, escape or number that edge cuts is followed by a piece the
// scanner's buffer has no room left for whole, a member no reader reads
// filling the line out to a uuid the second piece's edge cuts, so what of
// that piece the scanner takes in a second read is a value a reader reads.
func scanAcrossAFill() [][]byte {
	brief := ctBrief(ctUUID(1))
	var out [][]byte
	for _, at := range []int{-40, -21, -13, -7, -3, -2, -1, 0, 1, 2, 7} {
		// padded is rest, an object, its members behind `"pad"`, the first of
		// them beginning at the edge plus at.
		padded := func(rest string) []byte {
			return []byte(`{"pad":"` + strings.Repeat("x", lineChunkBytes+at-len(`{"pad":"",`)) + `",` + rest[1:])
		}
		for _, rest := range []string{
			brief,
			`{"type":"user","type":7}`,
			`{"type":7,"type":"user"}`,
			`{"\u0074ype":"user","isSidechain":false}`,
			`{"uuid":"` + ctUUID(2) + `"} x`,
			`{"uuid":"` + ctUUID(2),
			`{"uuid":"` + ctUUID(2) + "\x00" + `"}`,
		} {
			out = append(out, padded(rest))
		}
		for _, rest := range []string{`{"a":true`, `{"a":"\u0041\u0042"`, `{"a":12345678`} {
			line := padded(rest)
			const uuidAt = 2*lineChunkBytes - slotKeyLen/2
			fill := uuidAt - len(line) - len(`,"tail":"","uuid":"`)
			out = append(out, append(line, `,"tail":"`+strings.Repeat("x", fill)+`","uuid":"`+ctUUID(9)+`"}`...))
		}
	}
	return out
}

// scanSeeds is a copy of the scanner's own fuzz seeds (jsonscan's
// FuzzTheScannerReadsWhatBothDecodersRead), each at an edge of the grammar or
// of the decoding, read here as members of thread lines.
var scanSeeds = []string{
	`"\"\\\/\b\f\n\r\t"`, `"\u0041\u00e9\u20ac\uFFFF\u0000\u001f"`, `"\u00E9\u20AC"`, `{"\u0061":"\u0062"}`,
	`"\x"`, `"\u12G4"`, `"\u12"`, `"\u"`, `"\`, `"\u1`,
	`"\ud83d\ude00"`, `"\uD83D\uDE00"`, `"\udbff\udfff"`, `"\ud800"`, `"\ud800x"`, `"\ud800\u0041"`,
	`"\udc00"`, `"\udc00\ud800"`, `"\ud800\ud800"`, `"\ud800\ud800\udc00"`, `"\ud800\n"`, `"\ud800\\"`,
	`"\ud800\udc`, `"\ud800\u`, `"\ud800\`, `"\ud800`, `"\ud800\uZZZZ"`, `"\udbff\ue000"`,
	"\"\xff\"", "\"\xc3\"", "\"\xe2\x82\"", "\"\xed\xa0\x80\"", "\"\xf4\x90\x80\x80\"", "\"\xc0\x80\"",
	"\"a\x80b\"", "\"\xef\xbf\xbd\"", "\"\xe2\x82", "\xff", "[\xc3]", `{"a":1}` + "\xff", "{\"\xff\":1}",
	"\"\xf0\x9f\x98\x80\"", "\"\xf0\x9f\x98\"",
	"\"\x00\"", "\"\x1f\"", "\"\x7f\"", "\"a\tb\"", "\"\n\"", "\"a\rb\"",
	"-0", "0.5", "1e+5", "01", "1.", ".5", "-", "0", "-1.5e-10", "1E5", "1e", "1e+", "00", "-01", "1.5.",
	"123456789012345678901234567890", "[1.]", "[-]", "[01]", "1x", "0x10", "+1", "1e5.0", "-.5", "2E-0",
	"[1e]", "[1,-0,0.0,1E+2]", "1.0e", "9.e1",
	"true", "false", "null", "tru", "nul", "fals", "True", "nulL", "[true,false,null]", "tx", "n",
	" \t\n\r1 \t\n\r", "[ 1 , 2 ]", `{ "a" : 1 }`, "\f1", "\v1", "\xc2\xa01", "1\f",
	"\x00", "\x001", "\xef\xbb\xbf{}", "\xef\xbb\xbf", "\xef\xbb", " \x00", "\x00 ",
	"1 2", "{}{}", "[]x", `"a"b`, "truex", "null,", "1\x00", "{} ", "[] \n ]",
	"{}", "[]", "[[]]", `{"a":[]}`, "[1,]", "[,1]", "{,}", `{"a"}`, `{"a":}`, "{:1}", "[}", "{]", `{"a":1]`,
	`{"a":1,}`, `{"a" 1}`, `{1:2}`, "[1 2]", `{"a":1 "b":2}`, `[[[]]`, `{"a":{"b":{}}}`,
	`{"a":1,"a":2}`, `{"a":{"x":1},"a":{"y":2}}`, `{"a":1,"\u0061":2}`,
	"", "   ", "\n", "\r\n\t ",
	`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"line one\nline \"two\"` +
		` caf\u00e9 \ud83d\ude00"}]}]},"uuid":"5f0c","timestamp":"2026-11-14T10:00:00.000Z"}`,
}

// A LATE FAULT BEATS WHAT THE LINE SAID BEFORE IT: a line is judged only once
// the scanner has read it to its break, a fault of its grammar first wherever
// it lies, so a head refused early and then data after the value is
// trailing, a value cut at the break after it a prefix and a NUL in a string
// after it a syntax error; a list is no object, and a list followed by more
// is trailing; a value past its bound followed by more is trailing, one
// followed by a refused head is that head, and only a line whose grammar and
// head are whole fails at the bound, which ends the reading. A last piece
// with no break commits nothing whatever it holds, and a member's bound
// falling inside a line ends the reading there. Each holds of a line of a few
// bytes and of one past 8 MiB, the bound the whole line was read within.
//
// Mutation: answer the line before the scanner reads to its end, and the
// head fault and the bound beat the data after them; or commit a piece the
// member ends with no break, and the torn record reads as a line.
func TestALateFaultBeatsWhatTheLineSaidBeforeIt(t *testing.T) {
	t.Parallel()
	brief := ctThread(ctBrief(ctUUID(1)))
	over := strings.Repeat("u", threadValueMax+1)
	type want struct {
		fault  lineFault
		kind   recordKind
		notOne bool
		// end, where the line ends the reading, and what its error says.
		end  error
		says string
		torn bool
	}
	for _, n := range []int{16, 9 << 20} {
		pad := strings.Repeat("x", n)
		for name, tc := range map[string]struct {
			line     string
			torn     bool
			maxBytes int64
			want     want
		}{
			"a head refused, then data after the value": {line: `{"type":7,"content":"` + pad + `"} x`,
				want: want{fault: faultTrailing}},
			"a head refused, then the value cut at the break": {line: `{"type":7,"content":"` + pad,
				want: want{fault: faultPrefix}},
			"a head refused, then a NUL in a string": {line: `{"type":7,"content":"` + pad + "\x00" + pad + `"}`,
				want: want{fault: faultSyntax}},
			"a list":                     {line: `["` + pad + `"]`, want: want{fault: faultNotObject, notOne: true}},
			"a list, then data after it": {line: `["` + pad + `"] x`, want: want{fault: faultTrailing, notOne: true}},
			"a value past its bound, then data after the value": {
				line: `{"type":"user","uuid":"` + over + `","content":"` + pad + `"} x`, want: want{fault: faultTrailing}},
			"a value past its bound, then a refused head": {
				line: `{"type":"user","uuid":"` + over + `","content":"` + pad + `","isSidechain":1}`,
				want: want{fault: faultHead, kind: recordUser}},
			"a type past its bound, then a refused head": {
				line: `{"type":"` + over + `","content":"` + pad + `","isSidechain":1}`, want: want{fault: faultHead}},
			"a value past its bound": {line: `{"type":"user","uuid":"` + over + `","content":"` + pad + `"}`,
				want: want{end: errThreadBound, says: "uuid runs past"}},
			"a whole record the member ends with no break": {line: `{"type":"user","content":"` + pad + `"}`,
				torn: true, want: want{end: io.EOF, torn: true}},
			"the member's bound inside the line": {line: `{"type":"user","content":"` + pad + `"}`,
				maxBytes: int64(len(brief) + n/2), want: want{end: errThreadBound, says: "the member runs past"}},
		} {
			text := brief + tc.line
			if !tc.torn {
				text += "\n"
			}
			maxBytes := cmp.Or(tc.maxBytes, threadPlainMax)
			r := newScanReader(strings.NewReader(text), maxBytes)
			if line, err := r.Next(t.Context()); err != nil || line.Head.Type != recordUser {
				t.Fatalf("%s: the brief before the line reads as %q, %v", name, line.Head.Type, err)
			}
			_, err := r.Next(t.Context())
			var broken *threadLineError
			switch {
			case tc.want.end != nil:
				if !errors.Is(err, tc.want.end) || !strings.Contains(err.Error(), tc.want.says) ||
					r.Torn() != tc.want.torn {
					t.Errorf("%s, a line of %d bytes: the reading ended with %v, torn %t; want %v saying %q, torn %t",
						name, len(tc.line), err, r.Torn(), tc.want.end, tc.want.says, tc.want.torn)
				}
				if _, again := r.Next(t.Context()); !errors.Is(again, tc.want.end) {
					t.Errorf("%s, a line of %d bytes: asked again, the reading answered %v; want %v", name,
						len(tc.line), again, tc.want.end)
				}
			case !errors.As(err, &broken):
				t.Errorf("%s, a line of %d bytes: read as %v; want a line that is no record", name, len(tc.line), err)
			case broken.Fault != tc.want.fault || broken.Type != tc.want.kind ||
				errors.Is(broken.Err, errNotOneRecord) != tc.want.notOne || broken.Size != len(tc.line)+1 ||
				broken.Bytes.Len() != int64(len(tc.line)):
				t.Errorf("%s, a line of %d bytes: read as fault %q, type %q, size %d, %d bytes read, %v; want %q, %q, "+
					"%d, %d, errNotOneRecord %t", name, len(tc.line), broken.Fault, broken.Type, broken.Size,
					broken.Bytes.Len(), broken.Err, tc.want.fault, tc.want.kind, len(tc.line)+1, len(tc.line),
					tc.want.notOne)
			default:
				if _, end := r.Next(t.Context()); !errors.Is(end, io.EOF) || r.Torn() {
					t.Errorf("%s, a line of %d bytes: after it the reading ended with %v, torn %t; want io.EOF",
						name, len(tc.line), end, r.Torn())
				}
			}
		}
	}
}

// A LINE IS READ ALIKE IN ANY PIECES: what a reading answers of every line of
// the corpus the byte facts are held to ([byteFactLines]) and of the lines
// across a piece's edge ([scanAcrossAFill]) does not turn on the pieces the
// member hands the scanner: of 16, 17, 31, 64 and 1,000 bytes at most, and
// whole, the member read whole, a byte at a time or in halves.
//
// Mutation: drop what of a piece the scanner had no room left for, and a
// value after a literal, an escape or a number a piece's edge cut reads as
// another.
func TestALineIsReadAlikeInAnyPieces(t *testing.T) {
	t.Parallel()
	lines := append(byteFactLines(t), scanAcrossAFill()...)
	text := append(bytes.Join(lines, []byte("\n")), '\n')
	want := scanReading(t.Context(), newScanReader(bytes.NewReader(text), threadPlainMax))
	if len(want) != len(lines)+1 {
		t.Fatalf("the corpus reads as %d answers, want one a line and its end", len(want))
	}
	for _, chunk := range []int{16, 17, 31, 64, 1000, lineChunkBytes} {
		for name, member := range map[string]func(io.Reader) io.Reader{
			"whole":          func(r io.Reader) io.Reader { return r },
			"a byte at once": iotest.OneByteReader,
			"in halves":      iotest.HalfReader,
		} {
			if name == "a byte at once" && chunk != 17 && chunk != lineChunkBytes {
				continue
			}
			got := scanReading(t.Context(), newScanReaderSize(member(bytes.NewReader(text)), threadPlainMax, chunk))
			for i := range min(len(got), len(want)) {
				if got[i] != want[i] {
					t.Fatalf("pieces of %d bytes at most, the member read %s: answer %d reads\n%swant\n%s", chunk,
						name, i+1, got[i], want[i])
				}
			}
			if len(got) != len(want) {
				t.Fatalf("pieces of %d bytes at most, the member read %s: %d answers, want %d", chunk, name,
					len(got), len(want))
			}
		}
	}
}

// endlessLine is a member that hands a brief's line and then one queue
// operation of size bytes, generated as it is read, never held, and asks
// cancel once it has handed cancelAt bytes.
type endlessLine struct {
	head     string
	size     int64
	handed   int64
	cancelAt int64
	cancel   func()
}

// Read hands the member's next bytes.
func (e *endlessLine) Read(p []byte) (int, error) {
	if e.handed == e.size {
		return 0, io.EOF
	}
	n := 0
	for ; n < len(p) && e.handed < e.size; n, e.handed = n+1, e.handed+1 {
		switch at := e.handed; {
		case at < int64(len(e.head)):
			p[n] = e.head[at]
		case at >= e.size-3:
			p[n] = "\"}\n"[at-(e.size-3)]
		default:
			p[n] = 'x'
		}
	}
	if e.handed >= e.cancelAt {
		e.cancel()
	}
	return n, nil
}

// A READING STOPS WITHIN A FILL OF ITS CONTEXT: Next asks its context before
// each piece of the member a line takes, so a context that ends part way
// through a line of 64 MiB ends the reading with its error having read at
// most one piece ([lineChunkBytes]) past the byte it ended at, counted in the
// member's bytes, never in time; the line before it was read whole, and the
// reading stays over whatever context a later Next is handed.
//
// Mutation: ask the context only before a line, and the whole 64 MiB line is
// read.
func TestAReadingStopsWithinAFillOfItsContext(t *testing.T) {
	t.Parallel()
	brief := ctThread(ctBrief(ctUUID(1)))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	head := brief + `{"type":"queue-operation","operation":"enqueue","content":"`
	member := &endlessLine{head: head, size: int64(len(brief)) + 64<<20, cancelAt: int64(len(brief)) + 1<<20,
		cancel: cancel}
	r := newScanReader(member, threadPlainMax)
	if line, err := r.Next(ctx); err != nil || line.Head.Type != recordUser {
		t.Fatalf("the brief before the long line reads as %q, %v", line.Head.Type, err)
	}
	if _, err := r.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the line its context ended inside reads as %v; want %v", err, context.Canceled)
	}
	if member.handed > member.cancelAt+lineChunkBytes {
		t.Errorf("the reading took %d bytes of the member, %d past where its context ended; want at most %d",
			member.handed, member.handed-member.cancelAt, lineChunkBytes)
	}
	if _, err := r.Next(t.Context()); !errors.Is(err, context.Canceled) {
		t.Errorf("a Next after the reading's context ended answered %v; want %v", err, context.Canceled)
	}
}

// A RESET READER ON THE SCANNER READS AS A FRESH ONE: one reader reads a
// session's members one after another ([scanReader.Reset]), so a member read
// after another reads exactly as a fresh reader reads it, its lines numbered
// from 1, its bound its own, ended only by what ends it, and torn only where it
// is, whatever the member before it left: a torn piece, the member's bound, a
// value past its bound, a line that is no record, an error of its reader, or
// a context that ended. And one reader reads a thousand members, one after
// another, allocating for each what the member's line costs, never its
// buffers again: a fresh reader's buffers are its member reader's and its
// scanner's, 64 KiB each.
//
// Not parallel: the allocation count is the process's.
//
// Mutation: keep how a reading ended across Reset, and the member after it
// reads as ended; or make a scanner each member, and each member costs one.
func TestAResetReaderOnTheScannerReadsAsAFreshOne(t *testing.T) {
	brief := ctBrief(ctUUID(1))
	next := ctThread(ctBrief(ctUUID(2)), "[1]", ctBrief(ctUUID(3)))
	within := int64(len(next)) + 1
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	text := func(body string) func() io.Reader {
		return func() io.Reader { return strings.NewReader(body) }
	}
	for name, tc := range map[string]struct {
		before   func() io.Reader
		maxBytes int64
		ctx      context.Context
	}{
		"a torn member":           {text(ctThread(brief, brief) + `{"type":"user"`), threadPlainMax, t.Context()},
		"a member past its bound": {text(ctThread(brief, brief)), int64(len(brief)) + 3, t.Context()},
		"a member holding a value past its bound": {text(ctThread(brief, `{"uuid":"`+
			strings.Repeat("u", threadValueMax)+`"}`, brief)), threadPlainMax, t.Context()},
		"a member holding a line that is no record": {text(ctThread(brief, "[1]", brief, brief)), within,
			t.Context()},
		"a member whose reader failed": {func() io.Reader {
			return io.MultiReader(strings.NewReader(ctThread(brief, brief)), iotest.ErrReader(io.ErrUnexpectedEOF))
		}, within, t.Context()},
		"a reading whose context ended": {text(ctThread(brief, brief)), within, ended},
	} {
		for _, bound := range []int64{within, within - 2} {
			r := newScanReader(tc.before(), tc.maxBytes)
			scanReading(tc.ctx, r)
			r.Reset(strings.NewReader(next), bound)
			got := strings.Join(scanReading(t.Context(), r), "")
			if want := strings.Join(scanReading(t.Context(), newScanReader(strings.NewReader(next), bound)), ""); got != want {
				t.Errorf("after %s, the next member read within %d bytes reads as\n%swant it read as a fresh "+
					"reader reads it\n%s", name, bound, got, want)
			}
		}
	}

	// A THOUSAND MEMBERS, ONE READER: the fewest bytes of five walks, so what
	// another goroutine allocates meanwhile counts against no claim, held
	// below an eighth of what a fresh reader's buffers take.
	member := ctThread(brief)
	r := newScanReader(strings.NewReader(""), 0)
	const members = 1000
	walk := func() {
		for range members {
			r.Reset(strings.NewReader(member), threadPlainMax)
			for {
				if _, err := r.Next(t.Context()); err != nil {
					break
				}
			}
		}
	}
	walk()
	each := uint64(math.MaxUint64)
	var before, after runtime.MemStats
	for range 5 {
		runtime.ReadMemStats(&before)
		walk()
		runtime.ReadMemStats(&after)
		each = min(each, (after.TotalAlloc-before.TotalAlloc)/members)
	}
	runtime.ReadMemStats(&before)
	fresh := newScanReader(strings.NewReader(member), threadPlainMax)
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(fresh)
	if buffers := after.TotalAlloc - before.TotalAlloc; buffers < 2*lineChunkBytes || each >= buffers/8 {
		t.Errorf("one reader reading a member after another allocates %d bytes a member, where a fresh reader's "+
			"buffers take %d; want those at least %d, and a member under an eighth of them", each, buffers,
			2*lineChunkBytes)
	}
}
