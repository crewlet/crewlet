package executor_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// museCheckoutStandIn is a `muse` that records its calls ($HOME/muse-calls),
// keeps the plugin it is asked to install ($HOME/seen-plugin.json) and, for
// `muse exec`, records in $HOME/seen what Muse Code 1.4.25 reads as it starts:
// the home's rules files ($CONFIG_DIR's AGENTS.md and CLAUDE.md) and every
// SKILL.md under ~/.agents/skills; then, where $HOME/clone-src names a
// directory, copies it to $HOME/clone-dst as a clone during a tool call would
// and fires the plugin's nested hook where it has one, as a subagent's call
// (session_id its own thread's) and twice as the main loop's (session_id
// MUSE_SESSION_ID, the run's --session-id), recording each answer
// (nested-sub, nested-1, nested-2) and the skills after them (skills-late).
const museCheckoutStandIn = `#!/bin/sh
printf '%s\n' "$*" >> "$HOME/muse-calls"
case "$1" in
--version) echo 'Muse Code 1.4.25 (1.4.25-R9361.1)'; exit 0;;
plugins) [ "$2" != install ] || cp "$3/.muse-plugin/plugin.json" "$HOME/seen-plugin.json"; exit 0;;
exec) ;;
*) exit 0;;
esac
s=; prev=; for a; do [ "$prev" != --session-id ] || s=$a; prev=$a; done
seen=$HOME/seen; mkdir -p "$seen"
skills() { (cd "$HOME/.agents/skills" 2> /dev/null && find . -name SKILL.md | sort | while read -r f; do echo "== $f"; cat "$f"; done); }
for f in AGENTS.md CLAUDE.md; do [ ! -e "$HOME/.config/muse/$f" ] || cp "$HOME/.config/muse/$f" "$seen/config-$f"; done
skills > "$seen/skills"
if [ -f "$HOME/clone-src" ]; then
  cp -R "$(cat "$HOME/clone-src")" "$(cat "$HOME/clone-dst")"
  hook() { printf '{"hook_event_name":"PostToolUse","session_id":"%s","tool_name":"bash"}' "$1" | MUSE_SESSION_ID=$s MUSE_THREAD_ID=$1 "$HOME/.crewlet/muse-notehelper" -adapter muse-nested -config "$HOME/.crewlet/muse-checkouts/nested.json"; }
  if grep -q '"muse-nested"' "$HOME/seen-plugin.json"; then
    hook child-thread > "$seen/nested-sub"; hook "$s" > "$seen/nested-1"; hook "$s" > "$seen/nested-2"
  fi
  skills > "$seen/skills-late"
fi
exit 0
`

