package executor_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// The fixtures below are Claude Code's `--output-format stream-json`, written by
// hand from the Agent SDK's documented messages (the `tool_result` blocks with
// the harness's `tool_use_result` copy beside them) rather than captured from a
// run, because a test must not call a model.

// A tool result's BODY, which no transcript may carry: what a command printed
// and the whole of a file the agent read.
const (
	suiteOutput = "ok  \tgithub.com/acme/api\t0.412s\nTHE-SUITE-PRINTED-THIS"
	fileBody    = "package api\n\nfunc TestFlaky(t *testing.T) { THE-FILE-SAID-THIS }"
)

var claudeRunStream = []string{
	`{"type":"system","subtype":"init","cwd":"/home/user/repo","session_id":"sess-1",` +
		`"tools":["Bash","Read","Edit"],"mcp_servers":[{"name":"crewlet","status":"connected"}],` +
		`"model":"claude-sonnet-4-5","permissionMode":"bypassPermissions","apiKeySource":"ANTHROPIC_API_KEY"}`,
	`{"type":"assistant","message":{"id":"msg_01","type":"message","role":"assistant",` +
		`"model":"claude-sonnet-4-5","content":[{"type":"text","text":"I'll run the suite first."}],` +
		`"stop_reason":null,"usage":{"input_tokens":12,"output_tokens":8}},` +
		`"parent_tool_use_id":null,"session_id":"sess-1"}`,
	`{"type":"assistant","message":{"id":"msg_01","type":"message","role":"assistant",` +
		`"content":[{"type":"tool_use","id":"toolu_01","name":"Bash",` +
		`"input":{"command":"go test ./...","description":"Run the test suite"}}]},` +
		`"parent_tool_use_id":null,"session_id":"sess-1","timestamp":"2026-11-02T10:00:01.000Z"}`,
	`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01",` +
		`"type":"tool_result","content":"ok  \tgithub.com/acme/api\t0.412s\nTHE-SUITE-PRINTED-THIS",` +
		`"is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-1","timestamp":"2026-11-02T10:00:03.400Z",` +
		`"tool_use_result":{"stdout":"ok  \tgithub.com/acme/api\t0.412s\nTHE-SUITE-PRINTED-THIS",` +
		`"stderr":"","interrupted":false}}`,
	`{"type":"assistant","message":{"id":"msg_02","type":"message","role":"assistant",` +
		`"content":[{"type":"thinking","thinking":"THE-MODEL-THOUGHT-THIS","signature":"c2ln"},` +
		`{"type":"tool_use","id":"toolu_02","name":"Read",` +
		`"input":{"file_path":"/home/user/repo/flaky_test.go"}}]},` +
		`"parent_tool_use_id":null,"session_id":"sess-1"}`,
	`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_02",` +
		`"type":"tool_result","content":[{"type":"text",` +
		`"text":"package api\n\nfunc TestFlaky(t *testing.T) { THE-FILE-SAID-THIS }"}]}]},` +
		`"parent_tool_use_id":null,"session_id":"sess-1"}`,
	`{"type":"assistant","message":{"id":"msg_03","type":"message","role":"assistant",` +
		`"content":[{"type":"tool_use","id":"toolu_03","name":"Bash",` +
		`"input":{"command":"go test -run TestFlaky -count=20 ./..."}}]},` +
		`"parent_tool_use_id":null,"session_id":"sess-1","timestamp":"2026-11-02T10:00:05.000Z"}`,
	`{"type":"tool_progress","tool_use_id":"toolu_03","tool_name":"Bash","elapsed_time_seconds":3}`,
	`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_03",` +
		`"type":"tool_result","content":"exit status 1\n--- FAIL: TestFlaky (0.01s)","is_error":true}]},` +
		`"parent_tool_use_id":null,"session_id":"sess-1","timestamp":"2026-11-02T10:00:06.250Z"}`,
	`{"type":"assistant","message":{"id":"msg_04","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"Fixed the race and opened https://github.com/acme/api/pull/9"}],` +
		`"stop_reason":"end_turn"},"parent_tool_use_id":null,"session_id":"sess-1"}`,
	`{"type":"result","subtype":"success","is_error":false,"duration_ms":81234,` +
		`"duration_api_ms":60123,"num_turns":6,` +
		`"result":"Fixed the race and opened https://github.com/acme/api/pull/9",` +
		`"session_id":"sess-1","total_cost_usd":0.1834,"usage":{"input_tokens":30,` +
		`"cache_creation_input_tokens":2400,"cache_read_input_tokens":51200,"output_tokens":1900},` +
		`"permission_denials":[]}`,
}

