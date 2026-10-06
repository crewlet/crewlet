package codingagent_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// The fixtures below are Claude Code's `--output-format stream-json` as the
// vendor's Agent SDK documents its messages — a `system` init, `assistant`
// messages whose content is `text`, `thinking` and `tool_use` blocks, `user`
// messages handing back `tool_result` blocks (with `is_error` on a failure,
// and the CLI's own `tool_use_result` copy beside them), and the `result`
// message last — written by hand from that schema rather than captured from
// a run, because a test must not call a model.

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
		`"parent_tool_use_id":null,"session_id":"sess-1"}`,
	`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01",` +
		`"type":"tool_result","content":"ok  \tgithub.com/acme/api\t0.412s\nTHE-SUITE-PRINTED-THIS",` +
		`"is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-1",` +
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
		`"parent_tool_use_id":null,"session_id":"sess-1"}`,
	`{"type":"tool_progress","tool_use_id":"toolu_03","tool_name":"Bash","elapsed_time_seconds":3}`,
	`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_03",` +
		`"type":"tool_result","content":"exit status 1\n--- FAIL: TestFlaky (0.01s)","is_error":true}]},` +
		`"parent_tool_use_id":null,"session_id":"sess-1"}`,
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
	"[tool] Bash: go test ./...\n" +
	"[tool] Read: /home/user/repo/flaky_test.go\n" +
	"[tool] Bash: go test -run TestFlaky -count=20 ./...\n" +
	"[tool] Bash → error: exit status 1 (+1 more line(s))\n" +
	"Fixed the race and opened https://github.com/acme/api/pull/9"

func decodeClaude(lines ...string) string {
	dec := claude().Events()
	for _, line := range lines {
		dec.Line([]byte(line))
	}
	return dec.Result().Transcript
}

// THE STREAM IS READ INTO WHAT THE RUN DID: what the agent said, one line per
// tool call naming the tool and its subject, and a failed call marked as one.
func TestTheClaudeStreamBecomesATranscriptOfWhatTheRunDid(t *testing.T) {
	if got := decodeClaude(claudeRunStream...); got != claudeTranscript {
		t.Fatalf("transcript =\n%s\nwant\n%s", got, claudeTranscript)
	}
}

// A TOOL RESULT'S BODY NEVER REACHES THE TRANSCRIPT — not as the result block,
// not as the CLI's copy beside it, and not as text blocks — and neither does
// the model's thinking. A body is a whole file or everything a command
// printed: echoed, it would grow the record and the live view by orders of
// magnitude and push far more of somebody's files through redaction.
func TestAClaudeToolResultsBodyNeverReachesTheTranscript(t *testing.T) {
	got := decodeClaude(claudeRunStream...)
	for _, body := range []string{"THE-SUITE-PRINTED-THIS", "THE-FILE-SAID-THIS", "THE-MODEL-THOUGHT-THIS", "0.412s"} {
		if strings.Contains(got, body) {
			t.Errorf("the transcript carries %q:\n%s", body, got)
		}
	}
}

// A FAILED CALL NAMES ITS TOOL AND THE FIRST LINE OF WHY — the tool from the
// call the result answers, or a plain "tool" for a result whose call this
// decoder was never fed (a live reading begins part-way through a long
// stream).
func TestAFailedClaudeToolCallIsMarked(t *testing.T) {
	failed := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_09",` +
		`"type":"tool_result","content":[{"type":"text","text":"Error: file not found\nat line 3"}],"is_error":true}]}}`
	if got := decodeClaude(failed); got != "[tool] tool → error: Error: file not found (+1 more line(s))" {
		t.Errorf("a failure whose call was not read = %q", got)
	}
	call := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_09","name":"Edit",` +
		`"input":{"file_path":"/repo/main.go","old_string":"a","new_string":"b"}}]}}`
	want := "[tool] Edit: /repo/main.go\n[tool] Edit → error: Error: file not found (+1 more line(s))"
	if got := decodeClaude(call, failed); got != want {
		t.Errorf("a failure after its call =\n%s\nwant\n%s", got, want)
	}
}

