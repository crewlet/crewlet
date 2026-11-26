package executor_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// openCodeCheckoutDriver is run by the stand-in server as it starts, under
// node, in the server's environment: it records the files the run's
// configuration names in `instructions` and the directories in `skills` as
// OpenCode would read them, and the checkouts plugin's entry; then, where
// $HOME/clone-src names a directory, it sets the plugin up as OpenCode does,
// copies that directory to $HOME/clone-dst as a clone during a tool call
// would, and plays a tool's `execute.after` to the plugin three times, a
// subagent's session first, recording each result's content (nested-sub,
// nested-1, nested-2) and the skills directories after them (skills-late).
const openCodeCheckoutDriver = `import { cpSync, existsSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs"
import { join } from "node:path"
import { pathToFileURL } from "node:url"
const [dir, home] = process.argv.slice(2)
const record = (name, value) => writeFileSync(join(dir, name), JSON.stringify(value))
const files = (d) => {
  const out = {}
  if (!existsSync(d)) return out
  for (const rel of readdirSync(d, { recursive: true })) if (statSync(join(d, rel)).isFile()) out[rel] = readFileSync(join(d, rel), "utf8")
  return out
}
const cfg = JSON.parse(process.env.OPENCODE_CONFIG_CONTENT)
const skills = () => Object.fromEntries((cfg.skills ?? []).map((d) => [d, files(d)]))
record("instructions", Object.fromEntries((cfg.instructions ?? []).map((f) => [f, existsSync(f) ? readFileSync(f, "utf8") : null])))
record("skills", skills())
const entry = (cfg.plugins ?? []).find((p) => typeof p === "object" && p.package.endsWith("/opencode-checkouts-plugin"))
record("checkouts-plugin", entry ?? null)
if (entry && existsSync(join(home, "clone-src"))) {
  const plugin = (await import(pathToFileURL(join(entry.package, "index.js")).href)).default
  let hook = async () => {}
  await plugin.setup({ options: entry.options, tool: { hook: async (name, cb) => { if (name === "execute.after") hook = cb } } })
  cpSync(readFileSync(join(home, "clone-src"), "utf8"), readFileSync(join(home, "clone-dst"), "utf8"), { recursive: true })
  const fire = async (sessionID) => {
    const event = { tool: "shell", sessionID, agent: "build", result: { output: {}, content: [{ type: "text", text: "cloned" }], metadata: {} } }
    await hook(event)
    return event.result.content
  }
  record("nested-sub", await fire("ses_child"))
  record("nested-1", await fire(process.env.CREWLET_RUN_SESSION))
  record("nested-2", await fire(process.env.CREWLET_RUN_SESSION))
  record("skills-late", skills())
}
`

// openCodeCheckoutBox is a real box with an OpenCode runner and the fake
// `opencode` whose server runs [openCodeCheckoutDriver] as it starts, whose
// run ends its step, and which answers config.get with docs where it is not
// empty: the box and the directory the fake and the driver record in.
func openCodeCheckoutBox(t *testing.T, docs string) (*executor.Runner, sandbox.Sandbox, string) {
	t.Helper()
	node := prereqtest.OnPath(t, "node", "the OpenCode stand-in loads the run's checkouts plugin under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
	runner, box := openCodeBox(t, t.TempDir())
	p := executor.PathsFor(box)
	dir := t.TempDir()
	q := shellword.Quote
	writeBoxStandIn(t, box, p.BinDir()+"/oc-checkout-driver.mjs", openCodeCheckoutDriver)
	standIn := strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+q(dir), 1)
	if docs != "" {
		standIn = strings.Replace(standIn, "d="+q(dir), "d="+q(dir)+"\nFAKE_DOCUMENTS="+q(docs), 1)
	}
	// ITS RUN ENDS ITS STEP, as OpenCode's does, so the collection reads a run
	// that said how it ended.
	standIn = strings.Replace(standIn, `else echo '{"type":"text","part":{"text":"done"}}'; fi`,
		`else echo '{"type":"text","part":{"text":"done"}}'; fi; echo '{"type":"step_finish","part":{"reason":"stop"}}'`, 1)
	standIn = strings.Replace(standIn, "serve)\n", "serve)\n  "+q(node)+" "+q(p.BinDir()+"/oc-checkout-driver.mjs")+
		` "$d" "$HOME" 2> "$d/driver.err" || : > "$d/driver-failed"`+"\n", 1)
	writeBoxStandIn(t, box, p.BinDir()+"/opencode", standIn)
	return runner, box, dir
}

