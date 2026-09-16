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
// ONE process. `go test ./...` runs package binaries in parallel at
// -p=GOMAXPROCS, and a two-core runner under -race cannot form a multi-member
// JetStream quorum inside the per-create budget [internal/jsprovision] gives
// that. Measured on one commit: the dedicated job passed in 5m24s, while the
// same cases inside `go test ./...` failed on all three of their cluster-start
// attempts with `context deadline exceeded` creating streams and KV buckets.
//
// The broker harness records the same thing from the other end — see
// [jetstreamtest] `clusterStartAttempts`: "this harness passes in six seconds
// on a loaded machine in isolation and timed out at a hundred and twenty
// inside a full run." That retry is what kept internal/node and
// internal/statelog green in the contended job before this package existed,
// and a retry that usually wins is not the same thing as a partition.
//
// Raising the provisioning budget is NOT the fix and was considered: that 30s
// is an operator's boot diagnostic, whose job is that a genuinely wedged
// cluster fails rather than hanging a boot. Widening a production timeout to
// survive CI CPU starvation moves the cost onto the one person it was written
// for.
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
