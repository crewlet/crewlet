package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// museFixture is testdata/muse/1.4.25/<name>: a real Muse Code 1.4.25 run's
// stream (`*.stream.jsonl`), the file a run of this runner's own script left
// (`*.result.jsonl`), or a session's record (`*.session.jsonl`,
// `*.child.jsonl`), against a stand-in model and a stand-in bridge. A record
// keeps every line under 4 KiB and every line a reader here reads, whole; the
// lines carrying the vendor's system prompt and request traces are dropped.
func museFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "muse", "1.4.25", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// museDecoded is the decoder fed stream.
func museDecoded(t *testing.T, stream []byte) *museEvents {
	t.Helper()
	dec := &museEvents{}
	if err := eachLine(bytes.NewReader(stream), dec, maxLineBytes); err != nil {
		t.Fatal(err)
	}
	return dec
}

// A RUN THIS RUNNER LAUNCHED (the engine's own script, settings and brief, in
// a box against the stand-ins: a bridge call, a subagent, a shell call) reads
// as its last answer, its session, and its WHOLE spend: every `run.usage` of
// the exec and the subagent's requests its script added after the mark.
//
// Mutation: drop the records after the mark, or keep only the last
// `run.usage`, and the spend comes out short.
func TestALaunchedMuseRunReadsAsItsAnswerAndItsWholeSpend(t *testing.T) {
	res := (Muse{}).Parse(museFixture(t, "launched.result.jsonl"))
	if !res.Success || res.Text != "ANSWER launched run." || res.SessionID != "0b5e0000-0000-4000-8000-0000000000e1" {
		t.Fatalf("result = %+v, want the answer of session ...e1", res)
	}
	// The main thread: 4137 input (1600 cached), 200 output over 4 requests;
	// the subagent: 2070 (800 cached), 100 output.
	if res.InputTokens != 6207 || res.CacheReadTokens != 2400 || res.OutputTokens != 300 || res.Turns != 4 {
		t.Errorf("charged in %d (cache %d), out %d, turns %d; want 6207 (2400), 300, 4", res.InputTokens,
			res.CacheReadTokens, res.OutputTokens, res.Turns)
	}
	if !strings.Contains(res.Transcript, "mcp__crewlet__echo") || !strings.Contains(res.Transcript, "subagent_spawn") {
		t.Errorf("the transcript names the run's calls:\n%s", res.Transcript)
	}
}

// A SUBAGENT'S RECORD IS CHARGED ONLY AFTER THE SCRIPT'S MARK: the stream
// itself prints no session record, so one before the mark is not the
// script's, and a stream with no run.usage states no spend.
//
// Mutation: count a `model_completed` record whatever came before it.
func TestAMuseSubagentRecordBeforeTheMarkIsNotCharged(t *testing.T) {
	child := museFixture(t, "subagent.child.jsonl")
	stream := museFixture(t, "subagent.stream.jsonl")
	spent, stated := museDecoded(t, append(slices.Clone(child), stream...)).runSpend()
	if !stated || spent != (runSpend{input: 3048 - 1200, cacheRead: 1200, output: 150}) {
		t.Fatalf("spend %+v (stated %v), want the main thread's alone", spent, stated)
	}
	marked := append(slices.Clone(stream), []byte(`{"payload_type":"`+museChildMark+`"}`+"\n")...)
	spent, _ = museDecoded(t, append(marked, child...)).runSpend()
	if spent != (runSpend{input: 3048 + 2032 - 2000, cacheRead: 2000, output: 250}) {
		t.Fatalf("spend %+v, want the main thread's and the subagent's two requests", spent)
	}
	if _, stated := museDecoded(t, child).runSpend(); stated {
		t.Error("a stream with no run.usage stated a spend")
	}
}

// A RATE LIMIT OR A SPENT BALANCE IS A LIMIT ONLY ON MUSE'S OWN FAILED
// TERMINAL: the run fails, and its terminal line is the limit report; a run
// that completed reports none.
//
// Mutation: drop the limit from the failed terminal, or read it off any line.
func TestAMuseRateLimitIsReportedAsALimit(t *testing.T) {
	for _, name := range []string{"ratelimit.stream.jsonl", "billing.stream.jsonl"} {
		res := (Muse{}).Parse(museFixture(t, name))
		if res.Success || res.Limits == "" || !strings.Contains(res.Limits, `"run.terminal.failed"`) {
			t.Errorf("%s: result %+v, want a failure whose terminal is the limit report", name, res)
		}
	}
	if res := (Muse{}).Parse(museFixture(t, "tools.stream.jsonl")); !res.Success || res.Limits != "" {
		t.Errorf("a completed run reads as %+v, want success and no limit", res)
	}
}

