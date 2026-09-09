package builtin

import (
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
)

// THE DEFAULT ARM IS THE FAIL-CLOSED HALF, and it is invisible from outside:
// every builtin is named in the switch today, so the table-driven test over
// the registry never reaches it and could not notice it changing.
//
// It matters for the tool that does not exist yet. A future builtin that posts
// somewhere, added to the candidate list and forgotten in `annotationsFor`,
// must be denied to sub-agents until somebody classifies it — not admitted
// because the arm helpfully said "does not leave the process". That is the
// same asymmetry internal/mcp/probe.go protects for third-party servers, and
// it is the reason the three private-state builtins are NAMED rather than
// defaulted.
func TestAnUnclassifiedBuiltinIsTreatedAsAPublicWrite(t *testing.T) {
	t.Parallel()
	got := annotationsFor("a_builtin_nobody_has_written_yet")
	if !mcp.WritesToSharedSurface(got) {
		t.Errorf("an unnamed builtin classified as %+v, which a sub-agent may call — "+
			"the default arm must stay conservative", got)
	}
	if got.ReadOnly == mcp.Yes {
		t.Error("an unnamed builtin was assumed to be a read, which also exempts it " +
			"from the delivery gate")
	}
}
