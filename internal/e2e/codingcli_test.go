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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
)

// THE EXECUTOR'S CODING CLI: this test binary again, standing in for Claude
// Code inside a real box.
//
// Every agent seat's executor is its coding CLI's own run: the engine picks a
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
// right while the engine was wrong: it reads its brief off `-p`, its system
// prompt off `--append-system-prompt-file` and its tool surface off
// `--mcp-config`, dials the seat's bridge over a REAL MCP session, and loops —
// each round asked of the scripted model exactly as the text backend asks one
// (the transcript [cliagent.RenderPrompt] flattens, over the tools the bridge
// listed, the system prompt on the append channel), and each call the model
// answers made over the bridge, so every call lands on the run's row as a
// box's does. It ends when the model submits its work or answers in prose, or
// at `--max-turns`, and prints stream-json as Claude Code does: one object a
// line, the result message last, its usage the sum of every round's.
//
// SO THE SCRIPTED MODEL IS ONE MODEL FOR BOTH HALVES. The executor's rounds
// reach the same [scriptedModel.serve] the text phases do, keyed on the same
// offered tools, so a case scripting what the executor calls
// ([scriptedModel.callOnExecute]) scripts what the run calls.

// codingCLIEnv makes a re-execution of this binary the stand-in coding CLI
// rather than the suite, and its value is the address of the model that
// answers it — one variable for both, as [scriptedCLIEnv] is.
//
// It reaches the child through the shim's own `env`, which bakes it in: the
// box hands its CLI an environment of its own making, never the engine's.
const codingCLIEnv = "CREWLET_E2E_CODING_CLI"

// What a case may vary about a run, read off the run's own environment —
// which is role.sandbox.env, the ONLY route an external value has into a box,
// so a run that honours one is also proof that block resolved and arrived.
const (
	// codingModeEnv picks what the run does: [codingModeWork] (the default),
	// [codingModeAsk] or [codingModeHeld].
	codingModeEnv = "FAKE_AGENT_MODE"
	// codingArgvEnv names a file, outside the box, the run records its argv
	// in — outside because the box is reclaimed when the turn ends.
	codingArgvEnv = "FAKE_AGENT_ARGV"
	// codingReleaseEnv names a file a [codingModeHeld] run waits for.
	codingReleaseEnv = "FAKE_AGENT_RELEASE"
)

// The modes a run can be in.
const (
	// codingModeWork does the scripted work: the rounds the model answers.
	codingModeWork = "work"
	// codingModeAsk parks the run on a question through `crewlet-ask`, the
	// shim the runner installs, and ends it without touching the bridge.
	codingModeAsk = "ask"
	// codingModeHeld runs until [codingReleaseEnv] exists, then submits its
	// work over the seat's bridge and ends reporting it finished — so a case
	// can hold a run open across what it is testing, a restart, without a
	// sleep that a loaded machine outlasts. Its one call is made AFTER the
	// release, on a session it opens then, because that is the call a real
	// CLI makes after its engine restarted: the run outlives the process
	// that opened its bridge session, and its submission has to reach the
	// one that rebuilt it. It asks no model.
	codingModeHeld = "held"
)

// codingQuestion is what a [codingModeAsk] run asks.
const codingQuestion = "Which branch should I target?"

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
// [codingCLIEnv] falls through to the suite with a filter matching nothing.
// And GORACE, for the reason that entry sets it.
func codingShim(endpoint string) string {
	return fmt.Sprintf("#!/bin/sh\n"+
		"# The executor's coding CLI: the e2e test binary, standing in for Claude\n"+
		"# Code. See internal/e2e/codingcli_test.go.\n"+
		"exec env %s=%s %s=%s %s '-test.run=^$' \"$@\"\n",
		codingCLIEnv, shellQuote(endpoint),
		procgrouptest.GORACE, shellQuote(procgrouptest.StandInRaceOptions()),
		shellQuote(scriptedCLIBinary))
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
// THE SHIM DIRECTORY IS ON THE RUN'S PATH: the wrapper prepends
// `$HOME/.crewlet/bin` to the login shell's PATH, which is where the runner
// puts `crewlet-ask` too, so a CLI copied there is the `claude` the run's
// command line names.
func codingSandbox(boxes, shim string) string {
	return fmt.Sprintf(`  sandbox:
    default_run_in: direct
    local:
      state_dir: %q
    setup:
      - name: coding-cli
        commands:
          - %q
        brief: The stand-in coding CLI is installed.
`, boxes, `mkdir -p "$HOME/.crewlet/bin" && cp `+shellQuote(shim)+
		` "$HOME/.crewlet/bin/claude" && chmod +x "$HOME/.crewlet/bin/claude"`)
}

// codingEntry is the providers.llm entry, under key, every agent seat's
// executor launches on: Claude Code, keeping its homes under stateDir —
// indented to sit under `providers: llm:`.
//
// A VENDOR'S ENTRY WITH NOTHING OVERRIDDEN, because nothing of it runs on this
// host: a seat's chain names it after the scripted entry, so every text phase
// takes the scripted CLI at the chain's head, and the executor's launch skips
// that member (its CLI has no in-box runner) to take this one. What the entry
// decides is what the runner is handed — its CLI, its model, its cell.
func codingEntry(key, stateDir string) string {
	return fmt.Sprintf(`    %s:
      agent: claude-code
      model: %s
      state_dir: %q
`, key, codingModel, stateDir)
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// codingArgs is what the stand-in reads off the argv the runner wrote.
type codingArgs struct {
	brief, systemFile, mcpConfig string
	// resume is the session `--resume` names: the asking run's own, which
	// a run carrying it on with the answer continues.
	resume   string
	maxTurns int
}

// parseCodingArgs reads the flags Claude Code's runner passes, skipping every
// one this stand-in has no use for.
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
		case "-p":
			out.brief, err = value(i)
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
			"name": "mcp__" + config.BridgeServerName + "__" + name, "input": input,
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
		"usage": map[string]int{
			"input_tokens":                spent.input,
			"output_tokens":               spent.output,
			"cache_read_input_tokens":     spent.cacheRead,
			"cache_creation_input_tokens": spent.cacheWrite,
		},
	})
}

