package mcp

import (
	"strings"
	"testing"
)

func catalogue() []Entry {
	return []Entry{
		// A builtin: no server, and the discovery tools must skip it rather
		// than inventing a server bucket called "".
		{Name: "send_message", Description: "Reply to the trigger"},
		{Name: "create_pr", Description: "Open a pull request\nsecond line", Server: "github"},
		{Name: "get_file_contents", Description: "Read a file", Server: "github"},
		{Name: "search_issues", Description: "  \n  Search Jira", Server: "atlassian"},
	}
}

func run(t *testing.T, tool *MetaTool, args map[string]any) Result {
	t.Helper()
	res, err := tool.Call(t.Context(), args)
	if err != nil {
		t.Fatalf("%s returned a Go error: %v", tool.Name(), err)
	}
	return res
}

func TestListServerToolsListsOneServer(t *testing.T) {
	t.Parallel()
	tool := ListServerTools(catalogue(), nil)
	res := run(t, tool, map[string]any{"server": "github"})
	if res.Failed {
		t.Fatalf("failed: %s", res.Output)
	}
	if !strings.Contains(res.Output, "(2 total)") {
		t.Fatalf("output does not count the tools:\n%s", res.Output)
	}
	// Sorted — the listing goes into a model's context every time it is
	// called, and a reshuffling one is a diff the reader has to discount
	// every turn — and each description carried WHOLE, indented under its
	// bullet. Keeping only the first line dropped exactly the argument
	// rules and preconditions a vendor writes below its opening sentence,
	// and this listing is the only place they are ever shown.
	want := "- create_pr: Open a pull request\n  second line\n- get_file_contents: Read a file"
	if !strings.Contains(res.Output, want) {
		t.Fatalf("listing body wrong:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "send_message") {
		t.Fatal("a builtin leaked into an MCP server's listing")
	}
	// Leading blank lines must not produce an empty description.
	res = run(t, tool, map[string]any{"server": "atlassian"})
	if !strings.Contains(res.Output, "- search_issues: Search Jira") {
		t.Fatalf("blank-led description not trimmed:\n%s", res.Output)
	}
}

func TestListServerToolsRejections(t *testing.T) {
	t.Parallel()
	tool := ListServerTools(catalogue(), nil)
	cases := []struct {
		name  string
		args  map[string]any
		match string
	}{
		{"missing argument", map[string]any{}, "non-empty string"},
		{"empty argument", map[string]any{"server": ""}, "non-empty string"},
		{"wrong type", map[string]any{"server": 42}, "non-empty string"},
		{"unknown server", map[string]any{"server": "linear"}, "not configured for this role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := run(t, tool, tc.args)
			if !res.Failed {
				t.Fatalf("accepted %v", tc.args)
			}
			if !strings.Contains(res.Output, tc.match) {
				t.Fatalf("output %q does not say %q", res.Output, tc.match)
			}
		})
	}
	// The rejection has to name what IS available, or the model's only move
	// is to guess again.
	res := run(t, tool, map[string]any{"server": "linear"})
	if !strings.Contains(res.Output, "atlassian, github") {
		t.Fatalf("rejection does not list the available servers: %q", res.Output)
	}
}

