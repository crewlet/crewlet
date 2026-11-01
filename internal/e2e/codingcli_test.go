package e2e

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// THE EXECUTOR'S HARNESS: this test binary again, standing in for Claude
// Code inside a real box.
//
// Every agent seat's executor is its harness's own run: the engine picks a
// claude-code or opencode member of the seat's executor chain, opens a session
// on its tool bridge, provisions a box, and the runner starts `claude` there
// detached, with the brief, the seat's standing instructions and an MCP config
// naming the bridge. The golden company's executor member is a REAL claude-code
// entry, its box a REAL `direct` one and its runner the REAL Claude Code runner
// — the argv, the wrapper, the done marker, the stream and the result file are
// all the engine's. What stands in is the one thing a test cannot ship: the
// `claude` on the box's PATH, which a setup step copies there ([codingShim]).
//
// It does what Claude Code does with that argv, and nothing a test could get
// right while the engine was wrong: it answers `--version` with a release
// ([codingVersion], or [codingVersionEnv]'s), reads its brief on stdin, as `-p`
// with no prompt does, its system prompt off `--append-system-prompt-file`, its
// tool surface off `--mcp-config` and its hooks off `--settings`, dials the seat's
// bridge over a REAL MCP session, and loops —
// each round asked of the scripted model exactly as the scripted harness on the
// engine host asks one ([modelRequest]: the brief, the tools the bridge
// listed by the server's own names, the system prompt on the append channel
// and the run so far), and each call the model answers made over the bridge
// under a Claude Code tool-use id, so every call lands on the run's row as a
// box's does. It ends when the model submits its work or answers in prose, or
// at `--max-turns`, and prints stream-json as Claude Code does: one object a
// line, the result message last, its usage the sum of every round's.
//
// ITS HOOKS ARE PLAYED AT ITS BOUNDARIES, as Claude Code plays them
// ([codingHooks]): after each batch of calls has its results (PostToolBatch)
// and as it is about to finish (Stop), each hook a call of its tool over the
// run's own bridge session with the event and the agent id filled in. What a
// hook hands back is what the harness does with it: its transcript line printed as
// the harness's informational line, and its additional context — a person's note —
// put before the model's next request, or, at Stop, the turn continued with it.
//
// SO THE SCRIPTED MODEL IS ONE MODEL FOR BOTH HALVES. The executor's rounds
// reach the same [scriptedModel.serve] the phases on the host do, keyed on the same
// offered tools, so a case scripting what the executor calls
// ([scriptedModel.callOnExecute]) scripts what the run calls.

// harnessEnv makes a re-execution of this binary the stand-in harness
// rather than the suite, and its value is the address of the model that
// answers it — one variable for both, as [scriptedHarnessEnv] is.
//
// It reaches the child through the shim's own `env`, which bakes it in: the
// box hands its harness an environment of its own making, never the engine's.
const harnessEnv = "CREWLET_E2E_CODING_CLI"

// What a case may vary about a run, read off the run's own environment —
// which is role.sandbox.env, the ONLY route an external value has into a box,
// so a run that honours one is also proof that block resolved and arrived.
const (
	// codingModeEnv picks what the run does: [codingModeWork] (the default),
	// [codingModeAsk] or [codingModeHeld].
	codingModeEnv = "FAKE_AGENT_MODE"
	// codingArgvEnv names a file, outside the box, the run records its
	// invocation in — its argv and the brief it read on stdin
	// ([codingInvocation]) — outside because the box is reclaimed when the
	// turn ends.
	codingArgvEnv = "FAKE_AGENT_ARGV"
	// codingReleaseEnv names a file a [codingModeHeld] run waits for.
	codingReleaseEnv = "FAKE_AGENT_RELEASE"
	// codingVersionEnv is the release the stand-in answers `claude
	// --version` with, [codingVersion] when unset.
	codingVersionEnv = "FAKE_AGENT_VERSION"
)

// codingVersion is the release the stand-in reports by default: 2.1.310, the
// Claude Code the runner's fixtures were measured on, at or past the oldest a
// run is started with its note hooks on (executor's claudeNotesFloor).
const codingVersion = "2.1.310 (Claude Code)"

