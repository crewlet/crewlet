package testbinary

import (
	"flag"
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
