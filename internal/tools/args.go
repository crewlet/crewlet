package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// PlainJSON is v as the JSON text its reader reads AS TEXT — a person shown a
// record of what a tool was called with, a model handed what a tool answered —
// indented by `indent` per level ("" for one line), and ending where the value
// does.
//
// NOT `json.Marshal`, and the difference is one each reader sees. Marshal
// escapes `<`, `>` and `&` into `\u003c`, `\u003e` and `\u0026` so its
// output can be dropped inside an HTML document without closing a tag. Nothing
// read through this is such a document. A record goes into a store column and
// comes out as the text of a `pre`, where every URL argument would read
// `?a=1\u0026b=2` and every markdown or HTML body `\u003ch1\u003e` on the one
// screen somebody opens to find out what a tool was actually called with. An
// answer is spliced into a model's context, which is shown a comment saying
// `if a < b && c > d` as six-byte escapes it has to decode back into the code
// it is reasoning about — five more bytes each of the turn's context and of
// whatever ceiling the answer is held to.
//
// The encoder's trailing newline is dropped: a record holds the document alone
// and an answer ends where its value does, so the newline would be one more
// byte every consumer has to know to ignore.
//
// ONE FUNCTION because every caller is a copy of one rule, which is how the
// copies in this tree have always started drifting: `internal/whsec` and
// `internal/textcut` are both what a second spelling of a small decision cost.
// What stays with each caller is what genuinely differs — the indent its
// reader is handed, and how it spells nothing at all.
//
// NOT the only place the rule is written down. `internal/agent/ledger` renders
// a call into a prior-work line for the model to re-read — and for a person to
// read, since that line is what the seat screen shows as the conversation
// ledger — and imports nothing from crewlet by design, so it keeps its own
// encoder with the same escaping off. The two point at each other rather than
// folding into one name, because the import would drag this whole tool layer
// behind a type the turn context, the prompt builder and the API all hold.
func PlainJSON(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// RecordArgs renders a call's arguments as the JSON text a RECORD holds — a
// phase event's payload, a bridged run's row — for a person to read back on a
// transcript afterwards, through [PlainJSON] on one line.
//
// What stays with each of its two callers is how it spells "no arguments": a
// phase event spells it as `{}` and a bridged run's row as the empty string,
// and an encoder that chose for them would be choosing what two different
// screens say.
func RecordArgs(args map[string]any) (string, error) {
	text, err := PlainJSON(args, "")
	if err != nil {
		return "", err
	}
	return string(text), nil
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
// NOT a call as the model made it, which a seat's tool session reads to the
// same number rule ([github.com/crewlet/crewlet/internal/toolsession]) but
// REPORTS what it could not parse, because a model's malformed arguments are a
// fact about this round that the tool loop answers the model with rather than
// running the call. Here the text came out of a record this engine wrote.
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
