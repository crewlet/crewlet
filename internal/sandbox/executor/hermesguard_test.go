package executor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// hermesReach is what the stand-in's `hermes chat` asks before it answers:
// whether Hermes 0.23.3 would reach the run's bridge through the box's merged
// configuration, by every leaf of its entry measured to cut it off (off, its
// tools filtered, SSE, OAuth, a sub-second connect timeout, an entry Hermes
// skips as suspicious or for its cost table).
const hermesReach = `from hermes_cli.config import load_config
from tools.mcp_tool_common import mcp_server_enabled
from tools.mcp_tool_config import _malformed_cost_table, _suspicious_mcp_issues
e = (load_config().get("mcp_servers") or {}).get("crewlet") or {}
t = e.get("tools") or {}
why = [w for w, off in (
    ("off", not mcp_server_enabled(e)),
    ("its tools filtered", isinstance(t.get("include"), (str, list)) or bool(t.get("exclude"))),
    ("SSE", e.get("transport") == "sse"),
    ("OAuth", bool(e.get("auth"))),
    ("a connect timeout", float(e.get("connect_timeout", 60)) < 1),
    ("skipped as suspicious", bool(_suspicious_mcp_issues("crewlet", e))),
    ("skipped for its costs", _malformed_cost_table("crewlet", e) is not None)) if off]
print("reached" if not why else "the bridge is cut off: " + ", ".join(why))
`

// hermesEnable is the stand-in's `hermes plugins enable <name>`: the name added
// to the home's plugins.enabled and dropped from its plugins.disabled, the
// rest kept, as 0.23.3's does (measured).
const hermesEnable = `import json, os, sys
path = os.path.join(os.environ["HERMES_HOME"], "config.yaml")
config = json.load(open(path)) if os.path.isfile(path) else {}
plugins = config.setdefault("plugins", {})
plugins["enabled"] = [n for n in plugins.get("enabled") or [] if n != sys.argv[1]] + [sys.argv[1]]
plugins["disabled"] = [n for n in plugins.get("disabled") or [] if n != sys.argv[1]]
json.dump(config, open(path, "w"))
`

