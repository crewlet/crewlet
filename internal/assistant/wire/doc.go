// Package wire is the Dashboard Assistant's protocol vocabulary: every value
// the engine and the dashboard exchange about a conversation, a turn, its
// steps, its cards and its proposals, and the one encoding of the live stream.
//
// # A leaf, and both sides read it
//
// The engine side is this package and nothing else: the orchestrator, the
// HTTP surface, the frame log and the cards all build these values, so no two
// of them can spell a frame differently. The dashboard side is
// `dashboard/src/contract/assistant.ts`, held to this package in both
// directions by the gates in client_gate_test.go through internal/clientsource:
// every closed set is the same set, every limit the same number, and every
// shape carries the same members with the same optionality (a Go field that
// is `omitempty` is optional in TypeScript, and every other is required). A
// golden stream this package's encoder writes (testdata/frames.sse) is read by
// the dashboard's own suite through its own decoder, so a change to either
// side is a failing test until the other follows it.
//
// It imports the standard library and the uuid package only, which is what
// lets every assistant package, the API surface and the domain depend on it
// without depending on each other.
//
// # The stream
//
// A turn's live answer is a server-sent event stream read over fetch (never
// the dashboard socket, whose drop-oldest queue would corrupt a transcript
// rebuilt from deltas, and never EventSource, which cannot carry the
// dashboard's step-up and retry handling). Every frame is one SSE event whose
// `id` is `<turn_id>.<seq>`, whose `event` is the frame's [EventKind] and
// whose `data` is the [Frame] as one line of JSON. seq starts at 1 and only
// grows within a turn, so a client resuming with Last-Event-ID is replayed
// exactly what it has not drawn, and a frame it has already drawn is dropped
// by seq. A comment line ([KeepaliveComment]) is written every
// [KeepaliveSeconds] so a proxy never closes an idle stream.
//
// A FRAME NESTS ITS PAYLOAD under a member named for its kind
// (`{"kind":"step","step":{...}}`) rather than spreading it beside the head.
// Spread, a payload member could collide with a head member a later version
// adds, and the TypeScript side could not narrow a frame by its kind without
// listing every member of every payload.
//
// # Evolution
//
// [ProtocolVersion] is carried on every frame and every request. Within a
// version evolution is additive only: a client ignores an event kind it does
// not know and renders a card kind it does not know as a notice, and the
// server refuses a request naming a version it does not speak with
// `409 assistant_protocol_mismatch`, which the dashboard answers by asking the
// person to reload. A reshape takes a new version, never a reused one.
//
// # What the browser never receives
//
// Raw tool arguments, raw tool results, prompts, token counts, grants and
// request headers. A step carries a label the engine renders from the call's
// safe fields and the route it asked, a card is built by the engine from the
// tool's own answer, and a proposal card is rendered from a server-side
// preview, never from model prose (ADR-0051).
//
// # A read is named, never carded
//
// A read is never drawn as a card (ADR-0061): the drawer makes the answer's
// first mention of each record the turn read that record's chip, finding it by
// the key an EntityCard carries (a task's, a project's) or, for the kinds whose
// title is a name, by its title, and lists the reads the answer never named
// under it. Only a read the engine carded can become a chip, so nothing the
// model writes becomes a link.
package wire
