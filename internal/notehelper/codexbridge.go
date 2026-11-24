package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
)

// THE CODEX BRIDGE CHECK (codex-bridge): run once, before `codex exec` starts
// the thread, it exits 1 unless Codex would hand its model every tool of the
// engine's own server under its own name. Why and how: the package doc's "The
// Codex bridge check".

// bridgeConfig is what -config names for codex-bridge: the app-server's
// socket, the bound on each of its answers in seconds (its listing starts
// every server), and each bridge server with the SHA-256 of every address the
// run may have written for it (a digest: the address carries the job's token).
type bridgeConfig struct {
	Socket  string         `json:"socket"`
	Wait    int            `json:"wait_seconds"`
	Bridges []bridgeServer `json:"bridges"`
}

// bridgeServer is one bridge server of the run: its name and the SHA-256 of
// each address it may be served at.
type bridgeServer struct {
	Name    string   `json:"name"`
	Digests []string `json:"url_sha256"`
}

// codexConfigRead is what the check reads of config/read's answer.
type codexConfigRead struct {
	Config codexLayerConfig `json:"config"`
	Layers []codexLayer     `json:"layers"`
}

// codexLayer is one configuration layer, highest precedence first.
type codexLayer struct {
	Name   json.RawMessage  `json:"name"`
	Config codexLayerConfig `json:"config"`
}

// codexLayerConfig is a configuration's MCP servers and features.
type codexLayerConfig struct {
	MCPServers map[string]codexServerEntry `json:"mcp_servers"`
	Features   map[string]json.RawMessage  `json:"features"`
}

// codexServerEntry is one MCP server's configuration as the check reads it.
type codexServerEntry struct {
	URL     *string `json:"url"`
	Command *string `json:"command"`
	Enabled *bool   `json:"enabled"`
}

// codexServerStatus is one server of mcpServerStatus/list's answer.
type codexServerStatus struct {
	Name       string                     `json:"name"`
	PluginID   *string                    `json:"pluginId"`
	Tools      map[string]json.RawMessage `json:"tools"`
	ToolsError *string                    `json:"toolsError"`
}

// codexRenamingFeatures is each feature that changes the names Codex hands
// its model, or lets a server change its tools after the check, with why.
var codexRenamingFeatures = []struct{ key, why string }{
	{"non_prefixed_mcp_tool_names", "names MCP tools without their mcp__ prefix"},
	{"multi_environment_mcp", "puts an environment's hash in front of each MCP server's namespace"},
	{"mcp_tool_list_changed", "lets an MCP server change its tools after this check"},
}

// runCodexBridge is codex-bridge: exit 0 where every bridge's tools reach the
// model under their own names, 1 where they would not or that cannot be read,
// saying why on stderr.
func runCodexBridge(ctx context.Context, f flags, _ io.Reader, _, stderr io.Writer, _ func(string) string) int {
	refusals, err := checkCodexBridge(ctx, f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: checking that no MCP server renames the engine's own tools: %v\n", err)
		return exitError
	}
	for _, r := range refusals {
		fmt.Fprintf(stderr, "notehelper: %s\n", r)
	}
	if len(refusals) > 0 {
		return exitError
	}
	return exitDone
}

