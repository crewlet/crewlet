package executor

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// grokArg is the value the script hands flag, unquoted, "" where it hands none.
func grokArg(script, flag string) string {
	m := regexp.MustCompile(` ` + regexp.QuoteMeta(flag) + ` ('[^']*'|[^ ]+)`).FindStringSubmatch(script)
	if m == nil {
		return ""
	}
	return strings.Trim(m[1], "'")
}

// A GROK RUN STARTS WITH WHAT DRIVES IT, AND A WEB-DENIED ONE WITH NO WEB TOOL:
// every run asks nothing, its turn cap, model and effort Grok's own; it removes
// exactly the 1.0.92 tools a headless run cannot use or that break the runner
// and the scheduler's three, and the two web tools with them where the entry
// denies web; spawn_subagent and the image tools stay; and it pins only what
// the runner needs, Grok keeping its defaults, its fetch's among them.
//
// Mutations, each red: drop grokWebTools from the deny list; pin GROK_WEB_FETCH
// either way; drop --max-turns; drop a grokDenied entry; deny
// spawn_subagent; pass --no-subagents; add a pin.
func TestAWebDeniedGrokRunHasNoWebTool(t *testing.T) {
	t.Parallel()
	paths := Paths{Home: "/home/box"}
	for _, denied := range []bool{true, false} {
		req := sandbox.RunRequest{Brief: "x", Model: "grok-4.6", Effort: "high", WebDenied: denied,
			Limits: sandbox.Limits{MaxTurns: 7}}
		script := grokScript(req, Session{ID: "s"}, paths, time.Second)
		want := []string{"ask_user_question", "enter_plan_mode", "exit_plan_mode", "workflow",
			"scheduler_create", "scheduler_delete", "scheduler_list"}
		if denied {
			want = append(want, "web_search", "web_fetch")
		}
		if tools := strings.Split(grokArg(script, "--disallowed-tools"), ","); !slices.Equal(tools, want) {
			t.Errorf("web denied %t: the run removes %v, want %v", denied, tools, want)
		}
		for flag, value := range map[string]string{"--max-turns": "7", "-m": "grok-4.6", "--reasoning-effort": "high",
			"--agent": paths.GrokAgent(), "--output-format": "streaming-json"} {
			if got := grokArg(script, flag); got != value {
				t.Errorf("%s is %q, want %q", flag, got, value)
			}
		}
		for _, flag := range []string{"--always-approve", "--no-auto-update"} {
			if !strings.Contains(script, " "+flag+" ") {
				t.Errorf("the run starts without %s", flag)
			}
		}
		for _, flag := range []string{"--no-plan", "--no-subagents"} {
			if strings.Contains(script, " "+flag+" ") {
				t.Errorf("the run starts with %s, a limit a person's Grok does not run under", flag)
			}
		}
		pins := map[string]string{}
		for _, p := range (Grok{}).Pins(t.Context(), req, paths) {
			pins[p.Name] = p.Value
		}
		wantPins := map[string]string{"GROK_DISABLE_AUTOUPDATER": "1", "GROK_TURN_SUMMARY": "0"}
		if !maps.Equal(pins, wantPins) {
			t.Errorf("web denied %t: pins %v, want %v", denied, pins, wantPins)
		}
	}
}

// A GROK RUN'S NOTE HELPER IS EXECUTABLE BEFORE GROK STARTS: every hook runs
// it by its path, and the box writes it as a plain file, so a run wired to
// its notes or its skills' loads makes it executable first; one wired to
// neither has no helper to.
//
// Mutations, each red: drop the chmod; make it after the harness starts.
func TestAGrokRunsNoteHelperIsExecutableBeforeGrokStarts(t *testing.T) {
	t.Parallel()
	paths := Paths{Home: "/home/box"}
	chmod := "chmod 0755 " + shellword.Quote(paths.GrokNoteHelper())
	for name, req := range map[string]sandbox.RunRequest{
		"notes": {Steer: sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: "http://e/steer"}},
		"loads": {SkillLoads: sandbox.SkillWiring{Server: "crewlet", Tool: "skill_loaded", URL: "http://e/skill"}},
	} {
		script := grokScript(req, Session{ID: "s"}, paths, time.Second)
		at, start := strings.Index(script, chmod), strings.Index(script, "grok --output-format")
		if at < 0 || start < 0 || at > start {
			t.Errorf("%s: the helper is made executable at %d, the harness started at %d", name, at, start)
		}
	}
	if script := grokScript(sandbox.RunRequest{}, Session{ID: "s"}, paths, time.Second); strings.Contains(script, chmod) {
		t.Error("a run wired to nothing makes a helper executable")
	}
}

