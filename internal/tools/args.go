package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// RecordArgs renders a call's arguments as the JSON text a RECORD holds — a
// phase event's payload, a bridged run's row — for a person to read back on a
// transcript afterwards.
//
// NOT `json.Marshal`, and the difference is one that reader sees. Marshal
// escapes `<`, `>` and `&` into `\u003c`, `\u003e` and `\u0026` so its
// output can be dropped inside an HTML document without closing a tag. Nothing
// on this path is such a document: the bytes go into a store column and come
// out as the text of a `pre`. What the escaping buys here is nothing, and what
// it costs is that every URL argument reads `?a=1\u0026b=2` and every
// markdown or HTML body `\u003ch1\u003e` on the one screen somebody opens
// to find out what a tool was actually called with.
//
// ONE FUNCTION because the two callers are two copies of one rule, which is
// how the copies in this tree have always started drifting: `internal/whsec`
// and `internal/textcut` are both what a second spelling of a small decision
// cost. What stays with each caller is what genuinely differs — a phase event
// spells "no arguments" as `{}` and a bridged run's row spells it as the empty
// string, and an encoder that chose for them would be choosing what two
// different screens say.
//
// NOT the only place the rule is written down. `internal/agent/ledger` renders
// a call into a prior-work line for the model to re-read — and for a person to
// read, since that line is what the seat screen shows as the conversation
// ledger — and imports nothing from crewlet by design, so it keeps its own
// encoder with the same escaping off. The two point at each other rather than
// folding into one name, because the import would drag this whole tool layer
// behind a type the turn context, the prompt builder and the API all hold.
func RecordArgs(args map[string]any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return "", err
	}
	// Encode writes a trailing newline after the document. A record holds the
	// document alone — the newline would be one more byte every consumer has
	// to know to ignore.
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// ReadArgs turns a recorded argument document back into the map a renderer
// works from — the prior-work ledger's, which elides per VALUE and therefore
// cannot work from text.
//
// THROUGH json.Number, and that is the whole reason this is not a bare
// `json.Unmarshal`. Decoded into an `any` a number becomes a float64, so a
// 19-digit id — a Jira issue id, a Slack timestamp, a GitHub node id — comes
// back as 1.2345678901234568e+18 and re-encodes as 1234567890123456800. The
// record is written as TEXT precisely so an id survives one decode and not
// two; a resumed phase that read its own prior calls with the default decoder
// was the second one, and it re-published the rounded id into the next phase
// event as though the model had asked for it.
//
// NIL ON ANYTHING IT CANNOT READ, empty text included, and that is the
// caller's contract rather than a shortcut: nil renders as "no arguments
// shown" while an empty map is the claim that the tool was CALLED with none.
// A resumed turn losing one line's arguments is a worse-rendered ledger;
// failing the resume over it loses the whole conversation.
//
// NOT [github.com/crewlet/crewlet/internal/providers/llm/httpapi.DecodeArgs],
// which reads the same shape off a PROVIDER's wire: that one logs what it
// could not parse and answers an empty map, because a model's malformed
// arguments are a fact about this round that the round has to survive. Here
// the text came out of a record this engine wrote.
func ReadArgs(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil {
		return nil
	}
	// EXACTLY ONE DOCUMENT, which a Decoder does not otherwise insist on:
	// it reads one value and stops, so `{"channel":"C"}garbage` decodes
	// clean and the suffix is dropped in silence — where the `json.Unmarshal`
	// this replaced refused the whole thing. A record that is two documents,
	// or one with a tail, is a record something truncated or spliced, and
	// "nil on anything it cannot read" is what the callers are promised.
	// Trailing whitespace is not a tail: the encoder that writes these ends
	// its document with a newline, which the writer trims and a reader must
	// not start refusing over.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil
	}
	return args
}
