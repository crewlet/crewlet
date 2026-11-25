package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// museProjectFiles is the shell listing, a path a line, each `.mcp.json` Muse
// Code 1.4.25 reads a server from: the start directory's, and each directory's
// above it up to the repository's root, the start directory alone outside a
// repository (as [museGuard] names them).
const museProjectFiles = `gd=$(pwd -P); gr=$(git rev-parse --show-toplevel 2> /dev/null)
while :; do [ ! -f "$gd/.mcp.json" ] || printf '%s\n' "$gd/.mcp.json"; case "$gr" in ''|"$gd") break ;; esac; [ "$gd" != / ] || break; gd=$(dirname "$gd"); done`

// museServersHeld refuses a run where a repository's `.mcp.json` declares a
// server under one of names, the run's servers' names for its sessions: Muse
// Code runs that entry in place of the run's whole (measured on 1.4.25), and
// a name is its session's ([sessionNonce]), which code an earlier run of the
// session left can know. Each file is read as Muse reads it, a key decoded
// from JSON; one that does not read is passed over, which leaves out every
// server, the bridge's among them, and its guard says so ([museGuard]).
func museServersHeld(ctx context.Context, box sandbox.Sandbox, names []string) error {
	res, err := box.Exec(ctx, "sh -c "+shellword.Quote(museProjectFiles),
		sandbox.ExecOptions{Cwd: path.Join(box.Home(), sandbox.WorkspaceSubdir), TimeoutSec: harnessVersionWait.Seconds()})
	if err != nil {
		return fmt.Errorf("executor: listing the repository's MCP files Muse Code reads: %w", err)
	}
	for _, file := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if file == "" {
			continue
		}
		raw, err := box.ReadFile(ctx, file)
		if err != nil {
			continue
		}
		var doc struct {
			Servers map[string]json.RawMessage `json:"mcpServers"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		for _, name := range names {
			if _, ok := doc.Servers[name]; ok {
				return fmt.Errorf("executor: %s declares the MCP server %q, which Muse Code would run in place of "+
					"the run's own of that name: a repository's or home's MCP server may not take a name inside "+
					"Crewlet's own server's tool names, or its place; remove it from that file", file, name)
			}
		}
	}
	return nil
}

// museServerNames is servers' names as a session of nonce names them
// ([writeMuseServers]), sorted.
func museServerNames(servers map[string]sandbox.MCPServer, nonce string) []string {
	var out []string
	for name := range servers {
		out = append(out, name+"-"+nonce)
	}
	slices.Sort(out)
	return out
}