// grokFakeBox is a box whose machine is x86_64 and whose every other command
// succeeds, for WriteConfig to write a note helper into.
func grokFakeBox() *sandbox.FakeSandbox {
	b := sandbox.NewFakeSandbox("box-1")
	b.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if cmd == "uname -m" {
			return sandbox.ExecResult{Stdout: "x86_64\n"}, nil
		}
		return sandbox.ExecResult{}, nil
	}
	return b
}

// readBox is path's content in b.
func readBox(t *testing.T, b sandbox.Sandbox, path string) string {
	t.Helper()
	raw, err := b.ReadFile(t.Context(), path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}

// EVERY GROK RUN NAMES ITS SERVERS UNDER A NONCE OF ITS OWN, AND ITS SHELL
// KEEPS ITS SIGN-IN: the settings' servers and the agent file's tool
// instruction name one nonce, new every run, and the settings set no shell
// policy, so the model's shell starts with the sign-in as a person's does.
//
// Mutations, each red: write a shell policy; draw the nonce twice in one run;
// draw one nonce for every run.
func TestEveryGrokRunNamesItsServersUnderANonceOfItsOwn(t *testing.T) {
	t.Parallel()
	req := sandbox.RunRequest{Brief: "x",
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://e/mcp"}}}
	server := regexp.MustCompile(`\[mcp_servers\.crewlet_([0-9a-f]{6})\]`)
	var nonces []string
	for range 2 {
		b := grokFakeBox()
		paths := PathsFor(b)
		if _, err := (Grok{}).WriteConfig(t.Context(), b, req, Session{ID: "s"}, paths); err != nil {
			t.Fatalf("WriteConfig: %v", err)
		}
		config := readBox(t, b, paths.GrokConfig())
		if strings.Contains(config, "shell_environment_policy") {
			t.Errorf("the settings start the model's shell without what a person's has:\n%s", config)
		}
		m := server.FindStringSubmatch(config)
		if m == nil {
			t.Fatalf("the settings name no server under a nonce:\n%s", config)
		}
		if agent := readBox(t, b, paths.GrokAgent()); !strings.Contains(agent, "`crewlet_"+m[1]+"`") {
			t.Errorf("the agent file names another server than crewlet_%s:\n%s", m[1], agent)
		}
		nonces = append(nonces, m[1])
	}
	if nonces[0] == nonces[1] {
		t.Errorf("two runs named their servers under one nonce, %s", nonces[0])
	}
}

// A GROK RUN REPORTS EACH SKILL IT READS FROM WHERE ITS LAUNCH WROTE IT: each
// skill is written at <skills>/<listed name>/SKILL.md, the helper's config
// names that directory under the key the helper reads (`skillsDir`), and the
// PostToolUse hook reports loads, each skill's digest as written on its line,
// beside the notes, or alone (`-loads-only`, the run's one hook) where the run
// takes no notes; every hook names the file the main session is named in.
//
// Mutations, each red: pass "" for the skills directory; drop -skill-loads,
// -skill-digest or -loads-only; drop -thread-file from a hook that only
// reports; rename the config's key.
func TestAGrokRunReportsEachSkillItReadsFromWhereItsLaunchWroteIt(t *testing.T) {
	t.Parallel()
	const bridge = "http://127.0.0.1:7/mcp/run-token-placeholder"
	wired := sandbox.RunRequest{Brief: "x", Skills: []sandbox.Skill{{Name: "mcp-github", Description: "GitHub.",
		Body: "# GitHub\n"}},
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: bridge}},
		SkillLoads: sandbox.SkillWiring{Server: "crewlet", Tool: "skill_loaded", URL: bridge + "/skill"},
		Steer:      sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: bridge + "/steer"}}
	loadsOnly := wired
	loadsOnly.Steer = sandbox.SteerWiring{}
	for name, tc := range map[string]struct {
		req   sandbox.RunRequest
		hooks []string
	}{
		"with notes":    {wired, []string{"PostToolUse", "PostToolUseFailure", "Stop"}},
		"without notes": {loadsOnly, []string{"PostToolUse"}},
	} {
		b := grokFakeBox()
		paths := PathsFor(b)
		if _, err := (Grok{}).WriteConfig(t.Context(), b, tc.req, Session{ID: "s"}, paths); err != nil {
			t.Fatalf("%s: WriteConfig: %v", name, err)
		}
		var take map[string]any
		if err := json.Unmarshal([]byte(readBox(t, b, paths.GrokTakeConfig())), &take); err != nil ||
			take["skillsDir"] != paths.GrokSkills() {
			t.Errorf("%s: the helper's config is %v (%v), want skillsDir %s", name, take, err, paths.GrokSkills())
		}
		readBox(t, b, paths.GrokSkills()+"/crewlet:mcp-github/SKILL.md")
		var read struct {
			Hooks map[string][]struct{ Hooks []struct{ Command string } }
		}
		if _, err := toml.Decode(readBox(t, b, paths.GrokConfig()), &read); err != nil {
			t.Fatalf("%s: the settings are no TOML: %v", name, err)
		}
		got := slices.Sorted(maps.Keys(read.Hooks))
		for _, event := range got {
			command := read.Hooks[event][0].Hooks[0].Command
			want := shellword.Quote(paths.GrokTakeConfig()) + " '-thread-file' " + shellword.Quote(paths.GrokThread())
			if event == "PostToolUse" {
				want += " '-skill-loads'"
				for _, digest := range skillDigests(tc.req.Skills) {
					want += " '-skill-digest' " + shellword.Quote(digest)
				}
				if !tc.req.Steer.Set() {
					want += " '-loads-only'"
				}
			}
			if !strings.HasSuffix(command, want) {
				t.Errorf("%s: the %s hook runs %s, want it to end %q", name, event, command, want)
			}
		}
		if !slices.Equal(got, tc.hooks) {
			t.Errorf("%s: hooks %v, want %v", name, got, tc.hooks)
		}
	}
}

