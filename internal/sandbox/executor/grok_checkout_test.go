package executor_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// grokCheckoutStandIn is a `grok` that records in $HOME/seen what Grok Build
// 1.0.92 starts on (~/.grok/AGENTS.md, the SKILL.md files of ~/.grok/skills,
// its settings), then, where $HOME/clone-src names a directory, copies it to
// $HOME/clone-dst as a clone during a tool call would and runs the settings'
// grok-nested hook as Grok runs a PostToolUse hook, once for a subagent's call
// (nested-sub) and twice for the main loop's (nested-1, nested-2), recording
// the skills after them (skills-late); and then does what $HOME/mode says:
// exit 1, signal its script as a teardown does and wait to be ended, kill its
// script outright, or finish.
const grokCheckoutStandIn = `#!/bin/sh
case "$1" in --version) echo 'grok 1.0.92 (b06756fd6256)'; exit 0;; inspect) exit 1;; esac
cat > /dev/null
s="$HOME/seen"; mkdir -p "$s"
if [ -f "$HOME/.grok/AGENTS.md" ]; then cat "$HOME/.grok/AGENTS.md" > "$s/agents"; fi
skills() { (cd "$HOME/.grok/skills" 2> /dev/null && find . -name SKILL.md | LC_ALL=C sort | while IFS= read -r f; do printf '== %s\n' "$f"; cat "$f"; done); }
skills > "$s/skills"
cp "$HOME/.grok/config.toml" "$s/config.toml"
hook=$(sed -n 's/^.*command = "\(.*-adapter grok-nested.*\)".*$/\1/p' "$HOME/.grok/config.toml" | head -n 1)
if [ -f "$HOME/clone-src" ]; then
  cp -R "$(cat "$HOME/clone-src")" "$(cat "$HOME/clone-dst")"
  printf '%s' '{"hook_event_name":"PostToolUse","tool_name":"run_terminal_command","subagentType":"general-purpose"}' | eval "$hook" > "$s/nested-sub" 2> /dev/null
  printf '%s' '{"hook_event_name":"PostToolUse","tool_name":"run_terminal_command"}' | eval "$hook" > "$s/nested-1" 2> /dev/null
  printf '%s' '{"hook_event_name":"PostToolUse","tool_name":"run_terminal_command"}' | eval "$hook" > "$s/nested-2" 2> /dev/null
  skills > "$s/skills-late"
fi
case "$(cat "$HOME/mode" 2> /dev/null)" in
fail) exit 1;;
term|int|hup) kill -"$(tr a-z A-Z < "$HOME/mode")" "$PPID"; exec sleep 60;;
kill) kill -KILL "$PPID"; exit 0;;
esac
echo '{"type":"text","data":"done"}'; echo '{"type":"end","stopReason":"end_turn"}'
`

// grokSeen is what the stand-in recorded under name, "" for nothing.
func grokSeen(t *testing.T, box sandbox.Sandbox, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(box.Home(), "seen", name))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// grokNestedSeen is the context the stand-in's firing name was handed, ""
// for none.
func grokNestedSeen(t *testing.T, box sandbox.Sandbox, name string) string {
	t.Helper()
	raw := grokSeen(t, box, name)
	if raw == "" {
		return ""
	}
	var answer struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(raw), &answer); err != nil || answer.HookSpecificOutput.HookEventName != "PostToolUse" {
		t.Fatalf("the firing %s answered %q (%v)", name, raw, err)
	}
	return answer.HookSpecificOutput.AdditionalContext
}

