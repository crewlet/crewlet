package builtin

import (
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
)

// THE DEFAULT ARM IS THE FAIL-CLOSED HALF, invisible from outside: every
// builtin is named in the switch, so the registry's table test never reaches
// it. A future builtin that posts somewhere and is forgotten in
// `annotationsFor` must be denied to sub-agents until classified — the
// asymmetry internal/mcp/probe.go protects for third-party servers, and why
// the private-state builtins are NAMED rather than defaulted.
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