// The modes a run can be in.
const (
	// codingModeWork does the scripted work: the rounds the model answers.
	codingModeWork = "work"
	// codingModeAsk parks the run on a structured question through
	// `ask_and_wait` over the seat's bridge, is refused the submission it
	// tries after it, and ends without submitting:
	// its Stop hook still asks for notes, as Claude Code's does at the end of
	// every turn.
	codingModeAsk = "ask"
	// codingModeHeld runs until [codingReleaseEnv] exists, then submits its
	// work over the seat's bridge and ends reporting it finished — so a case
	// can hold a run open across what it is testing, a restart, without a
	// sleep that a loaded machine outlasts. Its one call is made AFTER the
	// release, on a session it opens then, because that is the call a real
	// harness makes after its engine restarted: the run outlives the process
	// that opened its bridge session, and its submission has to reach the
	// one that rebuilt it. It asks no model.
	codingModeHeld = "held"
)

// codingQuestion is what a [codingModeAsk] run asks, and codingOptions the
// options it gives, the first recommended: a person answers it with a press.
const codingQuestion = "Which branch should I target?"

var codingOptions = []map[string]any{
	{"id": "main", "label": "Target main", "detail": "Release with the next tag."},
	{"id": "next", "label": "Target next", "detail": "Hold for the next minor."},
}

// codingHeldReport is what a [codingModeHeld] run reports once released, and
// codingHeldOutcome the outcome it submits.
const (
	codingHeldReport  = "Finished after the restart."
	codingHeldOutcome = "blocked"
)

// codingModel is the model the coder entry names ([codingEntry]), which a run
// is handed on its argv.
const codingModel = "claude-golden"

// codingSession is the session id every stand-in run reports, as Claude Code
// names the conversation a run was.
const codingSession = "sess-e2e"

// codingMaxRounds bounds a run that names no --max-turns: a model that never
// stops calling tools is a fixture bug, and an unbounded loop would hold a box
// until the case times out with nothing said.
const codingMaxRounds = 16

// codingShim is the `claude` a setup step installs on a box's PATH: a script
// re-executing this binary as the stand-in, answering from the model at
// endpoint.
//
// THE SAME FORK-BOMB GUARD as [scriptedEntry]'s: a child that somehow lost
// [harnessEnv] falls through to the suite with a filter matching nothing.
// And GORACE, for the reason that entry sets it.
func codingShim(endpoint string) string {
	return fmt.Sprintf("#!/bin/sh\n"+
		"# The executor's harness: the e2e test binary, standing in for Claude\n"+
		"# Code. See internal/e2e/codingcli_test.go.\n"+
		"exec env %s=%s %s=%s %s '-test.run=^$' \"$@\"\n",
		harnessEnv, shellQuote(endpoint),
		procgrouptest.GORACE, shellQuote(procgrouptest.StandInRaceOptions()),
		shellQuote(scriptedHarnessBinary))
}

// writeCodingShim writes [codingShim] for endpoint under dir and returns its
// path.
func writeCodingShim(dir, endpoint string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "claude")
	return path, os.WriteFile(path, []byte(codingShim(endpoint)), 0o755)
}

// codingSandbox is the providers.sandbox block every company here runs its
// executors under: a `direct` box under boxes, whose one setup step installs
// the shim at shim as the box's `claude` — indented to sit under `providers:`.
//
// `direct` BECAUSE THE POINT IS THE ENGINE'S PROTOCOL: a container would make
// the suite need Docker, and the container argv and the path escape guards
// have their own tests in internal/sandbox.
//
// THE BOX'S BIN DIRECTORY IS ON THE RUN'S PATH: the wrapper prepends
// `$HOME/.crewlet/bin` to the login shell's PATH, so a harness a setup step copies
// there is the `claude` the run's command line names.
func codingSandbox(boxes, shim string) string {
	return fmt.Sprintf(`  sandbox:
    default_run_in: direct
    local:
      state_dir: %q
    setup:
      - name: coding-cli
        commands:
          - %q
        brief: The stand-in harness is installed.
`, boxes, `mkdir -p "$HOME/.crewlet/bin" && cp `+shellQuote(shim)+
		` "$HOME/.crewlet/bin/claude" && chmod +x "$HOME/.crewlet/bin/claude"`)
}

