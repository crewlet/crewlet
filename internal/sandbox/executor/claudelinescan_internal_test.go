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
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"unsafe"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE DIFFERENTIAL: a line read in one scanner pass ([threadReader]) answers
// what the whole decode answers of it ([decodeThreadLine]): record or refused,
// the fault, the kind of record a refused head names, the size, whether the
// error is errNotOneRecord (a refused head's in its own words), the byte facts,
// and for a record its head, every fact and every accessor's answer, an error
// by its text and a list by what it replays. Both bounds are raised
// ([threadReader.valueMax], [threadReader.meterMax]), since the whole decode has
// neither; [TestALinesFactsAreHeldToTheBound] holds them. A line whose last
// type reads a frame passed over for an earlier type ends the reading, which
// the differential models from the line's keys in order ([scanWant]).

// scanAnswer is what a reading answered of one line, line and err, as text:
// for a record, its place, its size, its head, every small value the pass
// keeps as written and whether it carries taskDelivery, its byte facts, every
// answer of the accessors of its head and of its small values
// ([scanRecord]), of its message ([scanMessage]) and of its payloads and
// lists ([scanPayloads]);
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
	b.WriteString(scanRecord(line))
	b.WriteString(scanMessage(line))
	b.WriteString(scanPayloads(line))
	b.WriteString(scanEstimate(line))
	return b.String()
}

// scanEstimate is what a line's estimate's accessors answer, and the facts
// behind them field by field, as text: a message's figure always, its
// content's charge where the figure is filled (an object whose content its
// rule reads; a list refused part way charges what it read), an attachment's
// facts always.
func scanEstimate(line threadLine) string {
	f := line.facts()
	e, err := line.Estimate()
	ae, read, aerr := line.AttachmentEstimate()
	m := f.message
	s := fmt.Sprintf("estimate %+v, %v; attachment estimate %+v %t, %v; estimating %t\nmessage estimate %+v, %v\n",
		e, err, ae, read, aerr, f.estimating, m.estimate, m.estimateErr)
	if m.kind == '{' && m.err == nil && m.contentErr == nil {
		// A fault by its words: a number's kind is its first byte as written
		// in the whole decode, '0' in the pass, both "a number".
		s += fmt.Sprintf("message charge %+v, %v, empty text %t\n", m.charge.est, m.charge.fault.err(),
			m.emptyText)
	}
	a := f.attachment
	return s + fmt.Sprintf("attachment estimate facts: pages %+v, images %d, estimate %+v, estimated %t\n", a.pdf,
		a.images, a.estimate, a.estimated)
}

// scanRecord is every answer the accessors of line's head and of its small
// values give, as text, each with its error.
func scanRecord(line threadLine) string {
	var b strings.Builder
	h := line.Head
	fmt.Fprintf(&b, "chained %t, parentless %t, main %t\n", h.Chained(), h.Parentless(), h.Main())
	uuid, uuidErr := line.UUID()
	version, versionErr := line.Version()
	origin, originErr := line.OriginKind()
	apiError, apiErr := line.APIError()
	leaf, marked, leafErr := line.Checkpoint()
	keyed, keyedErr := line.LeafKeyed()
	fmt.Fprintf(&b, "uuid %q, %v; version %q, %v; origin %q, %v; api error %t, %v; checkpoint %q %t, %v; "+
		"leaf keyed %t, %v\n", uuid, uuidErr, version, versionErr, origin, originErr, apiError, apiErr, leaf, marked,
		leafErr, keyed, keyedErr)
	virtual, virtualErr := line.IsVirtual()
	shutdown, shutdownErr := line.InterruptedByShutdown()
	summary, summaryErr := line.IsCompactSummary()
	meta, metaErr := line.IsMeta()
	ms, stamped, stampErr := line.Timestamp()
	source, sourced := line.SourceToolAssistantUUID()
	fmt.Fprintf(&b, "virtual %t, %v; interrupted by shutdown %t, %v; compact summary %t, %v; meta %t, %v; "+
		"timestamp %d %t, %v; source %x %t; task delivery %t\n", virtual, virtualErr, shutdown, shutdownErr, summary,
		summaryErr, meta, metaErr, ms, stamped, stampErr, source, sourced, line.TaskDelivery())
	return b.String()
}

// scanPayloads is every fact a line keeps of its toolUseResult, attachment,
// compactMetadata, retractedMessageUuids, replacements and batchToolUses,
// each field by its own name, and every answer their accessors give, as
// text: an error by its text and a list by the sequence it replays, then its
// error.
func scanPayloads(line threadLine) string {
	var b strings.Builder
	f := line.facts()
	r := f.result
	fmt.Fprintf(&b, "toolUseResult facts: hands back %t, completed %t, agent %q, refused %t\n", r.handsBack,
		r.completed, r.agent, r.refused)
	a := f.attachment
	fmt.Fprintf(&b, "attachment facts: %v, %v; type %q, toolUseID %q, by %q, hookEvent %q; content %v, strings %t, "+
		"objects %t, tokens %b; stdout tokens %b; skills %t; names %t, %v; entries %t\n", a.kind, a.err,
		ctRaw(a.typ), ctRaw(a.toolUseID), ctRaw(a.by), ctRaw(a.event), a.content, a.contentStrings, a.contentObjects,
		a.contentTokens, a.stdoutTokens, a.skillsShaped, a.namesKept, a.namesErr, a.entriesKept)
	c := f.compacted
	fmt.Fprintf(&b, "compactMetadata facts: %v, %v; preservedMessages %v, %s, %v; preservedSegment %q\n", c.kind,
		c.err, c.messages, scanPreserved(c.list), c.listErr, ctRaw(c.segment))
	fmt.Fprintf(&b, "retractedMessageUuids facts: %s, %v\n", scanRetracted(f.retracted), f.retractedErr)
	rs := f.replacements
	fmt.Fprintf(&b, "replacements facts: %v, %d, %q, %v\n", rs.kind, rs.count, rs.ids, rs.err)
	fmt.Fprintf(&b, "batchToolUses facts: %t, %q, %v\n", f.batch.present, f.batch.ids, f.batch.err)

	completed, agent, refused := line.AgentAnswer()
	typ, typErr := line.AttachmentType()
	key, isString := line.AttachmentToolUseID()
	kept, keptErr := line.AttachmentWellFormed()
	fmt.Fprintf(&b, "hands back %t; answer %t %q %t; attachment type %q, %v; tool use key %x %t; well formed %t, %v\n",
		line.HandsBack(), completed, agent, refused, typ, typErr, key, isString, kept, keptErr)
	event, eventErr := line.HookEvent()
	contentTakes, stdoutTakes := line.HookTakes()
	fmt.Fprintf(&b, "hook event %q, %v; hook takes %b %b\n", event, eventErr, contentTakes, stdoutTakes)
	preserved, preservedErr := line.Preserved()
	retracted, retractedErr := line.Retracted()
	fmt.Fprintf(&b, "preserved %s, %v; retracted %s, %v\n", scanPreserved(preserved), preservedErr,
		scanRetracted(retracted), retractedErr)
	var replaced, batched []string
	replacedErr := line.ReplacedResults(func(id string) { replaced = append(replaced, id) })
	count, countErr := line.ReplacementCount()
	batch, batchErr := line.BatchToolUses(func(id string) { batched = append(batched, id) })
	fmt.Fprintf(&b, "replaced %q, %v; %d replacements, %v; batch %q %t, %v\n", replaced, replacedErr, count, countErr,
		batched, batch, batchErr)
	return b.String()
}

// scanPreserved is p, every field, as text.
func scanPreserved(p threadPreserved) string {
	return fmt.Sprintf("{named %t, kept %t, anchor %q, segment %t, head %q, tail %q, listed %d, uuids %x, more %t, "+
		"nowhere %t}", p.Named, p.Kept, p.Anchor, p.Segment, p.Head, p.Tail, p.Listed, p.UUIDs, p.More, p.Nowhere)
}

// scanRetracted is r, every field and what Prefixes answers, as text.
func scanRetracted(r threadRetracted) string {
	held, all := r.Prefixes()
	return fmt.Sprintf("{entries %d, prefixes %x, more %t; prefixes %x, all %t}", r.Entries, r.prefixes, r.more, held,
		all)
}

// scanMessage is every answer the accessors of line's message give, as text,
// each by the accessor its readers call, never by the facts behind it: the
// message or its error, and of a message whether it is one text, the
// notification it names, whether it names the handback tool, its blocks
// counted by kind, what it says and each call, its target, its failure and
// its text's length and digests; and its stop reason, model, id, usage and
// cleared results, each with its error.
func scanMessage(line threadLine) string {
	var b strings.Builder
	if m, err := line.Message(); err != nil {
		fmt.Fprintf(&b, "message: %v\n", err)
	} else {
		agent, leg, notice := m.Notification()
		text, any := m.Says()
		fmt.Fprintf(&b, "message: one text %t, notification %q %q %t, names the handback %t, %d blocks of kinds "+
			"%06b, says text %t, says anything %t\n", m.IsText(), agent, leg, notice, m.NamesHandback(),
			m.Blocks().Len(), m.Blocks().seen, text, any)
		for _, c := range m.Calls() {
			fmt.Fprintf(&b, "call: %q %q %q to %q, failed %t, text of %d bytes, opening %x, whole %x\n", c.Kind,
				c.ID, c.Name, c.To, c.Failed, c.Text.Bytes(), c.Text.Opening, c.Text.Whole)
		}
	}
	stop, stopErr := line.StopReason()
	model, modelErr := line.Model()
	id, idErr := line.MessageID()
	usage, used, usageErr := line.Usage()
	cleared, clearedErr := line.ToolResultsCleared()
	fmt.Fprintf(&b, "stop reason %q, %v; model %q, %v; id %q, %v; usage %+v %t, %v; cleared %t, %v\n", stop,
		stopErr, model, modelErr, id, idErr, usage, used, usageErr, cleared, clearedErr)
	return b.String()
}

// scanBytes is every byte fact of b, as text.
func scanBytes(b threadBytes) string {
	return fmt.Sprintf("%d bytes, head %q, slot keyed %t, keyed %t by %q, marks %b", b.Len(), b.Head(), b.slotKeyed,
		b.keyed, b.key[:], b.marks)
}