// A RUN ITS CAP STOPPED READS AS CAPPED, even where Muse woke on a subagent's
// result and ran on to an answer after it (measured: a capped run, then a
// completed one, in one exec).
//
// Mutation: read the last terminal alone, and the woken exec reads as a success.
func TestAMuseRunItsCapStoppedReadsAsCapped(t *testing.T) {
	for _, name := range []string{"capped.stream.jsonl", "capped-then-woken.result.jsonl"} {
		res := (Muse{}).Parse(museFixture(t, name))
		if res.Success || !res.RoundCapped || !strings.HasPrefix(res.Error, museCapped) {
			t.Errorf("%s: result %+v, want the round capped", name, res)
		}
	}
}

// AN EXEC PAST ITS CAP FAILS WHATEVER EACH RUN SAID: the script names the cap,
// and the stream's own count of the main thread's requests is held to it.
//
// Mutation: drop the count check, and a five-request run under a cap of two
// succeeds.
func TestAMuseExecPastItsCapFailsWhateverEachRunSaid(t *testing.T) {
	stream := append([]byte(`{"payload_type":"`+museCapMark+`","turns":2}`+"\n"), museFixture(t, "tools.stream.jsonl")...)
	res := (Muse{}).Parse(stream)
	if res.Success || res.RoundCapped || !strings.Contains(res.Error, "turn cap of 2 was not enforced") {
		t.Fatalf("result %+v, want the unenforced cap", res)
	}
	stream = append([]byte(`{"payload_type":"`+museCapMark+`","turns":5}`+"\n"), museFixture(t, "tools.stream.jsonl")...)
	if res := (Muse{}).Parse(stream); !res.Success {
		t.Fatalf("a run within its cap reads as %+v", res)
	}
}

// museScriptHome is a box home under a temporary directory holding files, by
// path below it.
func museScriptHome(t *testing.T, files map[string]string) Paths {
	t.Helper()
	p := Paths{Home: t.TempDir()}
	for name, body := range files {
		full := filepath.Join(p.Home, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(p.WorkDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// runSh runs script under sh, failing the case on an error.
func runSh(t *testing.T, script string) []byte {
	t.Helper()
	for _, tool := range []string{"find", "wc", "sed", "tail", "grep", "head", "tar", "gzip", "cksum"} {
		prereqtest.OnPath(t, tool, "the case runs the box's script", "install "+tool)
	}
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	out, err := exec.Command(sh, "-c", script).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("the script failed: %v\n%s", err, exit.Stderr)
		}
		t.Fatalf("the script failed: %v", err)
	}
	return out
}

// THE SCRIPT ADDS ONLY THIS RUN'S SUBAGENT REQUESTS: lines a subagent's record
// held before the harness started are an earlier run's, and only the
// `model_completed` records after them follow the mark.
//
// Mutation: drop the count kept before the launch, and the earlier run's
// request is charged again.
func TestTheMuseScriptAddsOnlyThisRunsSubagentRequests(t *testing.T) {
	const session = "0b5e0000-0000-4000-8000-000000000005"
	child := "/.local/share/muse/sessions/2026/11/22/" + session + "/subagent/t1/session.jsonl"
	lines := bytes.SplitAfter(museFixture(t, "subagent.child.jsonl"), []byte("\n"))
	first := slices.IndexFunc(lines, func(l []byte) bool { return bytes.Contains(l, []byte(`"kind":"model_completed"`)) })
	if first < 0 {
		t.Fatal("the fixture holds no request")
	}
	p := museScriptHome(t, map[string]string{
		"/.local/share/muse/sessions/2026/11/22/" + session + "/session.jsonl": "{}\n",
		child: string(bytes.Join(lines[:first+1], nil)),
	})
	before, after := museChildUsage(p)
	runSh(t, "s="+session+"\n"+before)
	f, err := os.OpenFile(p.Home+child, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(bytes.Join(lines[first+1:], nil))
	_ = f.Close()
	out := runSh(t, "s="+session+"\n"+after)
	got := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	if len(got) != 2 || !bytes.Contains(got[0], []byte(museChildMark)) ||
		!bytes.Contains(got[1], []byte(`"input_tokens":1017`)) {
		t.Fatalf("the script added %q, want the mark and the second request alone", out)
	}
}

// A MUSE RUN STARTS ON ITS POSTURE: exec in JSON, every approval and the
// sandbox off with the workspace trusted for this run, its own session id, no
// reminder agents, no ask-the-user tool, its settings' layer, the brief from
// its file, the cap as Muse's own; and nothing secret on its command line,
// nor its standing instructions.
//
// Mutation: drop --yolo (an untrusted workspace loads no bridge), or put the
// bridge's header or the standing instructions on the command line.
func TestAMuseRunIsStartedOnItsPosture(t *testing.T) {
	p := Paths{Home: "/home/user"}
	req := sandbox.RunRequest{Model: "muse-1", Effort: "high", Limits: sandbox.Limits{MaxTurns: 7}, System: "SEAT-TEXT",
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://e/mcp",
			Headers: map[string]string{"Authorization": "Bearer sekret-token"}}}}
	cmd := (Muse{}).Command(req, Session{ID: "0b5e0000-0000-4000-8000-0000000000a1"}, p, p.MuseSettings())
	for _, want := range []string{"muse exec --json --yolo", `--session-id "$s"`, "--disable-reminders",
		"--disable-request-user-input", "--allow-workspace-switch",
		"--model '\\''muse-1'\\''", "--reasoning-effort '\\''high'\\''", "--max-model-steps 7",
		museLayer + ` --prompt-file "$p"`, "at 1 4 25", museCapMark} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the command lacks %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "sekret-token") || strings.Contains(cmd, "SEAT-TEXT") {
		t.Errorf("the command line carries the bridge's header or the standing instructions:\n%s", cmd)
	}
	if strings.Contains((Muse{}).Command(sandbox.RunRequest{}, Session{ID: "x"}, p, ""), "--max-model-steps") {
		t.Error("a run with no cap is capped")
	}
}