// codingEntry is the providers.llm entry, under key, every agent seat's
// executor launches on: Claude Code, keeping its homes under stateDir —
// indented to sit under `providers: llm:`.
//
// A VENDOR'S ENTRY WITH NOTHING OVERRIDDEN, because nothing of it runs on this
// host: a seat's chain names it after the scripted entry, so every phase on the host
// takes the scripted harness at the chain's head, and the executor's launch skips
// that member (its harness has no in-box runner) to take this one. What the entry
// decides is what the runner is handed — its harness, its model, its cell.
func codingEntry(key, stateDir string) string {
	return fmt.Sprintf(`    %s:
      harness: claude-code
      model: %s
      state_dir: %q
`, key, codingModel, stateDir)
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// codingArgs is what the stand-in reads off the argv the runner wrote, and the
// brief it read on stdin.
type codingArgs struct {
	brief, systemFile, mcpConfig, settings string
	// resume is the session `--resume` names: the asking run's own, which
	// a run carrying it on with the answer continues.
	resume   string
	maxTurns int
}

// codingInvocation is what a run records of how it was started
// ([codingArgvEnv]): its argv, and the brief it read on stdin.
type codingInvocation struct {
	Argv  []string `json:"argv"`
	Stdin string   `json:"stdin"`
}

// parseCodingArgs reads the flags Claude Code's runner passes, skipping every
// one this stand-in has no use for. `-p` takes no value: the prompt is read on
// stdin, which is where the runner puts the brief.
func parseCodingArgs(args []string) (codingArgs, error) {
	var out codingArgs
	value := func(i int) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s names no value", args[i])
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		var err error
		switch args[i] {
		case "--settings":
			out.settings, err = value(i)
		case "--append-system-prompt-file":
			out.systemFile, err = value(i)
		case "--mcp-config":
			out.mcpConfig, err = value(i)
		case "--resume":
			out.resume, err = value(i)
		case "--max-turns":
			var raw string
			if raw, err = value(i); err == nil {
				out.maxTurns, err = strconv.Atoi(raw)
			}
		default:
			continue
		}
		if err != nil {
			return codingArgs{}, err
		}
		i++
	}
	return out, nil
}

// codingStream is the stream-json the stand-in prints: Claude Code's message
// shapes, one object a line.
type codingStream struct{ out io.Writer }

func (s codingStream) line(v any) {
	raw, _ := json.Marshal(v)
	_, _ = fmt.Fprintf(s.out, "%s\n", raw)
}

func (s codingStream) init(tools []string) {
	s.line(map[string]any{"type": "system", "subtype": "init", "session_id": codingSession,
		"tools": tools})
}

// toolUse is one call as Claude Code streams it: under the name it gives an MCP
// server's tool, `mcp__<server>__<tool>`.
func (s codingStream) toolUse(id, name string, input map[string]any) {
	s.line(map[string]any{"type": "assistant", "session_id": codingSession,
		"message": map[string]any{"content": []map[string]any{{
			"type": "tool_use", "id": id,
			"name": "mcp__" + config.ToolServerName + "__" + name, "input": input,
		}}}})
}

func (s codingStream) toolResult(id, text string, failed bool) {
	s.line(map[string]any{"type": "user", "session_id": codingSession,
		"message": map[string]any{"content": []map[string]any{{
			"type": "tool_result", "tool_use_id": id, "content": text, "is_error": failed,
		}}}})
}

func (s codingStream) text(text string) {
	s.line(map[string]any{"type": "assistant", "session_id": codingSession,
		"message": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}})
}

// result is the stream's last line: how the run ended, its final text, how
// many turns its loop took and what every round billed.
func (s codingStream) result(subtype, text string, turns int, spent replyUsage) {
	s.line(map[string]any{
		"type": "result", "subtype": subtype, "is_error": false, "result": text,
		"session_id": codingSession, "num_turns": turns,
		"usage": spent.claudeUsage(),
	})
}