// scanReading is what a [threadReader] made of a member: each answer of each
// line, in order, read to the member's end BEFORE any is printed, so a line
// that shared a buffer with a later one reads otherwise; then what ended the
// reading, what one more Next answered, and whether the member ended torn.
func scanReading(ctx context.Context, r *threadReader) []string {
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

// checkScanReadsAsDecode reads text, split at its breaks, with a [threadReader]
// taking at most chunk bytes of it at a time, its bounds on a value and on a
// line's facts raised, and holds each line it reads to what the whole decode
// reads of the same line ([scanWant]): the same answer ([scanAnswer]), or the
// same end of the reading where the pass passes over a frame the line's type
// reads. The lines are read as one member up to each line that ends a
// reading, the lines after it as the next member of the same reader
// ([threadReader.Reset]); and the last member's end is torn exactly where a
// piece with no break ends it.
func checkScanReadsAsDecode(t testing.TB, text []byte, chunk int) {
	t.Helper()
	pieces := bytes.Split(text, []byte("\n"))
	lines, rest := pieces[:len(pieces)-1], pieces[len(pieces)-1]
	r := newThreadReaderSize(bytes.NewReader(nil), 0, chunk)
	r.valueMax, r.meterMax = math.MaxInt, math.MaxInt
	for from := 0; ; {
		// The member: the lines up to the first a reading ends at, or to the
		// last, and then what follows the last break.
		var (
			wants  []string
			member []byte
			end    string
		)
		to := from
		for ; to < len(lines) && end == ""; to++ {
			member = append(append(member, lines[to]...), '\n')
			want, ends := scanWant(to-from+1, lines[to])
			if ends != nil {
				end = fmt.Sprintf("ended by %v, then %v, torn false\n", ends, ends)
				continue
			}
			wants = append(wants, want)
		}
		last := end == ""
		if last {
			member = append(member, rest...)
			end = fmt.Sprintf("ended by EOF, then EOF, torn %t\n", len(rest) > 0)
		}
		r.Reset(bytes.NewReader(member), int64(len(member)), threadLinesMax)
		if err := r.Asking(scanAsked); err != nil {
			t.Fatal(err)
		}
		r.Estimating()
		got := scanReading(t.Context(), r)
		if len(got) != len(wants)+1 || got[len(wants)] != end {
			t.Fatalf("lines %d to %d, the last %.300q, read in pieces of %d bytes at most, read as %d answers "+
				"ending %q; want %d and %q", from+1, to, member[max(0, len(member)-301):], chunk, len(got)-1,
				got[len(got)-1], len(wants), end)
		}
		for i, want := range wants {
			if got[i] != want {
				line := lines[from+i]
				t.Fatalf("line %d, %.300q of %d bytes, read in one scanner pass in pieces of %d bytes at most:\n%s"+
					"read whole:\n%s", from+i+1, line, len(line), chunk, got[i], want)
			}
		}
		if last {
			return
		}
		from = to
	}
}

// scanWant is what a reading in one pass answers of raw, line n of its
// member, as the whole decode reads it ([scanAnswer]); or, where the line's
// last type reads a frame and the pass passed that frame over for the type
// the line wrote before it ([scanPassedOver]), the error that ends the
// reading there, naming the first such frame of [scanGates].
func scanWant(n int, raw []byte) (string, error) {
	line, err := decodeThreadLine(n, raw, scanAsked)
	if err != nil {
		return scanAnswer(line, err), nil
	}
	for _, g := range scanGates {
		if slices.Contains(g.types, line.Head.Type) && scanPassedOver(raw, g.frame, g.types...) {
			return "", fmt.Errorf("%w: line %d's %s came before a later type made it a record that reads it",
				errThreadBound, n, g.frame)
		}
	}
	return scanAnswer(line, nil), nil
}

// scanAsked is the take tokens the differential asks of every line
// ([threadReader.Asking]), so a hook's words naming one, both or a near miss of
// either are read by both readings ([scanPayloadLines]).
var scanAsked = []string{claudeTakeToken(ctUUID(7)), claudeTakeToken(ctUUID(8))}

// scanGates is each frame of a record a reading in one pass reads only on
// records of some types, and those types: those of the records each frame's
// accessors answer on, by type alone (a compaction's metadata is read on a
// system record of any subtype until the record ends), in the order a
// reading names the first it passed over.
var scanGates = []struct {
	frame string
	types []string
}{
	{"message", []string{recordUser, recordAssistant}},
	{"toolUseResult", []string{recordUser}},
	{"attachment", []string{recordAttachment}},
	{"compactMetadata", []string{recordSystem}},
	{"retractedMessageUuids", []string{recordUser, recordAssistant, recordSystem}},
	{"replacements", []string{recordContentReplacement}},
	{"batchToolUses", []string{recordAssistant}},
}

// scanPassedOver reports whether raw, one record, writes its last key named
// frame after a `type` that is a string naming none of types, its keys read
// in order ([eachKey]), as no reading of the whole decode reads them: the
// frame a reading in one pass passes over before it knows the record's type.
func scanPassedOver(raw []byte, frame string, types ...string) bool {
	var typ json.RawMessage
	passed := false
	_ = eachKey(raw, func(name []byte, value json.RawMessage, _ int) error {
		switch string(name) {
		case "type":
			typ = slices.Clone(value)
		case frame:
			s, err := wireRequiredString("type", typ)
			passed = err == nil && !slices.Contains(types, s)
		}
		return nil
	})
	return passed
}

// A LINE READ IN ONE SCANNER PASS IS READ AS ITS WHOLE DECODE READS IT: every
// line of the corpus ([scanCorpus]) answers what the whole decode answers
// ([scanAnswer]), and a line whose type reads a frame passed over for the type
// before it ends the reading there. The corpus is read as one member to each
// line that ends a reading, so nothing of a line reaches the next, each answer
// printed only once its member is read; whole and in pieces of 17 bytes, so
// every token of a short line meets a piece's edge somewhere. The mutations
// it kills are listed in the commits that built the pass.
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
	for _, line := range slices.Concat(scanMessageLines(), scanPayloadLines(), scanEstimateLines()) {
		seeds = append(seeds, string(line))
	}
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
// is read; records whose messages, payloads and lists hold what their
// readers read at their edges ([scanMessageLines], [scanPayloadLines]), and
// the estimate's ([scanEstimateLines]); each kind of record and each frame
// below it mutated at every key the pass reads ([scanMutations]); and lines
// whose keys, a key written twice or a fault fall across the edge of what the
// member hands at once ([scanAcrossAFill]).
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
	lines = append(lines, scanMessageLines()...)
	lines = append(lines, scanPayloadLines()...)
	lines = append(lines, scanEstimateLines()...)
	lines = append(lines, scanMutations(lines)...)
	return append(lines, scanAcrossAFill()...)
}

// scanReadKeys is every key of a record a reading of it reads: the head's,
// the small values kept as written, taskDelivery, and the frames.
var scanReadKeys = []string{"type", "subtype", "agentId", "parentUuid", "isSidechain", "replacesSpan", "uuid",
	"version", "origin", "isApiErrorMessage", "leafUuid", "explicit", "rewound", "isVirtual", "interruptedByShutdown",
	"isCompactSummary", "timestamp", "isMeta", "sourceToolAssistantUUID", "taskDelivery", "message", "toolUseResult",
	"attachment", "compactMetadata", "retractedMessageUuids", "replacements", "batchToolUses", "rendered"}

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

// scanFrames is each frame a reading reads below a record, by its path from
// the record (a key; "*" every element of a list; "0" its first), and every
// key a reading reads of it.
var scanFrames = []struct{ path, keys []string }{
	{[]string{"message"}, []string{"content", "id", "model", "stop_reason", "usage", "context_management"}},
	{[]string{"message", "content", "*"}, []string{"type", "id", "name", "input", "tool_use_id", "is_error",
		"content", "text", "thinking", "data", "title", "source"}},
	{[]string{"message", "content", "*", "input"}, []string{"to"}},
	{[]string{"message", "content", "*", "content", "0"}, []string{"type", "text"}},
	{[]string{"message", "content", "*", "content", "*"}, []string{"type", "text", "thinking", "data", "name",
		"input", "title", "source", "content"}},
	{[]string{"message", "content", "*", "source"}, []string{"type", "data", "content"}},
	{[]string{"message", "content", "*", "content", "*", "source"}, []string{"type", "data", "content"}},
	{[]string{"message", "content", "*", "source", "content", "*"}, []string{"type", "text", "title", "source",
		"content"}},
	{[]string{"taskDelivery"}, []string{"mcpTaskSnapshot", "result"}},
	{[]string{"taskDelivery", "mcpTaskSnapshot"}, []string{"result"}},
	{[]string{"taskDelivery", "result"}, []string{"content"}},
	{[]string{"taskDelivery", "result", "content", "*"}, []string{"type"}},
	{[]string{"attachment", "taskDelivery"}, []string{"mcpTaskSnapshot", "result"}},
	{[]string{"attachment", "reads", "*"}, []string{"pages"}},
	{[]string{"rendered", "*"}, []string{"content"}},
	{[]string{"rendered", "*", "content", "*"}, []string{"type", "text"}},
	{[]string{"toolUseResult"}, []string{"handback", "handbackReport", "status", "agentId", "success"}},
	{[]string{"attachment"}, []string{"type", "toolUseID", "by", "hookEvent", "content", "stdout", "skills", "names",
		"entries", "reads", "taskDelivery"}},
	{[]string{"attachment", "entries", "*"}, []string{"path", "displayPath", "why"}},
	{[]string{"compactMetadata"}, []string{"preservedMessages", "preservedSegment"}},
	{[]string{"compactMetadata", "preservedMessages"}, []string{"anchorUuid", "uuids"}},
	{[]string{"replacements", "*"}, []string{"kind", "toolUseId"}},
	{[]string{"batchToolUses", "*"}, []string{"id"}},
}

// scanMutations is a record of each kind among lines (one a type and
// subtype), mutated at every key a reading reads ([scanReadKeys]), and each
// frame below a record of each shape among lines ([scanFrames]) mutated at
// every key a reading reads of it, the rest of its record as written: each
// as [scanMutateObject] mutates an object.
func scanMutations(lines [][]byte) [][]byte {
	var out [][]byte
	seen := map[string]bool{}
	for _, line := range lines {
		members, ok := scanMembersOf(line)
		if !ok {
			continue
		}
		shape := ""
		for _, m := range members {
			if m.name == "type" || m.name == "subtype" {
				shape += m.name + "=" + m.value + " "
			}
		}
		if !seen[shape] {
			seen[shape] = true
			out = append(out, scanMutateObject(members, scanReadKeys)...)
		}
		for _, frame := range scanFrames {
			out = append(out, scanMutateAt(line, frame.path, frame.keys, shape+strings.Join(frame.path, "."), seen)...)
		}
	}
	return out
}

