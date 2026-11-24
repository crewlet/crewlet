package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// hermesStream is a measured stream with the plugin's record of its responses
// appended as the run's script appends them, and extra lines after.
func hermesStream(t *testing.T, stream, requests string, extra ...string) []byte {
	t.Helper()
	out := bytes.Clone(hermesFixture(t, "0.23.3", stream))
	if requests != "" {
		for l := range bytes.Lines(hermesFixture(t, "0.23.3", requests)) {
			out = append(out, `{"type":"`+hermesRequestEvent+`","line":`...)
			out = append(out, bytes.TrimSpace(l)...)
			out = append(out, "}\n"...)
		}
	}
	for _, l := range extra {
		out = append(out, l+"\n"...)
	}
	return out
}

// A HERMES RUN IS REFUSED BY ITS RELEASE BEFORE ANYTHING IS WRITTEN INTO THE
// HERMES HOME: the release gate comes before the plugin, the skills and the
// plugin's enabling, so a box on a release the runner does not drive keeps its
// home as it was. And WriteConfig itself writes only under the work directory.
//
// Mutation: install before the refusal, or have WriteConfig write the plugin
// or a skill into the Hermes home, and this goes red.
func TestAHermesRunIsRefusedBeforeItWritesTheHermesHome(t *testing.T) {
	t.Parallel()
	box := &writeLog{FakeSandbox: sandbox.NewFakeSandbox("b")}
	p := PathsFor(box)
	req := sandbox.RunRequest{System: "be brief", Skills: []sandbox.Skill{{Name: "deploy", Description: "d", Body: "b"}},
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://bridge.example.com/mcp"}}}
	if _, err := (Hermes{}).WriteConfig(t.Context(), box, req, Session{ID: "s"}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	for _, path := range box.paths {
		if strings.HasPrefix(path, p.HermesHome()+"/") {
			t.Errorf("WriteConfig wrote %s into the Hermes home before any release was read", path)
		}
	}
	script := hermesScript(req, Session{ID: "s"}, p, hermesRunTimes())
	refusal := strings.Index(script, "the hermes runner drives")
	for _, step := range []string{"cp -R", "hermes plugins enable", "hermes chat"} {
		if at := strings.Index(script, step); at < 0 || at < refusal {
			t.Errorf("%q comes before the release refusal (at %d, refusal at %d)", step, at, refusal)
		}
	}
}

// writeLog is a box that keeps the path of every file written to it.
type writeLog struct {
	*sandbox.FakeSandbox
	paths []string
}

func (w *writeLog) WriteFile(ctx context.Context, p string, content []byte) error {
	w.paths = append(w.paths, p)
	return w.FakeSandbox.WriteFile(ctx, p, content)
}

// A HERMES RUN NAMES ITS SESSION ITSELF AND CREATES IT ONLY WHERE IT IS NEW:
// the session is the runner's id as Hermes's title, `-c <id>` on every launch
// and `--create-if-missing` only on one that starts it; a continued run is
// handed a fresh id beside the one it continues.
//
// Mutation: drop --create-if-missing's condition, or mint no fresh id for a
// continued run, and this goes red.
func TestAHermesRunNamesItsSessionAndCreatesItOnlyWhereNew(t *testing.T) {
	t.Parallel()
	fresh := (Hermes{}).Session(sandbox.RunRequest{})
	if !sandbox.ValidSessionID(fresh.ID) || fresh.Continues {
		t.Fatalf("a new run's session = %+v, want a minted id of its own", fresh)
	}
	cont := (Hermes{}).Session(sandbox.RunRequest{Resume: hermesFixtureTitle})
	if cont.ID != hermesFixtureTitle || !cont.Continues || !sandbox.ValidSessionID(cont.Fresh) || cont.Fresh == cont.ID {
		t.Fatalf("a continued run's session = %+v, want the one it continues and a fresh id beside it", cont)
	}
	script := hermesScript(sandbox.RunRequest{}, fresh, PathsFor(sandbox.NewFakeSandbox("b")), hermesRunTimes())
	if !strings.Contains(script, `set -- -c "$sid"; [ "$k" = yes ] || set -- "$@" --create-if-missing`) {
		t.Errorf("the script does not name its session by -c and create it only where new:\n%s", script)
	}
}

// A HERMES RUN'S END STOPS THE GATEWAY: an end request stops the gateway
// before the client (killing the client leaves the turn running in it,
// measured on 0.23.3), and the script's exit stops it however the script ends.
//
// Mutation: drop the gateway's stop from endharness or from the EXIT trap, and
// this goes red.
func TestAHermesRunsEndStopsItsGateway(t *testing.T) {
	t.Parallel()
	script := hermesScript(sandbox.RunRequest{}, Session{ID: "s"}, PathsFor(sandbox.NewFakeSandbox("b")), hermesRunTimes())
	for _, want := range []string{`endedas=asked; endgw; end "$harness"`, `trap 'endgw' EXIT`,
		`quit() { trap '' USR1; trap - EXIT TERM INT HUP; endgw;`} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
}

// A HERMES STREAM READS AS ITS ANSWER, ITS CALLS AND ITS SPEND: the result
// event's text is the answer, each call is said, and the run's spend is what
// the plugin recorded of each response, the cache beside the prompt.
//
// Mutation: read the answer off the text deltas alone, drop a call, or sum
// the record wrong, and this goes red.
func TestAHermesStreamReadsAsItsAnswerCallsAndSpend(t *testing.T) {
	t.Parallel()
	res := (Hermes{}).Parse(hermesStream(t, "stream-steered.jsonl", "requests-steered.jsonl"))
	if !res.Success || res.Text != "STUB-OK" {
		t.Fatalf("result = %+v, want a success answering STUB-OK", res)
	}
	if !strings.Contains(res.Transcript, "terminal") {
		t.Errorf("the transcript does not say the terminal call:\n%s", res.Transcript)
	}
	d := &hermesEvents{}
	for l := range bytes.Lines(hermesStream(t, "stream-steered.jsonl", "requests-steered.jsonl")) {
		d.Line(bytes.TrimSpace(l))
	}
	spend, ok := d.runSpend()
	if !ok || spend.input != 20 || spend.output != 4 {
		t.Errorf("spend = %+v (%v), want the two recorded responses' 20 in and 4 out", spend, ok)
	}
	if got := d.reportedWindow(); got != 262144 {
		t.Errorf("window = %d, want the recorded 262144", got)
	}
}

// A HERMES RUN ENDS ON ITS ROUND CAP ONLY WHERE BOTH SAY SO: Hermes's own
// sentence as it exited 1, which the script turns into the cap's line, and
// the record's last turn holding the cap's tool rounds. Either alone keeps the
// run's own cause.
//
// Mutation: read the cap off either signal alone, and a row goes red.
func TestAHermesRunEndsOnItsRoundCapOnlyWhereBothSay(t *testing.T) {
	t.Parallel()
	capLine := `{"type":"` + hermesCappedEvent + `","rounds":2}`
	for _, c := range []struct {
		name     string
		stream   []byte
		want     bool
		failWith string
	}{
		{"summary given", hermesStream(t, "stream-capped-summary.jsonl", "requests-capped-summary.jsonl", capLine), true, ""},
		{"no summary", hermesStream(t, "stream-capped.jsonl", "requests-capped.jsonl", capLine), true, ""},
		{"no cap line", hermesStream(t, "stream-capped.jsonl", "requests-capped.jsonl"), false, "iteration limit"},
		{"no rounds", hermesStream(t, "stream-capped.jsonl", "", capLine), false, "iteration limit"},
	} {
		res := (Hermes{}).Parse(c.stream)
		if res.RoundCapped != c.want || res.Success {
			t.Errorf("%s: capped %v success %v, want capped %v and no success (%s)", c.name, res.RoundCapped,
				res.Success, c.want, res.Error)
		}
		if c.failWith != "" && !strings.Contains(res.Error, c.failWith) {
			t.Errorf("%s: error %q, want Hermes's own words", c.name, res.Error)
		}
	}
	if !strings.Contains(string(hermesFixture(t, "0.23.3", "stream-capped-summary.jsonl")), hermesCapSentence) ||
		!strings.Contains(string(hermesFixture(t, "0.23.3", "stream-capped.jsonl")), hermesCapNoSummary) {
		t.Error("the cap's sentences are not what 0.23.3 wrote")
	}
}

// A REFUSED KEY IS THE RUN'S FAILURE IN HERMES'S OWN WORDS, where the
// profile's sign-in markers read it.
//
// Mutation: drop the result's error or text from the failure, and this goes red.
func TestARefusedHermesKeyIsTheRunsFailure(t *testing.T) {
	t.Parallel()
	res := (Hermes{}).Parse(hermesFixture(t, "0.23.3", "stream-refused.jsonl"))
	if res.Success || !strings.Contains(res.Error, "401") {
		t.Fatalf("result = %+v, want a failure naming the refusal", res)
	}
}

// A RELAUNCH AFTER A DISCARD IS THE RUN'S ENDING: the refused launch's result
// is not the run's once the second launch starts its own stream.
//
// Mutation: keep the first result past a new init, and this goes red.
func TestAHermesRelaunchsResultIsTheRuns(t *testing.T) {
	t.Parallel()
	refused := `{"type": "system", "subtype": "init"}` + "\n" +
		`{"type": "result", "exit_code": 3, "text": "", "error": "Your message was not sent. To continue, run ` +
		"`hermes sessions discard x --yes`" + `"}` + "\n"
	res := (Hermes{}).Parse(append([]byte(refused), hermesStream(t, "stream-steered.jsonl", "requests-steered.jsonl")...))
	if !res.Success || res.Text != "STUB-OK" {
		t.Fatalf("result = %+v, want the relaunch's answer", res)
	}
}

// A STEERED NOTE IS READ BACK AS THE NOTE IT FRAMES: Hermes's frame around a
// steer is taken off byte for byte, and a user message it did not frame is no
// note.
//
// Mutation: loosen or drop the frame's match, and this goes red.
func TestAHermesSteeredNoteIsReadAsTheNoteItFrames(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		text, note string
		ok         bool
	}{
		{hermesNoteOpen + "fix it" + hermesNoteClose, "fix it", true},
		{"fix it", "", false},
		{hermesNoteOpen + "fix it", "", false},
	} {
		if note, ok := hermesSteered(c.text); note != c.note || ok != c.ok {
			t.Errorf("hermesSteered(%q) = %q, %v, want %q, %v", c.text, note, ok, c.note, c.ok)
		}
	}
}

// A HERMES SESSION READS AS ITS TRANSCRIPT: the person's brief, the model's
// call and its result, the steered note as a person's words and the model's
// answer, in the order the store wrote them.
//
// Mutation: drop a role's entries, or keep the steer's frame, and this goes red.
func TestAHermesSessionReadsAsItsTranscript(t *testing.T) {
	t.Parallel()
	entries := hermesTranscriptOf(t, kimiArchive(t, map[string][]byte{
		hermesArchiveMember: hermesFixture(t, "0.23.3", "session-steered.jsonl")}))
	var got []string
	for _, e := range entries {
		got = append(got, string(e.Kind)+":"+e.Tool+":"+strings.TrimSpace(e.Text))
	}
	want := []string{"brief::steer me", "tool_call:terminal:", "tool_result:terminal:", "brief::" + hermesSteeredNote,
		"model::STUB-OK"}
	if len(got) != len(want) {
		t.Fatalf("entries = %q, want %q", got, want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("entry %d = %q, want it to open %q", i, got[i], want[i])
		}
	}
}

// THE OVERLAY CARRIES THE RUN'S SERVERS, ITS WEB DENIAL AND ITS ALWAYS-LOADED
// TOOLS, and only those: each server as written, the web, browser and x_search
// toolsets disabled on a web-denied run alone (an allowed run's own browser
// has its case below), tool search off where a server is always loaded alone
// (Hermes exempts no server from it), and a value Hermes would expand refused.
//
// Mutation: drop a server's header, disable the web on an allowed run, leave
// tool search on beside an always-loaded server or turn it off with none, or
// let a `${` through, and a row goes red.
func TestTheHermesOverlayCarriesTheServersAndTheWebDenial(t *testing.T) {
	t.Parallel()
	servers := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://bridge.example.com/mcp",
		Headers: map[string]string{"Authorization": "Bearer t"}, CallTimeout: 600 * time.Second, AlwaysLoaded: true},
		"other": {Transport: sandbox.TransportHTTP, URL: "http://other.example.com/mcp"}}
	raw, err := hermesOverlay(servers, true)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Servers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Timeout int               `json:"timeout"`
		} `json:"mcp_servers"`
		Agent struct {
			Disabled []string `json:"disabled_toolsets"`
		} `json:"agent"`
		Tools struct {
			Search struct {
				Enabled string `json:"enabled"`
			} `json:"tool_search"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	s := got.Servers["crewlet"]
	if s.URL != "http://bridge.example.com/mcp" || s.Headers["Authorization"] != "Bearer t" || s.Timeout != 600 {
		t.Errorf("server = %+v", s)
	}
	if !slices.Equal(got.Agent.Disabled, []string{"web", "browser", "x_search"}) {
		t.Errorf("disabled toolsets = %v", got.Agent.Disabled)
	}
	if got.Tools.Search.Enabled != "off" {
		t.Errorf("tool search beside an always-loaded server = %q, want off", got.Tools.Search.Enabled)
	}
	delete(servers, "crewlet")
	allowed, err := hermesOverlay(servers, false)
	if err != nil || strings.Contains(string(allowed), `"web"`) || strings.Contains(string(allowed), "tool_search") {
		t.Errorf("an allowed run with no always-loaded server disables the web or tool search: %s (%v)", allowed, err)
	}
	servers["crewlet"] = sandbox.MCPServer{Transport: sandbox.TransportHTTP, URL: "http://x/${HOME}"}
	if _, err := hermesOverlay(servers, false); err == nil {
		t.Error("a value Hermes would expand was let through")
	}
}

// EVERY HERMES RUN BUT ONE HANDED A SEAT'S BROWSER TURNS HERMES'S OWN BROWSER
// OFF: a run with no browser server, a browser seat's whose browser the launch
// withheld, and a web-denied run each disable the `browser` toolset
// (browser_exec and its siblings, which install and run a Chromium in whatever
// box the run lands in); a run handed the seat's browser leaves it on, since
// Hermes 0.23.3 merges the seat's `browser` server into that toolset, so
// disabling it would hide the server too.
//
// Mutations, each red: disable it on every run; on a web-denied run alone; on
// a run handed the seat's browser alone.
func TestEveryHermesRunButOneHandedASeatsBrowserTurnsItsOwnBrowserOff(t *testing.T) {
	t.Parallel()
	bridge := sandbox.MCPServer{Name: "crewlet", Transport: sandbox.TransportHTTP, URL: "http://bridge.example.com/mcp",
		AlwaysLoaded: true}
	seat := sandbox.MCPServer{Name: browser.ServerName, Transport: sandbox.TransportStdio, Command: "node",
		Args: []string{"/home/user/.crewlet/browser-box/helper.mjs", "mcp"}}
	for name, c := range map[string]struct {
		servers   map[string]sandbox.MCPServer
		webDenied bool
		want      []string
	}{
		"a run with no browser": {servers: map[string]sandbox.MCPServer{"crewlet": bridge},
			want: []string{"browser"}},
		"a seat's browser handed": {servers: map[string]sandbox.MCPServer{"crewlet": bridge,
			browser.ServerName: seat}},
		"a web-denied run": {servers: map[string]sandbox.MCPServer{"crewlet": bridge}, webDenied: true,
			want: []string{"web", "browser", "x_search"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := hermesOverlay(c.servers, c.webDenied)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Agent struct {
					Disabled []string `json:"disabled_toolsets"`
				} `json:"agent"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Agent.Disabled, c.want) {
				t.Errorf("the run disables %v, want %v:\n%s", got.Agent.Disabled, c.want, raw)
			}
		})
	}
}