// museSeen is what the stand-in recorded under name, "" for nothing.
func museSeen(t *testing.T, box sandbox.Sandbox, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(box.Home(), "seen", name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(raw)
}

// museRulesFiles is the home's rules files Muse loads the first of: its
// $CONFIG_DIR's AGENTS.md and CLAUDE.md and the foreign ones, relative to the
// home, read now.
func museRulesFiles(t *testing.T, box sandbox.Sandbox) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, rel := range []string{".config/muse/AGENTS.md", ".config/muse/CLAUDE.md", ".claude/CLAUDE.md", ".codex/AGENTS.md"} {
		if raw, err := os.ReadFile(filepath.Join(box.Home(), rel)); err == nil {
			out[rel] = string(raw)
		}
	}
	return out
}

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A MUSE RUN: ./repo's one
// rules file, under its root, in the home's rules file Muse loads (after the
// person's $CONFIG_DIR/AGENTS.md or CLAUDE.md, else in an AGENTS.md of the
// run's opening with the foreign rules Muse would have loaded, unless
// switched off or a nearer file is there), the home's own again after; its
// .agents, .claude and .codex skills as `repo:<skill>`, none a home skill
// hides (said once); nothing else. Too big beside the start directory's own
// AGENTS.md, it is named; a clone during the run is handed once, to the main
// loop alone; no copy outlives the run. Mutation: drop the gate, the foreign
// copy or switch, the budget, the subagent check, the hide check, or
// restore's removeCopies, and a row goes red.
func TestWhatACheckoutBelowTheStartDirectoryBringsAMuseRun(t *testing.T) {
	t.Parallel()
	repo := piCheckout("workspace/repo", map[string]string{
		"AGENTS.md": "REPO-INSTRUCTIONS\n", "CLAUDE.md": "REPO-CLAUDE\n", ".agents/AGENTS.md": "REPO-DOT-AGENTS\n",
		".claude/CLAUDE.md": "REPO-DOT-CLAUDE\n", "AGENTS.override.md": "REPO-OVERRIDE\n", "MUSE.md": "REPO-MUSE\n",
		".muse/settings.json": `{"REPO-SETTINGS":1}`, ".muse/hooks.json": `{"REPO-HOOKS":1}`,
		".mcp.json": `{"mcpServers":{"REPO-MCP":{"command":"x"}}}`, ".muse/agents/a.md": "REPO-AGENT\n",
		".agents/skills/check/SKILL.md":  "---\nname: check\ndescription: Checks.\n---\nAGENTS-CHECK\n",
		".claude/skills/check/SKILL.md":  "---\nname: check\ndescription: Not this one.\n---\nCLAUDE-CHECK\n",
		".claude/skills/style/SKILL.md":  "---\nname: style\ndescription: Styles.\n---\nStyle.\n",
		".codex/skills/lint/SKILL.md":    "---\nname: lint\ndescription: Lints.\n---\nLint.\n",
		".codex/skills/hidden/SKILL.md":  "---\nname: hidden\ndescription: Hidden.\n---\nHIDDEN-COPY\n",
		".muse/skills/m/SKILL.md":        "---\nname: m\ndescription: Muse's.\n---\nREPO-MUSE-SKILL\n",
		".github/skills/g/SKILL.md":      "---\nname: g\ndescription: Copilot's.\n---\nREPO-GITHUB-SKILL\n",
		".agents/skills/deep/x/SKILL.md": "---\nname: x\ndescription: Deep.\n---\nREPO-DEEP\n",
	})
	marks := []string{"REPO-CLAUDE", "REPO-DOT-AGENTS", "REPO-DOT-CLAUDE", "REPO-OVERRIDE", "REPO-MUSE", "REPO-SETTINGS",
		"REPO-HOOKS", "REPO-MCP", "REPO-AGENT", "CLAUDE-CHECK", "HIDDEN-COPY", "REPO-GITHUB-SKILL", "REPO-DEEP"}
	for _, c := range []struct {
		name, settings, into, opens string
		home                        map[string]string
		late, big                   bool
	}{
		{name: "a home with none, and a checkout cloned during the run", into: ".config/muse/AGENTS.md", late: true},
		{name: "the home's own AGENTS.md", into: ".config/muse/AGENTS.md", opens: "HOME-AGENTS\n",
			home: map[string]string{".config/muse/AGENTS.md": "HOME-AGENTS\n", ".config/muse/CLAUDE.md": "HOME-CLAUDE\n"}},
		{name: "the home's only CLAUDE.md, beside a foreign file", into: ".config/muse/CLAUDE.md", opens: "HOME-CLAUDE\n",
			home: map[string]string{".config/muse/CLAUDE.md": "HOME-CLAUDE\n", ".claude/CLAUDE.md": "FOREIGN-CLAUDE\n"}},
		{name: "Claude Code's personal rules, which Muse would load", into: ".config/muse/AGENTS.md",
			opens: "FOREIGN-CLAUDE\n", home: map[string]string{".claude/CLAUDE.md": "FOREIGN-CLAUDE\n",
				".codex/AGENTS.md": "FOREIGN-CODEX\n"}},
		{name: "Codex's personal rules, which Muse would load", into: ".config/muse/AGENTS.md",
			opens: "FOREIGN-CODEX\n", home: map[string]string{".codex/AGENTS.md": "FOREIGN-CODEX\n"}},
		{name: "foreign personal rules switched off", into: ".config/muse/AGENTS.md",
			settings: `{"schema_version":1,"context":{"foreign_personal_rules":false}}`,
			home:     map[string]string{".claude/CLAUDE.md": "FOREIGN-CLAUDE\n"}},
		{name: "a checkout too big beside the start directory's own", into: ".config/muse/AGENTS.md", big: true},
		{name: "an empty AGENTS.md, which Muse loads before CLAUDE.md or a foreign file", into: ".config/muse/AGENTS.md",
			home: map[string]string{".config/muse/AGENTS.md": "", ".config/muse/CLAUDE.md": "HOME-CLAUDE\n",
				".claude/CLAUDE.md": "FOREIGN-CLAUDE\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewMuse(), map[string]string{"muse": museCheckoutStandIn})
			p := executor.PathsFor(box)
			plant(t, box, repo)
			plant(t, box, c.home)
			plant(t, box, map[string]string{".config/muse/skills/pdir/SKILL.md": "---\nname: repo:hidden\n---\n"})
			if c.settings != "" {
				plantFile(t, p.MuseSettings(), c.settings)
			}
			start := "START-OWN\n"
			if c.big {
				start = "START-OWN " + strings.Repeat("w", 30000) + "\n"
				plant(t, box, map[string]string{"workspace/repo/AGENTS.md": "REPO-INSTRUCTIONS " + strings.Repeat("r", 40000) + "\n"})
			}
			plant(t, box, map[string]string{"workspace/AGENTS.md": start})
			if c.late {
				late := t.TempDir()
				for rel, body := range piCheckout("late", map[string]string{"AGENTS.md": "LATE-INSTRUCTIONS\n",
					".agents/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\nFix.\n",
					".muse/settings.json":         `{"LATE-SETTINGS":1}`}) {
					plantFile(t, filepath.Join(late, rel), body)
				}
				plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"),
					"clone-dst": filepath.Join(box.Home(), "workspace", "late")})
			}
			before := museRulesFiles(t, box)
			runObserved(t, runner, box, sandbox.RunRequest{Brief: "go"})
			work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
			if err != nil {
				t.Fatal(err)
			}
			errs := boxFile(t, box, p.Err())

			// Instructions.
			section := "for `repo` with root " + work + "/repo\n\nREPO-INSTRUCTIONS\n"
			if c.big {
				section = "for `repo` with root " + work + "/repo\n\nread " + work + "/repo/AGENTS.md for its " +
					"instructions, which pass what Muse Code loads at the start beside the home's and the start " +
					"directory's own"
				if !strings.Contains(errs, "the instructions of "+work+"/repo are named, not handed") {
					t.Errorf("the run's error stream does not say the checkout was named:\n%s", errs)
				}
			}
			want := section
			if c.opens != "" {
				want = c.opens + "\n\n" + section
			}
			name := strings.TrimPrefix(c.into, ".config/muse/")
			if got := museSeen(t, box, "config-"+name); got != want {
				t.Errorf("instructions: muse started on %s reading\n%q\nwant\n%q", c.into, got, want)
			}
			for _, other := range []string{"AGENTS.md", "CLAUDE.md"} {
				if other != name && museSeen(t, box, "config-"+other) != before[".config/muse/"+other] {
					t.Errorf("instructions: $CONFIG_DIR/%s was not the home's own as muse started", other)
				}
			}
			if after := museRulesFiles(t, box); !maps.Equal(after, before) {
				t.Errorf("instructions: after the run the home's rules files are %q, want its own %q", after, before)
			}
			if got := boxFile(t, box, filepath.Join(box.Home(), "workspace/AGENTS.md")); got != start {
				t.Error("instructions: the start directory's own AGENTS.md was changed")
			}
			// Skills.
			skills := museSeen(t, box, "skills")
			for _, want := range []string{"== ./repo:check/SKILL.md\n---\nname: \"repo:check\"\ndescription: Checks.\n---\nAGENTS-CHECK\n",
				"== ./repo:style/SKILL.md", "== ./repo:lint/SKILL.md"} {
				if !strings.Contains(skills, want) {
					t.Errorf("skills: muse started without %q in ~/.agents/skills:\n%s", want, skills)
				}
			}
			line := "the skill " + work + "/repo/.codex/skills/hidden is left out: the home's " + p.MuseConfig() +
				"/skills/pdir, which Muse Code lists as repo:hidden, takes the name repo:hidden it would list the " +
				"copy under"
			if n := strings.Count(errs, line); n != 1 {
				t.Errorf("skills: the run's error stream says the hidden copy %d times, want once:\n%s", n, errs)
			}
			// Everything else.
			pinned := skills + museSeen(t, box, "config-AGENTS.md") + museSeen(t, box, "config-CLAUDE.md")
			for _, mark := range append(marks, "LATE-SETTINGS") {
				if strings.Contains(pinned, mark) {
					t.Errorf("%s: what the checkout should not bring reached what muse starts on", mark)
				}
			}
			if left, err := filepath.Glob(p.MuseSkills() + "/*:*"); err != nil || len(left) != 0 {
				t.Errorf("~/.agents/skills holds %v after the run (%v), want the copies removed", left, err)
			}
			if !c.late {
				return
			}
			// A checkout cloned during the run.
			late := work + "/late"
			wantLate := `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"for ` + "`late`" +
				` with root ` + late + `\n\n<rules-file scope=\"project\" path=\"` + late + `/AGENTS.md\">\nLATE-INSTRUCTIONS\n` +
				`</rules-file>\n\n# Skills of ` + late + `\n\nEach is a set of local instructions to follow, stored in its ` +
				"`SKILL.md`" + ` file; to use one, read that file.\n- late:fix: Fixes. (file: ` + p.MuseSkills() +
				`/late:fix/SKILL.md)"}}`
			if got := museSeen(t, box, "nested-sub"); got != "" {
				t.Errorf("clone: a subagent's call was answered %q", got)
			}
			if got := museSeen(t, box, "nested-1"); got != wantLate {
				t.Errorf("clone: the main loop's next call was answered\n%s\nwant\n%s", got, wantLate)
			}
			if got := museSeen(t, box, "nested-2"); got != "" {
				t.Errorf("clone: a later call was answered again: %q", got)
			}
			if !strings.Contains(museSeen(t, box, "skills-late"), "== ./late:fix/SKILL.md\n---\nname: \"late:fix\"") {
				t.Error("clone: its skill was not in ~/.agents/skills while the run held it")
			}
		})
	}
}