// claudeTranscript is what claudeRunStream says the run did.
const claudeTranscript = "I'll run the suite first.\n" +
	"[tool 1] Bash: go test ./...\n" +
	"[ok 1 2400ms]\n" +
	"[tool 2] Read: /home/user/repo/flaky_test.go\n" +
	"[ok 2]\n" +
	"[tool 3] Bash: go test -run TestFlaky -count=20 ./...\n" +
	"[failed 3 1250ms] exit status 1 (+1 more line(s))\n" +
	"Fixed the race and opened https://github.com/acme/api/pull/9"

func decodeClaude(lines ...string) string {
	dec := claude().Events()
	for _, line := range lines {
		dec.Line([]byte(line))
	}
	return dec.Result().Transcript
}

// THE STREAM IS READ INTO WHAT THE RUN DID: what the agent said, each call
// numbered with its tool and subject, and its result, timed where both lines
// carry a timestamp.
func TestTheClaudeStreamBecomesATranscriptOfWhatTheRunDid(t *testing.T) {
	t.Parallel()
	if got := decodeClaude(claudeRunStream...); got != claudeTranscript {
		t.Fatalf("transcript =\n%s\nwant\n%s", got, claudeTranscript)
	}
}

// A TOOL RESULT'S BODY NEVER REACHES THE TRANSCRIPT — not as the result block,
// the harness's copy beside it, or text blocks — and neither does the model's
// thinking (why: the comment atop claudestream.go).
func TestAClaudeToolResultsBodyNeverReachesTheTranscript(t *testing.T) {
	t.Parallel()
	got := decodeClaude(claudeRunStream...)
	for _, body := range []string{"THE-SUITE-PRINTED-THIS", "THE-FILE-SAID-THIS", "THE-MODEL-THOUGHT-THIS", "0.412s"} {
		if strings.Contains(got, body) {
			t.Errorf("the transcript carries %q:\n%s", body, got)
		}
	}
}

// A FAILED CALL'S RESULT NAMES ITS CALL'S NUMBER AND THE FIRST LINE OF WHY, or
// no number for a result whose call this decoder was never fed.
func TestAFailedClaudeToolCallIsMarked(t *testing.T) {
	t.Parallel()
	failed := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_09",` +
		`"type":"tool_result","content":[{"type":"text","text":"Error: file not found\nat line 3"}],"is_error":true}]}}`
	if got := decodeClaude(failed); got != "[failed] Error: file not found (+1 more line(s))" {
		t.Errorf("a failure whose call was not read = %q", got)
	}
	call := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_09","name":"Edit",` +
		`"input":{"file_path":"/repo/main.go","old_string":"a","new_string":"b"}}]}}`
	want := "[tool 1] Edit: /repo/main.go\n[failed 1] Error: file not found (+1 more line(s))"
	if got := decodeClaude(call, failed); got != want {
		t.Errorf("a failure after its call =\n%s\nwant\n%s", got, want)
	}
}

// A MESSAGE THIS BUILD DOES NOT KNOW IS SKIPPED, because a harness release adds
// them; so is a line read mid-write. Text that is not an event at all is what
// the harness printed, and is kept.
func TestAClaudeStreamSkipsWhatItDoesNotKnow(t *testing.T) {
	t.Parallel()
	got := decodeClaude(
		`{"type":"system","subtype":"init","session_id":"sess-1"}`,
		`{"type":"a_message_from_a_newer_cli","detail":"anything"}`,
		`{"type":"assistant","message":{"content":[{"type":"server_tool_use_v9","id":"x"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"half`,
		`Error: the stream ended unexpectedly`,
	)
	if got != "Error: the stream ended unexpectedly" {
		t.Errorf("transcript = %q; want only the harness's own text", got)
	}
}