// A HERMES SESSION CONTINUES ONLY BETWEEN BOXES ON ITS ONE RELEASE, written by
// it: an older, newer or unread release continues nothing.
//
// Mutation: hold the box or the writer loosely, and a row goes red.
func TestAHermesSessionContinuesOnlyOnItsOneRelease(t *testing.T) {
	t.Parallel()
	floor := hermesFloor()
	for _, c := range []struct {
		box, writer string
		ok          bool
	}{
		{floor, floor, true}, {"0.23.4", floor, false}, {"0.23.2", floor, false}, {"", floor, false},
		{floor, "0.23.4", false}, {floor, "", false},
	} {
		if ok, why := (Hermes{}).Continues(c.box, c.writer); ok != c.ok || (!ok && why == "") {
			t.Errorf("Continues(%q, %q) = %v, %q, want %v", c.box, c.writer, ok, why, c.ok)
		}
	}
}

// hermesLauncherStandIn is a directory holding a `hermes` that runs what its
// launcher's `--run-module cProfile -o /dev/null <script> <args>` runs, the
// script under the machine's python3, which keeps no exit status either, with
// [hermesLoaderStandIn] as Hermes's own modules; the box's managed scope is
// the directory's `etc-hermes` ([hermesSystemScope]).
func hermesLauncherStandIn(t *testing.T) string {
	t.Helper()
	python := prereqtest.OnPath(t, "python3", "the case runs the run's carry script", "install python3")
	bin := t.TempDir()
	for name, src := range hermesLoaderStandIn(hermesSystemScope(bin)) {
		module := filepath.Join(bin, "py", name)
		if err := os.MkdirAll(filepath.Dir(module), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(module, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := "import runpy, sys; sys.path.insert(0, " + strconv.Quote(filepath.Join(bin, "py")) +
		"); sys.argv = sys.argv[1:]; runpy.run_path(sys.argv[0], run_name='__main__')"
	launcher := "#!/bin/sh\n[ \"$1 $2 $3 $4\" = '--run-module cProfile -o /dev/null' ] || exit 2\nshift 4\n" +
		shellword.Quote(python) + " -I -c " + shellword.Quote(run) + " \"$@\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "hermes"), []byte(launcher), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// hermesSystemScope is the stand-in's `/etc/hermes`.
func hermesSystemScope(bin string) string { return filepath.Join(bin, "etc-hermes") }

// hermesLoaderStandIn is what the carry script reads of Hermes 0.23.3's own
// modules, as it reads them (YAML read as the JSON the cases write): the
// managed scope HERMES_MANAGED_DIR names, else system, where it is a
// directory; DEFAULT_CONFIG with HERMES_HOME's config.yaml and that scope's
// over it per leaf (its default model is a stand-in's, where 0.23.3's is "",
// so a case can tell a read with defaults from one without); -m's
// provider:model; and the route a launch resolves, the configured one's
// endpoint for its own provider, another's as the measured box named it. Each
// function keeps 0.23.3's own parameters, no catch-all, so a keyword Hermes
// lacks fails the carry as it would in the box.
func hermesLoaderStandIn(system string) map[string]string {
	return map[string]string{
		"gateway/__init__.py": "",
		"gateway/session_local_route.py": `def resolve_launch_route(params, config):
    provider, named, model = params["model"].partition(":")
    return dict(params, model=model, provider=provider) if named else params
`,
		"hermes_cli/__init__.py": "",
		"hermes_cli/managed_scope.py": `import json, os
from pathlib import Path
def get_managed_dir():
    p = Path(os.environ.get("HERMES_MANAGED_DIR", "").strip() or ` + strconv.Quote(system) + `)
    return p if p.is_dir() else None
def load_managed_config(*, strict=False):
    d = get_managed_dir()
    return json.loads((d / "config.yaml").read_text()) if d is not None and (d / "config.yaml").is_file() else {}
`,
		"hermes_cli/config.py": `import copy, json, os
from hermes_cli import managed_scope
DEFAULT_CONFIG = {"model": {"default": "stand-in-default"}}
def _deep_merge(base, override):
    result = base.copy()
    for k, v in override.items():
        over_dict = isinstance(result.get(k), dict)
        if over_dict and isinstance(v, dict):
            result[k] = _deep_merge(result[k], v)
        elif not (over_dict and v is None):
            result[k] = v
    return result
def load_config():
    path = os.path.join(os.environ.get("HERMES_HOME", "/nonexistent"), "config.yaml")
    user = json.load(open(path)) if os.path.isfile(path) else {}
    return _deep_merge(_deep_merge(copy.deepcopy(DEFAULT_CONFIG), user), managed_scope.load_managed_config())
`,
		"hermes_cli/runtime_provider.py": `from hermes_cli.config import load_config
ENDPOINTS = {"anthropic": "http://stub:9000"}
def resolve_runtime_provider(*, requested=None, explicit_api_key=None, explicit_base_url=None, target_model=None):
    model = load_config().get("model") or {}
    provider = requested or model.get("provider")
    base = explicit_base_url or (model.get("base_url") if provider == model.get("provider") else ENDPOINTS.get(provider))
    return {"provider": provider, "base_url": base, "api_mode": "chat_completions"}
`,
	}
}

// THE RUN'S MANAGED SCOPE IS THE BOX'S OWN WITH THE RUN'S KEYS OVER IT: the
// box's managed config.yaml and .env (which the run's HERMES_MANAGED_DIR would
// otherwise hide) kept, the run's servers and settings over them per leaf, and
// a web-denied run's disabled toolsets the box's effective list (its own
// config under its managed scope, lists whole) and then the run's; a box with
// no managed scope leaves no .env of an earlier run's.
//
// Mutation: write the run's overlay alone, replace the box's list with the
// run's, read the box's list without its managed scope, or keep an earlier
// .env, and a case goes red.
func TestAHermesRunsManagedScopeIsTheBoxsWithTheRunsKeysOver(t *testing.T) {
	t.Parallel()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	servers := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://bridge.example.com/mcp",
		AlwaysLoaded: true}}
	type overlay struct {
		Servers map[string]map[string]any `json:"mcp_servers"`
		Model   map[string]string         `json:"model"`
		Agent   struct {
			Disabled []string `json:"disabled_toolsets"`
		} `json:"agent"`
	}
	install := func(t *testing.T, scope map[string][]byte, webDenied bool) (overlay, []byte, bool) {
		t.Helper()
		bin := hermesLauncherStandIn(t)
		paths := Paths{Home: t.TempDir()}
		for _, dir := range []string{paths.HermesManaged(), paths.HermesHome()} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		staged, err := hermesOverlay(servers, webDenied)
		if err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{
			paths.HermesCarry():                              hermesCarryScript,
			paths.HermesOverlayStaged():                      staged,
			paths.HermesManagedEnv():                         []byte("EARLIER=1\n"),
			filepath.Join(paths.HermesHome(), "config.yaml"): []byte(`{"agent": {"disabled_toolsets": ["moa"]}}`),
		}
		for name, raw := range scope {
			files[filepath.Join(hermesSystemScope(bin), name)] = raw
		}
		for path, raw := range files {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(sh, "-c", hermesCarryRun(paths)+hermesInstallOverlay(paths, paths.HermesOverlayStaged()))
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"HERMES_HOME="+paths.HermesHome(), "HERMES_MANAGED_DIR="+paths.HermesManaged())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("the install failed: %v: %s", err, out)
		}
		var got overlay
		raw, err := os.ReadFile(paths.HermesOverlay())
		if err != nil || json.Unmarshal(raw, &got) != nil {
			t.Fatalf("the overlay reads %q (%v)", raw, err)
		}
		if info, err := os.Stat(paths.HermesOverlay()); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the overlay, which holds the bridge's token, is %v (%v), want mode 0600", info.Mode(), err)
		}
		if _, err := os.Stat(paths.HermesOverlayStaged()); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the staged overlay was left: %v", err)
		}
		env, err := os.ReadFile(paths.HermesManagedEnv())
		return got, env, err == nil
	}
	scope := map[string][]byte{"config.yaml": []byte(`{"agent": {"disabled_toolsets": ["homeassistant"]}, ` +
		`"mcp_servers": {"it": {"url": "http://it.example.com/mcp"}}, "model": {"default": "pinned-model"}}`),
		".env": []byte("IT_PINNED=1\n")}
	t.Run("a box's managed scope, web denied", func(t *testing.T) {
		got, env, _ := install(t, scope, true)
		if got.Servers["it"]["url"] != "http://it.example.com/mcp" || got.Servers["crewlet"]["url"] != "http://bridge.example.com/mcp" ||
			got.Model["default"] != "pinned-model" {
			t.Errorf("the overlay = %+v, want the box's server and model beside the run's server", got)
		}
		if want := []string{"homeassistant", "web", "browser", "x_search"}; !slices.Equal(got.Agent.Disabled, want) {
			t.Errorf("disabled toolsets = %v, want %v", got.Agent.Disabled, want)
		}
		if string(env) != "IT_PINNED=1\n" {
			t.Errorf("the managed .env = %q, want the box's", env)
		}
	})
	t.Run("a box's managed scope, web allowed", func(t *testing.T) {
		if got, _, _ := install(t, scope, false); !slices.Equal(got.Agent.Disabled, []string{"homeassistant", "browser"}) {
			t.Errorf("disabled toolsets = %v, want the box's and then Hermes's own browser", got.Agent.Disabled)
		}
	})
	t.Run("no managed scope, web denied", func(t *testing.T) {
		got, env, kept := install(t, nil, true)
		if want := []string{"moa", "web", "browser", "x_search"}; !slices.Equal(got.Agent.Disabled, want) {
			t.Errorf("disabled toolsets = %v, want %v", got.Agent.Disabled, want)
		}
		if kept {
			t.Errorf("an earlier run's managed .env was kept: %q", env)
		}
	})
}