// openCodeRecorded is what the driver or the fake recorded under name, decoded into v.
func openCodeRecorded(t *testing.T, dir, name string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(readIn(t, dir, name)), v); err != nil {
		t.Fatalf("the stand-in's %s: %v", name, err)
	}
}

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS AN OPENCODE RUN: of
// ./repo, which holds every file OpenCode 2.3.2 reads where it starts and
// more, its AGENTS.md (over CLAUDE.md) is handed in the sections file the
// configuration names; the skills its four directories hold to 6 deep come as
// `repo:<skill>`, but one a home skill's name takes, said; nothing else of it
// reaches the server. A clone is handed at the next tool result of the run's
// own session, framed as OpenCode frames an instruction file, once, within
// 192 KiB; a subagent's claims nothing; no copy outlives the run.
//
// Mutation: drop the gate, an entry, the plugin, its session check, the
// end's restore, the bound or the listed names, walk past 6, or hand twice.
func TestWhatACheckoutBelowTheStartDirectoryBringsAnOpenCodeRun(t *testing.T) {
	t.Parallel()
	runner, box, dir := openCodeCheckoutBox(t, "")
	p := executor.PathsFor(box)
	skill := func(name, mark string) string { return "---\nname: " + name + "\ndescription: " + mark + ".\n---\n" }
	plant(t, box, piCheckout("workspace/repo", map[string]string{
		"AGENTS.md": "REPO-AGENTS\n", "CLAUDE.md": "REPO-CLAUDE\n", "sub/AGENTS.md": "REPO-SUB-AGENTS\n",
		"opencode.json":           `{"mcp":{"servers":{"REPO-MCP":{"type":"local","command":["x"]}}}}`,
		".opencode/opencode.json": `{"theme":"REPO-SETTINGS"}`, ".opencode/plugins/p.js": "// REPO-PLUGIN\n",
		".opencode/agents/a.md": "REPO-AGENT\n", ".opencode/commands/c.md": "REPO-COMMAND\n",
		".opencode/rules.md":                          "REPO-RULES\n",
		".opencode/skills/check/SKILL.md":             skill("check", "Checks"),
		".opencode/skills/a/b/c/d/e/six/SKILL.md":     skill("six", "Six deep"),
		".opencode/skills/a/b/c/d/e/f/seven/SKILL.md": skill("seven", "REPO-SEVEN"),
		".opencode/skills/.hidden/h/SKILL.md":         skill("h", "REPO-HIDDEN"),
		".opencode/skill/one/SKILL.md":                skill("one", "Singular"),
		".claude/skills/cl/SKILL.md":                  skill("cl", "Claude's"),
		".agents/skills/ag/SKILL.md":                  skill("ag", "Agents'"),
		".codex/skills/x/SKILL.md":                    skill("x", "REPO-CODEX"),
	}))
	plant(t, box, map[string]string{".config/opencode/skills/mine/SKILL.md": skill("repo:cl", "HOME-CL")})
	late := t.TempDir()
	for rel, body := range piCheckout("late", map[string]string{"AGENTS.md": "LATE-INSTRUCTIONS\n",
		".agents/skills/fix/SKILL.md": skill("fix", "Fixes")}) {
		plantFile(t, filepath.Join(late, rel), body)
	}
	plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"),
		"clone-dst": filepath.Join(box.Home(), "workspace", "late")})
	if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", System: "SEAT"}); !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
	}
	if _, err := os.Stat(filepath.Join(dir, "driver-failed")); err == nil {
		t.Fatalf("the driver failed: %s", readIn(t, dir, "driver.err"))
	}
	work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}

	// Instructions.
	var instructions map[string]*string
	openCodeRecorded(t, dir, "instructions", &instructions)
	if got := slices.Sorted(maps.Keys(instructions)); !slices.Equal(got, slices.Sorted(slices.Values(
		[]string{p.System(), p.OpenCodeCheckoutSections()}))) {
		t.Fatalf("instructions: the server was named %q", got)
	}
	section := "for `repo` with root " + work + "/repo\n\nREPO-AGENTS\n"
	if got := instructions[p.OpenCodeCheckoutSections()]; got == nil || *got != section {
		t.Errorf("instructions: the checkouts' file read %v, want %q", got, section)
	}
	// Skills.
	var skills map[string]map[string]string
	openCodeRecorded(t, dir, "skills", &skills)
	gotSkills := slices.Sorted(maps.Keys(skills[p.OpenCodeCheckoutSkills()]))
	want := []string{"repo:ag/SKILL.md", "repo:check/SKILL.md", "repo:one/SKILL.md", "repo:six/SKILL.md"}
	if len(skills) != 1 || !slices.Equal(gotSkills, want) {
		t.Errorf("skills: the server found %q in %q, want %q in the checkouts' directory alone", gotSkills,
			slices.Collect(maps.Keys(skills)), want)
	}
	// Everything else.
	pinned := readIn(t, dir, "serve-config") + readIn(t, dir, "instructions") + readIn(t, dir, "skills")
	for _, mark := range []string{"REPO-CLAUDE", "REPO-SUB-AGENTS", "REPO-MCP", "REPO-SETTINGS", "REPO-PLUGIN",
		"REPO-AGENT\n", "REPO-COMMAND", "REPO-RULES", "REPO-SEVEN", "REPO-HIDDEN", "REPO-CODEX"} {
		if strings.Contains(pinned, mark) {
			t.Errorf("%s: what the checkout should not bring reached what the server starts on", mark)
		}
	}
	// A checkout cloned during the run.
	var sub, nested1, nested2 []map[string]string
	openCodeRecorded(t, dir, "nested-sub", &sub)
	openCodeRecorded(t, dir, "nested-1", &nested1)
	openCodeRecorded(t, dir, "nested-2", &nested2)
	cloned := []map[string]string{{"type": "text", "text": "cloned"}}
	if !slices.EqualFunc(sub, cloned, maps.Equal) {
		t.Errorf("clone: a subagent's tool result was changed: %v", sub)
	}
	lateRoot := work + "/late"
	wantLate := "for `late` with root " + lateRoot + "\n\nInstructions from: " + lateRoot +
		"/AGENTS.md\nLATE-INSTRUCTIONS\n\n\n# Skills of " + lateRoot + "\n\nEach is a set of local instructions to " +
		"follow, stored in its `SKILL.md` file; to use one, read that file.\n" +
		"- late:fix: Fixes. (file: " + p.OpenCodeCheckoutSkills() + "/late:fix/SKILL.md)"
	if want := append(slices.Clone(cloned), map[string]string{"type": "text", "text": wantLate}); !slices.EqualFunc(nested1, want, maps.Equal) {
		t.Errorf("clone: the first tool result after it was\n%q\nwant\n%q", nested1, want)
	}
	if !slices.EqualFunc(nested2, cloned, maps.Equal) {
		t.Errorf("clone: the later tool result was changed again: %v", nested2)
	}
	var lateSkills map[string]map[string]string
	openCodeRecorded(t, dir, "skills-late", &lateSkills)
	if got := lateSkills[p.OpenCodeCheckoutSkills()]["late:fix/SKILL.md"]; got != "---\nname: \"late:fix\"\ndescription: Fixes.\n---\n" {
		t.Errorf("clone: its skill read %q while the run held it", got)
	}
	if left, err := os.ReadDir(p.OpenCodeCheckoutSkills()); err != nil || len(left) != 0 {
		t.Errorf("the checkouts' skills directory holds %v after the run (%v), want the copies removed", left, err)
	}
	if errs := boxFile(t, box, p.Err()); !strings.Contains(errs, "the skill "+work+"/repo/.claude/skills/cl is left out: "+
		"the home's "+box.Home()+"/.config/opencode/skills/mine, which OpenCode lists as repo:cl") {
		t.Errorf("skills: the run does not say its copy repo:cl is left out for the home's skill of that name:\n%s", errs)
	}
	if nested := boxFile(t, box, p.OpenCodeCheckoutState()+"/nested.json"); !strings.Contains(nested, `"firingLimit":196608`) {
		t.Errorf("the plugin's nested config holds no firingLimit of 192 KiB:\n%s", nested)
	}
	if errs := boxFile(t, box, p.Err()); strings.Contains(errs, "no note helper") || strings.Contains(errs, "crewlet: the note helper") {
		t.Errorf("the run said its checkouts were not handed:\n%s", errs)
	}
}

