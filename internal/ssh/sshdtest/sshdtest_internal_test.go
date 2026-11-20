package sshdtest

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A BUILD THAT FAILS SAYS WHY: the error a failed image build answers carries
// what its failing step printed, as apt's own error would be, not only the
// step's exit status. The sentinel is assembled by the step's shell, so the
// step's command line, which a quiet build still repeats, does not hold it.
// Mutation: build with -q, or drop the output from the error, and this fails.
func TestABuildThatFailsSaysWhy(t *testing.T) {
	t.Parallel()
	Require(t)
	text := fmt.Sprintf("FROM %s\nRUN printf 'E: %%s-%%s\\n' mirror unreachable >&2; exit 100\n", Images[0])
	ctx, cancel := context.WithTimeout(t.Context(), buildTimeout)
	defer cancel()
	err := build(ctx, runtimeName(), "crewlet-sshdtest:fails", text)
	if err == nil {
		t.Fatal("a build whose step exits 100 answered no error")
	}
	if !strings.Contains(err.Error(), "E: mirror-unreachable") {
		t.Fatalf("the failed build's error leaves out what its step printed:\n%v", err)
	}
}
