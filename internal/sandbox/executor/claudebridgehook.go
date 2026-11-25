package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// claudeManagedMCP is Claude Code's managed MCP configuration on Linux. While it
// exists the harness refuses a run's `--mcp-config` (the file readable) or drops
// it silently, the bridge with it, and starts with no server of the engine's
// (the file unreadable, unparsable or writable by the run's account): measured
// on 2.1.330. So a run handed servers is not started beside it ([claudeScript]).
const claudeManagedMCP = "/etc/claude-code/managed-mcp.json"

// claudeUnbridgedEvent is the type of the line a run's script prints once its
// harness exits where the guard refused a call ([Paths.ClaudeUnbridged]): the
// stream's last line, the harness's result, wrapped with the server's source
// and agent as the hook read them, so the run fails in the guard's words and
// keeps its spend ([ClaudeCode.Parse]).
const claudeUnbridgedEvent = "crewlet_claude_unbridged"

// claudeShadowed is the sentence a run whose bridge another server stood in for
// fails with, the start of every such failure.
const claudeShadowed = "a repository's or home's MCP server may not take a name inside Crewlet's own " +
	"server's tool names, or its place"

// claudeRefusal is the reason the guard hands Claude Code for a refused call,
// which the model reads (a subagent's) and the loop stops on (the main
// thread's): fixed words, never the hook input's.
const claudeRefusal = "Crewlet refused this call: another MCP server than Crewlet's own serves it under " +
	"a name of Crewlet's own server's (an agent definition's own mcpServers, most likely), so the run fails."

// claudeToolSpelling is a server's name as Claude Code writes it in a tool's
// `mcp__<server>__<tool>`: every character outside `A-Za-z0-9_-` as `_`, one for
// one (read on 2.1.330).
func claudeToolSpelling(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, name)
}

// claudeGuardScript is the hook every call named inside a bridge's names
// (`mcp__<bridge>__`) runs ([Paths.ClaudeBridgeGuard]): only the run's own
// `--mcp-config` server of that name (source `dynamic`) is let through; else
// the call is refused, the loop stopped, and the server's source and the agent
// recorded for the script's [claudeUnbridgedEvent] line. AN AGENT'S OWN server
// `crewlet` (`.claude/agents/<n>.md`, `mcpServers`) serves every bridge name its
// `tools:` leaves out, and names the bridge lists none of (measured on 2.1.330);
// only the call shows it. READ OFF THE INPUT'S END: `mcp_server` is its last
// key, after `tool_input`, so a model's arguments cannot write the end matched;
// the agent is read before the first nested object, sanitised.
func claudeGuardScript(bridges []string, marker string) (string, error) {
	var allowed []string
	for _, name := range bridges {
		n, err := json.Marshal(name)
		if err != nil {
			return "", err
		}
		allowed = append(allowed, "*"+shellword.Quote(`,"mcp_server":{"name":`+string(n)+`,"source":"dynamic"}}`))
	}
	decision, err := json.Marshal(map[string]any{"continue": false, "stopReason": claudeRefusal,
		"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "deny",
			"permissionDecisionReason": claudeRefusal}})
	if err != nil {
		return "", err
	}
	m := shellword.Quote(marker)
	return "i=$(cat)\n" +
		"case $i in " + strings.Join(allowed, "|") + ") exit 0 ;; esac\n" +
		`s=$(printf '%s' "$i" | sed -n 's/.*,"mcp_server":{"name":"[^"]*","source":"\([^"]*\)"}}$/\1/p' | tr -cd 'A-Za-z0-9_-' | cut -c1-32)` + "\n" +
		`a=$(printf '%s' "$i" | sed -n 's/^{[^{]*"agent_type":"\([^"]*\)".*/\1/p' | tr -cd 'A-Za-z0-9_.:-' | cut -c1-64)` + "\n" +
		"[ -s " + m + " ] || printf '%s %s\\n' \"${s:-unstated}\" \"$a\" > " + m + "\n" +
		"printf '%s\\n' " + shellword.Quote(string(decision)) + "\n", nil
}

// claudeGuardMatcher is the PreToolUse entry that runs the guard on a call.
type claudeGuardMatcher struct {
	Matcher string              `json:"matcher"`
	Hooks   []claudeCommandHook `json:"hooks"`
}

// claudeCommandHook is one hook that runs a command: the bridge guard's, and
// the checkouts' plugin's ([writeClaudeCheckouts]).
type claudeCommandHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// claudeGuardHooks is the guard's PreToolUse entries: one per bridge, on every
// tool named inside its names, as a regular expression (Claude Code reads a
// matcher of other characters than letters, digits, `_` and `|` as one).
func claudeGuardHooks(bridges []string, paths Paths) []claudeGuardMatcher {
	var out []claudeGuardMatcher
	for _, name := range bridges {
		out = append(out, claudeGuardMatcher{
			Matcher: "^mcp__" + regexp.QuoteMeta(claudeToolSpelling(name)) + "__",
			Hooks: []claudeCommandHook{{Type: "command", Command: "sh " + shellword.Quote(paths.ClaudeBridgeGuard()),
				Timeout: int(ClaudeHookTimeout / time.Second)}},
		})
	}
	return out
}

// claudeGuardedSettings is a settings file's guarded copy: its pinned copy's
// settings, and its hooks beside the guard's, handed in its place on every
// release ([claudePinGate]). Hooks are never switched on: where the box's
// settings switch them off the run is refused ([claudeHooksOffGate]).
type claudeGuardedSettings struct {
	claudeSettings

	// Hooks maps an event to its entries: the guard's under PreToolUse, and
	// the notes' where the file is the notes'.
	Hooks map[string]any `json:"hooks"`
}

