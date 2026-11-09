// Package accessdomain is the state log's record of each seat's ACCESS
// (ADR-0103): what every turn of a seat was told about its own access, and every
// SSH certificate the engine signed and SSH command it ran or a person denied,
// on the `CREWLET_ACCESS_LOG` stream and applied into every data node's
// replicated estate.
//
// # Who has to agree on it
//
// A seat's next turn may dispatch on any node, and is told what changed since
// the access it was last told, so that baseline is the whole fleet's,
// identically: the replicated estate, which only this domain's applier writes.
// The SSH audit is answered by whichever data node a person reaches, and a
// node's own database, the coordination store and the event store (30 days, per
// node) would each lose it.
//
// # Every record is created once
//
// A told record is keyed by its seat and turn, a certificate by its seat and
// serial, a command by its seat and its own id, each on a subject of its own
// and published first-writer-wins, so a retried write lands once and a second
// write for the same turn is [ErrRecorded], not an error of the caller's. No
// record is updated after it lands, but for two: a seat's newest told record
// is its BASELINE (`latest`), which the applier moves in log order, and a
// command's sealed text, which a rekey moves onto the active key (`resealed`).
//
//   - `told` records one turn's snapshot of the seat's access and the changes
//     since its baseline, or none on a seat's first (`baseline`). The snapshot
//     and the changes are the capability package's JSON, opaque here.
//   - `issued` records one SSH certificate's public facts: serial, key id,
//     purpose, hosts, principals, validity and fingerprints. Never a key.
//   - `ran` records one SSH command: its text SEALED by the writer under the
//     fleet keyring and bound to the record ([AAD], [Sealer]), the sizes and
//     SHA-256 digests of its stdin and output (never the bytes), its exit, a
//     closed failure word where it did not run or finish (never an error's
//     text), and who approved or denied it. Only an administrator's audit read
//     opens it.
//   - `resealed` writes one command's text sealed afresh under the keyring's
//     active key, on the command's own subject ([Writer.Reseal]).
//   - `forget` removes what a seat the company removed was told, and records the
//     removal: a told record decided before a removal it did not see lands as
//     nothing, so a turn racing its seat's removal is not told into a seat that
//     is gone, while the seat's next tenure is. Its certificates and commands
//     stay to their retention.
//   - `expire` removes one hour of records, and everything older, past the
//     company's `access_history.retention_days`: every told record but each
//     seat's baseline, every removal, certificate and command. The trash
//     reaper nominates the hours and publishes one record each.
//
// The eviction, the generation, the barrier and the trimmed record are the
// framework's.
//
// # A new record kind joins without reshaping
//
// A kind is a subject kind, an op and a payload: its row in [versionedFields]
// (the subject kind's value, at the next [RecordVersion]), a case in
// [DecodeMutation] and the applier, a table in [ReproducibleTables] and
// [RetainedTables], and a writer method. Builds below it retain its records.
//
// # Reads
//
// A turn's dispatch reads its seat's baseline and recent told records at
// `linearizable` ([DispatchRead], [Reader.Recent]): the seat's previous turn
// may have been told on another node, and a copy that has not applied it would
// tell the same change twice. The dashboard pages a seat's told records, and an
// administrator's audit pages certificates and commands, at the caller's level.
package accessdomain
