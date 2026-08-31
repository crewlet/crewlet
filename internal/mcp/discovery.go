package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// The discover-then-activate meta-tools.
//
// A phase never sees MCP tool schemas in its tools=[...] upfront. The
// catalogue in its system prompt lists BUILTINS by name and MCP SERVERS by
// name only — a hundred tool schemas would swamp the prompt and most of them
// would go unused. Two meta-tools take the model from a server name to a
// callable tool:
//
//	list_mcp_server_tools(server) -> the name: description listing for one
//	activate_tool(name)           -> promotes it into tools=[...] for the
//	                                 next round
//
// Both are built over the MERGED universe — see ListServerTools.

// ActivationSurface is the live per-phase tool surface a meta-tool mutates.
//
// Two methods because the two answers mean different things to the model:
// already-active is a success it should act on (call the tool), while a failed
// activation is a dead end it must route around.
type ActivationSurface interface {
	// Has reports whether the tool is already in tools=[...].
	Has(name string) bool
	// Activate promotes a tool into tools=[...] and reports whether it could.
	Activate(name string) bool
}

// MetaTool is one of the discovery tools: a schema plus the function that runs
// it. A struct with a function field rather than an interface, because there
// is exactly one shape of these and an interface would only be ceremony.
type MetaTool struct {
	name        string
	description string
	parameters  map[string]any
	call        func(ctx context.Context, args map[string]any) Result
}

var _ Callable = (*MetaTool)(nil)

// Name is the name the model calls this meta-tool by.
func (m *MetaTool) Name() string { return m.name }

// Description is what the model is told the meta-tool does.
func (m *MetaTool) Description() string { return m.description }

// Parameters is the meta-tool's JSON Schema.
func (m *MetaTool) Parameters() map[string]any { return m.parameters }

// Call runs the meta-tool. It never returns an error: these run entirely in
// process, over data already in hand, so every outcome is something to tell
// the model.
func (m *MetaTool) Call(ctx context.Context, args map[string]any) (Result, error) {
	return m.call(ctx, args), nil
}

// ListServerTools builds the list_mcp_server_tools meta-tool.
//
// # Give it the MERGED universe
//
// `merged` must be the same set of tools the prompt catalogue advertises — the
// global registry PLUS the role's own MCP tools, not the role's alone. The
// catalogue renders every server it can see and tells the agent to call this
// tool for the names, so a server present in one and missing from the other is
// a server the prompt advertises and this tool denies.
//
// That is exactly what happened to every `shared: true` MCP server: its tools
// live in the global registry, the catalogue listed it, and an agent following
// its own instructions got "not configured for this role. Available servers:
// (none)". Nothing else reveals the tool names — the slim catalogue
// deliberately withholds them — so no tool on any shared server was reachable
// unless the model guessed a name exactly.
//
// Entries with no Server are builtins and are skipped, which is why passing
// the whole merged catalogue is safe.
//
// # available
//
// A nil `available` means no per-turn gating. A non-nil one (INCLUDING an
// empty one, which means "nothing is available this turn") restricts what the
// listing shows, so discovery cannot advertise a tool that activate_tool would
// then refuse.
func ListServerTools(merged []Entry, available map[string]struct{}) *MetaTool {
	// Bucket the full surface BEFORE gating, so the two failures can be told
	// apart: "this role has no such server" and "the server is there but
	// everything on it is gated off this turn" need different reactions from
	// the model, and only the second is fixable mid-turn.
	all := bucketByServer(merged, nil)
	live := bucketByServer(merged, available)
	liveNames := sortedKeys(live)

	call := func(_ context.Context, args map[string]any) Result {
		server, _ := args["server"].(string)
		if server == "" {
			return Result{Failed: true, Output: "server must be a non-empty string"}
		}
		if _, known := all[server]; !known {
			return Result{Failed: true, Output: fmt.Sprintf(
				"MCP server '%s' is not configured for this role. Available servers: %s.",
				server, orNone(liveNames))}
		}
		tools, anyLive := live[server]
		if !anyLive {
			return Result{Failed: true, Output: fmt.Sprintf(
				"MCP server '%s' is configured for this role but every tool on it is "+
					"currently unavailable (role policy / per-turn gating). Servers with "+
					"available tools this turn: %s.", server, orNone(liveNames))}
		}
		lines := make([]string, len(tools))
		for i, t := range tools {
			lines[i] = catalogueLine(t.Name, t.Description)
		}
		return Result{Output: fmt.Sprintf(
			"Tools on MCP server '%s' (%d total). Call activate_tool(name=...) to "+
				"promote one into your tools=[...] so you can invoke it on the next "+
				"round.\n\n%s", server, len(lines), strings.Join(lines, "\n"))}
	}

	return &MetaTool{
		name: "list_mcp_server_tools",
		description: "List the tools available on one MCP server (e.g. github, " +
			"atlassian, slack). Use this when you need a tool from an MCP server " +
			"and don't yet know its exact name — your system prompt's `## MCP " +
			"servers` block lists only server names, not their individual tools. " +
			"After picking a tool, call `activate_tool(name=...)` to promote it " +
			"into your `tools=[...]` so its schema arrives on the next message.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"server": map[string]any{
					"type": "string",
					"description": "MCP server name, exactly as it appears in the " +
						"`## MCP servers` block of your system prompt.",
				},
			},
			"required": []any{"server"},
		},
		call: call,
	}
}