// scanMutateAt is raw, a value, mutated at the object path leads to from it
// ([scanFrames]) as [scanMutateObject] mutates it at keys, the rest of raw as
// written: at every object path leads to whose shape (shape, beside the
// object's own `type` and its `content`'s kind) is not yet in seen, which
// takes it.
func scanMutateAt(raw []byte, path, keys []string, shape string, seen map[string]bool) [][]byte {
	members, isObject := scanMembersOf(raw)
	if len(path) == 0 {
		if !isObject {
			return nil
		}
		for _, m := range members {
			switch m.name {
			case "type":
				shape += " type=" + m.value
			case "content":
				shape += " content=" + m.value[:1]
			}
		}
		if seen[shape] {
			return nil
		}
		seen[shape] = true
		return scanMutateObject(members, keys)
	}
	var out [][]byte
	if isObject {
		for i, m := range members {
			if m.name != path[0] {
				continue
			}
			for _, mutated := range scanMutateAt([]byte(m.value), path[1:], keys, shape, seen) {
				changed := slices.Clone(members)
				changed[i].value = string(mutated)
				out = append(out, scanObject(changed))
			}
		}
		return out
	}
	var elements [][]byte
	if eachElement(raw, func(element json.RawMessage, _ int) error {
		elements = append(elements, slices.Clone(element))
		return nil
	}) != nil {
		return nil
	}
	for i, element := range elements {
		if path[0] == "0" && i > 0 {
			break
		}
		for _, mutated := range scanMutateAt(element, path[1:], keys, shape, seen) {
			changed := slices.Clone(elements)
			changed[i] = mutated
			out = append(out, append(append([]byte{'['}, bytes.Join(changed, []byte(","))...), ']'))
		}
	}
	return out
}

// scanMembersOf is raw's members, in order, where it is one object.
func scanMembersOf(raw []byte) ([]scanMember, bool) {
	var members []scanMember
	if eachKey(raw, func(name []byte, value json.RawMessage, _ int) error {
		members = append(members, scanMember{name: string(name), value: string(value)})
		return nil
	}) != nil {
		return nil, false
	}
	return members, true
}

// scanMutateObject is an object of members mutated at each of keys: each
// occurrence of the key moved first and moved last, the others in place; and
// a value of every kind written before the object's own and after it, so a
// key written twice is read wholly by its last, whichever of the two is the
// one the object wrote, and every key is read of every kind; a key the object
// lacks among them; and after the object's own, a value of every kind under
// the key's name with a letter's case changed, which is another key, and
// under its name with a letter escaped, which is the key.
func scanMutateObject(members []scanMember, keys []string) [][]byte {
	var out [][]byte
	for _, key := range keys {
		var own, others []scanMember
		for _, m := range members {
			if m.name == key {
				own = append(own, m)
			} else {
				others = append(others, m)
			}
		}
		for _, m := range own {
			out = append(out, scanObject(append([]scanMember{m}, others...)),
				scanObject(append(slices.Clone(others), m)))
		}
		for _, kind := range scanKinds {
			written := scanMember{name: key, value: kind}
			out = append(out, scanObject(append([]scanMember{written}, members...)),
				scanObject(append(slices.Clone(members), written)))
			for _, spelled := range scanSpellings(key) {
				out = append(out, scanObject(append(slices.Clone(members), scanMember{value: kind, quoted: spelled})))
			}
		}
	}
	return out
}

