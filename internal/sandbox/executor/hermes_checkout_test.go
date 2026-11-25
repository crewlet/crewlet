package executor_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// hermesCheckoutChat is the stand-in's `hermes chat`, run under the
// stand-in's interpreter and modules: it records in $HOME/seen the system
// prompt the run launched with and the home's skills; loads the run's
// plugin in a stand-in gateway and fires its turn and its first response
// (the run's window); where $HOME/clone-src names a directory, copies it to
// $HOME/clone-dst as a clone during a tool call would and fires the plugin's
// transform_tool_result for a delegated subagent's call, then twice for the
// main loop's, recording each answer and the home's skills after them
// (skills-late); then prints a finished run's stream.
const hermesCheckoutChat = `import importlib.util, json, os, shutil, sys
home, hermes = os.environ["HOME"], os.environ["HERMES_HOME"]
seen = os.path.join(home, "seen")
os.makedirs(seen, exist_ok=True)
def record(name, value):
    with open(os.path.join(seen, name), "w", encoding="utf-8") as f:
        json.dump(value, f)
def skills():
    root, out = os.path.join(hermes, "skills"), {}
    for top, _dirs, files in os.walk(root):
        for f in files:
            out[os.path.relpath(os.path.join(top, f), root)] = open(os.path.join(top, f), encoding="utf-8").read()
    return out
record("prompt", os.environ.get("HERMES_EPHEMERAL_SYSTEM_PROMPT"))
record("skills", skills())
spec = importlib.util.spec_from_file_location("crewlet_plugin", os.path.join(hermes, "plugins", "crewlet-runner", "__init__.py"))
plugin = importlib.util.module_from_spec(spec)
spec.loader.exec_module(plugin)
hooks = {}
class Ctx:
    def register_hook(self, name, fn, **kw):
        hooks[name] = fn
plugin.register(Ctx())
hooks["pre_llm_call"](session_id="main")
hooks["post_api_request"](session_id="main", context_length=262144, usage={}, response={})
clone = os.path.join(home, "clone-src")
if os.path.exists(clone):
    src, dst = open(clone).read(), open(os.path.join(home, "clone-dst")).read()
    shutil.copytree(src, dst, symlinks=True)
    fire = hooks["transform_tool_result"]
    record("nested", {"subagent": fire(session_id="child", tool_name="terminal", result="cloned"),
                      "main-1": fire(session_id="main", tool_name="terminal", result="cloned"),
                      "main-2": fire(session_id="main", tool_name="terminal", result="cloned")})
    record("skills-late", skills())
sys.stdout.write(open(sys.argv[1]).read())
`

// hermesCheckoutBox is a real box whose `hermes` answers the release,
// enables a plugin ([hermesEnable]), runs the carry script's overlay, guard,
// plugoff and gate under the stand-in of Hermes's modules, and answers
// `chat` with [hermesCheckoutChat]; and the stand-in's directory.
func hermesCheckoutBox(t *testing.T) (*executor.Runner, sandbox.Sandbox, string) {
	t.Helper()
	runner, box := standInBox(t, executor.NewHermes(), nil)
	bin := executor.HermesStandIn(t, box.Home()+"/system")
	stream, err := os.ReadFile(filepath.Join("testdata", "hermes", "0.23.3", "stream-steered.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{"chat.py": hermesCheckoutChat, "enable.py": hermesEnable,
		"stream.jsonl": string(stream)} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	q := shellword.Quote
	launch := q(bin+"/hermes") + " --run-module cProfile -o /dev/null "
	writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/hermes", "#!/bin/sh\ncase \"$1\" in\n"+
		"--version) echo 'Hermes Agent v0.23.3 (2026.9.24)'; exit 0;;\n"+
		"--run-module) case \"$6\" in overlay|guard|plugoff|gate) exec "+q(bin+"/hermes")+" \"$@\";; esac; exit 0;;\n"+
		"plugins) [ \"$2\" = enable ] && exec "+launch+q(bin+"/enable.py")+" \"$3\"; exit 0;;\n"+
		"chat) exec "+launch+q(bin+"/chat.py")+" "+q(bin+"/stream.jsonl")+";;\n"+
		"esac\nexit 0\n")
	return runner, box, bin
}

