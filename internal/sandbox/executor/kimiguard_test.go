package executor_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// kimiKAP is a stand-in of `kimi web` for the driver's guards: KAP's routes the
// driver calls, answered from $HOME/kap.json as Kimi Code 2.7.2 answered them
// for the planted file (measured per row): the servers of the home's mcp.json,
// each listing submit_work and report, inactive where `inactive` names them,
// and none where a home's `startup` cut their start and the run's entry sets
// no bound of its own (each listing recorded in $HOME/kap.tools); `config`
// and `plugins` as KAP lists them; and, once the
// brief is posted, `events` on the session's socket, then the session idle
// with `outcome`, its main agent's `turns` holding the brief's prompt where
// one says BRIEF; with `hold` the session never goes idle, so only the guard
// ends the run.
const kimiKAP = `import http from "node:http"
import crypto from "node:crypto"
import fs from "node:fs"
const kap = JSON.parse(fs.readFileSync(process.env.HOME + "/kap.json", "utf8"))
const mcpFile = process.env.KIMI_CODE_HOME + "/mcp.json"
const servers = JSON.parse(fs.readFileSync(mcpFile, "utf8")).mcpServers ?? {}
const sid = "session_0123456789abcdef"
let socket = null, brief = "", idle = false, sent = false
const waits = []
const frame = (text) => {
  const b = Buffer.from(text)
  const head = b.length < 126 ? Buffer.from([0x81, b.length]) : Buffer.from([0x81, 126, b.length >> 8, b.length & 255])
  return Buffer.concat([head, b])
}
const answer = (res, data) => { res.setHeader("content-type", "application/json"); res.end(JSON.stringify({ code: 0, msg: "success", data })) }
const settle = () => { if (!idle) return; for (const res of waits.splice(0)) answer(res, { session: { activity: "idle", last_turn_outcome: kap.outcome ?? "completed" }, pending_interactions: [] }) }
const flush = () => {
  if (socket === null || brief === "" || sent) return
  sent = true
  for (const e of kap.events ?? []) socket.write(frame(JSON.stringify(e)))
  idle = kap.hold !== true
  settle()
}
const server = http.createServer((req, res) => {
  let body = ""
  req.on("data", (c) => (body += c))
  req.on("end", () => {
    if (req.headers.authorization !== "Bearer t") { res.statusCode = 401; return res.end() }
    const url = new URL(req.url, "http://kap")
    const p = url.pathname
    if (p === "/api/v1/mcp/servers") return answer(res, Object.keys(servers).map((name) => ({ name, origin: mcpFile })))
    if (p === "/api/v1/sessions" && req.method === "POST") return answer(res, { id: sid, workspace_id: "wd_kap" })
    if (p === "/api/v1/tools") {
      const tools = [{ name: "Bash", source: "builtin", active: true }]
      for (const [name, entry] of Object.entries(servers)) {
        if (kap.startup !== undefined && entry.startupTimeoutMs === undefined) continue
        for (const t of ["submit_work", "report"]) {
          const tool = "mcp__" + name + "__" + t
          tools.push({ name: tool, source: "mcp", active: !(kap.inactive ?? []).includes(tool) })
        }
      }
      fs.writeFileSync(process.env.HOME + "/kap.tools", tools.map((t) => t.name).join("\n") + "\n")
      return answer(res, { tools })
    }
    if (p === "/api/v1/config") return answer(res, kap.config ?? {})
    if (p === "/api/v1/plugins") return answer(res, { plugins: kap.plugins ?? [] })
    if (p.startsWith("/api/v1/plugins/")) {
      const id = decodeURIComponent(p.slice("/api/v1/plugins/".length))
      return answer(res, { id, manifest_path: process.env.KIMI_CODE_HOME + "/plugins/managed/" + id + "/kimi.plugin.json" })
    }
    if (p === "/api/v1/sessions/" + sid + "/prompts" && req.method === "POST") {
      brief = JSON.parse(body).prompt_id
      answer(res, { prompt_id: brief, status: "running" })
      return flush()
    }
    if (p === "/api/v1/sessions/" + sid + "/prompts") return answer(res, { active: null, queued: [] })
    if (p === "/api/v1/sessions/" + sid + ":wait") { waits.push(res); return settle() }
    if (p === "/api/v1/sessions/" + sid + "/agents/main/turns") return answer(res, { turns: JSON.parse(JSON.stringify(kap.turns ?? []).replaceAll("BRIEF", brief)), has_more: false })
    if (p === "/api/v1/sessions/" + sid + ":cancel") return answer(res, {})
    if (p === "/api/v1/shutdown") { answer(res, {}); return res.on("finish", () => process.exit(0)) }
    res.statusCode = 404
    res.end()
  })
})
server.on("upgrade", (req, sock) => {
  const accept = crypto.createHash("sha1").update(req.headers["sec-websocket-key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64")
  sock.write("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n")
  sock.on("data", () => {})
  sock.on("error", () => {})
  socket = sock
  flush()
})
server.listen(0, "127.0.0.1", () => {
  const origin = "http://127.0.0.1:" + server.address().port
  process.stdout.write(JSON.stringify({ origin, ws_url: origin.replace("http", "ws") + "/api/v1/ws", token: "t" }) + "\n")
})
`