// A DOCUMENT THAT STOPS THE CHECKOUTS PLUGIN STARTS THE RUN WITHOUT IT; ONE
// THAT STOPS A PLUGIN OF THE RUN'S NOTES STILL REFUSES IT. The checkouts
// plugin carries no notes, skills or bridge, so a policy denying it, or a
// `plugins` entry removing it by id, leaves it unloaded while the launch's
// handing stands, and the run's error stream says once, naming the document
// and the rule, that a checkout cloned during the run is not handed; a deny
// of the notes plugin refuses the run before anything starts.
//
// Mutation: refuse the run for the checkouts plugin, load it all the same,
// drop the skip line or print it twice: red.
func TestADocumentThatStopsTheCheckoutsPluginStartsTheRunWithoutIt(t *testing.T) {
	t.Parallel()
	home := "/home/person/.config/opencode/opencode.json"
	for name, c := range map[string]struct{ info, rule string }{
		"a policy": {`{"policies":[{"action":"plugin.use","resource":"*/opencode-checkouts-plugin","effect":"deny"}]}`,
			home + `'s policies deny plugin.use "*/opencode-checkouts-plugin" (effect deny), which stops the run's plugin `},
		"a removal": {`{"plugins":["-crewlet.checkouts"]}`, home + `'s plugins remove "-crewlet.checkouts", the run's plugin `},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box, dir := openCodeCheckoutBox(t, `[{"type":"document","path":"`+home+`","info":`+c.info+`}]`)
			p := executor.PathsFor(box)
			plant(t, box, piCheckout("workspace/repo", map[string]string{"AGENTS.md": "REPO-AGENTS\n"}))
			if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
				t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
			}
			var entry any
			openCodeRecorded(t, dir, "checkouts-plugin", &entry)
			if entry != nil {
				t.Errorf("the configuration still names the checkouts plugin: %v", entry)
			}
			var instructions map[string]*string
			openCodeRecorded(t, dir, "instructions", &instructions)
			if got := instructions[p.OpenCodeCheckoutSections()]; got == nil || !strings.Contains(*got, "REPO-AGENTS") {
				t.Errorf("the launch's handing did not stand: the checkouts' file read %v", got)
			}
			line := "crewlet: checkouts cloned during the run are not handed their instructions and skills: " + c.rule +
				p.OpenCodeCheckoutsPlugin()
			if errs := boxFile(t, box, p.Err()); strings.Count(errs, line) != 1 {
				t.Errorf("the run's error stream does not say once %q:\n%s", line, errs)
			}
		})
	}

	_, box, _ := openCodeCheckoutBox(t, `[{"type":"document","path":"`+home+`","info":{"policies":[`+
		`{"action":"plugin.use","resource":"*","effect":"deny"}]}}]`)
	p := executor.PathsFor(box)
	_, err := executor.OpenCode{}.WriteConfig(t.Context(), box, sandbox.RunRequest{Steer: steerWiring}, executor.Session{}, p)
	if err == nil || !strings.Contains(err.Error(), "which stops the run's plugin "+p.OpenCodeSteerPlugin()) {
		t.Errorf("a deny of the notes plugin: WriteConfig = %v, want the run refused naming it", err)
	}
}
