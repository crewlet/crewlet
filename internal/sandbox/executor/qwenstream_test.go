package executor_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// qwenFixture is one of testdata/qwen/<release>'s files, made from the build
// VM's captures of Qwen Code over ACP against a stub of an OpenAI-compatible
// API: each session/update line as Qwen Code sent it, verbatim, and the
// helper's lines as it writes them for that run, their values from the same
// capture; chat files as Qwen Code wrote them. The captures cut its long
// available_commands_update lines, which the stream leaves out and nothing
// reads.
func qwenFixture(t *testing.T, release, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "qwen", release, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A QWEN RUN'S ANSWER IS ITS LAST REPLY, AND ITS TRANSCRIPT SHOWS EACH CALL
// WITH ITS INPUT AND THE NOTE IT WAS HANDED. On 0.28.4's stream of a run
// that called two tools and was handed a note after the first: the
// answer is the text after the last call, never a thought; the run succeeded
// under the session its helper named; each call reads once, with the input
// Qwen Code states only once it starts the call, and reads OK by its
// provider's call id; the note is a steer line.
//
// Mutations, each red: keep thoughts in the answer; write a call's line at
// tool_call (no input); key a result by anything but toolCallId (no OK
// lines); drop the note line.
func TestAQwenRunsAnswerIsItsLastReplyAndItsTranscriptShowsItsCallsAndNote(t *testing.T) {
	t.Parallel()
	for release, session := range map[string]string{
		"0.28.4": "11111111-2222-4333-8444-000000000001",
	} {
		t.Run(release, func(t *testing.T) {
			t.Parallel()
			res := executor.QwenCode{}.Parse(qwenFixture(t, release, "stream-note.jsonl"))
			if res.Text != "Final answer." || !res.Success {
				t.Fatalf("the run answered %q (success %v), want the last reply alone", res.Text, res.Success)
			}
			if res.SessionID != session {
				t.Errorf("the run's session reads %q, want the helper's %s", res.SessionID, session)
			}
			var calls, oks, steers []string
			for _, l := range strings.Split(res.Transcript, "\n") {
				switch activityline.Parse(l).Tag {
				case activityline.TagCall:
					calls = append(calls, l)
				case activityline.TagOK:
					oks = append(oks, l)
				case activityline.TagSteer:
					steers = append(steers, l)
				case activityline.TagError:
					t.Errorf("a run that succeeded shows an error line %q", l)
				}
			}
			if len(calls) != 2 || !strings.Contains(calls[0], `mcp__probe__slow: {"seconds":3}`) ||
				!strings.Contains(calls[1], `mcp__probe__echo: {"text":"hello echo"}`) {
				t.Errorf("the calls read %q, want each probe call once with its input", calls)
			}
			if len(oks) != 2 {
				t.Errorf("%d calls read OK, want 2:\n%s", len(oks), res.Transcript)
			}
			if len(steers) != 1 || !strings.Contains(steers[0], "drained note") {
				t.Errorf("the notes read %q, want the one handed", steers)
			}
			if strings.Contains(res.Transcript, "thinking") {
				t.Errorf("a thought reached the transcript:\n%s", res.Transcript)
			}
		})
	}
}

// A QWEN RUN IS CHARGED ITS REQUESTS, THE CACHE INSIDE THEIR INPUT AND THE
// THOUGHTS INSIDE THEIR OUTPUT. The 0.28.4 run's three requests each state
// 1,001 to 1,003 in, 600 of it cached, and 50 out with 20 of it reasoning
// (Qwen Code's session stats: 3,006 prompt, 1,800 cached, 150 out).
//
// Mutations, each red: add the cached share to the input (4,806); add the
// thoughts to the output (210); count a request per chunk rather than per
// usage (Turns past 3).
func TestAQwenRunIsChargedItsRequestsCacheInsideInputThoughtsInsideOutput(t *testing.T) {
	t.Parallel()
	res := executor.QwenCode{}.Parse(qwenFixture(t, "0.28.4", "stream-note.jsonl"))
	if res.InputTokens != 3006 || res.CacheReadTokens != 1800 || res.OutputTokens != 150 || res.Turns != 3 {
		t.Fatalf("the run is charged in %d (cached %d), out %d over %d requests, want 3006 (1800), 150 over 3",
			res.InputTokens, res.CacheReadTokens, res.OutputTokens, res.Turns)
	}
}

// A QWEN RUN THAT FAILED SAYS HOW, IN FIELDS AND WORDS: its error is the end
// line's, the provider's details where Qwen Code relayed them; its Event is
// the helper's limit line where there is one, else the end line; only the
// providers' own sentence for a prompt past the window marks the context
// exhausted; a cancelled turn and a stream with no end line say so. On
// 0.28.4's own runs of a spent quota and a rate limit (each after seven
// attempts), a prompt past the window, and a turn cancelled mid-call.
//
// Mutations, each red: read the error off Qwen Code's message over the
// details; leave Event empty, or the end line where a limit line stands; mark
// every failure ContextExhausted; read a cancelled turn as a success.
func TestAFailedQwenRunSaysHow(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fixture, said, event string
		exhausted            bool
	}{
		"a spent quota": {fixture: "stream-quota.jsonl", said: "Rate limit exceeded",
			event: "429 You exceeded your current quota"},
		"a rate limit": {fixture: "stream-rate.jsonl", said: "Rate limit exceeded",
			event: "429 Rate limit reached"},
		"a prompt past the window": {fixture: "stream-context.jsonl",
			said: "maximum context length is 128000 tokens", event: `"type":"crewlet_qwen_end"`, exhausted: true},
		"a cancelled turn": {fixture: "stream-cancel.jsonl", said: "cancelled",
			event: `"stop_reason":"cancelled"`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := executor.QwenCode{}.Parse(qwenFixture(t, "0.28.4", tc.fixture))
			if res.Success || !strings.Contains(res.Error, tc.said) {
				t.Fatalf("the run ended %q (success %v), want a failure saying %q", res.Error, res.Success, tc.said)
			}
			if !strings.Contains(res.Event, tc.event) {
				t.Errorf("the run's Event is %q, want it holding %q", res.Event, tc.event)
			}
			if res.ContextExhausted != tc.exhausted {
				t.Errorf("ContextExhausted is %v, want %v", res.ContextExhausted, tc.exhausted)
			}
			if res.Text != "" {
				t.Errorf("a failed run answered %q", res.Text)
			}
		})
	}
	cut := strings.SplitN(string(qwenFixture(t, "0.28.4", "stream-note.jsonl")), "\n", 5)
	if res := (executor.QwenCode{}).Parse([]byte(strings.Join(cut[:4], "\n") + "\n")); res.Success ||
		!strings.Contains(res.Error, "no end line") {
		t.Errorf("a stream cut before its end reads %q (success %v), want it said", res.Error, res.Success)
	}
}

