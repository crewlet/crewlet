package executor

import (
	"bytes"
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The lists a harness's record holds, as the readers outside the thread layer
// read them: a run's event stream ([claudeEvents]), its result
// ([ClaudeCode.Parse]) and a person's transcript of either harness's session
// ([transcriptBuilder]).

// listBytes is how long a list the cost case reads: hundreds of thousands of
// elements, so a per-element allocation shows, yet small enough that a mutant's
// whole decode still fits a test's memory. Cost is per element, so this stands
// for a list at the 32 MiB a line may hold ([maxLineBytes]).
const listBytes = 1 << 20

// listCopies is how many copies of the list a reader may make: three at most,
// measured over every reader held to it (the stream's and the result's make
// none, [wireAlias]), plus one for headroom. A reader may never spend anything
// per element.
const listCopies = 4

// listSlack is how many more allocations a list read to its end may cost than a
// list of one: the count is the process's, and a collection set off mid-measure
// empties the pools (the decoders, fmt's printers), which a read then refills
// at a few allocations each; under the race detector a pool also drops a
// quarter of what is put in it. A per-element allocation is hundreds of
// thousands.
const listSlack = 64

// listBuilder is a transcript read's builder, as a read from the archive's
// first entry starts one.
func listBuilder() *transcriptBuilder {
	return newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{})
}

// allocationsOf is what read allocates, heap objects and bytes, the fewest of
// three calls after a warm-up, read off the process's counts with no goroutine
// but the test binary's own beside each ([requireNoStrays]). For a reader that
// allocates on goroutines it starts, or draws on a sync.Pool, which
// [ownAllocations]' collections would empty. GOMAXPROCS is one meanwhile, as
// testing.AllocsPerRun's is.
func allocationsOf(t testing.TB, read func()) (objects, bytes uint64) {
	t.Helper()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	read()
	objects, bytes = math.MaxUint64, math.MaxUint64
	var before, after runtime.MemStats
	for range 3 {
		requireNoStrays(t)
		runtime.ReadMemStats(&before)
		read()
		runtime.ReadMemStats(&after)
		objects = min(objects, after.Mallocs-before.Mallocs)
		bytes = min(bytes, after.TotalAlloc-before.TotalAlloc)
	}
	return objects, bytes
}

// heldBy is how many bytes what read returns keeps alive, the fewest of three
// calls: heap in use after a collection with the answer held, less that after a
// collection before the call. Unlike [allocationsOf]'s bytes, what the read let
// go of along the way is no part of it. No goroutine but the test binary's own
// may run beside it ([requireNoStrays]); GOMAXPROCS is one while it measures.
func heldBy(t testing.TB, read func() any) uint64 {
	t.Helper()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	held := uint64(math.MaxUint64)
	var before, after runtime.MemStats
	for range 3 {
		// Twice: a sync.Pool's victim cache outlives one, and freed after the read it offsets the read's hold.
		runtime.GC()
		runtime.GC()
		requireNoStrays(t)
		runtime.ReadMemStats(&before)
		answer := read()
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(answer)
		held = min(held, after.HeapAlloc-min(before.HeapAlloc, after.HeapAlloc))
	}
	return held
}

// A LIST IN A HARNESS'S RECORD COSTS ITS READER ONE ELEMENT AT A TIME: every
// list the event stream, the result and a transcript read (a message's blocks,
// a call's result, a result's errors, an OpenCode step's items, a tool's
// answer) is read and checked an element at a time, so hundreds of thousands of
// elements that say nothing cost no more allocations than one, and no more
// bytes than a few copies of the list.
//
// Not parallel: the counts are the process's.
//
// Mutation: decode any of those lists whole before reading the first element,
// or have the blocks' walk collect them before handing out the first, and the
// list costs hundreds of thousands of allocations, or tens of MiB, more.
func TestAListInAHarnesssRecordCostsItsReaderOneElementAtATime(t *testing.T) {
	// Each read is handed bytes made once, outside what is measured, as a
	// reader is handed a line it did not copy.
	stream := func(line string) func() {
		raw := []byte(line)
		return func() { (&claudeEvents{}).Line(raw) }
	}
	parse := func(line string) func() {
		raw := []byte(line)
		return func() { _ = ClaudeCode{}.Parse(raw) }
	}
	transcript := func(line string) func() {
		raw := []byte(line)
		return func() { _ = listBuilder().claudeLine(t.Context(), raw) }
	}
	step := func(content string) func() {
		raw := []byte(`{"messages":[{"type":"assistant","content":` + content + `}]}`)
		return func() { _ = listBuilder().openCodeExport(t.Context(), bytes.NewReader(raw)) }
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
		fewObjects, fewBytes := allocationsOf(t, tc.read(one))
		manyObjects, manyBytes := allocationsOf(t, tc.read(full))
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

// A LIST IS READ AN ELEMENT AT A TIME, AS ITS HARNESS READS IT: an element that
// is no object is no block, item or part, a key of the wrong kind reads as
// nothing, and each costs itself alone, the elements after it still read; a key
// is matched exactly, as both harnesses' JSON.parse does, so `Type` is no
// block's type; a lone surrogate or invalid UTF-8 reads as U+FFFD, never as no
// list. A result's errors likewise: each string as written, anything else as
// nothing, a line apart, trimmed, as a decode into strings joined would read.
//
// Mutation: match keys by case, as encoding/json does, and `Type` reads as text;
// end a walk at a non-object, and the elements after it are lost; write one
// line between two errors however many said nothing between them, and the
// errors read as fewer lines than they were.
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
		d := &claudeEvents{}
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
		return entries(b, b.openCodeExport(t.Context(),
			strings.NewReader(`{"messages":[{"type":"assistant","content":`+content+`}]}`)))
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
			"[failed] cut \ufffd (+3 more line(s))"},
		"a run's event stream, its result's errors": {stream(ending),
			"[error] the run ended: error_during_execution: cut \ufffd (+4 more line(s))"},
		"a run's result, its errors": {func() string {
			res := ClaudeCode{}.Parse([]byte(ending))
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
