package executor_test

import (
	"context"
	"encoding/json"
	"maps"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// handedServer is a stdio server as a harness's configuration hands it: its
// command, its arguments, and whether it carries an environment of its own.
type handedServer struct {
	command string
	args    []string
	env     bool
}

// browserRead is how one runner's configuration is read back for its browser
// server, after its WriteConfig wrote it into box, whose paths are p, and
// answered config; it fails the case on anything the runner must also say.
type browserRead func(t *testing.T, h executor.Harness, req sandbox.RunRequest, box *sandbox.FakeSandbox,
	p executor.Paths, config string) handedServer

// browserReads is every runner's reading, by its harness's name. A runner
// added to the registry stays red until it has one.
var browserReads = map[string]browserRead{
	executor.ClaudeCodeName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		_ executor.Paths, config string) handedServer {
		return jsonServer(t, box, config, "mcpServers", browser.ServerName)
	},
	executor.CodexName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		p executor.Paths, _ string) handedServer {
		return codexServer(t, box, p.CodexConfig())
	},
	executor.GeminiCLIName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		p executor.Paths, _ string) handedServer {
		return jsonServer(t, box, p.GeminiSettings(), "mcpServers", browser.ServerName)
	},
	executor.QwenCodeName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		p executor.Paths, _ string) handedServer {
		return jsonServer(t, box, p.QwenSettings(), "mcpServers", browser.ServerName)
	},
	executor.OpenCodeName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		_ executor.Paths, config string) handedServer {
		entry := jsonAt(t, box, config, "mcp", "servers", browser.ServerName)
		if typ := jsonString(t, entry, "type"); typ != "local" {
			t.Errorf("OpenCode is handed the browser as %q, want a local command", typ)
		}
		command := jsonStrings(t, entry, "command")
		if len(command) == 0 {
			t.Fatalf("OpenCode's browser has no command: %v", entry)
		}
		_, env := entry["environment"]
		return handedServer{command: command[0], args: command[1:], env: env}
	},
	executor.CursorAgentName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		_ executor.Paths, config string) handedServer {
		return cursorServer(t, box, config)
	},
	executor.CopilotName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		_ executor.Paths, config string) handedServer {
		entry := jsonAt(t, box, config, "mcpServers", browser.ServerName)
		if typ := jsonString(t, entry, "type"); typ != "local" {
			t.Errorf("Copilot is handed the browser as %q, want local", typ)
		}
		if tools := jsonStrings(t, entry, "tools"); !slices.Equal(tools, []string{"*"}) {
			t.Errorf("Copilot is offered the browser's tools %q, want every one", tools)
		}
		return stdioServer(t, entry)
	},
	executor.MuseName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		p executor.Paths, _ string) handedServer {
		files := filesUnder(box, p.MuseServers()+"/"+browser.ServerName+"-", ".json")
		if len(files) != 1 {
			t.Fatalf("Muse Code's servers hold %q, want one browser file", files)
		}
		entry := jsonAt(t, box, files[0])
		if typ := jsonString(t, entry, "type"); typ != "stdio" {
			t.Errorf("Muse Code is handed the browser as %q, want stdio", typ)
		}
		return stdioServer(t, entry)
	},
	executor.KimiName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		p executor.Paths, _ string) handedServer {
		profiles := filesUnder(box, p.KimiAgents()+"/", ".md")
		if len(profiles) != 1 {
			t.Fatalf("Kimi's agents hold %q, want the run's one profile", profiles)
		}
		// EVERY SERVER'S TOOLS, the browser's among them: Kimi names a server's
		// tool mcp__<server>__<tool>.
		if profile := readFile(t, box, profiles[0]); !strings.Contains(profile, `"mcp__*"`) {
			t.Errorf("Kimi's profile does not admit the browser's tools:\n%s", profile)
		}
		return jsonServer(t, box, p.KimiMCPConfig(), "mcpServers", browser.ServerName)
	},
	executor.GrokName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		_ executor.Paths, config string) handedServer {
		return grokServer(t, box, config)
	},
	executor.HermesName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		p executor.Paths, _ string) handedServer {
		return jsonServer(t, box, p.HermesOverlayStaged(), "mcp_servers", browser.ServerName)
	},
	executor.PiName: func(t *testing.T, _ executor.Harness, _ sandbox.RunRequest, box *sandbox.FakeSandbox,
		p executor.Paths, _ string) handedServer {
		_, rest, ok := strings.Cut(readFile(t, box, p.PiExtension()), "const config = ")
		line, _, _ := strings.Cut(rest, "\n")
		var settings struct {
			Servers []struct {
				Name   string         `json:"name"`
				Config map[string]any `json:"config"`
			} `json:"servers"`
		}
		if !ok || json.Unmarshal([]byte(line), &settings) != nil {
			t.Fatalf("Pi's extension holds no settings")
		}
		for _, s := range settings.Servers {
			if s.Name == browser.ServerName {
				return stdioServer(t, s.Config)
			}
		}
		t.Fatalf("Pi's extension registers %+v, and no browser", settings.Servers)
		return handedServer{}
	},
}