// MUSE'S SETTINGS KEEP THE HOME'S: the standing instructions are set as the
// home's `run.system_prompt_append` after the home's own, beside its other
// `run` keys, settings and MCP servers, and a home with no settings gets them
// alone; a run handed no system text leaves the file untouched; a file that
// is no JSON object, or an append that is no string, is refused. EACH SERVER IS WRITTEN APART, for the script to add
// after the notes plugin's approval: every one required with the engine's
// call wait in whole seconds rounded up, a command server as `stdio`.
//
// Mutation: write the settings whole, drop `required`, round the wait down,
// or write the servers into the settings, whose approval drops both.
func TestMuseWriteConfigWritesItsSettingsAndServers(t *testing.T) {
	nonce := sessionNonce("s")
	box := sandbox.NewFakeSandbox("b")
	p := PathsFor(box)
	home := `{"schema_version":1,"run":{"toolset":["shell"],"system_prompt_append":"HOME"},` +
		`"mcpServers":{"home":{"type":"stdio","command":"h"}}}`
	if err := box.WriteFile(t.Context(), p.MuseSettings(), []byte(home)); err != nil {
		t.Fatal(err)
	}
	req := sandbox.RunRequest{Brief: "fix it", System: "SEAT <a & b>", MCPServers: map[string]sandbox.MCPServer{
		"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://e/mcp", Headers: map[string]string{"Authorization": "Bearer t"},
			CallTimeout: 6*time.Minute + 500*time.Millisecond},
		"local": {Command: "srv", Args: []string{"-x"}, Env: map[string]string{"K": "v"}},
	}}
	config, err := (Muse{}).WriteConfig(t.Context(), box, req, Session{ID: "s"}, p)
	if err != nil || config != p.MuseSettings() {
		t.Fatalf("WriteConfig = %q, %v", config, err)
	}
	read := func(b sandbox.Sandbox, path string) map[string]any {
		raw, _ := b.ReadFile(t.Context(), path)
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, raw)
		}
		return got
	}
	if got := read(box, p.MuseSettings()); !jsonEqual(got, map[string]any{"schema_version": 1.0,
		"run":        map[string]any{"toolset": []any{"shell"}, "system_prompt_append": "HOME\n\nSEAT <a & b>"},
		"mcpServers": map[string]any{"home": map[string]any{"type": "stdio", "command": "h"}}}) {
		t.Errorf("settings = %v", got)
	}
	if got := read(box, p.MuseServers()+"/crewlet-"+nonce+".json"); !jsonEqual(got, map[string]any{"url": "http://e/mcp",
		"headers": map[string]any{"Authorization": "Bearer t"}, "required": true, "tool_timeout_sec": 361.0}) {
		t.Errorf("crewlet = %v", got)
	}
	if got := read(box, p.MuseServers()+"/local-"+nonce+".json"); !jsonEqual(got, map[string]any{"type": "stdio",
		"command": "srv", "args": []any{"-x"}, "env": map[string]any{"K": "v"}, "required": true}) {
		t.Errorf("local = %v", got)
	}
	if brief, _ := box.ReadFile(t.Context(), p.Brief()); string(brief) != "fix it" {
		t.Errorf("the brief file holds %q", brief)
	}
	bare := sandbox.NewFakeSandbox("c")
	if _, err := (Muse{}).WriteConfig(t.Context(), bare, sandbox.RunRequest{System: "SEAT"}, Session{ID: "s"}, p); err != nil {
		t.Fatal(err)
	}
	if got := read(bare, p.MuseSettings()); !jsonEqual(got, map[string]any{"schema_version": 1.0,
		"run": map[string]any{"system_prompt_append": "SEAT"}}) {
		t.Errorf("a home with no settings got %v", got)
	}
	untouched := sandbox.NewFakeSandbox("d")
	if err := untouched.WriteFile(t.Context(), p.MuseSettings(), []byte(home)); err != nil {
		t.Fatal(err)
	}
	if _, err := (Muse{}).WriteConfig(t.Context(), untouched, sandbox.RunRequest{}, Session{ID: "s"}, p); err != nil {
		t.Fatal(err)
	}
	if raw, _ := untouched.ReadFile(t.Context(), p.MuseSettings()); string(raw) != home {
		t.Errorf("a run with no system text wrote %s", raw)
	}
	for _, broken := range []string{"[1]", "null", `{"run":"x"}`, "{", `{"run":{"system_prompt_append":1}}`} {
		if err := untouched.WriteFile(t.Context(), p.MuseSettings(), []byte(broken)); err != nil {
			t.Fatal(err)
		}
		if _, err := (Muse{}).WriteConfig(t.Context(), untouched, sandbox.RunRequest{System: "S"},
			Session{ID: "s"}, p); err == nil || !strings.Contains(err.Error(), p.MuseSettings()) {
			t.Errorf("settings %s: err = %v, want them refused by name", broken, err)
		}
	}
	if _, err := (Muse{}).WriteConfig(t.Context(), box, sandbox.RunRequest{MCPServers: map[string]sandbox.MCPServer{
		"a b": {Transport: sandbox.TransportHTTP, URL: "http://e"}}}, Session{ID: "s"}, p); err == nil {
		t.Error("a server name no file and shell word carries was taken")
	}
}

