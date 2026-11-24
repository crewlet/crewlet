package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// AN OPENCODE RUN WHOSE BRIDGE ANOTHER DOCUMENT DECLARES DOES NOT START.
// OpenCode 2.3.2 loads /etc/opencode after OPENCODE_CONFIG_CONTENT and
// replaces a server whole by the last document naming it, so /etc's
// `crewlet` stood in for the bridge while `mcp.list` read as ever
// (measured; the documents below are config.get's shape from that run). The
// script's check over `config.get` refuses a run whose documents declare the
// bridge's name other than once, and lets the run's own alone, beside an
// ordinary server's document.
//
// Mutations, each red: count from 0 at 1; leave the check out of the
// script; match the bare name, not the key.
func TestAnOpenCodeRunWhoseBridgeAnotherDocumentDeclaresDoesNotStart(t *testing.T) {
	t.Parallel()
	own := `{"type":"document","info":{"mcp":{"servers":{"crewlet":{"codemode":false,"disabled":false,"type":"remote","url":"http://127.0.0.1:7/b"}}}}}`
	etc := `{"type":"document","path":"/etc/opencode/opencode.json","info":{"mcp":{"servers":{"crewlet":{"type":"local","command":["node","x"]}}}}}`
	other := `{"type":"document","path":"/w/opencode.json","info":{"mcp":{"servers":{"github":{"type":"local","command":["x"]}},"agent":{"reviewer":{"prompt":"crewlet is the bridge"}}}}}`
	servers := map[string]sandbox.MCPServer{"crewlet": {Name: "crewlet", AlwaysLoaded: true}, "github": {Name: "github"}}
	for _, tc := range []struct {
		name, answer string
		started      bool
	}{
		{"the run's own alone", "[" + own + "]", true},
		{"the run's own beside an ordinary server", "[" + other + "," + own + "]", true},
		{"/etc/opencode's crewlet", "[" + own + "," + etc + "]", false},
		{"a repository's crewlet below the run's", "[" + etc + "," + own + "]", false},
		{"no document naming it", "[" + other + "]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			answer := filepath.Join(dir, "config.json")
			if err := os.WriteFile(answer, []byte(tc.answer), 0o600); err != nil {
				t.Fatal(err)
			}
			ans := filepath.Join(dir, "ans")
			script := "endasked= url=http://127.0.0.1:1\n" +
				"bounded() { shift 3; \"$@\"; }\n" +
				"opencode() { cat " + answer + "; }\n" +
				openCodeBridgeDeclared(servers, ans, 5) + "echo STARTED\n"
			out, err := exec.CommandContext(t.Context(), "sh", "-c", script).CombinedOutput()
			started := strings.Contains(string(out), "STARTED")
			if started != tc.started || (err == nil) != tc.started {
				t.Errorf("the run started: %v (%v): %s; want %v", started, err, out, tc.started)
			}
			if !tc.started && !strings.Contains(string(out), "may not take Crewlet's own server's place") {
				t.Errorf("the refusal says %q, want the rule named", out)
			}
		})
	}
	paths := PathsFor(sandbox.NewFakeSandbox("box-1"))
	script := OpenCode{}.Command(sandbox.RunRequest{MCPServers: servers}, Session{ID: "s"}, paths, "")
	if !strings.Contains(script, "config.get") {
		t.Error("a run with the bridge is started without the check over config.get")
	}
	if got := openCodeBridgeDeclared(map[string]sandbox.MCPServer{"github": {Name: "github"}}, "a", 5); got != "" {
		t.Errorf("a run with no bridge is checked: %q", got)
	}
}
