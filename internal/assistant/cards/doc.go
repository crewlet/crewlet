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
// A stored card is shown again only while the person holds every app it was
// built from (Visible), since a transcript outlives the grants it was made
// under.
//
// # A link is the engine's and is decided for the person
//
// An open in app card's address comes from this package's link table, held
// against the dashboard's own route table, never from a URL a model wrote, and
// so do its words: the screen's own, or a record's name as its read answered,
// so a card never names a place its link does not open. Its
// `permitted` is asked for the person, by sending the record's own read
// through their grant: an answer is permitted, while a refusal and an absence
// are both "not permitted" and never told apart, because the read itself does
// not tell a person which of the two it was. The link table is replayed
// through the dashboard's router from a golden file, so a renamed screen fails
// a test rather than a person's click.
//
// # An unwritten descriptor is inferred by one rule
//
// An answer no descriptor was written for goes through Infer: a list, or a
// record wrapping one list beside plain values (a page of a collection), is a
// table; any other record is ONE THING, an entity of its facts and every list
// of records it holds, because a table of its largest list misdrew it. Such an
// entity names no dashboard kind, unless the route serving it says where the
// record's identity is (a known record): then it is that record, its kind,
// id, title and page read from the answer. Bookkeeping (an epoch, a digest, a cursor,
// the state log's envelope on every read it serves) stays for the model and
// the downloads and is left out of the drawer.
//
// # A record is never named by a value nobody reads
//
// A link or entity is named by what it opens or by its spec's name, never by
// a UUID: a noun where the address takes an id, the key where it takes one a
// person types. A direct conversation is named by its people, and a room is
// drawn after the "#" every screen draws, so two that read alike are still
// two. A thread is headed by its first words, cut at a heading's length.
//
// # A refusal a person reads is the engine's sentence
//
// At every exit to a person (a step's line, a read's error card, a failed
// proposal's strip) PersonRefusal words the refusal: the envelope's sentence
// where the authority table refused, the route's own words only where it
// marked them as meant for a person, else a sentence chosen by code. An
// unmarked detail names tools, fields and config keys and goes to the model
// alone. A 409 reads as a collision only when it is one.
package cards
