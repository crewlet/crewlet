package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A GROK RUN STARTS ONLY WHERE THE BOX'S GROK RUNS ITS SERVERS AS ITS SETTINGS
// WRITE THEM, as `grok inspect --json` answers before it starts: an
// `/etc/grok/requirements.toml` defining one of the run's servers replaces its
// whole transport and a deny list there switches it off, each over the user's
// settings (measured on 1.0.92), so either refuses the run naming the file or
// rule, as does a file defining the fresh session's server, or one not listed.
// An answer this build cannot read refuses nothing (the bridge's connection
// holds that run).
//
// Mutations, each red: skip the check; accept any source; ignore
// disabledReason; check only the continued session's names; refuse an
// unreadable answer; drop the check that every server is listed.
func TestAGrokRunStartsOnlyWhereItsServersAreItsOwn(t *testing.T) {
	t.Parallel()
	const session, freshID = "0b5e0000-0000-4000-8000-0000000000c1", "0b5e0000-0000-4000-8000-0000000000c2"
	ours, theirs := "crewlet_"+sessionNonce(session), "crewlet_"+sessionNonce(freshID)
	settings := `"source":{"type":"configToml","path":"` + PathsFor(sandbox.NewFakeSandbox("box-1")).GrokConfig() + `"}`
	entry := func(name, rest string) string { return `{"name":"` + name + `","transport":"http",` + rest + `}` }
	answer := func(entries ...string) string {
		return `{"grokVersion":"1.0.92","mcpServers":[` + strings.Join(entries, ",") + `]}`
	}
	required := `"source":{"type":"requirements","path":"/etc/grok/requirements.toml"}`
	for name, tc := range map[string]struct {
		inspect string
		exit    int
		refused string
	}{
		"its own":           {inspect: answer(entry("homesrv", required), entry(ours, settings))},
		"another release":   {inspect: `{"servers":[]}`},
		"no answer":         {exit: 2},
		"defined by a file": {inspect: answer(entry(ours, required)), refused: "/etc/grok/requirements.toml"},
		"switched off": {inspect: answer(entry(ours, settings+`,"disabledReason":"matches deniedMcpServers `+
			`(/etc/grok/requirements.toml)"`)), refused: "matches deniedMcpServers"},
		"the fresh session's name": {inspect: answer(entry(ours, settings), entry(theirs, required)),
			refused: theirs},
		"not listed": {inspect: answer(), refused: "does not list"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := sandbox.NewFakeSandbox("box-1")
			var asked string
			b.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
				switch {
				case cmd == "uname -m":
					return sandbox.ExecResult{Stdout: "x86_64\n"}, nil
				case strings.Contains(cmd, "grok inspect --json"):
					asked = cmd
					return sandbox.ExecResult{Stdout: tc.inspect, ExitCode: tc.exit}, nil
				}
				return sandbox.ExecResult{}, nil
			}
			paths := PathsFor(b)
			servers := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://e/mcp"}}
			req := sandbox.RunRequest{Brief: "x", Resume: session, MCPServers: servers,
				FreshWiring: &sandbox.Wiring{MCPServers: servers}}
			_, err := (Grok{}).WriteConfig(t.Context(), b, req, Session{ID: session, Continues: true, Fresh: freshID},
				paths)
			if asked == "" || !strings.Contains(asked, "unset GROK_HOME") {
				t.Fatalf("the box's Grok was not asked which servers it runs as the run's Grok runs: %q", asked)
			}
			switch {
			case tc.refused == "" && err != nil:
				t.Fatalf("WriteConfig = %v, want the run written", err)
			case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)):
				t.Fatalf("WriteConfig = %v, want it refused naming %q", err, tc.refused)
			}
			if brief, _ := b.ReadFile(t.Context(), paths.Brief()); (tc.refused != "") == (string(brief) == "x") {
				t.Errorf("the run's brief holds %q, refused %t", brief, tc.refused != "")
			}
		})
	}
}
