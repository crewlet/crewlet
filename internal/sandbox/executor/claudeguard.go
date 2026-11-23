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
// `disabled` in, the `mcp_servers` its stream's init event lists, and the run
// goes on without its notes and its tools. So the script reads that event and
// ends a run whose own server is not there ([claudeGuard]), saying which file
// switched it off.

// crewletServers is the run's servers of Crewlet's own, by name, which a
// guard holds the harness to: the engine's own tools, the bridge's `crewlet`
// ([sandbox.MCPServer.AlwaysLoaded]). A seat's other servers are its own,
// which a home may switch off as a person's would.
func crewletServers(req sandbox.RunRequest) []string {
	var names []string
	for _, name := range slices.Sorted(maps.Keys(req.MCPServers)) {
		if req.MCPServers[name].AlwaysLoaded {
			names = append(names, name)
		}
	}
	return names
}

// claudeGuard is the part of a run's script that defines `claudeguard`, which
// [launchWatched] runs once the harness has started: it waits for the
// stream's first whole line (or the harness's end, or an end request), and
// where that is the init event, ends the harness unless each of servers is
// `connected` or still `pending` in it, saying why on the error stream.
//
// AN INIT EVENT IT CANNOT FIND IS NO VERDICT: every release measured opens
// its stream with one, keys in this order, so a stream that does not is not
// one a switch-off shows in. The reason names the file that switched the
// server off: each settings file holding a deny or allow list, or the state
// file that disables it.
func claudeGuard(paths Paths, servers []string) string {
	q := shellword.Quote
	stream := q(paths.Stream())
	state := q(paths.ClaudeConfigDir() + "/.claude.json")
	lists := strings.Join([]string{"/etc/claude-code/managed-settings.json",
		"/etc/claude-code/managed-settings.d/*.json", q(paths.ClaudeConfigDir() + "/settings.json"),
		`"$PWD/.claude/settings.json"`, `"$PWD/.claude/settings.local.json"`}, " ")
	var b strings.Builder
	b.WriteString(`claudeguard() {
while [ -z "$endhalted" ] && kill -0 "$harness" 2> /dev/null; do case $(wc -l < ` + stream + ` 2> /dev/null) in *[1-9]*) break ;; esac; sleep 0.1; done
[ -z "$endhalted" ] || return 0
gl=$(head -n 1 ` + stream + ` 2> /dev/null)
case "$gl" in '{"type":"system","subtype":"init",'*'"mcp_servers":'*) ;; *) return 0 ;; esac
gw=
`)
	for _, name := range servers {
		entry := claudeServerEntry(name)
		said := "Crewlet's MCP server " + name + ", which the run's notes and tools go through"
		b.WriteString(`[ -n "$gw" ] || case "$gl" in
*` + q(entry+`"connected"`) + `*|*` + q(entry+`"pending"`) + `*) ;;
*` + q(entry+`"disabled"`) + `*) gf=` + state + `; [ ! -L "$gf" ] || gf=` + q(paths.ClaudeState()) +
			`; gw="Claude Code started with ` + said + `, disabled: $gf disables it for the start directory ` +
			`(its projects entry's disabledMcpServers), and no setting of the run's undoes that" ;;
*` + q(entry) + `*) gs=${gl#*` + q(entry+`"`) + `}; gw="Claude Code started without ` + said +
			` connected: its init event reports it ${gs%%\"*}" ;;
*) gf=$(grep -l -e deniedMcpServers -e allowedMcpServers ` + lists + ` 2> /dev/null | tr '\n' ' '); ` +
			`gw="Claude Code started without ` + said + `: the servers its init event lists leave it out, ` +
			`as a deniedMcpServers entry naming it or an allowedMcpServers list without it does, and no setting ` +
			`of the run's undoes either. Settings holding one: ${gf:-none of those it reads (the managed ` +
			`settings of the box, ~/.claude/settings.json, and .claude/settings.json and ` +
			`.claude/settings.local.json in the start directory), so the error stream says what blocked it}" ;;
esac
`)
	}
	b.WriteString(`[ -z "$gw" ] || { printf 'crewlet: %s. The run was ended as it started.\n' "$gw" >&2; end "$harness"; }
}
`)
	return b.String()
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
