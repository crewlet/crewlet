package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// grokInspected is what a run reads of `grok inspect --json`: every MCP
// server Grok Build would run, under its name, with the file that defined it
// and why it is off, where it is (measured on 1.0.92). Servers is nil where
// the answer holds no such list.
type grokInspected struct {
	Servers *[]struct {
		Name   string `json:"name"`
		Source struct {
			Type string `json:"type"`
			Path string `json:"path"`
		} `json:"source"`
		DisabledReason string `json:"disabledReason"`
	} `json:"mcpServers"`
}

// grokSettingsSource is the source `grok inspect` names a server of the run's
// settings by ([Paths.GrokConfig]).
const grokSettingsSource = "configToml"

// grokServersHeld refuses a run whose box's Grok Build would not run each of
// its servers as the run's settings define it: a system-wide
// `/etc/grok/requirements.toml` naming a server replaces its whole transport,
// its bridge token included, over the user's settings (measured on 1.0.92).
// Each of continued must be listed from the run's settings, and no other file
// may define one of fresh, the names of the session the script may start
// instead; a policy switching one off is the script's guard's ([grokGuard]). Grok's own
// resolution decides, asked before the run starts, so the refusal names the
// file. An answer this build cannot read (another release) refuses nothing:
// the script holds the release, and a run that never lists its bridge's
// tools fails whatever made it so (detached.go, bridgeUnconnected).
func grokServersHeld(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths, continued,
	fresh []string) error {
	q := shellword.Quote
	ask := "PATH=" + q(paths.BinDir()) + `:"$PATH"; export PATH; unset ` + strings.Join(grokSwitchesOff, " ") +
		"; cd " + q(paths.Home) + " && exec grok inspect --json < /dev/null"
	res, err := box.Exec(ctx, "sh -lc "+q(ask), sandbox.ExecOptions{Env: req.Env,
		TimeoutSec: harnessVersionWait.Seconds()})
	if err != nil {
		return fmt.Errorf("executor: asking the box's Grok Build which MCP servers it runs: %w", err)
	}
	// AN ANSWER THIS BUILD CANNOT READ refuses nothing (above): whether it
	// failed or decoded to no list, the reading is simply absent.
	var got grokInspected
	if res.ExitCode != 0 || !decodesInspected(res.Stdout, &got) {
		return nil
	}
	listed := map[string]bool{}
	for _, s := range *got.Servers {
		ours, other := slices.Contains(continued, s.Name), slices.Contains(fresh, s.Name)
		switch {
		case !ours && !other:
			continue
		case !ours || s.Source.Type != grokSettingsSource || s.Source.Path != paths.GrokConfig():
			return fmt.Errorf("executor: the box's Grok Build runs the run's MCP server %q as %.200s (%.40s) "+
				"defines it, not as the run's settings do: remove that server from the file in the box's image",
				s.Name, s.Source.Path, s.Source.Type)
		}
		listed[s.Name] = true
	}
	for _, name := range continued {
		if !listed[name] {
			return fmt.Errorf("executor: the box's Grok Build does not list the run's MCP server %q among "+
				"those it runs (grok inspect): look for what drops it in the box's image", name)
		}
	}
	return nil
}

// decodesInspected reports whether out decodes into got as `grok inspect
// --json` answers, a server list included.
func decodesInspected(out string, got *grokInspected) bool {
	return json.Unmarshal([]byte(out), got) == nil && got.Servers != nil
}