// AN INFORMATIONAL HOOK NOTE BECOMES A [steer] LINE: the harness streams the
// line the run's own hook prints as it hands its model a note as
// `system/informational`, `<event> says: crewlet: …` (measured, the fixtures).
// Every other system line — another informational line, a hook's own start and
// end, the same shape from an event that takes no note — is skipped.
//
// Mutation: drop the system case from the decoder and the measured lines vanish;
// match any informational line and the others appear.
func TestAnInformationalHookNoteBecomesASteerLine(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2.1.263", "2.1.310"} {
		measured := strings.Split(strings.TrimSpace(fixture(t, version, "informational.jsonl")), "\n")
		want := "[steer] a note from ada reached the run\n" +
			"[steer] a note from bob reached the run as it was finishing"
		if got := decodeClaude(measured...); got != want {
			t.Errorf("%s: transcript =\n%s\nwant\n%s", version, got, want)
		}
	}
	skipped := decodeClaude(
		`{"type":"system","subtype":"informational","content":"PreToolUse says: crewlet: a note","level":"notice"}`,
		`{"type":"system","subtype":"informational","content":"PostToolBatch says: lint passed","level":"notice"}`,
		`{"type":"system","subtype":"informational","content":{"text":"PostToolBatch says: crewlet: x"}}`,
		`{"type":"system","subtype":"hook_response","hook_event":"Stop","content":"Stop says: crewlet: x"}`,
		`{"type":"system","subtype":"hook_started","hook_event":"PostToolBatch"}`,
	)
	if skipped != "" {
		t.Errorf("a system line that is no note of the run's own reached the transcript:\n%s", skipped)
	}
}

// A RUN THAT DID NOT SUCCEED SAYS HOW in its transcript, from the result
// message's subtype and errors — the documented fields of an error result,
// which carries no `result` text at all.
func TestAClaudeRunThatDidNotSucceedSaysHowItEnded(t *testing.T) {
	t.Parallel()
	end := `{"type":"result","subtype":"error_max_turns","is_error":false,"num_turns":30,` +
		`"session_id":"sess-1","total_cost_usd":0.92,"errors":["Reached maximum number of turns (30)"]}`
	if got := decodeClaude(end); got != "[error] the run ended: error_max_turns: Reached maximum number of turns (30)" {
		t.Errorf("transcript = %q", got)
	}
	res := claude().Parse([]byte(end))
	if res.Success || res.Error != "error_max_turns: Reached maximum number of turns (30)" || res.CostUSD != 0.92 {
		t.Errorf("Parse = success %v, error %q, cost %v; want the failure said and its spend kept",
			res.Success, res.Error, res.CostUSD)
	}
	if got := decodeClaude(claudeRunStream[len(claudeRunStream)-1]); got != "" {
		t.Errorf("a successful end added %q; its final text is already the assistant's last message", got)
	}
}

// THE RESULT IS THE STREAM'S LAST LINE, parsed as the one object
// `--output-format json` prints: its text, session, cost and whole prompt.
func TestTheClaudeResultIsTheStreamsLastLine(t *testing.T) {
	t.Parallel()
	res := claude().Parse([]byte(claudeRunStream[len(claudeRunStream)-1] + "\n"))
	if !res.Success || res.Text != "Fixed the race and opened https://github.com/acme/api/pull/9" {
		t.Fatalf("Parse = %+v; want the successful result", res)
	}
	if res.SessionID != "sess-1" || res.CostUSD != 0.1834 {
		t.Errorf("session %q, cost %v", res.SessionID, res.CostUSD)
	}
	if res.InputTokens != 30+2400+51200 || res.OutputTokens != 1900 ||
		res.CacheReadTokens != 51200 || res.CacheWriteTokens != 2400 {
		t.Errorf("tokens in %d (read %d, written %d) out %d", res.InputTokens,
			res.CacheReadTokens, res.CacheWriteTokens, res.OutputTokens)
	}
	if len(res.DeliveredRefs) != 1 || res.DeliveredRefs[0] != "https://github.com/acme/api/pull/9" {
		t.Errorf("refs %v", res.DeliveredRefs)
	}
}