// A QWEN RUN'S ROUND CAP IS QWEN CODE'S SESSION TURN CAP, AND A RUN THAT
// REACHES IT READS AS CAPPED. The run's settings carry the cap as
// model.maxSessionTurns, and none where the run is uncapped; on 0.28.4's own
// stream of a run capped at 2, whose turn ended max_turn_requests after its
// second request's call, the run failed as one that reached its cap, which
// the engine records as ExecutorErrorMaxTurns.
//
// Mutations, each red: leave the cap out of the settings; write it at 0;
// read max_turn_requests as any other stop (not RoundCapped).
func TestAQwenRunsRoundCapIsItsSessionTurnCapAndReadsAsCapped(t *testing.T) {
	t.Parallel()
	capped := sandbox.RunRequest{System: "SYSTEM", Brief: "x", Limits: sandbox.Limits{MaxTurns: 2}}
	settings, _, _, _ := writtenQwen(t, capped)
	if got := settings["model"].(map[string]any)["maxSessionTurns"]; got != float64(2) {
		t.Errorf("a run capped at 2 writes model.maxSessionTurns %v, want 2", got)
	}
	settings, _, _, _ = writtenQwen(t, sandbox.RunRequest{System: "SYSTEM", Brief: "x"})
	if got, ok := settings["model"].(map[string]any)["maxSessionTurns"]; ok {
		t.Errorf("an uncapped run writes model.maxSessionTurns %v, want none", got)
	}
	res := executor.QwenCode{}.Parse(qwenFixture(t, "0.28.4", "stream-cap.jsonl"))
	if res.Success || !res.RoundCapped || !strings.Contains(res.Error, "turn cap") {
		t.Errorf("a run ended at its cap reads %q (success %v, capped %v), want it failed at its turn cap",
			res.Error, res.Success, res.RoundCapped)
	}
	if res.Turns != 2 {
		t.Errorf("the capped run made %d requests, want the 2 its cap allowed", res.Turns)
	}
}

