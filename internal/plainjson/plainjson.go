// Package plainjson writes JSON for a reader who reads it AS TEXT — a person
// shown a record of what a tool was called with, a model handed what a tool
// answered or reminded what it has already done — with `<`, `>` and `&`
// written as themselves.
//
// # Not json.Marshal
//
// The difference is one every reader here sees. `json.Marshal` escapes `<`,
// `>` and `&` into `\u003c`, `\u003e` and `\u0026` so that its output can be
// dropped inside an HTML document without closing a tag. Nothing written
// through this package is such a document:
//
//   - a RECORD ([github.com/crewlet/crewlet/internal/tools.RecordArgs]: a phase
//     event's payload, a bridged run's row) goes into a store column and comes
//     out as the text of a `pre`, where every URL argument would read
//     `?a=1\u0026b=2` and every markdown or HTML body `\u003ch1\u003e` on the
//     one screen somebody opens to find out what a tool was called with;
//   - a TOOL ANSWER (the engine's builtins, a delegated worker's report) is
//     spliced into a model's context, which is shown a comment saying
//     `if a < b && c > d` as six-byte escapes it has to decode back into the
//     code it is reasoning about — five more bytes each of the turn's context
//     and of whatever ceiling the answer is held to;
//   - a PRIOR-WORK LINE ([github.com/crewlet/crewlet/internal/agent/ledger])
//     is re-read by the model every round and shown to a person on the seat
//     screen as the conversation ledger, and is neither kind of document;
//   - a CLI RUN'S TRANSCRIPT ([github.com/crewlet/crewlet/internal/providers/llm/cliagent])
//     shows the model its own earlier calls, which it reads as written:
//     shown a `<` it typed as an escape, it reads a call it never made;
//   - a COMMAND'S -json (`cmd/crewlet`: a validation's problems, a node's
//     answer decoded and printed again) is read at a terminal, matched by a
//     script and handed to the loop that fixes a document from
//     `crewlet validate -json`, where `replicas \u003e 1` and
//     `token:\u003cid\u003e` were escapes every one of them had to decode back
//     into the setting it was being told about;
//   - a TIER SCHEMA ([github.com/crewlet/crewlet/internal/config.Schema], the
//     committed and published `schema/*.schema.json`) is read by an editor
//     showing a field's description on hover, by a person reading the file
//     and by a model authoring a document against it.
//
// # The text ends where its value does
//
// The encoder's trailing newline is dropped: a record holds the document
// alone and an answer ends where its value does, so the newline would be one
// more byte every consumer has to know to ignore. With that gone, what
// [Marshal] writes under an indent is byte for byte what
// `json.MarshalIndent(v, "", indent)` writes apart from the three escapes, and
// what it writes under none is `json.Marshal`'s apart from them — key order
// included, a map's keys sorted — so a reader moved onto it changes in exactly
// the characters this package exists for.
//
// # One function
//
// Because every caller is a copy of one rule, which is how the copies in this
// tree have always started drifting: `internal/whsec` and `internal/textcut`
// are both what a second spelling of a small decision cost. What stays with
// each caller is what genuinely differs — the indent its reader is handed, and
// how it spells nothing at all.
//
// # A leaf
//
// Its readers sit at heights no other package spans. The tool layer imports
// the agent runtime, the MCP client and the coordination store; the ledger is
// a value the turn context, the prompt builder and the API all hold, and
// imports nothing from crewlet but this; a provider backend sits below the
// tool registry. Kept in the tool layer, the rule had to be copied into the
// ledger, whose import of it would have dragged that whole layer behind every
// holder of a ledger value — so it lives here, importing nothing but the
// standard library, and a test holds it to that.
//
// # Not every unescaped encoder in the tree is this one
//
// The coordination store's activation record ([github.com/crewlet/crewlet/internal/coord/kv])
// also writes with escaping off, for a different reason: a payload it embeds
// must come back byte for byte the body [github.com/crewlet/crewlet/internal/coord.CanonicalPayload]
// promised. That is a contract about bytes on a wire, not about how text
// reads, and a coordination encoder does not depend on a rule that may move
// for a reader's sake.
//
// Nor is the OpenCode readiness check's spelling of a server's name
// ([github.com/crewlet/crewlet/internal/sandbox/codingagent]). It looks for
// the name inside OpenCode's own answer, which JavaScript's JSON.stringify
// wrote, so it leaves U+2028 and U+2029 as themselves too — and
// encoding/json, which this package writes through, always escapes those two.
// It spells a peer's bytes, not text for a reader.
package plainjson

import (
	"bytes"
	"encoding/json"
)

// Marshal is v as the JSON text its reader reads as text, indented by
// `indent` per level ("" for one line), with `<`, `>` and `&` as themselves
// and no trailing newline.
//
// A value JSON cannot spell (a NaN, a channel, a cycle) is an error, as it is
// from `json.Marshal`: what to show instead is each caller's decision.
func Marshal(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