// runHarness is the stand-in harness: it does what its mode says, prints
// its stream, and returns its exit status.
//
// A FAILURE IS THE HARNESS'S OWN, on stderr with a non-zero exit, which the runner
// reports as the run failing with that account — so the reason reaches the
// turn's record rather than a case timing out with nothing said.
func runHarness(endpoint string) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "harness: "+format+"\n", args...)
		return 1
	}
	args := os.Args[1:]
	// A VERSION PROBE IS ANSWERED AS CLAUDE CODE ANSWERS IT, and nothing else
	// happens: no brief read, no invocation recorded, no mode run. A run wired
	// to its notes asks `claude --version` before it starts the harness, to decide
	// its hooks and its system text (executor's claudeNotesGate); a
	// stand-in that ran the probe as a run held a held case's wait in the
	// probe rather than in the harness, answered nothing, and left the gate to its
	// fallback, so no case ran the branch a real release takes.
	if slices.Equal(slices.DeleteFunc(slices.Clone(args), func(arg string) bool {
		// The shim's own filter for the test framework ([codingShim]),
		// ahead of what the run's script passed.
		return strings.HasPrefix(arg, "-test.")
	}), []string{"--version"}) {
		fmt.Println(cmp.Or(os.Getenv(codingVersionEnv), codingVersion))
		return 0
	}
	// THE PROMPT IS STDIN'S, as `-p` with no prompt reads it: the runner
	// points it at the brief's file, which ends.
	brief, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail("reading the brief on stdin: %v", err)
	}
	if path := os.Getenv(codingArgvEnv); path != "" {
		raw, _ := json.Marshal(codingInvocation{Argv: args, Stdin: string(brief)})
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return fail("recording the invocation: %v", err)
		}
	}
	parsed, err := parseCodingArgs(args)
	if err != nil {
		return fail("reading the argv: %v", err)
	}
	parsed.brief = string(brief)
	hooks, err := readCodingHooks(parsed.settings)
	if err != nil {
		return fail("%v", err)
	}
	stream := codingStream{out: os.Stdout}
	// A RUN CARRYING ON AN ASKING RUN, its session resumed with the answer
	// as its prompt: whatever the mode, it does the work now, as Claude Code
	// resuming a conversation answers its next prompt. Asking again would be
	// a run that never reads the answer it asked for. The session it names
	// is the one every stand-in run reports, which is what the asking run's
	// collection recorded.
	if parsed.resume != "" {
		if parsed.resume != codingSession {
			return fail("asked to resume session %q, and the run that asked was %q",
				parsed.resume, codingSession)
		}
		if err := codingWork(context.Background(), endpoint, parsed, hooks, stream); err != nil {
			return fail("%v", err)
		}
		return 0
	}
	switch mode := cmp.Or(os.Getenv(codingModeEnv), codingModeWork); mode {
	case codingModeWork:
	case codingModeHeld:
		// RUNS UNTIL THE TEST SAYS SO, rather than for a fixed time — see
		// [codingModeHeld]. The ceiling is a backstop against a case that
		// dies without releasing, not a timing assumption.
		release := os.Getenv(codingReleaseEnv)
		for deadline := time.Now().Add(2 * time.Minute); ; {
			if _, err := os.Stat(release); err == nil || release == "" {
				break
			}
			if time.Now().After(deadline) {
				return fail("held run never released (%s)", release)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err := codingHeldSubmit(context.Background(), parsed, hooks, stream); err != nil {
			return fail("%v", err)
		}
		stream.result("success", codingHeldReport, 1, replyUsage{})
		return 0
	case codingModeAsk:
		// A FRESH RUN CARRYING ON ONE THAT ASKED is told the question and
		// the answer in its brief, and does not ask again: it does the work,
		// as a real harness told what it was about to ask would.
		if strings.Contains(parsed.brief, codingQuestion) {
			if err := codingWork(context.Background(), endpoint, parsed, hooks, stream); err != nil {
				return fail("%v", err)
			}
			return 0
		}
		// ONLY AS THE RUN WAS TOLD TO. A real harness parks on a question only
		// because its standing instructions say how, so a stand-in that
		// asked whatever it was told would pass a launch that never told it.
		if told, err := toldHowToAsk(parsed); err != nil || !told {
			return fail("the run was to ask a person, and its standing instructions "+
				"never say how (%s): %v", builtin.AskAndWaitTool, err)
		}
		// THE SEAT'S OWN TOOL, over the run's bridge: what it records on the
		// run's row is what the collection parks on, so a question asked any
		// other way would test a protocol the engine does not speak.
		if err := codingAsk(context.Background(), parsed, hooks, stream); err != nil {
			return fail("%v", err)
		}
		stream.result("success", "blocked on a question", 1, replyUsage{})
		return 0
	default:
		// A MODE THIS STAND-IN DOES NOT KNOW SAYS SO. Falling through would
		// run the default and pass a case for the wrong reason.
		return fail("unknown %s %q", codingModeEnv, mode)
	}
	if err := codingWork(context.Background(), endpoint, parsed, hooks, stream); err != nil {
		return fail("%v", err)
	}
	return 0
}

// codingWork is a [codingModeWork] run: the rounds the scripted model answers,
// each call made over the seat's bridge, and the run's hooks played at its
// boundaries — a note one hands back after a batch rides the model's next
// request, and one handed back at the finish continues the turn with it.
func codingWork(ctx context.Context, endpoint string, args codingArgs, hooks codingHooks,
	stream codingStream) error {
	system := ""
	if args.systemFile != "" {
		raw, err := os.ReadFile(args.systemFile)
		if err != nil {
			return fmt.Errorf("reading the system prompt: %w", err)
		}
		system = string(raw)
	}
	bridge, err := bridgeURL(args.mcpConfig)
	if err != nil {
		return err
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "claude", Version: "e2e"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: bridge, HTTPClient: httpx.Client(0)}, nil)
	if err != nil {
		return fmt.Errorf("connecting to the bridge at %s: %w", bridge, err)
	}
	defer func() { _ = session.Close() }()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("listing the bridge's tools: %w", err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	stream.init(names)

	rounds := args.maxTurns
	if rounds <= 0 {
		rounds = codingMaxRounds
	}
	// THE REQUEST THE SCRIPTED HARNESS ON THE HOST POSTS, so the one script
	// answers both: the brief as the prompt, the bridge's tools by the
	// server's own names, and every earlier round of this run.
	req := modelRequest{System: system, Channel: channelAppend, Prompt: args.brief, Tools: names}
	var spent replyUsage
	// finish plays the run's Stop hooks with its last word said: a note one
	// hands back continues the turn, as Claude Code continues it, and
	// nothing ends it on that word.
	finish := func(round int, last string) (bool, error) {
		note, err := hooks.play(ctx, session, codingEventFinish, stream)
		if err != nil || note == "" {
			if err == nil {
				stream.result("success", last, round+1, spent)
			}
			return true, err
		}
		req.Run = append(req.Run, exchange{Reply: modelReply{Text: last}, Notes: []string{note}})
		return false, nil
	}
	for round := range rounds {
		reply, err := askModel(endpoint, req)
		if err != nil {
			return err
		}
		spent = spent.plus(reply.Usage)
		if len(reply.Calls) == 0 {
			stream.text(reply.Text)
			if done, err := finish(round, reply.Text); done || err != nil {
				return err
			}
			continue
		}
		ex := exchange{Reply: reply}
		report := ""
		for _, call := range reply.Calls {
			id := claudeID("toolu_")
			input := orEmptyArgs(call.Args)
			stream.toolUse(id, call.Name, input)
			out, failed := callOverBridge(ctx, session, id, call.Name, input)
			stream.toolResult(id, out, failed)
			ex.Results = append(ex.Results, callResult{Name: call.Name, Content: out, Failed: failed})
			if call.Name == runner.SubmitWorkTool && !failed {
				// THE RUN'S LAST WORD is what it submitted, as a harness
				// that finished by reporting ends on its report.
				report, _ = input["summary"].(string)
				report = cmp.Or(report, "Submitted the work.")
			}
		}
		// THE BATCH HAS ITS RESULTS: a note handed back now rides the
		// model's next request, after them.
		note, err := hooks.play(ctx, session, codingEventToolBatch, stream)
		if err != nil {
			return err
		}
		if note != "" {
			ex.Notes = append(ex.Notes, note)
		}
		req.Run = append(req.Run, ex)
		if report != "" {
			stream.text(report)
			if done, err := finish(round, report); done || err != nil {
				return err
			}
		}
	}
	stream.result("error_max_turns", "", rounds, spent)
	return nil
}

// The hook events the stand-in plays, as Claude Code names them.
const (
	codingEventToolBatch = "PostToolBatch"
	codingEventFinish    = "Stop"
)

// codingHooks is a run's own hooks, by event, read off the settings file its
// `--settings` names — none for a run started without one.
type codingHooks map[string][]codingHook

// codingHook is one hook of the settings file.
type codingHook struct {
	Type   string            `json:"type"`
	Server string            `json:"server"`
	Tool   string            `json:"tool"`
	Input  map[string]string `json:"input"`
}

// readCodingHooks reads the hooks of the settings file at path, refusing a
// file that switches hooks off or holds a hook this stand-in cannot play, so a
// run handed one fails saying so rather than playing nothing.
func readCodingHooks(path string) (codingHooks, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the settings file: %w", err)
	}
	var doc struct {
		DisableAllHooks *bool `json:"disableAllHooks"`
		Hooks           map[string][]struct {
			Hooks []codingHook `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("reading the settings file: %w", err)
	}
	if doc.DisableAllHooks == nil || *doc.DisableAllHooks {
		return nil, fmt.Errorf("the settings file does not switch hooks on, so a repository's "+
			"own disableAllHooks would switch the run's off: %s", raw)
	}
	out := codingHooks{}
	for event, matchers := range doc.Hooks {
		for _, matcher := range matchers {
			for _, hook := range matcher.Hooks {
				if hook.Type != "mcp_tool" || hook.Server != config.ToolServerName {
					return nil, fmt.Errorf("a %s hook calls %s on server %q, and the run's only "+
						"server is the bridge, %q", event, hook.Type, hook.Server, config.ToolServerName)
				}
				out[event] = append(out[event], hook)
			}
		}
	}
	return out, nil
}

// play plays every hook at event as Claude Code does on the main agent's own
// boundary: a call of its tool over the run's bridge session, its input's
// ${hook_event_name} and ${agent_id} filled in (the main agent's id is empty).
// Each answer's transcript line is printed as the harness prints it, and what they
// hand the model is answered, joined. A call that fails is the run's failure:
// the bridge answers this tool with text always.
func (h codingHooks) play(ctx context.Context, session *mcp.ClientSession, event string,
	stream codingStream) (string, error) {
	var notes []string
	for _, hook := range h[event] {
		args := map[string]any{}
		for k, v := range hook.Input {
			v = strings.ReplaceAll(v, "${hook_event_name}", event)
			args[k] = strings.ReplaceAll(v, "${agent_id}", "")
		}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: hook.Tool, Arguments: args})
		if err != nil {
			return "", fmt.Errorf("the %s hook's call of %s: %w", event, hook.Tool, err)
		}
		answer := callText(res)
		if res.IsError {
			return "", fmt.Errorf("the %s hook's call of %s answered an error: %s", event, hook.Tool, answer)
		}
		if strings.TrimSpace(answer) == "" {
			continue
		}
		var out struct {
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
			SystemMessage string `json:"systemMessage"`
		}
		if err := json.Unmarshal([]byte(answer), &out); err != nil {
			return "", fmt.Errorf("the %s hook's answer is not a hook's JSON: %w: %s", event, err, answer)
		}
		if out.SystemMessage != "" {
			stream.line(map[string]any{"type": "system", "subtype": "informational",
				"content": event + " says: " + out.SystemMessage, "level": "notice",
				"session_id": codingSession})
		}
		if note := out.HookSpecificOutput.AdditionalContext; note != "" {
			notes = append(notes, note)
		}
	}
	return strings.Join(notes, "\n\n"), nil
}

// codingAsk is a [codingModeAsk] run's calls: its structured question, over a
// session it opens on the seat's bridge, the submission the bridge refuses it
// once it has asked, then the Stop hook its harness runs as it ends its turn.
func codingAsk(ctx context.Context, args codingArgs, hooks codingHooks, stream codingStream) error {
	bridge, err := bridgeURL(args.mcpConfig)
	if err != nil {
		return err
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "claude", Version: "e2e"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: bridge, HTTPClient: httpx.Client(0)}, nil)
	if err != nil {
		return fmt.Errorf("connecting to the bridge at %s to ask: %w", bridge, err)
	}
	defer func() { _ = session.Close() }()
	stream.init([]string{builtin.AskAndWaitTool, runner.SubmitWorkTool})
	id := claudeID("toolu_")
	options := make([]any, len(codingOptions))
	for i, option := range codingOptions {
		options[i] = option
	}
	input := map[string]any{
		"question": codingQuestion,
		"context":  "The flaky **api** test passes on both branches; the fix is pushed to `wip/flake`.",
		"to":       "requester", "branch": "wip/flake",
		"options": options, "recommended": "main", "role": "approver",
		"rationale": "main ships with the next tag.",
	}
	stream.toolUse(id, builtin.AskAndWaitTool, input)
	out, failed := callOverBridge(ctx, session, id, builtin.AskAndWaitTool, input)
	stream.toolResult(id, out, failed)
	if failed {
		return fmt.Errorf("asking over the bridge: %s", out)
	}
	// AND A SUBMISSION AFTER THE QUESTION IS REFUSED, through the seat's real
	// surface: the job is parked, and a harness that submits rather than ending
	// is told so rather than told its work was delivered (ADR-0068).
	id = claudeID("toolu_")
	submission := map[string]any{"outcome": "delivered", "summary": "Done before the answer."}
	stream.toolUse(id, runner.SubmitWorkTool, submission)
	out, failed = callOverBridge(ctx, session, id, runner.SubmitWorkTool, submission)
	stream.toolResult(id, out, failed)
	if !failed || !strings.Contains(out, "parked on their answer") {
		return fmt.Errorf("a submission after the question answered %q (failed %v), "+
			"want it refused as parked", out, failed)
	}
	_, err = hooks.play(ctx, session, codingEventFinish, stream)
	return err
}

// toldHowToAsk reports whether the run's standing instructions — its system
// prompt file — say how to park on a person's answer.
func toldHowToAsk(args codingArgs) (bool, error) {
	if args.systemFile == "" {
		return false, errors.New("the run was handed no system prompt")
	}
	raw, err := os.ReadFile(args.systemFile)
	if err != nil {
		return false, err
	}
	return strings.Contains(string(raw), builtin.AskAndWaitTool), nil
}

// codingHeldSubmit is a released [codingModeHeld] run's one call: its
// submission, over a session it opens on the seat's bridge now — and the run's
// hooks after it, which take any note a person sent while it was held. It asks
// no model, so a note goes no further than the harness's own transcript line; the
// record of its reading is the engine's.
func codingHeldSubmit(ctx context.Context, args codingArgs, hooks codingHooks, stream codingStream) error {
	bridge, err := bridgeURL(args.mcpConfig)
	if err != nil {
		return err
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "claude", Version: "e2e"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: bridge, HTTPClient: httpx.Client(0)}, nil)
	if err != nil {
		return fmt.Errorf("connecting to the bridge at %s after the release: %w", bridge, err)
	}
	defer func() { _ = session.Close() }()
	stream.init([]string{runner.SubmitWorkTool})
	id := claudeID("toolu_")
	input := map[string]any{
		"outcome": codingHeldOutcome, "summary": codingHeldReport,
		"evidence": "the run was held across its engine's restart and released after it",
	}
	stream.toolUse(id, runner.SubmitWorkTool, input)
	out, failed := callOverBridge(ctx, session, id, runner.SubmitWorkTool, input)
	stream.toolResult(id, out, failed)
	if failed {
		return fmt.Errorf("submitting over the bridge after the release: %s", out)
	}
	if _, err := hooks.play(ctx, session, codingEventToolBatch, stream); err != nil {
		return err
	}
	stream.text(codingHeldReport)
	_, err = hooks.play(ctx, session, codingEventFinish, stream)
	return err
}

// bridgeURL is the seat's tool bridge, read off the MCP config the runner
// wrote — the one server it names under [config.ToolServerName].
func bridgeURL(path string) (string, error) {
	if path == "" {
		return "", errors.New("the run was handed no --mcp-config, so it has no tools")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the MCP config: %w", err)
	}
	var doc struct {
		Servers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("reading the MCP config: %w", err)
	}
	server, ok := doc.Servers[config.ToolServerName]
	if !ok || server.URL == "" {
		return "", fmt.Errorf("the MCP config names no %q server: %s", config.ToolServerName, raw)
	}
	return server.URL, nil
}

// askModel asks the scripted model one round, posting the request the
// scripted harness on the engine host posts ([harnessRun.ask]), and answers its reply.
func askModel(endpoint string, req modelRequest) (modelReply, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return modelReply{}, fmt.Errorf("encoding the round: %w", err)
	}
	resp, err := httpx.Client(0).Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return modelReply{}, fmt.Errorf("asking the model: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return modelReply{}, fmt.Errorf("reading the model's answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return modelReply{}, fmt.Errorf("the model answered %d: %s", resp.StatusCode,
			bytes.TrimSpace(raw))
	}
	var reply modelReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return modelReply{}, fmt.Errorf("reading the model's answer: %w", err)
	}
	return reply, nil
}

// callOverBridge makes one call over the run's bridge session, under the
// tool-use id Claude Code names it by in `_meta` — which the bridge records as
// the call's id — and answers what the model reads back: the tool's text, and
// whether it failed.
func callOverBridge(ctx context.Context, session *mcp.ClientSession, id, name string,
	args map[string]any) (string, bool) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args,
		Meta: mcp.Meta{claudeCodeToolUseMeta: id}})
	if err != nil {
		return err.Error(), true
	}
	return callText(res), res.IsError
}
