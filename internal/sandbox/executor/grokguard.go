package executor

import (
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// A HOME CANNOT SWITCH CREWLET'S OWN SERVER OR HOOKS OFF UNSEEN. Grok Build
// 1.0.92 reads a policy from `~/.grok/requirements.toml` (and from
// `/etc/grok` and Claude's managed settings) above every settings layer: a
// `denied_mcp_servers` entry matching the bridge's URL, or an
// `allowed_mcp_servers` list leaving it out, blocks the bridge, and
// `allow_managed_hooks_only` skips every hook of `config.toml`, the run's
// notes hooks among them; no layer can release either (measured with `grok
// inspect --json`). A server's own name carries the run's nonce, so
// `disabled_mcp_servers` cannot name it. So before Grok starts, the script
// asks `grok inspect --json` what it would load and refuses a run whose
// server Grok reports disabled, or whose hooks a pin skips ([grokGuard]).

// grokGuard is the part of a run's script that defines `grokguard`, which
// prints why the run cannot start and fails where `grok inspect --json`, in
// the run's own directory and environment, reports a server the run recorded
// ([Paths.GrokConfigOurs]) disabled (its `disabledReason`, naming the policy
// file and key, beside the policy's server list), or, where hooked, the
// `nonManagedHooks` pin engaged (naming the file that set it). An inspection
// that fails is no verdict. It reads Grok's pretty JSON line by line, its
// fields one a line at fixed indents, so it needs no tool beyond sed and grep.
func grokGuard(paths Paths, hooked bool) string {
	q := shellword.Quote
	var b strings.Builder
	b.WriteString(`grokguard() {
gi=$(grok inspect --json < /dev/null 2> /dev/null) || return 0
gl=$(printf '%s\n' "$gi" | sed -n '/^    "mcpServerAllowlist": \[/,/^    \]/s/^      "\(.*\)",\{0,1\}$/\1/p' | tr '\n' ' ')
gw=
while IFS= read -r gn; do
  [ -n "$gn" ] || continue
  gr=$(printf '%s\n' "$gi" | sed -n "/^      \"name\": \"$gn\",\$/,/^    }/s/^      \"disabledReason\": \"\(.*\)\"\$/\1/p")
  [ -z "$gr" ] || gw="${gw}Grok Build would start without Crewlet's MCP server $gn, which the run's notes and tools go through: $gr (the policy's servers: ${gl:-none listed}); no setting of the run's can release a policy's denied_mcp_servers or allowed_mcp_servers, so change that file's entry to let the server's name and URL pass. "
done < ` + q(paths.GrokConfigOurs()) + `
`)
	if hooked {
		b.WriteString(`gp=$(printf '%s\n' "$gi" | sed -n '/^    "enforced": \[/,/^    \]/p' | grep -A 2 -F '"setting": "nonManagedHooks",' | grep -A 1 -F '"enabled": false,' | sed -n 's/^        "source": "\(.*\)"$/\1/p' | head -n 1)
[ -z "$gp" ] || gw="${gw}Grok Build would run none of the run's hooks, which its notes and skill loads go through: $gp sets allow_managed_hooks_only (or allowManagedHooksOnly), a pin no setting of the run's can release, so remove it from that file. "
`)
	}
	b.WriteString(`[ -z "$gw" ] || { printf 'crewlet: %s The run was refused before Grok Build started.\n' "$gw" >&2; return 1; }
}
`)
	return b.String()
}
