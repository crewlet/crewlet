package executor

import (
	"bytes"
	"strings"
	"testing"
)

// transcriptOf is what c reads of archive at the engine's bounds.
func transcriptOf(t *testing.T, c carrier, archive []byte) (Transcript, error) {
	t.Helper()
	b := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{})
	return b.decode(t.Context(), c, bytes.NewReader(archive))
}

// texts is each entry's kind and text, a line each.
func texts(page Transcript) string {
	var out []string
	for _, e := range page.Entries {
		out = append(out, string(e.Kind)+": "+e.Text)
	}
	return strings.Join(out, "\n")
}

// hermesStored is a Hermes content stored as JSON, its JSON text value.
func hermesStored(value string) string { return boundedQuoted(hermesContentJSON + value) }

// storedImage is an image's data URL a stored content holds, 26 bytes.
const storedImage = "data:image/png;base64,AAAA"

// A HERMES CONTENT STORED AS JSON IS READ AS HERMES READS IT BACK, the oracle
// reading it as the words its JSON is: a list's text parts a line apart and
// each image in its place, a tool's `_multimodal` envelope by its parts, a
// string the words it holds (Hermes's escape of a literal opening with the
// prefix), and one of no shape Hermes reads, or JSON it cannot decode (the
// prefix alone among them), said in its place. A summary row's words are the
// same on the receipts' walk.
//
// Mutation: read a stored content as its words, join its parts with nothing
// between, read an envelope whose `_multimodal` is not true, or keep an
// image's bytes: a case fails.
func TestAHermesContentStoredAsJSONIsReadAsHermesReadsItBack(t *testing.T) {
	t.Parallel()
	parts := `[{"type": "text", "text": "seen"}, {"type": "image_url", "image_url": {"url": "` + storedImage + `"}}]`
	for _, c := range []struct {
		name, content, want string
	}{
		{"a list of parts", hermesStored(parts), "seen\n[an image of 26 bytes]"},
		{"an envelope", hermesStored(`{"_multimodal": true, "content": ` + parts + `, "text_summary": "s"}`),
			"seen\n[an image of 26 bytes]"},
		{"an envelope that is none", hermesStored(`{"_multimodal": false, "content": ` + parts + `}`),
			hermesStoredOdd},
		{"an envelope of no list", hermesStored(`{"_multimodal": true, "content": "s"}`), hermesStoredOdd},
		{"a literal", hermesStored(boundedQuoted(hermesContentJSON + "words")), hermesContentJSON + "words"},
		{"a number", hermesStored(`7`), hermesStoredOdd},
		{"JSON it cannot decode", hermesStored(`[{"type": "text"`), hermesStoredOdd},
		{"JSON and more", hermesStored(parts + ` x`), hermesStoredOdd},
		{"the prefix alone", boundedQuoted(hermesContentJSON), hermesStoredOdd},
	} {
		row := `{"t": "messages", "r": {"id": 5, "role": "assistant", "content": ` + c.content + `}}`
		page, err := transcriptOf(t, Hermes{}, boundedArchiveOf(t, hermesMembers([]string{row})...))
		if err != nil || len(page.Entries) != 1 || page.Entries[0].Text != c.want {
			t.Errorf("%s: read\n%s\n(%v), want %q", c.name, texts(page), err, c.want)
		}
		if c.want == hermesStoredOdd {
			continue
		}
		summary := `{"t": "messages", "r": {"id": 6, "role": "user", "_compressed_summary": 1, "content": ` +
			c.content + `}}`
		var s summaryRead
		if _, err := hermesNotesHeld(t.Context(), strings.NewReader(summary+"\n"), nil, 0, &s); err != nil {
			t.Fatal(err)
		}
		if got := s.reading(); got.Text != c.want {
			t.Errorf("%s: the summary reads %q (%q), want %q", c.name, got.Text, got.Unread, c.want)
		}
	}
}
