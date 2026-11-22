package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"time"
)

// THE MUSE CODE HOOK ADAPTER (-adapter muse-hook), a one-shot one: each of the
// run's plugin hooks runs the helper once per firing, its input the hook's
// JSON on stdin (measured on Muse Code 1.4.25, #1818's MUSE-MEASURE.md):
//   - PostToolUse takes the run's notes at the bridge's after-a-batch
//     boundary, and Stop as the run is about to finish; the answer goes on
//     stdout, raw UTF-8, and the take's token on stderr, which the session
//     keeps whole (hook_run_terminal.stderr) and never sends to the model;
//   - with -skill-loads, a PostToolUse of `read_skill` first reports the skill
//     loaded to the skill route; with -loads-only, the firing takes nothing;
//   - a subagent's firing (MUSE_THREAD_ID is not MUSE_SESSION_ID) answers
//     nothing: a note handed to it would bind one delegated piece of work.

// museAnswerBytes is Muse Code's ceiling on a hook's stdout: past 16,384 bytes
// it kills the hook and parses nothing (measured on 1.4.25), so an answer past
// it is never handed over, and its take is told again.
const museAnswerBytes = 16384

// museInputBytes bounds the hook's input read: a PostToolUse carries the
// call's output, whose size is the tool's.
const museInputBytes = 64 << 20

// onceBudget bounds one firing's work: under the ten seconds the run's
// plugin gives each hook (executor's museHookTimeout), so the helper ends a
// take the bridge holds too long on its own, its take untaken.
const onceBudget = 9 * time.Second

// The bridge's boundaries a Muse Code firing takes at (mcpbridge's
// SteerEventToolBatch and SteerEventFinish).
const (
	boundaryBatch  = "PostToolBatch"
	boundaryFinish = "Stop"
)

// museHookInput is the part of a hook's input the adapter reads.
type museHookInput struct {
	Event     string `json:"hook_event_name"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Name string `json:"name"`
	} `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

// runMuseHook is one firing: the exit status, the answer on stdout and the
// take's token on stderr. Every failure exits 0 with nothing on stdout, so the
// harness goes on with nothing handed over and the note is told again.
func runMuseHook(ctx context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer,
	getenv func(string) string) int {

	logger := log.New(stderr, "notehelper: ", 0)
	raw, err := io.ReadAll(io.LimitReader(stdin, museInputBytes))
	if err != nil {
		logger.Print("the hook's input could not be read")
		return exitDone
	}
	thread, session := getenv("MUSE_THREAD_ID"), getenv("MUSE_SESSION_ID")
	if thread == "" || thread != session {
		return exitDone
	}
	var in museHookInput
	if json.Unmarshal(raw, &in) != nil {
		logger.Print("the hook's input is not the JSON object Muse Code hands a hook")
		return exitDone
	}
	cfg, err := readTakeConfig(f.config)
	if err != nil {
		logger.Print(err)
		return exitDone
	}
	ctx, cancel := context.WithTimeout(ctx, onceBudget)
	defer cancel()
	if f.skillLoads && in.Event == "PostToolUse" && in.ToolName == "read_skill" && museSkillRead(in.ToolResponse) {
		if err := reportLoad(ctx, cfg, in.ToolInput.Name); err != nil {
			logger.Printf("the skill load was not reported: %v", err)
		}
	}
	if f.loadsOnly {
		return exitDone
	}
	var boundary string
	switch in.Event {
	case "PostToolUse":
		boundary = boundaryBatch
	case "Stop":
		boundary = boundaryFinish
	default:
		return exitDone
	}
	answer, token, err := take(ctx, cfg, boundary)
	switch {
	case err != nil:
		logger.Printf("no note was taken: %v", err)
		return exitDone
	case answer == "":
		return exitDone
	case len(answer) > museAnswerBytes:
		logger.Printf("the take's answer is %d bytes, past the %d Muse Code reads of a hook", len(answer),
			museAnswerBytes)
		return exitDone
	}
	// THE ANSWER FIRST, then its token: a token on the record stands for an
	// answer written whole.
	if _, err := io.WriteString(stdout, answer); err != nil {
		return exitDone
	}
	_, _ = io.WriteString(stderr, token+"\n")
	return exitDone
}

// museSkillRead reports whether a `read_skill` call's output says it read the
// skill: `<read-skill-result name="…" status="ok">` (measured on 1.4.25).
func museSkillRead(response json.RawMessage) bool {
	var text string
	if json.Unmarshal(response, &text) != nil {
		return false
	}
	head, _, _ := strings.Cut(text, ">")
	return strings.HasPrefix(head, "<read-skill-result ") && strings.Contains(head, `status="ok"`)
}
