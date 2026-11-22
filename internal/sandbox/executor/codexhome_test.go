package executor_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A CODEX RUN STARTS FROM ITS LAUNCH'S OWN INSTRUCTIONS, RULES AND SKILLS:
// whatever its box's home held before the run at each place a run on 0.171.0
// reads as it starts ([executor.Paths.CodexStartsWithout]: CODEX_HOME's
// AGENTS.md and AGENTS.override.md, its rules, the home's .agents/skills) is
// gone when Codex starts, and CODEX_HOME's skills are the run's own beside
// Codex's `.system`, every other entry cleared, an earlier run's included.
//
// Mutation: drop any path from CodexStartsWithout or the script's removal, or
// clear only the engine's namespace from the skills, and this goes red.
func TestACodexRunStartsFromItsLaunchsOwnInstructionsRulesAndSkills(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	p := executor.PathsFor(box)
	starts := p.CodexStartsWithout()
	if len(starts) != 4 {
		t.Fatalf("a run starts without %q, want the four places a run reads as it starts", starts)
	}
	for _, path := range starts {
		file := path
		if !strings.HasSuffix(path, ".md") {
			file = path + "/left/SKILL.md"
		}
		if err := box.WriteFile(t.Context(), file, []byte("left before the run\n")); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"left", "crewlet:old", ".system"} {
		if err := box.WriteFile(t.Context(), p.CodexSkills()+"/"+name+"/SKILL.md", []byte("x\n")); err != nil {
			t.Fatal(err)
		}
	}
	// WHAT CODEX FINDS AS IT STARTS, recorded by its app-server's stand-in.
	quoted := make([]string, len(starts))
	for i, path := range starts {
		quoted[i] = shellword.Quote(path)
	}
	look := `for f in ` + strings.Join(quoted, " ") + `; do [ ! -e "$f" ] || echo "$f"; done > "$rec/left"` + "\n" +
		`  ls -A "$CODEX_HOME/skills" > "$rec/skills"` + "\n"
	path := p.BinDir() + "/codex"
	standIn := boxFile(t, box, path)
	if !strings.Contains(standIn, "\napp-server)\n") {
		t.Fatal("the codex stand-in has no app-server branch")
	}
	writeBoxStandIn(t, box, path, strings.Replace(standIn, "\napp-server)\n", "\napp-server)\n  "+look, 1))

	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go",
		Skills: []sandbox.Skill{{Name: "deploy", Description: "How we deploy.", Body: "Run make deploy."}}})
	if !res.Success {
		t.Fatalf("the run failed: %s", res.Error)
	}
	if left := strings.Join(recorded(t, record, "left"), " "); left != "" {
		t.Errorf("Codex started beside %s, left in its home before the run", left)
	}
	if skills := recorded(t, record, "skills"); !slices.Equal(skills, []string{".system", "crewlet:deploy"}) {
		t.Errorf("Codex started with the skills %q, want the run's own beside .system", skills)
	}
}
