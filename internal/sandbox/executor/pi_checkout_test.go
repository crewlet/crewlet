package executor_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// piCheckoutStandIn is a `pi`, run under node, that records in $HOME/seen
// what pi 1.6.0 reads as it starts (its argv; the agent directory's files;
// each `--skill` directory's files), loads the run's extension, plays its
// first turn, then, where $HOME/clone-src names a directory, copies it to
// $HOME/clone-dst as a clone during a tool call would and plays that tool's
// result to the extension twice, recording what each answer made of it
// (nested-1, nested-2) and the --skill directories after them
// (skills-late), its nested config removed first where $HOME/break-nested
// is; where $HOME/break-home is, it makes the agent
// directory's AGENTS.override.md a directory no put-back can write; and it
// prints a finished answer.
const piCheckoutStandIn = `import { cpSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs"
import { join } from "node:path"
import { pathToFileURL } from "node:url"
const args = process.argv.slice(2)
if (args[0] === "--version") { console.log("1.6.0"); process.exit(0) }
const home = process.env.HOME, agent = process.env.PI_CODING_AGENT_DIR, seen = join(home, "seen")
mkdirSync(seen, { recursive: true })
const record = (name, value) => writeFileSync(join(seen, name), JSON.stringify(value))
const files = (dir) => {
  const out = {}
  if (!existsSync(dir)) return out
  for (const rel of readdirSync(dir, { recursive: true })) {
    if (statSync(join(dir, rel)).isFile()) out[rel] = readFileSync(join(dir, rel), "utf8")
  }
  return out
}
record("argv", args)
record("agent", files(agent))
const skills = {}
for (const [i, a] of args.entries()) if (args[i - 1] === "--skill") skills[a] = files(a)
record("skills", skills)
const out = (o) => process.stdout.write(JSON.stringify(o) + "\n")
const handlers = new Map()
const pi = {
  on: (name, h) => handlers.set(name, [...(handlers.get(name) ?? []), h]),
  registerMcpServer: () => {},
  appendEntry: (customType, data) => out({ type: "entry_appended", entry: { type: "custom", customType, data } }),
  getAllTools: () => ["read", "bash"].map((name) => ({ name })),
  getActiveTools: () => ["read", "bash"],
  getSettings: () => ({}),
}
const ctx = { cwd: process.cwd(), abort: () => {}, getContextUsage: () => undefined }
const fire = async (name, event) => {
  let answer = null
  for (const h of handlers.get(name) ?? []) { const a = await h(event, ctx); if (a !== undefined) answer = a }
  return answer
}
out({ type: "session", version: 3, id: "s", timestamp: new Date().toISOString(), cwd: process.cwd() })
for (const [i, a] of args.entries()) {
  if (args[i - 1] === "-e" && !a.startsWith("builtin:")) (await import(pathToFileURL(a).href)).default(pi)
}
await fire("before_agent_start", { systemPromptOptions: { sections: {} } })
await fire("agent_start", {})
await fire("turn_start", {})
if (existsSync(join(home, "clone-src"))) {
  cpSync(readFileSync(join(home, "clone-src"), "utf8"), readFileSync(join(home, "clone-dst"), "utf8"), { recursive: true })
  if (existsSync(join(home, "break-nested"))) rmSync(join(home, ".pi/crewlet/checkouts/nested.json"), { force: true })
  for (const i of [1, 2]) {
    record("nested-" + i, await fire("tool_result", { toolName: "bash", toolCallId: "call-" + i,
      input: { command: "git clone" }, content: [{ type: "text", text: "cloned" }], isError: false }))
  }
  for (const dir of Object.keys(skills)) skills[dir] = files(dir)
  record("skills-late", skills)
}
if (existsSync(join(home, "break-home"))) {
  rmSync(join(agent, "AGENTS.override.md"), { force: true })
  mkdirSync(join(agent, "AGENTS.override.md"))
}
out({ type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "done" }], stopReason: "stop" } })
await fire("session_shutdown", {})
out({ type: "agent_settled", aborted: false })
process.exit(0)
`

