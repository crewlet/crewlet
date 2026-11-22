package executor_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// geminiFixture is one of testdata/gemini/0.70.0's files: Gemini CLI 0.70.0's
// own output, captured on the build VM against a stub of the Gemini API.
func geminiFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "gemini", "0.70.0", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// geminiNode is the node a Gemini CLI box's scripts are run under here.
func geminiNode(t *testing.T) string {
	t.Helper()
	return prereqtest.OnPath(t, "node", "a Gemini CLI run's helper and take script run under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
}

// A GEMINI RUN'S ANSWER IS ITS LAST REPLY, and the transcript shows each call
// and the note its finish was handed. On 0.70.0's own stream of a run whose
// AfterAgent hook blocked its finish with a note, the answer is the reply after
// the block, the run succeeded under its session's id, both calls read OK, and
// the note is a steer line.
//
// Mutations, each red: keep the answer across the block (both replies); read
// the block's warning as an error line; key a call's result by anything but
// its tool_id (no OK lines).
func TestAGeminiRunsAnswerIsItsLastReply(t *testing.T) {
	t.Parallel()
	res := executor.GeminiCLI{}.Parse(geminiFixture(t, "stream-blocked.jsonl"))
	if res.Text != "STUB-ANSWER: saw the end note." || !res.Success {
		t.Fatalf("the run answered %q (success %v), want the reply after the block", res.Text, res.Success)
	}
	if res.SessionID != "fd93aab8-4f12-4fdc-98a0-4bc278732aa5" {
		t.Errorf("the run's session reads %q, want init's", res.SessionID)
	}
	lines := strings.Split(res.Transcript, "\n")
	var oks, steers int
	for _, l := range lines {
		parsed := activityline.Parse(l)
		switch parsed.Tag {
		case activityline.TagOK:
			oks++
		case activityline.TagSteer:
			steers++
			if !strings.Contains(l, "NOTE-END BRIDGE:echo: take-AfterAgent") {
				t.Errorf("the steer line %q does not carry the note", l)
			}
		case activityline.TagError:
			t.Errorf("the transcript holds an error line %q for a run that succeeded", l)
		}
	}
	if oks != 2 || steers != 1 {
		t.Errorf("the transcript shows %d calls answered and %d notes, want 2 and 1:\n%s", oks, steers, res.Transcript)
	}
}

// A GEMINI RUN THAT FAILED SAYS HOW, IN FIELDS AND WORDS: the result's error
// is the run's, its result line is the Event a limit is classified by, and
// only the API's sentence for a prompt past the window marks the context
// exhausted. On 0.70.0's own streams of a spent daily quota, a per-minute
// limit after its retries, and a prompt past the window.
//
// Mutations, each red: read the error off anything but the result; leave
// Event empty; mark every failure ContextExhausted.
func TestAFailedGeminiRunSaysHow(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fixture, said string
		exhausted     bool
	}{
		"a spent daily quota": {fixture: "stream-daily-quota.jsonl",
			said: "You have exhausted your daily quota on this model."},
		"a per-minute limit": {fixture: "stream-per-minute.jsonl", said: "Quota exceeded for metric"},
		"a prompt past the window": {fixture: "stream-context.jsonl",
			said: "exceeds the maximum number of tokens allowed", exhausted: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := executor.GeminiCLI{}.Parse(geminiFixture(t, tc.fixture))
			if res.Success || !strings.Contains(res.Error, tc.said) {
				t.Fatalf("the run ended %q (success %v), want a failure saying %q", res.Error, res.Success, tc.said)
			}
			if !strings.Contains(res.Event, `"type":"result"`) {
				t.Errorf("the run's Event is %q, want its result line", res.Event)
			}
			if res.ContextExhausted != tc.exhausted {
				t.Errorf("ContextExhausted is %v, want %v", res.ContextExhausted, tc.exhausted)
			}
		})
	}
	if res := (executor.GeminiCLI{}).Parse([]byte(strings.SplitN(string(geminiFixture(t, "stream-tools.jsonl")),
		"\n", 3)[0] + "\n")); res.Success || !strings.Contains(res.Error, "no result event") {
		t.Errorf("a stream cut before its result reads %q (success %v), want it said", res.Error, res.Success)
	}
}

