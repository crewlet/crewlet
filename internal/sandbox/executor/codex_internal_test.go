package executor

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// codexStream is a recorded `exec --json` stream of Codex 0.171.0.
func codexStream(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex/0.171.0/" + name)
	if err != nil {
		t.Fatalf("read the recorded stream: %v", err)
	}
	return raw
}

// A CODEX RUN'S ANSWER IS ITS LAST AGENT MESSAGE, and its transcript every
// call it made, a call's start and end one numbered call: read off a recorded
// stream, an error ITEM (a model's fallback metadata) is a warning beside a
// run that succeeded, never its failure.
//
// Mutation: read an error item as the failure, take the first agent message,
// or number a call's start and end apart, and this goes red.
func TestACodexRunsAnswerIsItsLastAgentMessageAndAnErrorItemNoFailure(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		stream, answer string
		call           []string
	}{
		"an MCP call": {"stream-mcp.jsonl", "MCP-DONE",
			activityline.Call(1, "mcp__crewlet__echo", `{"text":"viahttp"}`)},
		"a shell command": {"stream-shell.jsonl", "COMMIT-DONE",
			activityline.Call(1, "shell", `/bin/bash -lc "git log -1 --format=%B"`)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := Codex{}.Parse(codexStream(t, tc.stream))
			if !res.Success || res.Error != "" {
				t.Fatalf("the run reads as failed (%q), want succeeded", res.Error)
			}
			if !strings.HasPrefix(res.Text, tc.answer) {
				t.Errorf("the answer is %q, want the last agent message %q", res.Text, tc.answer)
			}
			lines := strings.Split(res.Transcript, "\n")
			at := slices.Index(lines, tc.call[0])
			if at < 0 || slices.Index(lines, activityline.OK(1, activityline.Took{})) < at {
				t.Errorf("the transcript does not hold call 1 and its result after it:\n%s", res.Transcript)
			}
			if strings.Contains(res.Transcript, "[tool 2]") {
				t.Errorf("one call is numbered twice:\n%s", res.Transcript)
			}
		})
	}
}

// A TURN THAT FAILED IS THE RUN'S FAILURE, in the words of turn.failed, not
// the retry notices before it, which the transcript keeps.
//
// Mutation: drop turn.failed's reading, or read the first error event as the
// failure, and this goes red.
func TestACodexTurnThatFailedIsTheRunsFailure(t *testing.T) {
	t.Parallel()
	res := Codex{}.Parse(codexStream(t, "stream-failed.jsonl"))
	if res.Success {
		t.Fatal("a turn that failed reads as a success")
	}
	if want := "stream disconnected before completion: fake response.failed: something broke"; res.Error != want {
		t.Errorf("the failure is %q, want turn.failed's %q", res.Error, want)
	}
	if !strings.Contains(res.Transcript, "Reconnecting... 1/5") {
		t.Errorf("the transcript drops the retry notices:\n%s", res.Transcript)
	}
}

// A RUN'S SPEND, WINDOW AND LIMITS ARE ITS ROLLOUT LINES', never the stream's
// turn.completed usage, which is the thread's running total: the lines the
// script appended state the run's own turns, the window Codex held its model
// to and its plan's windows, which reach the profile as a limit report keyed
// by window length, its use a fraction.
//
// Mutation: charge turn.completed's usage, or key a window by its position,
// and this goes red.
func TestACodexRunsFiguresAreItsRolloutLines(t *testing.T) {
	t.Parallel()
	rollout := `{"timestamp":"2026-11-21T18:34:00.136Z","ordinal":17,"type":"token_usage_record","payload":{"turn_id":"t1","turn_token_usage":{"input_tokens":300,"cached_input_tokens":200,"cache_write_input_tokens":10,"output_tokens":40,"reasoning_output_tokens":4,"total_tokens":340}}}
{"timestamp":"2026-11-21T18:34:00.138Z","ordinal":18,"type":"event_msg","payload":{"type":"token_count","info":{"model_context_window":258400},"rate_limits":{"limit_id":"codex","primary":{"used_percent":37.5,"window_minutes":300,"resets_at":1795287240},"secondary":{"used_percent":100,"window_minutes":10080,"resets_at":1795372440}}}}`
	var stream strings.Builder
	stream.Write(codexStream(t, "stream-mcp.jsonl"))
	for line := range strings.SplitSeq(rollout, "\n") {
		stream.WriteString(`{"type":"` + codexRolloutEvent + `","line":` + line + "}\n")
	}
	d := &codexEvents{}
	if res := decodeAll(d, []byte(stream.String())); !res.Success {
		t.Fatalf("the run reads as failed: %q", res.Error)
	}
	spent, stated := d.runSpend()
	if want := (runSpend{input: 90, cacheRead: 200, cacheWrite: 10, output: 40}); !stated || spent != want {
		t.Errorf("the run's own spend is %+v (stated %v), want the rollout turn's %+v", spent, stated, want)
	}
	if got := d.reportedWindow(); got != 272000 {
		t.Errorf("the reported window is %d, want the raw 272000 behind the reported 258400", got)
	}
	want := `{"type":"crewlet.codex_rate_limits","state":"reached","window":"10080","resets_at":1795372440,` +
		`"windows":{"10080":` +
		`{"used":1,"resets_at":1795372440},"300":{"used":0.375,"resets_at":1795287240}}}`
	if got := d.limitReport(); got != want {
		t.Errorf("the limit report is\n%s\nwant\n%s", got, want)
	}
}