// piCheckoutBox is a real box with a Pi runner and [piCheckoutStandIn] as its
// `pi`.
func piCheckoutBox(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	node := prereqtest.OnPath(t, "node", "the Pi stand-in loads the run's extension under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
	runner, box := standInBox(t, executor.NewPi(), nil)
	bin := executor.PathsFor(box).BinDir()
	writeBoxStandIn(t, box, bin+"/pi-standin.mjs", piCheckoutStandIn)
	writeBoxStandIn(t, box, bin+"/pi", "#!/bin/sh\nexec "+shellword.Quote(node)+" "+
		shellword.Quote(bin+"/pi-standin.mjs")+" \"$@\"\n")
	return runner, box
}

// seenBy is what the stand-in recorded under name, decoded into v.
func seenBy(t *testing.T, box sandbox.Sandbox, name string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(boxFile(t, box, box.Home()+"/seen/"+name)), v); err != nil {
		t.Fatalf("the stand-in's %s: %v", name, err)
	}
}

// piCheckout is a finished clone's files under rel: its git directory and
// each of files.
func piCheckout(rel string, files map[string]string) map[string]string {
	out := map[string]string{rel + "/.git/HEAD": "ref: refs/heads/main\n", rel + "/.git/index": "DIRC",
		rel + "/.git/refs/heads/main": strings.Repeat("0", 40) + "\n"}
	for name, body := range files {
		out[rel+"/"+name] = body
	}
	return out
}

