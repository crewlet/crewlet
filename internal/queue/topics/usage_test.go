package topics_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE USAGE WILDCARD COVERS THE GRAMMAR IT IS CREATED FOR.
//
// The stream is created over the wildcard and every record is published under
// the prefix, and nothing but this compares the two: a wildcard one token off
// is a stream that accepts none of its own domain's appends.
func TestTheUsageWildcardCoversItsOwnGrammar(t *testing.T) {
	t.Parallel()
	prefix, ok := strings.CutSuffix(topics.UsageLogWildcard, ">")
	if !ok {
		t.Fatalf("the wildcard %q is not a wildcard", topics.UsageLogWildcard)
	}
	if prefix != topics.UsageLogPrefix+"." {
		t.Fatalf("the wildcard covers %q and subjects are built under %q",
			prefix, topics.UsageLogPrefix+".")
	}
}
