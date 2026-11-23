package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
// toolsets disabled on a web-denied run alone, tool search off where a server
// is always loaded alone (Hermes exempts no server from it), and a value
// Hermes would expand refused.
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
	if err != nil || strings.Contains(string(allowed), "disabled_toolsets") || strings.Contains(string(allowed), "tool_search") {
		t.Errorf("an allowed run with no always-loaded server disables toolsets or tool search: %s (%v)", allowed, err)
	}
	servers["crewlet"] = sandbox.MCPServer{Transport: sandbox.TransportHTTP, URL: "http://x/${HOME}"}
	if _, err := hermesOverlay(servers, false); err == nil {
		t.Error("a value Hermes would expand was let through")
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
	modules := filepath.Join(bin, "py", "hermes_cli")
	if err := os.MkdirAll(modules, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, src := range hermesLoaderStandIn(hermesSystemScope(bin)) {
		if err := os.WriteFile(filepath.Join(modules, name), []byte(src), 0o600); err != nil {
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
// modules, as it reads them (managed_scope.py, config.py; YAML read as the
// JSON the cases write): the managed scope HERMES_MANAGED_DIR names, else
// system, where it is a directory; and HERMES_HOME's config.yaml with that
// scope's merged over it per leaf.
func hermesLoaderStandIn(system string) map[string]string {
	return map[string]string{
		"__init__.py": "",
		"managed_scope.py": `import json, os
from pathlib import Path
def get_managed_dir():
    p = Path(os.environ.get("HERMES_MANAGED_DIR", "").strip() or ` + strconv.Quote(system) + `)
    return p if p.is_dir() else None
def load_managed_config():
    d = get_managed_dir()
    return json.loads((d / "config.yaml").read_text()) if d is not None and (d / "config.yaml").is_file() else {}
`,
		"config.py": `import json, os
from hermes_cli import managed_scope
def merge(under, over):
    out = dict(under)
    for k, v in over.items():
        out[k] = merge(out[k], v) if isinstance(v, dict) and isinstance(out.get(k), dict) else v
    return out
def load_config():
    path = os.path.join(os.environ.get("HERMES_HOME", "/nonexistent"), "config.yaml")
    user = json.load(open(path)) if os.path.isfile(path) else {}
    return merge(user, managed_scope.load_managed_config())
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
		if got, _, _ := install(t, scope, false); !slices.Equal(got.Agent.Disabled, []string{"homeassistant"}) {
			t.Errorf("disabled toolsets = %v, want the box's alone", got.Agent.Disabled)
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
// WAS CREATED WITH: restored into a new store and indexed where the box runs
// the one release and the launch and model route match; a launch naming
// another model, one whose working directory the box no longer has (Hermes
// 0.23.3 then runs nothing and exits 1), a box configured for another model
// provider (which Hermes would ignore for the frozen one), an archive with no
// launch kept, or a box on another release, starts the fresh session instead,
// saying why.
//
// The restored store is the archive's alone, so no other session's index line
// or kept launch stays beside it.
//
// Mutation: skip the policy or route check, read a missing launch as a
// mismatch of its values, restore on another release, continue without
// indexing the restored session, or keep another session's index line, and a
// row goes red.
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
// environment) and a resume keeps them, so the carry script writes the run's
// there, and removes them for a run that has none. Measured on 0.23.3: the
// resumed request's system message then ends with the renewed text.
//
// The text is the launch's own ($(cat), trailing newlines dropped), the
// launch is written back compact as Hermes writes it, and a store holding no
// frozen launch of the session answers none.
//
// Mutation: skip the policy's rewrite, keep the old text for a run with none,
// keep the file's trailing newlines, write the launch back spaced, or answer
// ok having renewed nothing, and this goes red.
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
	var out []byte
	for line := range bytes.Lines(hermesFixture(t, "0.23.3", "session-steered.jsonl")) {
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
// the last reply; two takes of one note's words need two notes; and a row
// holding several notes joined by newlines (one of several lines) holds each.
//
// Mutations, each red: match rows from 0 whatever the launch's start; take an
// inactive row; drop the display_kind check; settle without a later reply;
// match by digest set rather than one take per note; match a row whole.
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
		{"an earlier launch's identical words", keep, 5, one, nil},
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
// working directory holds its own AGENTS.md and a checkout at ./repo, as
// code_checkout places one, with every layer a repository brings. The runner
// hands none of it: Hermes 0.23.3's own subdirectory hints add a checkout's
// instructions (the first of AGENTS.override.md, AGENTS.md, agents.md,
// CLAUDE.md, claude.md and .cursorrules in each directory the model touches,
// up to the working directory) to the tool result that first touches it
// (measured), and it loads none of a checkout's .hermes.md, .cursor rules,
// skills, plugins or configuration, which it reads from the working directory
// alone. So no file the run writes, nor its script, carries any of them.
//
// Mutation: hand the checkout's instructions, skills, plugins or settings to
// the run in any file it writes or in its script, and its kind goes red.
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
