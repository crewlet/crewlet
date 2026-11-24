package executor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// piSettingsStandIn is a `pi`, run under node, that loads the run's extension
// and answers the calls its guard reads as pi 1.6.0 does (measured): the
// settings, the agent directory's settings.json merged under the working
// directory's .pi/settings.json, a file that is not JSON read as none; the
// active tools, pi's defaults with `defaultTools`' plain names or `+`/`-`
// entries applied, and a `direct` tool of each registered server. It plays the
// first turn and prints pi's JSON stream: the extension's entries, then a
// finished answer or, once the extension aborts, an aborted end.
const piSettingsStandIn = `import { readFileSync } from "node:fs"
import { pathToFileURL } from "node:url"
const args = process.argv.slice(2)
if (args[0] === "--version") { console.log("1.6.0"); process.exit(0) }
const out = (o) => process.stdout.write(JSON.stringify(o) + "\n")
const isObject = (v) => v !== null && typeof v === "object" && !Array.isArray(v)
const read = (f) => { try { const v = JSON.parse(readFileSync(f, "utf8")); return isObject(v) ? v : {} } catch { return {} } }
const merge = (a, b) => {
  const o = { ...a }
  for (const [k, v] of Object.entries(b)) o[k] = isObject(v) && isObject(o[k]) ? merge(o[k], v) : v
  return o
}
const settings = merge(read(process.env.PI_CODING_AGENT_DIR + "/settings.json"), read(".pi/settings.json"))
const listed = Array.isArray(settings.defaultTools) ? settings.defaultTools : []
const plain = listed.filter((n) => !/^[+-]/.test(n))
let active = plain.length > 0 ? plain : ["read", "bash", "edit", "write"]
for (const n of listed) {
  if (n.startsWith("+")) active.push(n.slice(1))
  if (n.startsWith("-")) active = active.filter((m) => m !== n.slice(1))
}
const handlers = new Map()
const pi = {
  on: (name, h) => handlers.set(name, [...(handlers.get(name) ?? []), h]),
  registerMcpServer: (name) => active.push("mcp__" + name.replace(/-/g, "_") + "__submit_work"),
  appendEntry: (customType, data) => out({ type: "entry_appended", entry: { type: "custom", customType, data } }),
  getAllTools: () => active.map((name) => ({ name })),
  getActiveTools: () => [...active],
  getSettings: () => structuredClone(settings),
}
let aborted = false
const ctx = { cwd: process.cwd(), abort: () => { aborted = true }, getContextUsage: () => undefined }
const fire = async (name, event) => { for (const h of handlers.get(name) ?? []) await h(event, ctx) }
out({ type: "session", version: 3, id: "s", timestamp: new Date().toISOString(), cwd: process.cwd() })
for (const [i, a] of args.entries()) {
  if (args[i - 1] === "-e" && !a.startsWith("builtin:")) (await import(pathToFileURL(a).href)).default(pi)
}
await fire("before_agent_start", { systemPromptOptions: { sections: {} } })
await fire("agent_start", {})
await fire("turn_start", {})
if (!aborted) {
  out({ type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "done" }], stopReason: "stop" } })
}
await fire("session_shutdown", {})
out({ type: "agent_settled", aborted })
process.exit(0)
`