// GROK'S LIMIT WORDS ARE ITS PROFILE'S LIMIT MARKERS: the decoder's report and
// the profile's classification of it read one list.
//
// Mutation: add a word to either list alone.
func TestGrokLimitWordsAreItsProfilesLimitMarkers(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin(GrokName)
	if !ok {
		t.Fatal("no grok profile")
	}
	var markers []string
	for _, m := range p.LimitMarkers {
		markers = append(markers, m.Sentinel)
	}
	if !slices.Equal(slices.Sorted(slices.Values(markers)), slices.Sorted(slices.Values(grokLimitWords))) {
		t.Errorf("the profile's limit markers %q, the decoder's words %q", markers, grokLimitWords)
	}
}

// GROK IS WRITTEN FOR THE RELEASE ITS PROFILE NAMES: the runner's one release
// is the first release word of the profile's written_for.
//
// Mutation: word written_for so its first release word is no bare release.
func TestGrokIsWrittenForTheReleaseItsProfileNames(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin(GrokName)
	if grokSupported() != "1.0.92" || grokSupported() != harnessprofile.Release(p.WrittenFor) {
		t.Errorf("the runner is written for %q, its profile's written_for %q names %q", grokSupported(),
			p.WrittenFor, harnessprofile.Release(p.WrittenFor))
	}
}

// grokGateBox is a home for grokSessionGate's cases: the session's record in
// the box's own copy (its last line marked), an archive of the session carried
// to it, holding a link where linked says so, the renewal, and the configs a
// fresh session is wired with.
type grokGateBox struct {
	paths              Paths
	copy, carried, sum string
}

const grokGateSession, grokGateFresh = "11111111-2222-4333-8444-0000000000c1", "11111111-2222-4333-8444-0000000000c2"