// hermesSkill is a SKILL.md named name, its body marking it.
func hermesSkill(name, body string) string {
	return "---\nname: " + name + "\ndescription: Does " + name + ".\n---\n" + body + "\n"
}

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A HERMES RUN: what Hermes's
// own code renders of a session started there (one kind: .hermes.md, else an
// AGENTS name, else CLAUDE.md, else .cursorrules and the .mdc rules, a file
// its scan blocks blocked), cut at the window Hermes resolves, after
// the run's own text, whole while the 128 KiB fit, else named, a later
// smaller one whole; a trusted root's skills copied, less a quarantined one,
// a home name's and an untrusted root's, each said once; nothing else. A
// clone during the run is handed once in the main loop's next result, none in
// a subagent's; no copy outlives the run. Mutation: drop the gate, the trust,
// the window, the budget, the subagent check, hput's restore or saySkipped,
// and a check goes red.
func TestWhatACheckoutBelowTheStartDirectoryBringsAHermesRun(t *testing.T) {
	t.Parallel()
	runner, box, _ := hermesCheckoutBox(t)
	p := executor.PathsFor(box)
	big := func(mark string) string { return strings.Repeat(mark+" line\n", 200000/len(mark+" line\n")) }
	files := map[string]string{}
	for rel, body := range map[string]map[string]string{
		"workspace/hm": {".hermes.md": "---\ntitle: x\n---\nHM-HERMES\n", "AGENTS.md": "HM-AGENTS\n", "CLAUDE.md": "HM-CLAUDE\n",
			".hermes/config.yaml": `{"mcp_servers": {"HM-MCP": {"command": "x"}}}`, ".hermes/plugins/p/plugin.yaml": "name: HM-PLUGIN\n"},
		"workspace/ag": {"AGENTS.override.md": "  \n", "AGENTS.md": "AG-AGENTS\n", "CLAUDE.md": "AG-CLAUDE\n",
			".hermes/skills/fix/SKILL.md": hermesSkill("fix", "AG-FIX")},
		"workspace/cl": {"CLAUDE.md": "CL-CLAUDE\n", ".cursorrules": "CL-CURSOR\n", ".env": "CL_SECRET=CL-ENV\n",
			".agents/skills/lint/SKILL.md":   hermesSkill("lint", "CL-LINT"),
			".agents/skills/deploy/SKILL.md": hermesSkill("deploy", "CL-DEPLOY"),
			".hermes/skills/evil/SKILL.md":   hermesSkill("evil", "QUARANTINE CL-EVIL")},
		"workspace/cu": {".cursorrules": "CU-RULES\n", ".cursor/rules/a.mdc": "CU-A\n",
			".cursor/rules/b.mdc": "IGNORE-ALL-PREVIOUS CU-B\n"},
		"workspace/big1": {"AGENTS.md": big("BIG1")},
		"workspace/big2": {"AGENTS.md": big("BIG2")},
		"workspace/big3": {"AGENTS.md": big("BIG3")},
	} {
		for name, text := range piCheckout(rel, body) {
			files[name] = text
		}
	}
	plant(t, box, files)
	work := filepath.Join(box.Home(), "workspace")
	real, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	plant(t, box, map[string]string{".hermes/config.yaml": `{"skills": {"trusted_project_dirs": ["` + real + `/cl"]}}`,
		".hermes/skills/mine/SKILL.md": hermesSkill("cl:deploy", "HOME-MINE")})
	late := t.TempDir()
	for rel, body := range piCheckout("late", map[string]string{"AGENTS.md": "LATE-INSTRUCTIONS\n",
		".hermes/skills/x/SKILL.md": hermesSkill("x", "LATE-X")}) {
		plantFile(t, filepath.Join(late, rel), body)
	}
	plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"), "clone-dst": filepath.Join(work, "late")})
	if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", System: "STANDING-RULES"}); !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
	}

	// Instructions, in the system prompt after the run's own.
	var prompt string
	seenBy(t, box, "prompt", &prompt)
	label := func(name string) string { return "for `" + name + "` with root " + real + "/" + name + "\n\n" }
	cut := func(name, mark string) string {
		body := "## AGENTS.md\n\n" + strings.TrimSpace(big(mark))
		return body[:50400] + "\n\n[...truncated AGENTS.md: kept 50400+14400 of " + strconv.Itoa(len(body)) + " chars. The middle " +
			"is omitted. If you need the full instructions, read the complete file with the read_file tool: " + real + "/" +
			name + "/AGENTS.md]\n\n" + body[len(body)-14400:]
	}
	want := strings.TrimRight(boxFile(t, box, p.System()), "\n") + "\n\n" + strings.Join([]string{
		label("ag") + "## AGENTS.md\n\nAG-AGENTS",
		label("big1") + cut("big1", "BIG1"),
		label("big2") + "read " + real + "/big2/AGENTS.md for its instructions, which whole pass what the run's system prompt has room for",
		label("big3") + "read " + real + "/big3/AGENTS.md for its instructions, which whole pass what the run's system prompt has room for",
		label("cl") + "## CLAUDE.md\n\nCL-CLAUDE",
		label("cu") + "## .cursorrules\n\nCU-RULES\n\n## .cursor/rules/a.mdc\n\nCU-A\n\n## .cursor/rules/b.mdc\n\n" +
			"[BLOCKED: .cursor/rules/b.mdc contained potential prompt injection (ignore_previous). Content not loaded.]\n\n",
		label("hm") + "## .hermes.md\n\nHM-HERMES",
		"# Skills of " + real + "/cl\n\nEach is a set of local instructions to follow, stored in its `SKILL.md` file; " +
			"to use one, read that file.\n- cl:lint: Does lint. (file: " + p.HermesSkills() + "/cl:lint/SKILL.md)"}, "\n\n")
	if prompt != want {
		at := 0
		for at < min(len(prompt), len(want)) && prompt[at] == want[at] {
			at++
		}
		t.Errorf("instructions: the run launched with a system prompt of %d bytes, %d wanted, apart from byte %d:\n%.400q\nwant\n%.400q",
			len(prompt), len(want), at, prompt[max(at-100, 0):], want[max(at-100, 0):])
	}
	if len("HERMES_EPHEMERAL_SYSTEM_PROMPT=")+len(prompt)+1 > 128<<10 {
		t.Errorf("instructions: the system prompt's %d bytes pass what one environment variable holds", len(prompt))
	}
	// Skills: a trusted root's, less the quarantined one and the one a home skill's name takes.
	var homeSkills map[string]string
	seenBy(t, box, "skills", &homeSkills)
	if got := homeSkills["cl:lint/SKILL.md"]; got != "---\nname: \"cl:lint\"\ndescription: Does lint.\n---\nCL-LINT\n" {
		t.Errorf("skills: cl:lint reads %q", got)
	}
	for name := range homeSkills {
		if strings.HasPrefix(name, "ag:") || strings.HasPrefix(name, "cl:evil") || strings.HasPrefix(name, "cl:deploy") {
			t.Errorf("skills: the run copied %s", name)
		}
	}
	errs := boxFile(t, box, p.Err())
	lateLine := "the skills of " + real + "/late are left out: Hermes loads a checkout's skills only where its " +
		"skills.trusted_project_dirs names the checkout's root"
	for _, line := range []string{
		lateLine,
		"the skills of " + real + "/ag are left out: Hermes loads a checkout's skills only where its skills.trusted_project_dirs names the checkout's root",
		"the skill " + real + "/cl/.hermes/skills/evil is left out: Hermes's scan quarantines it",
		"the skill " + real + "/cl/.agents/skills/deploy is left out: the home's skill " + p.HermesSkills() + "/mine is found by its name cl:deploy",
		"the instructions of " + real + "/big2 are named, not handed",
		"the instructions of " + real + "/big3 are named, not handed",
	} {
		if n := strings.Count(errs, line); n != 1 {
			t.Errorf("the run's error stream says %q %d times, want once:\n%s", line, n, errs)
		}
	}
	// Nothing else of a checkout.
	raw, _ := json.Marshal([]any{prompt, homeSkills})
	for _, mark := range []string{"HM-AGENTS", "HM-CLAUDE", "HM-MCP", "HM-PLUGIN", "AG-CLAUDE", "AG-FIX", "CL-CURSOR",
		"CL-ENV", "CU-B", "CL-EVIL", "title: x"} {
		if strings.Contains(string(raw), mark) {
			t.Errorf("%s: what the checkout should not bring reached what Hermes starts on", mark)
		}
	}
	// A checkout cloned during the run.
	var nested map[string]*string
	seenBy(t, box, "nested", &nested)
	if nested["subagent"] != nil || nested["main-2"] != nil {
		t.Errorf("clone: a subagent's call or a later one was handed %q, %q", deref(nested["subagent"]), deref(nested["main-2"]))
	}
	if got, want := deref(nested["main-1"]), "cloned\n\n"+label("late")+"## AGENTS.md\n\nLATE-INSTRUCTIONS\n\n"+
		"# Left out of the repositories below the working directory\n\n- "+lateLine; got != want {
		t.Errorf("clone: the main loop's next result was\n%q\nwant\n%q", got, want)
	}
	// No copy outlives the run.
	left, err := os.ReadDir(p.HermesSkills())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"mine"}) {
		t.Errorf("after the run the home's skills hold %q, want the home's own alone", names)
	}
}