// jsonEqual reports whether a and b marshal to the same JSON.
func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// A MUSE RUN PAST ITS STANDING-INSTRUCTION LIMIT IS REFUSED AT LAUNCH, naming
// the setting, rather than started to fail in its box: 1.4.25 refuses more than
// 65,536 bytes of `run.system_prompt_append` (measured).
//
// Mutation: raise or drop museSystemMax; leave the home's append out of the
// count.
func TestAMuseRunPastItsSystemLimitIsRefused(t *testing.T) {
	box := sandbox.NewFakeSandbox("b")
	p := PathsFor(box)
	if _, err := (Muse{}).WriteConfig(t.Context(), box, sandbox.RunRequest{System: strings.Repeat("x", 65536)}, Session{ID: "s"}, p); err != nil {
		t.Fatalf("65,536 bytes refused: %v", err)
	}
	_, err := (Muse{}).WriteConfig(t.Context(), box, sandbox.RunRequest{System: strings.Repeat("x", 65537)}, Session{ID: "s"}, p)
	if err == nil || !strings.Contains(err.Error(), "run.system_prompt_append") {
		t.Fatalf("65,537 bytes: %v, want the limit named", err)
	}
	// THE HOME'S OWN APPEND COUNTS: Muse reads the two as one setting.
	home := `{"run":{"system_prompt_append":"` + strings.Repeat("h", 65530) + `"}}`
	for system, refused := range map[string]bool{"xxxx": false, "xxxxx": true} {
		held := sandbox.NewFakeSandbox("h")
		if err := held.WriteFile(t.Context(), p.MuseSettings(), []byte(home)); err != nil {
			t.Fatal(err)
		}
		_, err = (Muse{}).WriteConfig(t.Context(), held, sandbox.RunRequest{System: system}, Session{ID: "s"}, p)
		if got := err != nil && strings.Contains(err.Error(), "the box home's run.system_prompt_append"); got != refused {
			t.Errorf("the home's 65,530 bytes and %d of the run's: %v, want refused %v", len(system), err, refused)
		}
	}
}

