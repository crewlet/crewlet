package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/plainjson"
)

// EVERY JSON DOCUMENT THIS COMMAND PRINTS GOES THROUGH HERE, because whoever
// reads one reads it as TEXT — a person at a terminal, a script matching a
// string, the fix loop `crewlet validate -json` exists for — and never drops it
// inside an HTML page, the one place `json.Marshal`'s escaping of `<`, `>` and
// `&` protects anything. Written that way a validation problem read
// `replicas \u003e 1 needs peers` and a Tier A token's login
// `token:\u003cid\u003e`, and a model fixing a document was handed escapes
// to decode back into the setting it was being told about.
//
// A NODE'S ANSWER IS DECODED AND PRINTED AGAIN rather than copied out as it
// came. The node writes every answer through one encoder that escapes markup
// uniformly (internal/api/httpjson says why), so the bytes as they came are
// exactly the escapes this file exists to keep off a terminal — and they are
// escaped at that one writer whatever produced each value, so no printer could
// promise text by copying them. What a copy kept and a decode would lose is
// nothing: the answer is decoded into a generic value, so every field this
// build does not know survives, and every number is kept as the digits the node
// sent ([decodeAnswer]). What changes is the layout: indented, and a map's keys
// in sorted order.

// printJSON writes v as indented JSON text, its strings as written, and ends
// the line.
func printJSON(w io.Writer, v any) error {
	text, err := plainjson.Marshal(v, "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", text)
	return err
}

// decodeAnswer decodes a node's JSON answer into v, keeping every number as the
// digits the node sent ([json.Number]) rather than as a float64.
//
// A FLOAT64 IS NOT A NUMBER THE NODE SENT once it is past 2^53: a 19-digit
// position or count decoded that way prints back as another number, and the
// answer printed is no longer the answer the node gave. The ONE decoder of a
// node's answer that is printed again is this one, so how a number is spelled
// is decided in one place.
//
// The whole answer is checked first, as [json.Unmarshal] checks it, so a body
// with anything after its value — a proxy's page appended to a truncated one —
// fills nothing and is refused.
func decodeAnswer(raw []byte, v any) error {
	var whole json.RawMessage
	if err := json.Unmarshal(raw, &whole); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(whole))
	dec.UseNumber()
	return dec.Decode(v)
}

// printAnswer prints a node's JSON answer through [printJSON]: every field it
// carries, every number as the node spelled it, and its text as written.
func printAnswer(w io.Writer, raw []byte) error {
	var answer any
	if err := decodeAnswer(raw, &answer); err != nil {
		return fmt.Errorf("the node's answer is not JSON: %w", err)
	}
	return printJSON(w, answer)
}
