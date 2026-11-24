package executor

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A HOME OR A REPOSITORY CANNOT SWITCH CREWLET'S OWN SERVER OFF UNSEEN. Claude
// Code has no layer above a `deniedMcpServers` entry or an `allowedMcpServers`
// list in any settings file, the home's and the repository's included, or
// above `disabledMcpServers` in the home's `.claude.json` for the start
// directory (measured on 2.1.330): the server is then missing from, or
// `disabled` in, the `mcp_servers` its stream's init event lists. Nor above a
// `permissions.deny` entry naming the server (`mcp__crewlet`), one of its
// tools (`mcp__crewlet__<tool>`) or a wildcard over them (`mcp__crewlet__*`,
// `mcp__*`, `*`), or an `agent` setting whose agent's `tools:` leave them out:
// it connects, its tools missing from the event's `tools`. So the script ends
// a run whose own server, or a tool the engine says it lists, is not there in
// that event ([claudeGuard]), naming the file and the entry.

// crewletServers is servers' of Crewlet's own, by name, sorted, which a guard
// holds the harness to: the engine's own tools, the bridge's `crewlet`
// ([sandbox.MCPServer.AlwaysLoaded]). A seat's other servers are its own,
// which a home may switch off as a person's would; no other server may take
// these names or a tool name inside them (code-sandbox.md, each harness's
// section).
func crewletServers(servers map[string]sandbox.MCPServer) []string {
	var names []string
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if servers[name].AlwaysLoaded {
			names = append(names, name)
		}
	}
	return names
}

