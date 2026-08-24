package mcp

import (
	"strings"
	"testing"
)

func TestOriginGrammar(t *testing.T) {
	t.Parallel()
	if OriginBuiltin != "builtin" || OriginCustom != "custom" {
		t.Fatal("the two bare origins are a contract with the operator surface, which groups on them")
	}
	if got := ExtensionOrigin("standup"); got != "extension:standup" {
		t.Fatalf("ExtensionOrigin = %q", got)
	}
	if got := MCPOrigin("github"); got != "mcp:github" {
		t.Fatalf("MCPOrigin = %q", got)
	}
	// A tool an extension registers is structurally identical to a builtin.
	// The origin is the only thing that tells them apart, so it must not
	// collide with the builtin string for any extension name.
	for _, name := range []string{"builtin", "custom", "mcp"} {
		if ExtensionOrigin(name) == OriginBuiltin || ExtensionOrigin(name) == OriginCustom {
			t.Fatalf("extension %q collides with a bare origin", name)
		}
	}
}

func TestMCPOriginNamesTheBareServerNotTheInstance(t *testing.T) {
	t.Parallel()
	// Two seats' children of one template are the same INTEGRATION to a
	// reader grouping the catalogue. Keying the origin on the instance would
	// split that view per seat.
	eng := MCPOrigin(ServerName(InstanceName("github", "Engineer")))
	pm := MCPOrigin(ServerName(InstanceName("github", "Product Manager")))
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
