package executor_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// copilotGuardStandIn is a `copilot` that answers the guard as Copilot 1.0.114
// does, from the row's planted answers: `config --list` prints
// $HOME/answer.settings (its stdout and stderr, as the guard reads them
// together) and keeps the settings it was asked about; `mcp list` prints
// $HOME/answer.mcp on stderr. As the run it records its start and finishes.
const copilotGuardStandIn = `#!/bin/sh
case "$1" in --version) echo 'GitHub Copilot CLI 1.0.114.'; exit 0;; esac
case "$1 $2" in
"config --list") cp "$COPILOT_HOME/settings.json" "$HOME/asked.settings"; cat "$HOME/answer.settings" 2> /dev/null; exit 0;;
"mcp list") printf '%s\n' "$COPILOT_HOME" > "$HOME/asked.home"; cat "$HOME/answer.mcp" >&2 2> /dev/null; exit 0;;
esac
s= p=; for a; do [ "$p" = --session-id ] && s=$a; p=$a; done
cat > /dev/null; : > "$HOME/started"
printf '{"type":"session.start","data":{"sessionId":"%s"}}\n' "$s"
echo '{"type":"assistant.message","data":{"messageId":"m1","content":"done","toolRequests":[]}}'
printf '{"type":"result","sessionId":"%s","exitCode":0}\n' "$s"
`