// A RUN WHOSE STREAM CARRIES NO ROLLOUT LINES STATES NO SPEND, so the
// collection charges nothing it could only guess, and reports no limits.
//
// Mutation: state a zero spend, or a limit report with no windows, and this
// goes red.
func TestACodexRunWithNoRolloutLinesStatesNothing(t *testing.T) {
	t.Parallel()
	d := &codexEvents{}
	decodeAll(d, codexStream(t, "stream-mcp.jsonl"))
	if _, stated := d.runSpend(); stated {
		t.Error("a run with no rollout lines states a spend")
	}
	if got := d.limitReport(); got != "" {
		t.Errorf("a run with no rollout lines reports limits %q", got)
	}
}

// EVERY VALUE OF A CODEX RUN'S CONFIGURATION IS A TOML BASIC STRING that
// reads back as written: a quote, a backslash, a line break and a control
// character are escapes, so no value ends its string or opens a key.
//
// Mutation: drop any escape in tomlString, and this goes red.
func TestEveryCodexConfigValueIsATOMLStringThatReadsBack(t *testing.T) {
	t.Parallel()
	for _, value := range []string{`plain`, `a "quoted" word`, `C:\path\to`, "two\nlines\tand\r", "bell\x07del\x7f",
		"emoji 🛠 and CJK 修正"} {
		quoted := tomlString(value)
		// TOML's basic-string escapes are a subset of Go's, so Go reads one back.
		got, err := strconv.Unquote(quoted)
		if err != nil || got != value {
			t.Errorf("tomlString(%q) = %s, which reads back as %q (%v)", value, quoted, got, err)
		}
		if strings.ContainsAny(quoted[1:len(quoted)-1], "\n\r\x07\x7f") {
			t.Errorf("tomlString(%q) = %s holds a raw control character", value, quoted)
		}
	}
}

// A CODEX RUN COMPACTS WHERE THE ENGINE JUDGES A CONTINUATION: each model is
// held to its own raw window from Codex's catalogue (§17.2), an unlisted one to
// the 272,000 Codex falls back to (§16.4) and unknown; the limit its
// configuration pins is the point of that window, 90%, and a request carries
// at most the 95% Codex reports as its model_context_window.
//
// Mutation: hold every model to one window, pin another limit, or change the
// reserve or buffer, and a case goes red.
func TestACodexRunCompactsWhereTheEngineJudgesIt(t *testing.T) {
	t.Parallel()
	for model, tc := range map[string]struct {
		window int
		limit  int64
		known  bool
	}{
		"gpt-6.2-sol": {500_000, 450_000, true},
		"GPT-5.5":     {272_000, 244_800, true},
		"gpt-test":    {272_000, 244_800, false},
		"":            {272_000, 244_800, false},
	} {
		w := Codex{}.Window(WindowAsk{Model: model})
		if w.Tokens != tc.window || w.Standard != tc.window || w.Known != tc.known {
			t.Errorf("%q is held to %d (standard %d, known %v), want %d (known %v)", model, w.Tokens,
				w.Standard, w.Known, tc.window, tc.known)
		}
		if got := w.Point(int64(w.Tokens)); got != tc.limit || codexCompactLimit(model) != got {
			t.Errorf("%q: the engine judges compaction at %d and the run pins %d, want both %d", model, got,
				codexCompactLimit(model), tc.limit)
		}
		if shown := int64(tc.window) * 19 / 20; !w.Carries(shown, 0) || w.Carries(shown+1, 0) {
			t.Errorf("%q carries other than the %d Codex reports it holds a request to", model, shown)
		}
		if got := (Codex{}).PinnedWindow(model); got != tc.window {
			t.Errorf("%q is pinned to %d, want %d", model, got, tc.window)
		}
	}
}