// A CONTINUING RUN NAMES ITS SERVERS AS ITS SESSION'S FIRST RUN DID, and
// writes a second wiring for the session its script may start instead, its
// servers wired to the job's other token under that session's own names, and
// the brief that session is handed. The record hands the model every earlier
// call under the name it was made by, and Muse refuses a call to a name the
// run no longer has (measured on 1.4.25). Where the run is handed no second
// wiring, the new session still names the run's servers for itself.
//
// Mutations, each red: draw the suffix per run; name the fresh servers for
// the continued session; write the fresh servers from the continued wiring;
// write no fresh wiring where the request hands none.
func TestAContinuingMuseRunNamesItsServersAsItsSessionDid(t *testing.T) {
	const session, freshID = "0b5e0000-0000-4000-8000-0000000000a1", "0b5e0000-0000-4000-8000-0000000000a2"
	server := func(token string) map[string]sandbox.MCPServer {
		return map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://e/mcp",
			Headers: map[string]string{"Authorization": "Bearer " + token}}}
	}
	first := sandbox.NewFakeSandbox("b")
	p := PathsFor(first)
	if _, err := (Muse{}).WriteConfig(t.Context(), first, sandbox.RunRequest{MCPServers: server("first")},
		Session{ID: session}, p); err != nil {
		t.Fatal(err)
	}
	named := p.MuseServers() + "/crewlet-" + sessionNonce(session) + ".json"
	if _, err := first.ReadFile(t.Context(), named); err != nil {
		t.Fatalf("the session's first run wrote no %s: %v", named, err)
	}
	box := sandbox.NewFakeSandbox("b")
	req := sandbox.RunRequest{Resume: session, FreshBrief: "start over",
		MCPServers: server("continued"), FreshWiring: &sandbox.Wiring{MCPServers: server("fresh")}}
	if _, err := (Muse{}).WriteConfig(t.Context(), box, req, Session{ID: session, Continues: true, Fresh: freshID},
		p); err != nil {
		t.Fatal(err)
	}
	continued, _ := box.ReadFile(t.Context(), named)
	fresh, _ := box.ReadFile(t.Context(), p.MuseServersFresh()+"/crewlet-"+sessionNonce(freshID)+".json")
	if !strings.Contains(string(fresh), "Bearer fresh") || !strings.Contains(string(continued), "Bearer continued") {
		t.Fatalf("servers %s, fresh %s", continued, fresh)
	}
	if brief, _ := box.ReadFile(t.Context(), p.FreshBrief()); string(brief) != "start over" {
		t.Errorf("the fresh brief holds %q", brief)
	}
	req.FreshWiring = nil
	unwired := sandbox.NewFakeSandbox("b")
	if _, err := (Muse{}).WriteConfig(t.Context(), unwired, req, Session{ID: session, Continues: true,
		Fresh: freshID}, p); err != nil {
		t.Fatal(err)
	}
	own, _ := unwired.ReadFile(t.Context(), p.MuseServersFresh()+"/crewlet-"+sessionNonce(freshID)+".json")
	if !strings.Contains(string(own), "Bearer continued") {
		t.Errorf("a run handed one wiring names no server for the session it may start: %q", own)
	}
	if !strings.Contains(museScript(req, Session{ID: session, Continues: true, Fresh: freshID}, p, time.Second),
		p.MuseServersFresh()) {
		t.Error("the script of a run handed one wiring installs no servers for the session it may start")
	}
	for _, s := range []Session{{}, {ID: session, Continues: true}} {
		if _, err := (Muse{}).WriteConfig(t.Context(), box, req, s, p); err == nil {
			t.Errorf("a run naming no session (%+v) was written", s)
		}
	}
}

