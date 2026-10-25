// Package cards turns a tool's answer into the engine-built cards the
// Dashboard Assistant shows: a table, an entity, an open in app link, an error
// or a notice, each a wire.Card (internal/assistant/wire).
//
// # Built from the answer, never by the model
//
// A number on a table card is the engine's number, read from the route's own
// JSON by the tool's descriptor, so a model can describe a result but never
// author one. A card is capped (1000 rows, 256 KiB encoded) and says so with a
// notice when it is cut; the model is handed a compact summary of the card
// (its columns, its row count, its first rows) under the turn's fence rather
// than the card itself.
//
// # A link is the engine's and is decided for the person
//
// An open in app card's address comes from this package's link table, held
// against the dashboard's own route table, never from a URL a model wrote. Its
// `permitted` is asked for the person, by sending the record's own read
// through their grant: an answer is permitted, while a refusal and an absence
// are both "not permitted" and never told apart, because the read itself does
// not tell a person which of the two it was.
package cards
