package executor_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A CALL ANOTHER SERVER SERVES UNDER A NAME OF THE BRIDGE'S STOPS THE RUN.
// Gemini CLI 0.70.0 names a tool `mcp_<server>_<tool>`, so a server
// `crewlet_x`, `mcp` listing `crewlet_x_echo`, or another `crewlet` takes
// `mcp_crewlet_x_echo` (measured). The BeforeTool hook is handed the server
// Gemini resolved for the call (`mcp_context`), and the helper stops the run
// unless that is the bridge, at its address, under its own name for the tool;
// each stop is a stream line in the marker. Others' calls and built-ins pass.
//
// Mutations, each red: match the server's name alone; drop the address's
// digest; let a call without `mcp_context` through; check only names that
// open `mcp_crewlet_`.
func TestACallAnotherServerServesUnderABridgeNameStopsTheRun(t *testing.T) {
	t.Parallel()
	node := geminiNode(t)
	const bridgeURL = "http://127.0.0.1:7/mcp/run-token-placeholder/"
	sum := sha256.Sum256([]byte(bridgeURL))
	digest := hex.EncodeToString(sum[:])
	bridged := map[string]any{"server_name": "crewlet", "tool_name": "x_echo", "url": bridgeURL}
	for _, tc := range []struct {
		name    string
		input   map[string]any
		stopped bool
	}{
		{"the bridge's own call", map[string]any{"tool_name": "mcp_crewlet_x_echo", "mcp_context": bridged}, false},
		{"a server crewlet_x's", map[string]any{"tool_name": "mcp_crewlet_x_echo",
			"mcp_context": map[string]any{"server_name": "crewlet_x", "tool_name": "echo", "command": "node"}}, true},
		{"a server mcp's", map[string]any{"tool_name": "mcp_crewlet_x_echo",
			"mcp_context": map[string]any{"server_name": "mcp", "tool_name": "crewlet_x_echo", "command": "node"}}, true},
		{"another crewlet at another address", map[string]any{"tool_name": "mcp_crewlet_x_echo",
			"mcp_context": map[string]any{"server_name": "crewlet", "tool_name": "x_echo", "url": "http://127.0.0.1:8/"}}, true},
		{"another crewlet started by a command", map[string]any{"tool_name": "mcp_crewlet_x_echo",
			"mcp_context": map[string]any{"server_name": "crewlet", "tool_name": "x_echo", "command": "node", "url": bridgeURL}}, true},
		{"a server no settings file names", map[string]any{"tool_name": "mcp_crewlet_x_echo"}, true},
		{"the bridge's server under another tool's name", map[string]any{"tool_name": "mcp_crewlet_x_echo",
			"mcp_context": map[string]any{"server_name": "crewlet", "tool_name": "echo", "url": bridgeURL}}, true},
		{"the bridge's server under a name outside its own", map[string]any{"tool_name": "echo", "mcp_context": bridged}, true},
		{"an ordinary server's", map[string]any{"tool_name": "mcp_github_x_echo",
			"mcp_context": map[string]any{"server_name": "github", "tool_name": "x_echo", "command": "node"}}, false},
		{"a server crewlet-x's", map[string]any{"tool_name": "mcp_crewlet-x_echo",
			"mcp_context": map[string]any{"server_name": "crewlet-x", "tool_name": "echo", "command": "node"}}, false},
		{"a built-in", map[string]any{"tool_name": "read_file"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			marker := filepath.Join(t.TempDir(), "gemini-unbridged")
			in, _ := json.Marshal(tc.input)
			cmd := exec.CommandContext(t.Context(), node, "gemini_helper.mjs", "bridged", "crewlet", digest, marker)
			cmd.Stdin = strings.NewReader(string(in))
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("the hook: %v", err)
			}
			var answer map[string]any
			if err := json.Unmarshal(out, &answer); err != nil {
				t.Fatalf("the hook answered %q, which is no JSON", out)
			}
			wrote, _ := os.ReadFile(marker)
			if !tc.stopped {
				if len(answer) != 0 || len(wrote) != 0 {
					t.Errorf("the hook answered %s and marked %q, want the call let through", out, wrote)
				}
				return
			}
			if answer["continue"] != false || !strings.Contains(string(wrote), `"type":"crewlet_gemini_unbridged"`) ||
				!strings.Contains(string(wrote), "may not take a name inside Crewlet's own server's tool names") {
				t.Errorf("the hook answered %s and marked %q, want the run stopped and the reason marked", out, wrote)
			}
		})
	}
}

