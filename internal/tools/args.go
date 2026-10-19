package tools

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/plainjson"
)

// RecordArgs renders a call's arguments as the JSON text a RECORD holds — a
// phase event's payload, a bridged run's row — for a person to read back on a
// transcript afterwards: [plainjson.Marshal] on one line, so a URL argument
// reads `?a=1&b=2` on the screen rather than the escapes an HTML-safe encoder
// writes (that package's doc says why it is not `json.Marshal`).
//
// What stays with each of its two callers is how it spells "no arguments": a
// phase event spells it as `{}` and a bridged run's row as the empty string,
// and an encoder that chose for them would be choosing what two different
// screens say.
func RecordArgs(args map[string]any) (string, error) {
	text, err := plainjson.Marshal(args, "")
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
