package tools_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tools"
)

// A RECORD IS READ BY A PERSON, so it holds the characters the model sent.
//
// `json.Marshal` escapes `<`, `>` and `&` for an HTML document, and nothing on
// this path is one: the bytes go into a store column and come out as the text
// of a `pre`. Escaped, every URL argument on the transcript reads
// `?a=1\u0026b=2` and every markdown body `\u003ch1\u003e`.
func TestRecordedArgumentsAreNotEscapedForHTML(t *testing.T) {
	got, err := tools.RecordArgs(map[string]any{
		"url":  "https://example.com/?a=1&b=2",
		"body": "<h1>Title</h1>",
	})
	if err != nil {
		t.Fatalf("RecordArgs: %v", err)
	}
	if strings.Contains(got, "\\u0026") || strings.Contains(got, "\\u003c") {
		t.Errorf("RecordArgs = %s, want the characters rather than their escapes", got)
	}
	if !strings.Contains(got, "?a=1&b=2") || !strings.Contains(got, "<h1>Title</h1>") {
		t.Errorf("RecordArgs = %s, want the argument text verbatim", got)
	}
	// STILL JSON. The whole value of the record is that a reader — and the
	// resume path, which decodes it — can parse it.
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("RecordArgs produced something that is not JSON: %v", err)
	}
	if back["url"] != "https://example.com/?a=1&b=2" {
		t.Errorf("round trip = %v, want the url back", back["url"])
	}
}

// ENCODE WRITES A TRAILING NEWLINE and a record holds the document alone —
// otherwise every consumer has one byte to know to ignore, and the two that
// forgot would disagree about what "no arguments" looks like.
func TestRecordedArgumentsCarryNoTrailingNewline(t *testing.T) {
	got, err := tools.RecordArgs(map[string]any{"a": 1})
	if err != nil {
		t.Fatalf("RecordArgs: %v", err)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("RecordArgs = %q, want no trailing newline", got)
	}
	if got != `{"a":1}` {
		t.Errorf("RecordArgs = %q, want the compact document", got)
	}
}

// AN UNENCODABLE ARGUMENT IS THE CALLER'S TO SPELL. Each one records a
// different empty — `{}` on a phase event, "" on a bridged run's row — so this
// reports the failure rather than choosing between them.
func TestRecordArgsReportsWhatItCannotEncode(t *testing.T) {
	if _, err := tools.RecordArgs(map[string]any{"nan": math.NaN()}); err == nil {
		t.Error("RecordArgs(NaN) = nil error, want the failure reported to the caller")
	}
}

// A NUMBER SURVIVES AS THE MODEL SPELLED IT, which is the reason the wire
// carries text rather than a map at all: the provider layer decodes through
// json.Number so an id wider than a float64 is exact, and this must not be
// where that stops.
func TestRecordedArgumentsKeepAWideID(t *testing.T) {
	got, err := tools.RecordArgs(map[string]any{"id": json.Number("9007199254740993")})
	if err != nil {
		t.Fatalf("RecordArgs: %v", err)
	}
	if got != `{"id":9007199254740993}` {
		t.Errorf("RecordArgs = %s, want the id exactly", got)
	}
}

// READING A RECORD BACK IS THE SECOND DECODE, and the text form exists to
// survive it. Through the default decoder a 19-digit id becomes a float64 and
// re-encodes as a DIFFERENT id — on a resumed phase that then publishes the
// rounded one into its own event as though the model had asked for it.
func TestReadArgsKeepsAWideIDExact(t *testing.T) {
	args := tools.ReadArgs(`{"issue_id":1234567890123456789}`)
	got, err := tools.RecordArgs(args)
	if err != nil {
		t.Fatalf("RecordArgs: %v", err)
	}
	if got != `{"issue_id":1234567890123456789}` {
		t.Errorf("round trip = %s, want the id unchanged", got)
	}
}

// NIL AND EMPTY ARE DIFFERENT CLAIMS on a ledger line: nil renders as "no
// arguments shown", an empty map as "called with none".
func TestReadArgsAnswersNilForWhatItCannotRead(t *testing.T) {
	for _, raw := range []string{"", "not json", `{"a":1`, `["a"]`, "null"} {
		if got := tools.ReadArgs(raw); got != nil {
			t.Errorf("ReadArgs(%q) = %v, want nil", raw, got)
		}
	}
}

func TestReadArgsReadsAnOrdinaryDocument(t *testing.T) {
	got := tools.ReadArgs(`{"channel":"C1","text":"a & b"}`)
	if got["channel"] != "C1" || got["text"] != "a & b" {
		t.Errorf("ReadArgs = %v, want both arguments", got)
	}
}
