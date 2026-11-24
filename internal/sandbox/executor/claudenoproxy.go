package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// claudeProxyBypass is the `env` entries the run's own settings add so Claude
// Code keeps the bridge direct ([noProxyExports]): Claude Code 2.1.330 assigns
// each settings file's `env` over the process's, so a home's `NO_PROXY` there
// replaced the wrapper's and, leaving the bridge's host out, cut the run off
// its tools (measured). For each spelling the home's, the project's or the
// local file sets, the one Claude Code would take (local over project over
// home) with every host of hosts it lacks appended, the person's own entries
// kept in their order; the run's `--settings` ranks above all three. A
// spelling no file sets is left out, so the wrapper's value stands.
func claudeProxyBypass(ctx context.Context, box sandbox.Sandbox, paths Paths, hosts []string) (map[string]string, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	start := path.Join(box.Home(), sandbox.WorkspaceSubdir)
	files := []string{paths.ClaudeConfigDir() + "/settings.json", start + "/.claude/settings.json",
		start + "/.claude/settings.local.json"}
	set := map[string]string{}
	for _, file := range files {
		raw, err := box.ReadFile(ctx, file)
		if err != nil {
			return nil, fmt.Errorf("executor: reading Claude Code's settings %s: %w", file, err)
		}
		var settings struct {
			Env map[string]any `json:"env"`
		}
		if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &settings) != nil {
			continue
		}
		for _, name := range []string{"NO_PROXY", "no_proxy"} {
			if v, ok := settings.Env[name].(string); ok {
				set[name] = v
			}
		}
	}
	if len(set) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(set))
	for name, v := range set {
		entries := strings.Split(v, ",")
		for _, host := range hosts {
			if !slices.ContainsFunc(entries, func(e string) bool { return strings.TrimSpace(e) == host }) {
				if strings.TrimSpace(v) == "" {
					v = host
				} else {
					v += "," + host
				}
				entries = append(entries, host)
			}
		}
		out[name] = v
	}
	return out, nil
}
