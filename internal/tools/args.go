package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
// NOT [github.com/crewlet/crewlet/internal/providers/llm/httpapi.DecodeArgs],
// which reads the same shape off a PROVIDER's wire: that one REPORTS what it
// could not parse, because a model's malformed arguments are a fact about this
// round that the tool loop answers the model with rather than running the call.
// Here the text came out of a record this engine wrote.
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

// DecodeCallArgs reads the arguments a caller sent one tool over a transport —
// a person's request on the HTTP write surface or the act transport, an
// operator's assistant over MCP, a coding agent over the bridge: nothing at
// all (empty, whitespace or a literal `null`) is a call with no arguments, and
// anything else must be exactly one JSON object. The map is never nil.
//
// THROUGH json.Number, because every reader of a tool's arguments reads one —
// a seat's arguments arrive that way from its provider
// ([github.com/crewlet/crewlet/internal/providers/llm/httpapi.DecodeArgs]) and
// the tools are written for it. Decoded into a float64 instead, an integer
// past 2^53 is rounded before any tool sees it: a 19-digit vendor id names a
// different entity, and a whole-number custom-field value is stored as one
// its writer never typed, and nothing reports an error.
//
// EXACTLY ONE VALUE, for [ReadArgs]'s reason: a Decoder reads one value and
// stops, so a request body `{"a":1} {"a":2}` would decode clean with the
// second dropped in silence — a call nobody can say which half of to run.
//
// ONE FUNCTION for every transport, because the copies had already drifted:
// the bridge read through json.Number while the HTTP write surface, the act
// transport and the operator's MCP read every number as a float64.
func DecodeCallArgs(raw []byte) (map[string]any, error) {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || string(trimmed) == "null" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil {
		return nil, fmt.Errorf("the arguments are not a JSON object: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, errors.New("the arguments carry something after their JSON " +
			"object; send exactly one")
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}
