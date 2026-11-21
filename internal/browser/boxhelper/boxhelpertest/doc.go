// Package boxhelpertest hands a case of the box helper the Nodes it runs
// under, each with its own npm: the machine's node and npm ([Node], [NPM]), at
// or above boxhelper.MinNodeMajor and MinNPMMajor, and the floor's pinned
// build with the npm it bundles ([Floor], [FloorNPM]), of those majors
// exactly, so what a case asserts holds on the oldest Node and npm a box may
// carry as well as on current ones (boxhelper's package doc says why the floor
// is pinned).
//
// # Where each comes from
//
// The machine's node and npm are found on PATH, as internal/prereqtest finds a
// program the suite needs: under the make targets that run the suite a missing
// one fails the case, and only a bare `go test` outside make skips it. Every
// make target that runs the whole suite (`test`, `test-solo`, `test-norace`)
// fetches the floor's build (`make node`, the one recipe scripts/node.sh),
// which keeps the npm its release bundles beside its node, and names it in
// CREWLET_TEST_NODE_FLOOR, under the same rule. The floor's npm is its
// npm-cli.js, run with the floor's node, never through a bin/npm, whose `env
// node` would run whichever node is on PATH.
//
// Each is held by what it answers to --version: the machine's at or above the
// floor, the floor's of its major exactly, since a newer one in the floor's
// place is a floor nobody is testing. It is asked without NODE_OPTIONS, which
// a Node refuses to start under when it names an option that Node predates,
// and without npm's own settings from the environment, every variable whose
// name begins npm_config_ in any case, as npm reads them: npm 10 and 11 end,
// printing nothing, under a loglevel they do not know.
//
// Test support (the name ends in `test`), so internal/fixedwait reads it as
// test code: it holds no fixed wait, and every bound in it is a context's.
package boxhelpertest