// kimiCall is a session's events for one call of the bridge's submit_work
// that failed with says, as 2.7.2 sends them: the call running (its item
// updated with its `fields`) where ran, else ended unrun, as a hook's veto
// ends it; then the turn's answer and its end.
func kimiCall(ran bool, says string) []map[string]any {
	call := map[string]any{"type": "tool_call", "item_id": "call_1", "step_index": 1, "name": "mcp__crewlet__submit_work"}
	events := []map[string]any{
		{"type": "turn.started", "turn": map[string]any{"turn_id": "t_1", "ordinal": 0, "kind": "agent", "status": "running"}},
		{"type": "item.started", "turn_id": "t_1", "item": call},
		{"type": "item.updated", "turn_id": "t_1", "item_id": "call_1", "set": map[string]any{"stage": "executing", "input": map[string]any{}}},
	}
	if ran {
		events = append(events, map[string]any{"type": "item.updated", "turn_id": "t_1", "item_id": "call_1", "set": map[string]any{"fields": map[string]any{}}})
	}
	ended := map[string]any{"status": "ended", "input": map[string]any{}, "outcome": "failed", "error": says}
	for k, v := range call {
		ended[k] = v
	}
	return append(events,
		map[string]any{"type": "item.ended", "turn_id": "t_1", "item": ended},
		map[string]any{"type": "item.ended", "turn_id": "t_1", "item": map[string]any{"type": "assistant_message",
			"item_id": "a_1", "status": "ended", "outcome": "completed", "text": "done"}},
		map[string]any{"type": "turn.ended", "turn": map[string]any{"turn_id": "t_1", "ordinal": 0, "kind": "agent",
			"status": "ended", "outcome": "completed", "steps": []map[string]any{{"index": 1, "outcome": "completed",
				"finish_reason": "end_turn", "usage": map[string]any{"input_other": 1, "output": 1}}}}})
}

// kimiBlockedBrief is the brief's turn ended `blocked` before any step, as
// 2.7.2 records a UserPromptSubmit hook's block: the brief, then the hook's
// words as the turn's message.
var kimiBlockedBrief = []map[string]any{{"turn_id": "t_1", "outcome": "failed", "end_reason": "blocked",
	"steps": []any{}, "items": []map[string]any{
		{"type": "user_message", "item_id": "BRIEF", "text": "go"},
		{"type": "assistant_message", "item_id": "t_1#1",
			"text": "<hook_result hook_event=\"UserPromptSubmit\">\nprompt held\n</hook_result>"}}}}

// kimiHook is a [[hooks]] entry as KAP's config lists it.
func kimiHook(event, matcher string) map[string]any {
	return map[string]any{"event": event, "matcher": matcher, "command": "exit 2"}
}