// A MUSE SESSION PACKS ITS RECORD, ITS SUBAGENTS' AND ITS TOOL OUTPUTS UNDER
// ITS DAY, never cron.db (binary, which the text scrub would corrupt) nor the
// cli log; and restores in another home under the same day, every other copy
// of it there removed, its script continuing it.
//
// Mutation: pack the whole directory, or restore under another day.
func TestAMuseSessionPacksAndRestoresUnderItsDay(t *testing.T) {
	const id = "0b5e0000-0000-4000-8000-0000000000b1"
	day := "2026/11/22/" + id
	dir := "/.local/share/muse/sessions/" + day
	from := museScriptHome(t, map[string]string{
		dir + "/session.jsonl":             "{\"payload_type\":\"x\"}\n",
		dir + "/subagent/t1/session.jsonl": "{}\n",
		dir + "/tool-outputs/a/out.txt":    "output",
		dir + "/cron.db":                   "SQLite format 3\x00",
		dir + "/cli-1.log":                 "log",
		"/.config/muse/auth.json":          `{"providers":{}}`,
	})
	dest := from.WorkDir() + "/pack.tgz"
	runSh(t, (Muse{}).Pack(from, id, from.WorkDir()+"/staged", dest, false))
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	members := tarFiles(t, raw)
	want := []string{day + "/session.jsonl", day + "/subagent/t1/session.jsonl", day + "/tool-outputs/a/out.txt"}
	if !slices.Equal(members, want) {
		t.Fatalf("members = %v, want %v", members, want)
	}

	stale := "/.local/share/muse/sessions/2026/11/01/" + id
	to := museScriptHome(t, map[string]string{stale + "/session.jsonl": "{}\n"})
	if err := os.WriteFile(to.SessionCarried(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	gate := "v=" + shellword.Quote("Muse Code "+museSupported()) + "\n" +
		museSessionGate(Session{ID: id, Continues: true, Replaces: true, Fresh: "fresh"}, to, false) +
		`printf '%s\n' "$s" "$k" "$w"`
	if got := string(runSh(t, gate)); got != id+"\nyes\n\n" {
		t.Fatalf("the gate said %q, want the carried session continued", got)
	}
	if _, err := os.Stat(to.Home + dir + "/tool-outputs/a/out.txt"); err != nil {
		t.Errorf("the session is not restored under its day: %v", err)
	}
	if _, err := os.Stat(to.Home + stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the box's own copy under another day stayed: %v", err)
	}
}

// tarFiles is the regular files of a gzipped tar, in its order.
func tarFiles(t *testing.T, raw []byte) []string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			files = append(files, h.Name)
		}
	}
}

// A MUSE SESSION CONTINUES ONLY BETWEEN TWO BOXES ON EXACTLY 1.4.25, each
// release as `muse --version` prints it, read as the engine reads it
// ([releaseOf]): any other pair, a newer release or a pre-release included,
// starts a session of its own, naming the release it found.
//
// Mutation: compare by a floor, read a release from its first digit, or drop
// the writer's release.
func TestAMuseSessionContinuesOnlyBetweenBoxesOnTheSupportedRelease(t *testing.T) {
	const supported, newer, older, pre = "Muse Code 1.4.25 (1.4.25-R9361.1)", "Muse Code 1.4.30 (1.4.30-R1.1)",
		"Muse Code 1.4.13 (1.4.13-R6996.1)", "Muse Code 1.4.25-rc.1"
	cases := []struct {
		box, writer string
		ok          bool
		names       string
	}{
		{supported, supported, true, ""},
		{newer, supported, false, "1.4.30"},
		{supported, newer, false, "1.4.30"},
		{older, supported, false, "1.4.13"},
		{supported, older, false, "1.4.13"},
		{pre, supported, false, "a release it could not read"},
		{supported, pre, false, "named no release this build reads"},
		{supported, "", false, "named no release this build reads"},
	}
	for _, c := range cases {
		ok, why := (Muse{}).Continues(releaseOf(c.box), releaseOf(c.writer))
		if ok != c.ok || !strings.Contains(why, c.names) {
			t.Errorf("Continues(%q, %q) = %v (%q), want %v naming %q", c.box, c.writer, ok, why, c.ok, c.names)
		}
	}
}