// A MESSAGE THIS BUILD DOES NOT KNOW IS SKIPPED, because a CLI release adds
// them; so is a line read mid-write. Text that is not an event at all is what
// the CLI printed, and is kept.
func TestAClaudeStreamSkipsWhatItDoesNotKnow(t *testing.T) {
	got := decodeClaude(
		`{"type":"system","subtype":"init","session_id":"sess-1"}`,
		`{"type":"a_message_from_a_newer_cli","detail":"anything"}`,
		`{"type":"assistant","message":{"content":[{"type":"server_tool_use_v9","id":"x"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"half`,
		`Error: the stream ended unexpectedly`,
	)
	if got != "Error: the stream ended unexpectedly" {
		t.Errorf("transcript = %q; want only the CLI's own text", got)
	}
}

// A RUN THAT DID NOT SUCCEED SAYS HOW in its transcript, from the result
// message's subtype and errors — the documented fields of an error result,
// which carries no `result` text at all.
func TestAClaudeRunThatDidNotSucceedSaysHowItEnded(t *testing.T) {
	end := `{"type":"result","subtype":"error_max_turns","is_error":false,"num_turns":30,` +
		`"session_id":"sess-1","total_cost_usd":0.92,"errors":["Reached maximum number of turns (30)"]}`
	if got := decodeClaude(end); got != "[error] the run ended: error_max_turns: Reached maximum number of turns (30)" {
		t.Errorf("transcript = %q", got)
	}
	res := claude().Parse(end)
	if res.Success || res.Error != "error_max_turns: Reached maximum number of turns (30)" || res.CostUSD != 0.92 {
		t.Errorf("Parse = success %v, error %q, cost %v; want the failure said and its spend kept",
			res.Success, res.Error, res.CostUSD)
	}
	if got := decodeClaude(claudeRunStream[len(claudeRunStream)-1]); got != "" {
		t.Errorf("a successful end added %q; its final text is already the assistant's last message", got)
	}
}

// THE RESULT IS THE STREAM'S LAST LINE, parsed as the one object `json` used
// to print: its text, its session, its cost and its whole prompt.
func TestTheClaudeResultIsTheStreamsLastLine(t *testing.T) {
	res := claude().Parse(claudeRunStream[len(claudeRunStream)-1] + "\n")
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
	res := claude().Parse(claudeRunStream[len(claudeRunStream)-2])
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
	res = claude().Parse(cut)
	if res.Success || strings.Contains(res.Error, "THE-FILE-SAID-THIS") ||
		!strings.Contains(res.Error, "not a whole JSON object") {
		t.Errorf("Parse(cut) = success %v, error %q; want it described by size", res.Success, res.Error)
	}
}

// A RUN WHOSE WRAPPER NEVER COPIED ITS RESULT IS READ FROM ITS STREAM. The
// result file is the stream's last line, written by the wrapper once the CLI
// has exited, so a run whose process group died — an OOM kill, a host
// restart — has none, and it used to be parsed as empty: a run that streamed a
// whole session, pushed a branch and was killed read "the coding agent
// produced no output" beside a transcript of everything it did. The stream's
// last line is what the copy would have held, so it is read in its place:
// the CLI's result, where it printed one before the wrapper died, and what
// the stream was saying when it stopped otherwise.
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
			if res.Transcript != "[tool] Bash: git push origin feature" {
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
			runner := codingagent.NewClaudeCode()
			b := box(t, runner)
			p := paths(b)
			if len(tc.stream) > 0 {
				b.Put(p.Stream(), strings.Join(tc.stream, "\n")+"\n")
			}
			// No result file, no exit status, no done marker: the poll
			// ended the run on the wrapper's liveness alone.
			res, err := runner.Collect(t.Context(), b, launched(runner))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			tc.check(t, res)
		})
	}
}