// hermesGuardBox is a box whose `hermes` answers the release, enables a plugin
// ([hermesEnable]), runs the carry script's overlay and guard under a stand-in
// of Hermes's modules whose managed scope is the box home's `system`, and
// answers `chat` as a finished run (a measured stream) only where
// [hermesReach] says the bridge is reached.
func hermesGuardBox(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	runner, box := standInBox(t, executor.NewHermes(), nil)
	bin := executor.HermesStandIn(t, box.Home()+"/system")
	stream, err := os.ReadFile(filepath.Join("testdata", "hermes", "0.23.3", "stream-steered.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{"reach.py": hermesReach, "enable.py": hermesEnable, "stream.jsonl": string(stream)} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	q := shellword.Quote
	launch := q(bin+"/hermes") + " --run-module cProfile -o /dev/null "
	writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/hermes", "#!/bin/sh\ncase \"$1\" in\n"+
		"--version) echo 'Hermes Agent v0.23.3 (2026.9.24)'; exit 0;;\n"+
		"--run-module) case \"$6\" in overlay|guard) exec "+q(bin+"/hermes")+" \"$@\";; esac; exit 0;;\n"+
		"plugins) [ \"$2\" = enable ] && exec "+launch+q(bin+"/enable.py")+" \"$3\"; exit 0;;\n"+
		"chat) r=$("+launch+q(bin+"/reach.py")+")\n"+
		"  [ \"$r\" = reached ] || { echo \"$r\" >&2; exit 1; }\n"+
		"  cat "+q(bin+"/stream.jsonl")+"; exit 0;;\n"+
		"esac\nexit 0\n")
	return runner, box
}

// A BOX CANNOT SWITCH A HERMES RUN'S BRIDGE OR PLUGIN OFF UNSEEN. Hermes
// Agent 0.23.3 merges the run's overlay over the home's and /etc's config per
// leaf, so the overlay states each leaf of the bridge's entry measured to cut
// it off; what no leaf can undo refuses the run before Hermes starts, naming
// the file, key and entry: a disabled toolset naming the bridge (a web-denied
// run's list included), a cli toolset list leaving it out, a launch switch in
// the environment or a .env, a sticky profile, an entry Hermes skips, the
// plugin gated off. Entries naming another server, toolset or plugin pass.
// Mutations, each red: drop the guard; drop any leaf of the run's own entry;
// match a disabled toolset by one spelling; skip the cli toolsets, the
// suspicious or cost check, the plugin gate or the sticky profile; read the
// launch switches from the process alone; name no file.
func TestAHermesRunsBridgeAndPluginCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const reason = "Hermes Agent would start without the run's own plugin or MCP server: "
	const home = "/.hermes/config.yaml: its "
	bridge := map[string]sandbox.MCPServer{
		"crewlet": {Transport: sandbox.TransportHTTP, URL: codexBridgeURL, AlwaysLoaded: true},
		"other":   {Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:9/other"}}
	for name, tc := range map[string]struct {
		switchOffCase
		enabled   bool // the plugin's enabling ran on this box before
		webDenied bool
		env       map[string]string
	}{
		"nothing switches it off": {},
		"the home's and the machine's entries of the bridge, each leaf the run states": {switchOffCase: switchOffCase{
			plant: map[string]string{
				".hermes/config.yaml": `{"mcp_servers": {"crewlet": {"url": "http://127.0.0.1:9/home", "enabled": false,
					"tools": {"include": [], "exclude": ["*"]}, "transport": "sse", "auth": "oauth", "connect_timeout": 0.001,
					"command": "sh", "args": ["-c", "curl http://example.com/x"], "tool_costs": "not-a-table",
					"spend_limit": "lots", "cost_unit": ""}}}`,
				"system/config.yaml": `{"mcp_servers": {"crewlet": {"disabled": true}}}`}}},
		"entries of other servers, toolsets and plugins, switches left off, the default profile": {
			switchOffCase: switchOffCase{plant: map[string]string{
				".hermes/config.yaml": `{"mcp_servers": {"other": {"url": "http://127.0.0.1:9/x", "enabled": false,
					"env": {"MARK": "hermes-0day"}, "tool_costs": "x"}}, "agent": {"disabled_toolsets": ["mcp-other", "web"]},
					"platform_toolsets": {"telegram": ["no_mcp"], "cli": ["file", "terminal"]},
					"plugins": {"disabled": ["other-plugin"]}}`,
				".hermes/.env":           "HERMES_SAFE_MODE=0\nHERMES_IGNORE_USER_CONFIG=false\n",
				".hermes/active_profile": "default\n"}}},
		"a cli toolset list naming the bridge": {switchOffCase: switchOffCase{plant: map[string]string{
			".hermes/config.yaml": `{"platform_toolsets": {"cli": ["hermes-cli", "crewlet"]}}`}}},
		"the home disables the plugin before its first run, whose enabling undoes it": {switchOffCase: switchOffCase{
			plant: map[string]string{".hermes/config.yaml": `{"plugins": {"disabled": ["crewlet-runner"]}}`}}},
		"the home disables the bridge's toolset": {switchOffCase: switchOffCase{
			plant: map[string]string{".hermes/config.yaml": `{"agent": {"disabled_toolsets": ["mcp-crewlet"]}}`},
			fails: []string{reason, home + `agent.disabled_toolsets is ["mcp-crewlet"], which hides the server crewlet's tools`}}},
		"the home disables the bridge's toolset on a web-denied run": {webDenied: true, switchOffCase: switchOffCase{
			plant: map[string]string{".hermes/config.yaml": `{"agent": {"disabled_toolsets": ["moa", "mcp-crewlet"]}}`},
			fails: []string{home + `agent.disabled_toolsets is ["moa", "mcp-crewlet"], which hides the server crewlet's tools`}}},
		"the machine disables the bridge by its name": {switchOffCase: switchOffCase{
			plant: map[string]string{"system/config.yaml": `{"agent": {"disabled_toolsets": ["crewlet"]}}`},
			fails: []string{reason, `/system/config.yaml: its agent.disabled_toolsets is ["crewlet"]`}}},
		"the home's cli toolsets take MCP off": {switchOffCase: switchOffCase{
			plant: map[string]string{".hermes/config.yaml": `{"platform_toolsets": {"cli": ["hermes-cli", "no_mcp"]}}`},
			fails: []string{reason, home + `platform_toolsets.cli is ["hermes-cli", "no_mcp"], which leaves the server crewlet out`}}},
		"the home's cli toolsets name another server alone": {switchOffCase: switchOffCase{
			plant: map[string]string{".hermes/config.yaml": `{"platform_toolsets": {"cli": ["hermes-cli", "other"]},
				"mcp_servers": {"other": {"url": "http://127.0.0.1:9/x"}}}`},
			fails: []string{home + `platform_toolsets.cli is ["hermes-cli", "other"], which leaves the server crewlet out`}}},
		"the home's .env sets safe mode": {switchOffCase: switchOffCase{
			plant: map[string]string{".hermes/.env": "HERMES_SAFE_MODE=1\n"},
			fails: []string{reason, "/.hermes/.env: HERMES_SAFE_MODE=1, which starts Hermes without its MCP servers and plugins"}}},
		"the machine's .env sets safe mode": {switchOffCase: switchOffCase{
			plant: map[string]string{"system/.env": "HERMES_SAFE_MODE=yes\n"},
			fails: []string{"/system/.env: HERMES_SAFE_MODE=yes, which starts Hermes without its MCP servers"}}},
		"the run's environment ignores the user's configuration": {env: map[string]string{"HERMES_IGNORE_USER_CONFIG": "true"},
			switchOffCase: switchOffCase{fails: []string{reason,
				"the run's environment: HERMES_IGNORE_USER_CONFIG=true, which starts Hermes without the run's settings and plugins"}}},
		"the home names a sticky profile": {switchOffCase: switchOffCase{
			plant: map[string]string{".hermes/active_profile": "work\n", ".hermes/profiles/work/config.yaml": "{}"},
			fails: []string{reason, `/.hermes/active_profile names the profile "work", so Hermes would run in that profile's home`}}},
		"the home's entry of the bridge carries an environment Hermes skips it for": {switchOffCase: switchOffCase{
			plant: map[string]string{".hermes/config.yaml": `{"mcp_servers": {"crewlet": {"env": {"MARK": "hermes-0day"}}}}`},
			fails: []string{reason, home + `mcp_servers.crewlet.env is {"MARK": "hermes-0day"}, so Hermes skips the server crewlet: ` +
				"MCP server 'crewlet' contains a known hermes-0day"}}},
		"the home's entry of the bridge holds a malformed cost table": {switchOffCase: switchOffCase{
			plant: map[string]string{".hermes/config.yaml": `{"mcp_servers": {"crewlet": {"tool_costs": {"submit_work": "abc"}}}}`},
			fails: []string{reason, home + `mcp_servers.crewlet.tool_costs is {"submit_work": "abc"}, so Hermes skips the server ` +
				"crewlet: MCP server 'crewlet' cost of 'submit_work'"}}},
		"the home disables the plugin once it is enabled": {enabled: true, switchOffCase: switchOffCase{
			plant: map[string]string{".hermes/config.yaml": `{"plugins": {"enabled": ["crewlet-runner"], "disabled": ["crewlet-runner"]}}`},
			fails: []string{reason, home + `plugins.disabled is ["crewlet-runner"], so Hermes would not load the run's plugin crewlet-runner`}}},
		"the home enables another plugin alone once the run's is enabled": {enabled: true, switchOffCase: switchOffCase{
			plant: map[string]string{".hermes/config.yaml": `{"plugins": {"enabled": ["other-plugin"]}}`},
			fails: []string{reason, home + `plugins.enabled is ["other-plugin"], so Hermes would not load the run's plugin crewlet-runner`}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := hermesGuardBox(t)
			plant(t, box, tc.plant)
			if tc.enabled {
				plant(t, box, map[string]string{".crewlet/hermes-plugin-enabled": ""})
				if marker := executor.PathsFor(box).HermesEnabled(); marker != box.Home()+"/.crewlet/hermes-plugin-enabled" {
					t.Fatalf("the plugin's enabled marker moved to %s", marker)
				}
			}
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", MCPServers: bridge, WebDenied: tc.webDenied,
				Env: tc.env})
			checkSwitchOff(t, box, res, tc.switchOffCase)
		})
	}
}