// TestListServerToolsMustSeeTheMergedUniverse reproduces the shared-server
// incident.
//
// A `shared: true` server's tools live in the GLOBAL registry, not in the
// role's own MCP list. The prompt catalogue renders every server it can see
// and tells the agent to call this tool for the names — so handed only the
// role's tools, this denies a server the prompt just advertised. Nothing else
// reveals the tool names, so no tool on any shared server was reachable unless
// the model guessed one exactly.
func TestListServerToolsMustSeeTheMergedUniverse(t *testing.T) {
	t.Parallel()
	perRole := []Entry{{Name: "search_issues", Description: "Search", Server: "atlassian"}}
	shared := []Entry{{Name: "calculate", Description: "Do sums", Server: "calculator"}}

	roleOnly := ListServerTools(perRole, nil)
	res := run(t, roleOnly, map[string]any{"server": "calculator"})
	if !res.Failed {
		t.Fatal("the role-only universe answered for a server it cannot see")
	}
	if !strings.Contains(res.Output, "not configured for this role") {
		t.Fatalf("expected the role-only listing to deny the shared server: %q", res.Output)
	}

	merged := ListServerTools(append(append([]Entry{}, perRole...), shared...), nil)
	res = run(t, merged, map[string]any{"server": "calculator"})
	if res.Failed {
		t.Fatalf("the merged universe still denied a shared server: %q", res.Output)
	}
	if !strings.Contains(res.Output, "calculate") {
		t.Fatalf("listing did not name the tool: %q", res.Output)
	}
}

func TestListServerToolsGating(t *testing.T) {
	t.Parallel()
	// A nil gate is "no per-turn gating". A non-nil one — INCLUDING an empty
	// one — gates, and the two must not collapse: an empty gate means
	// "nothing is available this turn", which is a real state.
	all := ListServerTools(catalogue(), nil)
	if res := run(t, all, map[string]any{"server": "github"}); res.Failed {
		t.Fatalf("nil gate blocked everything: %q", res.Output)
	}

	none := ListServerTools(catalogue(), map[string]struct{}{})
	res := run(t, none, map[string]any{"server": "github"})
	if !res.Failed {
		t.Fatal("an empty gate let everything through: nil and empty collapsed")
	}
	// The two failures need different reactions: a gated server is fixable
	// mid-turn by activating a specific tool, an unknown one is not.
	if !strings.Contains(res.Output, "currently unavailable") {
		t.Fatalf("a fully-gated server was reported as unknown: %q", res.Output)
	}
	if !strings.Contains(res.Output, "(none)") {
		t.Fatalf("output should say no server has available tools: %q", res.Output)
	}

	partial := ListServerTools(catalogue(), map[string]struct{}{"get_file_contents": {}})
	res = run(t, partial, map[string]any{"server": "github"})
	if res.Failed {
		t.Fatalf("a partially-gated server was refused: %q", res.Output)
	}
	if strings.Contains(res.Output, "create_pr") {
		t.Fatal("discovery advertised a tool activate_tool would refuse")
	}
	if !strings.Contains(res.Output, "get_file_contents") {
		t.Fatalf("the available tool was not listed: %q", res.Output)
	}
}

type fakeSurface struct {
	active    map[string]bool
	activable map[string]bool
}

func (f *fakeSurface) Has(name string) bool { return f.active[name] }
func (f *fakeSurface) Activate(name string) bool {
	if !f.activable[name] {
		return false
	}
	f.active[name] = true
	return true
}

func TestActivateTool(t *testing.T) {
	t.Parallel()
	surface := &fakeSurface{
		active:    map[string]bool{"send_message": true},
		activable: map[string]bool{"create_pr": true},
	}
	var activated []string
	tool := ActivateTool(catalogue(), func() ActivationSurface { return surface },
		func(name string) { activated = append(activated, name) })

	// Already active: a SUCCESS, because the model's next move is to call it.
	res := run(t, tool, map[string]any{"name": "send_message"})
	if res.Failed || !strings.Contains(res.Output, "ALREADY active") {
		t.Fatalf("already-active = %+v", res)
	}

	// A real activation.
	res = run(t, tool, map[string]any{"name": "create_pr"})
	if res.Failed || !strings.Contains(res.Output, "now active") {
		t.Fatalf("activation = %+v", res)
	}
	if len(activated) != 1 || activated[0] != "create_pr" {
		t.Fatalf("onActivated saw %v: the engine's event and sub-agent allowlist hang off this", activated)
	}
	// And the second call reports it as already active, not as a fresh one.
	res = run(t, tool, map[string]any{"name": "create_pr"})
	if !strings.Contains(res.Output, "ALREADY active") {
		t.Fatalf("re-activation = %+v", res)
	}
	if len(activated) != 1 {
		t.Fatalf("a no-op re-activation fired the callback again: %v", activated)
	}
}