// scanMessageLines is records whose messages hold what a message's readers
// read at its edges: a task notification whose status is its last line and
// one whose output file outgrows a piece, the handback's name across a
// piece's edge, texts of space alone as JavaScript's trim() and Go's read it
// apart, blocks whose text says something under every kind and one whose
// text is written twice, sends to an agent and to none, a block after one
// that held a key it lacks, results whose text is around 4 KiB and around
// what a piece holds, every shape of a result's list, every refusal of a
// block and a block refused after one that was not, calls of the kind the
// record does not make, messages and contents of every kind and written
// twice, a record whose type the line writes before its message, after it,
// escaped and twice, and an applied context edit.
func scanMessageLines() [][]byte {
	quote := func(s string) string {
		b, _ := jsontext.AppendQuote(nil, s)
		return string(b)
	}
	user := func(content string) string {
		return `{"type":"user","message":{"role":"user","content":` + content + `}}`
	}
	assistant := func(content string) string {
		return `{"message":{"role":"assistant","content":` + content + `},"type":"assistant"}`
	}
	result := func(content string) string {
		return user(`[{"type":"tool_result","tool_use_id":"toolu_r","content":` + content + `}]`)
	}
	notice := "<task-notification>\n<task-id>a1b2c3</task-id>\n<tool-use-id>toolu_01</tool-use-id>\n"
	status := "<status>completed</status>"
	lines := []string{
		user(quote(notice + status)),
		user(quote(notice + "<output-file>" + strings.Repeat("o", 9000) + "</output-file>\n" + status + "\n" +
			claudeToolHandback)),
		user(quote(notice + status + "\n" + strings.Repeat("z", textPieceBytes-8) + claudeToolHandback)),
		user(quote(notice + "<output-file></output-file>\n" + strings.Repeat("s", textPieceBytes) + status)),
		user(`"\u00a0\ufeff\u2028 \t"`), user(`"\u0085"`), user(`" "`), user(`""`),
		assistant(`[{"type":"text","text":" "},{"type":"thinking","thinking":"t","text":"hi"}]`),
		assistant(`[{"type":"thinking","text":"hi"},{"type":"text","text":"ok"}]`),
		assistant(`[{"text":"hi","type":"text","text":" "}]`),
		assistant(`[{"text":" ","type":"text","text":"hi"}]`),
		assistant(`[{"type":"text","text":5},{"type":"redacted_thinking","data":"d"},{"type":"server_tool_use"}]`),
		assistant(`[{"type":"text","text":"` + strings.Repeat(" ", 2*textPieceBytes) + `x"}]`),
		assistant(`[{"type":"tool_use","id":"toolu_s","name":"SendMessage","input":{"to":"agent-1","message":"m"}},` +
			`{"type":"tool_use","id":"toolu_b","name":"Bash"}]`),
		assistant(`[{"type":"tool_use","id":"toolu_s","name":"SendMessage","input":{"to":"a/b"}},` +
			`{"type":"tool_use","id":"toolu_t","name":"SendMessage","input":{"to":"agent-1","to":7}},` +
			`{"type":"tool_use","id":"toolu_u","name":"SendMessage","input":"agent-1"},` +
			`{"type":"tool_use","id":"toolu_v","name":"SendMessage","input":{"to":"agent-1"},"input":{}}]`),
		user(`[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"e"},` +
			`{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"` +
			strings.Repeat("y", 5000) + `"}]}]`),
		result(quote(strings.Repeat("r", 4095))), result(quote(strings.Repeat("r", 4096))),
		result(quote(strings.Repeat("r", 4097))), result(quote(strings.Repeat("r", 2*textPieceBytes+1))),
		result(`"a\u00e9\ud83d\ude00\ud800 \u0000"`), result("\"\xff\xfe\""),
		result(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`), result(`[1]`), result(`[]`),
		result(`[1,{"type":"text","text":"b"}]`), result(`[{"text":"a","type":"\u0074ext"}]`),
		result(`[{"type":"text","text":"a","text":5}]`), result(`[{"type":"text","text":"a","text":"bb"}]`),
		result(`[{"type":"image"}]`), result(`[{"type":"Text","text":"a"}]`), result(`null`), result(`{}`),
		result(`[{"type":"text","text":"` + strings.Repeat("q", 5000) + `"}]`),
		user(`[{"type":"tool_result","tool_use_id":"toolu_r","content":"a","content":[]}]`),
		assistant(`[{"type":"text"},1,{"type":5}]`), assistant(`[{"type":5}]`), assistant(`[null]`),
		assistant(`[{"type":"tool_use","name":"n"}]`), assistant(`[{"type":"tool_use","id":"","name":"n"}]`),
		assistant(`[{"type":"tool_use","id":"t","name":7}]`),
		user(`[{"type":"tool_result","tool_use_id":"t","is_error":"x"}]`),
		user(`[{"type":"tool_result","tool_use_id":7}]`),
		user(`[{"type":"tool_use","id":"toolu_u","name":"n"},{"type":"tool_result","tool_use_id":"toolu_r"}]`),
		assistant(`[{"type":"tool_result","tool_use_id":"toolu_r"},{"type":"tool_use","id":"toolu_u","name":"n"}]`),
		`{"type":"user","message":null}`, `{"type":"assistant","message":"x"}`, `{"type":"assistant","message":{}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"n"}],"id":"m","model":"x",` +
			`"stop_reason":"end_turn","usage":{"output_tokens":1}},"message":{"content":[]}}`,
		`{"type":"user","message":{"content":[1],"content":"ok"}}`,
		`{"type":"user","message":{"content":"ok","content":[{"type":"tool_result","tool_use_id":"t"}]}}`,
		`{"type":"assistant","message":{"content":[],"context_management":{"applied_edits":[{"type":"` +
			editClearToolUses + `","cleared_tool_uses":2}]},"usage":{"input_tokens":3,"output_tokens":1}}}`,
		`{"type":"api-request-blob","message":{"content":[{"type":"image","source":{"data":"` +
			strings.Repeat("A", 1000) + `"}}]}}`,
		`{"type":"user","message":{"content":"hi"},"type":"x"}`,
		`{"type":7,"message":{"content":"hi"},"type":"user"}`,
		`{"type":"\u0075ser","message":{"content":"hi"}}`,
		`{"type":"x","message":{"content":"a"},"type":"user","message":{"content":"b"}}`,
		`{"type":"x","message":{"content":"hi"},"type":"user"}`,
		`{"type":"user","message":{"content":"a"},"type":"x","message":{"content":"b"},"type":"assistant"}`,
		`{"type":"` + strings.Repeat("u", 100) + `","message":{"content":"hi"},"type":"user"}`,
	}
	out := make([][]byte, len(lines))
	for i, line := range lines {
		out[i] = []byte(line)
	}
	return out
}

// scanPayloadLines is records whose payloads and lists hold what their
// readers read at its edges: a leg's result and its handback in every shape
// beside a base64 longer than a piece; an attachment of every type its shape
// rule reads, met and missed, skill listings at both bounds and names whose
// bytes are no UTF-8; withheld memories of every refused entry; kept lists
// refused at an entry or the anchor, entries about a uuid's length, escaped,
// past ASCII and longer than a piece; retractions about a uuid's first 24
// characters; replacements and batch entries refused after one read, written
// twice and of every kind; and each frame on a record of a type that does not
// read it, or after such a type on a record whose last type does.
func scanPayloadLines() [][]byte {
	u := ctUUID
	quote := func(s string) string {
		b, _ := jsontext.AppendQuote(nil, s)
		return string(b)
	}
	result := func(own string) string {
		return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_r",` +
			`"content":"done"}]},"toolUseResult":` + own + `}`
	}
	attached := func(payload string) string { return ctAttachment(u(1), u(2), payload, "") }
	names := func(names ...string) string {
		return attached(`{"type":"skill_listing","names":[` + strings.Join(names, ",") + `]}`)
	}
	boundary := func(meta string) string {
		return `{"type":"system","subtype":"compact_boundary","uuid":"` + u(10) + `","compactMetadata":` + meta + `}`
	}
	kept := func(uuids ...string) string {
		return boundary(`{"preservedMessages":{"anchorUuid":"` + u(11) + `","uuids":[` + strings.Join(uuids, ",") +
			`]}}`)
	}
	reply := func(batch string) string {
		return `{"type":"assistant","message":{"role":"assistant","content":[]},"batchToolUses":` + batch + `}`
	}
	many := func(name string, n int) string {
		return strings.TrimSuffix(strings.Repeat(name+",", n), ",")
	}
	long := strings.Repeat("n", 2*textPieceBytes)
	take1, take2 := scanAsked[0], scanAsked[1]
	// Entries about the edges of what a reader reads of one: a uuid, one byte
	// short of it and one past it, its first 24 characters alone and one
	// short of them, one in capitals, one with a letter escaped, one past
	// ASCII inside its first 24 bytes, one of no UTF-8 there, the empty one
	// and one longer than a piece.
	entries := []string{quote(u(1)), quote(u(2)[:35]), quote(u(3) + "x"), quote(u(4)[:24]), quote(u(5)[:23]),
		quote(strings.ToUpper("abcdef00-0000-4000-8000-000000000006")), `"\u0030` + u(7)[1:] + `"`,
		quote("0000000\u00e9" + u(8)[8:]), "\"0000\xff" + u(9)[5:] + `"`, `""`, quote(u(12) + long)}
	lines := []string{
		result(`{"status":"completed","agentId":"a1b2c3","totalTokens":3}`),
		result(`{"status":"completed","agentId":"a/b"}`),
		result(`{"status":"failed","agentId":"a1b2c3","status":"completed"}`),
		result(`{"status":"completed","status":7}`),
		result(`{"success":false}`), result(`{"success":"false"}`), result(`{"success":false,"success":true}`),
		result(`{"handback":null}`), result(`{"stdout":"` + long + `","handbackReport":{"x":1}}`),
		result(`{"Handback":1}`),
		result(`"text"`), result(`[{"handback":1}]`), result(`null`), result(`{}`),
		result(`{"file":{"base64":"` + strings.Repeat("A", 3*textPieceBytes) +
			`"},"status":"completed","agentId":"a1b2c3"}`),
		`{"toolUseResult":{"status":"completed","agentId":"a1b2c3"},"type":"user"}`,
		`{"toolUseResult":{"handback":1},"type":"assistant"}`,
		`{"type":"user","toolUseResult":{"handback":1},"toolUseResult":{}}`,
		`{"type":"assistant","toolUseResult":{"handback":1},"type":"user"}`,

		attached(`{"type":"invoked_skills","skills":[{"name":"a"},["b"]]}`),
		attached(`{"type":"invoked_skills","skills":[{},1]}`),
		attached(`{"type":"invoked_skills"}`), attached(`{"type":"invoked_skills","skills":{}}`),
		attached(`{"type":"invoked_skills","skills":[1],"skills":[]}`),
		attached(`{"type":"hook_success","content":"ok","toolUseID":"toolu_h"}`),
		attached(`{"type":"hook_success","content":["ok"]}`),
		attached(`{"type":"hook_success","content":"x","content":7}`),
		attached(`{"content":"x","type":"hook_success"}`),
		attached(`{"type":"hook_additional_context","content":["a","b"]}`),
		attached(`{"type":"hook_additional_context","content":["a",1]}`),
		attached(`{"type":"hook_additional_context","content":[]}`),
		attached(`{"type":"hook_additional_context","content":"a"}`),
		attached(`{"type":"task_reminder","content":[{}]}`), attached(`{"type":"task_reminder","content":[{},[]]}`),
		attached(`{"type":"todo_reminder","content":[{"a":1}]}`), attached(`{"type":"todo_reminder","content":"x"}`),
		attached(`{"type":"file","content":{"text":"` + long + `"}}`), attached(`{"type":"file","content":[]}`),
		attached(`{"type":"already_read_file","content":{}}`),
		attached(`{"type":"skill_listing"}`), attached(`{"type":"skill_listing","names":null}`),
		attached(`{"type":"skill_listing","names":[]}`), attached(`{"type":"skill_listing","names":["a"],"names":[1]}`),
		names(`"a"`, `"b"`), names(`"a"`, `1`), names(many(`"a"`, skillListingMaxNames)),
		names(many(`"a"`, skillListingMaxNames+1)),
		names(quote(strings.Repeat("n", skillListingMaxNameUnits))),
		names(quote(strings.Repeat("n", skillListingMaxNameUnits+1))),
		names(quote(strings.Repeat("\U0001F600", skillListingMaxNameUnits/2))),
		names(quote(strings.Repeat("\U0001F600", skillListingMaxNameUnits/2+1))),
		names("\"" + strings.Repeat("n", skillListingMaxNameUnits) + "\xff\""),
		names("\"\xff" + strings.Repeat("n", skillListingMaxNameUnits) + `"`),
		names("\"" + long + "\xff\""), names("\"\xff\""),
		names("\""+strings.Repeat("n", skillListingMaxNameUnits)+"\xff\"", quote(strings.Repeat("n", 600))),
		names("\"" + strings.Repeat("n", skillListingMaxNameUnits) + "\xef\xbf\xbd\""),
		names(`"` + strings.Repeat("n", skillListingMaxNameUnits) + `\ufffd"`),
		attached(ctWithheld(`{"path":"a","displayPath":"a","why":"denied"}`,
			`{"path":"b","displayPath":"b","why":"outside"}`)),
		attached(ctWithheld(`{"path":"a","displayPath":"a","why":"other"}`)),
		attached(ctWithheld(`{"path":"a","displayPath":"a","why":7}`)),
		attached(ctWithheld(`{"path":7,"displayPath":"a","why":"denied"}`)),
		attached(ctWithheld(`{"path":"a","why":"denied"}`)),
		attached(ctWithheld(`1`, `{"path":"a","displayPath":"a","why":"denied"}`)),
		attached(ctWithheld(`{"path":"a","displayPath":"a","why":"x","why":"unjudged"}`)),
		attached(ctWithheld(`{"path":"a","displayPath":"a","why":"unsettled","why":"x"}`)),
		attached(ctWithheld(`{"why":"denied","displayPath":"a","path":"a"}`, `{"path":"a","displayPath":"a"}`)),
		attached(`{"type":"withheld_memory","by":"","entries":[]}`),
		attached(`{"type":"withheld_memory","entries":[]}`),
		attached(`{"type":"withheld_memory","by":"x","entries":{}}`),
		attached(`"x"`), attached(`null`), attached(`[]`), attached(`{"type":7}`), attached(`{}`),
		attached(`{"toolUseID":7,"type":"hook_success","content":"x"}`),
		attached(`{"type":"a","by":"b","toolUseID":"c"}`),
		// A hook's words naming the asked take tokens ([scanAsked]): in its
		// content and a hook_success's stdout, whichever key comes first; one
		// across the edge of a piece; one with its bracket escaped; near misses;
		// a stdout of another type, or of a type a later one replaces.
		attached(`{"type":"hook_success","hookEvent":"PostToolBatch","content":` + quote("a note "+take1) +
			`,"stdout":` + quote(`{"systemMessage":"from ada `+take2+`"}`) + `}`),
		attached(`{"stdout":` + quote(take1+take2+" and more") + `,"hookEvent":"Stop","type":"hook_success"}`),
		attached(`{"type":"hook_success","content":` + quote(strings.Repeat("n", textPieceBytes-20)+take1+take2) + `}`),
		attached(`{"type":"hook_success","content":"\u005bcrewlet take ` + u(7) + `]"}`),
		attached(`{"type":"hook_success","content":` + quote(strings.ToUpper(take1)+take1[:len(take1)-1]) + `}`),
		attached(`{"type":"hook_additional_context","content":[` + quote(take1) + `],"stdout":` + quote(take1) + `}`),
		attached(`{"type":"hook_success","stdout":` + quote(take1) + `,"type":"hook_cancelled"}`),
		attached(`{"type":"hook_success","stdout":` + quote(take1) + `,"stdout":"none"}`),
		attached(`{"type":"hook_success","stdout":7,"stdout":` + quote(take2) + `,"hookEvent":7}`),
		attached(`{"type":"hook_success","content":` + quote(take2) + `,"content":["x"]}`),
		`{"attachment":{"type":"hook_success","content":"x"},"type":"attachment"}`,
		`{"type":"attachment","attachment":{"type":"hook_success","content":"x"},"attachment":"x"}`,
		`{"attachment":{"type":"hook_success","content":"x"},"type":"user"}`,
		`{"type":"user","attachment":{"type":"hook_success","content":"x"},"type":"attachment"}`,

		ctBoundary(u(10), u(11), u(1), u(2)), kept(entries...), kept(entries[0], `7`, entries[0]),
		kept(), kept(`null`), kept(entries[0], `{}`),
		boundary(`{"preservedMessages":{"uuids":["` + u(1) + `",7]}}`),
		boundary(`{"preservedMessages":{"uuids":["` + u(1) + `"],"anchorUuid":7}}`),
		boundary(`{"preservedMessages":{"uuids":[7],"anchorUuid":"` + u(11) + `"}}`),
		boundary(`{"preservedMessages":{"anchorUuid":"` + u(11) + `"}}`),
		boundary(`{"preservedMessages":{"anchorUuid":"` + u(11) + `","uuids":null}}`),
		boundary(`{"preservedMessages":{"anchorUuid":"` + u(11) + `","uuids":"x"}}`),
		boundary(`{"preservedMessages":{"anchorUuid":"` + u(11) + `","uuids":["` + u(1) + `"],"uuids":["` + u(2) +
			`",1]}}`),
		boundary(`{"preservedMessages":{"anchorUuid":"` + u(11) + `","uuids":[1],"uuids":["` + u(2) + `"]}}`),
		boundary(`{"preservedMessages":{"anchorUuid":"a","anchorUuid":"` + u(11) + `","uuids":[]}}`),
		boundary(`{"preservedMessages":null,"preservedSegment":{"headUuid":"a","anchorUuid":"b","tailUuid":"c"}}`),
		boundary(`{"preservedMessages":{"anchorUuid":"a","uuids":[]},"preservedMessages":null}`),
		boundary(`{"preservedMessages":"x"}`), boundary(`{"preservedMessages":[]}`), boundary(`{"preservedSegment":7}`),
		boundary(`{"preservedSegment":{"headUuid":"a","anchorUuid":"b","tailUuid":"c"},"preservedSegment":null}`),
		boundary(`null`), boundary(`7`), boundary(`{}`), boundary(`[]`),
		`{"type":"system","subtype":"informational","compactMetadata":{"preservedMessages":{"anchorUuid":"a",` +
			`"uuids":[]}}}`,
		`{"compactMetadata":{"preservedSegment":7},"type":"system","subtype":"compact_boundary"}`,
		`{"type":"user","compactMetadata":{},"type":"system","subtype":"compact_boundary"}`,

		ctRetry(u(5), u(6), u(1), u(2)), ctFallback(u(5), u(6), u(1), u(2)),
		`{"type":"user","retractedMessageUuids":[` + strings.Join(entries, ",") + `]}`,
		`{"type":"assistant","retractedMessageUuids":["` + u(1) + `",7,"` + u(2) + `"]}`,
		`{"type":"assistant","retractedMessageUuids":null}`, `{"type":"user","retractedMessageUuids":"x"}`,
		`{"type":"user","retractedMessageUuids":{}}`, `{"type":"user","retractedMessageUuids":[]}`,
		`{"type":"system","subtype":"informational","retractedMessageUuids":["` + u(1) + `"]}`,
		`{"type":"system","subtype":"model_refusal_fallback","retractedMessageUuids":[7]}`,
		`{"retractedMessageUuids":["` + u(1) + `"],"type":"assistant","retractedMessageUuids":[]}`,
		`{"type":"user","retractedMessageUuids":[1],"retractedMessageUuids":["` + u(1) + `"]}`,
		`{"retractedMessageUuids":["` + u(1) + `"],"type":"attachment"}`,
		`{"type":"x","retractedMessageUuids":["` + u(1) + `"],"type":"user"}`,

		ctReplacement("", ctReplaced("toolu_A"), `{"kind":"other"}`, ctReplaced("toolu_B")),
		ctReplacement("a1b2c3", ctReplaced("toolu_A"), `7`, ctReplaced("toolu_B"), `"x"`),
		ctReplacement("", `{"kind":7}`, ctReplaced("toolu_A")), ctReplacement("", `{"kind":"tool-result"}`),
		ctReplacement("", `{"kind":"tool-result","toolUseId":7}`),
		ctReplacement("", `{"toolUseId":"toolu_A","kind":"tool-result","kind":"other"}`),
		ctReplacement("", `{"kind":"other","kind":"tool-result","toolUseId":"toolu_A"}`),
		ctReplacement("", `{"kind":"tool-result","toolUseId":7,"toolUseId":"toolu_A"}`), ctReplacement(""),
		`{"type":"content-replacement","replacements":null}`, `{"type":"content-replacement","replacements":"x"}`,
		`{"type":"content-replacement","replacements":{}}`, `{"type":"content-replacement"}`,
		`{"type":"content-replacement","replacements":[` + ctReplaced("toolu_A") + `],"replacements":[]}`,
		`{"replacements":[` + ctReplaced("toolu_A") + `],"type":"content-replacement"}`,
		`{"type":"user","replacements":[` + ctReplaced("toolu_A") + `]}`,
		`{"type":"x","replacements":[1],"type":"content-replacement"}`,

		reply(`[{"id":"toolu_A"},{"id":"toolu_B","name":"x"}]`), reply(`[{"id":"toolu_A"},7,{"id":"toolu_B"}]`),
		reply(`[{"id":7}]`), reply(`[{}]`), reply(`[{"id":"a","id":"b"}]`), reply(`[{"id":7,"id":"b"}]`),
		reply(`null`), reply(`"x"`), reply(`{}`), reply(`[]`), reply(`[{"id":"a"}],"batchToolUses":[1]`),
		`{"batchToolUses":[{"id":"a"}],"type":"assistant"}`, `{"type":"user","batchToolUses":[{"id":"a"}]}`,
		`{"type":"user","batchToolUses":[{"id":"a"}],"type":"assistant"}`,
	}
	out := make([][]byte, len(lines))
	for i, line := range lines {
		out[i] = []byte(line)
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
// the scanner has read it to its break, a grammar fault first wherever it
// lies: a head refused early, then data after the value, is trailing; a value
// cut at the break a prefix; a NUL after it syntax; a list no object; a value
// past its bound followed by more trailing, by a refused head that head. Only
// a line whose grammar and head are whole fails at the bound. A last piece
// with no break commits nothing, and a member's bound inside a line ends the
// reading. Each holds of a line of a few bytes and of one past 8 MiB.
//
// Mutation: answer the line before the scanner reads to its end; commit a
// piece the member ends with no break.
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
			r := newThreadReader(strings.NewReader(text), maxBytes)
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
	estimating := func(r *threadReader) *threadReader {
		r.Estimating()
		return r
	}
	want := scanReading(t.Context(), estimating(newThreadReader(bytes.NewReader(text), threadPlainMax)))
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
			got := scanReading(t.Context(), estimating(newThreadReaderSize(member(bytes.NewReader(text)),
				threadPlainMax, chunk)))
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
// member's bytes, never in time: a long string the pass reads, and a line
// refused at its first bytes whose rest is drained to its break. The line
// before it was read whole, and the reading stays over whatever context a
// later Next is handed.
//
// Mutation: ask the context only before a line, or not while draining, and
// the whole 64 MiB line is read.
func TestAReadingStopsWithinAFillOfItsContext(t *testing.T) {
	t.Parallel()
	brief := ctThread(ctBrief(ctUUID(1)))
	for name, opening := range map[string]string{
		"a long string":                     `{"type":"queue-operation","operation":"enqueue","content":"`,
		"a line refused at its first bytes": "{\"type\":\x00\"",
	} {
		ctx, cancel := context.WithCancel(t.Context())
		member := &endlessLine{head: brief + opening, size: int64(len(brief)) + 64<<20,
			cancelAt: int64(len(brief)) + 1<<20, cancel: cancel}
		r := newThreadReader(member, threadPlainMax)
		if line, err := r.Next(ctx); err != nil || line.Head.Type != recordUser {
			t.Fatalf("%s: the brief before the long line reads as %q, %v", name, line.Head.Type, err)
		}
		if _, err := r.Next(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: the line its context ended inside reads as %v; want %v", name, err, context.Canceled)
		}
		if member.handed > member.cancelAt+lineChunkBytes {
			t.Errorf("%s: the reading took %d bytes of the member, %d past where its context ended; want at most %d",
				name, member.handed, member.handed-member.cancelAt, lineChunkBytes)
		}
		if _, err := r.Next(t.Context()); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: a Next after the reading's context ended answered %v; want %v", name, err, context.Canceled)
		}
		cancel()
	}
}

// A RESET READER READS AS A FRESH ONE ([threadReader.Reset]): one reader
// reads a session's members one after another, each with its lines numbered
// from 1, its bound its own, ended and torn only by what ends or tears it,
// whatever the member before it left: a torn piece, the member's bound, a
// value past its bound, a line that is no record, an error of its reader, or
// an ended context. One reader reads a thousand members allocating what each
// line costs, never its buffers (its member reader's and its scanner's, 64 KiB
// each) again. Not parallel: the allocation count is the process's.
//
// Mutation: keep how a reading ended, how many lines it read, or the takes it
// asked, across Reset; make a scanner a member.
func TestAResetReaderReadsAsAFreshOne(t *testing.T) {
	brief := ctBrief(ctUUID(1))
	token := claudeTakeToken(ctProbeStopTake)
	said := ctAttachment(ctUUID(3), ctUUID(4), ctReplace(t, ctBatchSystem, `the run"`, `the run `+token+`"`), "")
	next := ctThread(ctBrief(ctUUID(2)), "[1]", ctBrief(ctUUID(3)), said)
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
			r := newThreadReader(tc.before(), tc.maxBytes)
			if err := r.Asking([]string{token}); err != nil {
				t.Fatal(err)
			}
			scanReading(tc.ctx, r)
			r.Reset(strings.NewReader(next), bound, threadLinesMax)
			got := strings.Join(scanReading(t.Context(), r), "")
			if want := strings.Join(scanReading(t.Context(), newThreadReader(strings.NewReader(next), bound)), ""); got != want {
				t.Errorf("after %s, the next member read within %d bytes reads as\n%swant it read as a fresh "+
					"reader reads it\n%s", name, bound, got, want)
			}
		}
	}

	// A THOUSAND MEMBERS, ONE READER: the fewest bytes of five walks, each its
	// own allocations ([ownAllocations]), held below an eighth of what a fresh
	// reader's buffers take, the fewest of three makings.
	member := ctThread(brief)
	r := newThreadReader(strings.NewReader(""), 0)
	const members = 1000
	walk := func() {
		for range members {
			r.Reset(strings.NewReader(member), threadPlainMax, threadLinesMax)
			for {
				if _, err := r.Next(t.Context()); err != nil {
					break
				}
			}
		}
	}
	walk()
	each := uint64(math.MaxUint64)
	for range 5 {
		_, bytes := ownAllocations(walk)
		each = min(each, bytes/members)
	}
	buffers := uint64(math.MaxUint64)
	for range 3 {
		var fresh *threadReader
		_, bytes := ownAllocations(func() { fresh = newThreadReader(strings.NewReader(member), threadPlainMax) })
		runtime.KeepAlive(fresh)
		buffers = min(buffers, bytes)
	}
	if buffers < 2*lineChunkBytes || each >= buffers/8 {
		t.Errorf("one reader reading a member after another allocates %d bytes a member, where a fresh reader's "+
			"buffers take %d; want those at least %d, and a member under an eighth of them", each, buffers,
			2*lineChunkBytes)
	}
}

// A READER KEEPS NO SCRATCH A LINE OF LONG VALUES GREW: lines holding a value
// at [threadValueMax] in every buffer the pass holds one in (the head's keys
// and the type's text, a block's keys, its input's `to` and its content's
// type, a leg's answer) leave the reader that read them holding no more than
// one that read ordinary lines but the capture buffer its scanner keeps
// ([jsonscan.Scanner.Capture]): under twice threadValueMax more, where the 17
// buffers kept would hold 17 times it. Not parallel: what a reader holds is
// read off the heap.
//
// Mutation: keep every scratch buffer whatever it grew to.
func TestAReaderKeepsNoScratchALineOfLongValuesGrew(t *testing.T) {
	ctx := t.Context()
	long := `"` + strings.Repeat("x", threadValueMax-2) + `"`
	held := func(member string) uint64 {
		return heldBy(func() any {
			r := newThreadReader(strings.NewReader(member), threadPlainMax)
			for {
				_, err := r.Next(ctx)
				var broken *threadLineError
				if err != nil && !errors.As(err, &broken) {
					break
				}
			}
			return r
		})
	}
	brief := ctBrief(ctUUID(1))
	ordinary := held(ctThread(brief, brief, brief))
	hostile := held(ctThread(
		`{"type":`+long+`,"subtype":`+long+`,"agentId":`+long+`,"parentUuid":`+long+`,"isSidechain":`+long+
			`,"replacesSpan":`+long+`}`,
		`{"message":{"content":[{"type":`+long+`,"id":`+long+`,"name":`+long+`,"tool_use_id":`+long+
			`,"is_error":`+long+`,"input":{"to":`+long+`},"content":[{"type":`+long+`}]}]},"type":"user"}`,
		`{"toolUseResult":{"status":`+long+`,"agentId":`+long+`,"success":`+long+`},"type":"user"}`))
	if hostile > ordinary+2*threadValueMax {
		t.Errorf("a reader that read lines of values at %d bytes holds %d bytes, where one that read ordinary "+
			"lines holds %d; want at most %d more", threadValueMax, hostile, ordinary, 2*threadValueMax)
	}
}

// scanEnds is every answer a reader gives of its member to its end, as text:
// each line's place, size, head type and parent and byte facts, or its fault
// as [scanAnswer] says it, then what ended the reading, what a Next after it
// answers and whether the member was torn.
func scanEnds(ctx context.Context, r interface {
	Next(context.Context) (threadLine, error)
	Torn() bool
}) string {
	var b strings.Builder
	for {
		line, err := r.Next(ctx)
		var broken *threadLineError
		switch {
		case err == nil:
			fmt.Fprintf(&b, "line %d, size %d, type %q, parent %q\nbytes: %s\n", line.N, line.Size, line.Head.Type,
				line.Head.Parent, scanBytes(line.Bytes))
		case errors.As(err, &broken):
			b.WriteString(scanAnswer(line, err))
		default:
			_, again := r.Next(ctx)
			fmt.Fprintf(&b, "ended by %v (io.EOF %t, past its bound %t), then %v, torn %t\n", err,
				errors.Is(err, io.EOF), errors.Is(err, errThreadBound), again, r.Torn())
			return b.String()
		}
	}
}

// THE READER ENDS A MEMBER AS THE WHOLE READER DOES ([wholeReader]): a
// member of records, refused lines and trailing data, read within every bound
// from none to two bytes past its length (so at, before and after each break),
// with each tail a member can end in (none, NULs, a whole record, a lone CR, a
// cut value, a blank, a short record), and its reader failing at every offset
// with an error of its own, a wrapped io.EOF, a wrapped scrub bound and a
// wrapped line bound, under each bound about the failure, answers alike in
// pieces of 16 bytes and of [lineChunkBytes].
//
// Mutation: count no line's break against the member's bound; read a wrapped
// io.EOF as the reader's error both where a piece is taken and where the line
// ends, each of which alone reads it as the member's end.
func TestTheReaderEndsAMemberAsTheWholeReaderDoes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	brief := ctBrief(ctUUID(1))
	member := ctThread(brief, "[1]", brief, `{"type":7}`, "\x00"+brief, brief+" x", brief)
	mismatches := 0
	compare := func(what string, read func() io.Reader, bound int64) {
		want := scanEnds(ctx, newWholeReader(read(), bound))
		for _, chunk := range []int{16, lineChunkBytes} {
			if got := scanEnds(ctx, newThreadReaderSize(read(), bound, chunk)); got != want {
				if mismatches++; mismatches <= 3 {
					t.Errorf("%s, within %d bytes, in pieces of %d, reads as\n%swant it read as the whole reader "+
						"reads it\n%s", what, bound, chunk, got, want)
				}
			}
		}
	}
	for _, tail := range []string{"", "\x00\x00", brief, "\r", "{", " ", `{"type":"user"}`} {
		text := member + tail
		for bound := range int64(len(text)) + 3 {
			compare(fmt.Sprintf("a member ending in %q", tail), func() io.Reader { return strings.NewReader(text) },
				bound)
		}
	}
	text := member + brief
	for _, failure := range []error{io.ErrUnexpectedEOF, fmt.Errorf("member: %w", io.EOF),
		fmt.Errorf("meter: %w", errScrubBound), fmt.Errorf("line: %w", errLineBound)} {
		for cut := range len(text) + 1 {
			read := func() io.Reader {
				return io.MultiReader(strings.NewReader(text[:cut]), iotest.ErrReader(failure))
			}
			for _, bound := range []int64{threadPlainMax, int64(cut) - 1, int64(cut), int64(cut) + 1} {
				compare(fmt.Sprintf("a member whose reader fails with %v at byte %d", failure, cut), read, bound)
			}
		}
	}
	if mismatches > 3 {
		t.Errorf("%d readings in all read otherwise than the whole reader's", mismatches)
	}
}

// scanPart is one stretch of a line a test generates as it is read
// ([scanGenerated]): text, times over.
type scanPart struct {
	text  string
	times int
}

// scanLit is text, once.
func scanLit(text string) []scanPart { return []scanPart{{text, 1}} }

// scanRun is n bytes of b.
func scanRun(b byte, n int) []scanPart {
	piece := strings.Repeat(string(b), 4096)
	return []scanPart{{piece, n / len(piece)}, {piece[:n%len(piece)], 1}}
}

// scanList is a JSON list of n copies of element, n at least one.
func scanList(element string, n int) []scanPart {
	return slices.Concat(scanLit("["+element), []scanPart{{"," + element, n - 1}}, scanLit("]"))
}

// scanLine is the line parts make, its break after it.
func scanLine(parts ...[]scanPart) []scanPart { return slices.Concat(append(parts, scanLit("\n"))...) }

// scanLength is how many bytes parts make.
func scanLength(parts []scanPart) int64 {
	var n int64
	for _, p := range parts {
		n += int64(len(p.text)) * int64(p.times)
	}
	return n
}

// scanGenerated is a member made of parts, generated as it is read and never
// held whole, by the test or by its reader.
type scanGenerated struct {
	parts []scanPart
	// part is the part being handed, done how many of its copies were handed,
	// and at how much of the copy being handed.
	part, done, at int
}

// Read hands the member's next bytes.
func (g *scanGenerated) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && g.part < len(g.parts) {
		part := g.parts[g.part]
		if g.done == part.times || part.text == "" {
			g.part, g.done, g.at = g.part+1, 0, 0
			continue
		}
		m := copy(p[n:], part.text[g.at:])
		n += m
		if g.at += m; g.at == len(part.text) {
			g.done, g.at = g.done+1, 0
		}
	}
	if n == 0 && g.part == len(g.parts) {
		return 0, io.EOF
	}
	return n, nil
}

