package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/plainjson"
)

// EVERY JSON DOCUMENT THIS COMMAND PRINTS GOES THROUGH HERE, because whoever reads
// one reads it as TEXT (a terminal, a script matching a string, the fix loop
// `crewlet validate -json` exists for) and never inside an HTML page, the one
// place `json.Marshal`'s escaping of `<`, `>` and `&` protects anything.
//
// A NODE'S ANSWER IS DECODED AND PRINTED AGAIN rather than copied. The node's one
// encoder escapes markup uniformly (internal/api/httpjson says why), so its bytes
// are exactly the escapes this file keeps off a terminal. A decode loses nothing:
// the answer goes into a generic value, so unknown fields survive and every
// number is kept as the node's digits ([decodeAnswer]). Only the layout changes:
// indented, a map's keys sorted.

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