// A BROWSER SERVER IS HANDED TO EACH HARNESS AS A PLAIN COMMAND: every
// registered runner writes a ready browser's server as node running the shim,
// its arguments (the helper and the workspace absolute), and no environment,
// in its own vocabulary: Claude Code's --mcp-config file, Codex's
// configuration, Gemini CLI's and Qwen Code's settings, OpenCode's as a local
// command, Cursor Agent's servers under the run's suffix, Copilot's
// --additional-mcp-config file as a local command offering every tool, Muse
// Code's server file as stdio, Kimi's mcp.json beside its profile admitting
// every server's tools, Grok Build's settings under the run's nonce, Hermes's
// overlay and Pi's extension. A runner added later fails until it has a
// reading. Mutations, each red: give the server an environment; make the
// shim's path relative; offer Copilot fewer of its tools than all.
func TestABrowserServerIsHandedToEachHarnessAsAPlainCommand(t *testing.T) {
	t.Parallel()
	server := readyBrowserServer(t)
	if server.Command != "node" || len(server.Env) > 0 {
		t.Fatalf("a ready browser's server is %+v, want node with no environment", server)
	}
	root := slices.Index(server.Args, "--root")
	if len(server.Args) == 0 || !path.IsAbs(server.Args[0]) || root < 0 || !path.IsAbs(server.Args[root+1]) {
		t.Fatalf("a ready browser's server runs %q, want the helper and the workspace absolute", server.Args)
	}
	for _, h := range executor.Registered() {
		t.Run(h.Name(), func(t *testing.T) {
			t.Parallel()
			read, ok := browserReads[h.Name()]
			if !ok {
				t.Fatalf("%s is a registered runner with no reading of its browser server: add one to "+
					"browserReads", h.Name())
			}
			box := sandbox.NewFakeSandbox("box-" + h.Name())
			p := executor.PathsFor(box)
			req := sandbox.RunRequest{Brief: "x", MCPServers: map[string]sandbox.MCPServer{browser.ServerName: server}}
			config, err := h.WriteConfig(t.Context(), box, req, h.Session(req), p)
			if err != nil {
				t.Fatalf("WriteConfig: %v", err)
			}
			got := read(t, h, req, box, p, config)
			if got.command != server.Command || !slices.Equal(got.args, server.Args) || got.env {
				t.Errorf("%s hands the browser as %+v, want %s %q with no environment", h.Name(), got,
					server.Command, server.Args)
			}
		})
	}
}

// readyBrowserServer is the server a container box's ready ensure hands its
// run, as the launch's own step makes it.
func readyBrowserServer(t *testing.T) sandbox.MCPServer {
	t.Helper()
	box := sandbox.NewFakeSandbox("box-1")
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if cmd != boxhelper.EnsureCommand(box.Home()) {
			return sandbox.ExecResult{}, nil
		}
		return sandbox.ExecResult{Stdout: "crewlet-browser-ensure/1 ready helper=0123456789abcdef pin=0.0.86 " +
			"node=22.23.5 chromium=" + browser.ExpectedChromiumVersion + " build=expected sandbox=required " +
			"supervisor=started"}, nil
	}
	out, err := sandbox.EnsureBrowser(t.Context(), box, sandbox.BrowserStep{Placement: sandbox.Container,
		Browser: sandbox.BrowserLaunch{Sandbox: browser.SandboxRequired}, LaunchID: "launch-1"})
	if err != nil || out.Server == nil {
		t.Fatalf("EnsureBrowser = %+v, %v, want a ready browser's server", out, err)
	}
	return *out.Server
}

// filesUnder is every file box holds whose path begins with prefix and ends
// with suffix: a runner names some of its files afresh every run.
func filesUnder(box *sandbox.FakeSandbox, prefix, suffix string) []string {
	return slices.DeleteFunc(box.Files(), func(p string) bool {
		return !strings.HasPrefix(p, prefix) || !strings.HasSuffix(p, suffix)
	})
}

