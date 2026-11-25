package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A MANAGED NO_PROXY LEAVING THE BRIDGE OUT ENDS A CLAUDE CODE RUN BEFORE IT
// STARTS. A managed settings file's `env` replaces the run's own NO_PROXY
// (measured on 2.1.330: the bridge failed behind a dead proxy), so where a
// proxy is set and a managed list leaves out a host of the run's own routes,
// the step fails naming the file, the key and the value; a list covering the
// host (itself, a domain it ends in, `*`, with a port), one it cannot judge (a
// range), no proxy at all, or no managed file, lets the run start.
//
// Mutations, each red: leave the step out of the script; read no_proxy alone;
// ignore whether a proxy is set; read a domain entry as no cover.
func TestAManagedNoProxyLeavingTheBridgeOutEndsTheRun(t *testing.T) {
	t.Parallel()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	hosts := []string{"bridge.crewlet.example", "127.0.0.1"}
	bridge := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP,
		URL: "http://bridge.crewlet.example:8080/mcp/t", AlwaysLoaded: true}}
	if script := (ClaudeCode{}).Command(sandbox.RunRequest{MCPServers: bridge}, Session{ID: "s"},
		PathsFor(sandbox.NewFakeSandbox("b")), ""); !strings.Contains(script, "ranks managed settings above") {
		t.Error("a Claude Code run's script does not check the managed settings' NO_PROXY")
	}
	for name, tc := range map[string]struct {
		managed, proxy string
		fails          []string
	}{
		"no managed file": {proxy: "http://proxy.example:3128"},
		"a managed list without the bridge": {managed: `{"env":{"NO_PROXY":"other.example"}}`,
			proxy: "http://proxy.example:3128",
			fails: []string{`sets env.NO_PROXY to "other.example"`, "leaves bridge.crewlet.example"}},
		"a managed lower-case list without loopback": {
			managed: `{"env":{"no_proxy":"bridge.crewlet.example"}}`, proxy: "http://proxy.example:3128",
			fails: []string{`sets env.no_proxy to "bridge.crewlet.example"`, "leaves 127.0.0.1"}},
		"a managed list covering both": {
			managed: `{"env":{"NO_PROXY":".crewlet.example,127.0.0.1:8080"}}`, proxy: "http://proxy.example:3128"},
		"a managed wildcard": {managed: `{"env":{"NO_PROXY":"*"}}`, proxy: "http://proxy.example:3128"},
		"a managed range":    {managed: `{"env":{"NO_PROXY":"crewlet.example,127.0.0.0/8"}}`, proxy: "http://proxy.example:3128"},
		"no proxy set":       {managed: `{"env":{"NO_PROXY":"other.example"}}`},
		"a proxy the file names": {managed: `{"env":{"NO_PROXY":"other.example","HTTPS_PROXY":"http://p:1"}}`,
			fails: []string{`sets env.NO_PROXY to "other.example"`}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			managed := filepath.Join(dir, "managed-settings.json")
			if tc.managed != "" {
				if err := os.WriteFile(managed, []byte(tc.managed), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p := PathsFor(sandbox.NewFakeSandbox("b"))
			cmd := exec.Command(sh, "-c", "cd "+dir+" || exit 1\n"+
				claudeManagedNoProxy(p, hosts, []string{managed, dir + "/managed-settings.d/*.json"})+"echo started")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
			if tc.proxy != "" {
				cmd.Env = append(cmd.Env, "HTTPS_PROXY="+tc.proxy)
			}
			out, err := cmd.CombinedOutput()
			if len(tc.fails) == 0 {
				if err != nil || !strings.Contains(string(out), "started") {
					t.Fatalf("the step ended a run it should start (%v):\n%s", err, out)
				}
				return
			}
			if err == nil || strings.Contains(string(out), "started") {
				t.Fatalf("the step started a run whose managed NO_PROXY leaves the bridge out:\n%s", out)
			}
			for _, want := range append(tc.fails, managed) {
				if !strings.Contains(string(out), want) {
					t.Errorf("the refusal does not say %q:\n%s", want, out)
				}
			}
		})
	}
}

// A CLAUDE CODE RUN'S NO_PROXY COPIES OF ITS SETTINGS GO WITH THE NEXT CLEAR:
// each `.np` copy ([claudeNoProxyMerge]) holds a run's routes' hosts.
//
// Mutation: drop a `.np` copy from the artifact files.
func TestAClaudeRunsNoProxyCopiesAreItsArtifacts(t *testing.T) {
	t.Parallel()
	p := PathsFor(sandbox.NewFakeSandbox("b"))
	files := (ClaudeCode{}).artifactFiles(p)
	for _, settings := range []string{p.ClaudeSettings(), p.ClaudeNotesSettings(), p.ClaudeSettingsPinned(),
		p.ClaudeNotesSettingsPinned()} {
		if !strings.Contains(strings.Join(files, "\n")+"\n", settings+".np\n") {
			t.Errorf("%s.np is not among a Claude Code run's artifacts %v", settings, files)
		}
	}
}