// A JOB IS READ BY THE LAYOUT ITS LAUNCH DECLARED, never by what the box
// holds. A build that ran Claude Code with `--output-format json` wrote its
// stdout to the result file and cleared no stream file, so a box this build
// used before carries the PREVIOUS job's stream when such a build reuses it —
// and collected by this build's layout, that job's commands were the new
// job's transcript, and its live view showed them for the whole run.
//
// Mutation: read every job by this build's own layout, and the stale stream's
// tool lines are the json job's transcript.
func TestAJobIsReadByTheLayoutItsLaunchDeclared(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewClaudeCode()
	if handle, err := runner.Start(t.Context(), box(t, runner), sandbox.RunRequest{Brief: "fix it"}); err != nil ||
		handle.Layout != runner.Layout() || handle.Layout == 0 {
		t.Fatalf("Start = %+v, %v; want the stream layout declared on the handle", handle, err)
	}

	b := box(t, runner)
	p := paths(b)
	// The previous job's stream, left where this build streams to.
	b.Put(p.Stream(), strings.Join(claudeRunStream, "\n")+"\n")
	// The json job: its one result object, and what it said on stderr.
	b.Put(p.Result(), `{"type":"result","subtype":"success","is_error":false,"result":"Renamed the flag",`+
		`"usage":{"input_tokens":10,"output_tokens":5}}`)
	b.Put(p.Err(), "warning: a deprecated flag")
	b.Put(p.ExitCode(), "0")
	b.Put(p.Done(), "0")

	json := sandbox.RunHandle{} // launched by a build that declared no layout
	res, err := runner.Collect(t.Context(), b, json)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !res.Success || res.Text != "Renamed the flag" || res.OutputTokens != 5 {
		t.Errorf("result = %+v; want the json job's own result", res)
	}
	if strings.Contains(res.Transcript, "[tool]") || res.Transcript != "warning: a deprecated flag" {
		t.Errorf("transcript = %q; want the json job's stderr, never the previous job's stream", res.Transcript)
	}

	live := read(t, runner.Follow(json), b)
	if strings.Contains(live.Text, "[tool]") || live.Source != sandbox.SourceStderr {
		t.Errorf("the live view = %q from %q; want the job's stderr, never the previous job's stream",
			live.Text, live.Source)
	}
}

// THE RESULT IS COPIED OUT AFTER THE CLI EXITS, and the exit status is the
// CLI's own: read into the wrapper's variable before the copy runs, so the
// copy's own success cannot replace a failure. A CLI whose result is its
// stream has nothing copied.
func TestTheWrapperCopiesTheResultLineAfterTheCLIExits(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	start(t, runner, b)
	// The script reaches the box single-quoted inside `sh -lc`, so its own
	// quotes arrive escaped; the order is what is asserted.
	script := lastBackground(t, b)
	exited := strings.Index(script, "code=$?")
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

	oc := codingagent.NewOpenCode()
	ob := box(t, oc)
	start(t, oc, ob)
	if script := lastBackground(t, ob); strings.Contains(script, "tail") {
		t.Errorf("a CLI whose result is its stream had a result copied:\n%s", script)
	}
}

// A RUN IN A REAL BOX IS COLLECTED FROM ITS STREAM — the wrapper, the shell,
// the copy and the exit status all as they run, with a stand-in for the CLI
// that prints the documented stream and exits with a status of its own.
func TestAClaudeRunInARealBoxIsCollectedFromItsStream(t *testing.T) {
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	runner := codingagent.NewClaudeCode()
	if err = runner.Install(t.Context(), box); err != nil {
		t.Fatalf("Install: %v", err)
	}
	p := codingagent.PathsFor(box)

	// The stand-in sits in the shim directory the wrapper puts first on
	// PATH, so `claude` resolves to it. It exits 3 AFTER printing a
	// successful result: the status recorded must be its own, not the
	// copy's.
	stream := strings.Join(claudeRunStream, "\n") + "\n"
	if err = box.WriteFile(t.Context(), p.WorkDir()+"/stream.fixture", []byte(stream)); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}
	fake := "#!/bin/sh\ncat '" + p.WorkDir() + "/stream.fixture'\nexit 3\n"
	if err = box.WriteFile(t.Context(), p.BinDir()+"/claude", []byte(fake)); err != nil {
		t.Fatalf("write the stand-in: %v", err)
	}
	if _, err = box.Exec(t.Context(), "chmod +x '"+p.BinDir()+"/claude'", sandbox.ExecOptions{}); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "fix the flake"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		done, err := runner.Poll(t.Context(), box, handle)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if done {
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
		t.Errorf("exit status %q; want the CLI's own 3, not the copy's", code)
	}

	res, err := runner.Collect(t.Context(), box, handle)
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