// ActivateTool builds the activate_tool meta-tool.
//
// `surface` is a SUPPLIER rather than the surface itself: the surface is built
// with its meta-tools already in it, so neither can be constructed with the
// other in hand. A one-element container the closure reads through solves it;
// a function that returns the current surface is the same trick without the
// container, and it also keeps working if the surface is replaced
// mid-phase rather than mutated.
//
// `merged` is the role's UNFILTERED catalogue, which is what lets the failure
// distinguish "registered but gated off this turn" from "no such tool". That
// split matters: the first is worth reporting to the model as a policy
// outcome, and the second sends it back to discovery. Conflating them produced
// a loop where the model re-activated a gated tool every round.
//
// `onActivated` is called after a successful activation, with the tool name.
// It is where the engine publishes its phase.tool_activated event and
// propagates the name into the sub-agent allowlist — both of which need engine
// state this package has no business holding. Nil is fine.
func ActivateTool(merged []Entry, surface func() ActivationSurface, onActivated func(name string)) *MetaTool {
	known := make(map[string]struct{}, len(merged))
	for _, e := range merged {
		known[e.Name] = struct{}{}
	}

	call := func(_ context.Context, args map[string]any) Result {
		name, _ := args["name"].(string)
		if name == "" {
			return Result{Failed: true, Output: "name must be a non-empty string"}
		}
		s := surface()
		if s == nil {
			return Result{Failed: true, Output: "no tool surface is active"}
		}
		if s.Has(name) {
			return Result{Output: fmt.Sprintf(
				"Tool '%s' is ALREADY active in your tools=[...] -- call it "+
					"directly. Do not activate again.", name)}
		}
		if s.Activate(name) {
			if onActivated != nil {
				onActivated(name)
			}
			return Result{Output: fmt.Sprintf(
				"Tool '%s' is now active in your tools=[...] and its schema will "+
					"appear on the next message -- call it directly. No need to "+
					"re-activate once active.", name)}
		}
		if _, registered := known[name]; registered {
			return Result{Failed: true, Output: fmt.Sprintf(
				"Tool '%s' is registered but not available in this context "+
					"(availability gate). Pick another tool from the catalogue.", name)}
		}
		return Result{Failed: true, Output: fmt.Sprintf(
			"Tool '%s' is not registered. Use list_mcp_server_tools(server=...) to "+
				"discover MCP tool names if you're not sure of the exact name.", name)}
	}

	return &MetaTool{
		name: "activate_tool",
		description: "Activate a tool from your role's catalogue so you can call " +
			"it directly on subsequent rounds. After activation the tool's schema " +
			"appears in your `tools=[...]` on the next message and you invoke it " +
			"normally; no need to re-activate once active.\n\n" +
			"Use this for both builtin tools (listed by name in `## Builtin " +
			"tools`) and MCP tools (whose names you got from " +
			"`list_mcp_server_tools(server=...)`). In Plan, activate read-only " +
			"recon tools you want to use before submitting the plan — action / " +
			"write tools belong in `submit_plan`'s `tools_needed` so Execute runs " +
			"them under Review. In Execute, activate ANY tool — action or read — " +
			"if the planner missed it.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []any{"name"},
		},
		call: call,
	}
}

// bucketByServer groups entries by their server, dropping anything that is not
// an MCP tool. A nil gate means no gating; a non-nil one keeps only names in
// it, and a server left with nothing does not appear.
func bucketByServer(entries []Entry, gate map[string]struct{}) map[string][]Entry {
	out := map[string][]Entry{}
	for _, e := range entries {
		if e.Server == "" {
			continue
		}
		if gate != nil {
			if _, ok := gate[e.Name]; !ok {
				continue
			}
		}
		out[e.Server] = append(out[e.Server], e)
	}
	// Sorted within each server so the listing is byte-stable across turns —
	// a prompt fragment that reshuffles between rounds is a diff the reader
	// has to discount every time.
	for server := range out {
		sort.Slice(out[server], func(i, j int) bool { return out[server][i].Name < out[server][j].Name })
	}
	return out
}

func sortedKeys(m map[string][]Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// catalogueLine renders one "- name: description" entry, description WHOLE.
//
// Continuation lines are indented under the bullet rather than dropped: the
// one-entry-per-bullet shape is what a model reads a listing as, and keeping
// only the first line paid for that shape with the description's content —
// a vendor's argument rules and preconditions live below its opening sentence,
// and this listing is the only place they are ever shown.
//
// A duplicate of tools.CatalogueLine, which this package cannot import
// (internal/tools imports internal/mcp). See the note on this file's own
// callers in [ListServerTools].
func catalogueLine(name, description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return "- " + name
	}
	lines := strings.Split(description, "\n")
	for i := 1; i < len(lines); i++ {
		// Blank lines stay blank: indenting one leaves trailing
		// whitespace on a line whose only job is the gap.
		if strings.TrimSpace(lines[i]) != "" {
			lines[i] = "  " + lines[i]
		} else {
			lines[i] = ""
		}
	}
	return "- " + name + ": " + strings.Join(lines, "\n")
}