// A SETTINGS FILE OR EXTENSION WHOSE SERVER WOULD TAKE THE BRIDGE'S NAMES IS
// REFUSED BEFORE GEMINI CLI STARTS, naming the file and the server: a name
// Gemini CLI would spell as `crewlet_…` or `mcp_crewlet…` in the home's or the
// working directory's settings or an extension the home loads there, and an
// extension's own `crewlet`. A system file root does not keep (Gemini CLI
// skips it), an extension off there, an ordinary server, `crewlet-x`, the
// run's own entry and another `crewlet` above it (the switch-off guard's) pass.
//
// Mutations, each red: drop the sanitizing; skip the extensions; read a
// disabled extension or a skipped system file; refuse the run's own entry.
func TestASettingsFileServerThatWouldTakeTheBridgesNamesIsRefused(t *testing.T) {
	t.Parallel()
	node := geminiNode(t)
	helper, err := filepath.Abs("gemini_helper.mjs")
	if err != nil {
		t.Fatal(err)
	}
	own := `{"mcpServers":{"crewlet":{"httpUrl":"http://127.0.0.1:7/b"}}}`
	extension := `{"name":"ext","version":"1.0.0","mcpServers":{"crewlet_x":{"command":"x"}}}`
	for _, tc := range []struct {
		name, file, settings string
		refused              bool
	}{
		{"the run's own entry", "home", own, false},
		{"a system file's crewlet_x root does not keep, which Gemini CLI skips", "system",
			`{"mcpServers":{"crewlet_x":{"command":"x"}}}`, false},
		{"a system default's crewlet_x root does not keep", "defaults", `{"mcpServers":{"crewlet_x":{"command":"x"}}}`,
			false},
		{"a repository's crewlet_x", "work", `{"mcpServers":{"crewlet_x":{"command":"x"}}}`, true},
		{"a repository's crewlet x, sanitized", "work", `{"mcpServers":{"crewlet x":{"command":"x"}}}`, true},
		{"a repository's mcp_crewlet", "work", `{"mcpServers":{"mcp_crewlet":{"command":"x"}}}`, true},
		{"a home's commented mcp_crewlet_y", "home", "// home\n{\"mcpServers\":{\"crewlet\":{},\"mcp_crewlet_y\":{\"command\":\"x\"}}}", true},
		{"an extension's crewlet_x", "extension", extension, true},
		{"an extension's crewlet", "extension", strings.Replace(extension, "crewlet_x", "crewlet", 1), true},
		{"a repository's crewlet, the switch-off guard's", "work", `{"mcpServers":{"crewlet":{"command":"x"}}}`, false},
		{"an extension off here", "extension off", extension, false},
		{"an ordinary server", "work", `{"mcpServers":{"github":{"command":"x"}}}`, false},
		{"a server crewlet-x", "work", `{"mcpServers":{"crewlet-x":{"command":"x"}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			home, work, system := filepath.Join(dir, "home"), filepath.Join(dir, "work"), filepath.Join(dir, "etc")
			extensions := filepath.Join(home, ".gemini", "extensions")
			files := map[string]string{"home": filepath.Join(home, ".gemini", "settings.json"),
				"work": filepath.Join(work, ".gemini", "settings.json"), "system": filepath.Join(system, "settings.json"),
				"defaults":      filepath.Join(system, "system-defaults.json"),
				"extension":     filepath.Join(extensions, "ext", "gemini-extension.json"),
				"extension off": filepath.Join(extensions, "ext", "gemini-extension.json")}
			for _, d := range []string{filepath.Dir(files["home"]), filepath.Dir(files["work"]), system,
				filepath.Dir(files["extension"])} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.file != "home" {
				if err := os.WriteFile(files["home"], []byte(own), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(files[tc.file], []byte(tc.settings), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.file == "extension off" {
				off := `{"ext":{"overrides":["!` + work + `/*"]}}`
				if err := os.WriteFile(filepath.Join(extensions, "extension-enablement.json"), []byte(off),
					0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), node, helper, "shadows", "crewlet", "--own", "crewlet")
			cmd.Dir = work
			cmd.Env = append(os.Environ(), "HOME="+home, "GEMINI_CLI_HOME=", "GEMINI_CLI_SYSTEM_SETTINGS_PATH="+files["system"])
			out, err := cmd.CombinedOutput()
			code := cmd.ProcessState.ExitCode()
			if err != nil && code < 0 {
				t.Fatalf("the check: %v", err)
			}
			if !tc.refused {
				if code != 0 {
					t.Errorf("the check exited %d (%s), want the run started", code, out)
				}
				return
			}
			if code != 3 || !strings.Contains(string(out), files[tc.file]) ||
				!strings.Contains(string(out), "may not take a name inside Crewlet's own server's tool names") {
				t.Errorf("the check exited %d saying %q, want 3 naming %s", code, out, files[tc.file])
			}
		})
	}
}

// A GEMINI RUN WITH THE BRIDGE IS GUARDED ON BOTH SIDES OF ITS HARNESS: its
// settings hold the BeforeTool hook on every name inside the bridge's, handed
// the address's digest, never the address, and its script refuses a shadowing
// settings file before Gemini CLI starts and adds the hook's stops to the
// stream once it exits. A run whose servers hold no bridge has no hook.
//
// Mutations, each red: hand the hook the address; match every tool; drop the
// check from the script; leave the marker unread.
func TestAGeminiRunWithTheBridgeIsGuardedOnBothSides(t *testing.T) {
	t.Parallel()
	req := geminiWired()
	bridge := req.MCPServers["crewlet"]
	bridge.AlwaysLoaded = true
	req.MCPServers["crewlet"] = bridge
	settings, _, p := writtenGemini(t, req)
	hooks, _ := settings["hooks"].(map[string]any)
	raw, _ := json.Marshal(hooks["BeforeTool"])
	sum := sha256.Sum256([]byte(req.MCPServers["crewlet"].URL))
	if !strings.Contains(string(raw), `"matcher":"^mcp_crewlet_"`) ||
		!strings.Contains(string(raw), "bridged 'crewlet' "+hex.EncodeToString(sum[:])+" "+
			shellword.Quote(p.GeminiUnbridged())) || strings.Contains(string(raw), "run-token-placeholder") {
		t.Errorf("the BeforeTool hook is %s, want the helper on mcp_crewlet_ handed the address's digest", raw)
	}
	// The script as sh reads it, out of the one word `sh -c` is handed.
	script := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(
		executor.GeminiCLI{}.Command(req, executor.Session{ID: "s"}, p, ""), "sh -c '"), "'"), `'\''`, "'")
	shadows, launch := strings.Index(script, " shadows 'crewlet' --own"), strings.Index(script, "gemini -p")
	if shadows < 0 || launch < 0 || shadows > launch || !strings.Contains(script, "|| exit 1") {
		t.Errorf("the script does not refuse a shadowing settings file before Gemini CLI starts:\n%s", script)
	}
	if !strings.Contains(script, "|| cat "+shellword.Quote(p.GeminiUnbridged())) {
		t.Errorf("the script does not add the hook's stops to the stream:\n%s", script)
	}

	settings, _, _ = writtenGemini(t, geminiWired())
	if hooks, _ := settings["hooks"].(map[string]any); hooks["BeforeTool"] != nil {
		t.Errorf("a run with no bridge has a BeforeTool hook: %v", hooks["BeforeTool"])
	}
}

// A RUN THE HOOK STOPPED FAILS IN THE HOOK'S WORDS, though Gemini CLI ended
// its turn as a success (measured on 0.70.0).
//
// Mutation: read the stream's result alone.
func TestAGeminiRunTheBridgeHookStoppedFails(t *testing.T) {
	t.Parallel()
	stream := `{"type":"init","session_id":"s"}` + "\n" +
		`{"type":"result","status":"success","stats":{}}` + "\n" +
		`{"type":"crewlet_gemini_unbridged","message":"Gemini CLI would run \"mcp_crewlet_x_echo\" on the server \"mcp\""}` + "\n"
	res := executor.GeminiCLI{}.Parse([]byte(stream))
	if res.Success || !strings.Contains(res.Error, `on the server "mcp"`) {
		t.Errorf("the run is %+v, want a failure in the hook's words", res)
	}
}
