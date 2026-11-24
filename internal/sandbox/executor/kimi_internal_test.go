package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// kimiFixture is testdata/kimi/<release>/<name>: a real Kimi Code run's stream
// (the driver's stdout, then what the run's script adds, [kimiUsage]) or a
// session's wire.jsonl, against a stand-in model (KIMI_MODEL_* to an
// OpenAI-compatible stub) and a stand-in bridge. Every byte is as the harness
// and the driver wrote it, but that a wire.jsonl's tool catalogue lines
// (`llm.tools_snapshot`, `tool_select.*`) are left out and its bound system
// prompt reads `[the vendor's text, not kept in this fixture]`.
func kimiFixture(t *testing.T, release, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "kimi", release, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// kimiReleases is every release the runner was measured on: the one Crewlet
// supports.
var kimiReleases = []string{kimiSupported}

// kimiRead decodes a whole fixture stream.
func kimiRead(t *testing.T, release, name string) (sandbox.Result, *kimiEvents) {
	t.Helper()
	dec := &kimiEvents{}
	if err := eachLine(bytes.NewReader(kimiFixture(t, release, name)), dec, maxLineBytes); err != nil {
		t.Fatal(err)
	}
	return dec.Result(), dec
}

// A KIMI RUN'S ANSWER IS ITS LAST ASSISTANT MESSAGE, AND ITS SPEND THE RECORDS
// ITS SCRIPT ADDED: a run steered a note while its tool ran answers the note's
// turn, is charged both requests as the wire log recorded them, the cache's
// share apart, and counts each step a request.
//
// Mutation: read the answer off the first turn, or sum KAP's turn usage instead
// of the records after the mark — the case fails.
func TestAKimiRunsAnswerAndSpendAreItsOwn(t *testing.T) {
	for _, release := range kimiReleases {
		res, dec := kimiRead(t, release, "steered.stream.jsonl")
		if !res.Success || res.Text != "answer-2" {
			t.Fatalf("%s: result = %+v, want the success the note's turn answered", release, res)
		}
		if res.Turns != 2 || dec.reportedWindow() != 262144 {
			t.Errorf("%s: turns %d, window %d; want the two requests and the env model's 262144",
				release, res.Turns, dec.reportedWindow())
		}
		if !strings.Contains(res.Transcript, "mcp__crewlet__slow") || !strings.Contains(res.Transcript, "[steer]") {
			t.Errorf("%s: the transcript names no call or no note:\n%s", release, res.Transcript)
		}
	}
	// 2.7.2: 100 then 60 not cached and 40 read from it, 7 and 3 out.
	res, _ := kimiRead(t, "2.7.2", "steered.stream.jsonl")
	if res.InputTokens != 200 || res.CacheReadTokens != 40 || res.CacheWriteTokens != 0 || res.OutputTokens != 10 ||
		res.CostUSD != 0 {
		t.Errorf("charged %+v, want input 200 (the cache's 40 inside it), output 10, no price", res)
	}
}

// A RUN THIS RUNNER LAUNCHED END TO END (the engine's own rendered script,
// profile, MCP servers, driver and settings, in a box against the stand-ins,
// a note sent while its tool ran) reads as its answer and its own spend off the
// result file its script left, and the note as held off its session's record.
//
// Mutation: read the script's usage mark as no mark, or confirm a note off
// another prompt_id — the case fails.
func TestALaunchedKimiRunReadsAsItsAnswerSpendAndNote(t *testing.T) {
	for _, release := range kimiReleases {
		res := (Kimi{}).Parse(kimiFixture(t, release, "launched.result.jsonl"))
		if !res.Success || res.Text != "ANSWER saw note" {
			t.Fatalf("%s: result = %+v, want the answer to the note", release, res)
		}
		if res.InputTokens != 200 || res.CacheReadTokens != 40 || res.OutputTokens != 10 {
			t.Errorf("%s: charged in %d (cache %d), out %d; want 200 (40), 10", release, res.InputTokens,
				res.CacheReadTokens, res.OutputTokens)
		}
		if !strings.Contains(res.Transcript, "[steer]") {
			t.Errorf("%s: the transcript does not show the note:\n%s", release, res.Transcript)
		}
		held, err := kimiNotesHeld(t.Context(), bytes.NewReader(kimiFixture(t, release, "launched.wire.jsonl")),
			kimiServed(kimiSteeredInbox), nil)
		if err != nil || !slices.Equal(held, []string{kimiSteeredInbox}) {
			t.Fatalf("%s: held = %v (%v), want the note", release, held, err)
		}
	}
}

// A SUBAGENT'S REQUESTS ARE THE RUN'S SPEND TOO, read off its own wire.jsonl,
// which KAP's turns of the main agent leave out (measured: its turn said 200
// in); its steps are not the main agent's turns.
//
// Mutation: read only the main agent's records — the case fails.
func TestAKimiSubagentsRequestsAreChargedToTheRun(t *testing.T) {
	res, _ := kimiRead(t, "2.7.2", "subagent.stream.jsonl")
	if !res.Success || res.Text != "final-3" {
		t.Fatalf("result = %+v, want the main agent's last answer", res)
	}
	if res.InputTokens != 300 || res.OutputTokens != 13 || res.Turns != 2 {
		t.Errorf("charged in %d out %d over %d turns; want 300 and 13, the subagent's 100 and 3 among them, "+
			"over the main agent's 2", res.InputTokens, res.OutputTokens, res.Turns)
	}
}

// A USAGE RECORD BEFORE THE SCRIPT'S MARK IS NOT CHARGED, and a stream with no
// mark states no spend: only the script adds records, after it.
//
// Mutation: count a record whatever came before it — the case fails.
func TestAKimiUsageRecordBeforeTheMarkIsNotCharged(t *testing.T) {
	stream := kimiFixture(t, "2.7.2", "steered.stream.jsonl")
	head, _, ok := bytes.Cut(stream, []byte(`{"type":"`+kimiUsageMark+`"}`))
	if !ok {
		t.Fatal("the fixture holds no mark")
	}
	forged := `{"type":"usage.record","agentId":"main","usage":{"inputOther":999,"output":9},"usageScope":"turn"}` + "\n"
	unmarked := &kimiEvents{}
	if err := eachLine(bytes.NewReader(append(slices.Clone(head), forged...)), unmarked, maxLineBytes); err != nil {
		t.Fatal(err)
	}
	if _, stated := unmarked.runSpend(); stated {
		t.Fatal("a stream with no mark stated a spend")
	}
	marked := &kimiEvents{}
	stream = append(append(slices.Clone(head), forged...), `{"type":"`+kimiUsageMark+`"}`+"\n"...)
	if err := eachLine(bytes.NewReader(stream), marked, maxLineBytes); err != nil {
		t.Fatal(err)
	}
	if spent, stated := marked.runSpend(); !stated || spent != (runSpend{}) {
		t.Fatalf("spend = %+v (stated %v), want nothing of the record before the mark", spent, stated)
	}
}

// A NOTE STEERED AT IDLE KEEPS THE RUN GOING: its turn's answer is the run's.
//
// Mutation: read the first turn's answer as the run's — the case fails.
func TestAKimiNoteAtIdleKeepsTheRunGoing(t *testing.T) {
	res, _ := kimiRead(t, "2.7.2", "idle-note.stream.jsonl")
	if !res.Success || res.Text != "second-2" || !strings.Contains(res.Transcript, "[steer]") {
		t.Fatalf("result = %+v, want the note's turn's answer", res)
	}
}

// A KIMI RUN THAT MET A RATE LIMIT FAILS AND REPORTS IT as the record's own
// turn end, the line the kimi-code profile's limit events read (`in: record`).
//
// Mutation: report KAP's frame instead of the record's line — the profile's
// matcher misses it and the case fails.
func TestAKimiRateLimitIsReportedAsTheRecordsLine(t *testing.T) {
	for _, release := range kimiReleases {
		res, _ := kimiRead(t, release, "ratelimit.stream.jsonl")
		if res.Success || !strings.Contains(res.Error, "5-hour usage limit") {
			t.Fatalf("%s: result = %+v, want the provider's words as the failure", release, res)
		}
		for _, line := range []string{res.Limits, res.Event} {
			if !strings.HasPrefix(line, `{"type":"turn.ended","agentId":"main"`) ||
				!strings.Contains(line, `"code":"provider.rate_limit"`) {
				t.Errorf("%s: line = %s, want the record's turn end naming the rate limit", release, line)
			}
		}
	}
}

// A KIMI RUN THAT REACHED ITS TURN CAP READS AS CAPPED, off its turn's
// `max_steps` end, never as a plain failure.
//
// Mutation: read `loop.max_steps_exceeded` as a failure — the case fails.
func TestAKimiRunStoppedAtItsCapReadsAsCapped(t *testing.T) {
	for _, release := range kimiReleases {
		res, _ := kimiRead(t, release, "capped.stream.jsonl")
		if res.Success || !res.RoundCapped || !strings.Contains(res.Error, "maxSteps=1") {
			t.Fatalf("%s: result = %+v, want the cap", release, res)
		}
	}
}

// A KIMI RUN PAST ITS MODEL'S CONTEXT READS AS EXHAUSTED: the turn ends on
// `compaction.failed` with APIContextOverflowError's words.
//
// Mutation: read the overflow off the code alone — the case fails.
func TestAKimiRunPastItsContextReadsAsExhausted(t *testing.T) {
	res, _ := kimiRead(t, "2.7.2", "overflow.stream.jsonl")
	if res.Success || !res.ContextExhausted || res.Limits != "" {
		t.Fatalf("result = %+v, want a context exhausted, and no limit", res)
	}
}

// A KIMI RUN ENDED ON REQUEST, OR KILLED BEFORE ITS DRIVER SAID HOW IT ENDED,
// FAILS: the first exits 143 with `asked`, the second leaves no `crewlet.end`.
//
// Mutation: read a stream with no end as a success — the second row fails.
func TestAKimiRunThatDidNotFinishFails(t *testing.T) {
	for _, tc := range []struct{ name, says string }{
		{"ended.stream.jsonl", "ended on request"},
		{"killed-in-gap.stream.jsonl", "stopped before it reported"},
	} {
		res, _ := kimiRead(t, "2.7.2", tc.name)
		if res.Success || !strings.Contains(res.Error, tc.says) {
			t.Errorf("%s: result = %+v, want a failure saying %q", tc.name, res, tc.says)
		}
	}
}

// kimiServed is a take per inbox id.
func kimiServed(inboxes ...string) []ServedTake {
	out := make([]ServedTake, len(inboxes))
	for i, inbox := range inboxes {
		out[i] = ServedTake{InboxID: inbox}
	}
	return out
}

// The inbox ids the fixtures' notes were steered under.
var (
	kimiSteeredInbox  = strings.Repeat("a", 32)
	kimiCompactedNote = strings.Repeat("d", 32)
	kimiKilledInbox   = strings.Repeat("e", 32)
	kimiInterrupted   = strings.Repeat("c", 32)
)

// A KIMI NOTE IS CONFIRMED ONLY OFF THE SESSION'S OWN RECORD, ONCE A REQUEST
// CARRIED IT PLAIN (ADR-0177): its line then a request's usage record confirms
// it; a kill between the two does not, until a resumed request carried it; a
// compaction between them does not, though the summary request read it; a
// request cut short by the note does not, until the next one answered.
//
// Mutation: confirm a note off its line alone (the killed row fails), count a
// compaction's own `usageScope: session` record or let a compaction pass (the
// compacted row fails), or clear nothing at a compaction — the case fails.
func TestAKimiNoteIsConfirmedOnlyOnceARequestCarriedItPlain(t *testing.T) {
	for _, release := range kimiReleases {
		for _, tc := range []struct {
			wire, inbox string
			held        bool
		}{
			{"steered.wire.jsonl", kimiSteeredInbox, true},
			{"killed-in-gap.wire.jsonl", kimiKilledInbox, false},
			{"resumed.wire.jsonl", kimiKilledInbox, true},
			{"compacted.wire.jsonl", kimiCompactedNote, false},
			{"interrupted.wire.jsonl", kimiInterrupted, true},
		} {
			raw, err := os.ReadFile(filepath.Join("testdata", "kimi", release, tc.wire))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			held, err := kimiNotesHeld(t.Context(), bytes.NewReader(raw), kimiServed(tc.inbox, "never"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(held, tc.inbox); got != tc.held {
				t.Errorf("%s %s: held %v, want %v", release, tc.wire, got, tc.held)
			}
			if slices.Contains(held, "never") {
				t.Errorf("%s %s: a note never steered is held", release, tc.wire)
			}
		}
	}
}

// THE RECORD ANSWERS ITS TAKES IN SERVE ORDER, ONCE EACH, and a note's line
// cut off at the file's end proves nothing: Kimi was still writing it.
//
// Mutation: answer in record order, or take the unended last line — the case
// fails.
func TestAKimiRecordAnswersInServeOrderAndNotOffATornLine(t *testing.T) {
	note := func(inbox string) string {
		return `{"type":"context.append_message","agentId":"main","message":{"role":"user","content":[],"id":"` +
			KimiNoteID(inbox) + `"}}` + "\n"
	}
	usage := `{"type":"usage.record","agentId":"main","usage":{"inputOther":1},"usageScope":"turn"}`
	wire := note("one") + note("two") + usage + "\n"
	held, err := kimiNotesHeld(t.Context(), strings.NewReader(wire), kimiServed("two", "one", "two"), nil)
	if err != nil || !slices.Equal(held, []string{"two", "one"}) {
		t.Fatalf("held = %v (%v), want two then one, once each", held, err)
	}
	torn := note("one") + usage
	if held, err := kimiNotesHeld(t.Context(), strings.NewReader(torn), kimiServed("one"), nil); err != nil || len(held) != 0 {
		t.Fatalf("held = %v (%v) off a record whose last line was cut off", held, err)
	}
}

// A LINE READ ONLY IN PART COUNTS BY ITS TYPE: a compaction whose other fields
// do not decode still stands between a note and the request after it, and a
// usage record whose fields do not decode confirms nothing.
//
// Mutation: skip a line that does not decode whole — the compaction row fails.
func TestAKimiLineReadOnlyInPartCountsByItsType(t *testing.T) {
	note := `{"type":"context.append_message","message":{"role":"user","id":"` + KimiNoteID("one") + `"}}` + "\n"
	for name, between := range map[string]string{
		"a compaction":   `{"type":"context.apply_compaction","message":7}` + "\n",
		"a usage record": `{"type":"usage.record","usageScope":["turn"]}` + "\n",
	} {
		usage := `{"type":"usage.record","usageScope":"turn"}` + "\n"
		wire := note + between
		if name == "a compaction" {
			wire += usage
		}
		held, err := kimiNotesHeld(t.Context(), strings.NewReader(wire), kimiServed("one"), nil)
		if err != nil || len(held) != 0 {
			t.Errorf("%s that does not decode whole: held = %v (%v), want none", name, held, err)
		}
	}
}

// THE BOX'S RECORD IS FOUND UNDER WHICHEVER WORKSPACE DIRECTORY HOLDS THE
// SESSION, and a session the box holds none or two of decides nothing.
//
// Mutation: open a fixed workspace path, or take the first of two — the case
// fails.
func TestAKimiNoteIsConfirmedOffTheBoxsRecord(t *testing.T) {
	const session = "session_6752b286-da65-4fa6-b177-7c428c2282fe"
	box := sandbox.NewFakeSandbox("b")
	sessions := PathsFor(box).KimiSessions()
	record := sessions + "/wd_wk_221ce7ca232a/" + session + "/agents/main/wire.jsonl"
	box.Put(record, string(kimiFixture(t, "2.7.2", "steered.wire.jsonl")))
	found := record
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if !strings.Contains(cmd, shellword.Quote(sessions)+"/*/"+session+"/agents/main/wire.jsonl") {
			t.Errorf("the record was looked for with %q", cmd)
		}
		return sandbox.ExecResult{Stdout: found}, nil
	}
	held, err := (Kimi{}).notesHeld(t.Context(), box, Session{ID: session}, kimiServed(kimiSteeredInbox))
	if err != nil || !slices.Equal(held, []string{kimiSteeredInbox}) {
		t.Fatalf("held = %v (%v), want the note", held, err)
	}
	for _, out := range []string{"", record + "\n" + record + "2"} {
		found = out
		if _, err := (Kimi{}).notesHeld(t.Context(), box, Session{ID: session}, kimiServed(kimiSteeredInbox)); err == nil {
			t.Errorf("records %q decided the take", out)
		}
	}
	if _, err := (Kimi{}).notesHeld(t.Context(), box, Session{ID: "../x"}, nil); err == nil {
		t.Error("a session id no Kimi run names was looked for")
	}
}

// A KIMI RUN IS STARTED ONLY ON THE SUPPORTED RELEASE OR A NEWER ONE, ON NODE,
// AND ITS SCRIPT IS SHELL: a box whose release is older is refused naming
// 2.7.2, and the script parses.
//
// Mutation: lower the floor, or drop the node check — the case fails.
func TestAKimiRunIsStartedOnlyOnItsFloor(t *testing.T) {
	script := kimiScript(sandbox.RunRequest{}, Session{}, PathsFor(sandbox.NewFakeSandbox("b")), "/s.json", sandbox.JobStopGrace)
	for _, want := range []string{"if ! at 2 7 2; then", "older than 2.7.2, the release Crewlet supports", "command -v node"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	sh := prereqtest.OnPath(t, "sh", "the case parses the box's script", "install a POSIX shell")
	if out, err := exec.Command(sh, "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("the script does not parse: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		printed string
		starts  bool
	}{{"2.7.0", false}, {"2.7.1", false}, {"2.7.2", true}, {"3.0.0-canary.1", true}, {"", true}} {
		check := strings.Replace(kimiRelease, "$(kimi --version < /dev/null 2> /dev/null | head -n 1)",
			shellword.Quote(tc.printed), 1) + "at " + floorArgs(kimiSupported)
		err := exec.Command(sh, "-c", check).Run()
		if starts := err == nil; starts != tc.starts {
			t.Errorf("release %q: starts %v, want %v", tc.printed, starts, tc.starts)
		}
	}
}

// THE SCRIPT ADDS THE SESSION'S OWN RECORDS AFTER ITS MARK: every agent's usage
// records and turn ends of the session the driver reported, nothing of another
// session, and nothing for a report that is no Kimi session id.
//
// Mutation: read only agents/main, or glob without the reported id — the case
// fails.
func TestKimisScriptAddsTheReportedSessionsRecords(t *testing.T) {
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	prereqtest.OnPath(t, "awk", "the case runs the box's script", "install awk")
	home := t.TempDir()
	paths := Paths{Home: home}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record := func(agent string, in int) string {
		return `{"type":"usage.record","agentId":"` + agent + `","usage":{"inputOther":` + strings.Repeat("1", in) +
			`},"usageScope":"turn"}` + "\n"
	}
	ws := paths.KimiSessions() + "/wd_wk_0123456789ab/"
	write(ws+"session_a/agents/main/wire.jsonl", `{"type":"metadata"}`+"\n"+record("main", 1)+
		`{"type":"turn.ended","agentId":"main","turnId":0,"reason":"completed"}`+"\n")
	write(ws+"session_a/agents/agent-0/wire.jsonl", record("agent-0", 2))
	write(ws+"session_b/agents/main/wire.jsonl", record("main", 3))
	run := func(report string) string {
		t.Helper()
		write(paths.SessionReport(), report)
		out, err := exec.Command(sh, "-c", "s=\n"+kimiSessionOf(paths)+kimiUsage(paths)).Output()
		if err != nil {
			t.Fatalf("the usage part of the script failed: %v", err)
		}
		return string(out)
	}
	got := run("session_a\n")
	if !strings.HasPrefix(got, `{"type":"crewlet.usage"}`+"\n") || strings.Count(got, "usage.record") != 2 ||
		!strings.Contains(got, `"agent-0"`) || !strings.Contains(got, "turn.ended") || strings.Contains(got, ":111}") {
		t.Errorf("the script added:\n%s\nwant the mark, session_a's two records and its turn end", got)
	}
	if got := run("../session_b\n"); got != `{"type":"crewlet.usage"}`+"\n" {
		t.Errorf("a report that is no session id added:\n%s", got)
	}
}

// A KIMI RUN IS PINNED TO ITS HOME AND POSTURE, AND KEEPS ONLY ITS ENTRY'S
// ROUTE: home, its workspace trusted, no update, no telemetry, three attempts
// a request, the cap a turn's steps; a key-signed run's model as its env model's name; and every
// routing variable the entry did not set unset, never a pinned one. (The
// telemetry's variables: TestEveryRunnersShellHoldsWhatItsHarnessIsHanded.)
//
// Mutation: drop the cap's or the trust's pin, pin the model without a key, or
// unset the entry's own base URL: the case fails.
func TestAKimiRunIsPinnedToItsHomeAndItsEntrysRoute(t *testing.T) {
	paths := PathsFor(sandbox.NewFakeSandbox("b"))
	keyed := sandbox.RunRequest{Model: "kimi-k3", Limits: sandbox.Limits{MaxTurns: 7},
		Env: map[string]string{kimiKeyEnv: "sk-x", "KIMI_MODEL_BASE_URL": "https://example.com/v1"}}
	pins := map[string]string{}
	for _, p := range (Kimi{}).Pins(t.Context(), keyed, paths) {
		pins[p.Name] = p.Value
	}
	want := map[string]string{"KIMI_CODE_HOME": paths.KimiHome(), "KIMI_CODE_TRUST_WORKSPACE": "1",
		"KIMI_CODE_NO_AUTO_UPDATE": "1", "KIMI_DISABLE_TELEMETRY": "1", "KIMI_LOOP_MAX_ATTEMPTS_PER_STEP": "3",
		"KIMI_LOOP_MAX_STEPS_PER_TURN": "7", kimiModelEnv: "kimi-k3"}
	for name, value := range want {
		if pins[name] != value {
			t.Errorf("%s pinned %q, want %q", name, pins[name], value)
		}
	}
	off := kimiRoutingOff(keyed)
	for _, kept := range []string{kimiKeyEnv, "KIMI_MODEL_BASE_URL", kimiModelEnv} {
		if slices.Contains(off, kept) {
			t.Errorf("the script unsets %s, which the entry set or the run pins", kept)
		}
	}
	if !slices.Contains(off, "KIMI_CODE_BASE_URL") || !slices.Contains(off, "KIMI_MODEL_PROVIDER_TYPE") ||
		!slices.Contains(off, "KIMI_WEB_SEARCH_API_KEY") {
		t.Errorf("unset %v, want every routing variable the entry left unset", off)
	}
	login := sandbox.RunRequest{Model: "kimi-code/kimi-for-coding"}
	for _, p := range (Kimi{}).Pins(t.Context(), login, paths) {
		if p.Name == kimiModelEnv || p.Name == "KIMI_LOOP_MAX_STEPS_PER_TURN" {
			t.Errorf("a login's uncapped run pins %s", p.Name)
		}
	}
	if !slices.Contains(kimiRoutingOff(login), kimiModelEnv) {
		t.Error("a login's run keeps an env model the image set")
	}
	if !slices.Contains((Kimi{}).Output(paths).Unset, "KIMI_LOOP_MAX_STEPS_PER_TURN") {
		t.Error("KIMI_LOOP_MAX_STEPS_PER_TURN is not removed whatever set it")
	}
}

// A WEB-DENIED KIMI RUN HAS NO WEB TOOL AND NO WEB SERVICE: its profile's
// allowlist leaves out FetchURL and WebSearch, its settings have the driver
// disable both for the session (every agent of it, a subagent included), and
// the web services' variables are unset even where the entry set them. A run
// that keeps the web has both tools, no such setting, and the entry's services.
//
// Mutation: list a web tool under denial, drop the settings' webDenied, or keep
// an entry's web service under denial — the case fails.
func TestAWebDeniedKimiRunHasNoWebToolOrService(t *testing.T) {
	for _, denied := range []bool{true, false} {
		box := sandbox.NewFakeSandbox("b")
		paths := PathsFor(box)
		req := sandbox.RunRequest{System: "S", WebDenied: denied,
			Env: map[string]string{"KIMI_WEB_SEARCH_BASE_URL": "https://search.example.com", "KIMI_WEB_SEARCH_API_KEY": "k"}}
		if _, err := (Kimi{}).WriteConfig(t.Context(), box, req, Session{}, paths); err != nil {
			t.Fatal(err)
		}
		var settings kimiSettings
		readJSON(t, box, paths.KimiSettings(), &settings)
		if settings.WebDenied != denied {
			t.Errorf("denied %v: settings' webDenied = %v", denied, settings.WebDenied)
		}
		profile, err := box.ReadFile(t.Context(), paths.KimiAgents()+"/"+kimiProfileDir+"/"+settings.Profile+".md")
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range kimiWebTools {
			if listed := strings.Contains(string(profile), "  - "+tool+"\n"); listed == denied {
				t.Errorf("denied %v: the profile lists %s %v:\n%s", denied, tool, listed, profile)
			}
		}
		off := kimiRoutingOff(req)
		for _, name := range []string{"KIMI_WEB_SEARCH_BASE_URL", "KIMI_WEB_SEARCH_API_KEY"} {
			if unset := slices.Contains(off, name); unset != denied {
				t.Errorf("denied %v: the entry's %s unset %v", denied, name, unset)
			}
		}
	}
}

// KIMI'S CONFIG IS ITS RUN'S OWN: a profile of a name no other run has, its
// allowlist every default tool but the two that ask a person and every MCP
// server's tools, Kimi's prompt first and the run's text after it, told of
// notes only where it reads them; the run's MCP servers in the home's file
// with their bearer, the engine's wait, their own start bound (which a home's
// `[mcp] startup_timeout_ms` would otherwise cut) and no deferral; the driver as
// embedded; and settings naming the profile, the login's model, the notes
// route, the report and the servers the guard holds to that file.
//
// Mutation: put the text before ${base_prompt}, list AskUserQuestion, defer
// the bridge, drop its start bound, name a key-signed run's model in the
// session, or leave the guard's servers out: the case fails.
func TestKimiWriteConfigWritesTheRunsOwnProfileServersAndDriver(t *testing.T) {
	box := sandbox.NewFakeSandbox("b")
	paths := PathsFor(box)
	req := sandbox.RunRequest{System: "STANDING", Model: "kimi-code/kimi-for-coding", Effort: "high",
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP,
			URL: "http://engine:1/mcp", Headers: map[string]string{"Authorization": "Bearer t"},
			CallTimeout: 600e9}},
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: "http://engine:1/mcp/steer"}}
	got, err := (Kimi{}).WriteConfig(t.Context(), box, req, Session{}, paths)
	if err != nil || got != paths.KimiSettings() {
		t.Fatalf("WriteConfig = %q, %v", got, err)
	}
	var settings kimiSettings
	readJSON(t, box, paths.KimiSettings(), &settings)
	if !strings.HasPrefix(settings.Profile, "crewlet-") || settings.Model != req.Model || settings.Thinking != "high" ||
		settings.Notes != req.Steer.URL || settings.Report != paths.SessionReport() ||
		settings.Staging != paths.sessionReportStaging() || !slices.Equal(settings.Servers, []string{"crewlet"}) ||
		settings.MCPConfig != paths.KimiMCPConfig() {
		t.Errorf("settings = %+v", settings)
	}
	profile, err := box.ReadFile(t.Context(), paths.KimiAgents()+"/"+kimiProfileDir+"/"+settings.Profile+".md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(profile)
	if !strings.Contains(text, "name: "+settings.Profile+"\n") || !strings.Contains(text, `  - "mcp__*"`) ||
		strings.Contains(text, "AskUserQuestion") || strings.Contains(text, "ExitPlanMode") ||
		!strings.Contains(text, "  - Bash\n") {
		t.Errorf("profile:\n%s", text)
	}
	body := text[strings.Index(text, "\n---\n")+5:]
	if !strings.HasPrefix(body, "${base_prompt}\n\nSTANDING\n\n"+KimiSteerInstruction) {
		t.Errorf("the profile's body is not Kimi's prompt, then the run's text and its notes:\n%s", body)
	}
	var mcp struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	readJSON(t, box, paths.KimiMCPConfig(), &mcp)
	bridge := mcp.Servers["crewlet"]
	if bridge["url"] != "http://engine:1/mcp" || bridge["deferred"] != false || bridge["toolTimeoutMs"] != float64(600000) ||
		bridge["startupTimeoutMs"] != float64(kimiServerStartMs) || settings.ServerStartMs != kimiServerStartMs ||
		bridge["headers"].(map[string]any)["Authorization"] != "Bearer t" {
		t.Errorf("the bridge reads %v", bridge)
	}
	if driver, err := box.ReadFile(t.Context(), paths.KimiDriver()); err != nil || string(driver) != kimiDriverSource {
		t.Errorf("the driver is not the embedded one (%v)", err)
	}

	again := sandbox.RunRequest{Model: "k", Env: map[string]string{kimiKeyEnv: "sk"}}
	if _, err := (Kimi{}).WriteConfig(t.Context(), box, again, Session{}, paths); err != nil {
		t.Fatal(err)
	}
	var second kimiSettings
	readJSON(t, box, paths.KimiSettings(), &second)
	if second.Profile == settings.Profile || second.Model != "" || second.Notes != "" {
		t.Errorf("a second, key-signed run without notes was written %+v", second)
	}
	quiet, err := box.ReadFile(t.Context(), paths.KimiAgents()+"/"+kimiProfileDir+"/"+second.Profile+".md")
	if err != nil || strings.Contains(string(quiet), "Notes from a person") {
		t.Errorf("a run wired to no notes is told of them (%v)", err)
	}
}

// readJSON decodes file in box into v.
func readJSON(t *testing.T, box sandbox.Sandbox, file string, v any) {
	t.Helper()
	raw, err := box.ReadFile(t.Context(), file)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
}

// A KIMI RUN'S ARTEFACTS ARE ITS OWN: its profile directory, so no earlier
// run's profile is a later run's subagent, and its driver files, which hold
// the notes route's token; never the home's skills, instructions, plugins,
// MCP servers, sessions or login, or the home's own MCP file it holds and
// the record of the run's servers, which a run cut off leaves for the next.
//
// Mutation: leave the profile directory, or clear the home or any of its own
// places: the case fails.
func TestAKimiRunsArtifactsAreItsOwnFiles(t *testing.T) {
	paths := PathsFor(sandbox.NewFakeSandbox("b"))
	files, dirs := (Kimi{}).artifactFiles(paths), (Kimi{}).artifactDirs(paths)
	if !slices.Equal(dirs, []string{paths.KimiAgents() + "/crewlet"}) ||
		!slices.Contains(files, paths.KimiSettings()) || !slices.Contains(files, paths.KimiDriver()) {
		t.Fatalf("artifacts = %v %v", files, dirs)
	}
	for _, f := range append(files, dirs...) {
		if !strings.HasPrefix(f, paths.WorkDir()+"/") && f != paths.KimiAgents()+"/crewlet" ||
			f == paths.KimiHomeMCPConfig() || f == paths.KimiMCPOurs() {
			t.Errorf("the clear removes %s, which is not the run's own", f)
		}
	}
}

// A KIMI SESSION IS MINTED BY ITS HARNESS AND CONTINUED BY ITS ID: a fresh
// start names no id and the runner reports minting; a resume starts, its
// script continuing the session it names and starting the driver on it.
//
// Mutation: name an id at a fresh start, or drop Resumes — the case fails.
func TestAKimiSessionIsMintedAndContinuedByItsID(t *testing.T) {
	if s := (Kimi{}).Session(sandbox.RunRequest{}); s != (Session{}) || !NewKimi().MintsSession() {
		t.Fatalf("session %+v, mints %v; want none named and minting", s, NewKimi().MintsSession())
	}
	box := sandbox.NewFakeSandbox("b")
	handle, err := NewKimi().Start(t.Context(), box, sandbox.RunRequest{Resume: kimiFixtureSession})
	if err != nil || handle.SessionID != kimiFixtureSession {
		t.Fatalf("Start = %+v, %v; want the resume started on its session", handle, err)
	}
	if script := box.Background()[0]; !strings.Contains(script, "set -- resume") {
		t.Fatal("a resume's script never starts the driver on the session it continues")
	}
}

// A KIMI NOTE IS ADMITTED UNDER AN ID OF THE JOB AND THE NOTE, AND HELD TO THE
// SHARED ITEM BOUND: the same pair always maps to one id, another pair never.
//
// Mutation: hash the note alone, or raise the ceiling past the bound — the case
// fails.
func TestAKimiInboxIDIsTheJobsAndTheNotesWithinTheBound(t *testing.T) {
	k := Kimi{}
	a, again, other := k.inboxID("job-1", "n1"), k.inboxID("job-1", "n1"), k.inboxID("job-2", "n1")
	if a != again || a == other || len(a) != 32 {
		t.Fatalf("ids %q %q %q", a, again, other)
	}
	if k.pollCeiling() != 4505 {
		t.Fatalf("ceiling %d, want the engine's item bound", k.pollCeiling())
	}
}