// piHomeNames is the names pi 1.6.0 loads the agent directory's one
// instruction file from, the first that exists (measured).
var piHomeNames = []string{"AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

// piInstructionFiles is the agent directory's files of the names pi 1.6.0
// loads its one instruction file from.
func piInstructionFiles(files map[string]string) map[string]string {
	out := map[string]string{}
	for _, name := range piHomeNames {
		if body, ok := files[name]; ok {
			out[name] = body
		}
	}
	return out
}

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A PI RUN: of ./repo, which
// holds every file pi 1.6.0 reads where it starts and more, the first of its
// five instruction names is handed under its root in the agent directory's
// file pi loads (the home's own, the section after its text, else one the run
// makes), the home's own again after; its skills a walk of .pi/skills and
// .agents/skills reaches come as `repo:<skill>` in a `--skill` directory of
// the run's own; nothing else of it reaches pi. A clone during the run is
// handed at the next tool result in pi's wrapping, once; no copy outlives it.
//
// Mutation: drop the gate, the `--skill`, the extension's hand or restore's
// removeCopies, read another name first, walk flat, or hand twice: red.
func TestWhatACheckoutBelowTheStartDirectoryBringsAPiRun(t *testing.T) {
	t.Parallel()
	repo := piCheckout("workspace/repo", map[string]string{
		"AGENTS.override.md": "REPO-INSTRUCTIONS\n", "AGENTS.md": "REPO-AGENTS\n", "AGENTS.MD": "REPO-UPPER\n",
		"CLAUDE.md": "REPO-CLAUDE\n", "CLAUDE.MD": "REPO-CLAUDE-UPPER\n", "CLAUDE.local.md": "REPO-LOCAL\n",
		".claude/CLAUDE.md": "REPO-DOT-CLAUDE\n", "GEMINI.md": "REPO-GEMINI\n", ".pi/AGENTS.md": "REPO-PI-AGENTS\n",
		".github/copilot-instructions.md": "REPO-COPILOT\n",
		".pi/settings.json":               `{"theme":"REPO-SETTINGS"}`, ".pi/mcp.json": `{"mcpServers":{"REPO-MCP":{"command":"x"}}}`,
		".pi/extensions/repo.ts": "// REPO-EXTENSION\n", ".pi/SYSTEM.md": "REPO-SYSTEM\n",
		".pi/APPEND_SYSTEM.md": "REPO-APPEND\n", ".pi/prompts/p.md": "REPO-PROMPT\n", ".pi/themes/t.json": `{"REPO-THEME":1}`,
		".pi/skills/check/SKILL.md":          "---\nname: check\ndescription: Checks.\n---\nPI-CHECK\n",
		".pi/skills/check/deeper/SKILL.md":   "---\nname: deeper\ndescription: Below a skill.\n---\n",
		".pi/skills/group/deep/SKILL.md":     "---\nname: deep\ndescription: Deep.\n---\nDeep.\n",
		".pi/skills/.hidden/h/SKILL.md":      "---\nname: h\ndescription: Hidden.\n---\n",
		".pi/skills/node_modules/n/SKILL.md": "---\nname: n\ndescription: A package's.\n---\n",
		".agents/skills/check/SKILL.md":      "---\nname: check\ndescription: Not this one.\n---\nAGENTS-CHECK\n",
		".agents/skills/lint/SKILL.md":       "---\nname: lint\ndescription: Lints.\n---\nLint.\n",
		".claude/skills/c/SKILL.md":          "---\nname: c\ndescription: Claude's.\n---\n",
		".github/skills/g/SKILL.md":          "---\nname: g\ndescription: Copilot's.\n---\n",
		".codex/skills/x/SKILL.md":           "---\nname: x\ndescription: Codex's.\n---\n",
	})
	marks := []string{"REPO-AGENTS", "REPO-UPPER", "REPO-CLAUDE", "REPO-LOCAL", "REPO-DOT-CLAUDE", "REPO-GEMINI",
		"REPO-PI-AGENTS", "REPO-COPILOT", "REPO-SETTINGS", "REPO-MCP", "REPO-EXTENSION", "REPO-SYSTEM", "REPO-APPEND",
		"REPO-PROMPT", "REPO-THEME", "AGENTS-CHECK", "Hidden.", "A package's", "Claude's",
		"Copilot's", "Codex's"}
	for _, c := range []struct {
		name, into string
		home       map[string]string
		late       bool
	}{
		{name: "a home with none, and a checkout cloned during the run", into: "AGENTS.override.md", late: true},
		{name: "the home's own AGENTS.override.md", into: "AGENTS.override.md",
			home: map[string]string{"AGENTS.override.md": "HOME-OVERRIDE\n", "AGENTS.md": "HOME-AGENTS\n"}},
		{name: "the home's only CLAUDE.md", into: "CLAUDE.md", home: map[string]string{"CLAUDE.md": "HOME-CLAUDE\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runner, box := piCheckoutBox(t)
			p := executor.PathsFor(box)
			plant(t, box, repo)
			for name, body := range c.home {
				plant(t, box, map[string]string{".pi/agent/" + name: body})
			}
			if c.late {
				late := t.TempDir()
				for rel, body := range piCheckout("late", map[string]string{"AGENTS.md": "LATE-INSTRUCTIONS\n",
					".agents/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\nFix.\n",
					".pi/settings.json":           `{"theme":"LATE-SETTINGS"}`}) {
					plantFile(t, filepath.Join(late, rel), body)
				}
				plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"),
					"clone-dst": filepath.Join(box.Home(), "workspace", "late")})
			}
			if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
				t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
			}
			work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
			if err != nil {
				t.Fatal(err)
			}

			// Instructions.
			var agent map[string]string
			seenBy(t, box, "agent", &agent)
			section := "for `repo` with root " + work + "/repo\n\nREPO-INSTRUCTIONS\n"
			want := maps.Clone(c.home)
			if want == nil {
				want = map[string]string{}
			}
			want[c.into] = section
			if own, ok := c.home[c.into]; ok {
				want[c.into] = own + "\n\n" + section
			}
			if got := piInstructionFiles(agent); !maps.Equal(got, want) {
				t.Errorf("instructions: pi started on the agent directory's %q, want %q", got, want)
			}
			after := map[string]string{}
			for _, name := range piHomeNames {
				if raw, err := os.ReadFile(filepath.Join(p.PiAgentDir(), name)); err == nil {
					after[name] = string(raw)
				}
			}
			if !maps.Equal(after, c.home) {
				t.Errorf("instructions: after the run the agent directory holds %q, want the home's own %q", after, c.home)
			}
			// Skills.
			var argv []string
			seenBy(t, box, "argv", &argv)
			if at := slices.Index(argv, p.PiCheckoutSkills()); at < 1 || argv[at-1] != "--skill" {
				t.Errorf("skills: pi was not named the checkouts' skills directory: %q", argv)
			}
			var skills map[string]map[string]string
			seenBy(t, box, "skills", &skills)
			gotSkills := slices.Sorted(maps.Keys(skills[p.PiCheckoutSkills()]))
			if !slices.Equal(gotSkills, []string{"repo:check/SKILL.md", "repo:check/deeper/SKILL.md", "repo:deep/SKILL.md",
				"repo:lint/SKILL.md"}) {
				t.Errorf("skills: pi found %q in the checkouts' skills directory", gotSkills)
			}
			if got := skills[p.PiCheckoutSkills()]["repo:check/SKILL.md"]; got != "---\nname: \"repo:check\"\ndescription: Checks.\n---\nPI-CHECK\n" {
				t.Errorf("skills: repo:check reads %q, want .pi/skills' own, renamed", got)
			}
			// Everything else.
			raw, _ := json.Marshal([]any{agent, argv, skills})
			pinned := string(raw) + boxFile(t, box, p.PiExtension())
			for _, mark := range append(marks, "LATE-SETTINGS") {
				if strings.Contains(pinned, mark) {
					t.Errorf("%s: what the checkout should not bring reached what pi starts on", mark)
				}
			}
			// A checkout cloned during the run.
			if !c.late {
				return
			}
			var nested1, nested2 any
			seenBy(t, box, "nested-1", &nested1)
			seenBy(t, box, "nested-2", &nested2)
			late := work + "/late"
			wantLate := "for `late` with root " + late + "\n\n<project_instructions path=\"" + late +
				"/AGENTS.md\">\nLATE-INSTRUCTIONS\n\n</project_instructions>\n\n# Skills of " + late + "\n\nEach is a set " +
				"of local instructions to follow, stored in its `SKILL.md` file; to use one, read that file.\n" +
				"- late:fix: Fixes. (file: " + p.PiCheckoutSkills() + "/late:fix/SKILL.md)"
			wantResult := map[string]any{"content": []any{map[string]any{"type": "text", "text": "cloned"},
				map[string]any{"type": "text", "text": wantLate}}}
			if got, _ := json.Marshal(nested1); string(got) != string(must(json.Marshal(wantResult))) {
				t.Errorf("clone: the first tool result after it was\n%s\nwant\n%s", got, must(json.Marshal(wantResult)))
			}
			if nested2 != nil {
				t.Errorf("clone: the later tool result was changed again: %v", nested2)
			}
			var lateSkills map[string]map[string]string
			seenBy(t, box, "skills-late", &lateSkills)
			if got := lateSkills[p.PiCheckoutSkills()]["late:fix/SKILL.md"]; got != "---\nname: \"late:fix\"\ndescription: Fixes.\n---\nFix.\n" {
				t.Errorf("clone: its skill read %q while the run held it", got)
			}
			if left, err := os.ReadDir(p.PiCheckoutSkills()); err != nil || len(left) != 0 {
				t.Errorf("clone: the checkouts' skills directory holds %v after the run (%v), want the copies removed", left, err)
			}
			if errs := boxFile(t, box, p.Err()); strings.Contains(errs, "no note helper") || strings.Contains(errs, "crewlet: the note helper") {
				t.Errorf("the run said its checkouts were not handed:\n%s", errs)
			}
		})
	}
}

