package executor

import (
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// A HOME CANNOT SWITCH CREWLET'S OWN SERVER OR HOOKS OFF UNSEEN. Grok Build
// 1.0.92 reads a policy (`~/.grok/requirements.toml`, `/etc/grok`, Claude's
// managed settings) above every settings layer: `denied_mcp_servers` or
// `allowed_mcp_servers` blocks the bridge, `allow_managed_hooks_only` skips
// every hook of `config.toml`, nothing releases either, and
// `~/.grok/disabled-hooks` skips the hooks it names with no word (measured).
// So before Grok starts, the script asks `grok inspect --json` what it would
// load, reads disabled-hooks, which inspect does not reflect, and refuses a
// run either switches off its server or contract hooks (notes, skill loads)
// ([grokGuard]). The nested hook only adds a clone's handing to the launch's,
// so its loss refuses nothing and is said once in the skip log.

// grokGuard is the part of a run's script that defines `grokguard`, which
// prints why the run cannot start and fails where `grok inspect --json`, in
// the run's directory and environment, reports a server the run recorded
// ([Paths.GrokConfigOurs]) disabled (its `disabledReason`, naming the policy
// file and key, beside its server list), or, where the run has contract hooks
// (an id in [Paths.GrokHookIDs]), the `nonManagedHooks` pin engaged, or
// `~/.grok/disabled-hooks` naming one of those ids on a line, trimmed as
// 1.0.92 trims it, or there and unreadable (all measured). Any of those that
// switches the nested hook off ([Paths.GrokNestedHookID]) refuses nothing: gk
// names it, for the script's skip line ([grokNestedOff]). An inspection that
// fails is no verdict; Grok's pretty JSON is read line by line with sed.
func grokGuard(paths Paths) string {
	q := shellword.Quote
	var b strings.Builder
	b.WriteString(`grokguard() {
gi=$(grok inspect --json < /dev/null 2> /dev/null) || gi=
gl=$(printf '%s\n' "$gi" | sed -n '/^    "mcpServerAllowlist": \[/,/^    \]/s/^      "\(.*\)",\{0,1\}$/\1/p' | tr '\n' ' ')
gw=
while IFS= read -r gn; do
  [ -n "$gn" ] || continue
  gr=$(printf '%s\n' "$gi" | sed -n "/^      \"name\": \"$gn\",\$/,/^    }/s/^      \"disabledReason\": \"\(.*\)\"\$/\1/p")
  [ -z "$gr" ] || gw="${gw}Grok Build would start without Crewlet's MCP server $gn, which the run's notes and tools go through: $gr (the policy's servers: ${gl:-none listed}); no setting of the run's can release a policy's denied_mcp_servers or allowed_mcp_servers, so change that file's entry to let the server's name and URL pass. "
done < ` + q(paths.GrokConfigOurs()) + `
`)
	ids, nested := q(paths.GrokHookIDs()), q(paths.GrokNestedHookID())
	b.WriteString(`gk=
gp=$(printf '%s\n' "$gi" | sed -n '/^    "enforced": \[/,/^    \]/p' | grep -A 2 -F '"setting": "nonManagedHooks",' | grep -A 1 -F '"enabled": false,' | sed -n 's/^        "source": "\(.*\)"$/\1/p' | head -n 1)
gd=` + q(paths.GrokDisabledHooks()) + `; gu=
if [ -e "$gd" ] && { [ ! -f "$gd" ] || [ ! -r "$gd" ]; }; then gu=1; fi
if [ -s ` + ids + ` ]; then
[ -z "$gp" ] || gw="${gw}Grok Build would run none of the run's hooks, which ` + grokHooksCarry + `: $gp sets allow_managed_hooks_only (or allowManagedHooksOnly), a pin no setting of the run's can release, so remove it from that file. "
if [ -n "$gu" ]; then gw="${gw}Grok Build would run none of the run's hooks, which ` + grokHooksCarry + `: $gd cannot be read, and Grok Build then skips every hook outside managed policy; make it a readable file, or remove it. "
elif [ -f "$gd" ]; then while IFS= read -r gh || [ -n "$gh" ]; do gh=$(printf '%s' "$gh" | tr -d '\r' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
  [ -n "$gh" ] && grep -q -x -F -e "$gh" ` + ids + ` 2> /dev/null && gw="${gw}Grok Build would skip the run's hook $gh, which ` + grokHooksCarry + `: $gd names it on a line of its own, and no setting of the run's may run a hook that file switches off, so remove that line. "
done < "$gd"; fi
fi
if [ -s ` + nested + ` ]; then
if [ -n "$gp" ]; then gk="$gp sets allow_managed_hooks_only (or allowManagedHooksOnly)"
elif [ -n "$gu" ]; then gk="$gd cannot be read, and Grok Build then skips every hook outside managed policy"
elif [ -f "$gd" ] && tr -d '\r' < "$gd" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | grep -q -x -F -f ` + nested + `; then gk="$gd names it"
fi
fi
`)
	b.WriteString(`[ -z "$gw" ] || { printf 'crewlet: %s The run was refused before Grok Build started.\n' "$gw" >&2; return 1; }
}
`)
	return b.String()
}

// grokHooksCarry is what the run's hooks carry, as a refusal names it.
const grokHooksCarry = "its notes and skill loads go through"

// grokNestedOff is the part of a run's script, after the gate, that adds to
// the run's skip log ([checkoutHome.saySkipped]) why the nested hook will not
// run, where the guard found the box's settings switch it off (gk).
func grokNestedOff() string {
	return `[ -z "$gk" ] || [ ! -d "$rr" ] || printf 'crewlet: a repository cloned during this run is not handed ` +
		`its instructions and skills: %s, which switches off the hook that hands them\n' "$gk" >> "$rr/skipped"
`
}