// A GEMINI RUN'S ROUND CAP IS ITS SESSION TURN CAP, AND A RUN THAT REACHES IT
// READS AS CAPPED. The run's settings carry the cap as model.maxSessionTurns,
// and none where the run is uncapped; on 0.70.0's own stream of a run capped
// at 2, whose result ended it with a FatalTurnLimitedError after its second
// request's call, the run failed as one that reached its cap, which the
// engine records as ExecutorErrorMaxTurns. Every other failure measured reads
// as it did.
//
// Mutations, each red: leave the cap out of the settings; read the cap off
// anything but the result error's type (every failure capped, or none);
// leave "turn cap" out of the error.
func TestAGeminiRunsRoundCapIsItsSessionTurnCapAndReadsAsCapped(t *testing.T) {
	t.Parallel()
	settings, _, _ := writtenGemini(t, sandbox.RunRequest{System: "SYSTEM", Brief: "x",
		Limits: sandbox.Limits{MaxTurns: 2}})
	if model, _ := settings["model"].(map[string]any); model["maxSessionTurns"] != float64(2) {
		t.Errorf("a run capped at 2 writes model %v, want maxSessionTurns 2", settings["model"])
	}
	settings, _, _ = writtenGemini(t, sandbox.RunRequest{System: "SYSTEM", Brief: "x"})
	if model, ok := settings["model"]; ok {
		t.Errorf("an uncapped run writes model %v, want none", model)
	}
	res := executor.GeminiCLI{}.Parse(geminiFixture(t, "stream-turn-cap.jsonl"))
	if res.Success || !res.RoundCapped || !strings.Contains(res.Error, "turn cap") {
		t.Errorf("a run ended at its cap reads %q (success %v, capped %v), want it failed at its turn cap",
			res.Error, res.Success, res.RoundCapped)
	}
	if res.Turns != 2 {
		t.Errorf("the capped run made %d requests, want the 2 its cap allowed", res.Turns)
	}
	for _, fixture := range []string{"stream-daily-quota.jsonl", "stream-per-minute.jsonl", "stream-context.jsonl"} {
		if res := (executor.GeminiCLI{}).Parse(geminiFixture(t, fixture)); res.RoundCapped ||
			strings.Contains(res.Error, "turn cap") {
			t.Errorf("%s reads as capped (%q)", fixture, res.Error)
		}
	}
}