// scanRead is what a fresh reader reads of the line parts make.
func scanRead(ctx context.Context, parts []scanPart) (threadLine, error) {
	return newThreadReader(&scanGenerated{parts: parts}, threadPlainMax).Next(ctx)
}

// scanAllocated is the fewest bytes a reading in one pass allocates from its
// Next to its return reading the line parts make, of runs readings, and what
// the last answered: its own allocations ([ownAllocations]), never another
// goroutine's. Each is a fresh reader's, warmed first on the line warm makes,
// so neither its buffers nor its scratch growing to a first line count, and
// what one reading keeps across lines counts in every reading.
func scanAllocated(ctx context.Context, warm, parts []scanPart, runs int) (uint64, threadLine, error) {
	fewest := uint64(math.MaxUint64)
	var (
		line threadLine
		err  error
	)
	for range runs {
		r := newThreadReader(&scanGenerated{parts: warm}, threadPlainMax)
		if _, err = r.Next(ctx); err != nil {
			return 0, threadLine{}, fmt.Errorf("the line it was warmed on: %w", err)
		}
		r.Reset(&scanGenerated{parts: parts}, threadPlainMax, threadLinesMax)
		_, bytes := ownAllocations(func() { line, err = r.Next(ctx) })
		fewest = min(fewest, bytes)
	}
	return fewest, line, err
}

