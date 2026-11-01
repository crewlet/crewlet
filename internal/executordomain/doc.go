// Package executordomain is the state log's record of each seat's EXECUTOR
// SESSIONS (ADR-0072): one row per (seat handle, turn) whose executor captured
// its coding CLI's session, naming the sealed object in the fleet's object
// store that holds it (ADR-0026), on the `CREWLET_EXECUTOR_LOG` stream and
// applied into every data node's replicated estate.
//
// # Who has to agree on it
//
// A seat's next turn on a conversation may run on any node, in any box, and
// continue the session its previous turn left: so the name of that session is
// the WHOLE FLEET's, identically, which is the replicated estate, which only
// this domain's applier writes. The bytes are not here at all. A session is
// tens of mebibytes, so it lives in the object store as one sealed object per
// capture under a key the store mints, and a row names it; the collector keeps
// every object a row names and deletes the rest a day after their upload
// (ADR-0027). Not a volume per node (a box on another node, or on e2b, could
// not reach it, and the collector, the audit and the backup could not see
// it), not the memory changelog (a compacted subject per seat cannot carry a
// conversation's ordering rules), and not the coordination store (a value of
// mebibytes in a bucket every lease shares).
//
// # One row per turn, never per conversation
//
// A conversation's next turn continues its HEAD, the newest ended turn of it
// that left a session — but that is a READ over rows that each belong to one
// turn, never a conversation row a later turn's capture takes over: a row that
// took another turn's object would be an object carry, which ADR-0027 (as
// ADR-0036 amends it) allows only inside one artifact. So a turn's capture
// only ever writes its own row, and a conversation's earlier rows go when a
// later turn's `end` ABSORBS them.
//
// # Subjects, scopes and the four records
//
// Every record about a conversation is ARBITRATED ON THE CONVERSATION
// ([ConversationSubject]: the seat's handle and a digest of the conversation's
// key, or of the turn's own id for a turn with no conversation), and decided
// from one snapshot of it, so the rules over a whole conversation — its head,
// its absorption, its work-key dedupe — are decided in log order on one
// subject. Every record DECLARES the conversation as its scope under the
// seat's path, so a seat-wide record contains it and a node that defers one
// record defers every later record its rows could change ([statelog]'s
// deferral contract).
//
//   - `capture` names a new object for a live turn. It is refused at the
//     decide for an object minted longer than objstore.RecordWithin ago by the
//     deciding node's clock, for an ended or tombstoned turn, and for a
//     (launch, capture) ordinal pair not newer than the row's; the applier
//     repeats the deterministic guards. A seat keeps at most the BYTE BUDGET
//     the record carries (the writer reads it from the fleet's revision,
//     `turn_engine.conversation_session.max_bytes_per_seat`): past it the
//     applier releases the objects of the seat's oldest ENDED turns, never a
//     live one, keeping their endings, and a capture that cannot fit even
//     then is refused. Because a release may reach any conversation of the
//     seat, a capture's scope is the SEAT.
//   - `end` stamps the turn ended with the engine's account of how it ended
//     ([Ending]) and tombstones it. With no row (nothing was captured) it
//     writes an ended row naming no object, so the next turn is still told how
//     this one ended. With a row it ABSORBS every other ended row of the
//     conversation whose end is at or below the position the turn's first
//     launch read the conversation's head at, and DEDUPES on a non-empty work
//     key, merging what the older attempt delivered.
//   - `delete` removes one turn's row and tombstone past the company's
//     retention (the duty passes the cutoff, and the apply compares the row's
//     own instant against it), or every row of a seat the company removed.
//
// The eviction, the generation and the barrier are the framework's records.
// THE TOMBSTONE IS A GUARD, NOT AN APPLY GATE, and deliberately: every record
// it refuses is about the same turn, so within the conversation scope the
// tombstoning record declared, and a node that cannot decode that record
// defers every later one about the turn as well. Only a node's eviction stops
// an applier that cannot read it.
//
// # Sealed, bounded, and gone with the conversation
//
// A session's bytes are a coding CLI's transcript: whatever the run read and
// wrote. So the object is sealed under the fleet keyring with the streaming
// format internal/secrets keeps for values too large to hold
// ([secrets.StreamSealer]), its associated data binding it to
// `executor/<seat handle>/<turn id>/<object key>` ([AAD]), so a capture
// streams box → seal → put and a restore streams get → open → box, each
// holding one chunk. An object that is missing, corrupt or sealed under a key
// the keyring no longer holds is [ErrUnreadable], which a restorer reads as no
// session. Every bound is this package's ([SessionMaxBytes],
// [CompletedWorkMaxBytes] and the rest), and the largest record they admit is
// held under the domain's declaration ([MaxRecordBytes]).
//
// A row lives until the company's conversation retention passes its own last
// instant (`turn_engine.conversation_session.retention_days`, read off the
// fleet's revision by the engine's retention duty, which never ages out a
// turn whose run is still live) or until the mailbox retirement removes its
// seat. A person's removal does not reach these rows in this release, as it
// did not reach the conversation ledger they replace (ADR-0072).
package executordomain
