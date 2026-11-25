package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// copilotRepoLaunched is a Copilot run's working directory under a fresh box
// home, its checkouts handed at launch by the real note helper as the run's
// gate runs it: the paths and the working directory as `pwd -P` reads it.
func copilotRepoLaunched(t *testing.T, files map[string]string) (Paths, string) {
	t.Helper()
	p, work := codexRepoBox(t, files)
	if err := os.MkdirAll(p.CopilotSkills(), 0o700); err != nil {
		t.Fatal(err)
	}
	bin, err := noteHelperBinary(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string][]byte{p.CopilotNoteHelper(): bin,
		p.CopilotCheckoutLaunch(): copilotCheckoutHome(p).launchConfig(copilotCheckouts(), nil)} {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, body, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(p.CopilotNoteHelper(), "-adapter", "checkout-launch", "-config", p.CopilotCheckoutLaunch()) //nolint:gosec // the engine's own helper
	cmd.Dir = work
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout-launch: %v\n%s", err, out)
	}
	return p, work
}

// A CHECKOUT CLONED DURING A COPILOT RUN IS LISTED AFTER THE MAIN AGENT'S
// NEXT TOOL, ONCE: the extension runs the real note helper after each of the
// main agent's tools and appends the listing of the skills it copied to that
// tool's own result, whole, the result's other fields kept; the next firing
// lists nothing again, and a subagent's tool hands nothing, leaving a later
// clone to the main agent's next tool.
//
// Mutation: hand on a subagent's firing, hand as additionalContext, replace the
// tool's result, or list a checkout twice.
func TestACheckoutClonedDuringACopilotRunIsListedOnce(t *testing.T) {
	p, work := copilotRepoLaunched(t, nil)
	probe := startCopilotProbe(t, copilotExtensionSettings{Helper: p.CopilotNoteHelper(),
		Nested: p.CopilotRepo() + "/nested.json"})
	probe.expect("joined")
	probe.expectArmed()
	clone := func(name string) {
		for rel, body := range map[string]string{".agents/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\n"} {
			path := filepath.Join(work, name, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		finishedClone(t, filepath.Join(work, name, ".git"))
	}
	fire := func(agent string) map[string]any {
		input := map[string]any{"toolName": "bash", "toolResult": map[string]any{"textResultForLlm": "cloned",
			"resultType": "success"}}
		if agent != "" {
			input["agentId"] = agent
		}
		probe.hook("onPostToolUse", input)
		return probe.expect("hooked").Output
	}
	listing := func(name string) string {
		return "cloned\n\n# Skills of " + work + "/" + name + "\n\nEach is a set of local instructions to follow, stored " +
			"in its `SKILL.md` file; to use one, read that file.\n- " + name + ":fix: Fixes. (file: " + p.CopilotSkills() +
			"/" + name + ":fix/SKILL.md)"
	}

	clone("late")
	out := fire("")
	result, _ := out["modifiedResult"].(map[string]any)
	if result["textResultForLlm"] != listing("late") || result["resultType"] != "success" || out["additionalContext"] != nil {
		t.Fatalf("the main agent's tool after the clone answered %v, want its result with the listing appended:\n%q",
			out, listing("late"))
	}
	if out := fire(""); out != nil {
		t.Errorf("the next firing answered %v, want nothing: the checkout was handed", out)
	}
	clone("later")
	if out := fire("sub-1"); out != nil {
		t.Errorf("a subagent's tool answered %v, want nothing", out)
	}
	result, _ = fire("")["modifiedResult"].(map[string]any)
	if text, _ := result["textResultForLlm"].(string); !strings.Contains(text, "later:fix") {
		t.Errorf("the main agent's next tool answered %v, want the later clone listed", result)
	}
}