func newGrokGateBox(t *testing.T, sh string, linked bool) grokGateBox {
	t.Helper()
	g := grokGateBox{paths: Paths{Home: t.TempDir()}}
	g.copy = filepath.Join(g.paths.GrokSessionsKey(), grokGateSession, "chat_history.jsonl")
	writeHere(t, g.copy)
	record := grokFixture(t, "plain.chat.jsonl")
	_ = os.WriteFile(g.copy, append(append([]byte(nil), record...), "{\"type\":\"user\",\"content\":\"BOX-COPY\"}\n"...), 0o600)
	src, member := t.TempDir(), "%2Fwriter/"+grokGateSession
	writeHere(t, filepath.Join(src, member, "chat_history.jsonl"))
	_ = os.WriteFile(filepath.Join(src, member, "chat_history.jsonl"), record, 0o600)
	if linked {
		if err := os.Symlink("/etc/hostname", filepath.Join(src, member, "linked")); err != nil {
			t.Fatal(err)
		}
	}
	g.carried = g.paths.SessionCarried()
	writeHere(t, g.carried)
	if out, err := exec.Command("tar", "-czf", g.carried, "-C", src, member).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, out)
	}
	sum, err := exec.Command(sh, "-c", "cksum < "+shellword.Quote(g.carried)).Output() //nolint:gosec // the test's own script
	if err != nil {
		t.Fatal(err)
	}
	g.sum = strings.TrimSpace(string(sum))
	rules, _ := grokRulesJSON("RENEWED")
	_ = os.WriteFile(g.paths.GrokRules(), rules, 0o600)
	for path, body := range map[string]string{g.paths.GrokConfig(): "OLD", g.paths.GrokConfigFresh(): "FRESH",
		g.paths.GrokTakeConfig(): "OLD-TAKE", g.paths.GrokTakeConfigFresh(): "FRESH-TAKE"} {
		writeHere(t, path)
		_ = os.WriteFile(path, []byte(body), 0o600)
	}
	return g
}

// read is path's content, "" where it is gone.
func (g grokGateBox) read(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

// A GROK SESSION CONTINUES IN ITS BOX ONLY AS ITS GATE SAYS: the box's copy is
// continued where the carried archive is one it already restored (its digest
// recorded), and the archive restored over it otherwise or where the
// relaunch replaces it, its digest then recorded; a box on another release
// starts the minted session and says why; an archive holding a link is not
// restored, the box's copy left whole; and a rewired run that starts the
// minted session is wired with the fresh configs, one that continues drops
// them.
//
// Mutations, each red: drop the release check; drop the digest check; drop the
// link check; drop either rewired mv; record no digest.
func TestAGrokSessionContinuesInItsBoxOnlyAsItsGateSays(t *testing.T) {
	t.Parallel()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	prereqtest.OnPath(t, "tar", "the case runs the box's script", "install tar")
	prereqtest.OnPath(t, "gzip", "the case runs the box's script", "install gzip")
	continued := grokGateSession + "|-r|yes|"
	for name, tc := range map[string]struct {
		version         string
		recorded        bool
		replaces        bool
		linked, rewired bool
		answer          string
		boxCopy         bool
		config          string
	}{
		"(a) an archive it restored already": {recorded: true, answer: continued, boxCopy: true},
		"(b) an archive it did not restore":  {answer: continued},
		"(c) a relaunch that replaces it":    {recorded: true, replaces: true, answer: continued},
		"(d) a box on another release": {version: "grok 1.0.93 (92384ee02e2d)", recorded: true, boxCopy: true,
			answer: grokGateFresh + "|-s|no|this box runs Grok Build 1.0.93, and a session continues only on 1.0.92, " +
				"the release this runner is written for"},
		"(e) an archive holding a link": {linked: true, boxCopy: true,
			answer: grokGateFresh + "|-s|no|its archive could not be restored"},
		"(f) a rewired run that starts afresh": {version: "grok 1.0.93 (x)", recorded: true, rewired: true,
			boxCopy: true, config: "FRESH"},
		"(f) a rewired run that continues": {recorded: true, rewired: true, answer: continued, boxCopy: true,
			config: "OLD"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGrokGateBox(t, sh, tc.linked)
			if tc.recorded {
				writeHere(t, g.paths.SessionDigests())
				_ = os.WriteFile(g.paths.SessionDigests(), []byte(grokGateSession+" "+g.sum+"\n"), 0o600)
			}
			version := cmp.Or(tc.version, "grok 1.0.92 (b06756fd6256)")
			gate := "v='" + version + "'\n" + grokSessionGate(Session{ID: grokGateSession, Continues: true,
				Fresh: grokGateFresh, Replaces: tc.replaces}, g.paths, tc.rewired) + `printf '%s|%s|%s|%s' "$s" "$sf" "$k" "$w"`
			out, err := exec.Command(sh, "-c", gate).Output() //nolint:gosec // the test's own script
			if err != nil || (tc.answer != "" && string(out) != tc.answer) {
				t.Fatalf("the gate answered %q (%v), want %q", out, err, tc.answer)
			}
			if got := strings.Contains(g.read(g.copy), "BOX-COPY"); got != tc.boxCopy {
				t.Errorf("the box's own copy is there %t, want %t", got, tc.boxCopy)
			}
			restored := !tc.boxCopy
			digests := g.read(g.paths.SessionDigests())
			if restored && !strings.Contains(digests, grokGateSession+" "+g.sum+"\n") {
				t.Errorf("the restored archive's digest is not recorded: %q", digests)
			}
			if tc.config != "" {
				if got := g.read(g.paths.GrokConfig()); got != tc.config {
					t.Errorf("the run's settings are %q, want %q", got, tc.config)
				}
				if take := g.read(g.paths.GrokTakeConfig()); take != tc.config+"-TAKE" {
					t.Errorf("the run's take config is %q, want %q", take, tc.config+"-TAKE")
				}
			}
		})
	}
}

