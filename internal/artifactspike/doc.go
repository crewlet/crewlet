// Package artifactspike holds the artifacts capacity spikes as reproducible Go
// benchmarks: the datastore's write rate in rows per second, a store's real
// on-disk footprint, streaming a large publish, the policy linter over a corpus
// of model-shaped pages, the edit hit rate, presence fan-out, and the write-ahead
// log's tail latency under a concurrent large publish into the default object
// store. The numbers they produce are what the limits and objectives are
// checked against.
//
// The package is test-only: this file is its documentation and the benchmarks
// are its tests.
package artifactspike
