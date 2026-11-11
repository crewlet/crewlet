// Package testbinary says whether this process is a test binary, for the few
// rules shipping code holds only a TEST to: the broker and Gitlet a test runs
// declare their sizes rather than take them from the machine running the suite
// (jetstream.ErrUndeclaredSize, engine.ErrUndeclaredCodeCache), and a node on
// an external broker reads its account's limit rather than stand the machine's
// free disk in for it (engine.ErrUndeclaredBrokerLimit).
//
// # Why not testing.Testing
//
// It is the documented answer, and it would link the testing package into the
// crewlet binary, which cmd/crewlet's TestTheBinaryLinksNoTestScaffolding
// refuses: shipping code and test scaffolding answer to different review bars.
// So Running reads the one fact a test binary carries and the crewlet binary
// never does — the testing package's own flags, which `go test`'s generated
// main registers on flag.CommandLine (testing.MainStart, through testing.Init)
// before any test, a TestMain included, runs. A process a test binary starts of
// itself is one too, the git hook Gitlet execs among them.
//
// # Read at every call
//
// Rather than once: a reading taken before the generated main registered the
// flags — a package's init, in a test binary — would be wrong for the rest of
// the process, and the read is a map lookup.
package testbinary

import "flag"

// Running reports whether this process is a test binary built by `go test`.
func Running() bool { return running(flag.CommandLine) }

// running reports whether set holds the testing package's flags.
func running(set *flag.FlagSet) bool { return set.Lookup("test.v") != nil }
