package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// The two discovery meta-tools, for a phase shown SERVER NAMES rather than
// tool names: a real MCP server publishes dozens of tools, and a tool call
// keeps the prompt prefix stable (and cacheable) while a catalogue changes.
//
// They are per-PHASE, like the submission tools, because activation mutates
// one surface: in the shared registry one phase's activation would leak into
// the next phase or turn.
//
// Their names are internal/toolname's: the skill guard, the worker deny list,
// the prompts and the delivery remedy all spell them, and all sit below this
// package.

// DiscoveryTools returns the pair bound to one surface through a getter read
// at call time: activate must mutate the very Surface the loop reads its
// definitions from, which cannot resolve the pair before it is in its snapshot
// (a placeholder Surface assigned over later copies a struct holding a mutex).
// Exported for [subagent.Config.Discovery]: a sub-agent's prompt names both
// tools, so a nil Discovery wastes a round each time the child obeys it.
//
// The entries carry their own hints because the pair bypasses the registry,
// and api/mcpbridge fences a stopped run's calls not proven read-only. Both
// are read-only (activating changes only which of the seat's granted tools
// this phase is offered, each still called under its own hints), idempotent
// ([tools.Surface.Activate]) and closed-world.
func DiscoveryTools(surface func() *tools.Surface) []tools.Entry {
	return []tools.Entry{
		{Tool: &listMCPTools{surface: surface}, Origin: tools.OriginBuiltin, Annotations: discoveryHints},
		{Tool: &activateTool{surface: surface}, Origin: tools.OriginBuiltin, Annotations: discoveryHints},
	}
}

// discoveryHints are the pair's behavioural hints; see [DiscoveryTools].
var discoveryHints = tools.Annotations{
	ReadOnly: mcp.Yes, Destructive: mcp.No, Idempotent: mcp.Yes, OpenWorld: mcp.No,
}

type listMCPTools struct{ surface func() *tools.Surface }

func (t *listMCPTools) Name() string { return toolname.ListMCPServerTools }

// Description promises "callable from your next round", which holds on every
// harness: one that re-lists a server's tools mid-run offers an activated tool
// at its model's next request, and one that does not is ended after the
// activation and starts its next run with it.
func (t *listMCPTools) Description() string {
	return "List the tools one MCP server offers. Your system prompt names " +
		"your MCP servers, not their tools; use this when you need one and do " +
		"not know its exact name, then `" + toolname.Activate + "` the one you pick."
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
		return tools.Result{
			Output: "Name a server. " + t.knownServers(), Failed: true,
			Cause: tools.RefusalInvalid,
		}, nil
	}
	var lines []string
	for _, e := range t.surface().Universe().Entries() {
		name, ok := e.FromMCP()
		if !ok || name != server {
			continue
		}
		lines = append(lines, tools.CatalogueLine(e.Name(), e.Tool.Description()))
	}
	if len(lines) == 0 {
		// An empty server means "ask again later", an unknown one "you have
		// the name wrong"; listing the known servers makes the second
		// recoverable.
		return tools.Result{
			Output: fmt.Sprintf("No tools found on MCP server %q. %s", server, t.knownServers()),
			Failed: true,
			Cause:  tools.RefusalNotFound,
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

func (t *activateTool) Name() string { return toolname.Activate }

func (t *activateTool) Description() string {
	return "Make a tool you can reach but do not hold callable, from your next " +
		"round on: a tool your system prompt lists to discover, or an MCP tool " +
		"whose name you got from `" + toolname.ListMCPServerTools + "`. Activate it when " +
		"you need it; once is enough."
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

// activationTarget is the tool an activate_tool call names.
func activationTarget(args map[string]any) string {
	name, _ := args["name"].(string)
	return name
}

func (t *activateTool) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	name := activationTarget(args)
	if strings.TrimSpace(name) == "" {
		return tools.Result{
			Output: "Name a tool to activate.", Failed: true,
			Cause: tools.RefusalInvalid,
		}, nil
	}
	if t.surface().Activate(name) {
		return tools.Result{Output: name + " is active: you can call it from your next round."}, nil
	}
	// A miss is almost always a guessed MCP tool name: naming the way to the
	// real one keeps the phase from guessing again.
	return tools.Result{
		Output: fmt.Sprintf("No tool named %q. If it belongs to an MCP server, call "+
			"%s to see that server's real tool names.", name, toolname.ListMCPServerTools),
		Failed: true,
		Cause:  tools.RefusalNotFound,
	}, nil
}