// checkCodexBridge reads config's app-server, from the working directory the
// thread starts in, and is every reason the run may not start.
func checkCodexBridge(ctx context.Context, config string) ([]string, error) {
	raw, err := readBounded(config, maxConfig)
	if err != nil {
		return nil, err
	}
	var c bridgeConfig
	if err = json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("the bridge config: %w", err)
	}
	if c.Socket == "" || c.Wait <= 0 || len(c.Bridges) == 0 {
		return nil, errors.New("the bridge config names no socket, no wait or no bridge")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	p := defaultPacing
	p.callBound = time.Duration(c.Wait) * time.Second
	conn, err := dialSocket(ctx, c.Socket, p)
	if err != nil {
		return nil, err
	}
	a, err := openAppSession(ctx, conn, "", p, events{}, skillSet{})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	defer a.close()
	answer, err := a.call(ctx, "config/read", map[string]any{"includeLayers": true, "cwd": cwd})
	if err != nil {
		return nil, fmt.Errorf("reading Codex's configuration: %w", err)
	}
	var read codexConfigRead
	if err = json.Unmarshal(answer, &read); err != nil {
		return nil, fmt.Errorf("the answer to config/read: %w", err)
	}
	var listed []codexServerStatus
	cursor := ""
	for {
		params := map[string]any{"detail": "toolsAndAuthOnly"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if answer, err = a.call(ctx, "mcpServerStatus/list", params); err != nil {
			return nil, fmt.Errorf("listing Codex's MCP servers: %w", err)
		}
		var page struct {
			Data       []codexServerStatus `json:"data"`
			NextCursor *string             `json:"nextCursor"`
		}
		if err = json.Unmarshal(answer, &page); err != nil {
			return nil, fmt.Errorf("the answer to mcpServerStatus/list: %w", err)
		}
		listed = append(listed, page.Data...)
		if page.NextCursor == nil || *page.NextCursor == "" || *page.NextCursor == cursor {
			break
		}
		cursor = *page.NextCursor
	}
	return bridgeRefusals(c.Bridges, read, listed), nil
}

// codexClean is name as Codex 0.171.0 cleans a namespace or a tool's name
// (sanitize_responses_api_tool_name): each character outside [A-Za-z0-9_]
// made `_`, and "" made "_".
func codexClean(name string) string {
	if name == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range name {
		if r < 0x80 && (r == '_' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// codexNamespace is the namespace Codex hands a server's tools in, with the
// mcp__ prefix it keeps on (codexRenamingFeatures).
func codexNamespace(server string) string {
	ns := codexClean(server)
	if strings.HasPrefix(ns, "mcp__") {
		return ns
	}
	return "mcp__" + ns
}

// bridgeRefusals is every reason, worded for a person, that Codex configured
// as read and listing listed would not hand its model each bridge's tools
// under their own names: a feature that renames them, a bridge served from
// anywhere but the run's own address, another server whose namespace is a
// bridge's or one of whose tools joins to a bridge tool's name, and another
// whose tools could not be listed though its namespace opens one.
func bridgeRefusals(bridges []bridgeServer, read codexConfigRead, listing []codexServerStatus) []string {
	var out []string
	for _, f := range codexRenamingFeatures {
		if string(read.Config.Features[f.key]) == "true" {
			out = append(out, fmt.Sprintf("Codex's configuration turns on features.%s%s, which %s, so the "+
				"engine's own tools would not reach the model under their names", f.key,
				read.declaredBy(func(c codexLayerConfig) bool { return string(c.Features[f.key]) == "true" }), f.why))
		}
	}
	listed := map[string]codexServerStatus{}
	for _, s := range listing {
		listed[s.Name] = s
	}
	servers := map[string]bool{}
	for name, e := range read.Config.MCPServers {
		servers[name] = e.Enabled == nil || *e.Enabled
	}
	for name := range listed {
		if _, ok := servers[name]; !ok {
			servers[name] = true
		}
	}
	for _, b := range bridges {
		out = append(out, read.placeRefusals(b)...)
		status, ok := listed[b.Name]
		if !ok || status.ToolsError != nil {
			out = append(out, fmt.Sprintf("Codex could not list the tools of the engine's own server %q%s", b.Name,
				listError(status)))
			continue
		}
		own := codexNamespace(b.Name)
		joined := map[string]string{}
		for tool := range status.Tools {
			joined[own+codexClean(tool)] = tool
		}
		for _, name := range slices.Sorted(maps.Keys(servers)) {
			if name == b.Name || !servers[name] {
				continue
			}
			where := read.serverFrom(name, listed[name])
			ns := codexNamespace(name)
			if ns == own {
				out = append(out, fmt.Sprintf("the MCP server %q%s takes the namespace of the engine's own server "+
					"%q: Codex names both %s and hands the model each one's tools under a hashed namespace instead; "+
					"rename or remove it", name, where, b.Name, own))
				continue
			}
			s, ok := listed[name]
			if !ok || s.ToolsError != nil {
				for _, j := range slices.Sorted(maps.Keys(joined)) {
					if strings.HasPrefix(j, ns) {
						out = append(out, fmt.Sprintf("the MCP server %q%s could not be listed%s, and Codex names its "+
							"tools %s<tool>, which could spell %s, the name of the engine's own %q; rename it or let "+
							"it list its tools", name, where, listError(s), ns, j, joined[j]))
						break
					}
				}
				continue
			}
			for _, tool := range slices.Sorted(maps.Keys(s.Tools)) {
				if bridged, hit := joined[ns+codexClean(tool)]; hit {
					out = append(out, fmt.Sprintf("the MCP server %q%s lists the tool %q, which Codex joins with "+
						"its namespace %s to %s%s, as it joins the engine's own %q with %s: it then hands the model "+
						"one of the two under a hashed name; rename or remove it", name, where, tool, ns, ns,
						codexClean(tool), bridged, own))
				}
			}
		}
	}
	return out
}

// placeRefusals is why b's place is not the run's own: missing, served by a
// command, at an address the run did not write, or turned off.
func (r codexConfigRead) placeRefusals(b bridgeServer) []string {
	e, ok := r.Config.MCPServers[b.Name]
	if !ok {
		return []string{fmt.Sprintf("Codex's configuration holds no MCP server %q, the engine's own", b.Name)}
	}
	own := e.URL != nil && e.Command == nil
	if own {
		sum := sha256.Sum256([]byte(*e.URL))
		own = slices.Contains(b.Digests, hex.EncodeToString(sum[:]))
	}
	where := r.declaredBy(func(c codexLayerConfig) bool {
		_, declared := c.MCPServers[b.Name]
		return declared
	})
	switch {
	case !own:
		return []string{fmt.Sprintf("Codex's configuration serves the engine's own MCP server %q from elsewhere "+
			"than the run's own address%s; remove that declaration", b.Name, where)}
	case e.Enabled != nil && !*e.Enabled:
		return []string{fmt.Sprintf("Codex's configuration turns off the engine's own MCP server %q%s; remove "+
			"that setting", b.Name, where)}
	}
	return nil
}

// declaredBy names the layers whose configuration holds, by holds, as
// " (declared in <layer>, …)", or "" for none. The run's own configuration,
// CODEX_HOME's, is named too: it is a layer like any other.
func (r codexConfigRead) declaredBy(holds func(codexLayerConfig) bool) string {
	var names []string
	for _, l := range r.Layers {
		if holds(l.Config) {
			names = append(names, layerName(l.Name))
		}
	}
	if len(names) == 0 {
		return ""
	}
	return " (declared in " + strings.Join(names, ", ") + ")"
}

// serverFrom names where server comes from: its layers, else its plugin.
func (r codexConfigRead) serverFrom(server string, s codexServerStatus) string {
	where := r.declaredBy(func(c codexLayerConfig) bool {
		_, declared := c.MCPServers[server]
		return declared
	})
	if where == "" && s.PluginID != nil {
		where = " (from the plugin " + *s.PluginID + ")"
	}
	return where
}

// layerName is a layer as a person finds it: its file or folder, else its
// kind and name.
func layerName(raw json.RawMessage) string {
	var n struct {
		Type, File, DotCodexFolder, Name, Domain, Key string
	}
	_ = json.Unmarshal(raw, &n)
	switch {
	case n.File != "":
		return n.File
	case n.DotCodexFolder != "":
		return n.DotCodexFolder
	case n.Name != "":
		return n.Type + " " + n.Name
	case n.Domain != "":
		return n.Type + " " + n.Domain + " " + n.Key
	case n.Type != "":
		return n.Type
	}
	return "a layer Codex does not name"
}

// listError is s's listing error as " (<error>)", "" for none.
func listError(s codexServerStatus) string {
	if s.ToolsError == nil {
		return ""
	}
	return " (" + *s.ToolsError + ")"
}