// scanCallsHeld is how many calls a line's facts hold within [threadLineMax]
// where each call's strings are each bytes long and the record's head holds
// head bytes of strings, by the meter's rule ([lineMeter]): each call's
// strings charged as the call is read, the array of calls grown by doubling
// from one, each array charged whole before it is made and none larger than
// what is left, and the head's strings charged at the line's end. WRITTEN
// FROM THE RULE, never through the meter, so a charge the pass skips or makes
// otherwise moves the boundary a test reads off it.
func scanCallsHeld(each, head int) int {
	size := int(unsafe.Sizeof(threadCall{}))
	left, capacity := threadLineMax, 0
	for n := 0; ; n++ {
		// Whether a line of n+1 calls is held, n being.
		if each > left {
			return n
		}
		left -= each
		if n == capacity {
			grown := min(max(2*capacity, 1), left/size)
			if grown <= n {
				return n
			}
			left, capacity = left-grown*size, grown
		}
		if head > left {
			return n
		}
	}
}

// A LINE'S FACTS ARE HELD TO THE BOUND, IN SUM: a line of calls is read to the
// most the meter's rule holds ([scanCallsHeld]), the calls the smallest
// results (held within a quarter more than the bound), results past 4 KiB,
// sends each to a 64-byte agent or results read before an assistant's type,
// and one call more fails it closed; a line writing `message` twice, each
// fitting, fails; a value of [threadValueMax] bytes reads and one byte more
// fails; and a line past either bound is still classed by a later fault or
// by its head. Not parallel: what a line holds is read off the heap.
//
// Mutation: charge an array by its header, or a call without its strings, its
// target, or before its type; refund a replaced frame; drop the value bound.
func TestALinesFactsAreHeldToTheBound(t *testing.T) {
	ctx := t.Context()
	small := `{"type":"tool_result","tool_use_id":"a"}`
	target := strings.Repeat("a", claudeAgentMaxBytes)
	results := func(extra, block, after string) func(n int) []scanPart {
		return func(n int) []scanPart {
			return scanLine(scanLit(`{"type":"user",`+extra+`"message":{"content":`), scanList(block, n),
				scanLit(`}}`+after))
		}
	}
	every := func(n int) int { return n }
	for _, tc := range []struct {
		name string
		line func(n int) []scanPart
		// each is the bytes of each call's strings, and head the head's.
		each, head int
		// held is how many calls a line of n blocks hands its reader.
		held func(n int) int
		to   string
	}{
		{"the smallest results", results("", small, ""), 1, len(recordUser), every, ""},
		{"results of more than 4 KiB each", results("", `{"type":"tool_result","tool_use_id":"a","content":"`+
			strings.Repeat("x", sandbox.MaxBridgeOutputBytes+1)+`"}`, ""), 1, len(recordUser), every, ""},
		{"sends, each to a 64-byte agent", func(n int) []scanPart {
			return scanLine(scanLit(`{"type":"assistant","message":{"content":`),
				scanList(`{"type":"tool_use","id":"a","name":"S","input":{"to":"`+target+`"}}`, n), scanLit(`}}`))
		}, 1 + 1 + len(target), len(recordAssistant), every, target},
		{"results an assistant's message holds before its type", func(n int) []scanPart {
			return scanLine(scanLit(`{"message":{"content":`), scanList(small, n), scanLit(`},"type":"assistant"}`))
		}, 1, len(recordAssistant), func(int) int { return 0 }, ""},
	} {
		n := scanCallsHeld(tc.each, tc.head)
		line, err := scanRead(ctx, tc.line(n))
		if err != nil {
			t.Fatalf("%s: a line of %d, the most the meter holds, reads as %v; want a record", tc.name, n, err)
		}
		m, err := line.Message()
		if calls := m.Calls(); err != nil || len(calls) != tc.held(n) ||
			(len(calls) > 0 && calls[len(calls)-1].To != tc.to) {
			t.Fatalf("%s: a line of %d hands %d calls, %v; want %d, each to %q", tc.name, n, len(calls), err,
				tc.held(n), tc.to)
		}
		if _, err = scanRead(ctx, tc.line(n+1)); !errors.Is(err, errThreadBound) ||
			!strings.Contains(err.Error(), "facts run past") {
			t.Errorf("%s: a line of %d, one more than the meter holds, reads as %v; want %v, its facts past it",
				tc.name, n+1, err, errThreadBound)
		}
	}

	n := scanCallsHeld(1, len(recordUser))
	if held := heldBy(func() any {
		line, _ := scanRead(ctx, results("", small, "")(n))
		return line
	}); held > threadLineMax*5/4+64<<10 {
		t.Errorf("a line of %d of the smallest results holds %d bytes; want at most %d", n, held,
			threadLineMax*5/4+64<<10)
	}
	twice := scanLine(scanLit(`{"type":"user","message":{"content":`), scanList(small, n),
		scanLit(`},"message":{"content":`), scanList(small, n), scanLit(`}}`))
	if _, err := scanRead(ctx, twice); !errors.Is(err, errThreadBound) {
		t.Errorf("a line writing a message of %d results twice reads as %v; want %v", n, err, errThreadBound)
	}

	pad := func(n int) string { return strings.Repeat("u", n) }
	usage := `{"input_tokens":1,"pad":""}`
	for _, tc := range []struct {
		name, key string
		// line is the line whose value is n bytes as written.
		line func(n int) string
		// read reports whether the line's reader is handed the value whole.
		read func(l threadLine) bool
	}{
		{"a uuid", "uuid", func(n int) string { return `{"type":"user","uuid":"` + pad(n-2) + `"}` },
			func(l threadLine) bool { return len(l.facts().uuid) == threadValueMax }},
		{"a call's id", "message.content.id", func(n int) string {
			return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + pad(n-2) + `","name":"S"}]}}`
		}, func(l threadLine) bool {
			m, err := l.Message()
			return err == nil && len(m.Calls()) == 1 && len(m.Calls()[0].ID) == threadValueMax-2
		}},
		{"a reply's usage", "message.usage", func(n int) string {
			return `{"type":"assistant","message":{"content":[],"usage":{"input_tokens":1,"pad":"` +
				pad(n-len(usage)) + `"}}}`
		}, func(l threadLine) bool { return len(l.facts().message.usage) == threadValueMax }},
	} {
		if line, err := scanRead(ctx, scanLine(scanLit(tc.line(threadValueMax)))); err != nil || !tc.read(line) {
			t.Errorf("%s of %d bytes as written reads as %v, handed whole %t; want it read", tc.name,
				threadValueMax, err, err == nil && tc.read(line))
		}
		if _, err := scanRead(ctx, scanLine(scanLit(tc.line(threadValueMax+1)))); !errors.Is(err, errThreadBound) ||
			!strings.Contains(err.Error(), tc.key+" runs past") {
			t.Errorf("%s of %d bytes as written reads as %v; want %v naming %s", tc.name, threadValueMax+1, err,
				errThreadBound, tc.key)
		}
	}

	for name, tc := range map[string]struct {
		line  []scanPart
		fault lineFault
		kind  recordKind
	}{
		"facts past the bound, then data after the value": {results("", small, " x")(n + 1), faultTrailing, ""},
		"facts past the bound, and a refused head": {results(`"isSidechain":1,`, small, "")(n + 1), faultHead,
			recordUser},
		"a value past its bound, then data after the value": {scanLine(scanLit(`{"type":"user","uuid":"` +
			pad(threadValueMax-1) + `"} x`)), faultTrailing, ""},
		"a value past its bound, and a refused head": {scanLine(scanLit(`{"type":"user","isSidechain":1,"uuid":"` +
			pad(threadValueMax-1) + `"}`)), faultHead, recordUser},
	} {
		_, err := scanRead(ctx, tc.line)
		var broken *threadLineError
		if !errors.As(err, &broken) || broken.Fault != tc.fault || broken.Type != tc.kind {
			t.Errorf("%s: read as %v; want fault %q, type %q", name, err, tc.fault, tc.kind)
		}
	}
}

