package executor

import (
	"encoding/json"
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The lists a harness's record holds, as the readers that are no thread
// layer's read them: a run's event stream ([claudeEvents]), its result
// ([ClaudeCode.Parse]) and a person's transcript of a Claude Code or an
// OpenCode session ([transcriptBuilder]).

// listBytes is how long a list the cost case reads is: hundreds of thousands
// of elements, so that a reader that spends an allocation an element spends
// hundreds of thousands, and short enough that the whole decode a mutant puts
// back still fits a test's memory. What a list costs is per element, so a list
// this long shows what one at the 32 MiB a line may hold ([maxLineBytes])
// costs.
const listBytes = 1 << 20

// listCopies is how many copies of the list a reader may make: three at most,
// measured, where a transcript's decode of a line keeps the line's message
// raw, the message's decode its content, and a call's block its result (the
// stream makes one or two, the result file two, an OpenCode step none or two),
// and one more for headroom. What a reader may never spend is anything for
// each element: read whole, a list cost from 56 to 519 times its size.
const listCopies = 4

// listSlack is how many more allocations a list read to its end may cost than
// a list of one, though it keeps nothing more of it: the count is the
// process's, and a list's copy can set off a collection while it is measured,
// which empties the process's pools (the decoders, fmt's printers), so a read
// builds what it takes from them afresh, a few allocations each. Under the
// race detector a pool also drops a quarter of what is put in it. An
// allocation an element is hundreds of thousands.
const listSlack = 64

// listBuilder is a transcript read's builder, as a read from the archive's
// first entry starts one.
func listBuilder() *transcriptBuilder {
	return &transcriptBuilder{bounds: TranscriptReadBounds(), entries: []TranscriptEntry{},
		places: []EntryPlace{}, calls: map[string]string{}}
}

// allocationsOf is what read allocates, the fewest of three calls after one
// that warms it up: how many heap objects, and how many bytes. GOMAXPROCS is
// one while it measures, as testing.AllocsPerRun's is, so another goroutine's
// allocations are counted no more than they must be.
func allocationsOf(read func()) (objects, bytes uint64) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	read()
	objects, bytes = math.MaxUint64, math.MaxUint64
	var before, after runtime.MemStats
	for range 3 {
		runtime.ReadMemStats(&before)
		read()
		runtime.ReadMemStats(&after)
		objects = min(objects, after.Mallocs-before.Mallocs)
		bytes = min(bytes, after.TotalAlloc-before.TotalAlloc)
	}
	return objects, bytes
}

// A LIST IN A HARNESS'S RECORD COSTS ITS READER ONE ELEMENT AT A TIME: every
// list a run's event stream, its result and a person's transcript read (a
// message's blocks, a call's result, a result's errors, an OpenCode step's
// items and a tool's answer) is read an element at a time, each checked as it
// is read, so a list of hundreds of thousands of elements, none of which says
// anything, costs no more allocations than a list of one and no more bytes than
// a few copies of itself. Decoded whole, each was a slice of every element
// first: a block's 136 bytes, an item's 160, a part's 32 or 48, an error's 16,
// and an error of its own for each element of another kind, before any was
// looked at.
//
// Not parallel: the counts are the process's, and a parallel test's
// allocations would be counted with this one's.
//
// Mutation: decode a message's blocks, a tool's answer, a step's items or a
// result's errors whole before reading the first, or have the walk of the
// blocks collect them before it hands out the first, and its reader's list
// costs hundreds of thousands of allocations, or tens of MiB, more than a list
// of one.
func TestAListInAHarnesssRecordCostsItsReaderOneElementAtATime(t *testing.T) {
	// Each read is handed bytes made once, outside what is measured, as a
	// reader is handed a line it did not copy.
	stream := func(line string) func() {
		raw := []byte(line)
		return func() { (&claudeEvents{tools: map[string]string{}}).Line(raw) }
	}
	parse := func(line string) func() {
		return func() { _ = ClaudeCode{}.Parse(line) }
	}
	transcript := func(line string) func() {
		raw := []byte(line)
		return func() { _ = listBuilder().claudeLine(t.Context(), raw) }
	}
	step := func(content string) func() {
		raw := json.RawMessage(content)
		return func() { _ = listBuilder().openCodeStep(t.Context(), raw, time.Time{}) }
	}
	const (
		reply  = `{"type":"assistant","message":{"role":"assistant","content":[`
		result = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result",` +
			`"tool_use_id":"toolu_test","is_error":true,"content":[`
		ending = `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":[`
		tool   = `[{"type":"tool","name":"bash","state":{"status":"completed","content":[`
	)
	for name, tc := range map[string]struct {
		head, tail string
		read       func(string) func()
	}{
		"a run's event stream, a message's blocks":     {head: reply, tail: `]}}`, read: stream},
		"a run's event stream, a failed call's result": {head: result, tail: `]}]}}`, read: stream},
		"a run's event stream, its result's errors":    {head: ending, tail: `]}`, read: stream},
		"a run's result, its errors":                   {head: ending, tail: `]}`, read: parse},
		"a Claude Code transcript, a message's blocks": {head: reply, tail: `]}}`, read: transcript},
		"a Claude Code transcript, a call's result":    {head: result, tail: `]}]}}`, read: transcript},
		"an OpenCode transcript, a step's items":       {head: `[`, tail: `]`, read: step},
		"an OpenCode transcript, a tool's answer":      {head: tool, tail: `]}}]`, read: step},
	} {
		one := tc.head + "1" + tc.tail
		full := ctListRecord(t, tc.head, "1", tc.tail, listBytes)
		fewObjects, fewBytes := allocationsOf(tc.read(one))
		manyObjects, manyBytes := allocationsOf(tc.read(full))
		if manyObjects > fewObjects+listSlack {
			t.Errorf("%s: a list of %d elements costs %d allocations, and a list of one %d", name,
				strings.Count(full, ","), manyObjects, fewObjects)
		}
		if manyBytes > fewBytes+listCopies*uint64(len(full)) {
			t.Errorf("%s: a list of %d bytes costs %d bytes allocated, and a list of one %d: more than %d "+
				"copies of the list", name, len(full), manyBytes, fewBytes, listCopies)
		}
	}
}

