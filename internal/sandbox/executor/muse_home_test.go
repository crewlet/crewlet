package executor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A MUSE RUN LOADS ONLY THE SKILLS AND INSTRUCTIONS ITS LAUNCH WRITES. Muse
// 1.4.25 hands its model, from the box home, every skill under
// `.agents/skills` and `.config/muse/skills`, the personal `.config/muse/AGENTS.md`
// and the skills of every plugin installed there, approved or not (measured);
// whatever an earlier run left in those places is gone once the launch has
// cleared the box and written its config, and the run's own skills, its
// sessions and its login are where they were.
//
// Mutation: drop any of the four from Muse's artifacts, or the clear's
// directories from them, and a planted place survives.
func TestAMuseRunLoadsOnlyTheSkillsAndInstructionsItsLaunchWrites(t *testing.T) {
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

	planted := []string{
		p.MuseSkills() + "/planted/SKILL.md",
		p.MuseConfigSkills() + "/planted/SKILL.md",
		p.MuseInstructions(),
		p.MusePlugins() + "/installed.json",
		p.MusePlugins() + "/cache/local/planted/0/package/skills/planted/SKILL.md",
	}
	kept := map[string]string{
		p.MuseConfig() + "/auth.json":                               `{"schema_version":1}`,
		p.MuseSessions() + "/2026/11/22/0b5e0000/session.jsonl":     "{}\n",
		filepath.Join(p.Home, ".local/share/muse/session-index.db"): "index",
	}
	for _, file := range planted {
		plantFile(t, file, "planted by an earlier run")
	}
	for file, body := range kept {
		plantFile(t, file, body)
	}

	runner := executor.NewMuse()
	if err = runner.ClearArtifacts(t.Context(), box); err != nil {
		t.Fatalf("ClearArtifacts: %v", err)
	}
	req := sandbox.RunRequest{Brief: "x", Skills: handedSkills()}
	if _, err = (executor.Muse{}).WriteConfig(t.Context(), box, req, executor.Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	for _, file := range planted {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("%s survived the launch (stat: %v)", file, err)
		}
	}
	for file, body := range kept {
		if got, err := os.ReadFile(file); err != nil || string(got) != body { //nolint:gosec // the case's own box
			t.Errorf("%s holds %q (%v), want %q", file, got, err, body)
		}
	}
	for _, s := range req.Skills {
		if _, err := os.Stat(p.MuseSkills() + "/" + s.Listed() + "/SKILL.md"); err != nil {
			t.Errorf("the run's own skill %s was not written: %v", s.Listed(), err)
		}
	}
}

// plantFile writes body to file, making its directories.
func plantFile(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