// writeClaudeGuard writes, for a run handed a bridge, the guard's script and
// the guarded copies of its settings: every run's, and the notes' where it
// reads notes.
func writeClaudeGuard(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths,
	pin map[string]string) error {
	bridges := crewletServers(req.MCPServers)
	if len(bridges) == 0 {
		return nil
	}
	script, err := claudeGuardScript(bridges, paths.ClaudeUnbridged())
	if err != nil {
		return fmt.Errorf("executor: rendering the run's bridge guard: %w", err)
	}
	if err = box.WriteFile(ctx, paths.ClaudeBridgeGuard(), []byte(script)); err != nil {
		return fmt.Errorf("executor: writing the run's bridge guard: %w", err)
	}
	guard := claudeGuardHooks(bridges, paths)
	err = writeClaudeSettings(ctx, box, paths.ClaudeSettingsGuarded(), claudeGuardedSettings{
		claudeSettings: newClaudeSettings(pin), Hooks: map[string]any{claudeEventToolCall: guard}})
	if err != nil || !req.Steer.Set() {
		return err
	}
	notes := claudeSteerSettings(req.Steer, pin)
	hooks := map[string]any{claudeEventToolCall: guard}
	for event, matchers := range notes.Hooks {
		hooks[event] = matchers
	}
	return writeClaudeSettings(ctx, box, paths.ClaudeNotesSettingsGuarded(),
		claudeGuardedSettings{claudeSettings: notes.claudeSettings, Hooks: hooks})
}

// claudeEventToolCall is the hook event that fires before a tool runs, in a
// subagent as in the main thread.
const claudeEventToolCall = "PreToolUse"

// claudeGuardLines is the script part around the harness of a run handed a
// bridge: before it, no marker an earlier run left; after it, where the guard
// refused a call, the [claudeUnbridgedEvent] line wrapping the stream's last
// line, null where that is no object. "" for a run with no bridge.
func claudeGuardLines(paths Paths) (before, after string) {
	m := shellword.Quote(paths.ClaudeUnbridged())
	before = "rm -f " + m + "\n"
	after = "if [ -s " + m + " ]; then gs=$(head -n 1 " + m + " | cut -d ' ' -f 1 | tr -cd 'A-Za-z0-9_-'); " +
		"ga=$(head -n 1 " + m + " | cut -d ' ' -f 2 | tr -cd 'A-Za-z0-9_.:-'); " +
		"gr=$(tail -n 1 " + shellword.Quote(paths.Stream()) + " 2> /dev/null); case $gr in '{'*'}') ;; *) gr=null ;; esac; " +
		`printf '{"type":"` + claudeUnbridgedEvent + `","source":"%s","agent":"%s","result":%s}\n' "$gs" "$ga" "$gr"; fi` + "\n"
	return before, after
}

// claudeUnbridgedFailure is how a run whose guard refused a call failed: the
// server's source and the agent as the hook read them, "" where unread.
func claudeUnbridgedFailure(source, agent string) string {
	who := "the main thread"
	if agent != "" {
		who = "the agent " + agent
	}
	return fmt.Sprintf("%s: Claude Code was to serve a call of %s, named inside them, from another MCP server "+
		"(its source %q), so the run's guard refused it; remove that server, most likely from the mcpServers "+
		"of an agent's definition under .claude/agents/ in the repository or the box home", claudeShadowed, who,
		firstNonBlank(source, "unstated"))
}

// claudeUnbridgedWire is a [claudeUnbridgedEvent] line: the hook's words and
// the harness's own result line, raw.
type claudeUnbridgedWire struct {
	Type   json.RawMessage `json:"type"`
	Source json.RawMessage `json:"source"`
	Agent  json.RawMessage `json:"agent"`
	Result json.RawMessage `json:"result"`
}

// parseUnbridged reads text as a [claudeUnbridgedEvent] line: the harness's
// result it wraps, its spend kept, failed in the guard's words before its own.
// False for any other line.
func parseUnbridged(text []byte) (sandbox.Result, bool) {
	var w claudeUnbridgedWire
	if wireAlias(text, &w) != nil || looseString(w.Type) != claudeUnbridgedEvent {
		return sandbox.Result{}, false
	}
	res := sandbox.Result{}
	if inner := []byte(strings.TrimSpace(string(w.Result))); len(inner) > 0 && inner[0] == '{' {
		if _, nested := parseUnbridged(inner); !nested {
			res = ClaudeCode{}.Parse(inner)
		}
	}
	why := claudeUnbridgedFailure(looseString(w.Source), looseString(w.Agent))
	if !res.Success && strings.TrimSpace(res.Error) != "" {
		why += "; " + res.Error
	}
	res.Success, res.Error, res.Unbridged = false, why, true
	return res, true
}

// claudeManagedGate is the script line that refuses, before the harness
// starts, a run handed servers in a box holding [claudeManagedMCP].
func claudeManagedGate() string {
	f := shellword.Quote(claudeManagedMCP)
	return "if [ -e " + f + " ] || [ -L " + f + " ]; then echo " + shellword.Quote("crewlet: this box holds "+
		claudeManagedMCP+", Claude Code's managed MCP configuration, which keeps every server of the run's own "+
		"from it, Crewlet's own among them (it refuses them, or drops them and starts without); remove the "+
		"file from the box image") + " >&2; exit 1; fi\n"
}
