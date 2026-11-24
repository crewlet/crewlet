package executor

import (
	"bytes"
	"strings"
	"testing"
)

// A BOUNDED RECORD'S KEY MATCHES ITS NAME EXACTLY, AND ONE WRITTEN TWICE HOLDS
// ITS LAST VALUE, as the harness's own JSON.parse reads it ([fieldKind]): an
// object written twice is the second, never the two merged as encoding/json
// merged them, and a key in another case is no key of the record's.
//
// Mutation: keep a message's or an event's members across a second one, or
// match a name in any case: a case fails.
func TestABoundedRecordsKeyIsItsExactNameAndItsLastValue(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		carrier carrier
		members []boundedMember
		want    string
	}{
		{"a Kimi event written twice", Kimi{}, kimiMembers([]string{`{"type":"context.append_loop_event",` +
			`"event":{"type":"content.part","part":{"type":"text","text":"first"}},"event":{"part":{"type":` +
			`"text","text":"merged"}}}`}, nil), ""},
		{"a Kimi summary written twice", Kimi{}, kimiMembers([]string{`{"type":"context.apply_compaction",` +
			`"summary":"first","summary":"last"}`}, nil), "summary:last"},
		{"a Kimi key in another case", Kimi{}, kimiMembers([]string{`{"type":"context.apply_compaction",` +
			`"Summary":"cased"}`}, nil), ""},
		{"Copilot data written twice", Copilot{}, copilotMembers([]string{`{"type":"assistant.message",` +
			`"data":{"content":"first"},"data":{"toolRequests":[]}}`}), ""},
		{"a Copilot type in another case", Copilot{}, copilotMembers([]string{`{"TYPE":"user.message",` +
			`"data":{"content":"cased"}}`}), "unread:a line of 50 bytes of the harness's record is not a " +
			"record, and was not read"},
		{"a Pi message written twice", Pi{}, piMembers([]string{`{"type":"message","message":{"role":"user",` +
			`"content":"first"},"message":{"content":"merged"}}`}), ""},
		{"a Pi block written twice", Pi{}, piMembers([]string{`{"type":"message","message":{"role":"user",` +
			`"content":[{"type":"text","text":"first","text":"last"}]}}`}), "brief:last"},
	} {
		b := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{})
		page, err := b.decode(t.Context(), c.carrier, bytes.NewReader(boundedArchiveOf(t, c.members...)))
		var got []string
		for _, e := range page.Entries {
			got = append(got, string(e.Kind)+":"+e.Text)
		}
		if err != nil || strings.Join(got, "\n") != c.want {
			t.Errorf("%s: read %q (%v), want %q", c.name, got, err, c.want)
		}
	}
}