// museRestoreStandIn is a `muse` that records the home's rules files as `muse
// exec` starts, then does what $HOME/mode says: exit 0, exit 1, signal its
// script as a teardown does (TERM, INT or HUP) and wait to be ended, or kill
// its script outright.
const museRestoreStandIn = `#!/bin/sh
case "$1" in --version) echo 'Muse Code 1.4.25 (1.4.25-R9361.1)'; exit 0;; exec) ;; *) exit 0;; esac
for n in AGENTS.md CLAUDE.md; do
  if [ -e "$HOME/.config/muse/$n" ]; then echo "== $n"; cat "$HOME/.config/muse/$n"; fi
done > "$HOME/seen-rules"
case "$(cat "$HOME/mode")" in
ok) exit 0;;
fail) exit 1;;
term|int|hup) kill -"$(tr a-z A-Z < "$HOME/mode")" "$PPID"; exec sleep 60;;
kill) kill -KILL "$PPID"; exit 0;;
esac
exit 2
`

// museGateFails is a `mkdir` that fails for the run's checkout state, so the
// gate stops after it has held the home's rules file.
const museGateFails = `#!/bin/sh
for a; do case "$a" in */muse-checkouts) exit 1;; esac; done
exec /bin/mkdir "$@"
`

// THE HOME'S RULES FILE IS ITS OWN HOWEVER A MUSE RUN ENDS: put back once
// muse has stopped, succeeded or failed, where the gate fails after holding,
// and on each teardown signal (TERM, INT, HUP), ending muse first; a run
// killed outright is put back by the next run's clear, whichever harness it
// drives. A home's own AGENTS.md comes back as it was, an AGENTS.md the run
// made is gone, and nothing held is left.
//
// Mutation: drop the put-back after muse, the EXIT trap or a signal's, or
// Muse's homeKeeper, and a row goes red.
func TestAMuseRunPutsTheHomesRulesFileBackHoweverItEnds(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ok", "fail", "gate", "term", "int", "hup", "kill"} {
		for _, own := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, home's own %v", mode, own), func(t *testing.T) {
				t.Parallel()
				runner, box := standInBox(t, executor.NewMuse(), map[string]string{"muse": museRestoreStandIn})
				p := executor.PathsFor(box)
				plant(t, box, piCheckout("workspace/repo", map[string]string{"AGENTS.md": "REPO\n",
					".agents/skills/s/SKILL.md": "---\nname: s\ndescription: S.\n---\n"}))
				plant(t, box, map[string]string{"mode": mode})
				home := map[string]string{}
				if own {
					home[".config/muse/AGENTS.md"] = "OWN\n"
					plant(t, box, home)
				}
				if mode == "gate" {
					writeBoxStandIn(t, box, p.BinDir()+"/mkdir", museGateFails)
				}
				handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "go"})
				if err != nil {
					t.Fatalf("Start: %v", err)
				}
				awaitOver(t, runner, box, handle, time.Minute, "the stand-in ends at once or asks its script to")
				work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
				if err != nil {
					t.Fatal(err)
				}
				if mode != "gate" {
					want := "== AGENTS.md\nfor `repo` with root " + work + "/repo\n\nREPO\n"
					if own {
						want = "== AGENTS.md\nOWN\n\n\nfor `repo` with root " + work + "/repo\n\nREPO\n"
					}
					if got := boxFile(t, box, box.Home()+"/seen-rules"); got != want {
						t.Fatalf("muse started on the home's rules\n%q\nwant\n%q", got, want)
					}
				}
				if mode == "kill" {
					if err := executor.NewClaudeCode().ClearArtifacts(t.Context(), box); err != nil {
						t.Fatalf("the next run's clear: %v", err)
					}
				}
				if after := museRulesFiles(t, box); !maps.Equal(after, home) {
					t.Errorf("after the run the home's rules files are %q, want its own %q", after, home)
				}
				for _, held := range []string{"", ".none", ".live", ".tmp"} {
					if _, err := os.Lstat(p.MuseHomeRules() + held); err == nil {
						t.Errorf("%s is left after the run", p.MuseHomeRules()+held)
					}
				}
				if left, _ := filepath.Glob(p.MuseSkills() + "/repo:*"); len(left) != 0 {
					t.Errorf("the run's copies %v are left after it", left)
				}
			})
		}
	}
}