// hermesFixtureRoute is the model section the steered fixture's session was
// created under, as the box's configuration holds it.
const hermesFixtureRoute = `{"model": {"provider": "openrouter", "base_url": "http://stub:9000/api/v1", "default": "stub-model"}}`

// hermesGate runs a continued run's session gate in a box at home, its Hermes
// having printed release, its configuration config, a carried archive of
// member, and returns sid, k and w.
func hermesGate(t *testing.T, home, release, config string, req sandbox.RunRequest, member []byte) []string {
	t.Helper()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	bin := hermesLauncherStandIn(t)
	paths := Paths{Home: home}
	for _, dir := range []string{paths.WorkDir(), paths.HermesHome()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(paths.HermesCarry(), hermesCarryScript, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.SessionCarried(), kimiArchive(t, map[string][]byte{hermesArchiveMember: member}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.HermesOverlayStaged(), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: hermesFixtureTitle, Continues: true, Fresh: "22222222-0000-4000-8000-000000000002"}
	if err := os.WriteFile(filepath.Join(paths.HermesHome(), "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.System(), []byte("THIS RUN'S OWN INSTRUCTIONS"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "n=" + shellword.Quote(release) + "\n" + hermesStopGateway(paths, time.Second) + hermesCarryRun(paths) +
		hermesSessionGate(req, session, paths, false) + `printf '%s|%s|%s' "$sid" "$k" "$w"`
	cmd := exec.Command(sh, "-c", script)
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "HERMES_HOME="+paths.HermesHome())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the gate failed: %v", err)
	}
	return strings.SplitN(string(out), "|", 3)
}

// A CARRIED HERMES SESSION IS RESTORED AND CONTINUED ONLY UNDER THE LAUNCH IT
// WAS CREATED WITH: restored into a store of its own alone (no other session's
// index line or kept launch stays) and indexed where the box runs the one
// release and the launch and model route match; another model, a working
// directory the box no longer has (0.23.3 then runs nothing), another route
// (provider, or a configured model; Hermes keeps the frozen one), no route or
// launch kept, or another release starts the fresh session, saying why.
//
// Mutation: skip the policy or route check or the route's model, read a
// missing launch or route as a mismatch, restore on another release, skip the
// index, or keep another session's index line, and a row goes red.
func TestACarriedHermesSessionContinuesOnlyUnderItsLaunch(t *testing.T) {
	t.Parallel()
	req := sandbox.RunRequest{Model: "stub-model"}
	member := func(home string) []byte {
		policy, _ := json.Marshal(map[string]string{"crewlet_policy": hermesPolicy(req) + home})
		return append(bytes.Clone(hermesFixture(t, "0.23.3", "session-steered.jsonl")), append(policy, '\n')...)
	}
	t.Run("matching", func(t *testing.T) {
		home := t.TempDir()
		other := "33333333-0000-4000-8000-000000000003"
		if err := os.MkdirAll(Paths{Home: home}.WorkDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		for path, body := range map[string]string{Paths{Home: home}.HermesSessions(): other + " local-other\n",
			Paths{Home: home}.HermesPolicy(other): "model x\n"} {
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got := hermesGate(t, home, hermesFloor(), hermesFixtureRoute, req, member(home))
		if got[1] != "yes" || got[2] != "" {
			t.Fatalf("gate = %q, want the session continued", got)
		}
		paths := Paths{Home: home}
		index, err := os.ReadFile(paths.HermesSessions())
		if err != nil || !strings.HasPrefix(string(index), hermesFixtureTitle+" local-") || strings.Contains(string(index), other) {
			t.Errorf("index = %q (%v), want the restored session's stored id alone", index, err)
		}
		if _, err := os.Stat(paths.HermesPolicy(other)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the replaced store's other session kept its launch: %v", err)
		}
		if _, err := os.Stat(paths.HermesStateDB()); err != nil {
			t.Errorf("no store was restored: %v", err)
		}
		if dump := hermesCarry(t, "dump", paths.HermesStateDB(), hermesFixtureTitle, "/dev/null"); !strings.Contains(dump,
			"THIS RUN'S OWN INSTRUCTIONS") {
			t.Error("the continued session was not handed the run's own standing instructions")
		}
	})
	t.Run("another model", func(t *testing.T) {
		home := t.TempDir()
		got := hermesGate(t, home, hermesFloor(), hermesFixtureRoute, sandbox.RunRequest{Model: "other-model"}, member(home))
		if got[0] != "22222222-0000-4000-8000-000000000002" || got[1] != "no" || !strings.Contains(got[2], "launch") {
			t.Fatalf("gate = %q, want the fresh session, saying the launch differs", got)
		}
	})
	t.Run("a working directory the box no longer has", func(t *testing.T) {
		home := t.TempDir()
		got := hermesGate(t, home, hermesFloor(), hermesFixtureRoute, req, member(filepath.Join(home, "gone")))
		if got[0] != "22222222-0000-4000-8000-000000000002" || got[1] != "no" || !strings.Contains(got[2], "working directory") {
			t.Fatalf("gate = %q, want the fresh session, saying the working directory differs", got)
		}
	})
	t.Run("another model route", func(t *testing.T) {
		home := t.TempDir()
		other := strings.Replace(hermesFixtureRoute, "openrouter", "custom", 1)
		got := hermesGate(t, home, hermesFloor(), other, req, member(home))
		if got[1] != "no" || !strings.Contains(got[2], "provider") {
			t.Fatalf("gate = %q, want the fresh session, saying the provider differs", got)
		}
	})
	t.Run("no model named, another configured", func(t *testing.T) {
		home := t.TempDir()
		none := sandbox.RunRequest{}
		policy, _ := json.Marshal(map[string]string{"crewlet_policy": hermesPolicy(none) + home})
		archive := append(bytes.Clone(hermesFixture(t, "0.23.3", "session-steered.jsonl")), append(policy, '\n')...)
		other := strings.Replace(hermesFixtureRoute, `"default": "stub-model"`, `"default": "other-model"`, 1)
		got := hermesGate(t, home, hermesFloor(), other, none, archive)
		if got[1] != "no" || !strings.Contains(got[2], "model route") {
			t.Fatalf("gate = %q, want the fresh session, saying the model route differs", got)
		}
	})
	t.Run("a model named in -m's own words", func(t *testing.T) {
		home := t.TempDir()
		named := sandbox.RunRequest{Model: "openrouter:stub-model"}
		policy, _ := json.Marshal(map[string]string{"crewlet_policy": hermesPolicy(named) + home})
		archive := append(bytes.Clone(hermesFixture(t, "0.23.3", "session-steered.jsonl")), append(policy, '\n')...)
		if got := hermesGate(t, home, hermesFloor(), hermesFixtureRoute, named, archive); got[1] != "yes" {
			t.Fatalf("gate = %q, want the session continued: 0.23.3 freezes openrouter:m as m", got)
		}
	})
	t.Run("a model named with another provider than the box's", func(t *testing.T) {
		home := t.TempDir()
		named := sandbox.RunRequest{Model: "anthropic:stub-model"}
		policy, _ := json.Marshal(map[string]string{"crewlet_policy": hermesPolicy(named) + home})
		fixture := hermesFixture(t, "0.23.3", "session-steered.jsonl")
		archive := append(bytes.Clone(fixture), append(policy, '\n')...)
		if got := hermesGate(t, home, hermesFloor(), hermesFixtureRoute, named, archive); got[1] != "no" ||
			!strings.Contains(got[2], "provider") {
			t.Fatalf("gate = %q under openrouter's frozen route, want the fresh session, saying the provider differs", got)
		}
		frozen := bytes.ReplaceAll(fixture, []byte(`\"gateway_runtime\": {\"provider\": \"openrouter\", \"base_url\": \"http://stub:9000/api/v1\"`),
			[]byte(`\"gateway_runtime\": {\"provider\": \"anthropic\", \"base_url\": \"http://stub:9000\"`))
		if bytes.Equal(frozen, fixture) {
			t.Fatal("the fixture's frozen route was not found to edit")
		}
		home = t.TempDir()
		policy, _ = json.Marshal(map[string]string{"crewlet_policy": hermesPolicy(named) + home})
		if got := hermesGate(t, home, hermesFloor(), hermesFixtureRoute, named, append(frozen, append(policy, '\n')...)); got[1] != "yes" {
			t.Fatalf("gate = %q, want the session continued: 0.23.3 resolves -m anthropic:m to anthropic as it froze it", got)
		}
	})
	t.Run("no model named or configured", func(t *testing.T) {
		home := t.TempDir()
		none := sandbox.RunRequest{}
		policy, _ := json.Marshal(map[string]string{"crewlet_policy": hermesPolicy(none) + home})
		fixture := hermesFixture(t, "0.23.3", "session-steered.jsonl")
		unnamed := bytes.ReplaceAll(fixture, []byte(`, \\\"default\\\": \\\"stub-model\\\"}`), []byte(`}`))
		if bytes.Equal(unnamed, fixture) {
			t.Fatal("the fixture's frozen configuration's default model was not found to edit")
		}
		box := strings.Replace(hermesFixtureRoute, `, "default": "stub-model"`, "", 1)
		if got := hermesGate(t, home, hermesFloor(), box, none, append(unnamed, append(policy, '\n')...)); got[1] != "yes" {
			t.Fatalf("gate = %q, want the session continued: both configurations read Hermes's default model", got)
		}
	})
	t.Run("no route frozen", func(t *testing.T) {
		home := t.TempDir()
		got := hermesGate(t, home, hermesFloor(), hermesFixtureRoute, req,
			bytes.ReplaceAll(member(home), []byte("gateway_runtime"), []byte("gateway_elsewhere")))
		if got[1] != "no" || !strings.Contains(got[2], "could not be read") {
			t.Fatalf("gate = %q, want the fresh session, saying the frozen route could not be read", got)
		}
	})
	t.Run("no launch kept", func(t *testing.T) {
		home := t.TempDir()
		got := hermesGate(t, home, hermesFloor(), hermesFixtureRoute, req, hermesFixture(t, "0.23.3", "session-steered.jsonl"))
		if got[1] != "no" || !strings.Contains(got[2], "no launch of it was kept") {
			t.Fatalf("gate = %q, want the fresh session, saying no launch was kept", got)
		}
	})
	t.Run("another release", func(t *testing.T) {
		home := t.TempDir()
		got := hermesGate(t, home, "0.23.4", hermesFixtureRoute, req, member(home))
		if got[1] != "no" || !strings.Contains(got[2], "0.23.4") {
			t.Fatalf("gate = %q, want no continuation on 0.23.4", got)
		}
	})
}

// hermesCarry runs the carry script's command under the machine's python3 and
// returns, for a dump, what it wrote; it fails the case where the script did
// not answer ok.
func hermesCarry(t *testing.T, command string, args ...string) string {
	t.Helper()
	python := prereqtest.OnPath(t, "python3", "the case runs the run's carry script", "install python3")
	dir := t.TempDir()
	carry := filepath.Join(dir, "carry.py")
	if err := os.WriteFile(carry, hermesCarryScript, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if command == "dump" {
		args = append(args, out)
	}
	answer, err := exec.Command(python, append([]string{"-I", carry, command}, args...)...).Output()
	if err != nil || !strings.HasPrefix(string(answer), "crewlet-carry ok") {
		t.Fatalf("%s: %v: %s", command, err, answer)
	}
	if command != "dump" {
		return string(answer)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A HERMES SESSION DUMPS AND RESTORES WHOLE, AND ALONE: the carry script's
// dump of a restored store is the dump it was restored from, row for row, the
// restored store's full-text indexes find its words, a restore names the
// restored session's stored id, a title the store does not hold dumps nothing,
// and another session's launch and admission results stay out of a dump.
//
// Mutation: drop a table from the dump, the triggers' second pass from the
// restore, or the dump's filter on state_meta, and this goes red.
func TestAHermesSessionDumpsAndRestoresWhole(t *testing.T) {
	t.Parallel()
	python := prereqtest.OnPath(t, "python3", "the case runs the run's carry script", "install python3")
	dir := t.TempDir()
	member := filepath.Join(dir, "session.jsonl")
	db := filepath.Join(dir, "state.db")
	if err := os.WriteFile(member, hermesFixture(t, "0.23.3", "session-steered.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	if answer := hermesCarry(t, "restore", member, db, filepath.Join(dir, "policy"), hermesFixtureTitle); !strings.HasPrefix(answer,
		"crewlet-carry ok local-") {
		t.Fatalf("restore answered %q, want the stored id", answer)
	}
	rows := func(dump string) []string {
		var out []string
		for _, l := range strings.Split(dump, "\n") {
			if strings.HasPrefix(l, `{"t": `) {
				out = append(out, l)
			}
		}
		return out
	}
	again := hermesCarry(t, "dump", db, hermesFixtureTitle, "/dev/null")
	if want, got := rows(string(hermesFixture(t, "0.23.3", "session-steered.jsonl"))), rows(again); !slices.Equal(got, want) {
		t.Errorf("the restored store dumps %d rows, the archive held %d", len(got), len(want))
	}
	carry := filepath.Join(dir, "carry.py")
	if err := os.WriteFile(carry, hermesCarryScript, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command(python, "-I", carry, "dump", db, "no-such-title", "/dev/null", filepath.Join(dir, "x")).Output(); string(out) != "crewlet-carry none\n" {
		t.Errorf("dumping an unheld title answered %q, want none", out)
	}
	// The restore's triggers fill the full-text indexes again, which the dump never carries.
	probe := `import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
print(db.execute("select count(*) from messages_fts where messages_fts match 'steer'").fetchone()[0],
      db.execute("select count(*) from messages_fts_trigram where messages_fts_trigram match 'tee'").fetchone()[0])
db.execute("insert into state_meta (key, value) values ('gateway.local_policy.v1:local-other', 'OTHER-POLICY'), "
           "('gateway.admission.result.v1.other', 'OTHER-ADMISSION')")
db.commit()`
	if out, err := exec.Command(python, "-I", "-c", probe, db).CombinedOutput(); err != nil || strings.HasPrefix(string(out), "0") ||
		strings.Contains(string(out), " 0") {
		t.Errorf("the restored store's full-text indexes find %q (%v), want the session's words", out, err)
	}
	if dump := hermesCarry(t, "dump", db, hermesFixtureTitle, "/dev/null"); strings.Contains(dump, "OTHER-") {
		t.Error("the dump carried another session's launch or admission")
	}
}

// A CONTINUED HERMES SESSION IS HANDED THE RUN'S OWN STANDING INSTRUCTIONS:
// Hermes freezes them with the session (its local policy's client
// environment) and a resume keeps them (measured on 0.23.3), so the carry
// script writes the run's there as the launch reads them ($(cat)), the launch
// written back compact as Hermes writes it, removes them for a run with none,
// and answers none for a store holding no frozen launch of the session.
//
// Mutation: skip the rewrite, keep the old text for a run with none, keep the
// file's trailing newlines, write the launch back spaced, or answer ok having
// renewed nothing, and this goes red.
func TestAContinuedHermesSessionIsHandedTheRunsOwnInstructions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	member := filepath.Join(dir, "session.jsonl")
	db := filepath.Join(dir, "state.db")
	system := filepath.Join(dir, "system.md")
	for path, body := range map[string][]byte{member: hermesFixture(t, "0.23.3", "session-steered.jsonl"),
		system: []byte("RENEWED STANDING TEXT\n\n")} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hermesCarry(t, "restore", member, db, filepath.Join(dir, "policy"), hermesFixtureTitle)
	hermesCarry(t, "renew", db, hermesFixtureTitle, system)
	dump := hermesCarry(t, "dump", db, hermesFixtureTitle, "/dev/null")
	if !strings.Contains(dump, `RENEWED STANDING TEXT\\\"`) {
		t.Fatal("the session's frozen environment does not carry the run's own instructions, as the launch reads them")
	}
	for line := range strings.Lines(dump) {
		var rec struct {
			T string `json:"t"`
			R struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"r"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || !strings.HasPrefix(rec.R.Key, "gateway.local_policy.v1:") {
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(rec.R.Value)); err != nil || compact.String() != rec.R.Value {
			t.Errorf("the renewed launch is not written compact, as Hermes writes it: %.120s (%v)", rec.R.Value, err)
		}
	}
	python := prereqtest.OnPath(t, "python3", "the case runs the run's carry script", "install python3")
	carry := filepath.Join(dir, "carry.py")
	if err := os.WriteFile(carry, hermesCarryScript, 0o600); err != nil {
		t.Fatal(err)
	}
	unheld := filepath.Join(dir, "unheld.db")
	if err := os.WriteFile(member, []byte(strings.ReplaceAll(string(hermesFixture(t, "0.23.3", "session-steered.jsonl")),
		"gateway.local_policy.v1:", "gateway.local_policy.v0:")), 0o600); err != nil {
		t.Fatal(err)
	}
	hermesCarry(t, "restore", member, unheld, filepath.Join(dir, "policy2"), hermesFixtureTitle)
	if out, _ := exec.Command(python, "-I", carry, "renew", unheld, hermesFixtureTitle, system).Output(); string(out) != "crewlet-carry none\n" {
		t.Errorf("renewing a session with no launch frozen answered %q, want none", out)
	}
	hermesCarry(t, "renew", db, hermesFixtureTitle, filepath.Join(dir, "none.md"))
	if dump := hermesCarry(t, "dump", db, hermesFixtureTitle, "/dev/null"); strings.Contains(dump, "RENEWED STANDING TEXT") {
		t.Fatal("a run with no standing instructions kept the earlier run's")
	}
}

// hermesNoteRows is the steered fixture's record with edit applied to each
// messages row, keyed by its id.
func hermesNoteRows(t *testing.T, edit func(id int64, row map[string]any)) []byte {
	t.Helper()
	return hermesFixtureRows(t, "session-steered.jsonl", edit)
}

// hermesFixtureRows is the named 0.23.3 fixture's record with edit applied to
// each messages row, keyed by its id.
func hermesFixtureRows(t *testing.T, fixture string, edit func(id int64, row map[string]any)) []byte {
	t.Helper()
	var out []byte
	for line := range bytes.Lines(hermesFixture(t, "0.23.3", fixture)) {
		var rec struct {
			T string         `json:"t"`
			R map[string]any `json:"r"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.T != "messages" {
			out = append(out, line...)
			continue
		}
		edit(int64(rec.R["id"].(float64)), rec.R)
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		out = append(append(out, raw...), '\n')
	}
	return out
}

// A HERMES NOTE IS CONFIRMED ONLY BY THIS LAUNCH'S OWN ROWS AND A REPLY AFTER
// IT: the fixture's note (row 4, a steer, framed) is held where the launch
// began before it and the model's reply (row 5) is stored after it; not where
// the row is an earlier launch's, inactive, shown as no steer, or stored after
// the last reply, folded by a compaction, or answered by no finished reply
// (a summary, a reply cut off); two takes of one note's words need two notes;
// a row of notes joined by newlines holds each.
//
// Mutations, each red: match from 0 or past the start; take an inactive or
// compacted row; drop the display_kind check; settle without a later reply, or
// on a summary or an unfinished one; match a digest set; match a row whole.
func TestAHermesNoteIsConfirmedOnlyByThisLaunchsReply(t *testing.T) {
	t.Parallel()
	note := ItemDigest(hermesSteeredNote)
	one := []ServedTake{{InboxID: "a", Digest: note}}
	keep := func(int64, map[string]any) {}
	for _, c := range []struct {
		name  string
		edit  func(int64, map[string]any)
		from  int64
		takes []ServedTake
		want  []string
	}{
		{"this launch's note and reply", keep, 0, one, []string{"a"}},
		{"a launch begun just before the note", keep, 3, one, []string{"a"}},
		{"a launch begun at the note", keep, 4, one, nil},
		{"an earlier launch's identical words", keep, 5, one, nil},
		{"a reply that is a compaction's summary", func(id int64, r map[string]any) {
			if id == 5 {
				r["_compressed_summary"] = 1
			}
		}, 0, one, nil},
		{"a reply the model never finished", func(id int64, r map[string]any) {
			if id == 5 {
				r["finish_reason"] = nil
			}
		}, 0, one, nil},
		{"a note a compaction folded away", func(id int64, r map[string]any) {
			if id == 4 {
				r["compacted"] = 1
			}
		}, 0, one, nil},
		{"an inactive row", func(id int64, r map[string]any) {
			if id == 4 {
				r["active"] = 0
			}
		}, 0, one, nil},
		{"a user row shown as no steer", func(id int64, r map[string]any) {
			if id == 4 {
				r["display_kind"] = nil
			}
		}, 0, one, nil},
		{"a note stored after the last reply", func(id int64, r map[string]any) {
			if id == 4 {
				r["id"] = 6
			}
		}, 0, one, nil},
		{"two takes of one note's words", keep, 0, []ServedTake{{InboxID: "a", Digest: note}, {InboxID: "b", Digest: note}},
			[]string{"a"}},
		{"notes steered before one request, one row", func(id int64, r map[string]any) {
			if id == 4 {
				r["content"] = hermesNoteOpen + "FIRST\nSECOND\nOF TWO LINES\nTHIRD" + hermesNoteClose
			}
		}, 0, []ServedTake{{InboxID: "a", Digest: ItemDigest("FIRST")}, {InboxID: "b", Digest: ItemDigest("SECOND\nOF TWO LINES")},
			{InboxID: "c", Digest: ItemDigest("THIRD")}, {InboxID: "d", Digest: ItemDigest("NEVER")}}, []string{"a", "b", "c"}},
	} {
		got, err := hermesNotesHeld(bytes.NewReader(hermesNoteRows(t, c.edit)), c.takes, c.from)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: held %v (%v), want %v", c.name, got, err, c.want)
		}
	}
}

// A HERMES LAUNCH'S START IS ITS OWN SESSION'S: a record naming another
// session, or none, decides nothing, so no earlier launch's rows confirm.
//
// Mutation: read the count whatever session the record names, or read a
// missing record as 0, and a case goes red.
func TestAHermesLaunchsStartNamesItsSession(t *testing.T) {
	t.Parallel()
	box := sandbox.NewFakeSandbox("b")
	if _, err := hermesRunFrom(t.Context(), box, hermesFixtureTitle); err == nil {
		t.Error("a box with no record of the launch's start read as one")
	}
	box.Put(PathsFor(box).HermesRunStart(), "22222222-0000-4000-8000-000000000002 7\n")
	if _, err := hermesRunFrom(t.Context(), box, hermesFixtureTitle); err == nil {
		t.Error("another session's start was read as this one's")
	}
	box.Put(PathsFor(box).HermesRunStart(), hermesFixtureTitle+" 7\n")
	if n, err := hermesRunFrom(t.Context(), box, hermesFixtureTitle); err != nil || n != 7 {
		t.Errorf("the launch's start reads %d (%v), want 7", n, err)
	}
}

// A HERMES SESSION CUT OFF IN ITS FIRST LAUNCH STILL CONTINUES: the gate keeps
// the launch as it starts, not once the client exits, so a session whose
// first launch was killed (no index line, no clean exit) is packed with its
// launch and continued under it by the next run.
//
// Mutation: keep the launch only after a clean exit (hermesIndex), and the
// continuation starts fresh, saying no launch was kept.
func TestAHermesSessionCutOffInItsFirstLaunchStillContinues(t *testing.T) {
	t.Parallel()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	home := t.TempDir()
	paths := Paths{Home: home}
	bin := hermesLauncherStandIn(t)
	for _, dir := range []string{paths.WorkDir(), paths.HermesHome()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string][]byte{paths.HermesCarry(): hermesCarryScript, paths.HermesOverlayStaged(): []byte("{}")} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	req := sandbox.RunRequest{Model: "stub-model"}
	first := hermesCarryRun(paths) + hermesSessionGate(req, Session{ID: hermesFixtureTitle}, paths, false)
	cmd := exec.Command(sh, "-c", first)
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the first launch's gate failed: %v: %s", err, out)
	}
	kept, err := os.ReadFile(paths.HermesPolicy(hermesFixtureTitle))
	if err != nil || string(kept) != hermesPolicy(req)+home+"\n" {
		t.Fatalf("the first launch kept %q (%v), want its launch before the client starts", kept, err)
	}
	// The killed launch's session, in the store with no index line, packed as the run's end packs it.
	dir := t.TempDir()
	member := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(member, hermesFixture(t, "0.23.3", "session-steered.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "state.db")
	hermesCarry(t, "restore", member, store, filepath.Join(dir, "unused"), hermesFixtureTitle)
	packed := hermesCarry(t, "dump", store, hermesFixtureTitle, paths.HermesPolicy(hermesFixtureTitle))
	got := hermesGate(t, home, hermesFloor(), hermesFixtureRoute, req, []byte(packed))
	if got[1] != "yes" || got[2] != "" {
		t.Fatalf("gate = %q, want the cut-off session continued under its kept launch", got)
	}
}

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A HERMES RUN: a box whose
// working directory holds its own AGENTS.md and a checkout at ./repo with
// every layer a repository brings. The runner hands none of it: 0.23.3's own
// subdirectory hints add a checkout's instructions to the tool result that
// first touches a directory, and it reads a checkout's .hermes.md, .cursor
// rules, skills, plugins and configuration not at all (measured;
// code-sandbox.md "Hermes Agent"). So no file the run writes, nor its script,
// carries any of them.
//
// Mutation: hand the checkout's instructions, skills, plugins or settings to
// the run in a file it writes or its script, and its kind goes red.
func TestWhatACheckoutBelowTheStartDirectoryBringsAHermesRun(t *testing.T) {
	t.Parallel()
	fake := sandbox.NewFakeSandbox("b")
	for rel, body := range map[string]string{
		"workspace/AGENTS.md":                           "WORKDIR-INSTRUCTIONS\n",
		"workspace/repo/.git/HEAD":                      "ref: refs/heads/main\n",
		"workspace/repo/AGENTS.md":                      "REPO-INSTRUCTIONS\n",
		"workspace/repo/.hermes.md":                     "REPO-HERMES-MD\n",
		"workspace/repo/.cursor/rules/r.mdc":            "REPO-CURSOR-RULE\n",
		"workspace/repo/.hermes/skills/check/SKILL.md":  "---\nname: check\ndescription: REPO-SKILL\n---\nCheck.\n",
		"workspace/repo/.hermes/plugins/rp/plugin.yaml": "name: rp\ndescription: REPO-PLUGIN\n",
		"workspace/repo/.hermes/config.yaml":            "agent:\n  personality: REPO-CONFIG\n",
		"workspace/repo/.hermes/plugins/rp/__init__.py": "REPO-PLUGIN-CODE\n",
	} {
		if err := fake.WriteFile(t.Context(), fake.Home()+"/"+rel, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	box := &writeLog{FakeSandbox: fake}
	p := PathsFor(box)
	req := sandbox.RunRequest{System: "be brief", Skills: []sandbox.Skill{{Name: "deploy", Description: "d", Body: "b"}},
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://bridge.example.com/mcp",
			AlwaysLoaded: true}}}
	if _, err := (Hermes{}).WriteConfig(t.Context(), box, req, Session{ID: "s"}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	written := []string{hermesScript(req, Session{ID: "s"}, p, hermesRunTimes())}
	for _, path := range box.paths {
		raw, err := box.ReadFile(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		written = append(written, path+": "+string(raw))
	}
	for _, where := range written {
		for kind, mark := range map[string]string{"instructions": "REPO-INSTRUCTIONS", ".hermes.md": "REPO-HERMES-MD",
			"cursor rules": "REPO-CURSOR-RULE", "skills": "REPO-SKILL", "plugins": "REPO-PLUGIN", "settings": "REPO-CONFIG",
			"the working directory's own instructions": "WORKDIR-INSTRUCTIONS"} {
			if strings.Contains(where, mark) {
				t.Errorf("%s: the checkout reached the run's own files: %.300s", kind, where)
			}
		}
	}
}

// hermesRecordOf is a dump of messages rows alone, each {"id", "role",
// "content", ...} as given, every row active and in the conversation unless it
// says otherwise.
func hermesRecordOf(t *testing.T, rows ...map[string]any) []byte {
	t.Helper()
	var out []byte
	for _, r := range rows {
		row := map[string]any{"active": 1, "compacted": 0, "_compressed_summary": 0}
		maps.Copy(row, r)
		raw, err := json.Marshal(map[string]any{"t": "messages", "r": row})
		if err != nil {
			t.Fatal(err)
		}
		out = append(append(out, raw...), '\n')
	}
	return out
}

// A HERMES NOTE THE RECORD LACKS HOLDS NO LATER ONE BACK: takes A, B and C,
// B never reaching the record (its steer's outcome unknown, or Hermes rejected
// it), A and C each answered: A and C are held and B is not, since the record
// is matched as an in-order subsequence of the serves.
//
// Mutation: match each row against the next take alone, and C is never held.
func TestAHermesNoteTheRecordLacksHoldsNoLaterOneBack(t *testing.T) {
	t.Parallel()
	note := func(id int, text string) map[string]any {
		return map[string]any{"id": id, "role": "user", "display_kind": "steer", "content": hermesNoteOpen + text + hermesNoteClose}
	}
	reply := func(id int) map[string]any {
		return map[string]any{"id": id, "role": "assistant", "finish_reason": "stop", "content": "ok"}
	}
	takes := []ServedTake{{InboxID: "a", Digest: ItemDigest("A")}, {InboxID: "b", Digest: ItemDigest("B")},
		{InboxID: "c", Digest: ItemDigest("C")}}
	got, err := hermesNotesHeld(bytes.NewReader(hermesRecordOf(t, note(1, "A"), reply(2), note(3, "C"), reply(4))), takes, 0)
	if err != nil || !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("held %v (%v), want a and c with b lost", got, err)
	}
}

// A HERMES ROW IS READ WHOLE BEFORE ITS NOTES ARE MATCHED: a note of several
// lines whose first line is another take's words, steered alone, holds that
// note, whichever of the two was served first; the row's parts are the takes
// whose in-order parse consumes it all, the earliest takes among equals.
//
// Mutation: cut the shortest leading part matching any take, and "fix" is held
// with the note's tail unread.
func TestAHermesRowIsReadWholeBeforeItsNotesAreMatched(t *testing.T) {
	t.Parallel()
	note := func(id int, text string) map[string]any {
		return map[string]any{"id": id, "role": "user", "display_kind": "steer", "content": hermesNoteOpen + text + hermesNoteClose}
	}
	reply := func(id int) map[string]any {
		return map[string]any{"id": id, "role": "assistant", "finish_reason": "stop", "content": "ok"}
	}
	x := ServedTake{InboxID: "x", Digest: ItemDigest("fix\nthen push")}
	y := ServedTake{InboxID: "y", Digest: ItemDigest("fix")}
	for _, c := range []struct {
		name  string
		takes []ServedTake
		row   string
		want  []string
	}{
		{"x served before y, x steered", []ServedTake{x, y}, "fix\nthen push", []string{"x"}},
		{"y served before x, y lost", []ServedTake{y, x}, "fix\nthen push", []string{"x"}},
		{"both steered in one request", []ServedTake{x, y}, "fix\nthen push\nfix", []string{"x", "y"}},
	} {
		got, err := hermesNotesHeld(bytes.NewReader(hermesRecordOf(t, note(1, c.row), reply(2))), c.takes, 0)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: held %v (%v), want %v", c.name, got, err, c.want)
		}
	}
}

// A ROTATED HERMES SESSION CARRIES WITH THE SESSION IT CONTINUES: where the
// box's compression.in_place is false, 0.23.3 ends the run's session
// "compression", moves its title to a child naming it as parent, and copies
// the kept tail there (measured). The dump of the title holds the parent and
// then the child, so the restore's stored id is the child's, and the earlier
// conversation is carried too.
//
// Mutation: start the lineage at the titled session alone, and the parent is
// dropped.
func TestARotatedHermesSessionCarriesWithTheSessionItContinues(t *testing.T) {
	t.Parallel()
	python := prereqtest.OnPath(t, "python3", "the case runs the run's carry script", "install python3")
	dir := t.TempDir()
	member, db := filepath.Join(dir, "session.jsonl"), filepath.Join(dir, "state.db")
	if err := os.WriteFile(member, hermesFixture(t, "0.23.3", "session-steered.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := strings.TrimSpace(strings.TrimPrefix(hermesCarry(t, "restore", member, db, filepath.Join(dir, "policy"),
		hermesFixtureTitle), "crewlet-carry ok "))
	rotate := `import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("update sessions set title = null, end_reason = 'compression' where id = ?", (sys.argv[2],))
db.execute("insert into sessions (id, title, parent_session_id, source, model_config, started_at) select 'child', ?, id, "
           "source, model_config, started_at + 1 from sessions where id = ?", (sys.argv[3], sys.argv[2]))
db.commit()`
	if out, err := exec.Command(python, "-I", "-c", rotate, db, parent, hermesFixtureTitle).CombinedOutput(); err != nil {
		t.Fatalf("rotating the session: %v: %s", err, out)
	}
	var sessions []string
	messages := 0
	for line := range strings.Lines(hermesCarry(t, "dump", db, hermesFixtureTitle, "/dev/null")) {
		var rec struct {
			T string         `json:"t"`
			R map[string]any `json:"r"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch id, _ := rec.R["id"].(string); {
		case rec.T == "sessions":
			sessions = append(sessions, id)
		case rec.T == "messages" && rec.R["session_id"] == parent:
			messages++
		}
	}
	if !slices.Equal(sessions, []string{parent, "child"}) || messages == 0 {
		t.Errorf("the rotated session dumps sessions %v and %d of the parent's messages, want %s then child with them",
			sessions, messages, parent)
	}
}

// A HERMES NOTE A ROTATION COPIES IS READ ONCE: a rotated child holds a copy
// of the kept tail under new ids with each row's message_uid (measured on
// 0.23.3), while the parent's rows stay current. A note of this launch
// steered before the rotation holds its own take alone, never a later
// identical one, whose steer the record lacks.
//
// Mutation: skip only copies of an earlier launch's rows, and the copy holds
// the second take.
func TestAHermesNoteARotationCopiesIsReadOnce(t *testing.T) {
	t.Parallel()
	note := func(id int, uid string) map[string]any {
		return map[string]any{"id": id, "message_uid": uid, "role": "user", "display_kind": "steer",
			"content": hermesNoteOpen + "W" + hermesNoteClose}
	}
	reply := func(id int, uid string) map[string]any {
		return map[string]any{"id": id, "message_uid": uid, "role": "assistant", "finish_reason": "stop", "content": "ok"}
	}
	summary := map[string]any{"id": 3, "message_uid": "s", "role": "user", "_compressed_summary": 1, "content": "summary"}
	takes := []ServedTake{{InboxID: "a", Digest: ItemDigest("W")}, {InboxID: "b", Digest: ItemDigest("W")}}
	record := hermesRecordOf(t, note(1, "n"), reply(2, "r"), summary, note(4, "n"), reply(5, "r"))
	if got, err := hermesNotesHeld(bytes.NewReader(record), takes, 0); err != nil || !slices.Equal(got, []string{"a"}) {
		t.Errorf("held %v (%v), want a alone: the child's row is a copy", got, err)
	}
}

// A HERMES NOTE SURVIVES AN IN-PLACE COMPACTION ONLY AS A ROW OF THE
// CONVERSATION: in 0.23.3's in-place compaction (measured,
// session-compacted.jsonl) the folded rows go inactive and compacted, the
// summary is a user row of its own, and the kept tail is written again under
// new ids, each keeping its message_uid. A note of this launch (row 7, from 6)
// is held through its copy (row 14); a copy of an earlier launch's (from 9) is
// not; nor are a note only the summary holds (row 10) or one folded (row 1).
//
// Mutation: take the summary or a folded row as a note, or a copy for this
// launch's row, and its case goes red.
func TestAHermesNoteSurvivesAnInPlaceCompactionOnlyAsAConversationRow(t *testing.T) {
	t.Parallel()
	take := []ServedTake{{InboxID: "a", Digest: ItemDigest(hermesSteeredNote)}}
	for _, c := range []struct {
		rows []int64
		from int64
		want []string
	}{
		{[]int64{7, 14}, 6, []string{"a"}}, {[]int64{7, 14}, 9, nil}, {[]int64{10}, 9, nil}, {[]int64{1}, 0, nil},
	} {
		record := hermesFixtureRows(t, "session-compacted.jsonl", func(id int64, r map[string]any) {
			if slices.Contains(c.rows, id) {
				r["display_kind"] = "steer"
				r["content"] = hermesNoteOpen + hermesSteeredNote + hermesNoteClose
			}
		})
		if got, err := hermesNotesHeld(bytes.NewReader(record), take, c.from); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("the note as rows %v from %d: held %v (%v), want %v", c.rows, c.from, got, err, c.want)
		}
	}
}

// A STEERED HERMES NOTE RIDES THE REQUEST AFTER THE TOOL RESULT: the stand-in
// model's capture of 0.23.3's second request (request-steered.json) ends with
// the tool's result and then the note, in Hermes's frame, so the reply to that
// request is the model's answer to the note.
//
// Mutation: a capture with the note elsewhere, or none, goes red.
func TestASteeredHermesNoteRidesTheRequestAfterTheToolResult(t *testing.T) {
	t.Parallel()
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(hermesFixture(t, "0.23.3", "request-steered.json"), &body); err != nil {
		t.Fatal(err)
	}
	n := len(body.Messages)
	if n < 2 || body.Messages[n-2].Role != "tool" || body.Messages[n-1].Role != "user" ||
		body.Messages[n-1].Content != hermesNoteOpen+hermesSteeredNote+hermesNoteClose {
		t.Fatalf("the request ends %+v, want the tool's result and then the framed note", body.Messages)
	}
}

// THE STORE'S NEWEST MESSAGE IS WHERE A LAUNCH'S ROWS BEGIN, recorded before
// the client starts: the carry script's top answers a store's newest message
// id, 0 for no store, and the script records it after the session gate and
// before the launch.
//
// Mutation: answer another id, record it after the launch, or not at all.
func TestTheStoresNewestMessageIsWhereALaunchsRowsBegin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	member := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(member, hermesFixture(t, "0.23.3", "session-steered.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "state.db")
	hermesCarry(t, "restore", member, db, filepath.Join(dir, "policy"), hermesFixtureTitle)
	if got := hermesCarry(t, "top", db); got != "crewlet-carry ok 5\n" {
		t.Errorf("top of the steered store = %q, want its newest message, 5", got)
	}
	if got := hermesCarry(t, "top", filepath.Join(dir, "none.db")); got != "crewlet-carry ok 0\n" {
		t.Errorf("top of no store = %q, want 0", got)
	}
	script := hermesScript(sandbox.RunRequest{}, Session{ID: "s"}, PathsFor(sandbox.NewFakeSandbox("b")), hermesRunTimes())
	top, gate, launch := strings.Index(script, "hcarry top"), strings.Index(script, " k=no w="), strings.Index(script, "hermes chat")
	if top < 0 || gate < 0 || launch < 0 || top < gate || top > launch {
		t.Errorf("the launch's start is recorded at %d, the gate at %d and the launch at %d", top, gate, launch)
	}
}

// hermesPolicyValue is the frozen launch (state_meta's local policy value) a
// record holds, as stored.
func hermesPolicyValue(t *testing.T, record []byte) string {
	t.Helper()
	for line := range bytes.Lines(record) {
		var rec struct {
			T string `json:"t"`
			R struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"r"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.T == "state_meta" && strings.HasPrefix(rec.R.Key, "gateway.local_policy.v1:") {
			return rec.R.Value
		}
	}
	t.Fatal("the record holds no frozen launch")
	return ""
}

// A RENEWED HERMES LAUNCH IS WRITTEN AS HERMES WRITES IT (measured on 0.23.3):
// the whole compact with its keys sorted and non-ASCII raw, the client
// environment spaced, sorted, non-ASCII escaped. Renewed with the text it was
// frozen with, the steered fixture's launch comes back byte for byte, also from
// one whose keys run backwards and whose environment lacks the instructions;
// renewed with non-ASCII text in a launch whose working directory is
// non-ASCII, the directory stays raw and the text is escaped.
//
// Mutation: escape the whole's non-ASCII, space it, or leave either's keys
// unsorted.
func TestARenewedHermesLaunchIsWrittenAsHermesWritesIt(t *testing.T) {
	t.Parallel()
	renewed := func(t *testing.T, record []byte, text string) string {
		dir := t.TempDir()
		member, db, system := filepath.Join(dir, "session.jsonl"), filepath.Join(dir, "state.db"), filepath.Join(dir, "system.md")
		for path, body := range map[string][]byte{member: record, system: []byte(text)} {
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		hermesCarry(t, "restore", member, db, filepath.Join(dir, "policy"), hermesFixtureTitle)
		hermesCarry(t, "renew", db, hermesFixtureTitle, system)
		return hermesPolicyValue(t, []byte(hermesCarry(t, "dump", db, hermesFixtureTitle, "/dev/null")))
	}
	fixture := hermesFixture(t, "0.23.3", "session-steered.jsonl")
	frozen := hermesPolicyValue(t, fixture)
	if got := renewed(t, fixture, "SEAT-TEXT"); got != frozen {
		t.Errorf("renewed with its own text, the launch reads\n%.300s\nwant it as frozen\n%.300s", got, frozen)
	}
	var policy map[string]any
	if err := json.Unmarshal([]byte(frozen), &policy); err != nil {
		t.Fatal(err)
	}
	policy["policy"].(map[string]any)["cwd"] = "/work/naïve"
	literal := func(v any) []byte {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
		return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	}
	edited := bytes.Replace(fixture, literal(frozen), literal(string(literal(policy))), 1)
	if bytes.Equal(edited, fixture) {
		t.Fatal("the fixture's frozen launch was not found to edit")
	}
	var backwards func(v any) string
	backwards = func(v any) string {
		m, ok := v.(map[string]any)
		if !ok {
			return string(literal(v))
		}
		keys := slices.Sorted(maps.Keys(m))
		slices.Reverse(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, string(literal(k))+":"+backwards(m[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	dec := json.NewDecoder(strings.NewReader(frozen))
	dec.UseNumber()
	var unsorted map[string]any
	if err := dec.Decode(&unsorted); err != nil {
		t.Fatal(err)
	}
	launch := unsorted["policy"].(map[string]any)
	var env map[string]any
	if err := json.Unmarshal([]byte(launch["client_env_json"].(string)), &env); err != nil {
		t.Fatal(err)
	}
	if _, ok := env["HERMES_EPHEMERAL_SYSTEM_PROMPT"]; !ok || len(env) < 2 {
		t.Fatalf("the fixture's client environment %v holds no instructions beside another key to reorder", env)
	}
	delete(env, "HERMES_EPHEMERAL_SYSTEM_PROMPT")
	launch["client_env_json"] = backwards(env)
	reordered := bytes.Replace(fixture, literal(frozen), literal(backwards(unsorted)), 1)
	if got := renewed(t, reordered, "SEAT-TEXT"); got != frozen {
		t.Errorf("renewed from backwards keys and no instructions, the launch reads\n%.300s\nwant it as frozen\n%.300s",
			got, frozen)
	}
	got := renewed(t, edited, "naïve — text")
	if !strings.Contains(got, `"cwd":"/work/naïve"`) || !strings.Contains(got, `na\\u00efve \\u2014 text`) {
		t.Errorf("the renewed launch reads %.400s; want its directory raw and its text escaped in the environment", got)
	}
}

// hermesReceiptsOf is the receipts Hermes's record confirm answers ask with,
// on a box whose session store dumps as record and whose launch began at 0.
func hermesReceiptsOf(t *testing.T, record []byte, ask sandbox.MainThreadAsk) sandbox.TakeReceipts {
	t.Helper()
	box := sandbox.NewFakeSandbox("b")
	box.Put(PathsFor(box).HermesRunStart(), hermesFixtureTitle+" 0\n")
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		box.Put(PathsFor(box).HermesRecordRead(), string(record))
		return sandbox.ExecResult{Stdout: "crewlet-carry ok\n"}, nil
	}
	ask.Session = hermesFixtureTitle
	return recordReceipts(t.Context(), Hermes{}, box, ask)
}

// A READ HERMES NOTE SPENDS ITS OWN ROW, so it stands in for no later serve:
// the receipts ask carries the launch's serves in order, read notes' included
// ([sandbox.MainThreadAsk.Served]). (a) A read note A and a later identical B
// whose steer was lost: A's row and reply hold A, never B. (b) A row holding
// read N1 and unread N2 holds N2. (c) A read note's row, its words also a
// later serve's, is spent on it, so the serve between them is still held.
//
// Mutation: match the asked takes alone (recordReceipts ignoring Served), and
// every case goes red.
func TestAReadHermesNoteSpendsItsOwnRow(t *testing.T) {
	t.Parallel()
	note := func(id int, text string) map[string]any {
		return map[string]any{"id": id, "role": "user", "display_kind": "steer", "content": hermesNoteOpen + text + hermesNoteClose}
	}
	reply := func(id int) map[string]any {
		return map[string]any{"id": id, "role": "assistant", "finish_reason": "stop", "content": "ok"}
	}
	serve := func(take, words string, read bool) sandbox.AskedTake {
		return sandbox.AskedTake{Take: take, Digest: ItemDigest(words), Read: read}
	}
	for _, c := range []struct {
		name   string
		record []byte
		served []sandbox.AskedTake
		want   []string
	}{
		{"(a) an identical note whose steer was lost", hermesRecordOf(t, note(1, "W"), reply(2)),
			[]sandbox.AskedTake{serve("a", "W", true), serve("b", "W", false)}, nil},
		{"(b) a read note and an unread one in one row", hermesRecordOf(t, note(1, "N1\nN2"), reply(2)),
			[]sandbox.AskedTake{serve("n1", "N1", true), serve("n2", "N2", false)}, []string{"n2"}},
		{"(c) a read note's words a later serve's", hermesRecordOf(t, note(1, "W"), reply(2), note(3, "Y"), reply(4)),
			[]sandbox.AskedTake{serve("r", "W", true), serve("y", "Y", false), serve("z", "W", false)}, []string{"y"}},
	} {
		var asked []sandbox.AskedTake
		for _, s := range c.served {
			if !s.Read {
				asked = append(asked, s)
			}
		}
		got := hermesReceiptsOf(t, c.record, sandbox.MainThreadAsk{Takes: asked, Served: c.served})
		if !slices.Equal(got.Held, c.want) {
			t.Errorf("%s: held %v (%s), want %v", c.name, got.Held, got.Why, c.want)
		}
	}
}

// A FAILED HERMES TURN SETTLES NO NOTE: 0.23.3 stores a turn whose request
// failed as an assistant row with no finish reason, shown as failed_turn
// (measured, session-failed.jsonl). A note before it is not held; the same
// row with a finish reason, a reply the model finished, holds it.
//
// Mutation: settle on any assistant row, and the first case goes red.
func TestAFailedHermesTurnSettlesNoNote(t *testing.T) {
	t.Parallel()
	take := []ServedTake{{InboxID: "a", Digest: ItemDigest(hermesSteeredNote)}}
	for finished, want := range map[bool][]string{false: nil, true: {"a"}} {
		record := hermesFixtureRows(t, "session-failed.jsonl", func(id int64, r map[string]any) {
			switch {
			case id == 1:
				r["display_kind"] = "steer"
				r["content"] = hermesNoteOpen + hermesSteeredNote + hermesNoteClose
			case id == 2 && finished:
				r["finish_reason"] = "stop"
			}
		})
		if got, err := hermesNotesHeld(bytes.NewReader(record), take, 0); err != nil || !slices.Equal(got, want) {
			t.Errorf("a reply finished %v: held %v (%v), want %v", finished, got, err, want)
		}
	}
}
