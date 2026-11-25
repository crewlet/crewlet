package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/textindex"
)

// readPerson reads doc, an export, as a person from the page's start, every
// value past valueMax streamed, and with redactor where it is not nil.
func readPerson(t *testing.T, doc string, valueMax int, filter TranscriptFilter,
	redactor func(io.Writer) entryRedaction) (Transcript, error) {
	t.Helper()
	b := newTranscriptBuilder(0, TranscriptReadBounds(), filter)
	b.valueMax, b.redactor = valueMax, redactor
	return b.decode(t.Context(), OpenCode{}, bytes.NewReader(gzipOf(t, doc)))
}

// quoted is s as a JSON string.
func quoted(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// completedCall is a step of one completed call that answered with text.
func completedCall(t *testing.T, text string) string {
	t.Helper()
	return `{"type":"assistant","content":[{"type":"tool","name":"bash","state":{"status":"completed",` +
		`"input":{"c":1},"content":[{"type":"text","text":` + quoted(t, text) + `}]}}]}`
}

// A VALUE OF ANY LENGTH IS SHOWN AS THE OPENING OF ITS WHOLE REDACTION: a tool's
// answer of three MiB, streamed past the value bound, a credential across the
// bound and one at its end, is the opening of the whole text's redaction and
// states that redaction's size; under a term written at its end, the window
// around it is the whole redaction's.
//
// Mutation: stop the stream at the value bound, or redact the held opening
// alone, and the size or the opening goes red.
func TestAValueOfAnyLengthIsShownAsTheOpeningOfItsWholeRedaction(t *testing.T) {
	t.Parallel()
	const valueMax = 64 << 10
	secret := redacttest.Shapes()[0].Sample
	filler := strings.Repeat("a line of the build's log\n", (3<<20)/26)
	text := filler[:valueMax-len(secret)/2] + secret + filler + secret + " target"
	whole := redact.Secrets(text)
	page, err := readPerson(t, `{"messages":[`+completedCall(t, text)+`]}`, valueMax, TranscriptFilter{}, nil)
	if err != nil || len(page.Entries) != 2 {
		t.Fatalf("read (%d entries, %v), want the call and its answer", len(page.Entries), err)
	}
	answer := page.Entries[1]
	if want := textcut.Within(whole, TranscriptEntryMaxBytes); answer.Text != want || answer.Bytes != len(whole) ||
		!answer.Cut {
		t.Errorf("the answer reads %d bytes stating %d, cut %t; want the whole redaction's opening of %d bytes "+
			"and its %d", len(answer.Text), answer.Bytes, answer.Cut, len(want), len(whole))
	}
	terms := TranscriptFilter{Terms: []string{"target"}}
	page, err = readPerson(t, `{"messages":[`+completedCall(t, text)+`]}`, valueMax, terms, nil)
	first, _ := textindex.Plain.FindIn(redact.OwnWords(whole), terms.Terms)
	shown, from, cut := windowAround(whole, first, TranscriptEntryMaxBytes)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Text != shown || page.Places[0].TextFrom != from ||
		page.Entries[0].Cut != cut {
		t.Errorf("under a term at the end, the read is %d entries (%v); want the window of the whole redaction "+
			"from %d", len(page.Entries), err, from)
	}
}

// A READ ENDS AT ITS LINES BOUND: an export framing one record or element past
// [TranscriptReadLines] (a step of that many items) ends there, cut, its last
// entry saying so and nothing of the step read, while one framing exactly the
// bound reads whole; and the meter reads the export past the bound on.
//
// Mutation: drop the bound, or count a step's items as one record, and the
// read past it reads on.
func TestAReadEndsAtItsLinesBound(t *testing.T) {
	t.Parallel()
	step := func(items int) string {
		return `{"type":"assistant","content":[` + strings.Repeat(`{},`, items-1) + `{"type":"text","text":"x"}]}`
	}
	brief := `{"type":"user","text":"the brief"}`
	for _, tc := range []struct {
		items int
		cut   bool
	}{{TranscriptReadLines - 2, false}, {TranscriptReadLines - 1, true}} {
		// The brief and the step are two records; the step's items the rest.
		doc := `{"messages":[` + brief + `,` + step(tc.items) + `]}`
		page, err := readPerson(t, doc, maxLineBytes, TranscriptFilter{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		last := page.Entries[len(page.Entries)-1]
		switch {
		case !tc.cut && (page.Cut != "" || len(page.Entries) != 2 || last.Text != "x"):
			t.Errorf("%d items framed within the bound: %d entries, cut %q; want the brief and the step's words",
				tc.items, len(page.Entries), page.Cut)
		case tc.cut && (page.Cut != CutRead || len(page.Entries) != 2 || last.Kind != EntryUnread ||
			!strings.Contains(last.Text, fmt.Sprint(TranscriptReadLines))):
			t.Errorf("%d items framed past the bound: %d entries, cut %q, last %q; want the brief and the read "+
				"cut where the step began", tc.items, len(page.Entries), page.Cut, last.Text)
		}
		var count openCodeCountReading
		if _, err = countExport(t.Context(), bytes.NewReader(gzipOf(t, doc)), &count, nil); err != nil {
			t.Errorf("the meter reads an export past a person's lines bound: %v", err)
		}
	}
}

// A CALL'S RESULT WRITTEN OUT OF ORDER PAST THE BOUND IS SAID: a failed call
// whose answer runs past the value bound before its failure is written, or
// only joined with the failure written after it, or a call whose status is
// written after its answer, is an [EntryUnread] in place of its result, since a
// stream is written its text in the order it is shown; written in order, the
// failure's words open the result and the answer follows.
//
// Mutation: stream the answer whatever the order, or hold the joined text out
// of order, and the result shows the answer without the failure that opens it,
// or a text past the bound held.
func TestAFailedCallWrittenOutOfOrderPastTheBoundIsSaid(t *testing.T) {
	t.Parallel()
	answer := strings.Repeat("the build failed here\n", 100)
	state := func(s string) string {
		return `{"messages":[{"type":"assistant","content":[{"type":"tool","name":"bash","state":` + s + `}]}]}`
	}
	part := `[{"type":"text","text":` + quoted(t, answer) + `}]`
	for name, tc := range map[string]struct {
		doc    string
		unread bool
	}{
		"in order":                      {state(`{"status":"error","error":{"message":"boom"},"content":` + part + `}`), false},
		"the answer before its failure": {state(`{"status":"error","content":` + part + `,"error":{"message":"boom"}}`), true},
		"the status after the answer":   {state(`{"content":` + part + `,"status":"completed"}`), true},
		"joined past it, the answer first": {state(`{"status":"error","content":[{"type":"text","text":"` +
			strings.Repeat("y", 600) + `"}],"error":{"message":"` + strings.Repeat("m", 600) + `"}}`), true},
	} {
		page, err := readPerson(t, tc.doc, 1<<10, TranscriptFilter{}, nil)
		if err != nil || len(page.Entries) != 2 {
			t.Fatalf("%s: read (%d entries, %v)", name, len(page.Entries), err)
		}
		result := page.Entries[1]
		switch {
		case tc.unread && result.Kind != EntryUnread:
			t.Errorf("%s: the result reads %q; want it said as written out of order", name, result.Text)
		case !tc.unread && (result.Kind != EntryToolResult || !strings.HasPrefix(result.Text, "boom\n") ||
			result.Bytes != len("boom\n")+len(answer)):
			t.Errorf("%s: the result reads %.40q stating %d; want the failure's words, then the answer", name,
				result.Text, result.Bytes)
		}
	}
}

// A REDACTION THE STREAM REFUSES IS WITHHELD: a text past the value bound whose
// redaction the stream refuses is an [EntryUnread] in its entry's place, what
// the stream wrote never shown, and the record's other entries and the records
// after it are read.
//
// Mutation: show what the stream wrote, or let the refusal end the read, and
// a case goes red.
func TestARedactionTheStreamRefusesIsWithheld(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("the model's words ", 200)
	doc := `{"messages":[{"type":"assistant","content":[{"type":"text","text":` + quoted(t, long) + `},` +
		`{"type":"text","text":"short"}]},{"type":"user","text":"after"}]}`
	refusing := func(io.Writer) entryRedaction { return &refusingRedaction{} }
	page, err := readPerson(t, doc, 1<<10, TranscriptFilter{}, refusing)
	if err != nil || len(page.Entries) != 3 {
		t.Fatalf("read (%d entries, %v), want three", len(page.Entries), err)
	}
	if e := page.Entries[0]; e.Kind != EntryUnread || e.Text != unredactedText {
		t.Errorf("the refused text reads %s %.40q; want it withheld", e.Kind, e.Text)
	}
	if page.Entries[1].Text != "short" || page.Entries[2].Text != "after" {
		t.Errorf("the entries beside the refused one read %q and %q", page.Entries[1].Text, page.Entries[2].Text)
	}
}

// A RECORD'S KIND DECIDES ITS ENTRIES WHEREVER IT IS WRITTEN: a person's
// message, a step and a compaction whose `type` follows their words, past the
// value bound and within it, read as the same message with its `type` first,
// and a message whose `type` names no kind it read as one is said.
//
// Mutation: commit a message's entries as its words end, before its kind is
// read, and a case reads another page.
func TestARecordsKindDecidesItsEntriesWhereverItIsWritten(t *testing.T) {
	t.Parallel()
	long := quoted(t, strings.Repeat("words ", 400))
	for name, pair := range map[string][2]string{
		"a person's": {`{"type":"user","text":` + long + `}`, `{"text":` + long + `,"type":"user"}`},
		"a step's": {`{"type":"assistant","content":[{"type":"text","text":` + long + `}]}`,
			`{"content":[{"text":` + long + `,"type":"text"}],"type":"assistant"}`},
		"a compaction's": {`{"type":"compaction","status":"completed","summary":` + long + `}`,
			`{"summary":` + long + `,"status":"completed","type":"compaction"}`},
		"no kind": {`{"type":"later","text":` + long + `}`, `{"text":` + long + `,"type":"later"}`},
	} {
		for _, valueMax := range []int{maxLineBytes, 1 << 10} {
			first, err := readPerson(t, `{"messages":[`+pair[0]+`]}`, valueMax, TranscriptFilter{}, nil)
			if err != nil || len(first.Entries) != 1 {
				t.Fatalf("%s at %d: (%d entries, %v)", name, valueMax, len(first.Entries), err)
			}
			last, err := readPerson(t, `{"messages":[`+pair[1]+`]}`, valueMax, TranscriptFilter{}, nil)
			if err != nil || fmt.Sprint(last) != fmt.Sprint(first) {
				t.Errorf("%s at %d, its type last: %.200v (%v); want %.200v", name, valueMax, last, err, first)
			}
		}
	}
}
