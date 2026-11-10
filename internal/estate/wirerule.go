package estate

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
)

// AN OPERATION'S ARGUMENTS CROSS WITH EVERY FIELD AT ITS ZERO LEFT OFF.
//
// # Why
//
// A rolling upgrade mixes builds, and the node that asks is not always the
// build of the data node that answers. A field a later build adds to an
// operation's arguments must reach a data node of an earlier build only where
// a write SETS it: a write that does not use it is then served by a data node
// of either build exactly as before the field existed, since the field's zero
// means what a build without it does ([define] states the rule). Encoded as
// json.Marshal writes every field, every write of the later build would carry
// every new key at its zero, which no older build declares.
//
// And the wire's own reading of a value is what it was: a nil json.RawMessage
// crossed as `null`, which a decoder reads back as the four bytes "null" — so
// a seat's first told record, a baseline that tells no change, arrived on a
// data node carrying one, and was refused there as a baseline that tells a
// change. Left off at its zero, it arrives nil, as it left.
//
// # The standard library's own rule
//
// [jsonv2.OmitZeroStructFields], which encoding/json documents as `omitzero`
// on every field — a value is zero by its type's own IsZero method where it has
// one, and by Go's zero otherwise — over json.Marshal's options in every other
// respect ([json.DefaultOptionsV1]), so a field with a value is written byte
// for byte as json.Marshal writes it, and nothing here decides which fields an
// encoding carries.
//
// # The in-process path takes none of it
//
// A data node's own router answers its own seats from its own copy in process
// ([Router.tryLocal]) with the caller's own values: nothing is encoded, so
// nothing here runs.
var inputEncoding = jsonv2.JoinOptions(json.DefaultOptionsV1(), jsonv2.OmitZeroStructFields(true))

// encodeInput is v — an operation's arguments — as it crosses the wire:
// json.Marshal's encoding with every struct field at its zero left out
// ([inputEncoding]).
func encodeInput(v any) ([]byte, error) {
	return jsonv2.Marshal(v, inputEncoding)
}