// A STREAM THAT ENDS BEFORE ITS RESULT IS NOT A SUCCESS. The result file holds
// the stream's last line whatever it was, and a run that died part-way leaves
// an assistant message there — which has no subtype and no is_error, so read
// as a result it was a run that succeeded and said nothing.
func TestAClaudeStreamThatEndsBeforeItsResultIsNotASuccess(t *testing.T) {
	t.Parallel()
	res := claude().Parse([]byte(claudeRunStream[len(claudeRunStream)-2]))
	if res.Success {
		t.Fatal("a stream that ended on an assistant message read as a successful run")
	}
	if !strings.Contains(res.Error, `"assistant" message`) {
		t.Errorf("error = %q; want it to say what the stream ended on", res.Error)
	}

	// A line the process stopped writing part-way is described, not
	// carried: its fragment is a piece of a message, not an account.
	// (A stream line carries its newlines escaped, so a cut one is still
	// one line.)
	cut := `{"type":"user","message":{"content":[{"type":"tool_result","content":"` +
		strings.ReplaceAll(fileBody, "\n", `\n`)
	res = claude().Parse([]byte(cut))
	if res.Success || strings.Contains(res.Error, "THE-FILE-SAID-THIS") ||
		!strings.Contains(res.Error, "not a whole JSON object") {
		t.Errorf("Parse(cut) = success %v, error %q; want it described by size", res.Success, res.Error)
	}
}