// boxHome is the box home as a launch in it resolves it.
func boxHome(t *testing.T, box sandbox.Sandbox) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(box.Home())
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A GROK RUN: of a checkout at
// ~/repo (W is the box home) holding every kind a trusted Grok Build 1.0.92
// session started in it loads, and more, each instruction file and each file
// of .grok/rules and .claude/rules is handed whole under its root in
// ~/.grok/AGENTS.md (after the home's own, which is back after the run), its
// skills of .grok, .agents and .cursor (nested too) and its .claude command
// as `repo:<skill>` in ~/.grok/skills; nothing else of it (no MCP server,
// hook, setting, agent or plugin) reaches what grok starts on. A clone during
// the run is handed at the main loop's next call, once, never to a
// subagent's; no copy outlives the run. Mutation: drop the gate or the nested
// hook, hand a rules subdirectory or another name, hand to a subagent, or keep
// the copies, and a check goes red.
func TestWhatACheckoutBelowTheStartDirectoryBringsAGrokRun(t *testing.T) {
	t.Parallel()
	repo := piCheckout("repo", map[string]string{
		"Agents.md": "REPO-AGENTS-CAMEL\n", "CLAUDE.md": "REPO-CLAUDE\n", "CLAUDE.local.md": "REPO-LOCAL\n",
		"AGENTS.md": "REPO-AGENTS\n", ".claude/CLAUDE.md": "REPO-DOT-CLAUDE\n", ".grok/rules/style.md": "REPO-RULE\n",
		".claude/rules/c.md":                 "REPO-CLAUDE-RULE\n",
		".grok/skills/check/SKILL.md":        "---\nname: check\ndescription: Checks.\n---\nGROK-CHECK\n",
		".agents/skills/lint/SKILL.md":       "---\nname: lint\ndescription: Lints.\n---\nLint.\n",
		".cursor/skills/group/deep/SKILL.md": "---\nname: deep\ndescription: Deep.\n---\nDeep.\n",
		".claude/commands/fix.md":            "---\ndescription: Fixes.\n---\nFix.\n",
		"agents.md":                          "REPO-LOWER\n", "GROK.md": "REPO-GROK\n", ".grok/AGENTS.md": "REPO-GROK-AGENTS\n",
		"GEMINI.md": "REPO-GEMINI\n", "sub/AGENTS.md": "REPO-SUB\n", ".grok/rules/deeper/r.md": "REPO-DEEP-RULE\n",
		".cursor/rules/r.mdc": "REPO-CURSOR-RULE\n", ".github/copilot-instructions.md": "REPO-COPILOT\n",
		".grok/config.toml":             "[mcp_servers.repomcp]\nurl = \"http://127.0.0.1:9/REPO-MCP\"\n",
		".mcp.json":                     `{"mcpServers":{"REPO-DOT-MCP":{"command":"x"}}}`,
		".claude/settings.json":         `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"REPO-HOOK"}]}]}}`,
		".grok/hooks/hooks.json":        `{"hooks":{"PreToolUse":[{"hooks":[{"command":"REPO-GROK-HOOK"}]}]}}`,
		".grok/agents/a.md":             "---\nname: a\ndescription: REPO-SUBAGENT\n---\n",
		".claude/plugins/p/plugin.json": `{"name":"REPO-PLUGIN"}`,
		".codex/skills/x/SKILL.md":      "---\nname: x\ndescription: REPO-CODEX-SKILL\n---\n",
	})
	marks := []string{"REPO-LOWER", "REPO-GROK", "REPO-GEMINI", "REPO-SUB", "REPO-DEEP-RULE", "REPO-CURSOR-RULE",
		"REPO-COPILOT", "REPO-MCP", "REPO-DOT-MCP", "REPO-HOOK", "REPO-SUBAGENT", "REPO-PLUGIN", "REPO-CODEX-SKILL"}
	for _, own := range []string{"", "HOME-OWN\n"} {
		t.Run(fmt.Sprintf("home's own %q", own), func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewGrok(), map[string]string{"grok": grokCheckoutStandIn})
			p := executor.PathsFor(box)
			plant(t, box, repo)
			if own != "" {
				plant(t, box, map[string]string{".grok/AGENTS.md": own})
			}
			late := t.TempDir()
			for rel, body := range piCheckout("late", map[string]string{"AGENTS.md": "LATE-INSTRUCTIONS\n",
				".grok/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes late.\n---\nFix.\n",
				".grok/config.toml":         "[mcp_servers.late]\nurl = \"http://127.0.0.1:9/LATE-MCP\"\n"}) {
				plantFile(t, filepath.Join(late, rel), body)
			}
			plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"),
				"clone-dst": filepath.Join(box.Home(), "late")})
			if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
				t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
			}
			home := boxHome(t, box)

			// Instructions, whole, after the home's own.
			section := "for `repo` with root " + home + "/repo\n\nREPO-AGENTS-CAMEL\n\n\nREPO-CLAUDE\n\n\nREPO-LOCAL\n\n\n" +
				"REPO-AGENTS\n\n\nREPO-DOT-CLAUDE\n\n\nREPO-RULE\n\n\nREPO-CLAUDE-RULE\n"
			want := section
			if own != "" {
				want = own + "\n\n" + section
			}
			if got := grokSeen(t, box, "agents"); got != want {
				t.Errorf("instructions: grok started on ~/.grok/AGENTS.md\n%q\nwant\n%q", got, want)
			}
			if got := readHomeFile(t, box, ".grok/AGENTS.md"); got != own {
				t.Errorf("instructions: after the run ~/.grok/AGENTS.md is %q, want the home's own %q", got, own)
			}
			// Skills.
			skills := grokSeen(t, box, "skills")
			for _, want := range []string{"== ./repo:check/SKILL.md\n---\nname: \"repo:check\"\ndescription: Checks.\n---\nGROK-CHECK\n",
				"== ./repo:deep/SKILL.md\n", "== ./repo:fix/SKILL.md\n---\nname: \"repo:fix\"\ndescription: Fixes.\n---\nFix.\n",
				"== ./repo:lint/SKILL.md\n"} {
				if !strings.Contains(skills, want) {
					t.Errorf("skills: grok started on ~/.grok/skills\n%s\nwithout %q", skills, want)
				}
			}
			// Everything else.
			pinned := grokSeen(t, box, "agents") + skills + grokSeen(t, box, "config.toml")
			for _, mark := range append(marks, "LATE-MCP") {
				if strings.Contains(pinned, mark) {
					t.Errorf("%s: what the checkout should not bring reached what grok starts on", mark)
				}
			}
			// A checkout cloned during the run.
			if got := grokNestedSeen(t, box, "nested-sub"); got != "" {
				t.Errorf("clone: a subagent's call was handed %q", got)
			}
			wantLate := "for `late` with root " + home + "/late\n\n## From: " + home + "/late/AGENTS.md\nLATE-INSTRUCTIONS\n" +
				"\n\n# Skills of " + home + "/late\n\nEach is a set of local instructions to follow, stored in its " +
				"`SKILL.md` file; to use one, read that file.\n- late-fix: Fixes late. (file: " + p.GrokSkills() +
				"/late:fix/SKILL.md)"
			if got := grokNestedSeen(t, box, "nested-1"); got != wantLate {
				t.Errorf("clone: the main loop's call was handed\n%q\nwant\n%q", got, wantLate)
			}
			if got := grokNestedSeen(t, box, "nested-2"); got != "" {
				t.Errorf("clone: a later call was handed %q", got)
			}
			if !strings.Contains(grokSeen(t, box, "skills-late"), "== ./late:fix/SKILL.md\n") {
				t.Errorf("clone: its skill was not in ~/.grok/skills while the run held it:\n%s", grokSeen(t, box, "skills-late"))
			}
			if left := grokCopies(t, box); len(left) != 0 {
				t.Errorf("~/.grok/skills holds %q after the run, want the copies removed", left)
			}
			if errs := boxFile(t, box, p.Err()); strings.Contains(errs, "no note helper") {
				t.Errorf("the run said its checkouts were not handed:\n%s", errs)
			}
		})
	}
}