// qwenBox is a direct box whose `node`, the helper's runtime, is a stand-in
// running run: the run's script starts `node <helper> run ...`, so the
// stand-in plays the helper's whole run. Its `qwen` prints the release the
// suite's captures were made on when the script reads it.
func qwenBox(t *testing.T, run string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	return qwenBoxAt(t, qwenCapturedRelease, run)
}

// qwenCapturedRelease is the Qwen Code release whose output the suite's
// fixtures were captured from (testdata/qwen/0.28.4).
const qwenCapturedRelease = "0.28.4"

// qwenBoxAt is [qwenBox] whose Qwen Code prints release to `--version`, as
// the run's script reads it before its gate, and does nothing else: the
// helper's stand-in plays the harness.
func qwenBoxAt(t *testing.T, release, run string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	b, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { b.Close(t.Context()) })
	runner := executor.NewQwenCode()
	if err = runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// The guard ([executor.QwenCode]'s script) finds nothing switched off:
	// the stand-in plays the helper whole (guards_test.go runs the real one),
	// but for its `checkouts` mode, which the real node runs.
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/node", "#!/bin/sh\n[ \"$2\" = guard ] && exit 0\n"+
		"[ \"$2\" = checkouts ] && exec "+shellword.Quote(geminiNode(t))+` "$@"`+"\n"+run)
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/qwen",
		"#!/bin/sh\ncase \"$1\" in --version) echo "+shellword.Quote(release)+"; exit 0;; esac\nexit 2\n")
	return runner, b
}