// A GEMINI RUN IS CHARGED ITS REASONING AND ITS TOOL PROMPT: stream-json's
// result splits only the prompt and the reply out of its total, and the
// helper's sums off 0.70.0's own chat file (each response once although 0.70.0
// writes it up to three times) move the tool prompt, billed as input, to the
// input. The stream says 3,000 in and 150 out of 3,261; the run is charged
// 3,021 and 240, and so is a run that ended with no result, off the file alone.
//
// Mutations, each red: sum every entry rather than the latest of each id;
// leave the tool prompt out of the input; read the session file another
// session's first line names; leave the thoughts out of a resultless run's.
func TestAGeminiRunIsChargedItsReasoningAndItsToolPrompt(t *testing.T) {
	t.Parallel()
	node := geminiNode(t)
	chats := t.TempDir()
	const session = "842120d9-2536-4808-8ba9-437d34456f75"
	if err := os.WriteFile(filepath.Join(chats, "session-2026-11-21T18-48-842120d9.jsonl"),
		geminiFixture(t, "session-tools.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ANOTHER SESSION SHARING THE EIGHT CHARACTERS the file is named by.
	if err := os.WriteFile(filepath.Join(chats, "session-2026-11-21T18-47-842120d9.jsonl"),
		[]byte(`{"sessionId":"842120d9-0000-4000-8000-000000000000"}`+"\n"+
			`{"id":"x","type":"gemini","tokens":{"input":99999,"output":1}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	usage, err := exec.CommandContext(t.Context(), node, filepath.Join(".", "gemini_helper.mjs"), "usage", chats,
		session).Output()
	if err != nil {
		t.Fatalf("the helper's usage: %v", err)
	}
	stream := string(geminiFixture(t, "stream-tools.jsonl"))
	result := stream[strings.LastIndex(strings.TrimSuffix(stream, "\n"), "\n")+1:]
	if !strings.Contains(result, `"type":"result"`) {
		t.Fatal("the capture does not end with its result")
	}
	for name, raw := range map[string]string{"a run": stream, "a run with no result": strings.TrimSuffix(stream, result)} {
		res := executor.GeminiCLI{}.Parse(append([]byte(raw), usage...))
		if res.InputTokens != 3021 || res.CacheReadTokens != 1200 || res.OutputTokens != 240 || res.Turns != 3 {
			t.Errorf("%s is charged in %d (cached %d), out %d over %d requests, want 3021 (1200), 240 over 3; "+
				"the helper said %s", name, res.InputTokens, res.CacheReadTokens, res.OutputTokens, res.Turns, usage)
		}
	}
}

// A GEMINI RUN THAT COMPRESSED IS CHARGED ITS COMPRESSIONS. Each compression
// is two requests of Gemini CLI's own (a summary, then its check) that the
// chat file keeps no entry for, and the result counts. On 0.70.0's own run of
// two compressions (r6): 607,035 in (3,600 cached) and 748 out over five
// requests of the run's, where the chat file alone says 603,035 and 400.
//
// Mutation, red: charge the chat file's sums wherever the usage line is.
func TestAGeminiRunThatCompressedIsChargedItsCompressions(t *testing.T) {
	t.Parallel()
	node := geminiNode(t)
	chats := t.TempDir()
	if err := os.WriteFile(filepath.Join(chats, "session-2026-11-22T00-03-f3c2bd14.jsonl"),
		geminiFixture(t, "session-compressed.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	usage, err := exec.CommandContext(t.Context(), node, filepath.Join(".", "gemini_helper.mjs"), "usage", chats,
		"f3c2bd14-8784-4208-b09a-e920556c9785").Output()
	if err != nil {
		t.Fatalf("the helper's usage: %v", err)
	}
	res := executor.GeminiCLI{}.Parse(append(geminiFixture(t, "stream-compressed.jsonl"), usage...))
	if res.InputTokens != 607035 || res.CacheReadTokens != 3600 || res.OutputTokens != 748 || res.Turns != 5 {
		t.Fatalf("the run is charged in %d (cached %d), out %d over %d requests, want 607035 (3600), 748 over 5; "+
			"the helper said %s", res.InputTokens, res.CacheReadTokens, res.OutputTokens, res.Turns, usage)
	}
}

// A GEMINI RUN'S OWN SCRIPT CHARGES IT WHAT ITS SESSION SPENT. The runner's
// own capture of a whole run on 0.70.0 (its script, settings and take script
// against a stub API and bridge, its finish blocked twice by the same note):
// the usage line its helper appended after the result charges nine requests'
// whole prompts with their tool prompts and their replies with the reasoning,
// and the answer is the reply after the last block.
//
// Mutations, each red: charge the result's prompt and reply alone (9,000 in,
// 450 out); keep the reply before a block.
func TestAGeminiRunsOwnScriptChargesItWhatItsSessionSpent(t *testing.T) {
	t.Parallel()
	res := executor.GeminiCLI{}.Parse(geminiFixture(t, "stream-twin-end.jsonl"))
	if !res.Success || res.Text != "STUB-ANSWER: done after mcp_crewlet_echo." {
		t.Fatalf("the run answered %q (success %v)", res.Text, res.Success)
	}
	if res.InputTokens != 9063 || res.CacheReadTokens != 3600 || res.OutputTokens != 720 || res.Turns != 9 {
		t.Fatalf("the run is charged in %d (cached %d), out %d over %d requests, want 9063 (3600), 720 over 9",
			res.InputTokens, res.CacheReadTokens, res.OutputTokens, res.Turns)
	}
}

// ONLY A TERMINAL QUOTA AT THE RUN'S OWN FAILURE SITS A GEMINI ENTRY OUT. A
// whole run in a real box, its stand-in printing 0.70.0's own stream and
// exiting with the status recorded beside it, collected and read by the
// shipped profile as the engine reads an executor's ending
// (harness.Provider.EndedKind). A daily quota at an exit of 173, the 429's, is
// a limit; the same stream at an exit of 0 is none; a per-minute limit after
// its retries is none, its class transient; nor is a succeeded run whose reply
// quotes the quota's sentence, nor a prompt past the window.
//
// Mutations, each red: drop the exit gate in Collect; mark
// RetryableQuotaError a limit; drop the daily quota's sentence.
func TestOnlyATerminalQuotaAtTheRunsOwnFailureSitsAGeminiEntryOut(t *testing.T) {
	t.Parallel()
	geminiNode(t)
	p, err := harness.New(harness.Config{Key: "gemini", Model: "gemini-2.5-pro", Harness: executor.GeminiCLIName,
		StateDir: t.TempDir(), Timeout: time.Minute, MaxConcurrent: 1,
		Auth: harness.Auth{Mode: harness.AuthAPIKey, APIKey: "key-placeholder"}})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	for name, tc := range map[string]struct {
		fixture string
		exit    int
		limit   bool
	}{
		"a spent daily quota":                  {"stream-daily-quota.jsonl", 173, true},
		"a spent daily quota's stream, exit 0": {"stream-daily-quota.jsonl", 0, false},
		"a per-minute limit":                   {"stream-per-minute.jsonl", 173, false},
		"a reply quoting the quota":            {"stream-quota-said.jsonl", 0, false},
		"a prompt past the window":             {"stream-context.jsonl", 144, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture, err := filepath.Abs(filepath.Join("testdata", "gemini", "0.70.0", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			runner, b := geminiBox(t, "cat "+shellword.Quote(fixture)+"\nexit "+strconv.Itoa(tc.exit)+"\n")
			_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
			kind, ok := p.EndedKind(harness.RunEnd{Answer: res.Text, Error: res.Error, Failed: !res.Success,
				Event: res.Event, Limits: res.Limits, NoLimit: res.NoLimitExit,
				At: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)})
			if got := ok && kind == llm.KindRateLimit; got != tc.limit {
				t.Errorf("the ending reads as a limit: %v (kind %v, classified %v), want %v; the run: %+v",
					got, kind, ok, tc.limit, res.Error)
			}
		})
	}
}

// geminiOwnResponses is two responses a resumed run appends to 0.70.0's
// session-tools.jsonl: its last response under ids of their own, each with
// tokens no response of the capture has, so a sum shows which were charged.
func geminiOwnResponses(t *testing.T) string {
	t.Helper()
	const captured = `"tokens":{"input":1000,"output":50,"cached":400,"thoughts":30,"tool":7,"total":1087}`
	var last string
	for _, l := range strings.Split(string(geminiFixture(t, "session-tools.jsonl")), "\n") {
		if strings.Contains(l, `"type":"gemini"`) {
			last = l
		}
	}
	if !strings.Contains(last, captured) {
		t.Fatalf("the capture's last response holds no %s: %s", captured, last)
	}
	var own strings.Builder
	for _, r := range [][2]string{
		{"resumed-response-1", `"tokens":{"input":1500,"output":60,"cached":500,"thoughts":40,"tool":11,"total":1611}`},
		{"resumed-response-2", `"tokens":{"input":2000,"output":70,"cached":600,"thoughts":50,"tool":13,"total":2133}`},
	} {
		l := strings.Replace(last, "881853cd-0513-4cbd-b8bd-5bc81ae51ac1", r[0], 1)
		own.WriteString(strings.Replace(l, captured, r[1], 1) + "\n")
	}
	return own.String()
}

// A CONTINUED GEMINI RUN IS CHARGED ITS OWN RESPONSES, NOT ITS SESSION'S. A
// headless resume reports only the process's own stream totals, so the chat
// file's split is the run's only where it leaves out what the session held as
// the run started. The session 0.70.0 wrote (session-tools.jsonl, three
// responses) is listed before the run (`ids`), the resumed run appends two
// responses of tokens of their own, and the usage line charges those two
// alone, summed.
//
// Mutations, each red: list no response before the run; sum the file whatever
// the list names; list each entry rather than each response; charge the
// file's last response alone.
func TestAContinuedGeminiRunIsChargedItsOwnResponses(t *testing.T) {
	t.Parallel()
	node := geminiNode(t)
	chats := t.TempDir()
	const session = "842120d9-2536-4808-8ba9-437d34456f75"
	file := filepath.Join(chats, "session-2026-11-21T18-48-842120d9.jsonl")
	held := geminiFixture(t, "session-tools.jsonl")
	if err := os.WriteFile(file, held, 0o600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(".", "gemini_helper.mjs")
	ids, err := exec.CommandContext(t.Context(), node, helper, "ids", chats, session).Output()
	if err != nil {
		t.Fatalf("the helper's ids: %v", err)
	}
	want := "8b3b6c59-1fd4-4048-8954-f05684ee2f4f\n5a74d896-f691-4287-bca9-99845c7f923d\n" +
		"881853cd-0513-4cbd-b8bd-5bc81ae51ac1\n"
	if string(ids) != want {
		t.Fatalf("the session held %q as the run started, want its three responses %q", ids, want)
	}
	before := filepath.Join(t.TempDir(), "before")
	if err := os.WriteFile(before, ids, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(held, geminiOwnResponses(t)...), 0o600); err != nil {
		t.Fatal(err)
	}
	usage, err := exec.CommandContext(t.Context(), node, helper, "usage", chats, session, before).Output()
	if err != nil {
		t.Fatalf("the helper's usage: %v", err)
	}
	const charged = `{"type":"crewlet_gemini_usage","session_id":"` + session + `","requests":2,` +
		`"tokens":{"input":3500,"output":130,"cached":1100,"thoughts":90,"tool":24}}` + "\n"
	if string(usage) != charged {
		t.Errorf("the continued run is charged %s, want its two responses: %s", usage, charged)
	}
}
