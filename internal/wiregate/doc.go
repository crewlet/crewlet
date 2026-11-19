// Package wiregate holds every flag and number the engine sends a client to
// being sent, false and 0 included, and the dashboard's wire types to
// declaring each as it is sent.
//
// # The class
//
// encoding/json leaves a bool's false and a number's 0 out of the object
// under `omitempty` or `omitzero`. A Go reader decodes the missing key as the
// zero, but the dashboard reads JSON as it comes: a missing key is undefined,
// and each screen picks what undefined means. worklet.Blocker's `open` was
// left out when false, and the blocked chip read a finished blocker as still
// open while the timeline read it as closed. So a flag or number a client
// receives is sent always, unless the field says why a missing key and its
// zero are one value to every reader:
//
//	Pinned bool `json:"pinned,omitempty"` //wiregate:absent-is-false a mark the row draws only when set
//
// The marker is `//wiregate:absent-is-false` on a bool and
// `//wiregate:absent-is-zero` on a number, with a reason, after the field or
// as a line of its doc comment. A marker on a field the gate does not judge is
// refused, so none outlives its field.
//
// # What reaches a client
//
// The surfaces are internal/api (REST, the socket, the assistant's stream) and
// the CLI's printJSON. Every encoder they write through takes `any`, and so
// does everything carrying a value to one (a query's answer, a frame's data, a
// refusal's detail), so a ROOT is the static type of any value a surface hands
// to an empty interface, typed by go/types over the surfaces' source
// (sourcetree.TypeCheck). A value handed where it is only decoded into, formatted,
// logged or hashed is not one: an argument a decoder writes into, found from
// encoding/json's and yaml's decoders through every surface function that hands
// its parameter on to one, and anything handed to fmt, log, slog, errors,
// context, sync, reflect or the operation-id digest. The walk then reads every
// field encoding/json writes, an untagged embedded struct promoted.
//
// A field typed as an interface or raw JSON is OPAQUE: its struct is decided at
// run time or encoded elsewhere (an event's payload, a live call's rounds). Each
// one on the wire has a row in opaqueFields naming the types it carries, which
// are walked as roots (the event catalogue is read from its registry), or why
// it carries none of this module's, so a new opaque field fails until it says.
//
// A DOCUMENT is JSON somebody writes (the company's configuration, a dashboard
// artifact's spec): a key left out is the default its schema publishes, and one
// defaulting to other than its zero is a pointer or an org.Toggle, so its
// fields are not judged. A type writing its own JSON is not judged either: its
// zero is its own to state.
//
// # The dashboard's declarations
//
// dashboard/src/protocol and its contract name no Go type, so each interface
// there is paired with the struct whose object holds all its members, the one
// with fewest keys besides; where several tie and send a member differently it
// is not judged, and unpaired names an interface whose keys match a struct it
// does not mirror. A flag or number member is declared required where the
// struct always sends it, so no screen writes a default for a key never
// missing, and optional where the struct leaves it out at zero, so no screen
// trusts a key that may be missing.
//
// # An inventory that only shrinks
//
// The gate arrived after the code: what the tree held then is in
// inventory_test.go, omitted fields by package and optional members by
// interface. A finding without an entry fails, and so does an entry with no
// finding, so a field sent always or marked leaves its line, and a new one
// never gets one. The entries go a package at a time, each field sent always
// (its member made required and its readers made to read it as stated) or
// marked.
//
// # Not seen
//
// A value a surface reaches only through an opaque row that does not list it;
// a value encoded and handed over as bytes outside a surface; a tool's answer
// to a model, which internal/agent/builtin shapes for a reader that is not a
// screen; and a dashboard shape declared as a type alias rather than an
// interface.
//
// The package is test-only: this file is its documentation and the gates are
// its tests.
package wiregate