// A HOME OR A MACHINE'S POLICY CANNOT SWITCH COPILOT'S BRIDGE OR NOTES
// EXTENSION OFF UNSEEN (1.0.114 ranks nothing of the run's above them). Before
// anything is written, a run is refused naming the file, the key and the entry
// where its user settings as Copilot lists them switch the extension or --yolo
// off or are ignored whole, Copilot rejects the home's mcp-config.json, or the
// managed policy blocks the bridge or the extension or is rejected whole; an
// entry naming another, a case or path Copilot does not match, a check the run
// has no use for and allowManagedHooksOnly start it. Mutations, each red: drop
// the guard; ask without the run's key; check what the run lacks; drop any
// check; fold a name's case or a URL's path; read an allow list's names beside
// its URLs; name no entry.
func TestCopilotsBridgeAndExtensionCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const (
		policy   = "/policy/managed-settings.json"
		settings = "/.copilot/settings.json"
		closed   = "and then it loads no MCP server and no extension"
		dropped  = "Warning: Ignoring values from settings.json because it could not be loaded. Check the file for invalid settings.\n"
	)
	for name, tc := range map[string]struct {
		home, settings, mcp, policy string
		linked, notOwned            bool
		bridgeOnly, notesOnly       bool
		switchOffCase
	}{
		"nothing switches either off": {settings: "experimental=true\n"},
		"entries naming others, and values Copilot keeps": {
			home:     `{"model":"m","extensions":{"mode":"load_only"}}`,
			settings: "experimental=true\nextensions.mode=load_only\nextensions.disabledExtensions=user:other\nextensions.disabledExtensions=crewlet-notes\nmodel=m\n",
			mcp:      `Warning: MCP config "HOME/.copilot/mcp-config.json": Skipping invalid MCP server "bad": server config must be an object` + "\n" + `Warning: skipping workspace MCP config "HOME/workspace/.mcp.json" because it is malformed: mcpServers: Expected object` + "\n",
			policy: `{"deniedMcpServers":[{"serverName":"other"},{"serverName":"Crewlet"},{"serverUrl":"http://127.0.0.1:9/MCP/*"}],
				"allowedMcpServers":[{"serverUrl":"HTTP://127.0.0.1:9/*"},{"serverName":"other"}],"allowManagedHooksOnly":true,
				"strictPluginOnlyCustomization":["skills","hooks"],"permissions":{"deny":["Shell(rm)","shell(git push)"]}}`},
		"the extension's switches on a run without it": {bridgeOnly: true,
			settings: dropped + "extensions.mode=disabled\nextensions.disabledExtensions=user:crewlet-notes\n",
			policy:   `{"strictPluginOnlyCustomization":["extensions"]}`},
		"the bridge's switches on a run without it": {notesOnly: true,
			mcp:    "Error: Invalid MCP config at HOME/.copilot/mcp-config.json: mcpServers: Required\n",
			policy: `{"deniedMcpServers":[{"serverName":"crewlet"}],"allowedMcpServers":[],"permissions":{"deny":["Mcp(crewlet)"]}}`},
		"the home switches extensions off": {
			home:          `{"extensions":{"mode":"disabled"}}`,
			settings:      "experimental=true\nextensions.mode=disabled\n",
			switchOffCase: switchOffCase{fails: []string{settings + ` sets extensions.mode to "disabled"`}}},
		"the home disables the run's extension": {settings: "extensions.disabledExtensions=user:other\nextensions.disabledExtensions=user:crewlet-notes\n",
			switchOffCase: switchOffCase{fails: []string{settings + " lists user:crewlet-notes, the run's notes extension, in extensions.disabledExtensions"}}},
		"Copilot ignores the home's settings whole": {
			home:          `{"extensions":{"mode":"Disabled"}}`,
			settings:      dropped,
			switchOffCase: switchOffCase{fails: []string{settings + " holds a value Copilot rejects", "Ignoring values from settings.json"}}},
		"the home turns --yolo off": {bridgeOnly: true, settings: "permissions.disableBypassPermissionsMode=\n",
			switchOffCase: switchOffCase{fails: []string{settings + ` sets permissions.disableBypassPermissionsMode to ""`, "--yolo"}}},
		"Copilot rejects the home's MCP file": {bridgeOnly: true,
			mcp:           "Error: Invalid MCP config at HOME/.copilot/mcp-config.json: configuration must be an object\n",
			switchOffCase: switchOffCase{fails: []string{"/.copilot/mcp-config.json is a file Copilot rejects (configuration must be an object)"}}},
		"the policy denies the bridge by name": {policy: `{"deniedMcpServers":[{"serverUrl":"http://other.example.com/*"},{"serverName":"crewlet"}]}`,
			switchOffCase: switchOffCase{fails: []string{policy + ` lists {"serverName":"crewlet"} in deniedMcpServers`}}},
		"the policy denies the bridge's URL": {policy: `{"deniedMcpServers":[{"serverUrl":"HTTP://*:9/mcp/*"}]}`,
			switchOffCase: switchOffCase{fails: []string{policy + ` lists {"serverUrl":"HTTP://*:9/mcp/*"} in deniedMcpServers`}}},
		"the policy allows no server": {policy: `{"allowedMcpServers":[]}`,
			switchOffCase: switchOffCase{fails: []string{policy + " sets allowedMcpServers to [], which leaves the server crewlet out"}}},
		"the policy allows the bridge by name beside a URL": {policy: `{"allowedMcpServers":[{"serverName":"crewlet"},{"serverUrl":"http://other.example.com/*"}]}`,
			switchOffCase: switchOffCase{fails: []string{policy + ` sets allowedMcpServers to [{"serverName":"crewlet"},`}}},
		"the policy admits only its own servers": {policy: `{"allowManagedMcpServersOnly":true,"allowedMcpServers":[{"serverUrl":"http://127.0.0.1:9/*"}]}`,
			switchOffCase: switchOffCase{fails: []string{policy + " sets allowManagedMcpServersOnly to true"}}},
		"the policy denies another server's tools": {policy: `{"permissions":{"deny":["Mcp(other/x)"]}}`,
			switchOffCase: switchOffCase{fails: []string{policy + ` lists "Mcp(other/x)" in permissions.deny`}}},
		"the policy locks MCP servers": {policy: `{"strictPluginOnlyCustomization":["mcp"]}`,
			switchOffCase: switchOffCase{fails: []string{policy + ` sets strictPluginOnlyCustomization to ["mcp"], which locks MCP servers`}}},
		"the policy locks extensions": {notesOnly: true, policy: `{"strictPluginOnlyCustomization":["extensions"]}`,
			switchOffCase: switchOffCase{fails: []string{policy + ` sets strictPluginOnlyCustomization to ["extensions"], which locks extensions`}}},
		"the policy locks every surface": {policy: `{"strictPluginOnlyCustomization":true}`,
			switchOffCase: switchOffCase{fails: []string{"which locks MCP servers", "which locks extensions"}}},
		"the policy turns --yolo off": {notesOnly: true, policy: `{"permissions":{"disableBypassPermissionsMode":"enabled"}}`,
			switchOffCase: switchOffCase{fails: []string{policy + ` sets permissions.disableBypassPermissionsMode to "enabled"`}}},
		"the policy names an unknown rule family": {policy: `{"permissions":{"deny":["Shell(rm)","crewlet"]}}`,
			switchOffCase: switchOffCase{fails: []string{policy + ` lists "crewlet" in permissions.deny, a rule family Copilot does not know`, closed}}},
		"the policy holds a comment": {policy: "// the machine's\n{\"allowManagedHooksOnly\":true}",
			switchOffCase: switchOffCase{fails: []string{policy + " does not hold a JSON object", closed}}},
		"the policy is not root's": {notOwned: true, policy: `{}`,
			switchOffCase: switchOffCase{fails: []string{policy + " is not owned by root", closed}}},
		"the policy is a link": {linked: true, policy: `{}`,
			switchOffCase: switchOffCase{fails: []string{policy + " is a symbolic link", closed}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			box, err := local.Create(t.Context(), sandbox.Spec{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { box.Close(t.Context()) })
			home := box.Home()
			owner := os.Getuid()
			if tc.notOwned {
				owner++
			}
			runner := executor.NewCopilotUnderPolicy(home+policy, owner)
			if err = runner.Install(t.Context(), box); err != nil {
				t.Fatal(err)
			}
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/copilot", copilotGuardStandIn)
			at := strings.NewReplacer("HOME", home)
			if tc.home != "" {
				plant(t, box, map[string]string{".copilot/settings.json": tc.home})
			}
			plant(t, box, map[string]string{"answer.settings": tc.settings, "answer.mcp": at.Replace(tc.mcp)})
			if tc.policy != "" {
				target := "policy/managed-settings.json"
				if tc.linked {
					target = "policy/real.json"
				}
				plant(t, box, map[string]string{target: tc.policy})
				if tc.linked {
					if err = os.Symlink("real.json", home+policy); err != nil {
						t.Fatal(err)
					}
				}
			}
			req := sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge, Steer: loadsSteer}
			if tc.bridgeOnly {
				req.Steer = sandbox.SteerWiring{}
			}
			if tc.notesOnly {
				req.MCPServers = nil
			}
			if len(tc.fails) == 0 {
				_, res := runToEnd(t, runner, box, req)
				checkSwitchOff(t, box, res, tc.switchOffCase)
				if _, err = os.Stat(filepath.Join(home, "started")); err != nil {
					t.Errorf("copilot never started for a run nothing switched off: %v", err)
				}
			} else {
				before, _ := os.ReadFile(home + settings)
				if _, err = runner.Start(t.Context(), box, req); err == nil {
					t.Fatal("a run whose bridge or extension something switched off started")
				}
				for _, want := range tc.fails {
					if strings.HasPrefix(want, "/") {
						want = home + want
					}
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal does not say %q:\n%v", want, err)
					}
				}
				if _, started := os.Stat(filepath.Join(home, "started")); started == nil {
					t.Error("copilot started for a refused run")
				}
				if after, _ := os.ReadFile(home + settings); string(after) != string(before) {
					t.Errorf("a refused run changed the home's settings from %q to %q", before, after)
				}
				return
			}
			var asked map[string]any
			if raw, _ := os.ReadFile(filepath.Join(home, "asked.settings")); json.Unmarshal(raw, &asked) != nil ||
				asked["experimental"] != true {
				t.Errorf("Copilot was asked about settings %s, want the run's own beside the home's", raw)
			}
			raw, _ := os.ReadFile(filepath.Join(home, "asked.home"))
			if want := executor.PathsFor(box).CopilotHome(); !tc.notesOnly && strings.TrimSpace(string(raw)) != want {
				t.Errorf("Copilot listed the MCP servers of %q, want the run's home %s", raw, want)
			}
		})
	}
}