// A BOX'S RELEASE DECIDES THE RUN: an older one is refused before anything
// runs, a newer one or a pre-release starts a session of its own where a
// session was to be continued, saying so in the engine's words
// ([Muse.continuesInPlace]), and only 1.4.25 continues it (the script as a box
// runs it, its `muse --version` a stand-in's).
//
// Mutation: let a newer release or a pre-release continue, or start an older
// one.
func TestAMuseBoxsReleaseDecidesTheRun(t *testing.T) {
	const id = "0b5e0000-0000-4000-8000-0000000000c9"
	for _, c := range []struct {
		version string
		refused bool
		k       string
	}{
		{"Muse Code 1.4.13 (1.4.13-R6996.1)", true, ""},
		{"Muse Code 1.4.30 (1.4.30-R1.1)", false, "no"},
		{"Muse Code 1.4.25-rc.1", false, "no"},
		{"Muse Code 1.4.25 (1.4.25-R9361.1)", false, "yes"},
	} {
		p := museScriptHome(t, map[string]string{
			"/.local/share/muse/sessions/2026/11/22/" + id + "/session.jsonl": "{}\n",
		})
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "muse"), []byte("#!/bin/sh\necho '"+c.version+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		script := "PATH=" + bin + ":$PATH\n" + museRelease +
			fmt.Sprintf(`if ! at %s; then echo refused; exit 0; fi`+"\n", floorArgs(museSupported())) +
			museSessionGate(Session{ID: id, Continues: true, Fresh: "fresh-id"}, p, false) +
			`printf '%s %s\n' "$k" "$w"`
		out := strings.TrimSpace(string(runSh(t, script)))
		k, _, _ := strings.Cut(out, " ")
		switch {
		case c.refused && out != "refused":
			t.Errorf("%s: the script said %q, want it refused", c.version, out)
		case !c.refused && k != c.k:
			t.Errorf("%s: the script said %q, want continued %s", c.version, out, c.k)
		case c.k == "no" && !strings.Contains(out, inPlaceWhy(c.version)):
			t.Errorf("%s: the fresh start says %q, not as the engine's check says it", c.version, out)
		}
	}
}

// inPlaceWhy is why the engine's check refuses a box whose `muse --version`
// printed printed to continue a session in place.
func inPlaceWhy(printed string) string {
	_, why := (Muse{}).continuesInPlace(releaseOf(printed))
	return why
}

// museArchive is a gzipped tar holding files, by member name.
func museArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(files[name])),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(files[name])
	}
	_ = tw.Close()
	_ = zw.Close()
	return out.Bytes()
}

