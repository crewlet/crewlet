package executor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A KIMI CODE RUN LOADS ONLY THE CONFIG ITS LAUNCH WRITES. Kimi 2.7.2 hands
// its model every skill under `~/.kimi-code/skills` and `~/.agents/skills`
// and the personal AGENTS.md in the home and under `~/.agents`, runs the hooks
// and adds the system prompt of every plugin installed in the home, and
// starts a repository's MCP servers in a workspace whose trust was saved there,
// and reads hooks from config.toml (measured); whatever an earlier run left in
// those places is gone once the launch has cleared the box and written its
// config, and its sessions and its login are where they were.
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
		"config.toml, no login's":  p.KimiConfig(),
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

// A KIMI CODE RUN IS HANDED ITS LOGIN'S CONFIG AND NOTHING A RUN WROTE INTO
// IT: on a local box seeded with a login, a [[hooks]] table an earlier run
// appended to config.toml is gone once the launch has started the next run,
// and the login's own tables are there.
//
// Mutation: drop the launch's reseed, or config.toml from Kimi's artifacts.
func TestAKimiRunIsHandedOnlyItsLoginsConfig(t *testing.T) {
	t.Parallel()
	shared := t.TempDir()
	plantKimiFile(t, filepath.Join(shared, "config.toml"),
		"default_model = \"managed:kimi-code/k\"\n\n[providers.\"managed:kimi-code\"]\ntype = \"kimi\"\n")
	plantKimiFile(t, filepath.Join(shared, "kimi-code.json"), `{"token":"t"}`)
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{
		CredentialFiles: map[string]string{
			".kimi-code/config.toml":                filepath.Join(shared, "config.toml"),
			".kimi-code/credentials/kimi-code.json": filepath.Join(shared, "kimi-code.json"),
		},
		CredentialTables: map[string][]string{".kimi-code/config.toml": {"default_model", "providers"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	p := executor.PathsFor(box)
	config, err := os.ReadFile(p.KimiConfig())
	if err != nil {
		t.Fatal(err)
	}
	plantKimiFile(t, p.KimiConfig(), string(config)+"\n[[hooks]]\nevent = \"SessionStart\"\ncommand = \"touch /tmp/planted\"\n")

	runner := executor.NewKimi()
	if err = runner.Install(t.Context(), box); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err = runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "x"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got, err := os.ReadFile(p.KimiConfig())
	if err != nil {
		t.Fatalf("the run was handed no config.toml: %v", err)
	}
	if strings.Contains(string(got), "hooks") || !strings.Contains(string(got), "managed:kimi-code") {
		t.Errorf("the run was handed %q, want the login's tables alone", got)
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