// ONLY THE MATCHING QUOTA TEXT AFTER QWEN CODE'S OWN 429 FAILURE SITS A QWEN
// ENTRY OUT. A whole run in a real box, its helper's stand-in printing a
// 0.28.4 run's stream and exiting as the helper would, read by the shipped
// profile as the engine reads an executor's ending
// (harness.Provider.EndedKind). A spent quota's record after the 429's exit
// (173) is a limit; the same stream at exit 0 is none (any process of the box
// could have written it); a rate limit after a 429 failure is none, being
// transient; a prompt past the window is none. The window is the helper's.
//
// Mutations, each red: drop the exit gate in Collect (QwenCode's limitGate);
// match a rate limit's text in the profile; leave the limit line out of
// Event.
func TestOnlyTheMatchingQuotaAfterAReal429SitsAQwenEntryOut(t *testing.T) {
	t.Parallel()
	p, err := harness.New(harness.Config{Key: "qwen", Model: "qwen3-coder-plus", Harness: executor.QwenCodeName,
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
		"a spent quota after the 429":    {"stream-quota.jsonl", 173, true},
		"a quota record, nothing failed": {"stream-quota.jsonl", 0, false},
		"a rate limit after the 429":     {"stream-rate.jsonl", 173, false},
		"a prompt past the window":       {"stream-context.jsonl", 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture, err := filepath.Abs(filepath.Join("testdata", "qwen", "0.28.4", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			runner, b := qwenBox(t, "cat "+shellword.Quote(fixture)+"\nexit "+strconv.Itoa(tc.exit)+"\n")
			_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
			kind, ok := p.EndedKind(harness.RunEnd{Answer: res.Text, Error: res.Error, Failed: !res.Success,
				Event: res.Event, Limits: res.Limits, NoLimit: res.NoLimitExit,
				At: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)})
			if got := ok && kind == llm.KindRateLimit; got != tc.limit {
				t.Errorf("the ending reads as a limit: %v (kind %v, classified %v), want %v; the run: %q, event %q",
					got, kind, ok, tc.limit, res.Error, res.Event)
			}
			if tc.fixture != "stream-context.jsonl" && res.ReportedWindow != 200000 {
				t.Errorf("the run reported a window of %d, want Qwen Code's 200000", res.ReportedWindow)
			}
		})
	}
}

// A QWEN RUN STARTS ITS HARNESS WITH THE RUN'S ENVIRONMENT AND NO BRIDGE
// VARIABLE: the script starts the helper, and with it Qwen Code, with no
// variable carrying the bridge's address or token, which its settings and
// helper configuration hold; the key, the Gitlet names and an operator's own
// exporter settings stay. The helper's stand-in records the environment it
// starts with.
//
// Mutations, each red: hand the bridge's address through a variable; unset
// any OTEL_ variable; drop the key or the Gitlet names.
func TestAQwenRunStartsItsHarnessWithTheRunsEnvironmentAndNoBridgeVariable(t *testing.T) {
	t.Parallel()
	runner, b := qwenBox(t, `env > "$HOME/helper-env"
echo '{"type":"crewlet_qwen_end","stop_reason":"end_turn"}'
`)
	const bridge = "http://127.0.0.1:7/mcp/bridge-token-placeholder"
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "begin",
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP,
			URL: bridge}},
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: bridge + "/steer"},
		Env: map[string]string{
			"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "https://collector.example.com/v1/logs",
			"OTEL_SERVICE_NAME":                "seat-own",
			"OPENAI_API_KEY":                   "key-placeholder",
			sandbox.GitURLVar:                  "https://git.example.com/box/git",
			sandbox.GitAccessFileVar:           "/home/box/.crewlet/git-access",
		}})
	if !res.Success {
		t.Fatalf("the run failed: %q / %q", res.Error, res.Transcript)
	}
	env := boxFile(t, b, b.Home()+"/helper-env")
	for _, line := range strings.Split(env, "\n") {
		if strings.Contains(line, "bridge-token-placeholder") {
			t.Errorf("Qwen Code starts with %q, want it absent", line)
		}
	}
	for _, kept := range []string{sandbox.GitURLVar + "=", sandbox.GitAccessFileVar + "=", "OPENAI_API_KEY=",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=", "OTEL_SERVICE_NAME="} {
		if !strings.Contains(env, "\n"+kept) && !strings.HasPrefix(env, kept) {
			t.Errorf("Qwen Code starts without %s, which the run was handed", strings.TrimSuffix(kept, "="))
		}
	}
}

// A QWEN SUBAGENT'S REQUEST IS THE RUN'S: its usage counts toward the run's
// requests ([executor.Decoder.Requests]) as the main thread's does, while its
// words stay out of the run's answer.
//
// Mutation: pass a subagent's update over before its usage is counted, and the
// run counts one request of two.
func TestAQwenSubagentsRequestIsTheRuns(t *testing.T) {
	t.Parallel()
	chunk := func(text, meta string) string {
		return `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk",` +
			`"content":{"type":"text","text":"` + text + `"},"_meta":{` + meta + `"usage":{"inputTokens":10,"outputTokens":2}}}}}`
	}
	dec := executor.QwenCode{}.Events()
	for _, l := range []string{
		chunk("SUB WORDS", `"parentToolCallId":"call_1",`),
		chunk("MAIN ANSWER", ``),
		`{"type":"crewlet_qwen_end","stop_reason":"end_turn"}`,
	} {
		dec.Line([]byte(l))
	}
	if res := dec.Result(); dec.Requests() != 2 || res.Text != "MAIN ANSWER" {
		t.Fatalf("requests %d, answer %q; want 2 and the main thread's words alone", dec.Requests(), res.Text)
	}
}
