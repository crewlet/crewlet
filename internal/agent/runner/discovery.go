package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/tools"
)

// The two discovery meta-tools.
//
// They exist because the executor is shown SERVER NAMES, not tool names: a
// real MCP server publishes dozens of tools, and handing a model all of them
// buries the ones that matter under a wall of schemas nobody reads.
// Discovery being a tool call is also what keeps the prompt prefix stable
// while a server's catalogue changes underneath, which is what makes provider
// prompt caching worth anything.
//
// They are per-PHASE, like the submission tools, because activation mutates
// one surface. Registering them into the shared registry would let one phase's
// activation leak into the next — or into the next turn.
const (
	ListMCPToolsTool = "list_mcp_server_tools"
	ActivateTool     = "activate_tool"
)

// DiscoveryTools returns the pair bound to one surface, reached through a
// getter.
//
// A getter and not a pointer, because of a genuine cycle: activate must mutate
// the SAME Surface the loop reads its tool definitions from, so the tools
// cannot exist before the surface — and the surface cannot resolve them until
// they are in its snapshot. The getter is read at CALL time, by which point
// the surface exists.
//
// The alternative was building a placeholder Surface and assigning over it,
// which copies a struct holding a mutex. It is harmless while nothing holds
// that lock, and it is exactly the kind of harmless that stops being harmless.
//
// EXPORTED for [subagent.Config.Discovery], whose type it matches exactly: a
// sub-agent's own prompt tells it to call list_mcp_server_tools and
// activate_tool, so a nil Discovery makes that prompt a lie and costs a wasted
// round every time the child obeys it.
func DiscoveryTools(surface func() *tools.Surface) []tools.Callable {
	return []tools.Callable{
		&listMCPTools{surface: surface},
		&activateTool{surface: surface},
	}
}

type listMCPTools struct{ surface func() *tools.Surface }

func (t *listMCPTools) Name() string { return ListMCPToolsTool }

func (t *listMCPTools) Description() string {
	return "List the tools one MCP server offers. Use this when you need a tool " +
		"from a server and do not yet know its exact name — your system prompt " +
		"lists server names, not their individual tools. After picking one, call " +
		"`" + ActivateTool + "` so its schema arrives on the next message."
}

func (t *listMCPTools) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": map[string]any{
				"type": "string",
				"description": "The MCP server's name, exactly as your system " +
					"prompt spells it.",
			},
		},
		"required": []any{"server"},
	}
}

func (t *listMCPTools) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	server, _ := args["server"].(string)
	if strings.TrimSpace(server) == "" {
		return tools.Result{Output: "Name a server. " + t.knownServers(), Failed: true}, nil
	}
	var lines []string
	for _, e := range t.surface().Universe().Entries() {
		name, ok := e.FromMCP()
		if !ok || name != server {
			continue
		}
		if desc := firstLine(e.Tool.Description()); desc != "" {
			lines = append(lines, "- "+e.Name()+": "+desc)
			continue
		}
		lines = append(lines, "- "+e.Name())
	}
	if len(lines) == 0 {
		// A named server with no tools and an unknown server read very
		// differently to a model: one means "ask again later", the other
		// means "you have the name wrong". Listing what exists is what
		// turns the second into a recoverable round.
		return tools.Result{
			Output: fmt.Sprintf("No tools found on MCP server %q. %s", server, t.knownServers()),
			Failed: true,
		}, nil
	}
	return tools.Result{Output: strings.Join(lines, "\n")}, nil
}

func (t *listMCPTools) knownServers() string {
	var servers []string
	for _, e := range t.surface().Universe().Entries() {
		if name, ok := e.FromMCP(); ok && !slices.Contains(servers, name) {
			servers = append(servers, name)
		}
	}
	if len(servers) == 0 {
		return "This role has no MCP servers."
	}
	slices.Sort(servers)
	return "Servers on this role: " + strings.Join(servers, ", ") + "."
}

type activateTool struct{ surface func() *tools.Surface }

func (t *activateTool) Name() string { return ActivateTool }

func (t *activateTool) Description() string {
	return "Activate a tool from your role's catalogue so you can call it " +
		"directly from the next round on. Its schema appears in your tools then; " +
		"there is no need to activate twice.\n\n" +
		"Use it for first-party tools and for MCP tools whose names you got from " +
		"`" + ListMCPToolsTool + "`. Activate a tool the moment you need it — " +
		"there is no separate planning step to name it in, and nothing is " +
		"cheaper about activating it earlier."
}

func (t *activateTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "The tool's exact name."},
		},
		"required": []any{"name"},
	}
}

func (t *activateTool) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	name, _ := args["name"].(string)
	if strings.TrimSpace(name) == "" {
		return tools.Result{Output: "Name a tool to activate.", Failed: true}, nil
	}
	if t.surface().Activate(name) {
		return tools.Result{Output: name + " is active; its schema arrives on the next message."}, nil
	}
	// A miss is almost always a guessed MCP tool name. Saying so, and
	// naming the way to find the real one, is the difference between a
	// recoverable round and a phase that keeps guessing.
	return tools.Result{
		Output: fmt.Sprintf("No tool named %q. If it belongs to an MCP server, call "+
			"%s to see that server's real tool names.", name, ListMCPToolsTool),
		Failed: true,
	}, nil
}

// firstLine trims a description to its first line, so a paragraph does not
// break the one-tool-per-line shape a model reads a listing as.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
