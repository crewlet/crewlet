package mcp

import (
	"strings"
	"testing"
)

func TestOriginGrammar(t *testing.T) {
	t.Parallel()
	if OriginBuiltin != "builtin" {
		t.Fatal("the bare origin is a contract with the operator surface, which groups on it")
	}
	if got := Origin("github"); got != "mcp:github" {
		t.Fatalf("Origin = %q", got)
	}
	// A tool an MCP server serves is structurally identical to a builtin.
	// The origin is the only thing that tells them apart, so no server name
	// may produce the builtin string.
	for _, name := range []string{"builtin", "", "mcp"} {
		if Origin(name) == OriginBuiltin {
			t.Fatalf("server %q collides with the builtin origin", name)
		}
	}
}

// TWO REGISTRANTS, not four. "custom" and "extension:<name>" came from a
// Python engine that could be embedded and could load plugins; this one is a
// static binary whose only extension point is MCP, out of process. Keeping
// them would have left the dashboard two groups nothing could ever fill.
func TestTheGrammarNamesOnlyWhatCanRegister(t *testing.T) {
	t.Parallel()
	for _, gone := range []string{"custom", "extension:"} {
		if OriginBuiltin == gone || OriginMCPPrefix == gone {
			t.Fatalf("%q is back in the grammar with nothing able to produce it", gone)
		}
	}
}

func TestMCPOriginNamesTheBareServerNotTheInstance(t *testing.T) {
	t.Parallel()
	// Two seats' children of one template are the same INTEGRATION to a
	// reader grouping the catalogue. Keying the origin on the instance would
	// split that view per seat.
	eng := Origin(ServerName(InstanceName("github", "Engineer")))
	pm := Origin(ServerName(InstanceName("github", "Product Manager")))
	if eng != pm {
		t.Fatalf("per-role children reported different origins: %q vs %q", eng, pm)
	}
	if strings.Contains(eng, "Engineer") {
		t.Fatalf("origin %q leaked the per-role instance name", eng)
	}
}

func TestBridgeHandsOutToolsAlreadyStamped(t *testing.T) {
	t.Parallel()
	// The origin is recorded at REGISTRATION because it cannot be recovered
	// afterwards. So the bridge does not hand out a bare tool for a registry
	// to file: it hands out the pair, and a registry that takes a
	// Registration cannot lose the answer.
	b := NewBridge(discardLogger())
	change, err := b.Add(t.Context(), helperSpec(t, InstanceName("github", "Engineer"), "serve", nil))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _, _ = b.StopAll(t.Context()) })

	regs := change.Registrations()
	if len(regs) == 0 {
		t.Fatal("no registrations came back from a successful Add")
	}
	for _, r := range regs {
		if r.Origin != "mcp:github" {
			t.Fatalf("registration origin = %q, want mcp:github", r.Origin)
		}
		if r.Tool == nil || r.Tool.Name() == "" {
			t.Fatal("a registration arrived with no tool")
		}
	}
}