// A HOME OR A REPOSITORY CANNOT HIDE PI'S BRIDGE TOOLS FROM THE MODEL UNSEEN.
// Pi 1.6.0's codemode, active with `codemode.mode` "only" in the merged
// settings (the repository's .pi/settings.json over the agent directory's),
// declares itself alone, so the bridge's tools leave every request (measured),
// and no layer of the launch undoes it. So the run is stopped before its
// request, naming the file that set the mode, the key and the entry; it runs
// where codemode is inactive, where the winning mode is another, where
// codemode only lists no tool inline, and where the run has no server.
// Mutations, each red: drop the check; drop the codemode-active condition;
// name the first file setting the mode rather than the last; name no file.
func TestPisBridgeToolsCannotBeHiddenFromTheModelUnseen(t *testing.T) {
	t.Parallel()
	node := prereqtest.OnPath(t, "node", "the Pi stand-in loads the run's extension under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
	const only = `{"defaultTools":["+codemode"],"codemode":{"mode":"only"}}`
	const home, repo = ".pi/agent/settings.json", "workspace/.pi/settings.json"
	hidden := func(file string) []string {
		return []string{"the seat's tools never reached the model: ", `codemode.mode "only" in /`, file,
			"while codemode is active"}
	}
	for name, tc := range map[string]struct {
		noServer bool
		switchOffCase
	}{
		"nothing is planted": {},
		"the home's settings hide the bridge's tools behind codemode": {switchOffCase: switchOffCase{
			plant: map[string]string{home: only}, fails: hidden("/.pi/agent/settings.json")}},
		"the repository's settings hide the bridge's tools behind codemode": {switchOffCase: switchOffCase{
			plant: map[string]string{repo: only}, fails: hidden("/workspace/.pi/settings.json")}},
		"the home turns codemode on and the repository sets its mode": {switchOffCase: switchOffCase{
			plant: map[string]string{home: `{"defaultTools":["+codemode"]}`, repo: `{"codemode":{"mode":"only"}}`},
			fails: hidden("/workspace/.pi/settings.json")}},
		"both files set the mode, the repository's winning": {switchOffCase: switchOffCase{
			plant: map[string]string{home: only, repo: `{"codemode":{"mode":"only"}}`},
			fails: hidden("/workspace/.pi/settings.json")}},
		"the repository's mode overrides the home's": {switchOffCase: switchOffCase{
			plant: map[string]string{home: only, repo: `{"codemode":{"mode":"on"}}`}}},
		"codemode is not active": {switchOffCase: switchOffCase{
			plant: map[string]string{home: `{"codemode":{"mode":"only"}}`}}},
		"codemode lists no tool inline in its own mode": {switchOffCase: switchOffCase{
			plant: map[string]string{repo: `{"defaultTools":["+codemode"],"codemode":{"inlineBudget":0}}`}}},
		"a run without servers": {noServer: true, switchOffCase: switchOffCase{
			plant: map[string]string{home: only}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewPi(), nil)
			bin := executor.PathsFor(box).BinDir()
			writeBoxStandIn(t, box, bin+"/pi-standin.mjs", piSettingsStandIn)
			writeBoxStandIn(t, box, bin+"/pi", "#!/bin/sh\nexec "+shellword.Quote(node)+" "+
				shellword.Quote(bin+"/pi-standin.mjs")+" \"$@\"\n")
			plant(t, box, tc.plant)
			req := sandbox.RunRequest{Brief: "go", MCPServers: map[string]sandbox.MCPServer{
				"crewlet": {Transport: sandbox.TransportHTTP, URL: codexBridgeURL, AlwaysLoaded: true}}}
			if tc.noServer {
				req.MCPServers = nil
			}
			_, res := runToEnd(t, runner, box, req)
			checkSwitchOff(t, box, res, tc.switchOffCase)
		})
	}
}

// A HOME OR A REPOSITORY CANNOT KEEP A PI RUN'S SKILLS FROM THE MODEL UNSEEN.
// Pi 1.6.0 lists skills only while read or bash is among its tools, and takes
// its tools from the merged `defaultTools` (measured: neither dropped the whole
// listing, the run's own skills with it; either alone kept it; a repository's
// plain list replaced the home's). So a run handing skills is stopped before
// its request where neither is left, naming each file that sets the key and
// its value; it runs where one is kept, and a run with no skills of its own
// runs whatever the key says.
// Mutations, each red: drop the check; read "read and bash" in place of "read
// or bash"; name no file.
func TestAPiRunsSkillsCannotBeUnlistedUnseen(t *testing.T) {
	t.Parallel()
	node := prereqtest.OnPath(t, "node", "the Pi stand-in loads the run's extension under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
	const home, repo = ".pi/agent/settings.json", "workspace/.pi/settings.json"
	for name, tc := range map[string]struct {
		noSkills bool
		switchOffCase
	}{
		"nothing is planted": {},
		"the home keeps bash alone": {switchOffCase: switchOffCase{
			plant: map[string]string{home: `{"defaultTools":["bash","edit"]}`}}},
		"the repository takes read away and keeps bash": {switchOffCase: switchOffCase{
			plant: map[string]string{repo: `{"defaultTools":["-read"]}`}}},
		"the home leaves both out": {switchOffCase: switchOffCase{
			plant: map[string]string{home: `{"defaultTools":["edit","write"]}`},
			fails: []string{"the seat's skills never reached the model: /", "/.pi/agent/settings.json sets defaultTools to " +
				`["edit","write"]`, "leaves both read and bash out"}}},
		"the repository takes both away": {switchOffCase: switchOffCase{
			plant: map[string]string{repo: `{"defaultTools":["-read","-bash"]}`},
			fails: []string{"/workspace/.pi/settings.json sets defaultTools to " + `["-read","-bash"]`,
				"leaves both read and bash out"}}},
		"a run with no skills of its own": {noSkills: true, switchOffCase: switchOffCase{
			plant: map[string]string{home: `{"defaultTools":["edit"]}`}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewPi(), nil)
			bin := executor.PathsFor(box).BinDir()
			writeBoxStandIn(t, box, bin+"/pi-standin.mjs", piSettingsStandIn)
			writeBoxStandIn(t, box, bin+"/pi", "#!/bin/sh\nexec "+shellword.Quote(node)+" "+
				shellword.Quote(bin+"/pi-standin.mjs")+" \"$@\"\n")
			plant(t, box, tc.plant)
			req := sandbox.RunRequest{Brief: "go", Skills: handedSkills(), SkillLoads: skillWiring(),
				MCPServers: map[string]sandbox.MCPServer{
					"crewlet": {Transport: sandbox.TransportHTTP, URL: codexBridgeURL, AlwaysLoaded: true}}}
			if tc.noSkills {
				req.Skills, req.SkillLoads = nil, sandbox.SkillWiring{}
			}
			_, res := runToEnd(t, runner, box, req)
			checkSwitchOff(t, box, res, tc.switchOffCase)
		})
	}
}