func TestActivateToolRejections(t *testing.T) {
	t.Parallel()
	surface := &fakeSurface{active: map[string]bool{}, activable: map[string]bool{}}
	tool := ActivateTool(catalogue(), func() ActivationSurface { return surface }, nil)

	cases := []struct{ name, arg, match string }{
		{"empty name", "", "non-empty string"},
		// REGISTERED but gated: worth reporting as a policy outcome. Told
		// apart from the next case because conflating them produced a loop
		// where the model re-activated a gated tool every round.
		{"gated", "create_pr", "availability gate"},
		{"unknown", "nonesuch", "not registered"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := run(t, tool, map[string]any{"name": tc.arg})
			if !res.Failed {
				t.Fatalf("accepted %q", tc.arg)
			}
			if !strings.Contains(res.Output, tc.match) {
				t.Fatalf("output %q does not say %q", res.Output, tc.match)
			}
		})
	}
	// A wrong-typed argument is the same rejection as a missing one.
	if res := run(t, tool, map[string]any{"name": 7}); !res.Failed {
		t.Fatal("accepted a non-string name")
	}
	// The unknown-tool rejection must point back at discovery, or the model
	// has nowhere to go.
	res := run(t, tool, map[string]any{"name": "nonesuch"})
	if !strings.Contains(res.Output, "list_mcp_server_tools") {
		t.Fatalf("the dead end does not point back at discovery: %q", res.Output)
	}
}

func TestActivateToolWithNoSurface(t *testing.T) {
	t.Parallel()
	// The supplier exists because the surface is built with its meta-tools
	// already in it, so there is a window where it does not exist yet. A tool
	// called in that window must refuse, not panic.
	tool := ActivateTool(catalogue(), func() ActivationSurface { return nil }, nil)
	res := run(t, tool, map[string]any{"name": "create_pr"})
	if !res.Failed || !strings.Contains(res.Output, "no tool surface") {
		t.Fatalf("res = %+v", res)
	}
}

func TestMetaToolsAreCallable(t *testing.T) {
	t.Parallel()
	// Both meta-tools and bridged tools go to the model through one surface,
	// so both satisfy the same minimal interface.
	var tools []Callable
	tools = append(tools, ListServerTools(catalogue(), nil))
	tools = append(tools, ActivateTool(catalogue(), func() ActivationSurface { return nil }, nil))
	for _, c := range tools {
		if c.Name() == "" || c.Description() == "" {
			t.Fatalf("%T is missing its schema", c)
		}
		params := c.Parameters()
		if params["type"] != "object" {
			t.Fatalf("%s parameters are not a JSON Schema object: %v", c.Name(), params)
		}
		if _, ok := params["required"]; !ok {
			t.Fatalf("%s does not mark its argument required", c.Name())
		}
	}
}

func TestEntriesOf(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	spec := helperSpec(t, InstanceName("github", "Engineer"), "serve", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"create_pr", "Open a pull request", ""}),
	})
	spec.ToolPrefix = "gh_"
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	entries := EntriesOf(change.Added)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	if entries[0].Name != "gh_create_pr" {
		t.Fatalf("entry name = %q: the catalogue name is what activate_tool takes", entries[0].Name)
	}
	// A per-role child must appear under the BARE server name, which is the
	// only name the model has ever been shown.
	if entries[0].Server != "github" {
		t.Fatalf("entry server = %q", entries[0].Server)
	}

	listing := ListServerTools(entries, nil)
	if res := run(t, listing, map[string]any{"server": "github"}); res.Failed {
		t.Fatalf("a per-role child was not discoverable under its bare name: %q", res.Output)
	}
}
