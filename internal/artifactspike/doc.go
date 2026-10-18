// Package artifactspike holds the artifacts capacity spikes as reproducible Go
// benchmarks: the datastore's write rate in rows per second and its behaviour
// under contention (DS-1, S2), a store's real on-disk footprint against what
// the applier meters (DS-2), a 64 MiB publish streamed through the transfer
// leg (S3), the publish-time linter and the strict script policy over a
// committed corpus of model-shaped pages (S4), edit application over a set of
// republishes (S5), presence fan-out on a three-member fleet (S6), and the
// write-ahead log's latency, the object bucket's ceiling and its memory cost
// on the default object store (S7). The numbers they produce are what the
// limits in internal/artifacts and the objectives in internal/objectives are
// checked against; docs/reference/artifact-capacity.md records each run, its
// method, and the procedures for the spikes that need an environment of their
// own (S1, the S3 arm against a real bucket, S5 over captured republishes, and
// member replacement at scale).
//
// # Real components, nothing modelled
//
// Every spike drives the shipped code: the embedded broker (one member, or a
// three-member cluster with every write fsynced), the state-log runner and
// publisher, the datastore's writer, applier and reader, the upload service
// over a real object store backend, the presence hub on the queue's ephemeral
// broadcast, the socket hub's room routing, the linter and the edit
// application. The one reading that is a model is S4's static reading of what
// the served policy stops in a published page, which names the rules it
// applies and is checked in a browser by the procedure the capacity page
// gives.
//
// # Why benchmarks, and why they gate nothing
//
// A benchmark is a measurement of one machine on one day, so no threshold here
// fails a build: a figure that would pass on a quiet server fails on a loaded
// laptop and says nothing about either. What the suite holds in every run are
// the corpus records (TestTheS4CorpusReadsAsRecorded,
// TestTheS5EditsApplyAsRecorded), so a linter or edit change that moves a page
// fails until the record and the capacity page move with it, and the
// invariants a spike relies on (a run whose writes were refused measured
// refusals, and fails; a blind write that conflicts breaks the datastore's
// ordering rule, that the log orders and the applier decides, and fails).
//
// The package is test-only: this file is its documentation and the benchmarks
// are its tests. It stands up multi-member brokers, so it runs alone
// (internal/solo).
package artifactspike
