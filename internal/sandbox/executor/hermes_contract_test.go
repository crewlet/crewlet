package executor_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// hermesPluginOf is the plugin WriteConfig staged for req's run in a box, its
// settings file's contents and a directory of the case's own holding both.
func hermesPluginOf(t *testing.T, req sandbox.RunRequest) (plugin string, settings map[string]any) {
	t.Helper()
	box := sandbox.NewFakeSandbox("b")
	p := executor.PathsFor(box)
	if _, err := (executor.Hermes{}).WriteConfig(t.Context(), box, req, executor.Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	dir := t.TempDir()
	read := func(path string) []byte {
		raw, err := box.ReadFile(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	plugin = filepath.Join(dir, "plugin.py")
	if err := os.WriteFile(plugin, read(p.HermesStage()+"/plugin/__init__.py"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(read(p.HermesPluginConfig()), &settings); err != nil {
		t.Fatal(err)
	}
	// The route files, copied out of the box, the settings naming the copies.
	for _, key := range []string{"notes_url_file", "skills_url_file"} {
		if path, ok := settings[key].(string); ok {
			copied := filepath.Join(dir, filepath.Base(path))
			if err := os.WriteFile(copied, read(path), 0o600); err != nil {
				t.Fatal(err)
			}
			settings[key] = copied
		}
	}
	return plugin, settings
}

// hermesNotesProbe runs the plugin's poll of the notes route the file names,
// its turn running, Hermes's gateway stood in for by a steer that records,
// and returns each steer as [session, text] once the plugin has begun its
// next poll after the steer, so whatever it does about a steered note is done.
const hermesNotesProbe = `import importlib.util, json, sys, threading, time
spec = importlib.util.spec_from_file_location("crewlet_plugin", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
steers, polls = [], []
m._steer = lambda session, text: steers.append([session, text]) or True
real = m._opener.open
def counting(request, *args, **kwargs):
    url = request if isinstance(request, str) else request.full_url
    if "/notes?" in url:
        polls.append(len(steers))
    return real(request, *args, **kwargs)
m._opener.open = counting
m._turn.update(running=True, session="stored-1")
threading.Thread(target=m._notes, args=(sys.argv[2],), daemon=True).start()
deadline = time.monotonic() + 20
while not (steers and polls and polls[-1] > 0) and time.monotonic() < deadline:
    time.sleep(0.05)
print(json.dumps(steers))
`

// HERMES'S PLUGIN READS THE BRIDGE'S OWN NOTES ROUTE: the route the run is
// written ([executor.Hermes.WriteConfig]'s settings), a real bridge's, is
// polled while the turn runs, and its note is steered into the session Hermes
// stored the turn under, word for word, and no read is posted: Hermes's record
// confirms it ([executor.HermesConfirmsATake]).
//
// Mutation: rename `message` or `seq` on mcpbridge.SteerNote, poll another
// path, write the route under another settings key, or post the route's
// /read — this goes red.
func hermesReachesTheBridgesRoute(t *testing.T) {
	t.Parallel()
	python := prereqtest.OnPath(t, "python3", "the case runs Hermes's plugin", "install python3")
	note := mcpbridge.SteerNote{Seq: 3, ID: "req-7", Inbox: executor.InboxID(executor.HermesName, "job-1", "req-7"),
		Message: "## A note from ada, sent while this turn was running\n\nTarget the release branch."}
	inbox := &routeInbox{notes: []mcpbridge.SteerNote{note}, reads: make(chan []string, 4)}
	endpoint := probeBridge(t, inbox, &probeLedger{})
	plugin, settings := hermesPluginOf(t, sandbox.RunRequest{
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool, URL: mcpbridge.SteerURL(endpoint)},
	})
	notes, ok := settings["notes_url_file"].(string)
	if !ok {
		t.Fatalf("the run was written no notes route: %v", settings)
	}
	out, err := exec.Command(python, "-I", "-c", hermesNotesProbe, plugin, notes).Output()
	if err != nil {
		t.Fatalf("the plugin's poll: %v", err)
	}
	var steers [][]string
	if err := json.Unmarshal(out, &steers); err != nil {
		t.Fatalf("the probe printed %q: %v", out, err)
	}
	if len(steers) != 1 || !slices.Equal(steers[0], []string{"stored-1", note.Message}) {
		t.Fatalf("the plugin steered %q, want the note into the stored session", steers)
	}
	select {
	case ids := <-inbox.reads:
		t.Fatalf("the plugin posted a read of %v; Hermes's record confirms a note", ids)
	default:
	}
}

// hermesSkillsProbe registers the plugin with a stand-in of Hermes's plugin
// context and hands its post_tool_call hook a skill_view of the skill argv[2]
// names whose content is argv[3]'s file, as Hermes hands it.
const hermesSkillsProbe = `import importlib.util, json, os, sys
os.environ["CREWLET_HERMES_PLUGIN"] = sys.argv[4]
spec = importlib.util.spec_from_file_location("crewlet_plugin", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
hooks = {}
class Ctx:
    def register_hook(self, name, fn, **kw):
        hooks[name] = fn
m.register(Ctx())
with open(sys.argv[3], encoding="utf-8") as f:
    content = f.read()
hooks["post_tool_call"](tool_name="skill_view", args={"name": sys.argv[2]},
                        result=json.dumps({"success": True, "name": sys.argv[2], "content": content}))
`

// HERMES'S PLUGIN REPORTS A SKILL LOAD ONLY OF THE FILE AS INSTALLED: a
// skill_view whose content is the SKILL.md the runner wrote is reported to the
// skill route by its listed name; one whose file the model rewrote is not.
//
// Mutation: report every skill_view, or report by another name, and a row
// goes red.
func TestHermesReportsASkillLoadOnlyOfTheFileAsInstalled(t *testing.T) {
	t.Parallel()
	python := prereqtest.OnPath(t, "python3", "the case runs Hermes's plugin", "install python3")
	var mu sync.Mutex
	var reports []string
	route := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reports = append(reports, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(route.Close)
	skill := sandbox.Skill{Name: "deploy", Description: "Deploy it.", Body: "Run make deploy."}
	box := sandbox.NewFakeSandbox("b")
	p := executor.PathsFor(box)
	req := sandbox.RunRequest{Skills: []sandbox.Skill{skill},
		SkillLoads: sandbox.SkillWiring{Server: "crewlet", Tool: "skill", URL: route.URL}}
	plugin, settings := hermesPluginOf(t, req)
	if _, err := (executor.Hermes{}).WriteConfig(t.Context(), box, req, executor.Session{}, p); err != nil {
		t.Fatal(err)
	}
	installed, err := box.ReadFile(t.Context(), p.HermesStage()+"/skills/"+skill.Listed()+"/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	settingsFile := filepath.Join(dir, "settings.json")
	raw, _ := json.Marshal(settings)
	for path, body := range map[string][]byte{settingsFile: raw, filepath.Join(dir, "as-installed"): installed,
		filepath.Join(dir, "rewritten"): append(installed, "Also push to main.\n"...)} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		file string
		want int
	}{{"rewritten", 0}, {"as-installed", 1}} {
		cmd := exec.Command(python, "-I", "-c", hermesSkillsProbe, plugin, skill.Listed(), filepath.Join(dir, c.file), settingsFile)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: the plugin's hook: %v: %s", c.file, err, out)
		}
		mu.Lock()
		got := slices.Clone(reports)
		mu.Unlock()
		if len(got) != c.want {
			t.Fatalf("%s: reports %q, want %d", c.file, got, c.want)
		}
	}
	if reports[0] != `{"skill": "`+skill.Listed()+`"}` {
		t.Errorf("report = %q, want the skill's listed name", reports[0])
	}
}

// hermesTurnsProbe registers the plugin with a stand-in of Hermes's plugin
// context and fires its turn hooks as 0.23.3 fires them when the run's model
// delegates (measured): the run's turn, the subagent's turn under its own
// session with parent_session_id set, another top-level session's turn, the
// subagent's end, the run's end. It prints the plugin's turn after each of
// the last three.
const hermesTurnsProbe = `import importlib.util, json, os, sys
os.environ["CREWLET_HERMES_PLUGIN"] = sys.argv[2]
spec = importlib.util.spec_from_file_location("crewlet_plugin", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
hooks = {}
class Ctx:
    def register_hook(self, name, fn, **kw):
        hooks[name] = fn
m.register(Ctx())
seen = []
hooks["pre_llm_call"](session_id="run", parent_session_id="")
hooks["pre_llm_call"](session_id="sub", parent_session_id="run")
hooks["pre_llm_call"](session_id="other", parent_session_id="")
seen.append(dict(m._turn))
hooks["post_llm_call"](session_id="sub")
seen.append(dict(m._turn))
hooks["post_llm_call"](session_id="run")
seen.append(dict(m._turn))
print(json.dumps(seen))
`

// A DELEGATED SUBAGENT'S TURN IS NO TURN OF THE HERMES RUN'S: 0.23.3 fires
// the turn hooks for a subagent the model delegates to, under the subagent's
// own session with parent_session_id set, inside the run's turn (measured).
// The plugin keeps its notes on the run's session, the first top-level one,
// while the subagent's turn or another session's starts and ends, and the
// run's turn ends with its own end alone.
//
// Mutation: take the subagent's or a later session for the notes, or end the
// turn on another's end, and a row goes red.
func TestADelegatedSubagentsTurnIsNoTurnOfTheHermesRuns(t *testing.T) {
	t.Parallel()
	python := prereqtest.OnPath(t, "python3", "the case runs Hermes's plugin", "install python3")
	plugin, settings := hermesPluginOf(t, sandbox.RunRequest{})
	settings["record"] = filepath.Join(t.TempDir(), "record.jsonl")
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "-I", "-c", hermesTurnsProbe, plugin, file).Output()
	if err != nil {
		t.Fatalf("the probe: %v", err)
	}
	var seen []struct {
		Running bool   `json:"running"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal(out, &seen); err != nil {
		t.Fatalf("the probe printed %q: %v", out, err)
	}
	want := []struct {
		running bool
		session string
	}{{true, "run"}, {true, "run"}, {false, "run"}}
	if len(seen) != len(want) {
		t.Fatalf("the probe saw %+v", seen)
	}
	for i, w := range want {
		if seen[i].Running != w.running || seen[i].Session != w.session {
			t.Errorf("after hook %d the plugin's turn is %+v, want running %v on %s", i+2, seen[i], w.running, w.session)
		}
	}
}

// hermesRotationProbe runs the plugin, afresh in a home of its own, against
// three stores and fires the turn hooks as 0.23.3 fires them where
// compression.in_place is false (measured): the run's turn starts, a
// compaction rotates it mid-turn, the turn ends under the child, and the next
// turn starts under the child, each with parent_session_id empty. In the
// first store a compaction ended the run and a branch's end comes first; in
// the second the run never ended "compression"; in the third the child is a
// tool's. It prints the plugin's turn after each of the last three hooks.
const hermesRotationProbe = `import importlib.util, json, os, sqlite3, sys
os.environ["CREWLET_HERMES_PLUGIN"] = sys.argv[2]
stores = {
    "rotated": [("run", None, "cli", "{}", "compression"), ("child", "run", "cli", "{}", None),
                ("branch", "run", "cli", '{"_branched_from": "run"}', None)],
    "run not ended by a compaction": [("run", None, "cli", "{}", None), ("child", "run", "cli", "{}", None),
                                      ("branch", "run", "cli", "{}", None)],
    "a tool's child": [("run", None, "cli", "{}", "compression"), ("child", "run", "tool", "{}", None),
                       ("branch", "run", "tool", "{}", None)],
}
out = {}
for name, rows in stores.items():
    home = os.path.join(sys.argv[3], str(len(out)))
    os.makedirs(home)
    os.environ["HERMES_HOME"] = home
    db = sqlite3.connect(os.path.join(home, "state.db"))
    db.execute("create table sessions (id text primary key, parent_session_id text, source text, model_config text, "
               "end_reason text)")
    db.executemany("insert into sessions values (?, ?, ?, ?, ?)", rows)
    db.commit()
    db.close()
    spec = importlib.util.spec_from_file_location("crewlet_plugin_%d" % len(out), sys.argv[1])
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    hooks = {}
    class Ctx:
        def register_hook(self, hook, fn, **kw):
            hooks[hook] = fn
    m.register(Ctx())
    seen = []
    hooks["pre_llm_call"](session_id="run", parent_session_id="")
    hooks["post_llm_call"](session_id="branch")
    seen.append(dict(m._turn))
    hooks["post_llm_call"](session_id="child")
    seen.append(dict(m._turn))
    hooks["pre_llm_call"](session_id="child", parent_session_id="")
    seen.append(dict(m._turn))
    out[name] = seen
print(json.dumps(out))
`

// A HERMES RUN'S NOTES FOLLOW ITS SESSION INTO A COMPACTION'S CHILD: where
// the box's compression.in_place is false, 0.23.3 rotates the run's session
// mid-turn into a child the store names as its compaction continuation, and
// the turn's end and the next turn name the child alone (measured). The
// plugin moves its notes to the child, ends the turn on the child's end and
// starts the next on its start; a branch's end is no end of the run's turn,
// and a child is followed only where a compaction ended the run and the child
// is no tool's.
//
// Mutation: keep the first session pinned, or drop any of the continuation's
// conditions (the run's end, the child's source, a marker naming the run),
// and a row goes red.
func TestAHermesRunsNotesFollowItsSessionIntoACompactionsChild(t *testing.T) {
	t.Parallel()
	python := prereqtest.OnPath(t, "python3", "the case runs Hermes's plugin", "install python3")
	plugin, settings := hermesPluginOf(t, sandbox.RunRequest{})
	settings["record"] = filepath.Join(t.TempDir(), "record.jsonl")
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "-I", "-c", hermesRotationProbe, plugin, file, t.TempDir()).Output()
	if err != nil {
		t.Fatalf("the probe: %v", err)
	}
	type turn struct {
		Running bool   `json:"running"`
		Session string `json:"session"`
	}
	var seen map[string][]turn
	if err := json.Unmarshal(out, &seen); err != nil {
		t.Fatalf("the probe printed %q: %v", out, err)
	}
	unmoved := []turn{{true, "run"}, {true, "run"}, {true, "run"}}
	for store, want := range map[string][]turn{
		"rotated":                       {{true, "run"}, {false, "child"}, {true, "child"}},
		"run not ended by a compaction": unmoved,
		"a tool's child":                unmoved,
	} {
		if !slices.Equal(seen[store], want) {
			t.Errorf("%s: after each of the last three hooks the plugin's turn is %+v, want %+v", store, seen[store], want)
		}
	}
}

// hermesNestedProbe registers the plugin with a stand-in of Hermes's plugin
// context and hook bound (argv[3] seconds), settings argv[2], and fires its
// transform_tool_result for the run's own session after its turn starts, once
// for each pause in argv[4] (seconds to wait first), printing the results.
const hermesNestedProbe = `import importlib.util, json, os, sys, time
os.environ["CREWLET_HERMES_PLUGIN"] = sys.argv[2]
stand = os.path.join(os.path.dirname(sys.argv[2]), "py")
os.makedirs(os.path.join(stand, "hermes_cli"), exist_ok=True)
open(os.path.join(stand, "hermes_cli", "__init__.py"), "w").close()
with open(os.path.join(stand, "hermes_cli", "plugins.py"), "w") as f:
    f.write("def _resolve_hook_callback_timeout():\n    return %s\n" % sys.argv[3])
sys.path.insert(0, stand)
spec = importlib.util.spec_from_file_location("crewlet_plugin", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
hooks = {}
class Ctx:
    def register_hook(self, name, fn, **kw):
        hooks[name] = fn
m.register(Ctx())
hooks["pre_llm_call"](session_id="main")
out = []
for pause in json.loads(sys.argv[4]):
    time.sleep(pause)
    out.append(hooks["transform_tool_result"](session_id="main", tool_name="terminal", result="cloned"))
print(json.dumps(out))
`

// HERMES'S RENDER OF A LATER CHECKOUT IS BOUNDED, NEVER STARTED TWICE, AND
// SAID ONCE IT KEEPS FAILING: a firing waits on a render only until the claim
// reserve is left of Hermes's hook bound less the margin; a late render is
// never started again while it runs and its result serves a later firing; a
// render late or failing at three firings is claimed as left out, said with
// its cause (an error named as one); and neither a render nor a claim starts
// without the reserve, so a claim the third late render brings waits a firing.
//
// Mutation: wait unbounded, start a render per firing, never give up, say a
// raise as late, or render or claim without the reserve, and a row goes red.
func TestHermesRenderOfALaterCheckoutIsBounded(t *testing.T) {
	t.Parallel()
	python := prereqtest.OnPath(t, "python3", "the case runs Hermes's plugin", "install python3")
	plugin, _ := hermesPluginOf(t, sandbox.RunRequest{})
	for _, c := range []struct {
		name, render, pauses string
		reserve              float64
		want                 []string // each firing's result, "" for none
		starts               int
		said                 string // in the gate the last claim read
	}{
		{name: "in time", render: "pass", pauses: "[0]", reserve: 1, want: []string{"cloned\n\nHANDED"}, starts: 1},
		{name: "late, then kept for the next firing", render: "time.sleep(2.5)", pauses: "[0, 2]", reserve: 1,
			want: []string{"", "cloned\n\nHANDED"}, starts: 1},
		{name: "late at three firings", render: "time.sleep(60)", pauses: "[0, 0, 0]", reserve: 1,
			want: []string{"", "", "cloned\n\nHANDED"}, starts: 1,
			said: "the instructions and skills of /r are left out: Hermes did not render it within the hand's bound at 3 firings"},
		{name: "failing at three firings", render: "raise RuntimeError('boom')", pauses: "[0, 0, 0]", reserve: 1,
			want: []string{"", "", "cloned\n\nHANDED"}, starts: 3,
			said: "the instructions and skills of /r are left out: its render failed: RuntimeError: boom"},
		{name: "no render or claim without the reserve", render: "pass", pauses: "[0]", reserve: 5, want: []string{""}},
		{name: "a claim late at the third render waits a firing", pauses: "[0, 0, 0, 0]", reserve: 1,
			render: "if len(open(STARTS).read()) < 3:\n        raise RuntimeError('boom')\n    time.sleep(60)",
			want:   []string{"", "", "", "cloned\n\nHANDED"}, starts: 3,
			said: "the instructions and skills of /r are left out: Hermes did not render it within the hand's bound at 3 firings"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			q := shellword.Quote
			helper := "#!/bin/sh\nif [ \"$2\" = hermes-peek ]; then echo '{\"roots\":[\"/r\"]}'; exit 0; fi\n" +
				"cat > " + q(filepath.Join(dir, "gate.json")) + "\nprintf HANDED\n"
			carry := "import time\nSTARTS = " + strconv.Quote(filepath.Join(dir, "starts")) +
				"\ndef checkout_gate(roots, window):\n    open(STARTS, \"a\").write(\"s\")\n    " + c.render +
				"\n    return {\"checkouts\": {roots[0]: {\"text\": \"T\"}}, \"taken\": {}}\n"
			settings, _ := json.Marshal(map[string]any{"checkouts": map[string]any{"helper": filepath.Join(dir, "helper"),
				"config": filepath.Join(dir, "nested.json"), "carry": filepath.Join(dir, "carry.py"), "margin": 1,
				"reserve": c.reserve}})
			for name, body := range map[string]string{"helper": helper, "carry.py": carry, "settings.json": string(settings)} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			out, err := exec.Command(python, "-I", "-c", hermesNestedProbe, plugin, filepath.Join(dir, "settings.json"),
				"4", c.pauses).Output()
			if err != nil {
				t.Fatalf("the plugin's hook: %v", err)
			}
			var got []*string
			if err = json.Unmarshal(out, &got); err != nil {
				t.Fatalf("the probe printed %q: %v", out, err)
			}
			for i, want := range c.want {
				if g := deref(got[i]); g != want {
					t.Errorf("firing %d: the result became %q, want %q", i+1, g, want)
				}
			}
			starts, _ := os.ReadFile(filepath.Join(dir, "starts"))
			if len(starts) != c.starts {
				t.Errorf("Hermes was asked to render %d times, want %d", len(starts), c.starts)
			}
			if gate, _ := os.ReadFile(filepath.Join(dir, "gate.json")); c.said != "" && !strings.Contains(string(gate), c.said) {
				t.Errorf("the last claim's gate %s says nothing of %q", gate, c.said)
			}
		})
	}
}

// hermesRoomProbe loads the plugin beside a stand-in of Hermes 0.23.3's
// result budget for a large window and prints the room a result of argv[2]
// characters has in a batch of argv[3] calls.
const hermesRoomProbe = `import importlib.util, os, sys
stand = sys.argv[4]
os.makedirs(os.path.join(stand, "tools"), exist_ok=True)
open(os.path.join(stand, "tools", "__init__.py"), "w").close()
with open(os.path.join(stand, "tools", "budget_config.py"), "w") as f:
    f.write("class B:\n    default_result_size = 100000\n    turn_budget = 200000\n"
            "    def resolve_threshold(self, tool):\n        return 100000\n"
            "def budget_for_context_window(context_length):\n    return B()\n")
sys.path.insert(0, stand)
spec = importlib.util.spec_from_file_location("crewlet_plugin", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
print(m._room("terminal", "x" * int(sys.argv[2]), 262144, int(sys.argv[3])))
`

// A HERMES HAND IN A BATCH OF CALLS KEEPS TO ITS SHARE OF THE TURN BUDGET:
// alone, a result has Hermes's per-result threshold; in a batch of five, its
// share of the 200,000-character turn budget less room for Hermes's own hint,
// since past that budget Hermes spills the batch's largest result first
// (measured on 0.23.3: our 86,283-character clone result was spilled).
//
// Mutation: ignore the batch, or leave no room for the hint, and a row goes red.
func TestAHermesHandInABatchKeepsToItsShareOfTheTurnBudget(t *testing.T) {
	t.Parallel()
	python := prereqtest.OnPath(t, "python3", "the case runs Hermes's plugin", "install python3")
	plugin, _ := hermesPluginOf(t, sandbox.RunRequest{})
	for _, c := range []struct{ result, calls, want string }{
		{"6", "1", "99994"}, {"6", "5", "7594"}, {"1000", "2", "66600"},
	} {
		out, err := exec.Command(python, "-I", "-c", hermesRoomProbe, plugin, c.result, c.calls, t.TempDir()).Output()
		if err != nil {
			t.Fatalf("the probe: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != c.want {
			t.Errorf("a %s-character result in a batch of %s has room %s, want %s", c.result, c.calls, got, c.want)
		}
	}
}