// claudeGuard is the part of a run's script that defines `claudeguard`, which
// [launchWatched] runs once the harness has started: once the stream's first
// line is the init event, it ends the harness unless each of servers is
// `pending` in it, or `connected` with every tool mcp names for it listed (a
// tool of its own, where it names none), saying why on the error stream.
//
// AN INIT EVENT IT CANNOT FIND, OR ONE LISTING NO TOOLS, IS NO VERDICT: every
// release measured opens its stream with one. The reason names what switched
// the server off: a settings file holding a deny or allow list, each file and
// entry that can take the missing tools away (`claudedenies`, and each `agent`
// setting with its definition, `claudeagent`: the verdict is the init event's,
// so an entry is named, never judged), or the state file that disables it.
func claudeGuard(paths Paths, servers []string, mcp map[string]sandbox.MCPServer) string {
	q := shellword.Quote
	stream := q(paths.Stream())
	state := q(paths.ClaudeConfigDir() + "/.claude.json")
	agents := q(paths.ClaudeConfigDir() + "/agents")
	lists := strings.Join([]string{"/etc/claude-code/managed-settings.json",
		"/etc/claude-code/managed-settings.d/*.json", q(paths.ClaudeConfigDir() + "/settings.json"),
		`"$PWD/.claude/settings.json"`, `"$PWD/.claude/settings.local.json"`}, " ")
	read := "none of those it reads (the managed settings of the box, ~/.claude/settings.json, and " +
		".claude/settings.json and .claude/settings.local.json in the start directory), so the error " +
		"stream says what blocked it"
	var b strings.Builder
	b.WriteString(`claudedenies() { gn=; for gf in ` + lists + `; do [ -f "$gf" ] || continue; for ge; do ` +
		`grep -q -F -e "\"$ge\"" "$gf" 2> /dev/null && gn="$gn${gn:+, }$gf ($ge)"; done; done; }
claudeagent() { for gf in ` + lists + `; do [ -f "$gf" ] || continue; ` +
		`ga=$(sed -n 's/.*"agent"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$gf" 2> /dev/null | head -n 1); ` +
		`[ -n "$ga" ] || continue; gd=; for gx in "$PWD"/.claude/agents/*.md ` + agents + `/*.md; do [ -f "$gx" ] && ` +
		`grep -q -x -F -e "name: $ga" -e "name: \"$ga\"" -e "name: '$ga'" "$gx" 2> /dev/null && { gd=$gx; break; }; done; ` +
		`gn="$gn${gn:+, }$gf (agent: $ga, ${gd:-an agent of that name})"; done; }
claudeguard() {
while [ -z "$endhalted" ] && kill -0 "$harness" 2> /dev/null; do case $(wc -l < ` + stream + ` 2> /dev/null) in *[1-9]*) break ;; esac; sleep 0.1; done
[ -z "$endhalted" ] || return 0
gl=$(head -n 1 ` + stream + ` 2> /dev/null)
case "$gl" in '{"type":"system","subtype":"init",'*'"mcp_servers":'*) ;; *) return 0 ;; esac
gw=
`)
	for _, name := range servers {
		entry := claudeServerEntry(name)
		said := "Crewlet's MCP server " + name + ", which the run's notes and tools go through"
		tool := claudeToolPrefix(name)
		wild := q(tool) + " " + q(tool+"__*") + " 'mcp__*' '*'"
		var each strings.Builder
		for _, t := range slices.Sorted(slices.Values(mcp[name].Tools)) {
			listed := tool + "__" + claudeMCPName(t)
			each.WriteString(`case "$gt" in *` + q(`"`+listed+`"`) + `*) ;; *) gm="$gm ` + listed + `" ;; esac
`)
		}
		b.WriteString(`[ -n "$gw" ] || case "$gl" in
*` + q(entry+`"connected"`) + `*) case "$gl" in *'"tools":['*) gt=${gl#*'"tools":['}; gt=${gt%%']'*} ;; *) gt= ;; esac
case "$gt" in '') ;;
*` + q(`"`+tool+`__`) + `*) gm=
` + each.String() + `[ -z "$gm" ] || { claudedenies $gm ` + wild + `; claudeagent; gw="Claude Code started with ` + said +
			` connected, and without its tools$gm: a permissions.deny entry naming them, a wildcard ` +
			`matching them, or an agent setting running the main thread as an agent whose tools leave them ` +
			`out, takes them away, and no setting of the run's undoes that. Entries that can: ` +
			`${gn:-` + read + `}"; } ;;
*) claudedenies ` + wild + `; claudeagent; gw="Claude Code started with ` + said + ` connected, and none ` +
			`of its tools: a permissions.deny entry naming it (` + tool + `), its tools or a wildcard matching ` +
			`them, or an agent setting running the main thread as an agent whose tools leave them out, takes ` +
			`them away, and no setting of the run's undoes that. Entries that can: ${gn:-` + read + `}" ;; esac ;;
*` + q(entry+`"pending"`) + `*) ;;
*` + q(entry+`"disabled"`) + `*) gf=` + state + `; [ ! -L "$gf" ] || gf=` + q(paths.ClaudeState()) +
			`; gw="Claude Code started with ` + said + `, disabled: $gf disables it for the start directory ` +
			`(its projects entry's disabledMcpServers), and no setting of the run's undoes that" ;;
*` + q(entry) + `*) gs=${gl#*` + q(entry+`"`) + `}; gw="Claude Code started without ` + said +
			` connected: its init event reports it ${gs%%\"*}" ;;
*) gf=$(grep -l -e deniedMcpServers -e allowedMcpServers ` + lists + ` 2> /dev/null | tr '\n' ' '); ` +
			`gw="Claude Code started without ` + said + `: the servers its init event lists leave it out, ` +
			`as a deniedMcpServers entry naming it or an allowedMcpServers list without it does, and no setting ` +
			`of the run's undoes either. Settings holding one: ${gf:-` + read + `}" ;;
esac
`)
	}
	b.WriteString(`[ -z "$gw" ] || { printf 'crewlet: %s. The run was ended as it started.\n' "$gw" >&2; end "$harness"; }
}
`)
	return b.String()
}

// claudeToolPrefix is how each tool of server name is named, less its `__`
// and tool: `mcp__<name>` ([claudeMCPName]).
func claudeToolPrefix(name string) string { return "mcp__" + claudeMCPName(name) }

// claudeMCPName is a server's or a tool's name as Claude Code 2.1.330 writes
// it into a tool's: every character but letters, digits, `_` and `-` made `_`.
func claudeMCPName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, name)
}

// claudeServerEntry is how a server's entry opens in the init event's
// `mcp_servers`, up to its status's value: `{"name":"<name>","status":`, the
// name as JSON writes it (2.1.330's JSON.stringify, which escapes no HTML).
func claudeServerEntry(name string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(name)
	return `{"name":` + strings.TrimSuffix(b.String(), "\n") + `,"status":`
}
