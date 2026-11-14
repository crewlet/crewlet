// Package testbinary says whether this process is a test binary, for the few
// rules shipping code holds only a TEST to: the broker and Gitlet a test runs
// declare their sizes rather than take them from the machine running the suite
// (jetstream.ErrUndeclaredSize, engine.ErrUndeclaredCodeCache), a node on an
// external broker reads its account's limit rather than stand the machine's
// free disk in for it (engine.ErrUndeclaredBrokerLimit), and a test's broker
// keeps its store in a directory brokertest.TempDir made
// (jetstream.ErrStoreOutsideTempDir), which [InStoreDir] tells.
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
//
// # A test broker's store, by its name
//
// brokertest.TempDir names every directory it makes with [StoreDirPrefix],
// and jetstream refuses a test's broker whose store lies in no such directory.
// The prefix is here, beside Running, because both sides need it and neither
// can hold it for the other: jetstream is shipping code, which cannot import
// brokertest, since that imports the testing package; and brokertest cannot
// import jetstream, whose own tests import brokertest.
//
// A NAME RATHER THAN A RECORD of the directories made, because the broker is
// not always in the process that made its directory: a case can run its broker
// in a child test binary on a directory the parent made and handed it, and the
// name travels with the path where a record would stay behind.
package testbinary

import (
	"flag"
	"path/filepath"
	"strings"
)

// Running reports whether this process is a test binary built by `go test`.
func Running() bool { return running(flag.CommandLine) }

// running reports whether set holds the testing package's flags.
func running(set *flag.FlagSet) bool { return set.Lookup("test.v") != nil }

// StoreDirPrefix begins the name of every directory brokertest.TempDir makes
// for a test broker's store ([InStoreDir]).
const StoreDirPrefix = "crewlet-test-store-"

// InStoreDir reports whether path is, or lies under, a directory whose name
// begins with [StoreDirPrefix]: one brokertest.TempDir made. A relative path is
// read against the working directory, as nats-server reads a store directory,
// and a path is read cleaned, so a `..` that climbs out of such a directory is
// out of it.
func InStoreDir(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for dir := abs; ; {
		if strings.HasPrefix(filepath.Base(dir), StoreDirPrefix) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