// A LIST IS READ AN ELEMENT AT A TIME, AS ITS HARNESS READS IT: an element
// that is no object (a number, a string, null, a list) is no block, item or
// part, a key of another kind than its field's reads as nothing, and each
// costs itself alone, the elements after it read; a key is read exactly as
// written, as JSON.parse, which both harnesses read their records with, reads
// it, so `Type` is no block's type; and a string's lone surrogate or invalid
// UTF-8 reads as U+FFFD, never as no list. A result's errors are read the same
// way: each that is a string as written, each of another kind as nothing, a
// line apart, trimmed, as the list decoded into strings and joined read.
//
// Mutation: read a key by its case, as encoding/json matches a struct's
// fields, and the block, the part or the item spelled `Type` is read as text;
// end a walk at an element that is no object, and the elements after it are
// lost; write one line between two errors however many said nothing between
// them, and the errors read as fewer lines than they were.
func TestAListIsReadAnElementAtATimeAsItsHarnessReadsIt(t *testing.T) {
	t.Parallel()
	const (
		others = `1,"x",null,[],{"type":5,"text":"retyped"},{"Type":"text","text":"folded"},`
		said   = `{"type":"text","text":"cut \ud83d"},{"type":"text","text":"bad ` + "\xff" + `"},`
		parts  = `[` + others + said + `{"type":"image","source":{}},{"type":"file","mime":"image/png"},` +
			`{"type":"text","text":"kept"}]`
		blocks = `[` + others + said + `{"type":"text","text":"kept"}]`
		errs   = `[1,null,"cut \ud83d",{"text":"x"},"bad ` + "\xff" + `",[],"Prompt is too long"]`
		ending = `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":` + errs + `}`
	)
	stream := func(line string) string {
		d := &claudeEvents{tools: map[string]string{}}
		d.Line([]byte(line))
		return d.Result().Transcript
	}
	entries := func(b *transcriptBuilder, err error) string {
		if err != nil {
			return "error: " + err.Error()
		}
		var shown []string
		for _, e := range b.entries {
			shown = append(shown, string(e.Kind)+" "+e.Tool+" "+strconv.FormatBool(e.Failed)+": "+e.Text)
		}
		return strings.Join(shown, " | ")
	}
	transcript := func(line string) string {
		b := listBuilder()
		return entries(b, b.claudeLine(t.Context(), []byte(line)))
	}
	step := func(content string) string {
		b := listBuilder()
		return entries(b, b.openCodeStep(t.Context(), json.RawMessage(content), time.Time{}))
	}
	reply := `{"type":"assistant","message":{"role":"assistant","content":` + blocks + `}}`
	result := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result",` +
		`"tool_use_id":"toolu_test","is_error":true,"content":` + parts + `}]}}`
	// A Claude Code result's image and an OpenCode tool's file of one are the
	// parts each harness answers with an image; neither reads the other's.
	answer := "cut \ufffd\nbad \ufffd\n[an image]\nkept"
	for name, tc := range map[string]struct{ got, want string }{
		"a run's event stream, a message's blocks": {stream(reply), "cut \ufffd\nbad \ufffd\nkept"},
		"a run's event stream, a failed call's result": {stream(result),
			"[tool] tool → error: cut \ufffd (+3 more line(s))"},
		"a run's event stream, its result's errors": {stream(ending),
			"[error] the run ended: error_during_execution: cut \ufffd (+4 more line(s))"},
		"a run's result, its errors": {func() string {
			res := ClaudeCode{}.Parse(ending)
			return res.Error + " | " + strconv.FormatBool(res.ContextExhausted)
		}(), "error_during_execution: cut \ufffd\n\nbad \ufffd\n\nPrompt is too long | true"},
		"a Claude Code transcript, a message's blocks": {transcript(reply),
			"model  false: cut \ufffd | model  false: bad \ufffd | model  false: kept"},
		"a Claude Code transcript, a call's result": {transcript(result),
			"tool_result  true: " + answer},
		"an OpenCode transcript, a step's items": {step(blocks),
			"model  false: cut \ufffd | model  false: bad \ufffd | model  false: kept"},
		"an OpenCode transcript, a tool's answer": {step(`[{"type":"tool","name":"bash","state":` +
			`{"status":"completed","content":` + parts + `}}]`),
			"tool_call bash false:  | tool_result bash false: " + answer},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: read as\n%q\nwant\n%q", name, tc.got, tc.want)
		}
	}
}
