package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A MUSE RUN REFUSES A FILE THAT DECLARES ITS SESSION'S SERVER NAME: a server's
// suffix is stable across a session's runs (sessionNonce), so code an earlier
// run of the session left can know it, and a repository's `.mcp.json`
// declaring the name replaces the run's own entry whole (measured on 1.4.25:
// its tools were the ones offered). Before the run starts, each `.mcp.json`
// from the start directory up to the repository's root is read as Muse reads
// it, and a server under one of the run's names, the session's or a fresh
// one's, refuses the run naming the file; another name, or a file that does
// not read, refuses nothing.
//
// Mutation: skip the check, check only the continued session's names, or
// match the name as text: a case goes red.
func TestAMuseRunRefusesAFileDeclaringItsSessionsServerName(t *testing.T) {
	t.Parallel()
	const session, freshID = "0b5e0000-0000-4000-8000-0000000000d1", "0b5e0000-0000-4000-8000-0000000000d2"
	ours, theirs := "crewlet-"+sessionNonce(session), "crewlet-"+sessionNonce(freshID)
	servers := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://e/mcp"}}
	for name, tc := range map[string]struct {
		file    string
		refused bool
	}{
		"the session's own name":    {`{"mcpServers":{"` + ours + `":{"command":"x"}}}`, true},
		"the fresh session's name":  {`{"mcpServers":{"` + theirs + `":{"command":"x"}}}`, true},
		"the name in a JSON escape": {`{"mcpServers":{"crewlet-` + sessionNonce(session) + `":{"command":"x"}}}`, true},
		"another server":            {`{"mcpServers":{"crewlet-000000":{"command":"x"}}}`, false},
		"the name as a value alone": {`{"mcpServers":{"other":{"command":"` + ours + `"}}}`, false},
		"a file that does not read": {`{"mcpServers":`, false},
		"no file in the repository": {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := sandbox.NewFakeSandbox("box-m")
			const repo = "/repo/.mcp.json"
			b.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
				if strings.Contains(cmd, ".mcp.json") && tc.file != "" {
					return sandbox.ExecResult{Stdout: repo + "\n"}, nil
				}
				return sandbox.ExecResult{}, nil
			}
			if tc.file != "" {
				if err := b.WriteFile(t.Context(), repo, []byte(tc.file)); err != nil {
					t.Fatal(err)
				}
			}
			req := sandbox.RunRequest{Brief: "x", Resume: session, MCPServers: servers,
				FreshWiring: &sandbox.Wiring{MCPServers: servers}}
			_, err := (Muse{}).WriteConfig(t.Context(), b, req, Session{ID: session, Continues: true, Fresh: freshID},
				PathsFor(b))
			switch {
			case tc.refused && (err == nil || !strings.Contains(err.Error(), repo)):
				t.Errorf("WriteConfig = %v; want the run refused naming %s", err, repo)
			case !tc.refused && err != nil:
				t.Errorf("WriteConfig = %v; want the run written", err)
			}
		})
	}
}

// THE MUSE CHECK READS EVERY PROJECT FILE MUSE LOADS A SERVER FROM: the
// `.mcp.json` of the start directory and of each directory above it up to the
// repository's root, a directory between them included, and none above the
// root (measured on 1.4.25: each of those took the run's name, one above the
// root did not).
//
// Mutation: stop the walk at the start directory, or go past the root, and
// this goes red.
func TestTheMuseCheckReadsEveryProjectFileMuseLoads(t *testing.T) {
	t.Parallel()
	git := gittest.Require(t)
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	top, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, start := filepath.Join(top, "repo"), filepath.Join(top, "repo", "mid", "start")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(git, "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	for _, dir := range []string{top, repo, filepath.Join(repo, "mid"), start} {
		if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(sh, "-c", museProjectFiles)
	cmd.Dir = start
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(git)+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the listing: %v", err)
	}
	got := strings.Fields(string(out))
	want := []string{filepath.Join(start, ".mcp.json"), filepath.Join(repo, "mid", ".mcp.json"),
		filepath.Join(repo, ".mcp.json")}
	if !slices.Equal(got, want) {
		t.Errorf("the check reads %v; want %v (none above the root)", got, want)
	}
}