// must is v, failing nothing: the case's own JSON cannot fail to encode.
func must(v []byte, _ error) []byte { return v }

// piRestoreStandIn is a `pi` that records the agent directory's instruction
// files as it starts, then does what $HOME/mode says: exit 0, exit 1, signal
// its script as a teardown does (TERM, INT or HUP) and wait to be ended, or
// kill its script outright.
const piRestoreStandIn = `#!/bin/sh
case "$1" in --version) echo 1.6.0; exit 0;; esac
cat > /dev/null
for n in AGENTS.override.md AGENTS.md AGENTS.MD CLAUDE.md CLAUDE.MD; do
  if [ -e "$PI_CODING_AGENT_DIR/$n" ]; then echo "== $n"; cat "$PI_CODING_AGENT_DIR/$n"; fi
done > "$HOME/seen-agents"
case "$(cat "$HOME/mode")" in
ok) exit 0;;
fail) exit 1;;
term|int|hup) kill -"$(tr a-z A-Z < "$HOME/mode")" "$PPID"; exec sleep 60;;
kill) kill -KILL "$PPID"; exit 0;;
esac
exit 2
`

// piGateFails is a `mkdir` that fails for the run's checkout state, so the
// gate stops after it has held the home's file.
const piGateFails = `#!/bin/sh
for a; do case "$a" in */.pi/crewlet/checkouts) exit 1;; esac; done
exec /bin/mkdir "$@"
`

