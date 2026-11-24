package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A PERSON'S DENY CANNOT CUT A CURSOR RUN OFF FROM ITS BRIDGE UNSEEN. Cursor
// Agent 2026.11.22-e9042b2 honours every `permissions.deny` rule under
// `--force`, an `Mcp(...)` rule naming the run's own servers included, and has
// no layer of the run's above it (all measured, local-loop build). So before
// the run's servers are laid in, the launch reads the rules Cursor will use
// ([cursorEffectiveDenies]) and refuses the run when one matches a tool of the
// run's own servers ([cursorMCPDenyMatches]), naming the file and the rule.

// cursorProjectConfig is the start directory's permissions file, relative to
// it. Cursor reads it there alone: one in a directory above, the home's own
// `.cursor` included, is not read (measured).
const cursorProjectConfig = ".cursor/cli.json"

// cursorGuard refuses req's run where the permissions Cursor will use deny a
// tool of the run's own servers ([crewletServers], the fresh wiring's too),
// each under its name in the run (nonce, [cursorServerName]).
func cursorGuard(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths, nonce string) error {
	servers := map[string]sandbox.MCPServer{}
	for _, set := range []map[string]sandbox.MCPServer{req.MCPServers, freshServers(req)} {
		for _, name := range crewletServers(set) {
			servers[cursorServerName(name, nonce)] = set[name]
		}
	}
	if len(servers) == 0 {
		return nil
	}
	project := path.Join(box.Home(), sandbox.WorkspaceSubdir, cursorProjectConfig)
	home := paths.CursorDir() + "/cli-config.json"
	files := map[string][]byte{}
	for _, f := range []string{project, home} {
		raw, err := box.ReadFile(ctx, f)
		if err != nil {
			return fmt.Errorf("executor: reading %s, whose permissions Cursor would hold the run's tools to: %w", f, err)
		}
		files[f] = raw
	}
	file, rules := cursorEffectiveDenies(project, files[project], home, files[home])
	var said []string
	for _, rule := range rules {
		for _, name := range slices.Sorted(maps.Keys(servers)) {
			if tool, ok := cursorDeniedTool(rule, name, servers[name].Tools); ok {
				said = append(said, fmt.Sprintf("%s lists %q in permissions.deny, which refuses %s's %s", file, rule,
					name, tool))
				break
			}
		}
	}
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: Cursor would refuse the run's own MCP server's tools, so the run would lose its "+
		"bridge to the seat's tools, its notes and its submit_work: %s. Change those rules, or run on a box "+
		"without them", strings.Join(said, "; "))
}

// freshServers is req's fresh wiring's servers, none where it has none.
func freshServers(req sandbox.RunRequest) map[string]sandbox.MCPServer {
	if req.FreshWiring == nil {
		return nil
	}
	return req.FreshWiring.MCPServers
}

// cursorEffectiveDenies is the file whose `permissions.deny` Cursor holds a
// run to, and its rules, as measured: the start directory's file wherever it
// is a JSON object with a `permissions` key, which replaces the home's whole,
// an empty allow list beside the deny list included; else the home's. A start directory's file that
// is no JSON object, or one whose block Cursor's schema refuses, stops Cursor
// before any call, so it has no rules here. A home file Cursor rejects (no
// JSON object, `permissions` no object, `allow` or `deny` no list, a deny
// entry no string) it rewrites to its defaults, which deny nothing.
func cursorEffectiveDenies(project string, projectRaw []byte, home string, homeRaw []byte) (string, []string) {
	var p map[string]json.RawMessage
	if len(strings.TrimSpace(string(projectRaw))) > 0 {
		if json.Unmarshal(projectRaw, &p) != nil || p == nil {
			return project, nil
		}
		if block, ok := p["permissions"]; ok {
			rules, _ := cursorDenyList(block, true)
			return project, rules
		}
	}
	var h map[string]json.RawMessage
	if json.Unmarshal(homeRaw, &h) != nil || h == nil {
		return home, nil
	}
	block, ok := h["permissions"]
	if !ok {
		return home, nil
	}
	rules, valid := cursorDenyList(block, false)
	if !valid {
		return home, nil
	}
	return home, rules
}

// cursorDenyList is a permissions block's deny rules, none where the block is
// one Cursor's schema refuses, and whether it takes it: an object whose
// `allow` and `deny`, where given, are lists, every deny entry a string, and
// in a start directory's file (project) an `allow` list given (measured: a
// block of a deny list alone, or none, stops Cursor there).
func cursorDenyList(block json.RawMessage, project bool) ([]string, bool) {
	var b map[string]json.RawMessage
	if json.Unmarshal(block, &b) != nil || b == nil {
		return nil, false
	}
	raw, ok := b["allow"]
	if !ok && project {
		return nil, false
	}
	if ok {
		var allow []json.RawMessage
		if json.Unmarshal(raw, &allow) != nil || allow == nil {
			return nil, false
		}
	}
	if raw, ok = b["deny"]; !ok {
		return nil, true
	}
	var deny []string
	if json.Unmarshal(raw, &deny) != nil || deny == nil {
		return nil, false
	}
	return deny, true
}

// cursorDeniedTool is the first of tools rule denies on the server named
// server, as Cursor matches an `Mcp(...)` rule (measured): the rule trimmed,
// its family `Mcp` in that case (`mcp(` and `MCP(` match nothing), one colon
// between a server and a tool pattern, each trimmed (`Mcp(x)` and `Mcp(*)`
// match nothing, nor does a second colon). A pattern's `*` stands for any
// run of characters, as the whole, a prefix or mid-name; every other
// character, `?` and `[` included, stands for itself, case aside. A server
// whose tools the engine does not list is denied only by a tool pattern
// matching every name.
func cursorDeniedTool(rule, server string, tools []string) (string, bool) {
	inner, ok := strings.CutPrefix(strings.TrimSpace(rule), "Mcp(")
	if !ok {
		return "", false
	}
	if inner, ok = strings.CutSuffix(inner, ")"); !ok {
		return "", false
	}
	serverPattern, toolPattern, ok := strings.Cut(inner, ":")
	if !ok || strings.Contains(toolPattern, ":") {
		return "", false
	}
	serverPattern, toolPattern = strings.TrimSpace(serverPattern), strings.TrimSpace(toolPattern)
	if !cursorGlob(serverPattern, server) {
		return "", false
	}
	if len(tools) == 0 {
		if strings.Trim(toolPattern, "*") == "" && toolPattern != "" {
			return "every tool", true
		}
		return "", false
	}
	for _, tool := range tools {
		if cursorGlob(toolPattern, tool) {
			return "tool " + tool, true
		}
	}
	return "", false
}

// cursorGlob reports whether name matches pattern as Cursor matches a
// permission's server or tool: `*` any run of characters, the rest literal,
// without case.
func cursorGlob(pattern, name string) bool {
	parts := strings.Split(strings.ToLower(pattern), "*")
	name = strings.ToLower(name)
	if len(parts) == 1 {
		return parts[0] == name
	}
	rest, ok := strings.CutPrefix(name, parts[0])
	if !ok {
		return false
	}
	for _, part := range parts[1 : len(parts)-1] {
		at := strings.Index(rest, part)
		if at < 0 {
			return false
		}
		rest = rest[at+len(part):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}