// runCodingCLI is the stand-in coding CLI: it does what its mode says, prints
// its stream, and returns its exit status.
//
// A FAILURE IS THE CLI'S OWN, on stderr with a non-zero exit, which the runner
// reports as the run failing with that account — so the reason reaches the
// turn's record rather than a case timing out with nothing said.
func runCodingCLI(endpoint string) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "coding CLI: "+format+"\n", args...)
		return 1
	}
	args := os.Args[1:]
	if path := os.Getenv(codingArgvEnv); path != "" {
		if err := os.WriteFile(path, []byte(strings.Join(args, "\n")+"\n"), 0o644); err != nil {
			return fail("recording the argv: %v", err)
		}
	}
	parsed, err := parseCodingArgs(args)
	if err != nil {
		return fail("reading the argv: %v", err)
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
		if err := codingWork(context.Background(), endpoint, parsed, stream); err != nil {
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
		if err := codingHeldSubmit(context.Background(), parsed, stream); err != nil {
			return fail("%v", err)
		}
		stream.result("success", codingHeldReport, 1, replyUsage{})
		return 0
	case codingModeAsk:
		// A FRESH RUN CARRYING ON ONE THAT ASKED is told the question and
		// the answer in its brief, and does not ask again: it does the work,
		// as a real CLI told what it was about to ask would.
		if strings.Contains(parsed.brief, codingQuestion) {
			if err := codingWork(context.Background(), endpoint, parsed, stream); err != nil {
				return fail("%v", err)
			}
			return 0
		}
		// ONLY AS THE RUN WAS TOLD TO. A real CLI parks on a question only
		// because its standing instructions say how, so a stand-in that
		// asked whatever it was told would pass a launch that never told
		// it — the gap a run with the shim in its box and no word of it
		// once was.
		if told, err := toldHowToAsk(parsed); err != nil || !told {
			return fail("the run was to ask a person, and its standing instructions "+
				"never say how (crewlet-ask): %v", err)
		}
		// THE RUNNER'S OWN SHIM, on the run's PATH: what it writes is the
		// signal the collection reads, so a question asked any other way
		// would test a protocol the engine does not speak.
		ask := exec.Command("crewlet-ask", codingQuestion, "--to", "requester")
		ask.Stdout, ask.Stderr = os.Stderr, os.Stderr
		if err := ask.Run(); err != nil {
			return fail("asking through crewlet-ask: %v", err)
		}
		stream.init(nil)
		stream.result("success", "blocked on a question", 1, replyUsage{})
		return 0
	default:
		// A MODE THIS STAND-IN DOES NOT KNOW SAYS SO. Falling through would
		// run the default and pass a case for the wrong reason.
		return fail("unknown %s %q", codingModeEnv, mode)
	}
	if err := codingWork(context.Background(), endpoint, parsed, stream); err != nil {
		return fail("%v", err)
	}
	return 0
}