// A SAVE OF A GROK SESSION IS PACKED AS BESIDE A WORKING HARNESS: its pack is
// held to claudeHeldTar's rule, and a record whose last line is still being
// written is packed, record first.
//
// Mutation: pack a save as beside a harness that has exited.
func TestASaveOfAGrokSessionIsPackedAsBesideAWorkingHarness(t *testing.T) {
	t.Parallel()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	paths := Paths{Home: t.TempDir()}
	const session = "11111111-2222-4333-8444-0000000000d1"
	record := filepath.Join(paths.GrokSessionsKey(), session, "chat_history.jsonl")
	writeHere(t, record)
	_ = os.WriteFile(record, append(grokFixture(t, "plain.chat.jsonl"), `{"type":"assistant","con`...), 0o600)
	writeHere(t, paths.WorkDir()+"/x")
	staged, dest := paths.WorkDir()+"/staged", paths.WorkDir()+"/save"
	save, err := (Grok{}).PackSave(paths, sessionRecord{id: session}, staged, dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(save, claudeHeldTar(paths.GrokSessions(), staged)) {
		t.Error("a save is not held to the rule of a pack beside a working harness")
	}
	if out, err := exec.Command(sh, "-c", save).CombinedOutput(); err != nil { //nolint:gosec // the test's own script
		t.Fatalf("the save: %v\n%s", err, out)
	}
	list, err := exec.Command("tar", "-tzf", dest).Output()
	if err != nil || !strings.HasPrefix(string(list), grokPathKey(paths.Home)+"/"+session+"/chat_history.jsonl\n") {
		t.Errorf("the save lists %q (%v), want the record first", list, err)
	}
}

// grokThreadBox is a box holding record as session's record.
func grokThreadBox(session string, record []byte) (*sandbox.FakeSandbox, Paths) {
	b := sandbox.NewFakeSandbox("b")
	paths := PathsFor(b)
	b.Put(paths.GrokSessionsKey()+"/"+session+"/chat_history.jsonl", string(record))
	return b, paths
}

// A GROK RECORD THAT CANNOT BE READ WHOLE CONFIRMS NOTHING: a line that is no
// record Grok writes, with a later line after it, fails the reading, which
// then answers neither held nor absent and names the line; the same line last,
// still being written, leaves the takes before it held.
//
// Mutation: read past a line that is no record.
func TestAnUnreadableGrokRecordConfirmsNothing(t *testing.T) {
	t.Parallel()
	const session = "11111111-2222-4333-8444-0000000000a2"
	lines := bytes.SplitAfter(grokFixture(t, "notes.chat.jsonl"), []byte("\n"))
	at := slices.IndexFunc(lines, func(l []byte) bool { return bytes.Contains(l, []byte("Context from PostToolUse hook")) })
	torn := []byte("{\"type\":\"assist\n")
	mid := slices.Concat(lines[:at+1], [][]byte{torn}, lines[at+1:])
	if _, err := grokHeld(t.Context(), bytes.NewReader(bytes.Join(mid, nil)), nil); err == nil {
		t.Error("a record with a line that is no record, a later line after it, read whole")
	}
	takes := []sandbox.AskedTake{{Take: "0f0e0d0c-0000-4000-8000-000000000001"}}
	b, paths := grokThreadBox(session, bytes.Join(mid, nil))
	reading := Grok{}.readMainThread(t.Context(), b, paths, sandbox.MainThreadAsk{Session: session, Takes: takes,
		Ended: true})
	if r := reading.Receipts; len(r.Held) != 0 || len(r.Absent) != 0 || !strings.Contains(r.Why, "its line "+
		strconv.Itoa(at+2)) {
		t.Errorf("an unreadable record answered %+v", r)
	}
	b, paths = grokThreadBox(session, append(bytes.Join(lines, nil), torn...))
	reading = Grok{}.readMainThread(t.Context(), b, paths, sandbox.MainThreadAsk{Session: session, Takes: takes,
		Ended: true})
	if len(reading.Receipts.Held) != 1 {
		t.Errorf("a record whose last line is being written holds %+v, want the take before it", reading.Receipts)
	}
}

// A GROK TAKE IS ABSENT ONLY ONCE ITS JOB IS OVER: a take the record does not
// show is undecided while the job runs and absent once it ended, beside one
// the record holds.
//
// Mutation: answer absent while the job runs.
func TestAGrokTakeIsAbsentOnlyOnceItsJobIsOver(t *testing.T) {
	t.Parallel()
	const session = "11111111-2222-4333-8444-0000000000a2"
	b, paths := grokThreadBox(session, grokFixture(t, "notes.chat.jsonl"))
	const held, unseen = "0f0e0d0c-0000-4000-8000-000000000001", "0f0e0d0c-0000-4000-8000-0000000000ee"
	for _, ended := range []bool{false, true} {
		r := Grok{}.readMainThread(t.Context(), b, paths, sandbox.MainThreadAsk{Session: session, Ended: ended,
			Takes: []sandbox.AskedTake{{Take: held}, {Take: unseen}}}).Receipts
		wantAbsent := []string(nil)
		if ended {
			wantAbsent = []string{unseen}
		}
		if !slices.Equal(r.Held, []string{held}) || !slices.Equal(r.Absent, wantAbsent) {
			t.Errorf("ended %t: held %v, absent %v; want [%s], %v", ended, r.Held, r.Absent, held, wantAbsent)
		}
	}
}

// ONLY A HOOK OF THE RUN'S OWN SETTINGS HOLDS A GROK TAKE: a reminder from a
// hook Grok names under another scope than the home's (`user`), a
// repository's, holds no token it ends with, though a model's answer follows.
//
// Mutation: match a reminder by its event's words alone.
func TestOnlyAHookOfTheRunsOwnSettingsHoldsAGrokTake(t *testing.T) {
	t.Parallel()
	token := claudeTakeToken("0f0e0d0c-0000-4000-8000-0000000000bb")
	reminder, _ := json.Marshal(map[string]any{"type": "user", "synthetic_reason": "system_reminder",
		"content": []map[string]string{{"type": "text", "text": "<system-reminder>\nContext from PostToolUse hook " +
			"'project:post_tool_use[0].hooks[0]':\nA note.\n\n" + token + grokReminderTail}}})
	answer, _ := json.Marshal(map[string]any{"type": "assistant", "content": "Done."})
	held, err := grokHeld(t.Context(), bytes.NewReader(slices.Concat(grokFixture(t, "plain.chat.jsonl"), reminder, []byte("\n"),
		answer, []byte("\n"))), nil)
	if err != nil || held[token] {
		t.Errorf("a repository's hook held %v (%v)", held, err)
	}
}

// A GROK SKILL'S DIGEST IS WHAT GROK'S READ OF IT RETURNS: the note helper counts
// a load only where the text a read_file returned (`raw_output`) holds the
// -skill-digest the runner names, so the SKILL.md the runner writes and 1.0.92's
// whole read of it must hash alike (the helper's capture, of a skill the runner
// wrote).
//
// Mutation: digest anything but the file [skillFile] makes.
func TestAGrokSkillsDigestIsWhatGroksReadOfItReturns(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "notehelper", "testdata", "grok", "1.0.92",
		"posttooluse-skill-read.json"))
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		Response struct {
			Content struct {
				Read string `json:"raw_output"`
			} `json:"FileContent"`
		} `json:"tool_response"`
	}
	if err = json.Unmarshal(raw, &in); err != nil || in.Response.Content.Read == "" {
		t.Fatalf("the captured read holds no raw_output (%v)", err)
	}
	sum := sha256.Sum256([]byte(in.Response.Content.Read))
	want := "crewlet:probe-skill=" + hex.EncodeToString(sum[:])
	skill := sandbox.Skill{Name: "probe-skill", Description: "Use for probes.", Body: "Step one.\n"}
	if got := skillDigests([]sandbox.Skill{skill}); len(got) != 1 || got[0] != want {
		t.Errorf("the runner names %v, want %s: Grok's whole read of the skill as written", got, want)
	}
}