// scanArrayCharge is what the meter's rule charges an array that grows to n
// elements of size bytes: grown by doubling from one, each array charged
// whole as it is made.
func scanArrayCharge(n, size int) int {
	charged := 0
	for capacity := 0; capacity < n; {
		capacity = max(2*capacity, 1)
		charged += capacity * size
	}
	return charged
}

// A LINE'S FACTS ARE HELD TO THE BOUND BY EVERY CHARGE: beside what each case
// keeps, a line writes `uuid` at [threadValueMax] as often as the bound holds
// but once, then once more at the most bytes the meter's rule leaves (every
// copy charged and never refunded, each array charged whole as it doubles,
// the head last), and reads as a record keeping what the case keeps; a byte
// more fails it closed. WRITTEN FROM THE RULE, never through the meter, so a
// charge the pass drops moves the boundary: a kept raw's, a retraction's or a
// kept list's array, a kept list's anchor, a replacement's or a batch entry's
// id and their array, a leg's agent, and a task notification's agent and leg.
//
// Mutation: charge none of these, each in turn.
func TestEveryChargeOfALinesFactsIsHeldToTheBound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const listed = 64
	quoted := make([]string, listed)
	for i := range quoted {
		quoted[i] = `"` + ctUUID(i+1) + `"`
	}
	uuids := strings.Join(quoted, ",")
	anchor := ctUUID(listed + 1)
	agent := strings.Repeat("a", claudeAgentMaxBytes)
	leg := "toolu_" + strings.Repeat("l", 58)
	notice := `<task-notification>\n<task-id>` + agent + `</task-id>\n<tool-use-id>` + leg +
		`</tool-use-id>\n<status>completed</status>\n</task-notification>`
	ids := func(entry string) string {
		return strings.TrimSuffix(strings.Repeat(entry+",", listed), ",")
	}
	str := int(unsafe.Sizeof(""))
	counted := func(walk func(each func(string)) error) bool {
		n := 0
		return walk(func(string) { n++ }) == nil && n == listed
	}
	for _, tc := range []struct {
		name string
		// opening is the record up to its uuids, and typ and subtype its head.
		opening, typ, subtype string
		// extra is what the case's facts charge by the meter's rule, beside the
		// uuids and the head.
		extra int
		// kept reports whether the record keeps what the case keeps.
		kept func(l threadLine) bool
	}{
		{"kept raws", `{"type":"user",`, recordUser, "", 0, func(threadLine) bool { return true }},
		{"a retraction's prefixes", `{"type":"user","retractedMessageUuids":[` + uuids + `],`, recordUser, "",
			scanArrayCharge(listed, int(unsafe.Sizeof([chainPrefixBytes]byte{}))), func(l threadLine) bool {
				r, err := l.Retracted()
				held, all := r.Prefixes()
				return err == nil && all && len(held) == listed
			}},
		{"a kept list's uuids and anchor", `{"type":"system","subtype":"` + subtypeCompactBoundary +
			`","compactMetadata":{"preservedMessages":{"anchorUuid":"` + anchor + `","uuids":[` + uuids + `]}},`,
			recordSystem, subtypeCompactBoundary, scanArrayCharge(listed, int(unsafe.Sizeof([16]byte{}))) + len(anchor),
			func(l threadLine) bool {
				p, err := l.Preserved()
				return err == nil && p.Kept && len(p.UUIDs) == listed && p.Anchor == anchor
			}},
		{"a replacement's ids", `{"type":"` + recordContentReplacement + `","replacements":[` +
			ids(`{"kind":"`+replacementToolResult+`","toolUseId":"r"}`) + `],`, recordContentReplacement, "",
			listed + scanArrayCharge(listed, str), func(l threadLine) bool { return counted(l.ReplacedResults) }},
		{"a batch's ids", `{"type":"assistant","batchToolUses":[` + ids(`{"id":"b"}`) + `],`, recordAssistant, "",
			listed + scanArrayCharge(listed, str), func(l threadLine) bool {
				return counted(func(each func(string)) error {
					present, err := l.BatchToolUses(each)
					if !present {
						return errors.New("no batch")
					}
					return err
				})
			}},
		{"a leg's agent", `{"type":"user","toolUseResult":{"status":"completed","agentId":"` + agent + `"},`,
			recordUser, "", len(agent), func(l threadLine) bool {
				completed, got, _ := l.AgentAnswer()
				return completed && got == agent
			}},
		{"a task notification's agent and leg", `{"type":"user","message":{"content":"` + notice + `"},`, recordUser,
			"", len(agent) + len(leg), func(l threadLine) bool {
				m, err := l.Message()
				a, lg, _ := m.Notification()
				return err == nil && a == agent && lg == leg
			}},
	} {
		copies := threadLineMax/threadValueMax - 1
		last := threadLineMax - copies*threadValueMax - tc.extra - len(tc.typ) - len(tc.subtype)
		if last < 2 || last > threadValueMax {
			t.Fatalf("%s: the last uuid would run %d bytes as written, outside 2 to %d", tc.name, last, threadValueMax)
		}
		line := func(last int) []scanPart {
			return scanLine(scanLit(tc.opening), []scanPart{{`"uuid":"` + strings.Repeat("u", threadValueMax-2) + `",`,
				copies}}, scanLit(`"uuid":"`+strings.Repeat("u", last-2)+`"}`))
		}
		if got, err := scanRead(ctx, line(last)); err != nil || !tc.kept(got) {
			t.Errorf("%s: a line at the most the meter holds reads as %v, keeping what it writes %t; want a record "+
				"that keeps it", tc.name, err, err == nil && tc.kept(got))
		}
		if _, err := scanRead(ctx, line(last+1)); !errors.Is(err, errThreadBound) ||
			!strings.Contains(err.Error(), "facts run past") {
			t.Errorf("%s: a line a byte past the most the meter holds reads as %v; want %v, its facts past it",
				tc.name, err, errThreadBound)
		}
	}
}