// THE AGENT DIRECTORY'S INSTRUCTION FILES ARE THE HOME'S OWN HOWEVER A PI RUN
// ENDS: put back once pi has stopped, succeeded or failed, where the gate
// fails after holding, and on each teardown signal (TERM, INT, HUP), ending
// pi first; a run killed outright is put back by the next run's clear,
// whichever harness it drives. A home's own AGENTS.md comes back as it was,
// an AGENTS.override.md the run made is gone, and nothing held is left.
//
// Mutation: drop the put-back after pi, the EXIT trap or a signal's, or
// Pi's homeKeeper, and a row goes red.
func TestAPiRunPutsTheHomesInstructionFileBackHoweverItEnds(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ok", "fail", "gate", "term", "int", "hup", "kill"} {
		for _, own := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, home's own %v", mode, own), func(t *testing.T) {
				t.Parallel()
				runner, box := standInBox(t, executor.NewPi(), map[string]string{"pi": piRestoreStandIn})
				p := executor.PathsFor(box)
				plant(t, box, piCheckout("workspace/repo", map[string]string{"AGENTS.md": "REPO\n"}))
				plant(t, box, map[string]string{"mode": mode})
				home := map[string]string{}
				if own {
					home["AGENTS.md"] = "OWN\n"
					plant(t, box, map[string]string{".pi/agent/AGENTS.md": "OWN\n"})
				}
				if mode == "gate" {
					writeBoxStandIn(t, box, p.BinDir()+"/mkdir", piGateFails)
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
					want := "== AGENTS.override.md\nfor `repo` with root " + work + "/repo\n\nREPO\n"
					if own {
						want = "== AGENTS.md\nOWN\n\n\nfor `repo` with root " + work + "/repo\n\nREPO\n"
					}
					if got := startedRecord(t, box, box.Home()+"/seen-agents"); got != want {
						t.Fatalf("pi started on the agent directory's\n%q\nwant\n%q", got, want)
					}
				}
				if mode == "kill" {
					if err := executor.NewClaudeCode().ClearArtifacts(t.Context(), box); err != nil {
						t.Fatalf("the next run's clear: %v", err)
					}
				}
				after := map[string]string{}
				for _, name := range piHomeNames {
					if raw, err := os.ReadFile(filepath.Join(p.PiAgentDir(), name)); err == nil {
						after[name] = string(raw)
					}
				}
				if !maps.Equal(after, home) {
					t.Errorf("after the run the agent directory holds %q, want the home's own %q", after, home)
				}
				for _, held := range []string{"", ".none", ".live", ".tmp"} {
					if _, err := os.Lstat(p.PiHomeInstructions() + held); err == nil {
						t.Errorf("%s is left after the run", p.PiHomeInstructions()+held)
					}
				}
			})
		}
	}
}

