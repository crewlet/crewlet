package toolargs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/crewlet/crewlet/internal/plainjson"
)

// Decode reads the arguments a caller sent one tool over a transport: nothing
// at all (empty, whitespace or a literal `null`) is a call with no arguments,
// and anything else must be exactly one JSON object. The map is never nil.
//
// THROUGH json.Number, because every reader of a tool's arguments reads one —
// a seat's call arrives through its tool session
// ([github.com/crewlet/crewlet/internal/toolsession]), which decodes it here
// beside every other transport — and the tools are written for it. Decoded
// into a float64 instead, an integer past 2^53 is rounded before any tool sees
// it: a 19-digit vendor id names a different entity, and a whole-number
// custom-field value is stored as one its writer never typed, and nothing
// reports an error.
//
// EXACTLY ONE VALUE, for the reason
// [github.com/crewlet/crewlet/internal/tools.ReadArgs] gives: a Decoder reads
// one value and stops, so a request body `{"a":1} {"a":2}` would decode clean
// with the second dropped in silence — a call nobody can say which half of to
// run.
//
// ONE FUNCTION for every transport, because the copies had already drifted:
// the tool session read through json.Number but took the first of two values,
// while the HTTP write surface, the act transport and the operator's MCP read
// every number as a float64.
//
// ARGUMENTS THAT DO NOT DECODE ARE SAID IN WORDS ([plainjson.Unreadable]), the
// decoder's error beneath them for a caller that names the document otherwise:
// a model and a person each read "invalid character '}' looking for beginning
// of object key string" where they now read where the arguments went wrong.
func Decode(raw []byte) (map[string]any, error) {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || string(trimmed) == "null" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil {
		return nil, &unreadableArgs{err: err}
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, &unreadableArgs{err: plainjson.ErrTrailing}
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

// unreadableArgs is a call's arguments that did not decode, said in words
// ([plainjson.Unreadable]); the decoder's error is beneath it, so a surface
// that calls the document something else words it again.
type unreadableArgs struct{ err error }

func (e *unreadableArgs) Error() string {
	return plainjson.Unreadable("the arguments object", e.err)
}
func (e *unreadableArgs) Unwrap() error { return e.err }