// A RUN WHOSE WRAPPER NEVER COPIED ITS RESULT IS READ FROM ITS STREAM. The
// wrapper copies the stream's last line once the harness exits, so a run whose
// process group died (an OOM kill, a host restart) has no result file; read as
// empty, a run that pushed a branch would say "produced no output". The stream's
// last line is what the copy would have held: the harness's result, or what
// the stream was saying when it stopped.
//
// Mutation: parse the missing result file as it is, and each case reads as a
// run that produced no output.
func TestARunWhoseWrapperDiedIsReadFromItsStream(t *testing.T) {
	t.Parallel()
	push := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash",` +
		`"input":{"command":"git push origin feature"}}]}}`
	for _, tc := range []struct {
		name   string
		stream []string
		check  func(t *testing.T, res sandbox.Result)
	}{
		{"it stopped before it reported", []string{push}, func(t *testing.T, res sandbox.Result) {
			if res.Success || strings.Contains(res.Error, "produced no output") ||
				!strings.Contains(res.Error, "stopped before it reported how its run ended") {
				t.Errorf("success %v, error %q; want a run that stopped before its result", res.Success, res.Error)
			}
			if res.Transcript != "[tool 1] Bash: git push origin feature" {
				t.Errorf("transcript = %q; want what it did", res.Transcript)
			}
		}},
		{"it reported, then the wrapper died", claudeRunStream, func(t *testing.T, res sandbox.Result) {
			if !res.Success || res.Text != "Fixed the race and opened https://github.com/acme/api/pull/9" ||
				res.OutputTokens != 1900 {
				t.Errorf("result = %+v; want the result message the stream ended on", res)
			}
			if res.Transcript != claudeTranscript {
				t.Errorf("transcript =\n%s\nwant\n%s", res.Transcript, claudeTranscript)
			}
		}},
		{"it wrote nothing at all", nil, func(t *testing.T, res sandbox.Result) {
			if res.Success || !strings.Contains(res.Error, "produced no output") {
				t.Errorf("success %v, error %q; want a run that produced no output", res.Success, res.Error)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := executor.NewClaudeCode()
			b := box(t, runner)
			p := paths(b)
			if len(tc.stream) > 0 {
				b.Put(p.Stream(), strings.Join(tc.stream, "\n")+"\n")
			}
			// No result file, no exit status, no done marker: the poll
			// ended the run on the wrapper's liveness alone.
			res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			tc.check(t, res)
		})
	}
}

// THE RESULT IS COPIED OUT AFTER THE HARNESS EXITS, and the exit status is the
// harness's own: read into the wrapper's variable before the copy runs, so the
// copy's own success cannot replace a failure. A harness whose result is its
// stream has nothing copied.
func TestTheWrapperCopiesTheResultLineAfterTheHarnessExits(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	start(t, runner, b)
	// The script reaches the box single-quoted inside `sh -lc`, so its own
	// quotes arrive escaped; the order is what is asserted.
	script := lastBackground(t, b)
	// THE WRAPPER'S OWN status read, the last: the run's script reads its own
	// first, to pack the harness's session before it exits with that status.
	exited := strings.LastIndex(script, "code=$?")
	copied := strings.Index(script, "tail -n 1 ")
	recorded := strings.Index(script, "echo $code > ")
	if exited < 0 || copied < exited || recorded < copied {
		t.Fatalf("want the exit status read, then the result copied, then the status recorded:\n%s", script)
	}
	if run := script[:exited]; !strings.Contains(run, p.Stream()) || strings.Contains(run, p.Result()) {
		t.Errorf("stdout does not go to the stream file alone:\n%s", script)
	}
	if copy := script[copied:recorded]; !strings.Contains(copy, p.Stream()) || !strings.Contains(copy, p.Result()) ||
		strings.Index(copy, p.Stream()) > strings.Index(copy, p.Result()) {
		t.Errorf("the copy is not the stream's last line into the result file:\n%s", copy)
	}

	// The copy, not any tail: an OpenCode run's script reads a list's cursor
	// from the answer's last bytes with `tail -c` (openCodeCursor).
	oc := executor.NewOpenCode()
	ob := box(t, oc)
	start(t, oc, ob)
	if script := lastBackground(t, ob); strings.Contains(script, "tail -n 1 ") {
		t.Errorf("a harness whose result is its stream had a result copied:\n%s", script)
	}
}

// A RUN IN A REAL BOX IS COLLECTED FROM ITS STREAM — the wrapper, the shell,
// the copy and the exit status all as they run, with a stand-in for the harness
// that prints the documented stream and exits with a status of its own.
func TestAClaudeRunInARealBoxIsCollectedFromItsStream(t *testing.T) {
	t.Parallel()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	runner := executor.NewClaudeCode()
	if err = runner.Install(t.Context(), box); err != nil {
		t.Fatalf("Install: %v", err)
	}
	p := executor.PathsFor(box)

	// The stand-in sits in the box's bin directory, first on the wrapper's
	// PATH, so `claude` resolves to it. It exits 3 AFTER printing a successful
	// result: the status recorded must be its own, not the copy's.
	stream := strings.Join(claudeRunStream, "\n") + "\n"
	if err = box.WriteFile(t.Context(), p.WorkDir()+"/stream.fixture", []byte(stream)); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}
	writeBoxStandIn(t, box, p.BinDir()+"/claude", "#!/bin/sh\ncat '"+p.WorkDir()+"/stream.fixture'\nexit 3\n")

	handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "fix the flake"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		state, err := runner.Poll(t.Context(), box, handle)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if state.Over() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}

	result, err := box.ReadFile(t.Context(), p.Result())
	if err != nil || strings.TrimSpace(string(result)) != claudeRunStream[len(claudeRunStream)-1] {
		t.Fatalf("the result file holds %q, %v; want the stream's last line", result, err)
	}
	if code, _ := box.ReadFile(t.Context(), p.ExitCode()); strings.TrimSpace(string(code)) != "3" {
		t.Errorf("exit status %q; want the harness's own 3, not the copy's", code)
	}

	res, err := runner.Collect(t.Context(), box, handle, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Transcript != claudeTranscript {
		t.Errorf("transcript =\n%s\nwant\n%s", res.Transcript, claudeTranscript)
	}
	if res.Text != "Fixed the race and opened https://github.com/acme/api/pull/9" ||
		res.InputTokens != 30+2400+51200 || res.CostUSD != 0.1834 {
		t.Errorf("result text %q, input %d, cost %v; want the result line's", res.Text, res.InputTokens, res.CostUSD)
	}
}

