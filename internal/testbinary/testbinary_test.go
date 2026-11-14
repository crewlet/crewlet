package testbinary

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A TEST BINARY SAYS IT IS ONE, and a command line holding none of the testing
// package's flags reads as none — which is what the crewlet binary's is, since
// it never links that package (cmd/crewlet's
// TestTheBinaryLinksNoTestScaffolding holds that).
//
// Mutation: answer yes without reading the flag set — the fresh command line's
// row is red.
func TestATestBinarySaysItIsOne(t *testing.T) {
	t.Parallel()
	if !Running() {
		t.Error("a test binary says it is none")
	}
	if running(flag.NewFlagSet("crewlet", flag.ContinueOnError)) {
		t.Error("a command line with none of the testing package's flags reads as a test binary's")
	}
}

// A TEST BROKER'S STORE IS ONE brokertest.TempDir MADE, OR UNDER ONE: a path is
// in a store directory when it, or any directory above it, is named with the
// prefix TempDir gives — read whole, so a name that only contains it, or
// stops short of it, is not, and read cleaned, so a `..` climbing out of one
// is out of it.
//
// Mutation: read only the path's own name (the rows under a store directory
// are out); find the prefix anywhere in a name (the row that only contains it
// is in).
func TestAStoreDirectoryIsOneTempDirMadeOrUnderOne(t *testing.T) {
	t.Parallel()
	store := "/tmp/" + StoreDirPrefix + "1234"
	for path, want := range map[string]bool{
		store:                                   true,
		store + "/stream":                       true,
		store + "/a/b/o.dat":                    true,
		store + "/../stream":                    false,
		"/tmp/1234/stream":                      false,
		"/tmp/x-" + StoreDirPrefix + "1/stream": false,
		"/tmp/" + strings.TrimSuffix(StoreDirPrefix, "-"): false,
	} {
		if got := InStoreDir(path); got != want {
			t.Errorf("InStoreDir(%q) = %t, want %t", path, got, want)
		}
	}
}

// A RELATIVE STORE IS READ WHERE THE BROKER READS IT, against the working
// directory: a store named relative to a working directory inside a store
// directory is in it. Not parallel, since it changes the process's working
// directory.
//
// Mutation: read the path as written, never against the working directory,
// and the relative store is out.
func TestARelativeStoreIsReadAgainstTheWorkingDirectory(t *testing.T) {
	store := filepath.Join(t.TempDir(), StoreDirPrefix+"1234")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(store)
	if !InStoreDir("stream") {
		t.Errorf("a store named relative to a working directory in %s is not in it", store)
	}
}