// readHomeFile is a file under the box home, "" where there is none.
func readHomeFile(t *testing.T, box sandbox.Sandbox, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(box.Home(), rel))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// grokCopies is the checkouts' copies in ~/.grok/skills: every entry but
// the seats' own.
func grokCopies(t *testing.T, box sandbox.Sandbox) []string {
	t.Helper()
	entries, err := os.ReadDir(executor.PathsFor(box).GrokSkills())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), sandbox.SkillNamespace+":") {
			out = append(out, e.Name())
		}
	}
	return out
}

// grokGateFails is a `mkdir` that fails for the run's checkout state, so the
// gate stops after it has held the home's file.
const grokGateFails = `#!/bin/sh
for a; do case "$a" in */grok-checkouts) exit 1;; esac; done
exec /bin/mkdir "$@"
`

// ~/.GROK/AGENTS.MD IS THE HOME'S OWN, AND NO COPY IS LEFT, HOWEVER A GROK RUN
// ENDS: put back once grok has stopped, succeeded or failed, where the gate
// fails after holding, and on each teardown signal (TERM, INT, HUP), ending
// grok first; a run killed outright is put back by the next run's clear,
// whichever harness it drives, its skip log dropped. Nothing held is left.
//
// Mutation: drop the put-back after grok, the EXIT trap or a signal's, or
// Grok's homeKeeper, and a row goes red.
func TestAGrokRunPutsTheHomesInstructionsBackHoweverItEnds(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ok", "fail", "gate", "term", "int", "hup", "kill"} {
		for _, own := range []string{"", "OWN\n"} {
			t.Run(fmt.Sprintf("%s, home's own %q", mode, own), func(t *testing.T) {
				t.Parallel()
				runner, box := standInBox(t, executor.NewGrok(), map[string]string{"grok": grokCheckoutStandIn})
				p := executor.PathsFor(box)
				plant(t, box, piCheckout("repo", map[string]string{"AGENTS.md": "REPO\n",
					".grok/skills/s/SKILL.md": "---\nname: s\ndescription: S.\n---\n"}))
				plant(t, box, map[string]string{"mode": mode})
				if own != "" {
					plant(t, box, map[string]string{".grok/AGENTS.md": own})
				}
				if mode == "gate" {
					writeBoxStandIn(t, box, p.BinDir()+"/mkdir", grokGateFails)
				}
				handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "go"})
				if err != nil {
					t.Fatalf("Start: %v", err)
				}
				awaitOver(t, runner, box, handle, time.Minute, "the stand-in ends at once or asks its script to")
				if mode != "gate" {
					want := "for `repo` with root " + boxHome(t, box) + "/repo\n\nREPO\n"
					if own != "" {
						want = own + "\n\n" + want
					}
					if got := grokSeen(t, box, "agents"); got != want {
						t.Fatalf("grok started on ~/.grok/AGENTS.md\n%q\nwant\n%q", got, want)
					}
					if !strings.Contains(grokSeen(t, box, "skills"), "== ./repo:s/SKILL.md") {
						t.Fatalf("grok started without the checkout's skill:\n%s", grokSeen(t, box, "skills"))
					}
				}
				if mode == "kill" {
					plantFile(t, p.GrokCheckouts()+"/skipped", "notehelper: a line no script printed\n")
					if err := executor.NewClaudeCode().ClearArtifacts(t.Context(), box); err != nil {
						t.Fatalf("the next run's clear: %v", err)
					}
					if _, err := os.Lstat(p.GrokCheckouts() + "/skipped"); err == nil {
						t.Error("the killed run's skip log outlived the next run's clear")
					}
				}
				if got := readHomeFile(t, box, ".grok/AGENTS.md"); got != own {
					t.Errorf("after the run ~/.grok/AGENTS.md is %q, want the home's own %q", got, own)
				}
				if left := grokCopies(t, box); len(left) != 0 {
					t.Errorf("~/.grok/skills holds %q after the run, want the copies removed", left)
				}
				for _, held := range []string{"", ".none", ".link", ".tmp"} {
					if _, err := os.Lstat(p.GrokHomeInstructions() + held); err == nil {
						t.Errorf("%s is left after the run", p.GrokHomeInstructions()+held)
					}
				}
			})
		}
	}
}

