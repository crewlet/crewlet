package toolargs

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"

	"github.com/crewlet/crewlet/internal/plainjson"
)

// Decode reads the arguments a caller sent one tool over a transport: nothing
// at all (empty, whitespace or a literal `null`) is a call with no arguments,
// and anything else must be exactly one JSON object. The map is never nil.
//
// THROUGH json.Number: as a float64 an integer past 2^53 is rounded before any
// tool sees it, so a 19-digit vendor id names a different entity, silently.
// EXACTLY ONE VALUE, as [github.com/crewlet/crewlet/internal/tools.ReadArgs]
// explains: a Decoder stops after one, dropping `{"a":2}` in `{"a":1} {"a":2}`.
// ONE FUNCTION for every transport, so none can read floats again. Arguments
// that do not decode are said in words ([plainjson.Unreadable]), the decoder's
// error beneath.
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

// Key is the SHA-256 of the text a run's log keeps of the arguments raw holds
// (internal/tools.RecordArgs over [Decode]): the package doc says why two
// spellings of one call's arguments key equal, and where they key apart. An
// error is raw that does not decode.
func Key(raw []byte) ([32]byte, error) {
	args, err := Decode(raw)
	if err != nil {
		return [32]byte{}, err
	}
	text, err := plainjson.Marshal(args, "")
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(text), nil
}

// unreadableArgs is a call's arguments that did not decode, said in words
// ([plainjson.Unreadable]); the decoder's error is beneath it, so a surface
// that calls the document something else words it again.
type unreadableArgs struct{ err error }

func (e *unreadableArgs) Error() string {
	return plainjson.Unreadable("the arguments object", e.err)
}
func (e *unreadableArgs) Unwrap() error { return e.err }