// deref is s's text, "" for nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// A HERMES BOX WHOSE HOOK BOUND IS SHORTER THAN THE NESTED HAND'S STARTS
// WITHOUT THE HAND, SAYING SO ONCE: only checkout handing needs that bound, so
// a plugins.hook_callback_timeout under 10 s refuses nothing; the launch still
// hands its checkouts, a clone during the run is not handed, and the run's
// error stream names the setting once, after the run. At 10 s, the hand's
// margin, claim reserve and render floor together, the clone is handed.
//
// Mutation: refuse the run, hand the clone anyway, say it twice, or turn the
// hand off at the bound itself, and a row goes red.
func TestAHermesRunWithAShortHookBoundStartsWithoutTheNestedHand(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		bound  string
		handed bool
	}{{"5", false}, {"9.99", false}, {"10", true}} {
		t.Run(c.bound, func(t *testing.T) {
			t.Parallel()
			runner, box, _ := hermesCheckoutBox(t)
			p := executor.PathsFor(box)
			files := piCheckout("workspace/repo", map[string]string{"AGENTS.md": "REPO-AGENTS\n"})
			files[".hermes/config.yaml"] = `{"plugins": {"hook_callback_timeout": ` + c.bound + `}}`
			plant(t, box, files)
			late := t.TempDir()
			for rel, body := range piCheckout("late", map[string]string{"AGENTS.md": "LATE-INSTRUCTIONS\n"}) {
				plantFile(t, filepath.Join(late, rel), body)
			}
			plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"),
				"clone-dst": filepath.Join(box.Home(), "workspace", "late")})
			if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
				t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
			}
			var prompt string
			seenBy(t, box, "prompt", &prompt)
			if !strings.Contains(prompt, "## AGENTS.md\n\nREPO-AGENTS") {
				t.Errorf("the launch handed no checkout: %q", prompt)
			}
			var nested map[string]*string
			seenBy(t, box, "nested", &nested)
			if (nested["main-1"] != nil) != c.handed {
				t.Errorf("the clone was handed: %v, want %v", nested["main-1"] != nil, c.handed)
			}
			line := "crewlet: a repository cloned during the run is not handed its instructions and skills: " +
				"plugins.hook_callback_timeout is " + c.bound + ", under the 10 seconds the hand may take"
			if errs := boxFile(t, box, p.Err()); strings.Count(errs, line) != map[bool]int{true: 0, false: 1}[c.handed] {
				t.Errorf("the run's error stream says %q %d times:\n%s", line, strings.Count(errs, line), errs)
			}
		})
	}
}