// A COPY A HOME SKILL WOULD HIDE IS A LINE OF THE RUN'S ERROR STREAM, ONCE,
// AFTER GROK: a clone whose skill `late:deploy` Grok would list as
// late-deploy, the name of the home's own late-deploy (row x3: the copy is
// dropped, unlisted), is handed with the line in the hook's answer, no copy
// made, and the script prints the line once grok has stopped, once; the home
// skill is untouched.
//
// Mutation: compare the written name, drop the print, or print it twice,
// and a row goes red.
func TestAGrokCopyAHomeSkillHidesIsSaidOnce(t *testing.T) {
	t.Parallel()
	runner, box := standInBox(t, executor.NewGrok(), map[string]string{"grok": grokCheckoutStandIn})
	p := executor.PathsFor(box)
	homeSkill := "---\nname: late-deploy\ndescription: The person's.\n---\n"
	plant(t, box, map[string]string{".grok/skills/late-deploy/SKILL.md": homeSkill})
	late := t.TempDir()
	for rel, body := range piCheckout("late", map[string]string{
		".grok/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploys.\n---\n"}) {
		plantFile(t, filepath.Join(late, rel), body)
	}
	plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"),
		"clone-dst": filepath.Join(box.Home(), "late")})
	runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
	line := "the skill " + boxHome(t, box) + "/late/.grok/skills/deploy is left out: the home's " + p.GrokSkills() +
		"/late-deploy, which Grok Build lists as late-deploy, takes the name late-deploy it would list the copy under"
	if got := grokNestedSeen(t, box, "nested-1"); !strings.Contains(got, "- "+line) {
		t.Errorf("the hook's answer after the clone was %q, want the line in it", got)
	}
	errs := boxFile(t, box, p.Err())
	if n := strings.Count(errs, line); n != 1 {
		t.Errorf("the run's error stream says the line %d times, want once:\n%s", n, errs)
	}
	if strings.Contains(grokSeen(t, box, "skills-late"), "late:deploy") {
		t.Errorf("the hidden copy was made:\n%s", grokSeen(t, box, "skills-late"))
	}
	if got := readHomeFile(t, box, ".grok/skills/late-deploy/SKILL.md"); got != homeSkill {
		t.Errorf("the home skill reads %q after the run", got)
	}
}
