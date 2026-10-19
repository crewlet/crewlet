package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/toolname"
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
//
// THEIR NAMES ARE internal/toolname's, not this package's: the skill guard,
// the worker deny list, the prompts and the delivery remedy all have to spell
// them, and every one of those sits below this package.

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
// sub-agent's own prompt tells it to call both tools by name, so a nil
// Discovery makes that prompt a lie and costs a wasted
// round every time the child obeys it.
func DiscoveryTools(surface func() *tools.Surface) []tools.Callable {
	return []tools.Callable{
		&listMCPTools{surface: surface},
		&activateTool{surface: surface},
	}
}

type listMCPTools struct{ surface func() *tools.Surface }

func (t *listMCPTools) Name() string { return toolname.ListMCPServerTools }

// Description says what the call answers and what follows it, in words every
// CLI reads truly: "callable from your next round" — a CLI that re-lists the
// server's tools mid-run offers an activated tool at its model's next request,
// and one that does not is ended after the activation and starts its next run
// with it — never "its schema arrives on the next message", which described a
// vendor's tool channel this engine no longer has.
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
		// A named server with no tools and an unknown server read very
		// differently to a model: one means "ask again later", the other
		// means "you have the name wrong". Listing what exists is what
		// turns the second into a recoverable round.
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

// activationTarget is the tool an activate_tool call names. ONE READING, for
// the tool itself and for a box-mode run taken over while it works, whose
// surface is rebuilt with every tool its successful activations named
// ([Runner.ReopenAgentRun]).
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
	// A miss is almost always a guessed MCP tool name. Saying so, and
	// naming the way to find the real one, is the difference between a
	// recoverable round and a phase that keeps guessing.
	return tools.Result{
		Output: fmt.Sprintf("No tool named %q. If it belongs to an MCP server, call "+
			"%s to see that server's real tool names.", name, toolname.ListMCPServerTools),
		Failed: true,
		Cause:  tools.RefusalNotFound,
	}, nil
}
