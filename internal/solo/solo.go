// Package solo marks a test package that must have the test runner to itself.
//
// Importing this package from a test file IS the declaration — there is no
// list to keep in step, no build tag, and no environment variable. The
// partition both `make test` and ci.yml run is COMPUTED from that import
// (`go run ./internal/solo/partition`), and [roster_test.go] fails the build
// when a package that stands up a multi-member broker has not declared it.
//
// # What a solo package costs when it is not solo
//
// A package here stands up N engines, each embedding its OWN NATS server, in
// ONE process, and every replicated object it creates — a stream, a KV bucket
// — is a raft round trip a quorum of those servers has to answer. Starved of
// CPU, that round trip does not merely slow down: it has gone UNANSWERED for
// the whole of the deadline it was given, at thirty seconds and at forty-five
// (below). `go test ./...` runs package binaries in parallel at
// -p=GOMAXPROCS, and the rest of this suite runs a four-vCPU runner flat out
// under -race while it runs: 90-99% busy for fifteen minutes at bee5152, and
// still CPU-bound in the shorter run it takes now.
//
// Two measurements stand behind that, and only the first is of the case this
// package exists for:
//
//   - SHARING THE RUNNER, when a clustered create had thirty seconds: the
//     dedicated job passed in 5m24s, while the same cases inside `go test
//     ./...` failed on all three of their cluster-start attempts with
//     `context deadline exceeded` creating streams and KV buckets.
//   - ALONE, at bee5152 (2026-10-08), after [internal/jsprovision] had raised
//     a clustered create's budget to two minutes and the harness had come to
//     hold each bring-up attempt to 45 seconds ([jetstreamtest]
//     ClusterStartTerm) before retrying on fresh ports: internal/e2e at -p 1
//     on an otherwise idle four-vCPU machine (2-3% busy between its cases).
//     One fleet case lost a whole attempt — member 0's create of
//     CREWLET_TRACKER_LOG went unanswered for all 45 seconds with the machine
//     85% busy and the test binary alone holding three of its four cores, and
//     the attempt cost 73s with its teardown. That load was a FAULT IN THE HARNESS rather
//     than the cost of a fleet: it ticked every member's API every 25 ms, two
//     hundred times production, and each tick ran two certified listings
//     through the metadata raft group. So it says nothing about what three
//     members cost, nor about what the rest of the suite does to them. What it
//     does show is the failure at today's budgets — starved of CPU, whatever
//     starved it, the create was not slow but unanswered.
//
// The shared case HAS NOT BEEN RE-MEASURED under today's budgets. Whether the
// rest of the suite's load still starves a quorum past two minutes a create
// and 45 seconds an attempt is an open question, and the partition stands on
// the thirty-second measurement and on the mechanism above rather than on an
// answer to it. What would settle it is a CI run with these packages moved
// into the parallel half; nothing short of the runner the suite runs on
// measures that load.
//
// The broker harness records the same thing from the other end — see
// [jetstreamtest] `ClusterStartAttempts`: "this harness passes in six seconds
// on a loaded machine in isolation and timed out at a hundred and twenty
// inside a full run." That retry is what kept internal/node and
// internal/statelog green in the contended job before this package existed,
// and a retry that usually wins is not the same thing as a partition.
//
// A longer deadline is NOT the fix, on either side of the harness. The
// production budget is an operator's boot diagnostic, raised for an
// operator's reason — a busy host's slow create should not fail a boot — and
// its job is still that a genuinely wedged cluster is reported rather than
// waited on; stretching it until CI's CPU starvation fits inside it moves the
// cost onto the person it was written for. And the harness's term exists to
// ABANDON a stuck attempt for a fresh one: lengthening it to outlast a
// starved machine trades the retries for one long wait on the attempt that is
// starving.
//
// So the solo half runs at -p 1, one package binary at a time, and in CI on a
// runner of its own, beside nothing else of the suite.
//
// # Why a marker rather than one of the obvious mechanisms
//
// Every alternative was measured rather than reasoned about, and each fails
// the same way — silently:
//
//   - A BUILD TAG (`//go:build e2e`) hides the files from the compiler.
//     `go test ./...` reports `? pkg [no test files]` and exits 0; `go vet
//     ./...` stops reporting real errors in them; and golangci-lint, with this
//     repository's own config and no --build-tags, reports "0 issues" over a
//     file containing an undefined symbol. Tag EVERY file in the package and
//     `go list ./...` drops it altogether, so it lands in no job at all. There
//     is precedent in the tree, at .github/workflows/ci.yml: a `-tags=integration`
//     job and a CREWLET_INTEGRATION variable both shipped here once and BOTH
//     rotted unread.
//   - `-short` skips nothing on its own; it sets a bit each case must check,
//     which is a skip reported as ok. It would also collaterally drop
//     internal/store's two drainbench cases, the tree's only testing.Short()
//     readers.
//   - A CUSTOM FLAG does not survive `go test ./...`: every test binary that
//     does not define it dies with "flag provided but not defined".
//   - `-skip` leaves `ok pkg [no tests to run]` and exit 0, and Go's RE2 has no
//     negative lookahead, so "everything except" cannot be written as a -run
//     pattern at all. It also keys the partition on test FUNCTION names, which
//     rename silently.
//   - A NESTED MODULE excludes the package from the root `./...` cleanly, but
//     `go mod tidy -diff` is module-scoped while vet and lint are root-scoped,
//     so the number of commands ci.yml and the Makefile must keep in step goes
//     UP — and its `replace` would resolve nats-server and turso independently
//     of the release build.
//
// What every one of them has in common is that forgetting a package is
// invisible. A marker plus the roster guard is the only shape where a new
// cluster-forming package FAILS until it declares itself, and where declaring
// it is one line in that package's own diff rather than an edit to four files
// somewhere else.
package solo

import "testing"

// Run runs the suite. Call it from TestMain in place of m.Run():
//
//	func TestMain(m *testing.M) { os.Exit(solo.Run(m)) }
//
// It adds nothing to the run. The import is the point — this exists so the
// declaration is a function call a reader can follow to the doc above, rather
// than a blank import somebody deletes while tidying.
func Run(m *testing.M) int {
	return m.Run()
}
