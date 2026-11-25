package executor_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// copilotRepoStandIn is the `copilot` the checkout rows run: it answers the
// release check, records the home's instruction file, the instruction
// directories and the home's skills as the harness starts, and ends the run
// with an answer.
const copilotRepoStandIn = `#!/bin/sh
case "$1" in --version) echo 'GitHub Copilot CLI 1.0.114.'; exit 0;; esac
s="$HOME/seen"; mkdir -p "$s"
cat "$COPILOT_HOME/copilot-instructions.md" > "$s/instructions" 2> /dev/null
printf '%s' "${COPILOT_CUSTOM_INSTRUCTIONS_DIRS-unset}" > "$s/dirs"
[ ! -d "$COPILOT_HOME/skills" ] || cp -R "$COPILOT_HOME/skills" "$s/skills"
(cd "$COPILOT_HOME" && find . -type f -print) | sort > "$s/files"
printf '%s\n' '{"type":"assistant.message","data":{"content":"done"}}' '{"type":"result","exitCode":0}'
`

// plantTree writes files, by path relative to root.
func plantTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		plantFile(t, filepath.Join(root, rel), body)
	}
}

// copilotClone is the files of a finished clone's git directory under name.
func copilotClone(name string) map[string]string {
	return map[string]string{name + "/.git/HEAD": "ref: refs/heads/main\n", name + "/.git/index": "DIRC",
		name + "/.git/refs/heads/main": strings.Repeat("0", 40) + "\n"}
}

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A COPILOT RUN at launch,
// in a real box: its .github/instructions through COPILOT_CUSTOM_INSTRUCTIONS_DIRS
// after the person's own value; its root files and unscoped .claude/rules in the
// home's copilot-instructions.md after the run's and the home's own, a scoped
// rule left to Copilot's lazy load; its skills as `repo:<skill>`, .github's
// winning a name, one past 64 characters cut to 64. Its hooks, MCP servers,
// extensions, agents and settings reach nothing, and once the run ends the
// home's file is its own and no copy is left.
//
// Mutation: drop the gate, set the variable to the checkout alone, leave out
// the home's own text, keep a 65-character name, or leave the copies.
func TestWhatACheckoutBelowTheStartDirectoryBringsACopilotRun(t *testing.T) {
	t.Parallel()
	runner, box := standInBox(t, executor.NewCopilot(), map[string]string{"copilot": copilotRepoStandIn})
	p := executor.PathsFor(box)
	home := box.Home()
	long := strings.Repeat("l", 60)
	files := copilotClone("repo")
	for rel, body := range map[string]string{
		"repo/.github/copilot-instructions.md":          "CPI\n",
		"repo/AGENTS.md":                                "AGENTS\n",
		"repo/CLAUDE.md":                                "CLAUDE\n",
		"repo/GEMINI.md":                                "GEMINI\n",
		"repo/.claude/rules/r.md":                       "RULE\n",
		"repo/.claude/rules/py.md":                      "---\napplyTo: \"**/*.py\"\n---\nPY-RULE\n",
		"repo/.github/instructions/all.instructions.md": "INSTR\n",
		"repo/.github/skills/check/SKILL.md":            "---\nname: check\ndescription: GitHub's.\n---\n",
		"repo/.claude/skills/check/SKILL.md":            "---\nname: check\ndescription: Claude's.\n---\n",
		"repo/.agents/skills/lint/SKILL.md":             "---\nname: lint\ndescription: Lints.\n---\n",
		"repo/.claude/skills/" + long + "/SKILL.md":     "---\nname: " + long + "\ndescription: Long.\n---\n",
		"repo/.github/hooks/repo.json":                  `{"hooks":{"sessionStart":[{"bash":"REPO-HOOK"}]}}`,
		"repo/.github/extensions/repo/extension.mjs":    "// REPO-EXTENSION\n",
		"repo/.github/agents/repo.agent.md":             "REPO-AGENT\n",
		"repo/.mcp.json":                                `{"mcpServers":{"REPO-MCP":{"command":"x"}}}`,
		"repo/.github/copilot/settings.json":            `{"REPO-SETTINGS":true}`,
		"repo/.claude/settings.json":                    `{"REPO-CLAUDE-SETTINGS":true}`,
	} {
		files[rel] = body
	}
	plantTree(t, home+"/workspace", files)
	plantFile(t, p.CopilotInstructions(), "HOME-OWN\n")
	plantFile(t, p.CopilotSkills()+"/mine/SKILL.md", "---\nname: mine\n---\n")

	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", System: "Work well.",
		Env: map[string]string{"COPILOT_CUSTOM_INSTRUCTIONS_DIRS": home + "/own"}})
	if !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
	}
	work, err := filepath.EvalSymlinks(home + "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	seen := func(name string) string { return boxFile(t, box, home+"/seen/"+name) }

	t.Run(".github/instructions", func(t *testing.T) {
		if got, want := seen("dirs"), home+"/own,"+work+"/repo/.github/instructions"; got != want {
			t.Errorf("COPILOT_CUSTOM_INSTRUCTIONS_DIRS was %q as Copilot started, want %q", got, want)
		}
	})
	t.Run("root files and rules", func(t *testing.T) {
		want := "\n\nHOME-OWN\n\n\nfor `repo` with root " + work + "/repo\n\n" +
			"CPI\n\n\nAGENTS\n\n\nCLAUDE\n\n\nGEMINI\n\n\nRULE\n"
		if got := seen("instructions"); !strings.HasPrefix(got, "Work well.") || !strings.HasSuffix(got, want) ||
			strings.Count(got, "HOME-OWN") != 1 {
			t.Errorf("copilot-instructions.md was\n%q\nas Copilot started, want the run's text, then\n%q", got, want)
		}
		if got := boxFile(t, box, p.CopilotInstructions()); got != "HOME-OWN\n" {
			t.Errorf("the home's copilot-instructions.md is %q after the run, want its own", got)
		}
	})
	t.Run("skills", func(t *testing.T) {
		if got := boxFile(t, box, home+"/seen/skills/repo:check/SKILL.md"); !strings.Contains(got, "GitHub's") {
			t.Errorf("repo:check was %q, want .github/skills' over .claude/skills'", got)
		}
		if got := boxFile(t, box, home+"/seen/skills/repo:lint/SKILL.md"); !strings.HasPrefix(got, "---\nname: \"repo:lint\"\n") {
			t.Errorf("repo:lint was %q, want .agents/skills' renamed", got)
		}
		entries, err := os.ReadDir(home + "/seen/skills")
		if err != nil {
			t.Fatal(err)
		}
		var cut []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "repo:lll") {
				cut = append(cut, e.Name())
			}
		}
		if len(cut) != 1 || len(cut[0]) != 64 {
			t.Errorf("the 65-character name was copied as %q, want one name of 64, which Copilot lists", cut)
		}
		left, err := os.ReadDir(p.CopilotSkills())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range left {
			names = append(names, e.Name())
		}
		if !slices.Equal(names, []string{"mine"}) {
			t.Errorf("the home's skills after the run are %q, want its own alone", names)
		}
	})
	t.Run("nothing else of the checkout", func(t *testing.T) {
		var held []string
		for _, f := range strings.Fields(seen("files")) {
			held = append(held, boxFile(t, box, p.CopilotHome()+"/"+strings.TrimPrefix(f, "./")))
		}
		held = append(held, seen("instructions"), seen("dirs"))
		for _, mark := range []string{"PY-RULE", "REPO-HOOK", "REPO-EXTENSION", "REPO-AGENT", "REPO-MCP",
			"REPO-SETTINGS", "REPO-CLAUDE-SETTINGS"} {
			for _, h := range held {
				if strings.Contains(h, mark) {
					t.Errorf("%s reached the run's home: %.200s", mark, h)
				}
			}
		}
	})
}