// A BOOKKEEPING RECORD'S MESSAGE IS PASSED OVER, as is every gated frame
// ([gates]) after a type that does not read it: an api-request-blob whose
// frame holds a MiB of what a reader would keep costs at most 16 KiB more
// than one holding one element. A line whose last type reads a frame passed
// over for the type before it fails closed, and a frame met before any type
// is read as where the type comes first. Not parallel: the allocation count
// is the process's.
//
// Mutation: read every gated frame; trust the type a frame was passed over
// for; pass over a frame met before any type.
func TestABookkeepingRecordsMessageIsPassedOver(t *testing.T) {
	ctx := t.Context()
	many := func(element string) string {
		n := (1 << 20) / (len(element) + 1)
		return "[" + strings.Repeat(element+",", n-1) + element + "]"
	}
	uuid := `"` + ctUUID(7) + `"`
	for _, tc := range []struct {
		key, reads, subtype string
		// big is the frame holding a MiB, small one element of it.
		big, small string
		// read is what the frame's accessor answers, as text.
		read func(l threadLine) string
	}{
		{"message", recordAssistant, "", `{"content":` + many(`{"type":"tool_use","id":"a","name":"b"}`) + `}`,
			`{"content":[{"type":"tool_use","id":"a","name":"b"}]}`, func(l threadLine) string {
				m, err := l.Message()
				return fmt.Sprint(len(m.Calls()), err)
			}},
		{"toolUseResult", recordUser, "", `{"handback":1,"stdout":"` + strings.Repeat("x", 1<<20) + `"}`,
			`{"handback":1}`, func(l threadLine) string { return fmt.Sprint(l.HandsBack()) }},
		{"attachment", recordAttachment, "",
			`{"type":"x","entries":` + many(`{"path":"p","displayPath":"d","why":"denied"}`) + `}`, `{"type":"x"}`,
			func(l threadLine) string { return fmt.Sprint(l.AttachmentType()) }},
		{"compactMetadata", recordSystem, subtypeCompactBoundary,
			`{"preservedMessages":{"anchorUuid":` + uuid + `,"uuids":` + many(uuid) + `}}`,
			`{"preservedMessages":{"anchorUuid":` + uuid + `,"uuids":[` + uuid + `]}}`, func(l threadLine) string {
				p, err := l.Preserved()
				return fmt.Sprint(p.Named, p.Listed, err)
			}},
		{"retractedMessageUuids", recordUser, "", many(uuid), "[" + uuid + "]", func(l threadLine) string {
			r, err := l.Retracted()
			return fmt.Sprint(r.Entries, err)
		}},
		{"replacements", recordContentReplacement, "", many(`{"kind":"tool-result","toolUseId":"a"}`),
			`[{"kind":"tool-result","toolUseId":"a"}]`, func(l threadLine) string {
				var ids []string
				err := l.ReplacedResults(func(id string) { ids = append(ids, id) })
				return fmt.Sprint(ids, err)
			}},
		{"batchToolUses", recordAssistant, "", many(`{"id":"a"}`), `[{"id":"a"}]`, func(l threadLine) string {
			var ids []string
			batch, err := l.BatchToolUses(func(id string) { ids = append(ids, id) })
			return fmt.Sprint(ids, batch, err)
		}},
	} {
		blob := func(value string) []scanPart {
			return scanLine(scanLit(`{"type":"api-request-blob","` + tc.key + `":` + value + `}`))
		}
		bigCost, _, bigErr := scanAllocated(ctx, blob(tc.small), blob(tc.big), 3)
		smallCost, _, smallErr := scanAllocated(ctx, blob(tc.small), blob(tc.small), 3)
		if bigErr != nil || smallErr != nil || bigCost > smallCost+16<<10 {
			t.Errorf("an api-request-blob whose %s holds a MiB costs its reader %d bytes, %v; want at most 16 KiB "+
				"more than one holding one element, %d bytes, %v", tc.key, bigCost, bigErr, smallCost, smallErr)
		}

		typed := `"type":"` + tc.reads + `"`
		if tc.subtype != "" {
			typed += `,"subtype":"` + tc.subtype + `"`
		}
		turned := `{"type":"api-request-blob","` + tc.key + `":` + tc.small + `,` + typed + `}`
		if _, err := scanRead(ctx, scanLine(scanLit(turned))); !errors.Is(err, errThreadBound) ||
			!strings.Contains(err.Error(), tc.key+" came before") {
			t.Errorf("a record of type %s whose %s came after an api-request-blob's type reads as %v; want %v naming it",
				tc.reads, tc.key, err, errThreadBound)
		}

		none, noneErr := scanRead(ctx, scanLine(scanLit(`{`+typed+`}`)))
		after, afterErr := scanRead(ctx, scanLine(scanLit(`{`+typed+`,"`+tc.key+`":`+tc.small+`}`)))
		before, beforeErr := scanRead(ctx, scanLine(scanLit(`{"`+tc.key+`":`+tc.small+`,`+typed+`}`)))
		if noneErr != nil || afterErr != nil || beforeErr != nil || tc.read(before) != tc.read(after) ||
			tc.read(after) == tc.read(none) {
			t.Errorf("a record of type %s whose %s comes before its type reads %q, %v; want %q, %v, as where its type "+
				"comes first, and not %q, %v, as one without it", tc.reads, tc.key, tc.read(before), beforeErr,
				tc.read(after), afterErr, tc.read(none), noneErr)
		}
	}
}

// A LINE NO READER READS COSTS ITS READER NOTHING: a whole-PDF Read (its
// document's base64 and the tool's copy 27,962,028 bytes each, as one of
// 55,924,056 bytes holds them), a 64 MiB prompt, queue operation's content,
// Write's input and api-request-blob's message, each generated as it is read,
// reads as its short twin does but for its length, the PDF marked a document,
// costing from Next to its return at most 16 KiB more than the twin, the
// fewest of two readings. Not parallel: the allocation count is the
// process's.
//
// Mutation: capture a value the pass passes over, or keep a prompt as it is
// read for what it says.
func TestALineNoReaderReadsCostsItsReaderNothing(t *testing.T) {
	ctx := t.Context()
	const pdf, long = 27_962_028, 64 << 20
	head := `{"parentUuid":"` + ctUUID(1) + `","isSidechain":false,`
	stamped := `"uuid":"` + ctUUID(2) + `","timestamp":"` + ctStamp + `",`
	for _, tc := range []struct {
		name string
		line func(n int) []scanPart
		n    int
	}{
		{"a whole PDF read", func(n int) []scanPart {
			return scanLine(scanLit(head+`"type":"user",`+stamped+`"message":{"role":"user","content":`+
				`[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"document","source":`+
				`{"type":"base64","media_type":"application/pdf","data":"`), scanRun('A', n),
				scanLit(`"}}]}]},"toolUseResult":{"type":"pdf","file":{"base64":"`), scanRun('A', n),
				scanLit(`","filePath":"/w/x.pdf","originalSize":20971520}}}`))
		}, pdf},
		{"a prompt", func(n int) []scanPart {
			return scanLine(scanLit(head+`"type":"user",`+stamped+`"message":{"role":"user","content":"`),
				scanRun('x', n), scanLit(`"}}`))
		}, long},
		{"a queue operation's content", func(n int) []scanPart {
			return scanLine(scanLit(`{"type":"queue-operation","operation":"enqueue","timestamp":"`+ctStamp+
				`","sessionId":"`+ctSession+`","content":"`), scanRun('x', n), scanLit(`"}`))
		}, long},
		{"a Write's input", func(n int) []scanPart {
			return scanLine(scanLit(head+`"type":"assistant",`+stamped+`"message":{"id":"msg_1","content":`+
				`[{"type":"tool_use","id":"toolu_1","name":"Write","input":{"file_path":"/w/x","content":"`),
				scanRun('x', n), scanLit(`"}}]}}`))
		}, long},
		{"an api-request-blob's message", func(n int) []scanPart {
			return scanLine(scanLit(`{"type":"api-request-blob","sessionId":"`+ctSession+
				`","hash":"b1b1b1b1","message":{"role":"user","content":"`), scanRun('x', n), scanLit(`"}}`))
		}, long},
	} {
		twin, parts := tc.line(1), tc.line(tc.n)
		twinCost, twinLine, twinErr := scanAllocated(ctx, twin, twin, 3)
		cost, line, err := scanAllocated(ctx, twin, parts, 2)
		if twinErr != nil || err != nil {
			t.Fatalf("%s: the line reads as %v, its twin as %v; want both records", tc.name, err, twinErr)
		}
		if int64(line.Size) != scanLength(parts) {
			t.Errorf("%s: a line of %d bytes, its break among them, is read as %d", tc.name, scanLength(parts),
				line.Size)
		}
		facts := func(l threadLine) string {
			l.Size, l.Bytes.n = 0, 0
			return scanAnswer(l, nil)
		}
		if got, want := facts(line), facts(twinLine); got != want {
			t.Errorf("%s: the line of %d bytes reads as\n%swant it read as its twin, but its length\n%s", tc.name,
				line.Size, got, want)
		}
		if tc.n == pdf && line.Bytes.marks&markDocument == 0 {
			t.Errorf("%s: the line holds no document's mark; want it marked as its readers mark it", tc.name)
		}
		if cost > twinCost+16<<10 {
			t.Errorf("%s: the line of %d bytes costs its reader %d bytes; want at most 16 KiB more than its twin, "+
				"%d bytes", tc.name, line.Size, cost, twinCost)
		}
	}
}
