package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// claudeNoProxyHolder stands in the run's settings for the merged NO_PROXY list
// the script writes over it at launch ([claudeNoProxyMerge]).
const claudeNoProxyHolder = "@CREWLET_NO_PROXY@"

// claudeProxyEntries is every NO_PROXY and no_proxy entry the settings files
// Claude Code loads for the run hold in their `env` (the home's, then the
// project's and the local file), in that order, and whether any file sets
// either spelling. Claude Code 2.1.330 assigns each file's `env` over the
// process's, key by key, and reads no_proxy before NO_PROXY, so a file's value
// leaving the bridge's host out sent the bridge through the proxy (measured).
func claudeProxyEntries(ctx context.Context, box sandbox.Sandbox, paths Paths) ([]string, bool, error) {
	start := path.Join(box.Home(), sandbox.WorkspaceSubdir)
	var entries []string
	set := false
	for _, file := range []string{paths.ClaudeConfigDir() + "/settings.json", start + "/.claude/settings.json",
		start + "/.claude/settings.local.json"} {
		raw, err := box.ReadFile(ctx, file)
		if err != nil {
			return nil, false, fmt.Errorf("executor: reading Claude Code's settings %s: %w", file, err)
		}
		var settings struct {
			Env map[string]any `json:"env"`
		}
		if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &settings) != nil {
			continue
		}
		for _, name := range []string{"NO_PROXY", "no_proxy"} {
			if v, ok := settings.Env[name].(string); ok {
				set = true
				entries = append(entries, strings.Split(v, ",")...)
			}
		}
	}
	return entries, set, nil
}

// claudeNoProxyMerge is the script's step, before Claude Code starts, writing
// over [claudeNoProxyHolder] in the chosen settings ($cs) one list for both
// spellings: the process's NO_PROXY and no_proxy (the wrapper's, holding the
// bridge's hosts, [noProxyExports]), the settings files' entries
// ([Paths.ClaudeNoProxy]), then hosts, in order, blanks and repeats dropped.
// The run's `--settings` ranks above every file, so each keeps the person's
// entries and the bridge's host. Settings without the holder are left alone.
func claudeNoProxyMerge(paths Paths, hosts []string) string {
	q := shellword.Quote
	list := q(paths.ClaudeNoProxy())
	return `if grep -qF '` + claudeNoProxyHolder + `' "$cs"; then np=$(printf '%s,%s,%s,%s\n' "${NO_PROXY:-}" "${no_proxy:-}" "$(cat ` + list +
		`)" ` + q(strings.Join(hosts, ",")) + ` | tr ',' '\n' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | ` +
		`awk 'NF && !seen[$0]++' | paste -sd, -)
np=$(printf '%s' "$np" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/[\\&|]/\\&/g')
sed "s|` + claudeNoProxyHolder + `|$np|g" "$cs" > "$cs.np" && cs="$cs.np" || { echo 'crewlet: the run'"'"'s Claude Code settings could not be given its NO_PROXY list, so the run does not start' >&2; exit 1; }
fi
`
}