// A HERMES WHOSE OWN CODE MOVED STARTS WITHOUT ITS CHECKOUTS, SAYING WHY
// ONCE: only checkout handing reads that code, so where a function the runner
// reads is gone (here, the context discovery) the run starts, hands no
// checkout at launch or during the run, and its error stream names the
// release and the function once.
//
// Mutation: skip the check, refuse the run, or leave the nested hand on, and
// this goes red.
func TestAHermesWhoseReadCodeMovedStartsWithoutItsCheckouts(t *testing.T) {
	t.Parallel()
	runner, box, bin := hermesCheckoutBox(t)
	p := executor.PathsFor(box)
	moved := filepath.Join(bin, "py", "agent", "prompt_builder.py")
	src, err := os.ReadFile(moved)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(moved, []byte(strings.Replace(string(src), "def discover_context_files(", "def found_context_files(", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	plant(t, box, piCheckout("workspace/repo", map[string]string{"AGENTS.md": "REPO-AGENTS\n"}))
	late := t.TempDir()
	for rel, body := range piCheckout("late", map[string]string{"AGENTS.md": "LATE-INSTRUCTIONS\n"}) {
		plantFile(t, filepath.Join(late, rel), body)
	}
	plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"),
		"clone-dst": filepath.Join(box.Home(), "workspace", "late")})
	if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
	}
	var prompt string
	seenBy(t, box, "prompt", &prompt)
	if strings.Contains(prompt, "REPO-AGENTS") {
		t.Errorf("the launch handed a checkout Hermes could not render: %q", prompt)
	}
	var nested map[string]*string
	seenBy(t, box, "nested", &nested)
	if nested["main-1"] != nil {
		t.Errorf("the clone was handed: %q", *nested["main-1"])
	}
	work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	errs := boxFile(t, box, p.Err())
	for _, line := range []string{"crewlet: no repository below the working directory is handed its instructions and " +
		"skills: Hermes Agent 0.23.3 moved what the runner reads of its code " +
		"(agent.prompt_builder.discover_context_files is gone)",
		"crewlet: " + work + "/repo is not handed its instructions and skills, since the run hands no checkout after " +
			"its launch; read " + work + "/repo for its instructions"} {
		if strings.Count(errs, line) != 1 {
			t.Errorf("the run's error stream says %q %d times, want once:\n%s", line, strings.Count(errs, line), errs)
		}
	}
}