// A CALL'S WHOLE SUBJECT RIDES BENEATH ITS PREVIEW where the preview cuts it (a
// heredoc), and a tool with no subject field of its own (an MCP server's) is
// named by its arguments: what a reader expands to see what ran.
func TestAClaudeCallKeepsItsWholeSubjectBeneathItsPreview(t *testing.T) {
	t.Parallel()
	heredoc := "python3 - <<'EOF'\\nimport json\\nprint(json.dumps({}))\\nEOF"
	got := decodeClaude(
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash",`+
			`"input":{"command":"`+heredoc+`"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2",`+
			`"name":"mcp__crewlet__get_work_item","input":{"item":"ENG-12"}}]}}`)
	want := "[tool 1] Bash: python3 - <<'EOF' (+3 more line(s))\n" +
		"[input 1] python3 - <<'EOF'\n[input 1] import json\n[input 1] print(json.dumps({}))\n[input 1] EOF\n" +
		`[tool 2] mcp__crewlet__get_work_item: {"item":"ENG-12"}`
	if got != want {
		t.Errorf("transcript =\n%s\nwant\n%s", got, want)
	}
}

// A RUN'S LAST LIMIT REPORT RIDES ITS RESULT, answered or not, so the engine
// reads where the plan's windows stood (harness.Provider.RunEnded).
func TestARunsLastLimitReportRidesItsResult(t *testing.T) {
	t.Parallel()
	first := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","utilization":0.1}}`
	last := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"seven_day","utilization":0.8}}`
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	stream := append([]string{first}, claudeRunStream...)
	stream = append(stream[:len(stream)-1], last, stream[len(stream)-1])
	b.Put(paths(b).Stream(), strings.Join(stream, "\n")+"\n")
	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !res.Success || res.Limits != last {
		t.Errorf("success %v, limits %q; want the run's last report %q", res.Success, res.Limits, last)
	}
}

// A CLAUDE CODE COMPACTION IS A REQUEST OF THE RUN'S: its summary request
// names no assistant message, so the run's count takes each compaction the
// stream recorded beside the messages it named.
//
// Mutation: count the messages alone, and the run counts two of three.
func TestAClaudeCompactionIsARequestOfTheRuns(t *testing.T) {
	t.Parallel()
	dec := executor.ClaudeCode{}.Events()
	for _, l := range []string{
		`{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto"}}`,
		`{"type":"assistant","message":{"id":"msg_2","role":"assistant","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	} {
		dec.Line([]byte(l))
	}
	if dec.Requests() != 3 {
		t.Fatalf("requests %d, want the 2 messages and the compaction's", dec.Requests())
	}
}

// A CLAUDE CODE MESSAGE OF ITS OWN IS NO REQUEST: once Claude Code ends a turn
// itself ("Autocompact is thrashing", measured on 2.1.330), it writes an
// assistant message of the model `<synthetic>`, which no model request made,
// so the count takes the messages and compactions alone, the synthetic one
// read whole or past the line bound alike.
//
// Mutation: count the synthetic message, read whole or long, and the run
// counts more requests than it made.
func TestAClaudeCodeMessageOfItsOwnIsNoRequest(t *testing.T) {
	t.Parallel()
	synthetic := `{"type":"assistant","message":{"diagnostics":null,"id":"5c22e2b6-1cb8-45a7-bc85-586d75effa4f",` +
		`"container":null,"model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence",` +
		`"content":[{"type":"text","text":"Autocompact is thrashing"}],"usage":{"input_tokens":0,"output_tokens":0}}}`
	stream := []string{
		`{"type":"assistant","message":{"id":"msg_1","model":"claude-sonnet-4-5","role":"assistant","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto"}}`,
		`{"type":"assistant","message":{"id":"msg_2","model":"claude-sonnet-4-5","role":"assistant","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		synthetic,
	}
	dec := executor.ClaudeCode{}.Events()
	for _, l := range stream {
		dec.Line([]byte(l))
	}
	if dec.Requests() != 3 {
		t.Errorf("read whole: requests %d, want the 2 messages and the compaction's", dec.Requests())
	}
	long := executor.ClaudeCode{}.Events()
	for _, l := range stream[:3] {
		long.Line([]byte(l))
	}
	long.Skipped(executor.LongLine{Size: 1 << 30, Bound: 1 << 20, Head: []byte(synthetic[:200]),
		Tail: []byte(synthetic[len(synthetic)-40:])})
	if long.Requests() != 3 || long.Undecided() != 0 {
		t.Errorf("read past the bound: requests %d, undecided %d; want 3 and none", long.Requests(), long.Undecided())
	}
}