// readFile is the text of box's file.
func readFile(t *testing.T, box *sandbox.FakeSandbox, file string) string {
	t.Helper()
	raw, err := box.ReadFile(t.Context(), file)
	if err != nil || raw == nil {
		t.Fatalf("%s reads %q (%v)", file, raw, err)
	}
	return string(raw)
}

// jsonAt is the object at keys in box's JSON file.
func jsonAt(t *testing.T, box *sandbox.FakeSandbox, file string, keys ...string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(readFile(t, box, file)), &doc); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	for _, key := range keys {
		next, ok := doc[key].(map[string]any)
		if !ok {
			t.Fatalf("%s holds no object at %q", file, keys)
		}
		doc = next
	}
	return doc
}

// jsonServer is the stdio server at keys in box's JSON file.
func jsonServer(t *testing.T, box *sandbox.FakeSandbox, file string, keys ...string) handedServer {
	t.Helper()
	return stdioServer(t, jsonAt(t, box, file, keys...))
}

// stdioServer is a server written as command, args and env.
func stdioServer(t *testing.T, entry map[string]any) handedServer {
	t.Helper()
	_, env := entry["env"]
	return handedServer{command: jsonString(t, entry, "command"), args: jsonStrings(t, entry, "args"), env: env}
}

// jsonString is entry's string at key.
func jsonString(t *testing.T, entry map[string]any, key string) string {
	t.Helper()
	s, ok := entry[key].(string)
	if !ok {
		t.Fatalf("%v holds no string %s", entry, key)
	}
	return s
}

// jsonStrings is entry's list of strings at key.
func jsonStrings(t *testing.T, entry map[string]any, key string) []string {
	t.Helper()
	list, ok := entry[key].([]any)
	if !ok {
		t.Fatalf("%v holds no list %s", entry, key)
	}
	out := make([]string, len(list))
	for i, v := range list {
		if out[i], ok = v.(string); !ok {
			t.Fatalf("%v's %s holds %v, no string", entry, key, v)
		}
	}
	return out
}

// grokServer is the browser's table of Grok Build's settings, which names each
// server with the run's nonce after it: the one table under the browser's name.
func grokServer(t *testing.T, box *sandbox.FakeSandbox, file string) handedServer {
	t.Helper()
	var settings struct {
		Servers map[string]struct {
			Command string            `toml:"command"`
			Args    []string          `toml:"args"`
			Env     map[string]string `toml:"env"`
		} `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(readFile(t, box, file), &settings); err != nil {
		t.Fatalf("Grok Build's settings %s: %v", file, err)
	}
	var found []handedServer
	for name, s := range settings.Servers {
		if strings.HasPrefix(name, browser.ServerName+"_") {
			found = append(found, handedServer{command: s.Command, args: s.Args, env: s.Env != nil})
		}
	}
	if len(found) != 1 {
		t.Fatalf("Grok Build's settings name %d browser servers among %v, want one", len(found),
			slices.Sorted(maps.Keys(settings.Servers)))
	}
	return found[0]
}

// cursorServer is the browser's entry in a Cursor Agent run's servers, which
// names each server with the run's suffix after it: the one entry under the
// browser's name.
func cursorServer(t *testing.T, box *sandbox.FakeSandbox, file string) handedServer {
	t.Helper()
	var found []handedServer
	for name, entry := range jsonAt(t, box, file, "mcpServers") {
		server, ok := entry.(map[string]any)
		if ok && strings.HasPrefix(name, browser.ServerName+"-") {
			found = append(found, stdioServer(t, server))
		}
	}
	if len(found) != 1 {
		t.Fatalf("Cursor Agent's servers name %d browser servers, want one", len(found))
	}
	return found[0]
}

// codexServer is the browser's block of Codex's configuration, read line by
// line: its command, its arguments and whether it sets an environment.
func codexServer(t *testing.T, box *sandbox.FakeSandbox, file string) handedServer {
	t.Helper()
	_, block, ok := strings.Cut(readFile(t, box, file), `[mcp_servers."`+browser.ServerName+`"]`+"\n")
	if !ok {
		t.Fatalf("Codex's configuration has no browser block")
	}
	block, _, _ = strings.Cut(block, "\n[")
	var out handedServer
	for _, line := range strings.Split(block, "\n") {
		key, value, _ := strings.Cut(line, " = ")
		switch key {
		case "command":
			if err := json.Unmarshal([]byte(value), &out.command); err != nil {
				t.Fatalf("Codex's browser command %q: %v", value, err)
			}
		case "args":
			if err := json.Unmarshal([]byte(value), &out.args); err != nil {
				t.Fatalf("Codex's browser args %q: %v", value, err)
			}
		case "env":
			out.env = true
		}
	}
	return out
}