// A HERMES WHOSE CONTRACT CODE MOVED IS REFUSED AS BEFORE: the code the run's
// own contract reads (here, the managed scope the overlay installs through)
// is judged where it is read, so the overlay refuses the run as it always
// did; the checkout gate's own trouble only skips the checkouts.
//
// Mutation: have the gate refuse instead, or the overlay go on, and this goes
// red.
func TestAHermesWhoseContractCodeMovedIsRefusedAsBefore(t *testing.T) {
	t.Parallel()
	runner, box, bin := hermesCheckoutBox(t)
	p := executor.PathsFor(box)
	moved := filepath.Join(bin, "py", "hermes_cli", "managed_scope.py")
	src, err := os.ReadFile(moved)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(moved, []byte(strings.Replace(string(src), "def load_managed_config(", "def loaded_managed_config(", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); res.Success {
		t.Fatal("the run started on a Hermes whose managed scope moved")
	}
	if errs := boxFile(t, box, p.Err()); !strings.Contains(errs, "crewlet: the Hermes overlay of the run could not be installed, so the run does not start") {
		t.Errorf("the run was not refused by its overlay:\n%s", errs)
	}
}

// A TRUSTED CLONE DURING A HERMES RUN BRINGS ITS SKILLS AS AT LAUNCH: a root
// skills.trusted_project_dirs names, cloned during the run, is handed in the
// main loop's next result with its skill copied as `late:fix` and listed, its
// quarantined skill left out and said once; no copy outlives the run.
//
// Mutation: copy no skill of a clone, or copy its quarantined one, and this
// goes red.
func TestATrustedCloneDuringAHermesRunBringsItsSkills(t *testing.T) {
	t.Parallel()
	runner, box, _ := hermesCheckoutBox(t)
	p := executor.PathsFor(box)
	work := filepath.Join(box.Home(), "workspace")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	plant(t, box, map[string]string{".hermes/config.yaml": `{"skills": {"trusted_project_dirs": ["` + real + `/late"]}}`})
	late := t.TempDir()
	for rel, body := range piCheckout("late", map[string]string{"AGENTS.md": "LATE-INSTRUCTIONS\n",
		".hermes/skills/fix/SKILL.md":  hermesSkill("fix", "LATE-FIX"),
		".agents/skills/evil/SKILL.md": hermesSkill("evil", "QUARANTINE LATE-EVIL")}) {
		plantFile(t, filepath.Join(late, rel), body)
	}
	plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"), "clone-dst": filepath.Join(work, "late")})
	if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
	}
	var nested map[string]*string
	seenBy(t, box, "nested", &nested)
	quarantined := "the skill " + real + "/late/.agents/skills/evil is left out: Hermes's scan quarantines it"
	want := "cloned\n\nfor `late` with root " + real + "/late\n\n## AGENTS.md\n\nLATE-INSTRUCTIONS\n\n# Skills of " + real +
		"/late\n\nEach is a set of local instructions to follow, stored in its `SKILL.md` file; to use one, read that " +
		"file.\n- late:fix: Does fix. (file: " + p.HermesSkills() + "/late:fix/SKILL.md)\n\n# Left out of the " +
		"repositories below the working directory\n\n- " + quarantined
	if got := deref(nested["main-1"]); got != want {
		t.Errorf("the main loop's next result was\n%q\nwant\n%q", got, want)
	}
	var lateSkills map[string]string
	seenBy(t, box, "skills-late", &lateSkills)
	if got := lateSkills["late:fix/SKILL.md"]; got != "---\nname: \"late:fix\"\ndescription: Does fix.\n---\nLATE-FIX\n" {
		t.Errorf("late:fix read %q while the run held it", got)
	}
	for name := range lateSkills {
		if strings.HasPrefix(name, "late:evil") {
			t.Errorf("the quarantined skill was copied as %s", name)
		}
	}
	if errs := boxFile(t, box, p.Err()); strings.Count(errs, quarantined) != 1 {
		t.Errorf("the run's error stream says %q %d times, want once:\n%s", quarantined, strings.Count(errs, quarantined), errs)
	}
	if _, err := os.Stat(p.HermesSkills() + "/late:fix"); err == nil {
		t.Error("late:fix outlived the run")
	}
}
