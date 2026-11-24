package executor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A KIMI CODE RUN LOADS ONLY THE CONFIG ITS LAUNCH WRITES. Kimi 2.7.3 hands
// its model every skill under `~/.kimi-code/skills` and `~/.agents/skills`
// and the personal AGENTS.md in the home and under `~/.agents`, runs the hooks
// and adds the system prompt of every plugin installed in the home, and
// and starts a repository's MCP servers in a workspace whose trust was saved
// there (measured); whatever an earlier run left in those places is gone once
// the launch has cleared the box and written its config, and its sessions and
// its login are where they were.
//
// Mutation: drop any place from Kimi's artifacts, and it survives the launch.
func TestAKimiRunLoadsOnlyTheConfigItsLaunchWrites(t *testing.T) {
	t.Parallel()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	p := executor.PathsFor(box)

	planted := map[string]string{
		"a home skill":             p.KimiSkills() + "/planted/SKILL.md",
		"a user skill":             p.KimiUserSkills() + "/planted/SKILL.md",
		"the home's AGENTS.md":     p.KimiInstructions(),
		"a plugin's record":        p.KimiPlugins() + "/installed.json",
		"a plugin's package":       p.KimiPlugins() + "/planted/kimi.plugin.json",
		"a workspace trust":        p.KimiTrust() + "/wd_workspace_0123456789ab",
		"an earlier run's profile": p.KimiAgents() + "/crewlet/crewlet-0123456789ab.md",
		"`~/.agents/AGENTS.md`":    p.KimiUserInstructions()[0],
		"`~/.agents/agents.md`":    p.KimiUserInstructions()[1],
	}
	kept := map[string]string{
		p.KimiHome() + "/credentials/kimi-code.json":          `{"access_token":"x"}`,
		p.KimiSessions() + "/wd_ws_0123/session_1/state.json": "{}",
		p.KimiHome() + "/session_index.jsonl":                 "{}\n",
	}
	for _, file := range planted {
		plantKimiFile(t, file, "planted by an earlier run")
	}
	for file, body := range kept {
		plantKimiFile(t, file, body)
	}

	if err = executor.NewKimi().ClearArtifacts(t.Context(), box); err != nil {
		t.Fatalf("ClearArtifacts: %v", err)
	}
	if _, err = (executor.Kimi{}).WriteConfig(t.Context(), box, sandbox.RunRequest{Brief: "x"},
		executor.Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	for place, file := range planted {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("%s (%s) survived the launch (stat: %v)", place, file, err)
		}
	}
	for file, body := range kept {
		if got, err := os.ReadFile(file); err != nil || string(got) != body { //nolint:gosec // the case's own box
			t.Errorf("%s holds %q (%v), want %q", file, got, err, body)
		}
	}
}

// A KIMI CODE RUN IS HANDED ITS LOGIN'S CONFIG AND NOTHING ELSE OF IT: a
// config.toml holding a [[hooks]] table, a plugin entry and a tool setting
// beside the login, whether its seed held them or an earlier run wrote them,
// is rewritten to the login's tables before the run, and a config.toml that
// holds the login alone is left as it is.
//
// Mutation: drop the rewrite from WriteConfig, or keep a table the profile's
// login_keys does not name.
func TestAKimiRunIsHandedOnlyItsLoginsConfig(t *testing.T) {
	t.Parallel()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	p := executor.PathsFor(box)
	login := "default_model = \"managed:kimi-code/k\"\n\n[providers.\"managed:kimi-code\"]\ntype = \"kimi\"\n"
	plantKimiFile(t, p.KimiConfig(), login+"\n[[hooks]]\nevent = \"SessionStart\"\ncommand = \"touch /tmp/planted\"\n"+
		"\n[plugins.planted]\nenabled = true\n\n[tools]\ndisabled = [\"Bash\"]\n")
	write := func() string {
		t.Helper()
		if _, err := (executor.Kimi{}).WriteConfig(t.Context(), box, sandbox.RunRequest{Brief: "x"},
			executor.Session{}, p); err != nil {
			t.Fatalf("WriteConfig: %v", err)
		}
		got, err := os.ReadFile(p.KimiConfig())
		if err != nil {
			t.Fatalf("the run was handed no config.toml: %v", err)
		}
		return string(got)
	}
	got := write()
	for _, gone := range []string{"hooks", "touch /tmp/planted", "plugins", "tools"} {
		if strings.Contains(got, gone) {
			t.Errorf("the run is handed %q in %q", gone, got)
		}
	}
	if !strings.Contains(got, "managed:kimi-code") {
		t.Errorf("the run is handed %q, want the login's tables", got)
	}
	plantKimiFile(t, p.KimiConfig(), login)
	if got := write(); got != login {
		t.Errorf("a config.toml holding the login alone was rewritten to %q", got)
	}
}

// plantKimiFile writes body to file, making its directories.
func plantKimiFile(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
