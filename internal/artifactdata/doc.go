// Package artifactdata is the DATASTORE: the shared, durable collections of
// documents an artifact keeps, as a strict state-log domain of its own
// (CREWLET_ARTIFACT_DATA_LOG). Every write is one signed record; every data
// node applies it into its replicated estate; every viewer of a sheet sees the
// same rows. internal/artifacts is the vocabulary it is written in, and
// internal/artifactsdomain is the artifact beside it, on a log of its own.
//
// # A datastore write is decided where it is applied
//
// A sheet's writes are mostly blind (tick this box, set this chip) and a batch
// touches many documents, which a broker expectation on one subject cannot
// express, so writes are SEQUENCED (statelog.PatternSequenced): one subject per
// store, no expectation, the log orders them, and the applier decides each one
// as a pure function of the record and the rows it holds: grammar, schema,
// field rules, guards, caps and the stamped limits. A batch is all or nothing,
// and the verdict (applied, conflict, invalid, forbidden or quota) is written
// for every record, refused ones included, so every data node reaches the same
// verdict and the writer reads its own back by operation id. A schema change
// is arbitrated on the same subject instead, decided in a snapshot of the
// schema it replaces, so it is ordered against every write it judges.
//
// # Authority is stamped
//
// Whatever needs IAM or the org chart (the caller's role on the artifact,
// whether the caller is an agent, the plan quotas in force) is decided at the
// API and carried in the record; the applier never reads another domain, whose
// log applies at its own pace on each data node. A share revoked between the decide
// and the apply lets at most that one write through, the window every
// arbitrated write in the engine accepts.
//
// # The rules, in one vocabulary
//
// A collection's policy is a preset compiled into rule fields before it is
// stored (who reads, the least role that creates, the least role that deletes,
// and each column's write role and condition), so one evaluator reads one
// vocabulary. A writer below editor changes only rows they created, unless a
// column names a write role, whose condition then decides; `humans_only` refuses
// an agent whatever its role; `append` admits only growth; `immutable` admits
// only a row's creation; a `private` collection is each writer's alone. A
// long-text value is replaced only by a write naming the version it edited,
// or grown by an append, so a concurrent edit is a conflict the writer is told
// about rather than a paragraph silently lost. An agent tightens a schema and
// never loosens one. The read rules apply on every read path: the listing, a
// query, a document, the history and the export. The history judges each
// change by the creator recorded on it ([Change]), so a row that changes hands
// never shows its new creator what the previous one wrote.
//
// # Field-level last writer wins, by log position
//
// Merge granularity is the top-level field: a merge sets the fields it names,
// an increment adds exactly (on decimal text, never a float), an add or a
// remove moves one chip, and two writers' changes to different fields of one
// row both land in log order. A guard (`if_version`) makes a write conditional
// on the version a reader saw.
//
// # History, undo and the digest
//
// Every record writes a history row: who, when, the verdict, and each changed
// field's value before and after (bounded, a hash beyond), kept seven days and
// at most a quarter of the store's footprint bound (8 MiB by default), pruned
// in each record's own transaction from broker time. Undo publishes the
// inverse of one write, each document guarded on still being where that write
// left it, so an undo never silently reverts a teammate's later edit. Every
// store carries a STATE DIGEST, the sum modulo 2^256 of one SHA-256 per live
// document and per collection (internal/artifactdata/canon), and every history
// row carries the digest after it.
//
// # The hot store keeps what tiering needs
//
// Surrogate integer keys (the store's number is the packed position of its
// first record) keep the field index compact; the digest and the per-row
// digests let a later build's checkpoint be verified against the store; and
// the footprint is metered in the bytes the store's rows cost on disk (each
// document row with its index entries and field-index entries, kept on the
// row, plus hot history and its touches; meter.go), which is what
// `artifacts.limits.data_store_bytes` and `artifacts.limits.data_bytes` bound.
// Moving idle stores to object storage is a later build's; nothing here has to
// be re-derived for it.
//
// # Per-viewer state
//
// A page whose version declares the `state` capability keeps per-viewer state
// in the engine's `_state` collection: implicit in every store, schemaless and
// private, one document per viewer and key, so it follows the viewer across
// devices and survives an opaque origin on which browser storage throws.
//
// # What a batch moved
//
// After each committed batch the applier hands the engine what it moved (the
// documents it changed, with the facts a push is shaped by), built from the rows
// it wrote; the engine fans it out to the socket hub. No record wakes a seat in
// this release, so the domain declares no feed.
//
// # The decisions behind it
//
// A datastore write is decided where it is applied, from authority stamped at
// the API: ADR-0037. A cold store will live in object storage and return
// through the log, and the hot store keeps what that needs from its first
// release: ADR-0039. This package is the authority for both.
package artifactdata