// A GROK RUN PUTS THE BOX'S OWN SETTINGS BACK ONCE GROK EXITS: the kept copy
// replaces the run's settings, bridge and hooks with them, and is removed, so
// an edit to the box's settings between runs stands; an empty copy (the box
// had none) leaves none; with no copy kept the settings are left as they are.
// The script does this after the harness, before the record is read.
//
// Mutations, each red: drop the restore from the script; keep the copy after
// restoring; write an empty file where the box had none.
func TestAGrokRunPutsTheBoxsOwnSettingsBackOnceGrokExits(t *testing.T) {
	t.Parallel()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	for _, tc := range []struct {
		name, kept, want string
		keep, absent     bool
	}{
		{name: "the box's own", kept: "model = \"grok-4.6\"\n", keep: true, want: "model = \"grok-4.6\"\n"},
		{name: "none", kept: "", keep: true, absent: true},
		{name: "no copy kept", want: "RUN"},
	} {
		paths := Paths{Home: t.TempDir()}
		writeHere(t, paths.GrokConfig())
		_ = os.WriteFile(paths.GrokConfig(), []byte("RUN"), 0o600)
		if tc.keep {
			writeHere(t, paths.GrokConfigBox())
			_ = os.WriteFile(paths.GrokConfigBox(), []byte(tc.kept), 0o600)
		}
		if out, err := exec.Command(sh, "-c", grokRestoreConfig(paths)).CombinedOutput(); err != nil { //nolint:gosec // the runner's own script
			t.Fatalf("%s: %v %s", tc.name, err, out)
		}
		got, err := os.ReadFile(paths.GrokConfig())
		if tc.absent != os.IsNotExist(err) || !tc.absent && string(got) != tc.want {
			t.Errorf("%s: the settings are %q (%v), want %q (absent: %v)", tc.name, got, err, tc.want, tc.absent)
		}
		if _, err := os.Stat(paths.GrokConfigBox()); !os.IsNotExist(err) {
			t.Errorf("%s: the kept copy is still there (%v)", tc.name, err)
		}
	}
	script := grokScript(sandbox.RunRequest{Brief: "x"}, Session{ID: "s"}, Paths{Home: "/home/box"}, time.Second)
	restore := strings.Index(script, grokRestoreConfig(Paths{Home: "/home/box"}))
	if restore < 0 || restore < strings.Index(script, `wait "$harness"`) ||
		restore > strings.Index(script, grokRecordMark) {
		t.Errorf("the script puts the settings back at %d, want after the harness and before the record:\n%s",
			restore, script)
	}
}
