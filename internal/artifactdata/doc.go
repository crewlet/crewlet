// Package artifactdata is the DATASTORE: the shared, durable collections of
// documents an artifact keeps, as a strict state-log domain of its own. Every
// write is one signed record; every data node applies it into its replicated
// estate; every viewer of a sheet sees the same rows.
//
// A DATASTORE WRITE IS DECIDED WHERE IT IS APPLIED. A sheet's writes are mostly
// blind (tick this box, set this chip) and a batch touches many documents, which
// a broker expectation on one subject cannot express, so writes are SEQUENCED:
// one subject per store, no expectation, the log orders them, and the applier
// decides each one as a pure function of the record and the rows it holds:
// grammar, schema, field rules, guards, caps and the stamped limits. A batch is
// all or nothing, and the verdict is written for every record, so every node
// reaches the same verdict and the writer reads its own back.
//
// AUTHORITY IS STAMPED. Whatever needs IAM or the org chart (the caller's role
// on the artifact, whether the caller is an agent) is decided at the API and
// carried in the record; the applier never reads another domain, whose log
// applies at its own pace on each node.
//
// THE HOT STORE KEEPS WHAT TIERING NEEDS. Surrogate integer keys keep the field
// index compact, every store carries a running state digest over its live
// documents (and every history row the digest after it), and the footprint is
// metered in real bytes, so moving idle stores to object storage later is not a
// re-derive of every store.
//
// A datastore write is decided where it is applied, from authority stamped at
// the API: ADR-0037. A cold store will live in object storage and return
// through the log, and the hot store keeps what that needs from its first
// release: ADR-0039. This package is the authority for both.
package artifactdata
