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
// works from (the prior-work ledger's, which elides per VALUE).
//
// THROUGH json.Number, not a bare `json.Unmarshal`: into an `any` a number
// becomes a float64, so a 19-digit id (Jira, Slack, GitHub node) comes back
// rounded. The record is TEXT so an id survives one decode and not two.
//
// NIL ON ANYTHING IT CANNOT READ, empty text included: nil renders as "no
// arguments shown", an empty map claims the tool was CALLED with none; failing
// the resume over one line would lose the whole conversation. Unlike
// [github.com/crewlet/crewlet/internal/toolsession] it does not REPORT parse
// failures: the text came from a record this engine wrote.
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