// ONLY THE ENGINE'S OWN TOOLS ARE REQUIRED AND ALWAYS OFFERED: a seat's other
// server failing to start leaves the run its tools, and one Codex deferred
// behind its tool search would hide the bridge's. Every server's url,
// headers and environment are in the file, and its call bound in seconds.
//
// Mutation: require every server, or none, and this goes red.
func TestOnlyTheEnginesOwnCodexServerIsRequired(t *testing.T) {
	t.Parallel()
	got := codexConfigTOML(map[string]sandbox.MCPServer{
		"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:7/mcp/run-token", AlwaysLoaded: true,
			CallTimeout: 90 * time.Second},
		"docs": {Transport: sandbox.TransportHTTP, URL: "https://docs.example.com/mcp",
			Headers: map[string]string{"Authorization": "Bearer docs-token"}},
		"local": {Command: "node", Args: []string{"server.js"}, Env: map[string]string{"KEY": "v"}},
	}, "You are the seat.", "gpt-test")
	want := `model_auto_compact_token_limit = 244800
developer_instructions = "You are the seat."

[mcp_servers."crewlet"]
url = "http://127.0.0.1:7/mcp/run-token"
tool_timeout_sec = 90
required = true
startup_timeout_sec = 60
omit_tools_from = ["deferred"]
supports_parallel_tool_calls = true
default_tools_approval_mode = "approve"

[mcp_servers."docs"]
url = "https://docs.example.com/mcp"
http_headers = { "Authorization" = "Bearer docs-token" }
supports_parallel_tool_calls = true
default_tools_approval_mode = "approve"

[mcp_servers."local"]
command = "node"
args = ["server.js"]
env = { "KEY" = "v" }
supports_parallel_tool_calls = true
default_tools_approval_mode = "approve"
`
	if got != want {
		t.Errorf("the configuration is\n%s\nwant\n%s", got, want)
	}
}

// A RUN READS AS CAPPED ONLY WHERE THE HELPER MARKED ITS CAP AND THE ROLLOUT
// RECORDS THE TURN INTERRUPTED: an interrupt with no mark keeps its own cause,
// and a mark with no interrupt ended nothing. A run whose helper ended under it
// fails, never capped.
//
// Mutation: read the mark alone, or the interrupt alone, as the cap, or drop
// the helper-lost reading, and a case goes red.
func TestACodexRunReadsAsCappedOnlyOnTheHelpersMarkAndAnInterrupt(t *testing.T) {
	t.Parallel()
	capped := `{"type":"` + codexCappedEvent + `","rounds":3}` + "\n"
	aborted := `{"type":"` + codexRolloutEvent + `","line":{"type":"event_msg","payload":{"type":"turn_aborted",` +
		`"turn_id":"t9","reason":"interrupted"}}}` + "\n"
	lost := `{"type":"` + codexHelperLostEvent + `"}` + "\n"
	failed := string(codexStream(t, "stream-failed.jsonl"))
	for name, tc := range map[string]struct {
		stream       string
		roundCapped  bool
		errorHolding string
	}{
		"the mark and the interrupt": {failed + capped + aborted, true, "round cap of 3"},
		"an interrupt, no mark":      {failed + aborted, false, "stream disconnected"},
		"a mark, no interrupt":       {failed + capped, false, "stream disconnected"},
		"the helper lost":            {"{\"type\":\"thread.started\",\"thread_id\":\"x\"}\n" + lost + aborted, false, "note helper ended"},
	} {
		res := Codex{}.Parse([]byte(tc.stream))
		if res.Success || res.RoundCapped != tc.roundCapped || !strings.Contains(res.Error, tc.errorHolding) {
			t.Errorf("%s: success %v, capped %v, %q; want capped %v, an error holding %q", name, res.Success,
				res.RoundCapped, res.Error, tc.roundCapped, tc.errorHolding)
		}
	}
}