// museTranscriptOf is archive's transcript entries.
func museTranscriptOf(t *testing.T, archive []byte) []TranscriptEntry {
	t.Helper()
	got, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(t.Context(), Muse{},
		bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	return got.Entries
}

// A MUSE ARCHIVE READS AS ITS SESSION'S TRANSCRIPT: the brief, the model's
// calls and their results, its answer, and a subagent's lines under its
// thread; a compaction as its summary; a hook's context the engine wrote as
// the note it carries, and one it did not write not at all.
//
// Mutation: read the subagent's record as the main thread's, or file a hook's
// context as a brief.
func TestAMuseArchiveReadsAsTheSessionsTranscript(t *testing.T) {
	const day = "2026/11/22/0b5e0000-0000-4000-8000-000000000005"
	got := museTranscriptOf(t, museArchive(t, map[string][]byte{
		day + "/session.jsonl": museFixture(t, "subagent.session.jsonl"),
		day + "/subagent/01a20260-c393-79c2-8689-54b9168800ec/session.jsonl": museFixture(t, "subagent.child.jsonl"),
	}))
	has := func(kind EntryKind, thread, text string) bool {
		return slices.ContainsFunc(got, func(e TranscriptEntry) bool {
			return e.Kind == kind && e.Thread == thread && strings.Contains(e.Text, text)
		})
	}
	if !has(EntryBrief, "", "run it m5") || !has(EntryModel, "", "done m5.") ||
		!slices.ContainsFunc(got, func(e TranscriptEntry) bool { return e.Kind == EntryToolCall && e.Tool == "subagent_spawn" }) ||
		!has(EntryModel, "01a20260-c393-79c2-8689-54b9168800ec", "CHILD-FINAL") {
		t.Fatalf("entries = %+v", got)
	}

	note := "## A note from ada, sent while this turn was running\n\nTarget main."
	block, _ := json.Marshal(map[string]any{"payload_type": "runtime.session", "recorded_at": 1795336482096791,
		"payload": map[string]any{"event": map[string]any{"kind": "context_block_updated", "source": museHookSource,
			"text": note}}})
	compacted := append(slices.Clone(museFixture(t, "compacted.session.jsonl")), append(block, '\n')...)
	got = museTranscriptOf(t, museArchive(t, map[string][]byte{"2026/11/22/s/session.jsonl": compacted}))
	if !has(EntrySummary, "", "SUMMARY-M6") || !has(EntryNote, "", "Target main.") {
		t.Fatalf("entries = %+v, want the summary and the note", got)
	}
	if slices.ContainsFunc(got, func(e TranscriptEntry) bool { return strings.Contains(e.Text, "PERIWINKLE") }) {
		t.Error("a hook's context the engine did not write was read as an entry")
	}
}

// A MUSE RUN'S ARTEFACTS ARE ITS OWN FILES, never the home's settings,
// skills, instructions or plugins, its login, its sessions, or the record of
// the servers it added, which the next clear takes them out by.
//
// Mutation: list the settings, a skill directory, the plugins, the login, the
// sessions directory or the servers' record.
func TestAMuseRunsArtifactsAreItsOwnFiles(t *testing.T) {
	p := Paths{Home: "/h"}
	files, dirs := (Muse{}).artifactFiles(p), (Muse{}).artifactDirs(p)
	if !slices.Equal(files, []string{p.MuseChildLines(), p.MuseTakeConfig(), p.MuseTakeConfigFresh(), p.MuseNoteHelper(),
		p.MuseNotesInstall(), p.MuseCheckoutLaunch(), p.MuseCheckoutSections(), p.MuseForeignRules(),
		p.MuseHomeRulesOwn()}) ||
		!slices.Equal(dirs, []string{p.MuseServers(), p.MuseServersFresh(), p.MuseNotesPlugin()}) {
		t.Fatalf("artifacts = %v %v", files, dirs)
	}
	for _, kept := range []string{p.MuseSettings(), p.MuseConfig() + "/auth.json", p.MuseSessions(),
		p.MuseConfig() + "/AGENTS.md", p.MuseSkills() + "/home", p.MuseConfig() + "/skills/home",
		p.Home + "/.local/share/muse/plugins/installed.json", p.MuseServersAdded(), p.MuseHomeRules()} {
		if slices.ContainsFunc(append(files, dirs...), func(gone string) bool {
			return kept == gone || strings.HasPrefix(kept, gone+"/")
		}) {
			t.Errorf("the clear removes %s", kept)
		}
	}
}

// A MUSE SESSION IS JUDGED ON THE SAFE SIDE: its record holds no window, so a
// relaunch hands the summarized telling, thins nothing, and counts a brief's
// byte as a token.
//
// Mutation: judge it by a window.
func TestAMuseSessionIsJudgedOnTheSafeSide(t *testing.T) {
	got := SessionTraits(MuseName)
	if got.FirstRequest != sandbox.FirstRequestBox || got.CallKeying != sandbox.CallKeyingNone || got.Charge != 1 {
		t.Fatalf("traits = %+v", got)
	}
}

// MUSE'S ONE RELEASE IS THE ONE ITS PROFILE'S written_for NAMES, read bare
// as every release here is: never none, so a box or writer whose release
// does not read ("") never equals it and continues nothing.
//
// Mutation: read the release from a written_for that names none, or take it
// from a literal the profile does not hold, and a check goes red.
func TestMuseIsWrittenForTheReleaseItsProfileNames(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin(MuseName)
	if museSupported() == "" || museSupported() != harnessprofile.Release(p.WrittenFor) {
		t.Fatalf("the runner is written for %q, and the profile's written_for (%q) names %q", museSupported(),
			p.WrittenFor, harnessprofile.Release(p.WrittenFor))
	}
	if ok, _ := (Muse{}).Continues("", ""); ok {
		t.Error("an unread box continues an unread writer's session")
	}
}

// A SERVER NAMED IN UP TO 19 CHARACTERS IS SHOWN WHOLE, AND A LONGER ONE STILL
// RUNS: with the run's `-<6 hex>`, a 19-character name is within the 26 Muse
// Code shows the model as written, and a 40-character one, which Muse shows
// as a hash, is written all the same; nothing is refused for its length.
//
// Mutation: lengthen the suffix, or refuse a name for its length.
func TestAMuseServerNameStaysWholeUpTo19Characters(t *testing.T) {
	nonce := sessionNonce("s")
	readable := strings.Repeat("a", 19)
	if full := readable + "-" + nonce; len(full) > museServerNameShown {
		t.Fatalf("a 19-character name runs as %q, past the %d Muse shows whole", full, museServerNameShown)
	}
	box := sandbox.NewFakeSandbox("b")
	long := strings.Repeat("b", 40)
	server := sandbox.MCPServer{URL: "http://e/mcp"}
	err := writeMuseServers(t.Context(), box, "/s", map[string]sandbox.MCPServer{readable: server, long: server}, nonce)
	if err != nil {
		t.Fatalf("names of 19 and 40 characters: %v", err)
	}
	for _, name := range []string{readable, long} {
		if _, err := box.ReadFile(t.Context(), "/s/"+name+"-"+nonce+".json"); err != nil {
			t.Errorf("the server %s is not written under its name and the run's suffix: %v", name, err)
		}
	}
}