// WHAT A FIRING LEFT OUT REACHES THE RUN'S ERROR STREAM ONCE, AFTER PI, AND
// EVERY STEP OF THE END RUNS: a checkout cloned during the run whose walk
// finds two skills of one name is handed with the line in the tool's result,
// and the script prints the line once pi has stopped, once, the helper's own
// error stream passed on nowhere; where the file the run made will not go,
// the put-back says so and keeps the none marker for the next try, and the
// line is printed and the copies removed all the same.
//
// Mutation: drop the print, pass the helper's stderr on, chain the end's
// steps, or drop the marker whatever the rm did, and a row goes red.
func TestAPiRunsLeftOutLinesReachItsErrorStreamOnce(t *testing.T) {
	t.Parallel()
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprintf("put-back fails %v", broken), func(t *testing.T) {
			t.Parallel()
			runner, box := piCheckoutBox(t)
			p := executor.PathsFor(box)
			plant(t, box, piCheckout("workspace/repo", map[string]string{"AGENTS.md": "REPO\n"}))
			late := t.TempDir()
			skill := "---\nname: deploy\ndescription: Deploys.\n---\n"
			for rel, body := range piCheckout("late", map[string]string{".pi/skills/infra/deploy/SKILL.md": skill,
				".pi/skills/web/deploy/SKILL.md": skill}) {
				plantFile(t, filepath.Join(late, rel), body)
			}
			plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"),
				"clone-dst": filepath.Join(box.Home(), "workspace", "late")})
			if broken {
				plant(t, box, map[string]string{"break-home": ""})
			}
			runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
			work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
			if err != nil {
				t.Fatal(err)
			}
			line := "the skill " + work + "/late/.pi/skills/web/deploy is left out: " + work +
				"/late/.pi/skills/infra/deploy, found first, takes its name late:deploy"
			var nested map[string]any
			seenBy(t, box, "nested-1", &nested)
			if raw, _ := json.Marshal(nested); !strings.Contains(string(raw), "- "+line) {
				t.Errorf("the tool's result after the clone was %s, want the line in it", raw)
			}
			errs := boxFile(t, box, p.Err())
			if n := strings.Count(errs, line); n != 1 {
				t.Errorf("the run's error stream says the line %d times, want once:\n%s", n, errs)
			}
			if raw, err := os.ReadDir(p.PiCheckoutSkills()); err != nil || len(raw) != 0 {
				t.Errorf("the checkouts' skills directory holds %v after the run (%v), want the copies removed", raw, err)
			}
			if failed := strings.Contains(errs, "was not put back"); failed != broken {
				t.Errorf("the run's error stream says a put-back failed: %v, want %v:\n%s", failed, broken, errs)
			}
			if _, err := os.Lstat(p.PiHomeInstructions() + ".none"); (err == nil) != broken {
				t.Errorf("the home's none marker is held after the run: %v, want %v (the next try needs it)", err == nil, broken)
			}
		})
	}
}

// THE NOTE HELPER'S OWN TROUBLE REACHES THE RUN'S ERROR STREAM: a firing that
// cannot read its config (here removed mid-run) hands nothing and says why,
// and the extension passes that on, as it passes on all the helper's stderr
// but what it left out, which the run's skip log carries.
//
// Mutation: pass the helper's stderr on nowhere, and this goes red.
func TestAPiNoteHelpersOwnTroubleReachesTheRunsErrorStream(t *testing.T) {
	t.Parallel()
	runner, box := piCheckoutBox(t)
	late := t.TempDir()
	for rel, body := range piCheckout("late", map[string]string{"AGENTS.md": "LATE\n"}) {
		plantFile(t, filepath.Join(late, rel), body)
	}
	plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"),
		"clone-dst": filepath.Join(box.Home(), "workspace", "late"), "break-nested": ""})
	runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
	if errs := boxFile(t, box, executor.PathsFor(box).Err()); strings.Count(errs, "notehelper: the nested config: ") != 2 {
		t.Errorf("the run's error stream does not say each firing's trouble with its config:\n%s", errs)
	}
}

// THE CLEAR DROPS A SKIP LOG NO SCRIPT PRINTED: a run killed before its
// script printed what its firings left out leaves the log in its checkout
// state, and the clear before the next run, whichever harness it drives,
// removes Codex's, Pi's, Hermes's, Copilot's and Kimi Code's alike, so a
// script ended before its gate prints no line of another run's as its own.
//
// Mutation: drop Codex's, Hermes's, Copilot's or Kimi's removal from its
// restore, and a row goes red (Pi's state is also under the directory the
// clear removes
// whole).
func TestTheClearDropsASkipLogNoScriptPrinted(t *testing.T) {
	t.Parallel()
	_, box := standInBox(t, executor.NewPi(), nil)
	p := executor.PathsFor(box)
	logs := map[string]string{"codex": p.CodexRepo() + "/skipped", "pi": p.PiCheckouts() + "/skipped",
		"hermes": p.HermesCheckouts() + "/skipped", "copilot": p.CopilotRepo() + "/skipped",
		"kimi": p.KimiCheckouts() + "/skipped"}
	for _, log := range logs {
		plantFile(t, log, "notehelper: a killed run's line\n")
	}
	if err := executor.NewClaudeCode().ClearArtifacts(t.Context(), box); err != nil {
		t.Fatalf("the clear: %v", err)
	}
	for name, log := range logs {
		if _, err := os.Lstat(log); err == nil {
			t.Errorf("%s: the clear left the killed run's skip log %s", name, log)
		}
	}
}