// A HOME CANNOT SWITCH KIMI CODE'S BRIDGE TOOLS OFF UNSEEN. Kimi Code 2.7.2
// reads `[tools]`, `[mcp]` and hooks from the home's config.toml and plugins
// alone, and no layer the run writes undoes them but the start bound, which
// the run's servers carry. A session listing a tool of the run's inactive is
// refused before the brief, naming the `[tools]` entries; a call of one Kimi
// ended unrun, or a brief a blocked turn kept from the model, ends the run
// naming each matching hook Kimi lists. Other tools, a hook for another tool
// or one that let the call run, and the repository's config.toml pass.
// Mutations, each red: drop the tools check; drop the start bound; take a
// vetoed call as run; take every call as unrun; skip the plugins; take no
// matcher as matching none; skip the turns check; drop the refusal line.
func TestKimiCodesBridgeAndHooksCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	node := prereqtest.OnPath(t, "node", "a Kimi run's driver and its stand-in server run under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
	const toml = ".kimi-code/config.toml"
	const config = "/.kimi-code/config.toml: "
	const plugin = ".kimi-code/plugins/managed/homeplug/kimi.plugin.json"
	homeplug := []map[string]any{{"id": "homeplug", "enabled": true, "hook_count": 1}}
	for name, tc := range map[string]struct {
		kap map[string]any
		switchOffCase
	}{
		"nothing switches it off": {kap: map[string]any{"events": kimiCall(true, "ok")}},
		"lists of other tools, a hook for another tool, and the repository's own config.toml": {
			kap: map[string]any{"config": map[string]any{"tools": map[string]any{"disabled": []string{"mcp__other__*", "Grep"}},
				"hooks": []any{kimiHook("PreToolUse", "mcp__other__.*")}}, "events": kimiCall(true, "ok")},
			switchOffCase: switchOffCase{plant: map[string]string{
				toml:                               "[tools]\ndisabled = [\"mcp__other__*\", \"Grep\"]\n",
				"workspace/.kimi-code/config.toml": "[tools]\ndisabled = [\"mcp__crewlet__*\"]\n"}}},
		"the home's tool list keeps the bridge's": {
			kap: map[string]any{"config": map[string]any{"tools": map[string]any{"enabled": []string{"Bash", "mcp__crewlet__*"}}},
				"events": kimiCall(true, "ok")},
			switchOffCase: switchOffCase{plant: map[string]string{toml: "[tools]\nenabled = [\"Bash\", \"mcp__crewlet__*\"]\n"}}},
		"the home cuts every server's start, which the run's own bound undoes": {
			kap: map[string]any{"startup": 1, "config": map[string]any{"mcp": map[string]any{"startupTimeoutMs": 1}},
				"events": kimiCall(true, "ok")},
			switchOffCase: switchOffCase{plant: map[string]string{toml: "[mcp]\nstartup_timeout_ms = 1\n"}}},
		"a hook lets the call run and the bridge itself answers an error": {
			kap: map[string]any{"config": map[string]any{"hooks": []any{kimiHook("PreToolUse", "")}},
				"events": kimiCall(true, "bridge says no")},
			switchOffCase: switchOffCase{plant: map[string]string{toml: "[[hooks]]\nevent = \"PreToolUse\"\ncommand = \"true\"\n"}}},
		"the home disables the bridge's tools": {
			kap: map[string]any{"inactive": []string{"mcp__crewlet__submit_work", "mcp__crewlet__report"},
				"config": map[string]any{"tools": map[string]any{"disabled": []string{"mcp__crewlet__*", "Grep"}}}},
			switchOffCase: switchOffCase{plant: map[string]string{toml: "[tools]\ndisabled = [\"mcp__crewlet__*\", \"Grep\"]\n"},
				fails: []string{"none of the run's own tools mcp__crewlet__submit_work, mcp__crewlet__report",
					config + `its [tools] disabled lists "mcp__crewlet__*"`}}},
		"the home's tool list leaves one of the bridge's tools out": {
			kap: map[string]any{"inactive": []string{"mcp__crewlet__report"},
				"config": map[string]any{"tools": map[string]any{"enabled": []string{"Bash", "mcp__crewlet__submit_work"}}}},
			switchOffCase: switchOffCase{plant: map[string]string{toml: "[tools]\nenabled = [\"Bash\", \"mcp__crewlet__submit_work\"]\n"},
				fails: []string{"none of the run's own tools mcp__crewlet__report:",
					config + `its [tools] enabled lists only "Bash", "mcp__crewlet__submit_work"`}}},
		"a home hook keeps the bridge's tool from running": {
			kap: map[string]any{"hold": true, "config": map[string]any{"hooks": []any{kimiHook("PreToolUse", "mcp__crewlet__.*"),
				kimiHook("PreToolUse", "mcp__other__.*")}}, "events": kimiCall(false, "held by a hook")},
			switchOffCase: switchOffCase{plant: map[string]string{toml: "[[hooks]]\nevent = \"PreToolUse\"\nmatcher = \"mcp__crewlet__.*\"\ncommand = \"exit 2\"\n"},
				fails: []string{"Kimi did not run the run's own tool mcp__crewlet__submit_work: a PreToolUse hook blocked it (held by a hook)",
					config + `its [[hooks]] entry event PreToolUse, matcher "mcp__crewlet__.*", command "exit 2"`}}},
		"a plugin's hook keeps the bridge's tool from running": {
			kap: map[string]any{"hold": true, "plugins": homeplug, "events": kimiCall(false, "held by a plugin")},
			switchOffCase: switchOffCase{plant: map[string]string{plugin: `{"name":"homeplug","hooks":[{"event":"PreToolUse","command":"exit 2"}]}`},
				fails: []string{"a PreToolUse hook blocked it (held by a plugin)",
					"/" + plugin + ` (the plugin homeplug): its hooks entry event PreToolUse, matcher "", command "exit 2"`}}},
		"a home hook keeps the brief from the model": {
			kap: map[string]any{"outcome": "failed", "turns": kimiBlockedBrief,
				"config": map[string]any{"hooks": []any{kimiHook("UserPromptSubmit", "^go$"), kimiHook("UserPromptSubmit", "^stop$")}}},
			switchOffCase: switchOffCase{plant: map[string]string{toml: "[[hooks]]\nevent = \"UserPromptSubmit\"\nmatcher = \"^go$\"\ncommand = \"exit 2\"\n"},
				fails: []string{"Kimi kept the run's brief from the model: a UserPromptSubmit hook blocked it (prompt held)",
					config + `its [[hooks]] entry event UserPromptSubmit, matcher "^go$"`}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewKimi(), map[string]string{
				"node": "#!/bin/sh\nexec " + shellword.Quote(node) + " \"$@\"\n",
				"kimi": "#!/bin/sh\ncase \"$1\" in --version) echo 2.7.3; exit 0;; web) exec " + shellword.Quote(node) +
					" \"$HOME/kap.mjs\";; esac\nexit 1\n",
			})
			answers, err := json.Marshal(tc.kap)
			if err != nil {
				t.Fatal(err)
			}
			plant(t, box, map[string]string{"kap.mjs": kimiKAP, "kap.json": string(answers)})
			plant(t, box, tc.plant)
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge})
			checkSwitchOff(t, box, res, tc.switchOffCase)
			if len(tc.fails) > 0 {
				return
			}
			// THE BRIDGE'S TOOLS WERE THE SESSION'S, the home's start bound included.
			listed, err := os.ReadFile(filepath.Join(box.Home(), "kap.tools"))
			if err != nil || !strings.Contains(string(listed), "mcp__crewlet__submit_work\n") {
				t.Errorf("the session listed no tool of the bridge's (%v):\n%s", err, listed)
			}
		})
	}
}
