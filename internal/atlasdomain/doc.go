// Package atlasdomain is the state log's record of what people tell the engine
// about what the company runs on (ADR-0126): the claims the atlas cannot compute
// from what the engine already keeps, on the `CREWLET_ATLAS_LOG` stream and
// applied into every data node's replicated estate.
//
// # Who has to agree on it
//
// Every data node, identically: the atlas is read from whichever node a person
// reaches, several people, the agents and the integrations' reads change it, and its history must outlive any one node's
// event store. That is the replicated estate, which only this domain's applier
// writes. What the engine already keeps (the company settings, Gitlet, the
// integrations' status, the access log) is never copied here: [atlas.Fold]
// computes it beside these claims when the atlas is read.
//
// # One subject an entry
//
// An entry ([atlas.Ref]) is one subject, named by its id. Every claim a person
// makes about it is published there, arbitrated on the entry's own version:
// an edit decided from a version the entry has moved past is [ErrStaleVersion],
// and two entries never serialize behind each other.
//
//   - `claim` records one [atlas.Claim]: the entry itself (added, retired,
//     restored), one fact (set, corrected, denied), one relation (drawn,
//     removed), an owner, or two entries a person joined ("the same") or kept
//     apart ("not the same", which always wins). The source is the caller and
//     its value was judged by the vocabulary's grammar, credentials refused,
//     before it was published. A claim is kept, never updated, but for being
//     REPLACED: a voice's newer claim on the same slot ([atlas.Claim.Slot])
//     marks its older one, which stays as the entry's history.
//     An AGENT's claim is the same record at version 3 (ADR-0132), written by
//     [Writer.Observe]: its source is the seat, with the turn it recorded in
//     and the evidence the engine checked behind it, so its confidence is
//     read off the record and chosen by nobody. It replaces only the seat's
//     own earlier claim on the slot, is conditioned on no version, and an
//     agent's denial is a dispute the fold never lets win.
//   - `read` records one integration's read of the entry (ADR-0129): the
//     claims the read holds on it that are new or changed, in the
//     integration's voice, and the slots of its earlier claims it no longer
//     holds, which are withdrawn into the entry's history. A read that stopped
//     seeing the entry retires it with one denied exists claim. A read that
//     holds what the log already holds is never recorded, so an unchanged
//     hourly read of thousands of names writes nothing.
//   - `purge` erases what it names everywhere, history included, and leaves a
//     permanent MARKER: an entry (every claim on it, every claim naming it),
//     or a value by its digest (every claim carrying it, every entry it is the
//     key of). Every later record held to a marker lands as nothing on every
//     node, so nothing a person, an agent or a read reports brings it back. A
//     purge never writes the value down: a value is named by its [Digest].
//     Whether a caller may purge is the caller's question; an agent never may.
//   - `expire` removes one hour of history, and everything older, past the
//     retention its writer names: every claim its voice replaced, and every
//     entry a person retired that nothing was recorded about since, with every
//     claim on it. A live claim never expires: it is the atlas. The trash
//     reaper nominates the hours past the company's `atlas.retention_days`
//     ([Reader.ExpirableHours]) and publishes one record each.
//
// The eviction, the generation, the barrier and the trimmed record are the
// framework's.
//
// # The applier judges no text
//
// Every judgement of a value (its grammar, its shape, whether it is private,
// the slot it fills) is the writer's, before it publishes. The applier stores
// what the record says, vocabulary words as text, so two builds that know
// different words still write one row, and a reader that does not know a word
// yet skips the claim by name ([atlas.View.Skipped]).
//
// # A new record kind joins without reshaping
//
// A kind is a subject kind, an op and a payload: its row in [versionedFields]
// (the subject kind's value, or a source kind's, at the next [RecordVersion]),
// a case in [DecodeMutation] and the applier, a table in [ReproducibleTables],
// and a writer method. Builds below it retain its records.
//
// # Reads
//
// [Reader.Live] reads every live claim for the fold, at the caller's level,
// `session` by default ([DefaultRead]); [Reader.LiveOf] one voice's, which an
// integration's read is diffed against; [Reader.Entry] an entry's version for
// a person's next edit; [Reader.History] an entry's claims, replaced ones
// included; and [Reader.Purges] what was purged, by whom and when, never what.
package atlasdomain