// codingWork is a [codingModeWork] run: the rounds the scripted model answers,
// each call made over the seat's bridge.
func codingWork(ctx context.Context, endpoint string, args codingArgs, stream codingStream) error {
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
	defs := make([]llm.ToolDef, 0, len(listed.Tools))
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		defs = append(defs, llm.ToolDef{Name: tool.Name, Description: tool.Description,
			Parameters: schemaMap(tool.InputSchema)})
		names = append(names, tool.Name)
	}
	stream.init(names)

	rounds := args.maxTurns
	if rounds <= 0 {
		rounds = codingMaxRounds
	}
	messages := []llm.Message{{Role: llm.RoleUser, Content: args.brief}}
	var spent replyUsage
	for round := range rounds {
		answer, usage, err := askModel(endpoint, system, llm.Request{Messages: messages, Tools: defs})
		if err != nil {
			return err
		}
		spent.input += usage.input
		spent.output += usage.output
		spent.cacheRead += usage.cacheRead
		spent.cacheWrite += usage.cacheWrite
		env := cliagent.ParseEnvelope(answer)
		if len(env.ToolCalls) == 0 {
			stream.text(env.Message)
			stream.result("success", env.Message, round+1, spent)
			return nil
		}
		calls := make([]llm.ToolCall, len(env.ToolCalls))
		for i, call := range env.ToolCalls {
			calls[i] = llm.ToolCall{ID: fmt.Sprintf("toolu_%02d_%d", round, i),
				Name: call.Name, Arguments: call.Arguments}
		}
		messages = append(messages, llm.Message{Role: llm.RoleAssistant,
			Content: env.Message, ToolCalls: calls})
		report := ""
		for _, call := range calls {
			stream.toolUse(call.ID, call.Name, call.Arguments)
			out, failed := callOverBridge(ctx, session, call)
			stream.toolResult(call.ID, out, failed)
			messages = append(messages, llm.Message{Role: llm.RoleTool,
				ToolCallID: call.ID, Name: call.Name, Content: out, Failed: failed})
			if call.Name == runner.SubmitWorkTool && !failed {
				// THE RUN'S LAST WORD is what it submitted, as a CLI
				// that finished by reporting ends on its report.
				report, _ = call.Arguments["summary"].(string)
				report = cmp.Or(report, "Submitted the work.")
			}
		}
		if report != "" {
			stream.text(report)
			stream.result("success", report, round+1, spent)
			return nil
		}
	}
	stream.result("error_max_turns", "", rounds, spent)
	return nil
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
	return strings.Contains(string(raw), "crewlet-ask"), nil
}

// codingHeldSubmit is a released [codingModeHeld] run's one call: its
// submission, over a session it opens on the seat's bridge now.
func codingHeldSubmit(ctx context.Context, args codingArgs, stream codingStream) error {
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
	call := llm.ToolCall{ID: "toolu_held", Name: runner.SubmitWorkTool, Arguments: map[string]any{
		"outcome": codingHeldOutcome, "summary": codingHeldReport,
		"evidence": "the run was held across its engine's restart and released after it",
	}}
	stream.toolUse(call.ID, call.Name, call.Arguments)
	out, failed := callOverBridge(ctx, session, call)
	stream.toolResult(call.ID, out, failed)
	if failed {
		return fmt.Errorf("submitting over the bridge after the release: %s", out)
	}
	stream.text(codingHeldReport)
	return nil
}

// bridgeURL is the seat's tool bridge, read off the MCP config the runner
// wrote — the one server it names under [config.BridgeServerName].
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
	server, ok := doc.Servers[config.BridgeServerName]
	if !ok || server.URL == "" {
		return "", fmt.Errorf("the MCP config names no %q server: %s", config.BridgeServerName, raw)
	}
	return server.URL, nil
}

// askModel asks the scripted model one round, as the text backend asks one:
// the request flattened by [cliagent.RenderPrompt], the system prompt on the
// append channel — which is the one Claude Code's runner puts it on — and
// answers the reply's text and what it billed.
func askModel(endpoint, system string, req llm.Request) (string, replyUsage, error) {
	prompt, err := cliagent.RenderPrompt(req)
	if err != nil {
		return "", replyUsage{}, fmt.Errorf("rendering the round: %w", err)
	}
	body, err := json.Marshal(cliCall{System: system, Channel: channelAppend, Prompt: prompt})
	if err != nil {
		return "", replyUsage{}, err
	}
	resp, err := httpx.Client(0).Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", replyUsage{}, fmt.Errorf("asking the model: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", replyUsage{}, fmt.Errorf("reading the model's answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", replyUsage{}, fmt.Errorf("the model answered %d: %s", resp.StatusCode,
			bytes.TrimSpace(raw))
	}
	var answer struct {
		Result string `json:"result"`
		Usage  struct {
			Input      int `json:"input_tokens"`
			Output     int `json:"output_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", replyUsage{}, fmt.Errorf("reading the model's answer: %w", err)
	}
	return answer.Result, replyUsage{input: answer.Usage.Input, output: answer.Usage.Output,
		cacheRead: answer.Usage.CacheRead, cacheWrite: answer.Usage.CacheWrite}, nil
}

// callOverBridge makes one call over the run's bridge session and answers what
// the model reads back: the tool's text, and whether it failed.
func callOverBridge(ctx context.Context, session *mcp.ClientSession, call llm.ToolCall) (string, bool) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.Name, Arguments: call.Arguments})
	if err != nil {
		return err.Error(), true
	}
	var text []string
	for _, content := range res.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			text = append(text, tc.Text)
		}
	}
	return strings.Join(text, "\n"), res.IsError
}

// schemaMap is a bridged tool's input schema as a model's tool definition
// carries it.
func schemaMap(schema any) map[string]any {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
